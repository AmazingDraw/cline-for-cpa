package plugin

// Regression tests for the 2026-09-26 review (plan A): B1 the refresh contract
// must follow the effective config, B2 name resolution must stay side-effect free.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// B1: a non-default refresh_interval_seconds must reach the key credential file.
func TestB1RefreshIntervalReachesKeyFile(t *testing.T) {
	dir := t.TempDir()
	v := 1200
	cfg := defaultConfig()
	cfg.AuthDir = dir
	cfg.APIKey = "sk_b1"
	cfg.HostRefreshIntervalSeconds = &v
	syncConfigAPIKeyCredential(cfg)

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("want 1 credential file, got %d", len(entries))
	}
	raw, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	var got map[string]any
	json.Unmarshal(raw, &got)
	if n := int(got["refresh_interval_seconds"].(float64)); n != 1200 {
		t.Fatalf("B1 not fixed: file says %d, want 1200", n)
	}

	// Re-apply with a different value: the existing file must be updated too.
	v2 := 900
	cfg.HostRefreshIntervalSeconds = &v2
	syncConfigAPIKeyCredential(cfg)
	raw, _ = os.ReadFile(filepath.Join(dir, entries[0].Name()))
	json.Unmarshal(raw, &got)
	if n := int(got["refresh_interval_seconds"].(float64)); n != 900 {
		t.Fatalf("B1 not fixed on update: file says %d, want 900", n)
	}
}

// B2: resolving a name must never move files; only the migrator may.
func TestB2ResolverIsSideEffectFree(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "cline-key-ad9e.json")
	if err := os.WriteFile(legacy, []byte(`{"api_key":"sk_legacy","type":"cline"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	name, err := resolveGreekKeyFileName(dir, "sk_legacy")
	if err != nil {
		t.Fatal(err)
	}
	if name != "cline-key-monochord.json" {
		t.Fatalf("resolver returned %q, want cline-key-monochord.json", name)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("B2 not fixed: resolver mutated the filesystem: %v", err)
	}

	// The migrator, by contrast, must move it.
	if _, err := assignGreekKeyFileName(dir, "sk_legacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("migrator did not move the legacy file")
	}
	if _, err := os.Stat(filepath.Join(dir, "cline-key-monochord.json")); err != nil {
		t.Fatalf("migrator did not create the canonical file: %v", err)
	}
}

// A-prime: the plugin log must not grow without bound.
func TestLogRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cline-for-cpa.log")

	if err := os.WriteFile(path, make([]byte, logMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	rotateLogIfNeeded(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("oversized log was not rotated away")
	}
	rotated, err := os.Stat(path + ".1")
	if err != nil || rotated.Size() != logMaxBytes+1 {
		t.Fatalf("rotated file wrong: %v size=%v", err, rotated.Size())
	}

	// Under the cap: untouched.
	if err := os.WriteFile(path, []byte("small"), 0o600); err != nil {
		t.Fatal(err)
	}
	rotateLogIfNeeded(path)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("small log must not be rotated")
	}
}

// A-prime: the greek name set must agree with greekMusicName for every n.
func TestGreekNameSetComplete(t *testing.T) {
	if len(greekNameSet) != 1000 {
		t.Fatalf("greekNameSet has %d entries, want 1000", len(greekNameSet))
	}
	for n := 1; n <= 1000; n++ {
		if !isGreekKeySequenceName("cline-key-" + greekMusicName(n) + ".json") {
			t.Fatalf("n=%d (%s) not recognised", n, greekMusicName(n))
		}
	}
}
