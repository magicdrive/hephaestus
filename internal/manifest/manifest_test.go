package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sum = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func sample() *Manifest {
	m := New("mycli", "1.2.3", "v1.2.3", "abc")
	m.Put(Artifact{Kind: KindArchive, ID: "default", Name: "b.tar.gz", Path: "dist/b.tar.gz", OS: "linux", Arch: "amd64", Format: "tar.gz", SHA256: sum,
		Executables: []string{"mycli"}, Files: []File{{Path: "mycli", Type: "file", Mode: "0755", SHA256: sum}}, Source: Source{Stage: "archive"}})
	m.Put(Artifact{Kind: KindBinary, ID: "default", Name: "mycli", Path: "out/mycli", OS: "linux", Arch: "amd64", Source: Source{Stage: "build", Provider: "command"}})
	m.Put(Artifact{Kind: KindArchive, ID: "default", Name: "a.tar.gz", Path: "dist/a.tar.gz", OS: "darwin", Arch: "arm64", Format: "tar.gz", SHA256: sum, Source: Source{Stage: "archive"}})
	return m
}

func TestRoundTripAndDeterminism(t *testing.T) {
	a, err := sample().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// Different insertion order serializes identically.
	m := sample()
	m.Artifacts[0], m.Artifacts[2] = m.Artifacts[2], m.Artifacts[0]
	b, _ := m.Marshal()
	if !bytes.Equal(a, b) {
		t.Error("serialization depends on insertion order")
	}
	back, err := Read(bytes.NewReader(a))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := back.Marshal()
	if !bytes.Equal(a, c) {
		t.Error("round trip changed the document")
	}
	if !strings.HasSuffix(string(a), "}\n") || strings.Index(string(a), `"binary"`) > strings.Index(string(a), `"archive"`) {
		t.Errorf("unexpected layout:\n%s", a)
	}
}

func TestReadRejects(t *testing.T) {
	good, _ := sample().Marshal()
	cases := map[string]string{
		"future schema":  strings.Replace(string(good), `"schema_version": 1`, `"schema_version": 2`, 1),
		"unknown field":  strings.Replace(string(good), `"project"`, `"extra": 1, "project"`, 1),
		"trailing":       string(good) + "{}",
		"bad sha":        strings.Replace(string(good), sum, strings.ToUpper(sum), 1),
		"path traversal": strings.Replace(string(good), `"dist/a.tar.gz"`, `"../a.tar.gz"`, 1),
		"bad kind":       strings.Replace(string(good), `"kind": "binary"`, `"kind": "exe"`, 1),
		"not json":       "nope",
	}
	for name, doc := range cases {
		if _, err := Read(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidate(t *testing.T) {
	m := sample()
	m.Artifacts = append(m.Artifacts, m.Artifacts[0])
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), "duplicates") {
		t.Errorf("duplicate: %v", err)
	}
	m = sample()
	m.Put(Artifact{Kind: KindArchive, ID: "x", Name: "c.zip", Path: "dist/c.zip", OS: "linux", Arch: "amd64", Format: "rar", Source: Source{Stage: "archive"}})
	if err := m.Validate(); err == nil {
		t.Error("archive without sha256/with bad format accepted")
	}
}

func TestFilterRemoveAndAtomicWrite(t *testing.T) {
	m := sample()
	if n := len(m.Filter(KindArchive)); n != 2 {
		t.Errorf("Filter = %d", n)
	}
	m.RemoveKind(KindArchive, "other")
	if n := len(m.Filter(KindArchive)); n != 2 {
		t.Error("RemoveKind removed other ids")
	}
	m.RemoveKind(KindArchive)
	if n := len(m.Filter(KindArchive)); n != 0 {
		t.Error("RemoveKind kept artifacts")
	}
	p := filepath.Join(t.TempDir(), "sub", "m.json")
	if err := sample().WriteFile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(p); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Errorf("temporary files left: %v", entries)
	}
}

func FuzzRead(f *testing.F) {
	good, _ := sample().Marshal()
	f.Add(string(good))
	f.Fuzz(func(t *testing.T, s string) {
		m, err := Read(strings.NewReader(s))
		if err != nil {
			return
		}
		out, err := m.Marshal()
		if err != nil {
			t.Fatalf("valid manifest failed to marshal: %v", err)
		}
		if _, err := Read(bytes.NewReader(out)); err != nil {
			t.Fatalf("re-read failed: %v", err)
		}
	})
}
