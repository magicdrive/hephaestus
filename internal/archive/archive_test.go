package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// fixture creates files under a temporary project root. A value starting
// with "->" creates a symlink to the rest of the value; a key ending in "*"
// is made executable.
func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, "*") {
			name = strings.TrimSuffix(name, "*")
			mode = 0o755
		}
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if target, ok := strings.CutPrefix(content, "->"); ok {
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func loadConfig(t *testing.T, root, yml string) *config.Config {
	t.Helper()
	cfg, err := config.Load(strings.NewReader(yml), root)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func input(cfg *config.Config) Input {
	return Input{
		Config:   cfg,
		Manifest: manifest.New(cfg.Project.Name, "1.2.3", "v1.2.3", ""),
		Vars:     tmpl.Vars{Project: cfg.Project.Name, ProjectName: cfg.Project.Name, Version: "1.2.3", Tag: "v1.2.3"},
	}
}

type member struct {
	typ     string
	mode    int64
	content string
	mtime   time.Time
	uid     int
	uname   string
}

func readArchive(t *testing.T, path string) (names []string, members map[string]member) {
	t.Helper()
	members = map[string]member{}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(path, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range zr.File {
			m := member{typ: "file", mode: int64(f.Mode().Perm()), mtime: f.Modified.UTC()}
			if f.FileInfo().IsDir() {
				m.typ = "dir"
			} else {
				rc, err := f.Open()
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(rc)
				rc.Close()
				m.content = string(b)
			}
			name := strings.TrimSuffix(f.Name, "/")
			names = append(names, name)
			members[name] = m
		}
		return names, members
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if gz.Name != "" || !gz.ModTime.IsZero() {
		t.Errorf("gzip header carries name %q / mtime %v", gz.Name, gz.ModTime)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		m := member{typ: "file", mode: h.Mode, mtime: h.ModTime.UTC(), uid: h.Uid, uname: h.Uname}
		switch h.Typeflag {
		case tar.TypeDir:
			m.typ = "dir"
		case tar.TypeReg:
			b, _ := io.ReadAll(tr)
			m.content = string(b)
		default:
			t.Fatalf("unexpected tar entry type %c for %s", h.Typeflag, h.Name)
		}
		name := strings.TrimSuffix(h.Name, "/")
		names = append(names, name)
		members[name] = m
	}
	return names, members
}

func runOK(t *testing.T, cfg *config.Config) (*Result, Input) {
	t.Helper()
	in := input(cfg)
	res, err := Run(in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res, in
}

func runErr(t *testing.T, cfg *config.Config, want string) {
	t.Helper()
	_, err := Run(input(cfg))
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Run error = %v, want containing %q", err, want)
	}
}

const header = `
project: {name: mycli}
archives:
  - targets: [{os: linux, arch: amd64}]
`

func TestSingleFileAndRename(t *testing.T) {
	root := fixture(t, map[string]string{"build/out/mycli*": "BIN", "README.md": "readme"})
	cfg := loadConfig(t, root, header+`    files:
      - src: build/out/mycli
        dst: bin/mycli
      - README.md
`)
	res, _ := runOK(t, cfg)
	names, m := readArchive(t, filepath.Join(root, "dist", "mycli_1.2.3_linux_amd64.tar.gz"))
	want := []string{"README.md", "bin", "bin/mycli"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("entries = %v, want %v", names, want)
	}
	if m["bin/mycli"].content != "BIN" || m["bin/mycli"].mode != 0o755 {
		t.Errorf("bin/mycli = %+v", m["bin/mycli"])
	}
	if m["README.md"].mode != 0o644 || m["bin"].mode != 0o755 {
		t.Errorf("modes: README %o, bin %o", m["README.md"].mode, m["bin"].mode)
	}
	a := res.Archives[0]
	if strings.Join(a.Executables, ",") != "bin/mycli" || a.Format != "tar.gz" || a.OS != "linux" {
		t.Errorf("manifest artifact = %+v", a)
	}
	data, _ := os.ReadFile(filepath.Join(root, a.Path))
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != a.SHA256 || a.Size != int64(len(data)) {
		t.Error("manifest sha256/size do not match the file")
	}
}

func TestDstDirectoryForLiteral(t *testing.T) {
	root := fixture(t, map[string]string{"LICENSE": "mit"})
	cfg := loadConfig(t, root, header+`    files: [{src: LICENSE, dst: legal/}]
`)
	runOK(t, cfg)
	names, _ := readArchive(t, filepath.Join(root, "dist", "mycli_1.2.3_linux_amd64.tar.gz"))
	if strings.Join(names, ",") != "legal,legal/LICENSE" {
		t.Fatalf("entries = %v", names)
	}
}

func TestGlobExcludeAndStructure(t *testing.T) {
	root := fixture(t, map[string]string{
		"docs/a.md":             "a",
		"docs/guide/b.md":       "b",
		"docs/guide/c.txt":      "c",
		"docs/internal/x.md":    "secret plan",
		"docs/.drafts/d.md":     "draft",
		"docs/guide/.hidden.md": "h",
	})
	cases := []struct {
		name, spec string
		want       []string
	}{
		{"preserve with dst", `{src: "docs/**/*.md", dst: documentation, exclude: ["docs/internal/**"]}`,
			[]string{"documentation", "documentation/a.md", "documentation/guide", "documentation/guide/b.md"}},
		{"keep project path without dst", `{src: "docs/**/*.md", exclude: ["docs/internal/**"]}`,
			[]string{"docs", "docs/a.md", "docs/guide", "docs/guide/b.md"}},
		{"strip_parent flattens", `{src: "docs/**/*.md", dst: md/, strip_parent: true, exclude: ["**/internal/**"]}`,
			[]string{"md", "md/a.md", "md/b.md"}},
		{"single level", `{src: "docs/*.md", dst: "."}`, nil},
		{"braces", `{src: "docs/guide/*.{md,txt}", dst: g/}`,
			[]string{"g", "g/b.md", "g/c.txt"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want == nil {
				_, err := config.Load(strings.NewReader(header+"    files: ["+tc.spec+"]\n"), root)
				if err == nil {
					t.Fatal("dst \".\" accepted")
				}
				return
			}
			cfg := loadConfig(t, root, header+"    files: ["+tc.spec+"]\n")
			runOK(t, cfg)
			names, _ := readArchive(t, filepath.Join(root, "dist", "mycli_1.2.3_linux_amd64.tar.gz"))
			if strings.Join(names, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("entries = %v, want %v", names, tc.want)
			}
		})
	}
}

func TestRequiredAndOptional(t *testing.T) {
	root := fixture(t, map[string]string{"README.md": "r"})
	runErr(t, loadConfig(t, root, header+`    files: [README.md, docs/man/mycli.1]
`), `required file "docs/man/mycli.1" does not exist`)
	runErr(t, loadConfig(t, root, header+`    files: [README.md, "man/*.1"]
`), `matched no files`)

	cfg := loadConfig(t, root, header+`    files:
      - README.md
      - {src: docs/man/mycli.1, dst: man/mycli.1, required: false}
      - {src: "completions/*", required: false}
`)
	res, _ := runOK(t, cfg)
	names, _ := readArchive(t, filepath.Join(root, "dist", "mycli_1.2.3_linux_amd64.tar.gz"))
	if strings.Join(names, ",") != "README.md" {
		t.Fatalf("entries = %v", names)
	}
	if got := res.Skipped["mycli_1.2.3_linux_amd64.tar.gz"]; len(got) != 2 {
		t.Errorf("skipped = %v", got)
	}
}

func TestCollisions(t *testing.T) {
	root := fixture(t, map[string]string{"a/README.md": "1", "b/README.md": "2", "x": "x", "X2": "x"})
	runErr(t, loadConfig(t, root, header+`    files: [{src: a/README.md, dst: README.md}, {src: b/README.md, dst: README.md}]
`), `archive path "README.md" collides`)
	runErr(t, loadConfig(t, root, header+`    files: [{src: "*/README.md", dst: docs/, strip_parent: true}]
`), "collides")
	runErr(t, loadConfig(t, root, header+`    files: [{src: a/README.md, dst: readme.md}, {src: b/README.md, dst: README.MD}]
`), "differ only in letter case")
	runErr(t, loadConfig(t, root, header+`    files: [{src: x, dst: docs}, {src: X2, dst: docs/x}]
`), "needs it to be a directory")
}

func TestTargetConditions(t *testing.T) {
	root := fixture(t, map[string]string{"bin/linux/mycli*": "L", "bin/windows/mycli.exe*": "W", "mac.txt": "m"})
	cfg := loadConfig(t, root, `
project: {name: mycli}
archives:
  - targets: [{os: linux, arch: amd64}, {os: windows, arch: amd64}, {os: darwin, arch: arm64}]
    format_overrides: [{os: windows, format: zip}]
    files:
      - {src: "bin/{{ .OS }}/mycli{{ .Ext }}", dst: "mycli{{ .Ext }}", when: {os: [linux, windows]}}
      - {src: mac.txt, when: {os: [darwin], arch: [arm64]}}
`)
	res, _ := runOK(t, cfg)
	if len(res.Archives) != 3 {
		t.Fatalf("archives = %d", len(res.Archives))
	}
	got := map[string]string{}
	for _, a := range res.Archives {
		names, _ := readArchive(t, filepath.Join(root, a.Path))
		got[a.Name] = strings.Join(names, ",")
	}
	want := map[string]string{
		"mycli_1.2.3_linux_amd64.tar.gz":  "mycli",
		"mycli_1.2.3_windows_amd64.zip":   "mycli.exe",
		"mycli_1.2.3_darwin_arm64.tar.gz": "mac.txt",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestInvalidPaths(t *testing.T) {
	root := fixture(t, map[string]string{"a": "a"})
	for _, spec := range []string{
		`{src: /etc/passwd}`, `{src: ../outside}`, `{src: a/../a}`, `{src: a, dst: /abs}`,
		`{src: a, dst: ../escape}`, `{src: a, dst: "x/../../y"}`, `{src: 'a\b'}`, `{src: a, dst: "C:/x"}`,
		`{src: a, mode: "4755"}`, `{src: a, mode: "0777"}`, `{src: a, mode: "0222"}`, `{src: a, mode: rwx}`,
	} {
		if _, err := config.Load(strings.NewReader(header+"    files: ["+spec+"]\n"), root); err == nil {
			t.Errorf("config accepted %s", spec)
		}
	}
	// Templated values are validated after rendering.
	runErr(t, loadConfig(t, root, header+`    files: [{src: a, dst: "{{ .Env.D }}x"}]
`), "")
	cfg := loadConfig(t, root, header+`    files: [{src: "{{ .Env.S }}"}]
`)
	in := input(cfg)
	in.Vars.Env = map[string]string{"S": "../../etc/passwd"}
	if _, err := Run(in); err == nil || !strings.Contains(err.Error(), `".." segment`) {
		t.Errorf("templated traversal: %v", err)
	}
}

func TestSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644)
	root := fixture(t, map[string]string{
		"real.txt":       "real",
		"inside.txt":     "->real.txt",
		"escape.txt":     "->" + filepath.Join(outside, "secret"),
		"rel-escape.txt": "->../" + filepath.Base(outside) + "/secret",
		"dir/f.txt":      "f",
		"linkdir":        "->dir",
		"broken":         "->nope",
	})
	cfg := loadConfig(t, root, header+`    files: [inside.txt]
`)
	runOK(t, cfg)
	_, m := readArchive(t, filepath.Join(root, "dist", "mycli_1.2.3_linux_amd64.tar.gz"))
	if m["inside.txt"].content != "real" || m["inside.txt"].typ != "file" {
		t.Errorf("followed link = %+v", m["inside.txt"])
	}
	runErr(t, loadConfig(t, root, header+`    files: [escape.txt]
`), "outside the project root")
	runErr(t, loadConfig(t, root, header+`    files: [rel-escape.txt]
`), "outside the project root")
	runErr(t, loadConfig(t, root, header+`    files: [broken]
`), "outside the project root or is broken")
	runErr(t, loadConfig(t, root, header+`    files: [linkdir]
`), "links to directories are never followed")
	runErr(t, loadConfig(t, root, header+`    symlinks: reject
    files: [inside.txt]
`), "symlinks: reject")
	// A glob never walks into a linked directory.
	cfg = loadConfig(t, root, header+`    files: ["**/f.txt"]
`)
	runOK(t, cfg)
	names, _ := readArchive(t, filepath.Join(root, "dist", "mycli_1.2.3_linux_amd64.tar.gz"))
	if strings.Join(names, ",") != "dir,dir/f.txt" {
		t.Errorf("entries = %v", names)
	}
}

func TestPermissions(t *testing.T) {
	root := fixture(t, map[string]string{"tool*": "t", "doc": "d", "script": "s"})
	cfg := loadConfig(t, root, header+`    files: [tool, doc, {src: script, mode: "0755"}, {src: tool, dst: ro, mode: "0444"}]
`)
	runOK(t, cfg)
	_, m := readArchive(t, filepath.Join(root, "dist", "mycli_1.2.3_linux_amd64.tar.gz"))
	for name, want := range map[string]int64{"tool": 0o755, "doc": 0o644, "script": 0o755, "ro": 0o444} {
		if m[name].mode != want {
			t.Errorf("%s mode = %o, want %o", name, m[name].mode, want)
		}
	}
	if m["tool"].uid != 0 || m["tool"].uname != "" {
		t.Errorf("owner leaked: %+v", m["tool"])
	}
}

func TestZipFormat(t *testing.T) {
	root := fixture(t, map[string]string{"mycli.exe*": "W", "docs/a.md": "a"})
	cfg := loadConfig(t, root, `
project: {name: mycli}
archives:
  - targets: [{os: windows, arch: arm64}]
    format: zip
    wrap_in_directory: true
    files: [mycli.exe, "docs/**"]
`)
	runOK(t, cfg)
	names, m := readArchive(t, filepath.Join(root, "dist", "mycli_1.2.3_windows_arm64.zip"))
	want := "mycli_1.2.3_windows_arm64,mycli_1.2.3_windows_arm64/docs,mycli_1.2.3_windows_arm64/docs/a.md,mycli_1.2.3_windows_arm64/mycli.exe"
	if strings.Join(names, ",") != want {
		t.Fatalf("entries = %v", names)
	}
	e := m["mycli_1.2.3_windows_arm64/mycli.exe"]
	if e.mode != 0o755 || e.content != "W" || !e.mtime.Equal(DefaultMTime) {
		t.Errorf("exe = %+v", e)
	}
}

func TestDeterministicOutput(t *testing.T) {
	yml := `
project: {name: mycli}
archives:
  - id: tgz
    targets: [{os: linux, arch: amd64}]
    files: [mycli, "docs/**/*.md"]
  - id: zip
    name: "{{ .Project }}_{{ .OS }}_{{ .Arch }}"
    format: zip
    targets: [{os: linux, arch: amd64}]
    files: [mycli, "docs/**/*.md"]
`
	files := map[string]string{"mycli*": strings.Repeat("binary", 1000), "docs/z.md": "z", "docs/a/b.md": "b", "docs/c.md": "c"}
	build := func(touch time.Time, umask bool) map[string]string {
		root := fixture(t, files)
		filepath.Walk(root, func(p string, _ os.FileInfo, _ error) error {
			return os.Chtimes(p, touch, touch)
		})
		if umask {
			os.Chmod(filepath.Join(root, "docs", "c.md"), 0o600)
		}
		res, _ := runOK(t, loadConfig(t, root, yml))
		out := map[string]string{}
		for _, a := range res.Archives {
			out[a.Name] = a.SHA256
		}
		return out
	}
	a := build(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), false)
	b := build(time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC), true)
	if len(a) != 2 {
		t.Fatalf("archives = %v", a)
	}
	for k := range a {
		if a[k] != b[k] {
			t.Errorf("%s differs between runs: %s vs %s", k, a[k], b[k])
		}
	}
	// Entry order is sorted, independent of declaration order.
	root := fixture(t, files)
	runOK(t, loadConfig(t, root, yml))
	names, _ := readArchive(t, filepath.Join(root, "dist", "mycli_1.2.3_linux_amd64.tar.gz"))
	if !sort.StringsAreSorted(names) {
		t.Errorf("entries not sorted: %v", names)
	}
}

func TestMTimeSources(t *testing.T) {
	root := fixture(t, map[string]string{"a": "a"})
	cfg := loadConfig(t, root, header+`    files: [a]
`)
	in := input(cfg)
	in.SourceDateEpoch = "1700000000"
	if _, err := Run(in); err != nil {
		t.Fatal(err)
	}
	_, m := readArchive(t, filepath.Join(root, "dist", "mycli_1.2.3_linux_amd64.tar.gz"))
	if !m["a"].mtime.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("mtime = %v", m["a"].mtime)
	}
	cfg = loadConfig(t, root, header+`    mtime: "2021-02-03T04:05:06Z"
    files: [a]
`)
	runOK(t, cfg)
	_, m = readArchive(t, filepath.Join(root, "dist", "mycli_1.2.3_linux_amd64.tar.gz"))
	if !m["a"].mtime.Equal(time.Date(2021, 2, 3, 4, 5, 6, 0, time.UTC)) {
		t.Errorf("mtime = %v", m["a"].mtime)
	}
}

func TestSensitiveFiles(t *testing.T) {
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\n" + strings.Repeat("QUJD", 30) + "\n-----END OPENSSH PRIVATE KEY-----\n"
	root := fixture(t, map[string]string{
		"conf/.env":       "TOKEN=x",
		"conf/server.pem": "pem",
		"conf/app.yaml":   "ok",
		"notes.txt":       key,
		"lib.txt":         "contains -----BEGIN RSA PRIVATE KEY----- marker only",
		".git/config":     "x",
	})
	runErr(t, loadConfig(t, root, header+`    files: [conf/server.pem]
`), "matches *.pem")
	runErr(t, loadConfig(t, root, header+`    files: [conf/.env]
`), "matches .env")
	runErr(t, loadConfig(t, root, header+`    files: [notes.txt]
`), "PEM private key")
	runErr(t, loadConfig(t, root, header+`    files: [.git/config]
`), "inside a .git directory")
	runErr(t, loadConfig(t, root, header+`    files: ["conf/*"]
`), "matches *.pem")
	// Dot files are not matched by wildcards; the .env is never seen here.
	cfg := loadConfig(t, root, header+`    files: [{src: "conf/*", exclude: ["**/*.pem"]}, lib.txt]
`)
	runOK(t, cfg)
	cfg = loadConfig(t, root, header+`    files: [{src: conf/server.pem, allow_sensitive: true}]
`)
	runOK(t, cfg)
}

func TestLimits(t *testing.T) {
	root := fixture(t, map[string]string{"a": "1", "b": "2", "c": "3", "big": strings.Repeat("x", 2048)})
	runErr(t, loadConfig(t, root, "limits: {max_files: 2}\n"+header+`    files: ["{a,b,c}"]
`), "max_files")
	runErr(t, loadConfig(t, root, "limits: {max_file_size: 1KiB}\n"+header+`    files: [big]
`), "max_file_size")
	runErr(t, loadConfig(t, root, "limits: {max_total_size: 2}\n"+header+`    files: [a, b, c]
`), "max_total_size")
}

func TestDistNeverRepackaged(t *testing.T) {
	root := fixture(t, map[string]string{"a.txt": "a", "dist/old.txt": "old", "dist/linux/mycli*": "bin"})
	cfg := loadConfig(t, root, header+`    files: ["**/*.txt", dist/linux/mycli]
`)
	runOK(t, cfg)
	names, _ := readArchive(t, filepath.Join(root, "dist", "mycli_1.2.3_linux_amd64.tar.gz"))
	if strings.Join(names, ",") != "a.txt,dist,dist/linux,dist/linux/mycli" {
		t.Errorf("entries = %v", names)
	}
}

func TestDefaultContentsFromManifest(t *testing.T) {
	root := fixture(t, map[string]string{"out/linux/mycli*": "L", "out/darwin/mycli*": "D"})
	cfg := loadConfig(t, root, "project: {name: mycli}\narchives: [{}]\n")
	in := input(cfg)
	for _, os := range []string{"linux", "darwin"} {
		in.Manifest.Put(manifest.Artifact{Kind: manifest.KindBinary, ID: "default", Name: "mycli", Path: "out/" + os + "/mycli", OS: os, Arch: "arm64", Source: manifest.Source{Stage: "build"}})
	}
	res, err := Run(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Archives) != 2 {
		t.Fatalf("archives = %v", res.Archives)
	}
	for _, a := range res.Archives {
		_, m := readArchive(t, filepath.Join(root, a.Path))
		if len(m) != 1 || m["mycli"].mode != 0o755 {
			t.Errorf("%s: %+v", a.Name, m)
		}
	}
	if _, err := config.Load(strings.NewReader("project: {name: mycli}\narchives: [{files: []}]\n"), root); err == nil {
		t.Error("explicit empty files accepted")
	}
}

func TestArchiveNames(t *testing.T) {
	root := fixture(t, map[string]string{"a": "a"})
	runErr(t, loadConfig(t, root, header+`    name: "my cli"
    files: [a]
`), "may only contain")
	runErr(t, loadConfig(t, root, `
project: {name: mycli}
archives:
  - name: same
    targets: [{os: linux, arch: amd64}, {os: darwin, arch: amd64}]
    files: [a]
`), "also produced by")
	runErr(t, loadConfig(t, root, "project: {name: mycli}\narchives: [{files: [a]}]\n"), "no targets")
}

func TestSourceChangedBetweenPlanAndWrite(t *testing.T) {
	root := fixture(t, map[string]string{"a": "original"})
	cfg := loadConfig(t, root, header+`    files: [a]
`)
	plans, err := BuildPlans(input(cfg))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "a"), []byte("tampered"), 0o644)
	if _, err := Write(root, plans[0]); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, plans[0].Path)); !os.IsNotExist(err) {
		t.Error("partial archive left behind")
	}
}

func TestRerunReplacesManifestEntries(t *testing.T) {
	root := fixture(t, map[string]string{"a": "a"})
	cfg := loadConfig(t, root, header+`    files: [a]
`)
	in := input(cfg)
	for i := 0; i < 2; i++ {
		if _, err := Run(in); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(in.Manifest.Filter(manifest.KindArchive)); n != 1 {
		t.Errorf("archives in manifest = %d", n)
	}
}

// TestExtractWithSystemTools extracts with the platform's tar and unzip to
// make sure real tools accept the output, not just Go's readers.
func TestExtractWithSystemTools(t *testing.T) {
	root := fixture(t, map[string]string{"mycli*": "#!/bin/sh\necho hi\n", "docs/a.md": "a"})
	cfg := loadConfig(t, root, `
project: {name: mycli}
archives:
  - id: t
    targets: [{os: linux, arch: amd64}]
    files: [{src: mycli, dst: bin/}, "docs/**"]
  - id: z
    name: "{{ .Project }}_{{ .OS }}_z"
    format: zip
    targets: [{os: linux, arch: amd64}]
    files: [{src: mycli, dst: bin/}, "docs/**"]
`)
	res, _ := runOK(t, cfg)
	for _, a := range res.Archives {
		dir := t.TempDir()
		var cmd *exec.Cmd
		if a.Format == "zip" {
			if _, err := exec.LookPath("unzip"); err != nil {
				t.Skip("unzip not installed")
			}
			cmd = exec.Command("unzip", "-q", filepath.Join(root, a.Path), "-d", dir)
		} else {
			cmd = exec.Command("tar", "-xzf", filepath.Join(root, a.Path), "-C", dir)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", cmd, err, out)
		}
		st, err := os.Stat(filepath.Join(dir, "bin", "mycli"))
		if err != nil || st.Mode().Perm()&0o100 == 0 {
			t.Errorf("%s: bin/mycli = %v, %v", a.Name, st, err)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "docs", "a.md")); string(b) != "a" {
			t.Errorf("%s: docs/a.md = %q", a.Name, b)
		}
		if runtime.GOOS != "windows" {
			out, err := exec.Command(filepath.Join(dir, "bin", "mycli")).Output()
			if err != nil || string(out) != "hi\n" {
				t.Errorf("%s: running extracted binary: %q %v", a.Name, out, err)
			}
		}
	}
}

func TestRerunKeepsImportedArchives(t *testing.T) {
	root := fixture(t, map[string]string{"a": "a"})
	cfg := loadConfig(t, root, header+`    files: [a]
`)
	in := input(cfg)
	imported := manifest.Artifact{Kind: manifest.KindArchive, ID: "default", Name: "prebuilt.zip", Path: "pre/prebuilt.zip", OS: "linux", Arch: "arm64", Format: "zip",
		SHA256: strings.Repeat("a", 64), Source: manifest.Source{Stage: "build", Provider: "existing"}}
	in.Manifest.Put(imported)
	if _, err := Run(in); err != nil {
		t.Fatal(err)
	}
	if n := len(in.Manifest.Filter(manifest.KindArchive)); n != 2 {
		t.Errorf("archives = %d, want imported + generated", n)
	}
}
