package build

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/manifest"
)

type member struct {
	name string
	mode int64
	body string
	link string // symlink target
}

func tarGz(t *testing.T, members ...member) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		h := &tar.Header{Name: m.name, Mode: m.mode, Typeflag: tar.TypeReg, Size: int64(len(m.body))}
		switch {
		case m.link != "":
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, m.link, 0
		case strings.HasSuffix(m.name, "/"):
			h.Typeflag, h.Size = tar.TypeDir, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(m.body))
	}
	tw.Close()
	gz.Close()
	return buf.String()
}

func zipData(t *testing.T, members ...member) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range members {
		h := &zip.FileHeader{Name: m.name, Method: zip.Deflate}
		if m.mode != 0 {
			h.SetMode(os.FileMode(m.mode))
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(m.body))
	}
	zw.Close()
	return buf.String()
}

const existingHeader = `
project: {name: app}
builds:
  - existing:
`

func TestExistingBinary(t *testing.T) {
	root := fixture(t, map[string]string{
		"prebuilt/app_linux_amd64":       "ELF-amd64",
		"prebuilt/app_darwin_arm64":      "MACHO-arm64",
		"prebuilt/app_windows_amd64.exe": "MZ",
	})
	cfg := loadConfig(t, root, existingHeader+`      path: "prebuilt/app_{{ .OS }}_{{ .Arch }}{{ .Ext }}"
    matrix: {os: [linux, darwin, windows], arch: [amd64, arm64], exclude: [{os: linux, arch: arm64}, {os: darwin, arch: amd64}, {os: windows, arch: arm64}]}
`)
	in, _ := newInput(cfg)
	res := run(t, in)
	if len(res.Binaries) != 3 || len(res.Planned) != 0 || len(res.Imported) != 0 {
		t.Fatalf("result = %+v", res)
	}
	a := res.Binaries[2]
	if a.Name != "app_windows_amd64.exe" || a.Path != "prebuilt/app_windows_amd64.exe" || a.OS != "windows" || a.SHA256 != sha([]byte("MZ")) || a.Size != 2 {
		t.Errorf("artifact = %+v", a)
	}
	if a.Source != (manifest.Source{Stage: "build", Provider: "existing"}) || a.Executables[0] != a.Name {
		t.Errorf("source = %+v", a.Source)
	}
	if n := len(in.Manifest.Filter(manifest.KindBinary)); n != 3 {
		t.Errorf("manifest binaries = %d", n)
	}

	// One missing target fails the build and leaves the manifest alone.
	cfg = loadConfig(t, root, existingHeader+`      path: "prebuilt/app_{{ .OS }}_{{ .Arch }}"
    targets: [{os: linux, arch: amd64}, {os: linux, arch: riscv64}]
`)
	in, _ = newInput(cfg)
	runErr(t, in, "target linux/riscv64", `"prebuilt/app_linux_riscv64" does not exist`)
	if len(in.Manifest.Artifacts) != 0 {
		t.Error("manifest modified")
	}

	for path, want := range map[string]string{
		"../outside":         `".." segment`,
		"/etc/passwd":        "relative",
		"prebuilt":           "is a directory",
		"prebuilt/{{ .OS }}": "does not exist",
	} {
		cfg = loadConfig(t, root, existingHeader+fmt.Sprintf("      path: %q\n    targets: [{os: linux, arch: amd64}]\n", path))
		in, _ = newInput(cfg)
		runErr(t, in, want)
	}

	// Dry runs still verify, read-only.
	cfg = loadConfig(t, root, existingHeader+`      path: "prebuilt/app_{{ .OS }}_{{ .Arch }}"
    targets: [{os: linux, arch: amd64}]
`)
	in, _ = newInput(cfg)
	in.DryRun = true
	if res := run(t, in); len(res.Binaries) != 1 || len(in.Manifest.Artifacts) != 0 {
		t.Errorf("dry run: result %+v, manifest %+v", res, in.Manifest.Artifacts)
	}
}

func TestExistingArchive(t *testing.T) {
	tgz := tarGz(t,
		member{name: "app_1.2.3_linux_amd64/", mode: 0o755},
		member{name: "app_1.2.3_linux_amd64/app", mode: 0o755, body: "ELF"},
		member{name: "app_1.2.3_linux_amd64/README.md", mode: 0o644, body: "readme"},
	)
	zipped := zipData(t, member{name: "app.exe", mode: 0o644, body: "MZ"}, member{name: "LICENSE", mode: 0o644, body: "mit"})
	root := fixture(t, map[string]string{
		"release/app_1.2.3_linux_amd64.tar.gz": tgz,
		"release/app_1.2.3_windows_amd64.zip":  zipped,
	})
	cfg := loadConfig(t, root, existingHeader+`      kind: archive
      path: "release/app_{{ .Version }}_{{ .OS }}_{{ .Arch }}.{{ if eq .OS \"windows\" }}zip{{ else }}tar.gz{{ end }}"
    targets: [{os: linux, arch: amd64}, {os: windows, arch: amd64}]
`)
	in, _ := newInput(cfg)
	res := run(t, in)
	if len(res.Binaries) != 0 || len(res.Imported) != 2 {
		t.Fatalf("result = %+v", res)
	}
	a := res.Imported[0]
	if a.Kind != manifest.KindArchive || a.Format != "tar.gz" || a.Name != "app_1.2.3_linux_amd64.tar.gz" || a.SHA256 != sha([]byte(tgz)) || a.Size != int64(len(tgz)) {
		t.Errorf("archive = %+v", a)
	}
	if strings.Join(a.Executables, ",") != "app_1.2.3_linux_amd64/app" || len(a.Files) != 3 || a.Files[2].Path != "app_1.2.3_linux_amd64/app" || a.Files[2].SHA256 != sha([]byte("ELF")) {
		t.Errorf("contents = %+v / %v", a.Files, a.Executables)
	}
	if a.Source.Provider != "existing" || a.Source.Stage != "build" {
		t.Errorf("source = %+v", a.Source)
	}
	if z := res.Imported[1]; z.Format != "zip" || len(z.Files) != 2 || len(z.Executables) != 0 {
		t.Errorf("zip = %+v", z)
	}
	if got := in.Manifest.Filter(manifest.KindArchive); len(got) != 2 {
		t.Errorf("manifest archives = %+v", got)
	}
	// A re-run replaces the imported archives instead of duplicating them.
	run(t, in)
	if got := in.Manifest.Filter(manifest.KindArchive); len(got) != 2 {
		t.Errorf("after re-run: manifest archives = %d", len(got))
	}
}

func TestExistingArchiveRejected(t *testing.T) {
	cases := []struct {
		name, file, data, want string
	}{
		{"bad extension", "pkg.tar.xz", "x", "not a .tar.gz, .tgz or .zip"},
		{"zip slip", "pkg.zip", "", `".." segment`},
		{"symlink", "pkg.tgz", "", "symbolic link"},
		{"not an archive", "pkg.tar.gz", "plain text", "not a gzip stream"},
		{"bad asset name", "pkg v1.zip", "x", "may only contain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.data
			switch tc.name {
			case "zip slip":
				data = zipData(t, member{name: "../../evil", body: "x"})
			case "symlink":
				data = tarGz(t, member{name: "link", link: "/etc/passwd"})
			}
			root := fixture(t, map[string]string{tc.file: data})
			cfg := loadConfig(t, root, existingHeader+fmt.Sprintf("      kind: archive\n      path: %q\n    targets: [{os: linux, arch: amd64}]\n", tc.file))
			in, _ := newInput(cfg)
			runErr(t, in, tc.want)
		})
	}
	if _, err := loadConfigErr(existingHeader + "      kind: tarball\n      path: x\n    targets: [{os: linux, arch: amd64}]\n"); err == nil || !strings.Contains(err.Error(), "existing.kind") {
		t.Errorf("invalid kind accepted: %v", err)
	}
}

// grFixture writes artifacts.json with the given elements (JSON objects).
func grFixture(t *testing.T, files map[string]string, elems ...string) string {
	t.Helper()
	if files == nil {
		files = map[string]string{}
	}
	files["dist/artifacts.json"] = "[" + strings.Join(elems, ",") + "]"
	return fixture(t, files)
}

const grHeader = `
project: {name: app}
builds:
  - id: gr
    goreleaser:
`

func grBin(name, path, goos, goarch string) string {
	return fmt.Sprintf(`{"name":%q,"path":%q,"goos":%q,"goarch":%q,"type":"Binary","internal_type":4,"extra":{"ID":"app","Binary":"app","Ext":""}}`, name, path, goos, goarch)
}

func TestGoReleaserBinaries(t *testing.T) {
	files := map[string]string{
		"dist/app_linux_amd64_v1/app":     "amd64-v1",
		"dist/app_linux_amd64_v3/app":     "amd64-v3",
		"dist/app_linux_arm_7/app":        "armv7",
		"dist/app_darwin_all/app":         "universal",
		"dist/app_windows_amd64_v1/a.exe": "MZ",
		"dist/other_linux_amd64_v1/other": "other",
		"dist/app_1.2.3_linux_amd64":      "uploadable copy",
	}
	sumV1 := sha([]byte("amd64-v1"))
	root := grFixture(t, files,
		`{"name":"app","path":"dist/app_linux_amd64_v1/app","goos":"linux","goarch":"amd64","goamd64":"v1","target":"linux_amd64_v1","type":"Binary","internal_type":4,"extra":{"ID":"app","Binary":"app","Checksum":"sha256:`+sumV1+`","DynamicallyLinked":false}}`,
		`{"name":"app","path":"dist/app_linux_amd64_v3/app","goos":"linux","goarch":"amd64","goamd64":"v3","type":"Binary","extra":{"ID":"app"}}`,
		`{"name":"app","path":"dist/app_linux_arm_7/app","goos":"linux","goarch":"arm","goarm":"7","type":"Binary","extra":{"ID":"app"}}`,
		`{"name":"app","path":"dist/app_darwin_all/app","goos":"darwin","goarch":"all","type":"Binary","extra":{"ID":"app","Replaces":true}}`,
		`{"name":"a.exe","path":"dist\\app_windows_amd64_v1\\a.exe","goos":"windows","goarch":"amd64","goamd64":"v1","type":"Binary","extra":{"ID":"app"}}`,
		`{"name":"other","path":"dist/other_linux_amd64_v1/other","goos":"linux","goarch":"amd64","goamd64":"v1","type":"Binary","extra":{"ID":"other"}}`,
		`{"name":"app_1.2.3_linux_amd64","path":"dist/app_1.2.3_linux_amd64","goos":"linux","goarch":"amd64","goamd64":"v1","type":"Binary","extra":{"ID":"app","Format":"binary"}}`,
		`{"name":"checksums.txt","path":"dist/checksums.txt","type":"Checksum","extra":{}}`,
		`{"name":"app_1.2.3_linux_amd64.tar.gz","path":"dist/app_1.2.3_linux_amd64.tar.gz","goos":"linux","goarch":"amd64","type":"Archive","extra":{"ID":"default"}}`,
	)
	cfg := loadConfig(t, root, grHeader+"      ids: [app]\n")
	in, _ := newInput(cfg)
	res := run(t, in)
	got := map[string]manifest.Artifact{}
	for _, a := range res.Binaries {
		got[a.OS+"/"+a.Arch+a.Arm] = a
	}
	if len(res.Binaries) != 4 || len(res.Imported) != 0 {
		t.Fatalf("binaries = %+v", res.Binaries)
	}
	if a := got["linux/amd64"]; a.Path != "dist/app_linux_amd64_v1/app" || a.SHA256 != sumV1 || a.Name != "app" || a.ID != "gr" {
		t.Errorf("amd64 = %+v (goamd64 v1 must be selected by default)", a)
	}
	if a := got["linux/arm7"]; a.Arm != "7" || a.SHA256 != sha([]byte("armv7")) {
		t.Errorf("arm = %+v", a)
	}
	if _, ok := got["darwin/all"]; !ok {
		t.Error("universal binary missing")
	}
	if a := got["windows/amd64"]; a.Path != "dist/app_windows_amd64_v1/a.exe" || a.Name != "a.exe" {
		t.Errorf("windows = %+v", a)
	}
	if a := got["linux/amd64"]; a.Source != (manifest.Source{Stage: "build", Provider: "goreleaser", Origin: "dist/artifacts.json"}) {
		t.Errorf("source = %+v", a.Source)
	}

	// goamd64: v3 picks the other level.
	cfg = loadConfig(t, root, grHeader+"      ids: [app]\n      goamd64: v3\n")
	in, _ = newInput(cfg)
	for _, a := range run(t, in).Binaries {
		if a.OS == "linux" && a.Arch == "amd64" && a.SHA256 != sha([]byte("amd64-v3")) {
			t.Errorf("goamd64 v3 imported %+v", a)
		}
	}

	// Without ids both build ids are imported; the "other" binary has a
	// different name, so there is no clash.
	cfg = loadConfig(t, root, grHeader+"      artifacts: dist/artifacts.json\n")
	in, _ = newInput(cfg)
	if res := run(t, in); len(res.Binaries) != 5 {
		t.Errorf("without ids: %d binaries", len(res.Binaries))
	}

	cfg = loadConfig(t, root, grHeader+"      ids: [nothing]\n")
	in, _ = newInput(cfg)
	runErr(t, in, "lists no Binary artifacts with ids [nothing]")
}

func TestGoReleaserRejects(t *testing.T) {
	bin := map[string]string{"dist/app": "BIN"}
	sum := sha([]byte("BIN"))
	md := md5.Sum([]byte("BIN"))
	cases := []struct {
		name, elems, want string
	}{
		{"sha256 mismatch", `{"name":"app","path":"dist/app","goos":"linux","goarch":"amd64","type":"Binary","extra":{"ID":"a","Checksum":"sha256:` + strings.Repeat("ab", 32) + `"}}`, "checksum mismatch"},
		{"md5 mismatch", `{"name":"app","path":"dist/app","goos":"linux","goarch":"amd64","type":"Binary","extra":{"ID":"a","Checksum":"md5:00000000000000000000000000000000"}}`, "checksum mismatch"},
		{"unknown checksum algorithm", `{"name":"app","path":"dist/app","goos":"linux","goarch":"amd64","type":"Binary","extra":{"ID":"a","Checksum":"blake9:00"}}`, "not supported"},
		{"malformed checksum", `{"name":"app","path":"dist/app","goos":"linux","goarch":"amd64","type":"Binary","extra":{"ID":"a","Checksum":"` + sum + `"}}`, "algorithm:hex"},
		{"outside root", `{"name":"app","path":"../app","goos":"linux","goarch":"amd64","type":"Binary","extra":{"ID":"a"}}`, `".." segment`},
		{"absolute", `{"name":"app","path":"/usr/bin/true","goos":"linux","goarch":"amd64","type":"Binary","extra":{"ID":"a"}}`, "must be relative to the project root"},
		{"missing file", `{"name":"app","path":"dist/gone","goos":"linux","goarch":"amd64","type":"Binary","extra":{"ID":"a"}}`, "does not exist"},
		{"missing platform", `{"name":"app","path":"dist/app","type":"Binary","extra":{"ID":"a"}}`, "invalid or missing platform"},
		{"duplicate", `{"name":"app","path":"dist/app","goos":"linux","goarch":"amd64","type":"Binary","extra":{"ID":"a"}},{"name":"app","path":"dist/app","goos":"linux","goarch":"amd64","goarm64":"v9.0","type":"Binary","extra":{"ID":"b"}}`, "is also listed at dist/artifacts.json[0]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			for k, v := range bin {
				files[k] = v
			}
			root := grFixture(t, files, tc.elems)
			in, _ := newInput(loadConfig(t, root, grHeader+"      ids: []\n"))
			runErr(t, in, "build gr", tc.want)
			if len(in.Manifest.Artifacts) != 0 {
				t.Error("manifest modified")
			}
		})
	}

	// A matching non-sha256 checksum is accepted.
	root := grFixture(t, map[string]string{"dist/app": "BIN"}, `{"name":"app","path":"dist/app","goos":"linux","goarch":"amd64","type":"Binary","extra":{"ID":"a","Checksum":"md5:`+hex.EncodeToString(md[:])+`"}}`)
	in, _ := newInput(loadConfig(t, root, grHeader+"      ids: [a]\n"))
	run(t, in)

	if runtime.GOOS != "windows" {
		outside := filepath.Join(t.TempDir(), "bin")
		os.WriteFile(outside, []byte("x"), 0o755)
		root := grFixture(t, nil, `{"name":"app","path":"dist/link","goos":"linux","goarch":"amd64","type":"Binary","extra":{"ID":"a"}}`)
		if err := os.Symlink(outside, filepath.Join(root, "dist", "link")); err != nil {
			t.Fatal(err)
		}
		in, _ := newInput(loadConfig(t, root, grHeader+"      ids: [a]\n"))
		runErr(t, in, "outside the project root")
	}

	for name, content := range map[string]string{
		"not json":   "not json",
		"object":     `{"name":"x"}`,
		"trailing":   `[] []`,
		"wrong type": `[{"name":1}]`,
	} {
		root := fixture(t, map[string]string{"dist/artifacts.json": content})
		in, _ := newInput(loadConfig(t, root, grHeader+"      ids: [a]\n"))
		if _, err := Run(t.Context(), in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	root = fixture(t, nil)
	in, _ = newInput(loadConfig(t, root, grHeader+"      ids: [a]\n"))
	runErr(t, in, "dist/artifacts.json does not exist")
}

func TestGoReleaserArchives(t *testing.T) {
	tgz := tarGz(t,
		member{name: "app_1.2.3_linux_amd64/app", mode: 0o755, body: "ELF"},
		member{name: "app_1.2.3_linux_amd64/LICENSE", mode: 0o644, body: "mit"},
	)
	zipped := zipData(t, member{name: "app.exe", mode: 0o644, body: "MZ"})
	root := grFixture(t, map[string]string{
		"dist/app_linux_amd64_v1/app":        "ELF",
		"dist/app_1.2.3_linux_amd64.tar.gz":  tgz,
		"dist/app_1.2.3_windows_amd64.zip":   zipped,
		"dist/app_1.2.3_darwin_arm64.tar.xz": "xz",
	},
		grBin("app", "dist/app_linux_amd64_v1/app", "linux", "amd64"),
		`{"name":"app_1.2.3_linux_amd64.tar.gz","path":"dist/app_1.2.3_linux_amd64.tar.gz","goos":"linux","goarch":"amd64","goamd64":"v1","type":"Archive","extra":{"ID":"app","Binaries":["app"],"WrappedIn":"app_1.2.3_linux_amd64","Format":"tar.gz","Checksum":"sha256:`+sha([]byte(tgz))+`"}}`,
		`{"name":"app_1.2.3_windows_amd64.zip","path":"dist/app_1.2.3_windows_amd64.zip","goos":"windows","goarch":"amd64","goamd64":"v1","type":"Archive","extra":{"ID":"app","Binaries":["app.exe"],"WrappedIn":"","Format":"zip"}}`,
		`{"name":"app_1.2.3_darwin_arm64.tar.xz","path":"dist/app_1.2.3_darwin_arm64.tar.xz","goos":"darwin","goarch":"arm64","type":"Archive","extra":{"ID":"xz","Format":"tar.xz"}}`,
	)
	// Without archives: true archives are ignored.
	in, _ := newInput(loadConfig(t, root, grHeader+"      ids: [app]\n"))
	if res := run(t, in); len(res.Imported) != 0 || len(res.Binaries) != 1 {
		t.Fatalf("archives imported without archives: true: %+v", res)
	}

	in, _ = newInput(loadConfig(t, root, grHeader+"      ids: [app]\n      archives: true\n"))
	res := run(t, in)
	if len(res.Binaries) != 1 || len(res.Imported) != 2 {
		t.Fatalf("result = %+v", res)
	}
	a := res.Imported[0]
	if a.Kind != manifest.KindArchive || a.Format != "tar.gz" || a.SHA256 != sha([]byte(tgz)) || len(a.Files) != 2 || strings.Join(a.Executables, ",") != "app_1.2.3_linux_amd64/app" {
		t.Errorf("tar.gz = %+v", a)
	}
	// The zip lacks Unix modes; the declared binary is still executable.
	if z := res.Imported[1]; strings.Join(z.Executables, ",") != "app.exe" || z.Source.Provider != "goreleaser" {
		t.Errorf("zip = %+v", z)
	}
	if got := in.Manifest.Filter(manifest.KindArchive, "gr"); len(got) != 2 {
		t.Errorf("manifest archives = %+v", got)
	}

	// Unsupported formats are an explicit error, not silently dropped.
	in, _ = newInput(loadConfig(t, root, grHeader+"      archives: true\n"))
	runErr(t, in, `archive format "tar.xz" is not supported`)

	// A declared binary missing from the archive is an error.
	root = grFixture(t, map[string]string{"dist/x.tar.gz": tgz},
		`{"name":"x.tar.gz","path":"dist/x.tar.gz","goos":"linux","goarch":"amd64","type":"Archive","extra":{"ID":"app","Binaries":["nope"],"WrappedIn":"app_1.2.3_linux_amd64","Format":"tar.gz"}}`)
	in, _ = newInput(loadConfig(t, root, grHeader+"      archives: true\n"))
	runErr(t, in, `does not contain the declared binary "app_1.2.3_linux_amd64/nope"`)

	if _, err := loadConfigErr(grHeader + "      goamd64: v5\n"); err == nil || !strings.Contains(err.Error(), "goamd64") {
		t.Errorf("goamd64 v5 accepted: %v", err)
	}
}

func loadConfigErr(yml string) (*config.Config, error) {
	return config.Load(strings.NewReader(yml), os.TempDir())
}
