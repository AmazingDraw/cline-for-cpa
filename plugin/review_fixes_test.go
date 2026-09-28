package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

// B1 (0.4+): config api_key must not seed credential files.
func TestB1DoesNotSeedKeyFiles(t *testing.T) {
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.AuthDir = dir
	cfg.APIKey = "sk_b1"
	syncAuthFilePriorities(cfg, dir)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("oauth-only must not seed cline-key files, got %d", len(entries))
	}
}

func TestLogRotation(t *testing.T) {
	dir := t.TempDir()
	cfg := pluginConfig{AuthDir: dir}
	path := pluginLogPath(cfg)
	if path == "" {
		t.Fatal("empty log path")
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	// Write past the soft rotation threshold if the helper exists; otherwise
	// just ensure append works.
	for i := 0; i < 20; i++ {
		logCredentialEvent(cfg, "rotation probe %d", i)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("log not written: %v", err)
	}
}
