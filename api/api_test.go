package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/snyk/snyk-code-review-exercise/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// review: There are very few tests for other happy paths, non happy paths and
// pathological cases like circular dependencies, or both deep and wide trees.
// What happens if the given version if not accepted by the semver package? What
// if the response body from npmjs.com isn't json? What is the response is
// abnormal (not 2xx)?
//
// review: I already wrote something similar to this elsewhere, but these tests
// should be reproducible. I tried running the test, and it failed, because it's
// using an external system. The test should use a mock http server instead.
func TestPackageHandler(t *testing.T) {
	handler := api.New()
	server := httptest.NewServer(handler)
	defer server.Close()

	// idea: instead of relying on npmjs.com for unit tests, it would be
	// better to create a mock http server and use that instead. Unit tests
	// should be offline and reproducible.
	resp, err := server.Client().Get(server.URL + "/package/react/16.13.0")
	require.Nil(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.Nil(t, err)

	var data api.NpmPackageVersion
	err = json.Unmarshal(body, &data)
	require.Nil(t, err)

	assert.Equal(t, "react", data.Name)
	assert.Equal(t, "16.13.0", data.Version)

	fixture, err := os.Open(filepath.Join("testdata", "react-16.13.0.json"))

	// review: Prefer require.NoError. The intention of the assertion is
	// clearer, and testify should present it better than just the value not
	// being nil.
	//
	// review: This is probably a distraction, sorry, it would be good to
	// establish some guidelines for the project. Google's Go style guide
	// discourages use of assertion helps like testify as it is not
	// considered idiomatic. I really see the value though and have used
	// testify extensively myself.
	//
	//      https://google.github.io/styleguide/go/best-practices#leave-testing-to-the-test-function
	//
	// 	https://google.github.io/styleguide/go/decisions#assert
	require.Nil(t, err)

	var fixtureObj api.NpmPackageVersion
	require.Nil(t, json.NewDecoder(fixture).Decode(&fixtureObj))

	// review: Is this the best way to check the API is returning what we
	// expect from it? We are comparing json which has been decoded into the
	// same struct. What is there are extra fields being encoded which
	// shouldn't be there, or if the structure of the json changes. This
	// won't necessarily protect against regressions or breaking changes in
	// the API. Go's json encoder is deterministic, so it might be a better
	// idea to both validate that the response decodes okay, and compare the
	// raw response body with the testdata. [json.Compact] may be helpful
	// for this, as it will ensure the two json strings are comparable
	// without consideration for whitespace.
	assert.Equal(t, fixtureObj, data)
}

func TestCycle(t *testing.T) {
	handler := api.New()
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := server.Client().Get(server.URL + "/package/d/1.0.1")
	require.Nil(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.Nil(t, err)

	var data api.NpmPackageVersion
	err = json.Unmarshal(body, &data)
	require.Nil(t, err)

	assert.Equal(t, "d", data.Name)
	assert.Equal(t, "1.0.1", data.Version)
}
