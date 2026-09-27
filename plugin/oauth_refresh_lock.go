package plugin

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// Cross-process OAuth refresh lock.
//
// Ported from the official CLI (cline/cline, Apache-2.0):
// sdk/packages/core/src/runtime/orchestration/oauth-refresh-lock.ts
//
// Official semantics we must preserve:
//   - one exclusive holder per provider while credentials are read/refreshed/saved;
//   - the lock is an OS file lock, so a crashed holder releases it automatically;
//   - the lock file holds no credentials and is never deleted while running;
//   - the wait is bounded (60s) and non-blocking between attempts.
//
// Deviation: the official implementation uses SQLite (`BEGIN EXCLUSIVE`) purely
// to obtain an OS file lock. Go reaches the same primitive with flock(2), which
// is lighter and dependency-free on both supported platforms (darwin, linux).
const (
	oauthLockTimeout      = 60 * time.Second
	oauthLockPollInterval = 25 * time.Millisecond
	// diskProviderID is this plugin's credential identity on disk.
	diskProviderID = "cline"
	// desktopStoreProviderIDs are the provider keys the Cline desktop app, CLI
	// and hub serialize on inside ~/.cline/data/settings/providers.json.
	desktopStoreDirRelative = ".cline/data/settings"
)

// clineAuthDir resolves the auth-file directory whose mutations we serialize.
func clineAuthDir(cfg pluginConfig) string {
	if dir := strings.TrimSpace(cfg.AuthDir); dir != "" {
		return dir
	}
	if fi, err := os.Stat("/app/auths"); err == nil && fi.IsDir() {
		return "/app/auths"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cli-proxy-api", "auths")
}

// oauthLockPath names a lock exactly like the official implementation:
// <owner>.oauth-<sha256(providerId)>.lock
func oauthLockPath(dir, owner, providerID string) string {
	sum := sha256.Sum256([]byte(providerID))
	return filepath.Join(dir, fmt.Sprintf("%s.oauth-%x.lock", owner, sum))
}

// refreshLockPaths returns every lock that must be held for one refresh, in a
// stable order so concurrent acquisitions can never deadlock.
func refreshLockPaths(cfg pluginConfig) []string {
	var paths []string
	if dir := clineAuthDir(cfg); dir != "" {
		paths = append(paths, oauthLockPath(dir, diskProviderID, diskProviderID))
	}
	if cfg.ShareDesktopStore {
		if home, err := os.UserHomeDir(); err == nil {
			storeDir := filepath.Join(home, desktopStoreDirRelative)
			if _, err := os.Stat(storeDir); err == nil {
				for _, pid := range []string{"cline", "cline-pass"} {
					paths = append(paths, oauthLockPath(storeDir, "providers.json", pid))
				}
			}
		}
	}
	sort.Strings(paths)
	return paths
}

// fileLock is one held OS file lock.
type fileLock struct {
	file *os.File
	path string
}

func acquireFileLock(path string, deadline time.Time) (*fileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &fileLock{file: f, path: path}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close()
			return nil, fmt.Errorf("flock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("timed out waiting for another process to refresh OAuth credentials")
		}
		time.Sleep(oauthLockPollInterval)
	}
}

func (l *fileLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
	l.file = nil
}

// withOAuthRefreshLock runs fn while holding every refresh lock this config
// requires (its own auth store, plus the official desktop store when enabled).
func withOAuthRefreshLock(cfg pluginConfig, fn func() error) error {
	paths := refreshLockPaths(cfg)
	if len(paths) == 0 {
		return fn()
	}
	deadline := time.Now().Add(oauthLockTimeout)
	held := make([]*fileLock, 0, len(paths))
	defer func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i].release()
		}
	}()
	for _, p := range paths {
		l, err := acquireFileLock(p, deadline)
		if err != nil {
			return err
		}
		held = append(held, l)
	}
	return fn()
}

// reloadStorageFromDisk re-reads our auth file under the lock so a rotation by
// another holder is reused instead of being raced (official: "Another holder may
// already have rotated the token while we waited"). Best effort: when the file
// cannot be located we keep the caller's snapshot.
func reloadStorageFromDisk(cfg pluginConfig, st *clineOAuthStorage) *clineOAuthStorage {
	if st == nil {
		return st
	}
	dir := clineAuthDir(cfg)
	if dir == "" {
		return st
	}
	name := ""
	if st.Email != "" {
		name = clineAuthFileName(st.Email)
	}
	if name == "" {
		return st
	}
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return st
	}
	var onDisk clineOAuthStorage
	if err := json.Unmarshal(unwrapJSONBytes(raw), &onDisk); err != nil {
		return st
	}
	if strings.TrimSpace(onDisk.RefreshToken) == "" && strings.TrimSpace(onDisk.AccessToken) == "" {
		return st
	}
	return &onDisk
}
