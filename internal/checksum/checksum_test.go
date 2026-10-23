package checksum

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

func setup(t *testing.T) (Input, string) {
	t.Helper()
	root := t.TempDir()
	cfg, err := config.Load(strings.NewReader("project: {name: p}\n"), root)
	if err != nil {
		t.Fatal(err)
	}
	m := manifest.New("p", "1.0.0", "v1.0.0", "")
	os.MkdirAll(filepath.Join(root, "dist"), 0o755)
	for _, n := range []string{"p_linux.tar.gz", "p_darwin.tar.gz"} {
		data := []byte("content of " + n)
		os.WriteFile(filepath.Join(root, "dist", n), data, 0o644)
		sum := sha256.Sum256(data)
		m.Put(manifest.Artifact{Kind: manifest.KindArchive, ID: "default", Name: n, Path: "dist/" + n, OS: strings.Split(strings.TrimSuffix(n, ".tar.gz"), "_")[1], Arch: "amd64", Format: "tar.gz", SHA256: hex.EncodeToString(sum[:]), Source: manifest.Source{Stage: "archive"}})
	}
	return Input{Config: cfg, Manifest: m, Vars: tmpl.Vars{Project: "p", Version: "1.0.0"}}, root
}

func TestRun(t *testing.T) {
	in, root := setup(t)
	a, err := Run(in)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "dist", "p_1.0.0_checksums.txt"))
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "  p_darwin.tar.gz") || !strings.HasSuffix(lines[1], "  p_linux.tar.gz") {
		t.Fatalf("checksums file:\n%s", got)
	}
	if a.Name != "p_1.0.0_checksums.txt" || len(in.Manifest.Filter(manifest.KindChecksum)) != 1 {
		t.Errorf("artifact = %+v", a)
	}
	// Idempotent.
	if _, err := Run(in); err != nil || len(in.Manifest.Filter(manifest.KindChecksum)) != 1 {
		t.Errorf("rerun: %v", err)
	}
}

func TestTamperDetected(t *testing.T) {
	in, root := setup(t)
	os.WriteFile(filepath.Join(root, "dist", "p_linux.tar.gz"), []byte("evil"), 0o644)
	if _, err := Run(in); err == nil || !strings.Contains(err.Error(), "modified after") {
		t.Fatalf("err = %v", err)
	}
}

func TestDisabledAndEmpty(t *testing.T) {
	in, _ := setup(t)
	in.Config.Checksum.Disable = true
	if a, err := Run(in); a != nil || err != nil {
		t.Error("disabled stage did work")
	}
	in.Config.Checksum.Disable = false
	in.Manifest.RemoveKind(manifest.KindArchive)
	if _, err := Run(in); err == nil {
		t.Error("expected error with no archives")
	}
}

// F7: when dist is a link out of the project root, the stage neither reads
// archives nor writes the checksums file there.
func TestDistLinkOutsideRoot(t *testing.T) {
	in, root := setup(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Rename(filepath.Join(root, "dist"), outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dist")); err != nil {
		t.Skip(err)
	}
	if _, err := Run(in); err == nil {
		t.Fatal("checksum stage worked through a dist link that leaves the root")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 2 {
		t.Errorf("outside directory changed: %v", entries)
	}
	// Only the checksums file location escaping is also refused.
	in2, root2 := setup(t)
	out2 := t.TempDir()
	in2.Config.Checksum.Name = "sums.txt"
	if err := os.Symlink(filepath.Join(out2, "sums.txt"), filepath.Join(root2, "dist", "sums.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(in2); err != nil {
		t.Fatal(err) // the link itself is replaced, never written through
	}
	if entries, _ := os.ReadDir(out2); len(entries) != 0 {
		t.Errorf("wrote through the link: %v", entries)
	}
}

func TestFileSHA256In(t *testing.T) {
	_, root := setup(t)
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := FileSHA256In(r, "dist/p_linux.tar.gz"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"../x", "/etc/passwd"} {
		if _, err := FileSHA256In(r, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
