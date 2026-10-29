package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Startup sync: the ONLY point at which the plugin refreshes the model list.
//
// Why at plugin.register and nowhere else: the host registers models once, right
// after register returns (model.static / model.for_auth) and has no ABI for a
// plugin to re-register later. A refresh that lands after register (the old
// 6h ticker) can update this process's memory but never the host's list, so the
// list the user sees was always the one from the previous cache. Fetching
// synchronously before register returns makes /v1/models fresh on the first
// start after the cache aged out.
//
// Bounded cost: one shared deadline (startupSyncBudget) for the models feed, the
// models catalog and the version probes, all in parallel, so the worst case is
// the budget itself, not the sum. Every failure falls back to what is already
// there (disk cache, then compiled catalog). After a failed sync, restarts within
// startupFailBackoff skip that part so an offline machine does not pay the
// budget on every restart.
const (
	syncStateFile = "cline-sync-state.json"
)

var (
	startupSyncBudget  = 6 * time.Second
	startupFailBackoff = 10 * time.Minute

	startupSyncOnce sync.Once
)

// syncState is persisted next to the models cache so the backoff survives the
// restart it is meant to protect.
type syncState struct {
	ModelsFailedAt   time.Time `json:"models_failed_at,omitempty"`
	VersionsFailedAt time.Time `json:"versions_failed_at,omitempty"`
}

func syncStatePath() string {
	p := modelsSnapshotPath()
	if p == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p), syncStateFile)
}

func loadSyncState() syncState {
	var st syncState
	if p := syncStatePath(); p != "" {
		if raw, err := os.ReadFile(p); err == nil {
			_ = json.Unmarshal(raw, &st)
		}
	}
	return st
}

func saveSyncState(st syncState) {
	p := syncStatePath()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	raw, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err == nil {
		_ = os.Rename(tmp, p)
	}
}

func inBackoff(failedAt time.Time) bool {
	return !failedAt.IsZero() && time.Since(failedAt) < startupFailBackoff
}

// staleVersionKeys lists probe targets whose cache is missing or past its TTL.
func staleVersionKeys() []string {
	versionMu.RLock()
	defer versionMu.RUnlock()
	var keys []string
	for _, k := range []string{targetKeyDesktop, targetKeyCLI, targetKeyCore} {
		e, ok := versionCache[k]
		if !ok || e.Version == "" || time.Since(e.FetchedAt) >= versionCacheTTL {
			keys = append(keys, k)
		}
	}
	return keys
}

// RunStartupSync is called once, from plugin.register, after the config was
// applied (base_url / auth_dir matter) and before the registration is returned.
// plugin.reconfigure never calls it. Safe to call repeatedly: only the first
// call does anything.
func RunStartupSync() {
	startupSyncOnce.Do(runStartupSync)
}

func runStartupSync() {
	started := time.Now()
	modelsDiskLoadedOnce.Do(loadModelsSnapshotFromDisk)
	diskLoadedOnce.Do(loadVersionSnapshotFromDisk)

	st := loadSyncState()
	needModels := !modelsCacheFresh() && !inBackoff(st.ModelsFailedAt)
	var needVersions []string
	if !inBackoff(st.VersionsFailedAt) {
		needVersions = staleVersionKeys()
	}
	if !needModels && len(needVersions) == 0 {
		pluginDebugf("startup sync skipped (models fresh or in backoff, versions fresh)")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), startupSyncBudget)
	defer cancel()

	var (
		wg           sync.WaitGroup
		modelsOK     bool
		versionsFail bool
		mu           sync.Mutex
	)
	if needModels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok := syncModelsAtStartup(ctx)
			mu.Lock()
			modelsOK = ok
			mu.Unlock()
		}()
	}
	for _, key := range needVersions {
		key := key
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok := refreshVersionCtx(ctx, key)
			if !ok {
				mu.Lock()
				versionsFail = true
				mu.Unlock()
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(startupSyncBudget + 500*time.Millisecond):
		// Every probe honours ctx, so this is a belt-and-braces guard: register
		// must return even if some HTTP layer ignored the deadline.
		cancel()
	}

	mu.Lock()
	defer mu.Unlock()
	next := st
	now := time.Now()
	if needModels {
		if modelsOK {
			next.ModelsFailedAt = time.Time{}
		} else {
			next.ModelsFailedAt = now
		}
	}
	if len(needVersions) > 0 {
		if versionsFail {
			next.VersionsFailedAt = now
		} else {
			next.VersionsFailedAt = time.Time{}
		}
	}
	if next != st {
		saveSyncState(next)
	}
	modelsState := "skipped"
	if needModels {
		modelsState = map[bool]string{true: "refreshed", false: "failed(cache)"}[modelsOK]
	}
	pluginLogf("startup sync: models=%s versions_probed=%d versions_failed=%t took=%s",
		modelsState, len(needVersions), versionsFail, time.Since(started).Round(time.Millisecond))
}
