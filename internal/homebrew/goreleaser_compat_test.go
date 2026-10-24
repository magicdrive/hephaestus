package homebrew

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/compat/goreleaser"
	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/manifest"
)

// The GoReleaser compatibility fixture: formulae and archive listings that
// GoReleaser itself produced from testdata/compat/goreleaser/project
// (scripts/goreleaser-compat-fixture.sh).
var compatDir = filepath.Join("..", "..", "testdata", "compat", "goreleaser")

const compatVersion = "v2.19.0"

// platformFormula is what a formula installs on one platform.
type platformFormula struct {
	url, sha256 string
	bins        []string // sorted bin.install sources
}

// TestGoReleaserCompatibility generates formulae from the fixture's
// .goreleaser.yaml (through brews_from parsing) and its archives, and
// compares them, platform by platform, with the formulae GoReleaser wrote:
// download URL, checksum and installed executables must match. Intentional
// differences are asserted explicitly at the end.
func TestGoReleaserCompatibility(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(compatDir, "project", ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	project, brews, _, err := goreleaser.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Project: config.Project{Name: project}, Brews: brews}
	cfg.ApplyDefaults()
	in := Input{
		Config: cfg, Manifest: compatManifest(t), Vars: testVars(),
		// The source repository: the formula must still download from the
		// release.github repository of .goreleaser.yaml.
		Owner: "acme", Repo: "compatcli", Tag: "v1.2.3",
	}
	in.Vars.Project, in.Vars.ProjectName = project, project
	fs, err := Generate(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 2 {
		t.Fatalf("%d formulae", len(fs))
	}
	for _, f := range fs {
		t.Run(f.Name, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join(compatDir, compatVersion, "homebrew", f.Name+".rb"))
			if err != nil {
				t.Fatalf("GoReleaser wrote no %s.rb: %v", f.Name, err)
			}
			gr := goreleaserPlatforms(t, string(want))
			ours := ourPlatforms(t, f)
			if len(gr) == 0 {
				t.Fatal("no platforms parsed from the GoReleaser formula")
			}
			for plat, g := range gr {
				o, ok := ours[plat]
				if !ok {
					t.Errorf("%s: GoReleaser serves it, Hephaestus does not", plat)
					continue
				}
				if o.url != g.url || o.sha256 != g.sha256 {
					t.Errorf("%s: url/sha256\n  goreleaser %s %s\n  hephaestus %s %s", plat, g.url, g.sha256, o.url, o.sha256)
				}
				if strings.Join(o.bins, " ") != strings.Join(g.bins, " ") {
					t.Errorf("%s: installs %v, GoReleaser %v", plat, o.bins, g.bins)
				}
			}
			for plat := range ours {
				if _, ok := gr[plat]; !ok {
					t.Errorf("%s: Hephaestus serves it, GoReleaser does not", plat)
				}
			}

			// Intentional differences (docs/compatibility/goreleaser.md).
			c := string(f.Content)
			if !strings.Contains(string(want), `version "1.2.3"`) || strings.Contains(c, "\n  version ") {
				t.Error("version: GoReleaser writes it, Hephaestus omits it when the URL already carries it")
			}
			if !strings.Contains(string(want), "#{HOMEBREW_PREFIX}") || !strings.Contains(c, `\#{HOMEBREW_PREFIX}`) {
				t.Error("caveats: GoReleaser interpolates #{...}, Hephaestus escapes it")
			}
		})
	}
	for _, b := range brews {
		if d := DescribePublish(&b); !strings.HasPrefix(d, "direct commit") {
			t.Errorf("%s: published by %s, GoReleaser commits directly", b.Name, d)
		}
	}
}

// compatManifest builds archive artifacts from the fixture's listings and
// checksums.
func compatManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	sums := map[string]string{}
	data, err := os.ReadFile(filepath.Join(compatDir, compatVersion, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			sums[f[1]] = f[0]
		}
	}
	lists, err := filepath.Glob(filepath.Join(compatDir, compatVersion, "archives", "*.list"))
	if err != nil || len(lists) == 0 {
		t.Fatalf("no archive listings: %v", err)
	}
	nameRe := regexp.MustCompile(`^(compatcli(?:-intel)?)_1\.2\.3_(darwin|linux)_(amd64|arm64)\.tar\.gz$`)
	m := manifest.New("compatcli", "1.2.3", "v1.2.3", "")
	for _, l := range lists {
		name := strings.TrimSuffix(filepath.Base(l), ".list")
		p := nameRe.FindStringSubmatch(name)
		if p == nil {
			t.Fatalf("unexpected archive %s", name)
		}
		id := "default"
		if p[1] == "compatcli-intel" {
			id = "intel"
		}
		a := manifest.Artifact{
			Kind: manifest.KindArchive, ID: id, Name: name, Path: "dist/" + name, OS: p[2], Arch: p[3],
			Format: "tar.gz", SHA256: sums[name], Source: manifest.Source{Stage: "build", Provider: "goreleaser"},
		}
		f, err := os.Open(l)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			mode, path, ok := strings.Cut(sc.Text(), " ")
			if !ok {
				continue
			}
			fm := "0644"
			if strings.Contains(mode, "x") {
				fm = "0755"
				a.Executables = append(a.Executables, path)
			}
			a.Files = append(a.Files, manifest.File{Path: path, Type: "file", Mode: fm})
		}
		f.Close()
		m.Artifacts = append(m.Artifacts, a)
	}
	return m
}

var (
	urlLine    = regexp.MustCompile(`^\s*url "([^"]+)"`)
	sha256Line = regexp.MustCompile(`^\s*sha256 "([0-9a-f]{64})"`)
	binLine    = regexp.MustCompile(`^\s*bin\.install "([^"]+)"`)
)

// goreleaserPlatforms reads GoReleaser's formula layout: on_macos/on_linux
// blocks holding `if Hardware::CPU.<cpu>?` blocks (or one unconditional
// package serving both CPUs), each with url, sha256 and define_method install.
func goreleaserPlatforms(t *testing.T, src string) map[string]*platformFormula {
	t.Helper()
	out := map[string]*platformFormula{}
	goos, pending := "", ""
	var cur *platformFormula
	for _, l := range strings.Split(src, "\n") {
		s := strings.TrimSpace(l)
		switch {
		case s == "on_macos do":
			goos, pending, cur = "darwin", "", nil
		case s == "on_linux do":
			goos, pending, cur = "linux", "", nil
		case goos != "" && strings.HasPrefix(s, "if Hardware::CPU.intel?"):
			pending = "amd64"
		case goos != "" && strings.HasPrefix(s, "if Hardware::CPU.arm?"):
			// Also opens the Rosetta caveats block: only a url starts a
			// package.
			pending = "arm64"
		case goos != "" && urlLine.MatchString(l):
			cur = &platformFormula{url: urlLine.FindStringSubmatch(l)[1]}
			if pending == "" { // unconditional: serves both CPUs
				out[goos+"/amd64"], out[goos+"/arm64"] = cur, cur
			} else {
				out[goos+"/"+pending] = cur
			}
			pending = ""
		case sha256Line.MatchString(l) && cur != nil:
			cur.sha256 = sha256Line.FindStringSubmatch(l)[1]
		case binLine.MatchString(l) && cur != nil:
			cur.bins = append(cur.bins, binLine.FindStringSubmatch(l)[1])
		case l == "  end":
			goos, pending, cur = "", "", nil
		}
	}
	for _, p := range out {
		sort.Strings(p.bins)
	}
	return out
}

// ourPlatforms reads a Hephaestus formula: the url and sha256 of each
// on_arm/on_intel block, and the bin.install calls of the single def install
// that apply to the archive behind that url.
func ourPlatforms(t *testing.T, f Formula) map[string]*platformFormula {
	t.Helper()
	body, err := installBody(f.Content)
	if err != nil {
		t.Fatal(err)
	}
	calls := analyzeInstall(body)
	byURL := map[string]manifest.Artifact{}
	for _, a := range f.Archives {
		u, _ := archiveURLForTest(f, a)
		byURL[u] = a
	}
	out := map[string]*platformFormula{}
	goos, cpu := "", ""
	var cur *platformFormula
	for _, l := range strings.Split(string(f.Content), "\n") {
		s := strings.TrimSpace(l)
		switch {
		case s == "on_macos do":
			goos = "darwin"
		case s == "on_linux do":
			goos = "linux"
		case s == "on_arm do":
			cpu = "arm64"
		case s == "on_intel do":
			cpu = "amd64"
		case urlLine.MatchString(l) && goos != "":
			cur = &platformFormula{url: urlLine.FindStringSubmatch(l)[1]}
			if cpu == "" { // a single archive for the OS
				out[goos+"/amd64"], out[goos+"/arm64"] = cur, cur
			} else {
				out[goos+"/"+cpu] = cur
			}
		case sha256Line.MatchString(l) && cur != nil:
			cur.sha256 = sha256Line.FindStringSubmatch(l)[1]
			a, ok := byURL[cur.url]
			if !ok {
				t.Fatalf("url %s matches no archive", cur.url)
			}
			for _, c := range calls {
				if c.receiver == "bin" && appliesTo(c.platforms, a) {
					cur.bins = append(cur.bins, c.srcs...)
				}
			}
			sort.Strings(cur.bins)
			cur, cpu = nil, ""
		}
	}
	return out
}

// archiveURLForTest finds the url line the formula wrote for archive a.
func archiveURLForTest(f Formula, a manifest.Artifact) (string, bool) {
	for _, l := range strings.Split(string(f.Content), "\n") {
		if m := urlLine.FindStringSubmatch(l); m != nil && strings.HasSuffix(m[1], "/"+a.Name) {
			return m[1], true
		}
	}
	return "", false
}
