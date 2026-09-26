package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestGreekMusicName(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{1, "monochord"},
		{2, "dichord"},
		{3, "trichord"},
		{4, "tetrachord"},
		{5, "pentachord"},
		{6, "hexachord"},
		{7, "heptachord"},
		{8, "octachord"},
		{9, "enneachord"},
		{10, "decachord"},
		{11, "hendecachord"},
		{12, "dodecachord"},
		{20, "icosachord"},
		{1000, "chiliachord"},
	}
	for _, tc := range cases {
		if got := greekMusicName(tc.n); got != tc.want {
			t.Errorf("greekMusicName(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestAssignGreekKeyFileName(t *testing.T) {
	dir := t.TempDir()

	name1, err := assignGreekKeyFileName(dir, "sk-1111")
	if err != nil {
		t.Fatal(err)
	}
	if name1 != "cline-key-monochord.json" {
		t.Fatalf("want monochord, got %q", name1)
	}

	// Create monochord
	_ = os.WriteFile(filepath.Join(dir, name1), []byte(`{"api_key":"sk-1111"}`), 0o600)

	// Second key should get dichord
	name2, err := assignGreekKeyFileName(dir, "sk-2222")
	if err != nil {
		t.Fatal(err)
	}
	if name2 != "cline-key-dichord.json" {
		t.Fatalf("want dichord, got %q", name2)
	}

	// First key asked again should return same monochord
	name1Again, err := assignGreekKeyFileName(dir, "sk-1111")
	if err != nil {
		t.Fatal(err)
	}
	if name1Again != "cline-key-monochord.json" {
		t.Fatalf("want monochord again, got %q", name1Again)
	}
}

func TestMigrateLegacyKeyFiles(t *testing.T) {
	dir := t.TempDir()
	legacyFile := filepath.Join(dir, "cline-key-ad9e.json")
	_ = os.WriteFile(legacyFile, []byte(`{"api_key":"sk-ad9e"}`), 0o600)

	migrated, err := migrateLegacyKeyFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrated) != 1 {
		t.Fatalf("want 1 migration, got %d", len(migrated))
	}

	// Legacy file should not exist, monochord should exist
	if _, err := os.Stat(legacyFile); !os.IsNotExist(err) {
		t.Error("legacy file still exists")
	}
	newFile := filepath.Join(dir, "cline-key-monochord.json")
	raw, err := os.ReadFile(newFile)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	_ = json.Unmarshal(raw, &data)
	if data["api_key"] != "sk-ad9e" {
		t.Fatalf("unexpected content: %s", string(raw))
	}
}
