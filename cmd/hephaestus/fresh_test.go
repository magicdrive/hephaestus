package hephaestus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// With --fresh, archives and checksum files a previous run wrote are removed
// before they are written again, so dist/ holds only the new version. Files
// Hephaestus did not write, or that changed since, are kept.
func TestFresh_RemovesStaleOutputs(t *testing.T) {
	clearGitHubEnv(t)
	root := t.TempDir()
	write := func(rel, data string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("hephaestus.yaml", `version: 1
project:
  name: tool
archives:
  - id: tool
    name: "{{ .Project }}_{{ .Version }}"
    targets: [{os: all, arch: all}]
    files:
      - {src: tool.sh, dst: tool.sh, mode: "0755"}
checksum:
  name: "{{ .Project }}_{{ .Version }}_checksums.txt"
`)
	write("tool.sh", "#!/bin/sh\necho tool\n")
	cfg := filepath.Join(root, "hephaestus.yaml")
	run := func(args ...string) {
		t.Helper()
		if code, out, errs := runCLI(t, append([]string{"run", "--config", cfg, "--stages", "archive,checksum"}, args...)...); code != 0 {
			t.Fatalf("run %v: %d\n%s%s", args, code, out, errs)
		}
	}
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		return err == nil
	}

	run("--tag", "v1.0.0")
	if !exists("dist/tool_1.0.0.tar.gz") || !exists("dist/tool_1.0.0_checksums.txt") {
		t.Fatal("first run wrote no archive")
	}
	write("dist/notes.txt", "kept: not written by Hephaestus")

	// Without --fresh another version is refused, and nothing is removed.
	if code, _, errs := runCLI(t, "run", "--config", cfg, "--stages", "archive,checksum", "--tag", "v1.1.0"); code == 0 || !strings.Contains(errs, "--fresh") {
		t.Fatalf("a manifest of another version was continued: %d %s", code, errs)
	}
	if !exists("dist/tool_1.0.0.tar.gz") {
		t.Fatal("a run without --fresh removed the old archive")
	}

	run("--tag", "v1.1.0", "--fresh")
	for _, gone := range []string{"dist/tool_1.0.0.tar.gz", "dist/tool_1.0.0_checksums.txt"} {
		if exists(gone) {
			t.Errorf("%s is still in dist/", gone)
		}
	}
	for _, kept := range []string{"dist/tool_1.1.0.tar.gz", "dist/tool_1.1.0_checksums.txt", "dist/notes.txt"} {
		if !exists(kept) {
			t.Errorf("%s is missing", kept)
		}
	}

	// An output modified since the run that wrote it is not deleted.
	write("dist/tool_1.1.0_checksums.txt", "edited by hand\n")
	run("--tag", "v1.2.0", "--fresh")
	if !exists("dist/tool_1.1.0_checksums.txt") {
		t.Error("a modified file was deleted")
	}
	if exists("dist/tool_1.1.0.tar.gz") {
		t.Error("the unmodified 1.1.0 archive is still in dist/")
	}

	// A run of other stages (here checksum only) does not remove the
	// archives it does not write again.
	run("--tag", "v1.3.0", "--fresh")
	if !exists("dist/tool_1.3.0.tar.gz") {
		t.Fatal("no 1.3.0 archive")
	}
	if code, out, errs := runCLI(t, "run", "--config", cfg, "--stages", "checksum", "--tag", "v1.3.0", "--fresh"); code == 0 {
		t.Logf("checksum-only fresh run: %s%s", out, errs)
	}
	if !exists("dist/tool_1.3.0.tar.gz") {
		t.Error("a run that does not archive removed an archive")
	}
}
