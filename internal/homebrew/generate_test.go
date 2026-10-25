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
			name: "partial_executables",
			in: func(t *testing.T) Input {
				arts := fourPlatforms(false)
				for i := range arts {
					if arts[i].OS == "darwin" {
						arts[i].Files = append(arts[i].Files, file("helper", modeExec))
						arts[i].Executables = append(arts[i].Executables, "helper")
					}
				}
				return input(baseBrew(), arts...)
			},
		},
		{
			name: "per_target_install",
			in: func(t *testing.T) Input {
				var arts []manifest.Artifact
				for _, a := range fourPlatforms(false) {
					a.Files = append(a.Files, file("mycli_"+a.OS+"_"+a.Arch, modeExec))
					arts = append(arts, a)
				}
				b := baseBrew()
				b.Install = "bin.install \"mycli_{{ .Os }}_{{ .Arch }}\" => \"mycli\"\n"
				b.ExtraInstall = "doc.install \"README.md\"\n"
				return input(b, arts...)
			},
		},
		{
			name: "macos_intel_only",
			in: func(t *testing.T) Input {
				arts := fourPlatforms(false)
				return input(baseBrew(), arts[0], arts[2], arts[3])
			},
			wantWarnings: []string{"Apple Silicon Macs install the darwin/amd64 build"},
		},
		{
			// A Bash script: one archive for every platform.
			name: "noarch",
			in: func(t *testing.T) Input {
				a := arch("all", "all", "", []manifest.File{file("mycli", modeExec), file("mycli.1", modeRegular), file("completions/", ""),
					file("completions/mycli.bash", modeRegular), file("README.md", modeRegular)}, "mycli")
				b := baseBrew()
				b.Dependencies = []config.BrewDependency{{Name: "bash"}}
				b.Install = "bin.install \"mycli\"\nman1.install \"mycli.1\"\nbash_completion.install \"completions/mycli.bash\" => \"mycli\"\n"
				return input(b, a)
			},
		},
		{
			// Completions generated by the installed executable, on every
			// platform of a per-platform formula.
			name: "completions_generate",
			in: func(t *testing.T) Input {
				b := baseBrew()
				b.Completions = &config.BrewCompletions{Generate: &config.BrewCompletionGenerate{Args: []string{"completion"}}}
				return input(b, fourPlatforms(false)...)
			},
		},
		{
			// Completions shipped in a noarch archive, installed under the
			// names Homebrew expects.
			name: "completions_files",
			in: func(t *testing.T) Input {
				a := arch("all", "all", "", []manifest.File{file("mycli", modeExec), file("completions/", ""),
					file("completions/mycli.bash", modeRegular), file("completions/mycli.zsh", modeRegular),
					file("completions/mycli.fish", modeRegular), file("README.md", modeRegular)}, "mycli")
				b := baseBrew()
				b.Completions = &config.BrewCompletions{Files: config.BrewCompletionFiles{
					Bash: "completions/mycli.bash", Zsh: "completions/mycli.zsh", Fish: "completions/mycli.fish"}}
				return input(b, a)
			},
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

// TestGenerateURLInvalidUTF8 is a regression test: a url_template value that
// renders to invalid UTF-8 used to panic in the Ruby quoting.
func TestGenerateURLInvalidUTF8(t *testing.T) {
	b := baseBrew()
	b.URLTemplate = "https://example.com/{{ .Env.DL_SEG }}/{{ .ArtifactName }}"
	in := input(b, fourPlatforms(false)...)
	in.Vars.Env = map[string]string{"DL_SEG": "a\xffb"}
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Generate panicked: %v", r)
			}
		}()
		_, err = Generate(in)
	}()
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("err = %v, want an invalid UTF-8 error", err)
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

// TestInstallPerTargetTemplate renders install for every archive, as
// GoReleaser does, and selects each rendering with a platform condition.
func TestInstallPerTargetTemplate(t *testing.T) {
	var arts []manifest.Artifact
	for _, a := range fourPlatforms(false) {
		a.Files = append(a.Files, file("mycli_"+a.OS, modeExec))
		arts = append(arts, a)
	}
	b := baseBrew()
	b.Install = `bin.install "mycli_{{ .Os }}" => "mycli"`
	f := generateOne(t, input(b, arts...))
	for _, want := range []string{
		"    if OS.mac?\n      bin.install \"mycli_darwin\" => \"mycli\"\n    end\n",
		"    if OS.linux?\n      bin.install \"mycli_linux\" => \"mycli\"\n    end\n",
	} {
		if !strings.Contains(string(f.Content), want) {
			t.Errorf("formula lacks %q:\n%s", want, f.Content)
		}
	}
	// The verifier checks each rendering against the archives it selects.
	b.Install = `bin.install "mycli_{{ .Arch }}" => "mycli"`
	if _, err := Generate(input(b, arts...)); err == nil || !strings.Contains(err.Error(), `"mycli_arm64"`) {
		t.Errorf("err = %v", err)
	}
}

// TestDefaultInstallEveryExecutable checks that, as in GoReleaser, every
// archive's executables are installed, under a condition when only some
// archives have them.
func TestDefaultInstallEveryExecutable(t *testing.T) {
	arts := fourPlatforms(false)
	for i := range arts {
		if arts[i].OS == "darwin" {
			arts[i].Files = append(arts[i].Files, file("helper", modeExec))
			arts[i].Executables = append(arts[i].Executables, "helper")
		}
	}
	arts[3].Files = append(arts[3].Files, file("armtool", modeExec)) // linux/arm64
	arts[3].Executables = append(arts[3].Executables, "armtool")
	f := generateOne(t, input(baseBrew(), arts...))
	want := "  def install\n" +
		"    bin.install \"mycli\"\n" +
		"    bin.install \"armtool\" if OS.linux? && Hardware::CPU.arm?\n" +
		"    bin.install \"helper\" if OS.mac?\n" +
		"  end\n"
	if !strings.Contains(string(f.Content), want) {
		t.Errorf("install block:\n%s", f.Content)
	}
}

func TestPlatformConds(t *testing.T) {
	all := fourPlatforms(false) // darwin/amd64, darwin/arm64, linux/amd64, linux/arm64
	pick := func(arts []manifest.Artifact, idx ...int) []manifest.Artifact {
		var out []manifest.Artifact
		for _, i := range idx {
			out = append(out, arts[i])
		}
		return out
	}
	for _, tc := range []struct {
		all  []manifest.Artifact
		sel  []int
		want string
	}{
		{all, []int{0, 1, 2, 3}, ""},
		{all, []int{0, 1}, "OS.mac?"},
		{all, []int{1, 3}, "Hardware::CPU.arm?"},
		{all, []int{0, 2}, "Hardware::CPU.intel?"},
		{all, []int{1}, "OS.mac? && Hardware::CPU.arm?"},
		{all, []int{0, 1, 3}, "OS.mac? | OS.linux? && Hardware::CPU.arm?"},
		{all, []int{0, 3}, "OS.mac? && Hardware::CPU.intel? | OS.linux? && Hardware::CPU.arm?"},
		// A single darwin archive also serves Apple Silicon: OS only.
		{pick(all, 0, 2, 3), []int{0, 1}, "OS.mac? | OS.linux? && Hardware::CPU.intel?"},
		{pick(all, 0, 2, 3), []int{0}, "OS.mac?"},
	} {
		got := strings.Join(platformConds(pick(tc.all, tc.sel...), tc.all), " | ")
		if got != tc.want {
			t.Errorf("sel %v: got %q, want %q", tc.sel, got, tc.want)
		}
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

// TestGenerateDirectoryTemplate renders directory as a template (GoReleaser
// uses it verbatim).
func TestGenerateDirectoryTemplate(t *testing.T) {
	b := baseBrew()
	b.Directory = "Formula/{{ .ProjectName }}"
	f := generateOne(t, input(b, fourPlatforms(false)...))
	if f.Path != "dist/homebrew/Formula/mycli/mycli.rb" {
		t.Errorf("path = %q", f.Path)
	}
}

// TestDependencyWarnings checks the documented dependency differences:
// version is ignored with a warning, and an OS-specific dependency is
// skipped with a warning when the formula has no archive for that OS.
func TestDependencyWarnings(t *testing.T) {
	b := baseBrew()
	b.Dependencies = []config.BrewDependency{{Name: "git", Version: "2.40"}, {Name: "util-linux", OS: "linux"}, {Name: "pinentry-mac", OS: "mac"}}
	f := generateOne(t, input(b, fourPlatforms(false)[:2]...)) // macOS only
	w := strings.Join(f.Warnings, "\n")
	for _, want := range []string{`dependency git: version "2.40" is ignored`, "dependency util-linux is for Linux but the formula has no Linux archives; skipped"} {
		if !strings.Contains(w, want) {
			t.Errorf("missing warning %q in\n%s", want, w)
		}
	}
	c := string(f.Content)
	if !strings.Contains(c, `  depends_on "git"`+"\n") || strings.Contains(c, "util-linux") || !strings.Contains(c, `    depends_on "pinentry-mac"`) {
		t.Errorf("dependencies:\n%s", c)
	}
}

// TestExtraInstallPerTarget applies the per-archive install rule to
// extra_install, and keeps it after install.
func TestExtraInstallPerTarget(t *testing.T) {
	var arts []manifest.Artifact
	for _, a := range fourPlatforms(false) {
		a.Files = append(a.Files, file("doc_"+a.OS+".md", modeRegular))
		arts = append(arts, a)
	}
	b := baseBrew()
	b.ExtraInstall = `doc.install "doc_{{ .Os }}.md"`
	f := generateOne(t, input(b, arts...))
	want := "    bin.install \"mycli\"\n    if OS.mac?\n      doc.install \"doc_darwin.md\"\n    end\n    if OS.linux?\n      doc.install \"doc_linux.md\"\n    end\n"
	if !strings.Contains(string(f.Content), want) {
		t.Errorf("install:\n%s", f.Content)
	}
}

func TestNoarchSelection(t *testing.T) {
	noarch := arch("all", "all", "", stdFiles(), "mycli")
	plat := fourPlatforms(false)
	if _, err := Generate(input(baseBrew(), append(plat, noarch)...)); err == nil || !strings.Contains(err.Error(), "both platform-specific and noarch") {
		t.Errorf("mixed: %v", err)
	}
	other := noarch
	other.Name, other.Path, other.ID = "mycli_1.2.3_docs.tar.gz", "dist/mycli_1.2.3_docs.tar.gz", "docs"
	if _, err := Generate(input(baseBrew(), noarch, other)); err == nil || !strings.Contains(err.Error(), "several noarch archives") {
		t.Errorf("two noarch: %v", err)
	}
	// brews[].ids selects one kind.
	b := baseBrew()
	b.IDs = []string{"docs"}
	in := input(b, append(plat, other)...)
	in.Config.Archives = []config.Archive{{ID: "default"}, {ID: "docs"}}
	f := generateOne(t, in)
	if !strings.Contains(string(f.Content), "\n  url \"https://github.com/acme/mycli/releases/download/v1.2.3/mycli_1.2.3_docs.tar.gz\"") || strings.Contains(string(f.Content), "on_macos") {
		t.Errorf("noarch formula:\n%s", f.Content)
	}
}

func TestGenerateCompletions(t *testing.T) {
	gen := func(c *config.BrewCompletions, arts ...manifest.Artifact) (string, error) {
		b := baseBrew()
		b.Completions = c
		if len(arts) == 0 {
			arts = fourPlatforms(false)
		}
		fs, err := Generate(input(b, arts...))
		if err != nil {
			return "", err
		}
		return string(fs[0].Content), nil
	}
	for _, tc := range []struct {
		name string
		c    *config.BrewCompletions
		want string
	}{
		{"default shells are not listed", &config.BrewCompletions{Generate: &config.BrewCompletionGenerate{Args: []string{"completion"}}},
			`generate_completions_from_executable(bin/"mycli", "completion")`},
		{"same shells in another order are the default", &config.BrewCompletions{Generate: &config.BrewCompletionGenerate{Args: []string{"completion"}, Shells: []string{"fish", "bash", "zsh"}}},
			`generate_completions_from_executable(bin/"mycli", "completion")`},
		{"subset of shells", &config.BrewCompletions{Generate: &config.BrewCompletionGenerate{Args: []string{"completion"}, Shells: []string{"zsh", "bash"}}},
			`generate_completions_from_executable(bin/"mycli", "completion", shells: [:bash, :zsh])`},
		{"flag format", &config.BrewCompletions{Generate: &config.BrewCompletionGenerate{Args: []string{"completions"}, ShellParameterFormat: "flag"}},
			`generate_completions_from_executable(bin/"mycli", "completions", shell_parameter_format: :flag)`},
		{"cobra keeps the three shells (its default adds pwsh)", &config.BrewCompletions{Generate: &config.BrewCompletionGenerate{ShellParameterFormat: "cobra"}},
			`generate_completions_from_executable(bin/"mycli", shell_parameter_format: :cobra, shells: [:bash, :zsh, :fish])`},
		{"arguments are Ruby literals", &config.BrewCompletions{Generate: &config.BrewCompletionGenerate{Args: []string{`x"; system("id`, "#{ENV}"}}},
			`generate_completions_from_executable(bin/"mycli", "x\"; system(\"id", "\#{ENV}")`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := gen(tc.c)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(c, "\n    "+tc.want+"\n  end\n") {
				t.Fatalf("missing %s in:\n%s", tc.want, c)
			}
		})
	}

	t.Run("no completions: formula unchanged", func(t *testing.T) {
		c, err := gen(nil)
		if err != nil || strings.Contains(c, "completion") {
			t.Fatalf("%v\n%s", err, c)
		}
	})
	t.Run("executable not installed", func(t *testing.T) {
		_, err := gen(&config.BrewCompletions{Executable: "other", Generate: &config.BrewCompletionGenerate{}})
		if err == nil || !strings.Contains(err.Error(), `bin/"other"`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("executable missing from one archive", func(t *testing.T) {
		arts := fourPlatforms(false)
		arts[3].Executables = nil
		arts[3].Files = []manifest.File{file("LICENSE", modeRegular), file("other", modeExec)}
		arts[3].Executables = []string{"other"}
		_, err := gen(&config.BrewCompletions{Generate: &config.BrewCompletionGenerate{}}, arts...)
		if err == nil || !strings.Contains(err.Error(), arts[3].Name) {
			t.Fatalf("err = %v", err)
		}
	})
	noarch := func(extra ...manifest.File) manifest.Artifact {
		return arch("all", "all", "", append([]manifest.File{file("mycli", modeExec)}, extra...), "mycli")
	}
	t.Run("generated and installed twice", func(t *testing.T) {
		b := baseBrew()
		b.ExtraInstall = `bash_completion.install "c.bash" => "mycli"`
		b.Completions = &config.BrewCompletions{Generate: &config.BrewCompletionGenerate{}}
		_, err := Generate(input(b, noarch(file("c.bash", modeRegular))))
		if err == nil || !strings.Contains(err.Error(), "generated (completions.generate) and also installed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("another command's completion is not a conflict", func(t *testing.T) {
		b := baseBrew()
		b.ExtraInstall = `bash_completion.install "c.bash" => "other-tool"`
		b.Completions = &config.BrewCompletions{Generate: &config.BrewCompletionGenerate{}}
		if _, err := Generate(input(b, noarch(file("c.bash", modeRegular)))); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("generate written twice", func(t *testing.T) {
		b := baseBrew()
		b.ExtraInstall = `generate_completions_from_executable(bin/"mycli", "completion")`
		b.Completions = &config.BrewCompletions{Generate: &config.BrewCompletionGenerate{Args: []string{"completion"}}}
		_, err := Generate(input(b, noarch()))
		if err == nil || !strings.Contains(err.Error(), "2 times") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("executable put into bin by install_symlink", func(t *testing.T) {
		b := baseBrew()
		b.Install = "libexec.install \"mycli\"\nbin.install_symlink libexec/\"mycli\""
		b.Completions = &config.BrewCompletions{Generate: &config.BrewCompletionGenerate{Args: []string{"completion"}}}
		if _, err := Generate(input(b, noarch())); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("long call is wrapped for brew style", func(t *testing.T) {
		b := baseBrew()
		b.Name = "hephaestus-long-name"
		a := arch("all", "all", "", []manifest.File{file("hephaestus-long-name", modeExec)}, "hephaestus-long-name")
		b.Completions = &config.BrewCompletions{Generate: &config.BrewCompletionGenerate{Args: []string{"completion"}, ShellParameterFormat: "cobra", Shells: []string{"bash", "zsh"}}}
		fs, err := Generate(input(b, a))
		if err != nil {
			t.Fatal(err)
		}
		c := string(fs[0].Content)
		want := "    generate_completions_from_executable(bin/\"hephaestus-long-name\", \"completion\",\n" +
			"                                         shell_parameter_format: :cobra, shells: [:bash, :zsh])\n"
		if !strings.Contains(c, want) {
			t.Fatalf("missing wrapped call:\n%s", c)
		}
		for _, l := range strings.Split(c, "\n") {
			if len(l) > maxFormulaLine {
				t.Errorf("line longer than %d: %q", maxFormulaLine, l)
			}
		}
		if got := CompletionLines(&b); len(got) != 1 || strings.Contains(got[0], "\n") {
			t.Errorf("CompletionLines = %q, want one logical line", got)
		}
	})
	t.Run("shipped file missing from the archive", func(t *testing.T) {
		_, err := gen(&config.BrewCompletions{Files: config.BrewCompletionFiles{Zsh: "completions/_mycli"}})
		if err == nil || !strings.Contains(err.Error(), `"completions/_mycli" (zsh_completion.install`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("shipped file installed twice is a collision", func(t *testing.T) {
		a := arch("all", "all", "", []manifest.File{file("mycli", modeExec), file("c.bash", modeRegular)}, "mycli")
		b := baseBrew()
		b.ExtraInstall = `bash_completion.install "c.bash" => "mycli"`
		b.Completions = &config.BrewCompletions{Files: config.BrewCompletionFiles{Bash: "c.bash"}}
		_, err := Generate(input(b, a))
		if err == nil || !strings.Contains(err.Error(), "installed twice") {
			t.Fatalf("err = %v", err)
		}
	})
}
