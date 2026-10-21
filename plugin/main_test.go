package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

// testDefaultCachePath isolates the version snapshot for the whole test binary:
// header resolution now always consults the version cache (client AND core), so
// without this the tests would read — and background refreshes would rewrite —
// the operator's real ~/.cli-proxy-api/data/cline-version-cache.json.
var testDefaultCachePath string

// testDefaultModelsPath does the same for the discovered-models snapshot:
// without it the model tests silently passed against the operator's real
// cline-models-cache.json and failed on a clean CI runner.
var testDefaultModelsPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cline-for-cpa-test-")
	if err != nil {
		panic(err)
	}
	testDefaultCachePath = filepath.Join(dir, "cline-version-cache.json")
	customCachePath = testDefaultCachePath
	testDefaultModelsPath = filepath.Join(dir, "cline-models-cache.json")
	customModelsPath = testDefaultModelsPath
	// Package init already ran exposedModels(), which loaded the real snapshot
	// from the default path; drop it so tests see only the generated catalog.
	modelsUpdaterMu.Lock()
	dynamicCatalog, dynamicOrder = nil, nil
	modelsUpdaterMu.Unlock()
	rebuildModelIndex()
	// Version probes must never reach the public internet from tests.
	githubReleasesURL = "http://127.0.0.1:1/releases"
	npmRegistryBaseURL = "http://127.0.0.1:1"
	// No background probes: tests call refreshVersionInBackground directly.
	scheduleVersionRefresh = func(string) {}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
