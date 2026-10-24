package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestV020ConfigsStillLoad pins the compatibility contract: configurations
// that worked with v0.2.0 (Hephaestus's own and magicdrive/t's, copied
// verbatim into testdata/compat/v0.2.0) load unchanged and resolve the same
// way. Breaking this test is a breaking change (docs/compatibility/policy.md).
func TestV020ConfigsStillLoad(t *testing.T) {
	for _, tc := range []struct {
		file     string
		project  string
		archives int
		brews    int
		notes    string
	}{
		{"hephaestus.yaml", "hephaestus", 1, 1, "RELEASE_NOTES.md"},
		{"hephaestus.rehearsal.yaml", "hephaestus", 1, 1, "RELEASE_NOTES.md"},
		{"t.yaml", "t", 1, 1, ""},
	} {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "compat", "v0.2.0", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(strings.NewReader(string(data)), t.TempDir())
			if err != nil {
				t.Fatalf("a v0.2.0 configuration no longer loads: %v", err)
			}
			if cfg.Project.Name != tc.project || len(cfg.Archives) != tc.archives || len(cfg.Brews) != tc.brews {
				t.Errorf("project %q, %d archives, %d brews", cfg.Project.Name, len(cfg.Archives), len(cfg.Brews))
			}
			if tc.notes != "" && (cfg.Release.GitHub == nil || cfg.Release.GitHub.NotesFile != tc.notes) {
				t.Errorf("notes_file %+v", cfg.Release.GitHub)
			}
		})
	}
}

func TestSchemaVersion(t *testing.T) {
	// 0 (or omitted) means 1; any other version is rejected.
	for v, ok := range map[string]bool{"1": true, "2": false, "0": true} {
		_, err := Load(strings.NewReader("version: "+v+"\nproject: {name: x}\n"), t.TempDir())
		if (err == nil) != ok {
			t.Errorf("version %s: err %v", v, err)
		}
	}
}
