package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Refreshed credentials MUST be written back to our auth file.
//
// Cline rotates the refresh token on every refresh. The executor path refreshes
// credentials on its own initiative, and the ABI only carries updated storage
// back through auth.refresh — so without an explicit write the file keeps the
// already-rotated (dead) refresh token. The host's periodic auth refresh then
// presents that dead token, gets invalid_grant, and reports "re-authenticate"
// even though the session is perfectly healthy. That failure mode cost a live
// session on 2026-09-23 (503 burst → forced manual re-auth).

// authFilePath resolves the auth file a credential belongs to (OAuth email slug).
func authFilePath(cfg pluginConfig, st *clineOAuthStorage) string {
	if st == nil {
		return ""
	}
	dir := clineAuthDir(cfg)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, credentialFileName(st))
}

// persistRefreshedStorage atomically writes a rotated credential back to disk,
// merging into whatever the host stored so unrelated metadata survives.
func persistRefreshedStorage(cfg pluginConfig, st *clineOAuthStorage) error {
	path := authFilePath(cfg, st)
	if path == "" {
		return fmt.Errorf("cannot resolve auth file for credential")
	}
	merged := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		var existing map[string]any
		if err := json.Unmarshal(raw, &existing); err == nil && existing != nil {
			merged = existing
		}
	}
	encoded, err := json.Marshal(st)
	if err != nil {
		return err
	}
	var fresh map[string]any
	if err := json.Unmarshal(encoded, &fresh); err != nil {
		return err
	}
	for k, v := range fresh {
		if s, ok := v.(string); ok && s == "" {
			continue // never blank out an existing field
		}
		merged[k] = v
	}
	// Anti-pollution: never persist OAuth tokens into a cline-key-*.json file!
	isKeyFile := strings.HasPrefix(filepath.Base(path), "cline-key-")
	if isKeyFile {
		delete(merged, "refresh_token")
		delete(merged, "access_token")
		delete(merged, "expires_at")
	}

	isOAuth := !isKeyFile && (strings.TrimSpace(st.RefreshToken) != "" || strings.TrimSpace(st.AccessToken) != "")
	merged["priority"] = priorityForCredential(cfg.CredentialPreference, isOAuth)
	out, err := json.MarshalIndent(merged, "", " ")
	if err != nil {
		return err
	}
	if raw, err := os.ReadFile(path); err == nil && bytes.Equal(raw, out) {
		return nil // skip unchanged write to prevent unneeded file watcher churn
	}
	return safeInPlaceWrite(path, out)
}

// safeInPlaceWrite updates a file in place without renaming.
//
// In-place truncation and writing preserves the existing inode on macOS/Darwin.
// This is critical because replacing files via os.Rename triggers NOTE_DELETE /
// NOTE_RENAME in the kernel, surfacing to fsnotify as fsnotify.Remove. The host's
// file watcher interprets that as credential deletion and unregisters the client.
// An in-place write surfaces as fsnotify.Write only, which is safe against removal.
func safeInPlaceWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

// pluginLogPath is the plugin-owned log file. The host does not forward plugin
// stderr to a readable file, which left credential incidents undiagnosable.
func pluginLogPath(cfg pluginConfig) string {
	dir := clineAuthDir(cfg)
	if dir == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(dir), "logs", "cline-for-cpa.log")
}

// logThrottled emits an incident line at most once per key per interval.
//
// Credential problems are frequently per-request events: without suppression a
// broken credential would rewrite the plugin log on every call, which hides the
// first occurrence instead of surfacing it. Keys are namespaced by event kind,
// so unrelated lines can never suppress each other.
var (
	throttledLogMu   sync.Mutex
	throttledLogSeen = map[string]time.Time{}
)

func logThrottled(cfg pluginConfig, key string, interval time.Duration, format string, args ...any) {
	key = strings.TrimSpace(key)
	now := time.Now()
	throttledLogMu.Lock()
	if key != "" {
		if last, seen := throttledLogSeen[key]; seen && now.Sub(last) < interval {
			throttledLogMu.Unlock()
			return
		}
		throttledLogSeen[key] = now
	}
	throttledLogMu.Unlock()
	logCredentialEvent(cfg, format, args...)
}

// logHostPollWithoutRefresh records a host poll that needed no upstream call.
//
// It exists to make the 0.2.8 refresh contract observable in production: the host
// only wakes an auth up early if it honoured the published
// Metadata["refresh_interval_seconds"], so seeing this line *before* the token is
// near expiry is the positive proof that the contract took effect. One line per
// credential per hour keeps the signal without turning the log into a heartbeat.
func logHostPollWithoutRefresh(cfg pluginConfig, st *clineOAuthStorage) {
	if st == nil {
		return
	}
	logThrottled(cfg, "host-poll:"+credentialKey(st), time.Hour,
		"host polled auth.refresh (refresh contract active) account=%s expires_in=%dm — credential still fresh, no upstream call",
		firstNonEmpty(st.Email, st.AccountID, diskProviderID), minutesUntil(st.ExpiresAt))
}

// logCredentialEvent appends a timestamped line to the plugin log (best effort).
func logCredentialEvent(cfg pluginConfig, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	pluginLogf("%s", msg)
	path := pluginLogPath(cfg)
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	rotateLogIfNeeded(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
}

// logMaxBytes caps the plugin log. The log is append-only by design — it is the
// only place credential incidents are diagnosable, since the host does not
// forward plugin stderr anywhere readable — but nothing ever trimmed it, so a
// long-lived NAS deployment grew the file without bound.
const logMaxBytes = 5 << 20 // 5 MiB

// rotateLogIfNeeded keeps path, plus at most one generation of history at
// path+".1". A single generation is deliberate: these logs are read to answer
// "what happened just before the failure", and a deep history costs disk on a
// NAS for no diagnostic gain.
//
// Best effort by design. Logging must never be able to fail a request, so every
// error here is swallowed — a log that cannot be rotated is a nuisance, not an
// incident. The rename is done with os.Rename because the log lives outside the
// auth dir: no fsnotify watcher is keyed on it, so the NOTE_DELETE that
// safeInPlaceWrite exists to dodge does not apply here.
func rotateLogIfNeeded(path string) {
	fi, err := os.Stat(path)
	if err != nil || fi.Size() < logMaxBytes {
		return
	}
	_ = os.Remove(path + ".1")
	_ = os.Rename(path, path+".1")
}
