package homebrew

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

const (
	sumDarwinArm  = "1111111111111111111111111111111111111111111111111111111111111111"
	sumDarwinX86  = "2222222222222222222222222222222222222222222222222222222222222222"
	sumLinuxArm   = "3333333333333333333333333333333333333333333333333333333333333333"
	sumLinuxX86   = "4444444444444444444444444444444444444444444444444444444444444444"
	sumOther      = "5555555555555555555555555555555555555555555555555555555555555555"
	testVersion   = "1.2.3"
	testTag       = "v1.2.3"
	testOwner     = "acme"
	testRepo      = "mycli"
	testProject   = "mycli"
	testHomepage  = "https://github.com/acme/mycli"
	testDesc      = "Friendly tool for managing widgets"
	testLicense   = "MIT"
	modeExec      = "0755"
	modeRegular   = "0644"
	defaultFormat = "tar.gz"
)

func sumFor(goos, arch string) string {
	switch goos + "/" + arch {
	case "darwin/arm64":
		return sumDarwinArm
	case "darwin/amd64":
		return sumDarwinX86
	case "linux/arm64":
		return sumLinuxArm
	case "linux/amd64":
		return sumLinuxX86
	}
	return sumOther
}

// file builds a manifest file entry; a trailing "/" makes a directory.
func file(p, mode string) manifest.File {
	if strings.HasSuffix(p, "/") {
		return manifest.File{Path: strings.TrimSuffix(p, "/"), Type: "dir", Mode: "0755"}
	}
	return manifest.File{Path: p, Type: "file", Mode: mode}
}

// arch builds an archive artifact. wrap, when non-empty, places every entry
// under that top-level directory.
func arch(goos, goarch, wrap string, files []manifest.File, execs ...string) manifest.Artifact {
	name := testProject + "_" + testVersion + "_" + goos + "_" + goarch + ".tar.gz"
	a := manifest.Artifact{
		Kind: manifest.KindArchive, ID: "default", Name: name, Path: "dist/" + name,
		OS: goos, Arch: goarch, Format: defaultFormat, SHA256: sumFor(goos, goarch),
		Source: manifest.Source{Stage: "archive"},
	}
	if wrap != "" {
		a.Files = append(a.Files, file(wrap+"/", ""))
	}
	for _, f := range files {
		if wrap != "" {
			f.Path = wrap + "/" + f.Path
		}
		a.Files = append(a.Files, f)
	}
	for _, e := range execs {
		if wrap != "" {
			e = wrap + "/" + e
		}
		a.Executables = append(a.Executables, e)
	}
	return a
}

func stdFiles() []manifest.File {
	return []manifest.File{file("LICENSE", modeRegular), file("README.md", modeRegular), file("mycli", modeExec)}
}

func fourPlatforms(wrap bool) []manifest.Artifact {
	var out []manifest.Artifact
	for _, t := range [][2]string{{"darwin", "amd64"}, {"darwin", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
		w := ""
		if wrap {
			w = testProject + "_" + testVersion + "_" + t[0] + "_" + t[1]
		}
		out = append(out, arch(t[0], t[1], w, stdFiles(), "mycli"))
	}
	return out
}

func newManifest(arts ...manifest.Artifact) *manifest.Manifest {
	m := manifest.New(testProject, testVersion, testTag, "0123456789abcdef0123456789abcdef01234567")
	m.Artifacts = append(m.Artifacts, arts...)
	return m
}

func baseBrew() config.Brew {
	return config.Brew{Name: testProject, Description: testDesc, Homepage: testHomepage, License: testLicense}
}

func testVars() tmpl.Vars {
	return tmpl.Vars{
		Project: testProject, ProjectName: testProject, Version: testVersion, Tag: testTag, RawVersion: testTag,
		Major: "1", Minor: "2", Patch: "3", Commit: "0123456789abcdef0123456789abcdef01234567", ShortCommit: "0123456",
		Env: map[string]string{"TAP_TOKEN": "secret"},
	}
}

func input(b config.Brew, arts ...manifest.Artifact) Input {
	cfg := &config.Config{Project: config.Project{Name: testProject}, Brews: []config.Brew{b}}
	cfg.ApplyDefaults()
	return Input{Config: cfg, Manifest: newManifest(arts...), Vars: testVars(), Owner: testOwner, Repo: testRepo, Tag: testTag}
}

func generateOne(t *testing.T, in Input) Formula {
	t.Helper()
	fs, err := Generate(in)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(fs) != 1 {
		t.Fatalf("Generate returned %d formulae", len(fs))
	}
	return fs[0]
}

type goldenCase struct {
	name string
	in   func(t *testing.T) Input
	// wantWarnings are substrings that must appear among the warnings.
	wantWarnings []string
}

func goldenCases() []goldenCase {
	return []goldenCase{
		{
			name: "four_platforms",
			in: func(t *testing.T) Input {
				arts := fourPlatforms(false)
				arts = append(arts,
					arch("windows", "amd64", "", stdFiles(), "mycli.exe"),
					arch("linux", "386", "", stdFiles(), "mycli"),
				)
				armv7 := arch("linux", "arm", "", stdFiles(), "mycli")
				armv7.Arm = "7"
				armv7.Name = "mycli_1.2.3_linux_armv7.tar.gz"
				armv7.Path = "dist/" + armv7.Name
				arts = append(arts, armv7)
				return input(baseBrew(), arts...)
			},
			wantWarnings: []string{"linux/386) skipped", "linux/armv7) skipped"},
		},
		{
			name: "macos_only",
			in: func(t *testing.T) Input {
				return input(baseBrew(), fourPlatforms(false)[:2]...)
			},
		},
		{
			name: "linux_amd64_only",
			in: func(t *testing.T) Input {
				return input(baseBrew(), fourPlatforms(false)[2])
			},
			wantWarnings: []string{"no linux/arm64 archive"},
		},
		{
			name: "wrap_in_directory",
			in: func(t *testing.T) Input {
				var arts []manifest.Artifact
				for _, a := range fourPlatforms(true) {
					top := strings.TrimSuffix(a.Name, ".tar.gz")
					a.Files = append(a.Files,
						file(top+"/completions/", ""),
						file(top+"/completions/mycli.bash", modeRegular),
						file(top+"/completions/_mycli", modeRegular),
						file(top+"/man/", ""),
						file(top+"/man/mycli.1", modeRegular),
					)
					arts = append(arts, a)
				}
				b := baseBrew()
				b.ExtraInstall = "bash_completion.install \"completions/mycli.bash\" => \"mycli\"\nzsh_completion.install \"completions/_mycli\"\nman1.install \"man/mycli.1\"\n"
				return input(b, arts...)
			},
		},
		{
			name: "custom",
			in: func(t *testing.T) Input {
				b := baseBrew()
				b.License = "MIT OR Apache-2.0"
				b.Install = "bin.install \"mycli\"\nlibexec.install \"README.md\"\n"
				b.Test = "assert_match \"{{ .Version }}\", shell_output(\"#{bin}/mycli version\")\n"
				b.Caveats = "Run `mycli init` to create #{HOME}/.mycli.\n\n  Docs: \"https://example.com\"\n"
				b.Dependencies = []config.BrewDependency{
					{Name: "git"}, {Name: "go", Type: "build"}, {Name: "bash-completion", Type: "optional"},
					{Name: "pinentry-mac", OS: "mac"}, {Name: "util-linux", OS: "linux", Type: "recommended"},
				}
				b.Conflicts = []string{"mycli-legacy", "othercli"}
				b.Service = "run [opt_bin/\"mycli\", \"serve\"]\nkeep_alive true\nlog_path var/\"log/mycli.log\"\n"
				b.PostInstall = "(var/\"mycli\").mkpath\n"
				b.CustomBlock = "skip_clean \"libexec\"\n"
				b.CustomRequire = "lib/custom_download_strategy"
				b.DownloadStrategy = "GitHubPrivateRepositoryReleaseDownloadStrategy"
				b.URLHeaders = []string{"Accept: application/octet-stream"}
				return input(b, fourPlatforms(false)...)
			},
			wantWarnings: []string{"post_install is deprecated"},
		},
		{
			name: "url_template",
			in: func(t *testing.T) Input {
				b := baseBrew()
				b.URLTemplate = "https://downloads.example.com/{{ .ProjectName }}/{{ .Version }}/{{ .ArtifactName }}"
				return input(b, fourPlatforms(false)...)
			},
		},
		{
			name: "non_github_url",
			in: func(t *testing.T) Input {
				arts := fourPlatforms(false)
				for i := range arts {
					arts[i].DownloadURL = "https://cdn.example.org/releases/" + testTag + "/" + arts[i].Name
				}
				in := input(baseBrew(), arts...)
				in.Owner, in.Repo = "", ""
				return in
			},
		},
	}
}

func TestGolden(t *testing.T) {
	for _, tc := range goldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := generateOne(t, tc.in(t))
			golden := filepath.Join("testdata", tc.name+".golden")
			if *update {
				if err := os.WriteFile(golden, f.Content, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create)", err)
			}
			if !bytes.Equal(f.Content, want) {
				t.Errorf("formula differs from %s (run with -update):\n%s", golden, f.Content)
			}
			all := strings.Join(f.Warnings, "\n")
			for _, w := range tc.wantWarnings {
				if !strings.Contains(all, w) {
					t.Errorf("missing warning %q in:\n%s", w, all)
				}
			}
			if err := Verify(f); err != nil {
				t.Errorf("Verify: %v", err)
			}
		})
	}
}

func TestGoldenRubySyntax(t *testing.T) {
	ruby := requireRuby(t)
	for _, tc := range goldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := generateOne(t, tc.in(t))
			p := filepath.Join(t.TempDir(), f.Name+".rb")
			if err := os.WriteFile(p, f.Content, 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(ruby, "-c", p).CombinedOutput(); err != nil {
				t.Fatalf("ruby -c: %v\n%s\n%s", err, out, f.Content)
			}
		})
	}
}

func requireRuby(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby not found in PATH")
	}
	return p
}

func TestGenerateDefaults(t *testing.T) {
	f := generateOne(t, input(baseBrew(), fourPlatforms(false)...))
	if f.Name != "mycli" || f.ClassName != "Mycli" || f.Path != "dist/homebrew/mycli.rb" {
		t.Errorf("got name=%q class=%q path=%q", f.Name, f.ClassName, f.Path)
	}
	if len(f.Archives) != 4 {
		t.Errorf("archives = %d", len(f.Archives))
	}
	c := string(f.Content)
	for _, want := range []string{
		`url "https://github.com/acme/mycli/releases/download/v1.2.3/mycli_1.2.3_darwin_arm64.tar.gz"`,
		`sha256 "` + sumLinuxX86 + `"`,
		`bin.install "mycli"`,
		`assert_predicate bin/"mycli", :executable?`,
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(c, "version \"") {
		t.Error("version must be omitted for GitHub release URLs whose tag is the version")
	}
}

func TestGenerateDirectoryAndOutput(t *testing.T) {
	b := baseBrew()
	b.Directory = "Formula"
	f := generateOne(t, input(b, fourPlatforms(false)...))
	if f.Path != "dist/homebrew/Formula/mycli.rb" {
		t.Errorf("path = %q", f.Path)
	}
	b.Output = "out/{{ .ProjectName }}.rb"
	f = generateOne(t, input(b, fourPlatforms(false)...))
	if f.Path != "out/mycli.rb" {
		t.Errorf("path = %q", f.Path)
	}
	for _, bad := range []string{"../escape.rb", "/abs/x.rb", "out/x.txt"} {
		b.Output = bad
		if _, err := Generate(input(b, fourPlatforms(false)...)); err == nil {
			t.Errorf("output %q accepted", bad)
		}
	}
}

func TestGenerateVersion(t *testing.T) {
	b := baseBrew()
	b.Version = "1.2.3-hotfix"
	f := generateOne(t, input(b, fourPlatforms(false)...))
	if !strings.Contains(string(f.Content), `version "1.2.3-hotfix"`) {
		t.Errorf("explicit version missing:\n%s", f.Content)
	}
	for _, bad := range []string{"v1.2.3", "1.2.3_1", "1.2 3", `1.2"3`} {
		b.Version = bad
		if _, err := Generate(input(b, fourPlatforms(false)...)); err == nil {
			t.Errorf("version %q accepted", bad)
		}
	}
	// A tag without "v" is also recognized.
	in := input(baseBrew(), fourPlatforms(false)...)
	in.Tag = "1.2.3"
	if f := generateOne(t, in); strings.Contains(string(f.Content), "version \"") {
		t.Error("version must be omitted when the tag equals the version")
	}
	// A tag that is not the version needs an explicit version.
	in.Tag = "release-7"
	if f := generateOne(t, in); !strings.Contains(string(f.Content), `version "1.2.3"`) {
		t.Errorf("explicit version expected:\n%s", f.Content)
	}
}

func TestGenerateURLs(t *testing.T) {
	// GitHub URL path elements are escaped.
	in := input(baseBrew(), fourPlatforms(false)...)
	in.Tag = "cli/v1.2.3"
	f := generateOne(t, in)
	if !strings.Contains(string(f.Content), "/releases/download/cli%2Fv1.2.3/") {
		t.Errorf("tag not escaped:\n%s", f.Content)
	}
	if !strings.Contains(string(f.Content), `version "1.2.3"`) {
		t.Error("a tag that is not v<version> needs an explicit version")
	}

	// No source of URLs.
	in = input(baseBrew(), fourPlatforms(false)...)
	in.Owner = ""
	if _, err := Generate(in); err == nil || !strings.Contains(err.Error(), "no download URL") {
		t.Errorf("err = %v", err)
	}

	for _, tc := range []struct{ tmpl, err string }{
		{"http://example.com/{{ .ArtifactName }}", "https"},
		{"ftp://example.com/x", "https"},
		{"https://user:pw@example.com/x", "credentials"},
		{"https://example.com/a b", "whitespace"},
		{"file:///tmp/{{ .ArtifactName }}", "HEPHAESTUS_ALLOW_FILE_URLS"},
		{"https:///nohost", "host"},
	} {
		b := baseBrew()
		b.URLTemplate = tc.tmpl
		_, err := Generate(input(b, fourPlatforms(false)...))
		if err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("url_template %q: err = %v, want %q", tc.tmpl, err, tc.err)
		}
	}

	t.Setenv(AllowFileURLsEnv, "1")
	b := baseBrew()
	b.URLTemplate = "file:///tmp/dist/{{ .ArtifactName }}"
	f = generateOne(t, input(b, fourPlatforms(false)...))
	if !strings.Contains(string(f.Content), `url "file:///tmp/dist/mycli_1.2.3_linux_amd64.tar.gz"`) ||
		!strings.Contains(string(f.Content), `version "1.2.3"`) {
		t.Errorf("file URL formula:\n%s", f.Content)
	}
}

func TestSelectArchives(t *testing.T) {
	// Duplicate OS/arch.
	dup := fourPlatforms(false)[0]
	dup.ID, dup.Name, dup.Path = "other", "other.tar.gz", "dist/other.tar.gz"
	_, err := Generate(input(baseBrew(), append(fourPlatforms(false), dup)...))
	if err == nil || !strings.Contains(err.Error(), "several archives for darwin/amd64") {
		t.Errorf("err = %v", err)
	}
	// ids selects one.
	b := baseBrew()
	b.IDs = []string{"other"}
	f := generateOne(t, input(b, append(fourPlatforms(false), dup)...))
	if len(f.Archives) != 1 || f.Archives[0].Name != "other.tar.gz" {
		t.Errorf("archives = %+v", f.Archives)
	}
	// Nothing usable.
	_, err = Generate(input(baseBrew(), arch("windows", "amd64", "", stdFiles(), "mycli.exe")))
	if err == nil || !strings.Contains(err.Error(), "no darwin or linux") {
		t.Errorf("err = %v", err)
	}
	// Missing executables without install.
	a := fourPlatforms(false)[0]
	a.Executables = nil
	if _, err := Generate(input(baseBrew(), a)); err == nil || !strings.Contains(err.Error(), "no executables") {
		t.Errorf("err = %v", err)
	}
	// Missing sha256 is never invented.
	a = fourPlatforms(false)[0]
	a.SHA256 = ""
	if _, err := Generate(input(baseBrew(), a)); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("err = %v", err)
	}
}

func TestInstallPerTargetTemplateRejected(t *testing.T) {
	b := baseBrew()
	b.Install = `bin.install "mycli_{{ .Os }}" => "mycli"`
	_, err := Generate(input(b, fourPlatforms(false)...))
	if err == nil || !strings.Contains(err.Error(), "renders differently") {
		t.Errorf("err = %v", err)
	}
}

func TestDefaultInstallCommonExecutables(t *testing.T) {
	arts := fourPlatforms(false)
	arts[0].Files = append(arts[0].Files, file("extra", modeExec))
	arts[0].Executables = append(arts[0].Executables, "extra")
	f := generateOne(t, input(baseBrew(), arts...))
	if strings.Contains(string(f.Content), `"extra"`) {
		t.Error("an executable missing from some archives must not be installed by default")
	}
}

func TestGenerateMultipleAndDuplicateOutput(t *testing.T) {
	in := input(baseBrew(), fourPlatforms(false)...)
	b2 := baseBrew()
	b2.Name = "mycli@1"
	in.Config.Brews = append(in.Config.Brews, b2)
	fs, err := Generate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 || fs[1].ClassName != "MycliAT1" || fs[1].Path != "dist/homebrew/mycli@1.rb" {
		t.Errorf("got %+v", fs[1])
	}
	in.Config.Brews[1].Output = "dist/homebrew/mycli.rb"
	if _, err := Generate(in); err == nil || !strings.Contains(err.Error(), "also written") {
		t.Errorf("err = %v", err)
	}
}

func TestRun(t *testing.T) {
	root := t.TempDir()
	in := input(baseBrew(), fourPlatforms(false)...)
	in.Config.Root = root
	fs, err := Run(in)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "dist", "homebrew", "mycli.rb"))
	if err != nil || !bytes.Equal(data, fs[0].Content) {
		t.Fatalf("written file: %v", err)
	}
	got := in.Manifest.Filter(manifest.KindFormula)
	if len(got) != 1 || got[0].Path != "dist/homebrew/mycli.rb" || got[0].Name != "mycli.rb" || !manifest.IsSHA256(got[0].SHA256) || got[0].Source.Stage != "homebrew" {
		t.Errorf("manifest formula = %+v", got)
	}
	if err := in.Manifest.Validate(); err != nil {
		t.Error(err)
	}
	// Re-running replaces the entry instead of accumulating.
	if _, err := Run(in); err != nil {
		t.Fatal(err)
	}
	if n := len(in.Manifest.Filter(manifest.KindFormula)); n != 1 {
		t.Errorf("formula entries after re-run = %d", n)
	}
}

func TestRunRefusesSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dist", "homebrew")); err != nil {
		t.Skip(err)
	}
	in := input(baseBrew(), fourPlatforms(false)...)
	in.Config.Root = root
	if _, err := Run(in); err == nil || !strings.Contains(err.Error(), "outside the project root") {
		t.Errorf("err = %v", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("wrote outside the root: %v", entries)
	}
}

func TestGenerateLicenseAndDescWarnings(t *testing.T) {
	b := baseBrew()
	b.Description = "the mycli tool."
	b.License = ""
	f := generateOne(t, input(b, fourPlatforms(false)...))
	all := strings.Join(f.Warnings, "\n")
	for _, w := range []string{"article", "full stop", "license is empty"} {
		if !strings.Contains(all, w) {
			t.Errorf("missing warning %q in %s", w, all)
		}
	}
	b.License = "MIT; rm -rf /"
	if _, err := Generate(input(b, fourPlatforms(false)...)); err == nil {
		t.Error("invalid license accepted")
	}
}
