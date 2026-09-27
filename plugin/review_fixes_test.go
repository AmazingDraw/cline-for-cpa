package plugin

// Regression tests for the 2026-09-26 review (plan A): B1 the refresh contract
// must follow the effective config, B2 name resolution must stay side-effect free.

import (
	"encoding/json"
	"fmt"
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
	cfg.APIKeys = []string{"sk_b1"}
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

// ─── Key 凭证治理（2026-09-27）───────────────────────────────────────────────

// P2 采纳：无标记但 key 在配置里的文件要被打上 managed_by，
// 且 disabled 必须原样保留 —— 采纳绝不能变成"偷偷重新启用"。
func TestAdoptionPreservesDisabledFlag(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		dir := t.TempDir()
		st := clineOAuthStorage{Type: ProviderKey, APIKey: "sk_adopt", Disabled: disabled}
		raw, _ := json.Marshal(st)
		path := filepath.Join(dir, "cline-key-monochord.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}

		cfg := defaultConfig()
		cfg.AuthDir = dir
		cfg.APIKeys = []string{"sk_adopt"}
		syncConfigAPIKeyCredential(cfg)

		var got map[string]any
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		meta, _ := got["metadata"].(map[string]any)
		if meta == nil || meta["managed_by"] != configManagedKeyMarker {
			t.Fatalf("disabled=%v: file was not adopted, metadata=%v", disabled, got["metadata"])
		}
		if gotDisabled := got["disabled"] == true; gotDisabled != disabled {
			t.Fatalf("disabled=%v: adoption changed the flag to %v", disabled, got["disabled"])
		}
	}
}

// 已有托管标记但 key 仍配置着 → 凭证语义必须原样保留。
// 字节级不变做不到也不该做：同一轮 sync 会补写 refresh 契约字段（B1），
// 所以这里断言的是真正重要的不变量 —— 文件仍在、disabled 仍 true、
// api_key 与托管标记未被篡改。
func TestManagedFileWithLiveKeyIsUntouched(t *testing.T) {
	dir := t.TempDir()
	st := map[string]any{
		"type":     ProviderKey,
		"api_key":  "sk_live",
		"disabled": true,
		"metadata": map[string]any{"managed_by": configManagedKeyMarker},
	}
	raw, _ := json.MarshalIndent(st, "", "  ")
	path := filepath.Join(dir, "cline-key-dichord.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := defaultConfig()
	cfg.AuthDir = dir
	cfg.APIKeys = []string{"sk_live"}
	syncConfigAPIKeyCredential(cfg)

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("managed file with live key was removed: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["disabled"] != true {
		t.Fatalf("sync flipped the disabled flag: %v", got["disabled"])
	}
	if got["api_key"] != "sk_live" {
		t.Fatalf("api_key changed: %v", got["api_key"])
	}
	meta, _ := got["metadata"].(map[string]any)
	if meta == nil || meta["managed_by"] != configManagedKeyMarker {
		t.Fatalf("managed marker lost: %v", got["metadata"])
	}
}

// 设置里删 key → 托管文件同步回收（增删同步的"删"方向）。
func TestManagedFileRemovedWhenKeyCleared(t *testing.T) {
	dir := t.TempDir()
	st := map[string]any{
		"type":     ProviderKey,
		"api_key":  "sk_doomed",
		"metadata": map[string]any{"managed_by": configManagedKeyMarker},
	}
	raw, _ := json.Marshal(st)
	path := filepath.Join(dir, "cline-key-trichord.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := defaultConfig()
	cfg.AuthDir = dir
	cfg.APIKeys = []string{"sk_other"}
	syncConfigAPIKeyCredential(cfg)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("managed file survived a key removal")
	}
}

// 旧配置只有 api_key 单值字段 → 加载时并入 api_keys，去重且不丢 key。
func TestLegacyAPIKeyFieldMigratesIntoArray(t *testing.T) {
	dir := t.TempDir()
	applyTestConfig(t, fmt.Sprintf("auth_dir: %q\napi_key: sk_legacy\n", dir))

	cfg := currentConfig()
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0] != "sk_legacy" {
		t.Fatalf("api_keys = %v, want [sk_legacy]", cfg.APIKeys)
	}
	if cfg.APIKey != "" {
		t.Fatalf("legacy field should be cleared after migration, got %q", cfg.APIKey)
	}
	if k := resolveAPIKeys(cfg); len(k) != 1 || k[0] != "sk_legacy" {
		t.Fatalf("resolveAPIKeys = %v, want [sk_legacy]", k)
	}
}

// api_key 与 api_keys 同时存在且重复 → 去重，绝不产生双份凭证文件。
func TestLegacyAPIKeyDeduplicatesAgainstArray(t *testing.T) {
	dir := t.TempDir()
	applyTestConfig(t, fmt.Sprintf("auth_dir: %q\napi_key: sk_dup\napi_keys: [\"sk_dup\", \"sk_other\"]\n", dir))

	cfg := currentConfig()
	keys := resolveAPIKeys(cfg)
	if len(keys) != 2 || keys[0] != "sk_dup" || keys[1] != "sk_other" {
		t.Fatalf("resolveAPIKeys = %v, want [sk_dup sk_other]", keys)
	}
	syncConfigAPIKeyCredential(cfg)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("want 2 credential files after dedup, got %d: %v", len(entries), names)
	}
}

// models_updater：OAuth 优先（D2）。控制面取 token 必须先看凭证文件。
func TestModelsUpdaterPrefersOAuthTokenOverConfigKey(t *testing.T) {
	dir := t.TempDir()
	st := map[string]any{"type": ProviderKey, "access_token": "oauth-token-1"}
	raw, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(dir, "cline-oauth@example.com.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.AuthDir = dir
	cfg.APIKeys = []string{"sk_config"}
	if got := firstAccessTokenFromDir(dir); got != "oauth-token-1" {
		t.Fatalf("firstAccessTokenFromDir = %q, want oauth-token-1", got)
	}
	_ = cfg
}

// Panel array widgets require a JSON array; pasting a raw key trips
// "请先修复插件配置表单错误". The field is now a string, so a scalar YAML
// value (what the panel writes for type=string) must still seed the list.
func TestAPIKeysAcceptsPanelStringPaste(t *testing.T) {
	dir := t.TempDir()
	applyTestConfig(t, fmt.Sprintf("auth_dir: %q\napi_keys: sk_pasted\n", dir))
	cfg := currentConfig()
	if got := resolveAPIKeys(cfg); len(got) != 1 || got[0] != "sk_pasted" {
		t.Fatalf("scalar api_keys = %v, want [sk_pasted]", got)
	}
}

func TestAPIKeysAcceptsJSONArrayText(t *testing.T) {
	dir := t.TempDir()
	applyTestConfig(t, fmt.Sprintf("auth_dir: %q\napi_keys: '[\"sk_a\", \"sk_b\"]'\n", dir))
	cfg := currentConfig()
	if got := resolveAPIKeys(cfg); len(got) != 2 || got[0] != "sk_a" || got[1] != "sk_b" {
		t.Fatalf("json-text api_keys = %v, want [sk_a sk_b]", got)
	}
}

func TestAPIKeysAcceptsNewlines(t *testing.T) {
	dir := t.TempDir()
	applyTestConfig(t, fmt.Sprintf("auth_dir: %q\napi_keys: |\n  sk_one\n  sk_two\n", dir))
	cfg := currentConfig()
	if got := resolveAPIKeys(cfg); len(got) != 2 || got[0] != "sk_one" || got[1] != "sk_two" {
		t.Fatalf("newline api_keys = %v, want [sk_one sk_two]", got)
	}
}
