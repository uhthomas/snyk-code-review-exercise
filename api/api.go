package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"

	"github.com/Masterminds/semver/v3"
	"github.com/gorilla/mux"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
)

func New() http.Handler {
	router := mux.NewRouter()
	router.Handle("/package/{package}/{version}", http.HandlerFunc(packageHandler))
	return router
}

type npmPackageMetaResponse struct {
	Versions map[string]npmPackageResponse `json:"versions"`
}

type npmPackageResponse struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Dependencies map[string]string `json:"dependencies"`
}

type NpmPackageVersion struct {
	parent *NpmPackageVersion

	Name    string `json:"name"`
	Version string `json:"version"`

	// review: I'm concerned that this is changing the contract of the API.
	// It's not clear to me as to what capacity this service is being used,
	// but existing clients will be expecting a `map[string]string` and will
	// fail to decode this.
	//
	// idea: Assuming that breaking changes are unacceptable, we could use a
	// new route which lists dependencies recursively.
	Dependencies map[string]*NpmPackageVersion `json:"dependencies"`
}

func (pkg *NpmPackageVersion) HasDirectAncestor(target *NpmPackageVersion) bool {
	if pkg.parent == nil {
		return false
	}
	if pkg.Is(target) {
		return true
	}
	return pkg.parent.HasDirectAncestor(target)
}

func (pkg *NpmPackageVersion) Is(target *NpmPackageVersion) bool {
	return pkg.Name == target.Name && pkg.Version == target.Version
}

func packageHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	pkgName := vars["package"]

	// review: It is important to note that this is actually a constraint
	// and note a version. I'm not sure what sort of consequences this may
	// have, or if it's a bad thing, but it is unexpected.
	pkgVersion := vars["version"]

	rootPkg := &NpmPackageVersion{Name: pkgName, Dependencies: map[string]*NpmPackageVersion{}}
	if err := newDependencyResolver().resolveDependencies(rootPkg, pkgVersion); err != nil {
		// review: I understand a precedent has already been set in this
		// project, but we should be using a structured logging package
		// and adding context to errors. Printing a plain error message
		// to stdout without any context is not really helpful.
		println(err.Error())

		// review: We should be using constants like
		// http.StatusInternalServerError instead of magic numbers.
		//
		// review: Is this the best response code to use? What if the
		// package simply can't be found? That would be a 404, not a
		// 500.
		w.WriteHeader(500)
		return
	}

	// idea: It may be nice to use [json.NewEncoder] instead. It should be
	// more efficient and is more concise.
	//
	// idea: I recognise indentation is helpful for humans and debugging,
	// but should a production system be serving indented json? It's a lot
	// of exta data.
	stringified, err := json.MarshalIndent(rootPkg, "", "  ")
	if err != nil {
		println(err.Error())
		w.WriteHeader(500)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	// idea: A 200 OK response is implied if no status code is explicitly
	// sent and as such this is kind of redundant.
	w.WriteHeader(200)

	// idea: Explicitly ignoring the return value here is only ever done to
	// appease linters and generally is discouraged.
	//
	// Ignoring ResponseWriter errors
	_, _ = w.Write(stringified)
}

type dependencyResolver struct {
	sem                     *semaphore.Weighted
	singleflight            singleflight.Group
	metaCache, packageCache sync.Map
}

func newDependencyResolver() *dependencyResolver {
	return &dependencyResolver{sem: semaphore.NewWeighted(10)}
}

// review: I like that this functionality has been moved into a function! It's
// much cleaner this way. I recognise it's necessary due to being recursive, but
// even so.
//
// review: I tried this against express@4.21.2, and it took a very long time to
// load. Is there anyway we can make it faster?
//
// review: Have we considered circular dependencies? Expressing them in a tree
// like this is basically impossible, so I guess the best thing to do would be
// to stop recursing if the child is also an ancestor (or itself).
//
// That said, even if we stop recursing when a package has a child of itself,
// the tree could still be huge if the same package is used by multiple other
// packages. I guess this is a long way of saying that a tree is probably not
// the most efficient way of representing this data. It really calls for a
// proper graph rather than a tree.
//
// idea: We could have a cache, both for package metadata and for packages
// themselves. This may help with speed.
//
// review: I tried this against npm@11.1.0 and received the following error:
//
//	improper constraint: npm:strip-ansi@^6.0.1
//
// This seems strange, as it looks like the code tries to account for this.
//
// Source of the error:
//
//	https://github.com/Masterminds/semver/blob/1558ca3488226e3490894a145e831ad58a5ff958/constraints.go#L32
//
//	https://github.com/Masterminds/semver/blob/1558ca3488226e3490894a145e831ad58a5ff958/constraints.go#L245
//
// review: I also tested trucolor@4.0.4 and it never finished loading. I can
// only assume it has a circular dependency.
//
// review: The task suggests fetching packages async to help improve speed. I
// think this is a nice idea, though it is lower on the priority list than
// caching and resolving circular dependencies. Fetching dependencies async may
// make the code more complicated and considerations then need to be made for
// data races. It's absolutely doable with tools like
// [https://pkg.go.dev/golang.org/x/sync/errgroup],
// [https://pkg.go.dev/golang.org/x/sync/singleflight] and [sync.Mutex].
// Complexity is my concern.
func (r *dependencyResolver) resolveDependencies(pkg *NpmPackageVersion, versionConstraint string) error {
	pkgMeta, err := r.fetchPackageMetaSingleflight(pkg.Name)
	if err != nil {
		return err
	}
	concreteVersion, err := highestCompatibleVersion(versionConstraint, pkgMeta)
	if err != nil {
		return err
	}
	pkg.Version = concreteVersion

	if pkg.parent != nil && pkg.parent.HasDirectAncestor(pkg) {
		return nil
	}

	npmPkg, err := r.fetchPackageSingleflight(pkg.Name, pkg.Version)
	if err != nil {
		return err
	}
	var g errgroup.Group
	for dependencyName, dependencyVersionConstraint := range npmPkg.Dependencies {
		dep := &NpmPackageVersion{
			parent:       pkg,
			Name:         dependencyName,
			Dependencies: map[string]*NpmPackageVersion{},
		}
		pkg.Dependencies[dependencyName] = dep

		g.Go(func() error {
			return r.resolveDependencies(dep, dependencyVersionConstraint)
		})
	}
	return g.Wait()
}

func highestCompatibleVersion(constraintStr string, versions *npmPackageMetaResponse) (string, error) {
	constraint, err := semver.NewConstraint(constraintStr)
	if err != nil {
		return "", err
	}
	filtered := filterCompatibleVersions(constraint, versions)
	sort.Sort(filtered)
	if len(filtered) == 0 {
		return "", errors.New("no compatible versions found")
	}
	return filtered[len(filtered)-1].String(), nil
}

func filterCompatibleVersions(constraint *semver.Constraints, pkgMeta *npmPackageMetaResponse) semver.Collection {
	var compatible semver.Collection
	for version := range pkgMeta.Versions {
		semVer, err := semver.NewVersion(version)
		if err != nil {
			continue
		}
		if constraint.Check(semVer) {
			compatible = append(compatible, semVer)
		}
	}
	return compatible
}

func (r *dependencyResolver) fetchPackageSingleflight(name, version string) (*npmPackageResponse, error) {
	v, err, _ := r.singleflight.Do("package/"+name+version, func() (interface{}, error) {
		return r.fetchPackage(name, version)
	})
	if err != nil {
		return nil, err
	}
	return v.(*npmPackageResponse), nil
}

func (r *dependencyResolver) fetchPackage(name, version string) (*npmPackageResponse, error) {
	if v, ok := r.packageCache.Load(name + version); ok {
		fmt.Printf("using cached package: %s@%s\n", name, version)
		return v.(*npmPackageResponse), nil
	}

	if err := r.sem.Acquire(context.Background(), 1); err != nil {
		return nil, err
	}
	defer r.sem.Release(1)

	// idea: URL construction could be safer with [path.Join] and
	// [net/url.URL.ResolveReference]. String templating is prone to all
	// sorts of weirdness.
	//
	// Example:
	//
	// 	u.ResolveReference(&url.URL{
	//		Path: path.Join(name, version),
	// 	})
	//
	// idea: It has historically been strongly encouraged to construct
	// bespoke a [*http.Client] with reasonable timeouts. It is not so
	// relevant these days as the default transport has pretty reasonable
	// defaults now. That said, a bespoke [*http.Client] may still be worth
	// considering as it would be useful for collecting metrics and tracing.
	resp, err := http.Get(fmt.Sprintf("https://registry.npmjs.org/%s/%s", name, version))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// idea: There should be some sort of limit. It's possible the response
	// body could be large enough to cause memory exhaustion.
	//
	// idea: In addition, it's probably better to [json.NewDecoder] where
	// possible.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// idea: We should not be ignoring decoding errors.
	var parsed npmPackageResponse
	_ = json.Unmarshal(body, &parsed)

	r.packageCache.Store(name+version, &parsed)

	return &parsed, nil
}

func (r *dependencyResolver) fetchPackageMetaSingleflight(p string) (*npmPackageMetaResponse, error) {
	v, err, _ := r.singleflight.Do("package-meta/"+p, func() (interface{}, error) {
		return r.fetchPackageMeta(p)
	})
	if err != nil {
		return nil, err
	}
	return v.(*npmPackageMetaResponse), nil
}

// idea: If the package is not found (404, I assume), we should return a
// different error which the handler can use to differentiate between fatal and
// non fatal.
func (r *dependencyResolver) fetchPackageMeta(p string) (*npmPackageMetaResponse, error) {
	if v, ok := r.metaCache.Load(p); ok {
		fmt.Printf("using cached package meta: %s\n", p)
		return v.(*npmPackageMetaResponse), nil
	}

	if err := r.sem.Acquire(context.Background(), 1); err != nil {
		return nil, err
	}
	defer r.sem.Release(1)

	resp, err := http.Get(fmt.Sprintf("https://registry.npmjs.org/%s", p))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var parsed npmPackageMetaResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return nil, err
	}

	r.metaCache.Store(p, &parsed)

	return &parsed, nil
}
