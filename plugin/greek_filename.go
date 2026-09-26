package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// greekMusicSequence holds the canonical Greek musical number names for
// n = 1..25 exactly as documented in cursor-for-cpa.
var greekMusicSequence = []string{
	"monochord", "dichord", "trichord", "tetrachord", "pentachord",
	"hexachord", "heptachord", "octachord", "enneachord", "decachord",
	"hendecachord", "dodecachord", "tridecachord", "tetradecachord",
	"pentadecachord", "hexadecachord", "heptadecachord", "octadecachord",
	"enneadecachord", "icosachord", "icosihenachord", "icosidichord",
	"icositrachord", "icositetrachord", "icosipentachord",
}

var greekUnits = map[int]string{
	1: "hen", 2: "di", 3: "tri", 4: "tetra", 5: "penta",
	6: "hexa", 7: "hepta", 8: "octa", 9: "ennea",
}

var greekTens = map[int]string{
	10: "deca", 20: "icosi", 30: "triaconta", 40: "tetraconta",
	50: "pentaconta", 60: "hexaconta", 70: "heptaconta",
	80: "octaconta", 90: "enneaconta",
}

var greekHundreds = map[int]string{
	100: "hecaton", 200: "diacosia", 300: "triacosia", 400: "tetracosia",
	500: "pentacosia", 600: "hexacosia", 700: "heptacosia",
	800: "octacosia", 900: "enneacosia",
}

// greekMusicName returns the lowercase Greek musical sequence name for a
// positive integer n (e.g. 1 -> monochord, 2 -> dichord, 12 -> dodecachord).
func greekMusicName(n int) string {
	if n >= 1 && n <= len(greekMusicSequence) {
		return greekMusicSequence[n-1]
	}
	if n == 1000 {
		return "chiliachord"
	}
	if n == 10000 {
		return "myriachord"
	}
	h := (n % 1000) / 100
	t := (n % 100) / 10
	u := n % 10
	var parts []string
	if h > 0 {
		if h == 1 && t == 0 && u == 0 {
			parts = append(parts, "hecta")
		} else {
			parts = append(parts, greekHundreds[h*100])
		}
	}
	if t == 1 {
		if u == 0 {
			parts = append(parts, "deca")
		} else {
			parts = append(parts, greekUnits[u]+"deca")
		}
	} else if t > 1 {
		if u == 0 {
			ten := greekTens[t*10]
			if strings.HasSuffix(ten, "i") {
				ten = ten[:len(ten)-1] + "a"
			}
			parts = append(parts, ten)
		} else {
			parts = append(parts, greekTens[t*10])
			parts = append(parts, greekUnitWithA(u))
		}
	} else if u > 0 {
		parts = append(parts, greekUnitWithA(u))
	}
	return strings.Join(parts, "") + "chord"
}

func greekUnitWithA(u int) string {
	unit := greekUnits[u]
	if strings.HasSuffix(unit, "a") {
		return unit
	}
	return unit + "a"
}

// greekNameSet is every legal cline-key-<greek>.json base name, built once.
//
// It used to be rebuilt by a 1000-iteration loop that called greekMusicName
// (which itself assembles the name from parts) for every candidate file, on
// every auth.parse / auth.refresh the host issues. That is 1000 string
// constructions per file per poll; the answer never changes, so it is computed
// exactly once here and the lookup is a map hit.
var greekNameSet = func() map[string]struct{} {
	set := make(map[string]struct{}, 1000)
	for n := 1; n <= 1000; n++ {
		set[greekMusicName(n)] = struct{}{}
	}
	return set
}()

// isGreekKeySequenceName reports whether name has the cline-key-<greek>.json shape.
func isGreekKeySequenceName(name string) bool {
	base := strings.TrimSuffix(strings.TrimPrefix(name, "cline-key-"), ".json")
	if base == name {
		return false
	}
	_, ok := greekNameSet[base]
	return ok
}

// keyCredentialFile describes a discovered cline-key-*.json file.
type keyCredentialFile struct {
	path   string
	name   string
	apiKey string
}

// scanKeyCredentialFiles lists cline-key-*.json files under authDir and reads
// their api_key field.
func scanKeyCredentialFiles(authDir string) []keyCredentialFile {
	if strings.TrimSpace(authDir) == "" {
		return nil
	}
	entries, err := os.ReadDir(authDir)
	if err != nil {
		return nil
	}
	var files []keyCredentialFile
	for _, entry := range entries {
		if entry == nil || entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "cline-key-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, errRead := os.ReadFile(filepath.Join(authDir, name))
		if errRead != nil {
			continue
		}
		var record struct {
			APIKey string `json:"api_key"`
		}
		if json.Unmarshal(data, &record) != nil {
			continue
		}
		files = append(files, keyCredentialFile{
			path:   filepath.Join(authDir, name),
			name:   name,
			apiKey: strings.TrimSpace(record.APIKey),
		})
	}
	return files
}

// resolveGreekKeyFileName is the PURE name resolver: given an auth dir and an
// API key it answers which file name the credential should have, and never
// touches the filesystem beyond reading it.
//
// It is deliberately separated from the migration below because it is called
// from credentialFileName → keyAuthFileName, i.e. from the auth.parse /
// auth.refresh hot path that the host hits every 30s per credential. A function
// whose name says "resolve" must not rename files: os.Rename raises
// NOTE_DELETE/NOTE_RENAME in the kernel, which reaches the host's fsnotify
// watcher as a Remove and makes it unregister the credential — the exact
// failure safeInPlaceWrite (persist.go) exists to prevent. This repository
// learned that in 0.3.16 and must not reintroduce it through a getter.
func resolveGreekKeyFileName(authDir, apiKey string) (string, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return "cline-key-monochord.json", nil
	}

	files := scanKeyCredentialFiles(authDir)
	occupied := make(map[string]struct{}, len(files)+1)
	var pendingMigration string

	for _, f := range files {
		occupied[f.name] = struct{}{}
		if f.apiKey != apiKey {
			continue
		}
		if isGreekKeySequenceName(f.name) {
			return f.name, nil
		}
		// A legacy cline-key-<last4>.json already holds this key: the canonical
		// name is still ours to compute, but moving the file is the migrator's
		// job, not the resolver's.
		pendingMigration = f.name
	}

	if pendingMigration != "" {
		delete(occupied, pendingMigration)
	}

	for n := 1; ; n++ {
		candidate := fmt.Sprintf("cline-key-%s.json", greekMusicName(n))
		if _, taken := occupied[candidate]; taken {
			continue
		}
		return candidate, nil
	}
}

// assignGreekKeyFileName returns the canonical cline-key-<greek>.json for an API key.
// If an existing file under authDir already contains this apiKey, its name is returned
// (or migrated if it was legacy cline-key-<last4>.json).
//
// Deprecated side-effect note: this wrapper still migrates. It is only called
// from migrateLegacyKeyFiles at config-apply time, never from a request path.
func assignGreekKeyFileName(authDir, apiKey string) (string, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return "cline-key-monochord.json", nil
	}
	if authDir == "" {
		return resolveGreekKeyFileName(authDir, apiKey)
	}

	var oldName string
	for _, f := range scanKeyCredentialFiles(authDir) {
		if f.apiKey == apiKey && !isGreekKeySequenceName(f.name) {
			oldName = f.name
			break
		}
	}
	target, err := resolveGreekKeyFileName(authDir, apiKey)
	if err != nil {
		return "", err
	}
	if oldName != "" && target != oldName {
		if err := migrateKeyFile(authDir, oldName, target); err != nil {
			return "", err
		}
	}
	return target, nil
}

// migrateKeyFile moves a legacy credential file to its canonical name without
// ever calling os.Rename on it.
//
// os.Rename is unsafe here (see resolveGreekKeyFileName): the host's fsnotify
// watcher sees the kernel's NOTE_DELETE and unregisters the credential, which
// presents to the user as a credential that vanishes on upgrade. Writing the
// destination first and unlinking the source keeps every mutation on a path the
// host watches as a Create/Write, and — unlike a rename — the credential is
// never in a state where it exists at neither path.
func migrateKeyFile(authDir, oldName, newName string) error {
	oldPath := filepath.Join(authDir, oldName)
	newPath := filepath.Join(authDir, newName)
	data, err := os.ReadFile(oldPath)
	if err != nil {
		return fmt.Errorf("read key credential %s: %w", oldName, err)
	}
	if err := safeInPlaceWrite(newPath, data); err != nil {
		return fmt.Errorf("migrate key credential %s -> %s: %w", oldName, newName, err)
	}
	if err := os.Remove(oldPath); err != nil {
		return fmt.Errorf("remove legacy key credential %s: %w", oldName, err)
	}
	return nil
}

// isLegacyLast4KeyFileName reports whether name has the cline-key-<4chars>.json shape (e.g. cline-key-ad9e.json).
func isLegacyLast4KeyFileName(name string) bool {
	prefix := "cline-key-"
	suffix := ".json"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return false
	}
	middle := name[len(prefix) : len(name)-len(suffix)]
	return len(middle) == 4
}

// migrateLegacyKeyFiles scans authDir for legacy cline-key-<last4>.json files
// and migrates them to cline-key-<greek>.json.
func migrateLegacyKeyFiles(authDir string) ([]string, error) {
	authDir = strings.TrimSpace(authDir)
	if authDir == "" {
		return nil, nil
	}
	files := scanKeyCredentialFiles(authDir)
	var migrated []string
	for _, f := range files {
		// Only migrate legacy cline-key-<last4>.json files
		if !isLegacyLast4KeyFileName(f.name) {
			continue
		}
		if f.apiKey == "" {
			continue
		}
		newName, err := assignGreekKeyFileName(authDir, f.apiKey)
		if err != nil {
			return migrated, err
		}
		if newName != f.name {
			migrated = append(migrated, fmt.Sprintf("%s -> %s", f.name, newName))
		}
	}
	return migrated, nil
}
