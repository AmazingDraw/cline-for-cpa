package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const startupFeedJSON = `{
	"recommended": [],
	"free": [{"id": "cline-free/startup-new-1", "name": "Startup New 1"}],
	"clinePass": [], "clineCloud": []
}`

type startupFixture struct {
	srv      *httptest.Server
	feedHits atomic.Int32
	catHits  atomic.Int32
	verHits  atomic.Int32
	// per-endpoint behaviour, set before register
	feedStatus int
	feedDelay  time.Duration
	catDelay   time.Duration
	hang       bool // feed + catalog block until the client gives up
}

// newStartupFixture isolates every global the sync touches (caches, state file,
// base URL, version endpoints, the once) and restores them afterwards.
func newStartupFixture(t *testing.T) *startupFixture {
	t.Helper()
	f := &startupFixture{feedStatus: http.StatusOK}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wait := func(d time.Duration) bool {
			if f.hang {
				select {
				case <-r.Context().Done():
				case <-time.After(10 * time.Second):
				}
				return false
			}
			if d > 0 {
				select {
				case <-time.After(d):
				case <-r.Context().Done():
					return false
				}
			}
			return true
		}
		switch r.URL.Path {
		case "/ai/cline/recommended-models":
			f.feedHits.Add(1)
			if !wait(f.feedDelay) {
				return
			}
			if f.feedStatus != http.StatusOK {
				http.Error(w, "boom", f.feedStatus)
				return
			}
			_, _ = w.Write([]byte(startupFeedJSON))
		case "/ai/cline/models":
			f.catHits.Add(1)
			if !wait(f.catDelay) {
				return
			}
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			f.verHits.Add(1)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)

	dir := t.TempDir()
	prevModels, prevVer := customModelsPath, customCachePath
	customModelsPath = filepath.Join(dir, "cline-models-cache.json")
	customCachePath = filepath.Join(dir, "cline-version-cache.json")
	withUpstreamBaseURL(t, f.srv.URL)
	resetVersionCacheForTest(t)
	prevGH, prevNPM := githubReleasesURL, npmRegistryBaseURL
	githubReleasesURL, npmRegistryBaseURL = f.srv.URL+"/releases", f.srv.URL
	prevBudget, prevBackoff := startupSyncBudget, startupFailBackoff
	// Fresh versions: isolate these tests to the models path.
	now := time.Now()
	versionMu.Lock()
	for _, k := range []string{targetKeyDesktop, targetKeyCLI, targetKeyCore} {
		versionCache[k] = versionCacheEntry{Version: "9.9.9", FetchedAt: now}
	}
	versionMu.Unlock()
	resetModelsForTest()
	startupSyncOnce = sync.Once{}

	t.Cleanup(func() {
		customModelsPath, customCachePath = prevModels, prevVer
		githubReleasesURL, npmRegistryBaseURL = prevGH, prevNPM
		startupSyncBudget, startupFailBackoff = prevBudget, prevBackoff
		resetModelsForTest()
		startupSyncOnce.Do(func() {})
	})
	return f
}

func resetModelsForTest() {
	modelsUpdaterMu.Lock()
	dynamicCatalog, dynamicOrder, lastModelsFetch = nil, nil, time.Time{}
	modelsUpdaterMu.Unlock()
	rebuildModelIndex()
	// Next register reloads the cache from disk, like a new process (the
	// rebuild above consumed the Once).
	modelsUpdaterMu.Lock()
	modelsDiskLoadedOnce = sync.Once{}
	modelsUpdaterMu.Unlock()
}

// seedModelsDisk writes a cache with one marker model, aged by age.
func seedModelsDisk(t *testing.T, age time.Duration) {
	t.Helper()
	snap := modelsSnapshot{
		FetchedAt: time.Now().Add(-age),
		Models:    map[string]modelMeta{"cline-free/disk-cached-1": {Tier: "free", DisplayName: "Disk", ContextLength: 1000}},
		Order:     []string{"cline-free/disk-cached-1"},
	}
	raw, _ := json.Marshal(snap)
	if err := os.WriteFile(customModelsPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func register(t *testing.T) time.Duration {
	t.Helper()
	start := time.Now()
	raw, err := HandleMethod("plugin.register", nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	var env struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(raw, &env) != nil {
		t.Fatalf("register returned non-JSON: %s", raw)
	}
	return time.Since(start)
}

func hasModel(id string) bool {
	_, ok := lookupModel(id)
	return ok
}

func TestStartupSyncSuccess(t *testing.T) {
	f := newStartupFixture(t)
	seedModelsDisk(t, 3*time.Hour) // stale: must be replaced

	register(t)

	if f.feedHits.Load() != 1 || f.catHits.Load() != 1 {
		t.Fatalf("feed/catalog hits = %d/%d, want 1/1", f.feedHits.Load(), f.catHits.Load())
	}
	// Fresh list is live in the index BEFORE register returned (what the host
	// reads right after), and the stale disk entry is gone.
	if !hasModel("cline-free/startup-new-1") {
		t.Fatal("fresh model not in index after register")
	}
	if hasModel("cline-free/disk-cached-1") {
		t.Fatal("stale disk model survived a successful sync")
	}
	var ids []string
	for _, m := range staticModels().Models {
		ids = append(ids, m.ID)
	}
	found := false
	for _, id := range ids {
		if id == "cline-free/startup-new-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("model.static does not advertise the fresh model: %v", ids)
	}
	raw, err := os.ReadFile(customModelsPath)
	if err != nil || !json.Valid(raw) {
		t.Fatalf("disk cache not written: %v", err)
	}
	var disk modelsSnapshot
	_ = json.Unmarshal(raw, &disk)
	if _, ok := disk.Models["cline-free/startup-new-1"]; !ok || time.Since(disk.FetchedAt) > time.Minute {
		t.Fatalf("disk cache not refreshed: %+v", disk)
	}
	if st := loadSyncState(); !st.ModelsFailedAt.IsZero() {
		t.Fatalf("failure recorded after success: %+v", st)
	}
}

func TestStartupSyncFreshCacheSkipsNetwork(t *testing.T) {
	f := newStartupFixture(t)
	seedModelsDisk(t, 10*time.Minute) // younger than modelsCacheTTL

	register(t)

	if f.feedHits.Load()+f.catHits.Load() != 0 {
		t.Fatalf("fresh cache must not fetch, hits=%d/%d", f.feedHits.Load(), f.catHits.Load())
	}
	if !hasModel("cline-free/disk-cached-1") {
		t.Fatal("fresh disk cache not loaded")
	}
}

func TestStartupSyncRunsInParallel(t *testing.T) {
	f := newStartupFixture(t)
	f.feedDelay, f.catDelay = 350*time.Millisecond, 350*time.Millisecond
	// Sequential fetches would need ~700ms and miss this budget.
	startupSyncBudget = 600 * time.Millisecond

	took := register(t)

	if !hasModel("cline-free/startup-new-1") {
		t.Fatalf("parallel fetch missed the budget (took %s)", took)
	}
	if took > 580*time.Millisecond {
		t.Fatalf("took %s: endpoints are not running in parallel", took)
	}
}

func TestStartupSyncTimeoutFallsBackToDiskCache(t *testing.T) {
	f := newStartupFixture(t)
	f.hang = true
	startupSyncBudget = 300 * time.Millisecond
	seedModelsDisk(t, 3*time.Hour)

	took := register(t)

	if took < 250*time.Millisecond || took > 1500*time.Millisecond {
		t.Fatalf("register took %s, want ≈ budget (300ms) and certainly bounded", took)
	}
	if !hasModel("cline-free/disk-cached-1") {
		t.Fatal("disk cache must keep serving after a timeout")
	}
	if hasModel("cline-free/startup-new-1") {
		t.Fatal("nothing should have been applied after a timeout")
	}
	if st := loadSyncState(); st.ModelsFailedAt.IsZero() {
		t.Fatal("timeout must be recorded for the backoff")
	}
}

func TestStartupSyncFailureFallsBackToCompiledCatalog(t *testing.T) {
	f := newStartupFixture(t)
	f.feedStatus = http.StatusInternalServerError // no disk cache at all

	register(t)

	if f.feedHits.Load() != 1 {
		t.Fatalf("feed hits = %d, want 1", f.feedHits.Load())
	}
	if hasModel("cline-free/startup-new-1") {
		t.Fatal("failed sync must not apply anything")
	}
	if len(exposedModels()) == 0 {
		t.Fatal("compiled catalog must keep serving when the sync and the disk cache both fail")
	}
	if st := loadSyncState(); st.ModelsFailedAt.IsZero() {
		t.Fatal("failure must be recorded for the backoff")
	}
}

func TestStartupSyncFailureBackoff(t *testing.T) {
	f := newStartupFixture(t)
	f.feedStatus = http.StatusBadGateway
	register(t)
	if f.feedHits.Load() != 1 {
		t.Fatalf("first start: feed hits = %d, want 1", f.feedHits.Load())
	}

	// A quick restart (a new process: once + in-memory state reset) must not
	// pay the network again.
	startupSyncOnce = sync.Once{}
	resetModelsForTest()
	took := register(t)
	if f.feedHits.Load() != 1 {
		t.Fatalf("restart inside the backoff re-fetched: hits = %d", f.feedHits.Load())
	}
	if took > 300*time.Millisecond {
		t.Fatalf("restart inside the backoff took %s", took)
	}

	// Once the backoff has elapsed, the sync is tried again — and recovers.
	st := loadSyncState()
	st.ModelsFailedAt = time.Now().Add(-startupFailBackoff - time.Minute)
	saveSyncState(st)
	f.feedStatus = http.StatusOK
	startupSyncOnce = sync.Once{}
	resetModelsForTest()
	register(t)
	if f.feedHits.Load() != 2 {
		t.Fatalf("after the backoff: feed hits = %d, want 2", f.feedHits.Load())
	}
	if !hasModel("cline-free/startup-new-1") {
		t.Fatal("sync after the backoff should have applied the list")
	}
	if st := loadSyncState(); !st.ModelsFailedAt.IsZero() {
		t.Fatalf("failure marker not cleared after recovery: %+v", st)
	}
}

func TestStartupSyncOnlyOncePerProcess(t *testing.T) {
	f := newStartupFixture(t)
	f.feedStatus = http.StatusServiceUnavailable
	register(t)
	register(t) // a second register in the same process
	if f.feedHits.Load() != 1 {
		t.Fatalf("feed hits = %d, want 1", f.feedHits.Load())
	}
}

func TestStartupSyncReconfigureNeverFetches(t *testing.T) {
	f := newStartupFixture(t)
	seedModelsDisk(t, 3*time.Hour) // stale: register would fetch

	start := time.Now()
	raw, err := HandleMethod("plugin.reconfigure", []byte(`{}`))
	if err != nil || len(raw) == 0 {
		t.Fatalf("reconfigure: %v %s", err, raw)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatalf("reconfigure took %s", time.Since(start))
	}
	if n := f.feedHits.Load() + f.catHits.Load() + f.verHits.Load(); n != 0 {
		t.Fatalf("reconfigure hit the network %d times", n)
	}
	// And a register after a reconfigure still gets its one sync.
	register(t)
	if f.feedHits.Load() != 1 {
		t.Fatalf("register after reconfigure: feed hits = %d, want 1", f.feedHits.Load())
	}
}

func TestStartupSyncVersionProbesShareTheBudget(t *testing.T) {
	f := newStartupFixture(t)
	seedModelsDisk(t, time.Minute) // models fresh: only versions are stale
	versionMu.Lock()
	versionCache = map[string]versionCacheEntry{} // all three missing
	versionMu.Unlock()
	// Version endpoints hang; the models path is not needed.
	hangSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer hangSrv.Close()
	githubReleasesURL, npmRegistryBaseURL = hangSrv.URL+"/releases", hangSrv.URL
	startupSyncBudget = 300 * time.Millisecond
	_ = f

	took := register(t)
	if took > 1500*time.Millisecond {
		t.Fatalf("hanging version probes held register for %s", took)
	}
	if st := loadSyncState(); st.VersionsFailedAt.IsZero() {
		t.Fatal("version failure must be recorded for the backoff")
	}
}
