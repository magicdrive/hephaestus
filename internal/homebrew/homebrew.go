// Package homebrew generates Homebrew formulae for prebuilt archives.
//
// Configuration follows GoReleaser's `brews` entries, but the output follows
// current Homebrew (7.x) style rules rather than GoReleaser's template:
//
//   - URLs live in two-level on_macos/on_linux + on_arm/on_intel blocks
//     (FormulaAudit/ComponentsOrder rejects url directly under on_macos);
//   - there is a single `def install` instead of per-platform define_method;
//   - 32-bit ARM and 386 archives are skipped (Homebrew does not support
//     them), and is_64_bit? checks are never emitted;
//   - version is omitted when Homebrew derives the same value from a GitHub
//     release URL, to avoid the "redundant version" audit;
//   - OS-specific dependencies go inside on_macos/on_linux instead of
//     `if OS.mac?` modifiers;
//   - SPDX expressions become license any_of:/all_of: forms.
//
// Every configuration value that becomes Ruby data is emitted as an escaped
// string literal. User-written Ruby (install, test, service, ...) is embedded
// as code, as in GoReleaser, after checks that keep it from escaping its
// block; template values substituted into that code are validated.
package homebrew

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/pathutil"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// AllowFileURLsEnv enables file:// download URLs (tests and end-to-end runs
// against a local tap). Production formulae only use https.
const AllowFileURLsEnv = "HEPHAESTUS_ALLOW_FILE_URLS"

// Input is everything the formula stage reads.
type Input struct {
	Config   *config.Config
	Manifest *manifest.Manifest
	Vars     tmpl.Vars
	// Owner and Repo name the GitHub repository hosting the release assets;
	// with Tag they build download URLs when neither url_template nor a
	// manifest download_url is available. They may be empty.
	Owner, Repo string
	Tag         string
}

// Formula is one generated formula.
type Formula struct {
	Name      string
	ClassName string
	// Path is the project-relative, slash-separated output path.
	Path     string
	Content  []byte
	Warnings []string
	// Archives are the archives the formula installs, sorted by OS and arch.
	Archives []manifest.Artifact
}

// Generate renders every configured formula without writing anything.
func Generate(in Input) ([]Formula, error) {
	if in.Config == nil || in.Manifest == nil {
		return nil, errors.New("homebrew: configuration and manifest are required")
	}
	var out []Formula
	var errs []error
	paths := map[string]string{}
	for i := range in.Config.Brews {
		b := &in.Config.Brews[i]
		f, err := generate(in, b)
		if err != nil {
			errs = append(errs, fmt.Errorf("brews[%d] (%s): %w", i, b.Name, err))
			continue
		}
		if other, dup := paths[f.Path]; dup {
			errs = append(errs, fmt.Errorf("brews[%d] (%s): output %s is also written by formula %s", i, b.Name, f.Path, other))
			continue
		}
		paths[f.Path] = f.Name
		out = append(out, f)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// Run generates every formula, writes each atomically under the project
// root, and records it in the manifest as kind=formula (replacing earlier
// formula entries). The caller persists the manifest.
func Run(in Input) ([]Formula, error) {
	fs, err := Generate(in)
	if err != nil {
		return nil, err
	}
	root := in.Config.Root
	if root == "" {
		root = "."
	}
	for _, f := range fs {
		if err := writeConfined(root, f.Path, f.Content); err != nil {
			return nil, fmt.Errorf("formula %s: %w", f.Name, err)
		}
	}
	in.Manifest.RemoveKind(manifest.KindFormula)
	for _, f := range fs {
		sum := sha256.Sum256(f.Content)
		in.Manifest.Put(manifest.Artifact{
			Kind:   manifest.KindFormula,
			ID:     f.Name,
			Name:   path.Base(f.Path),
			Path:   f.Path,
			SHA256: hex.EncodeToString(sum[:]),
			Size:   int64(len(f.Content)),
			Source: manifest.Source{Stage: "homebrew"},
		})
	}
	return fs, nil
}

// writeConfined writes rel under root. Every step (creating parent
// directories, the temporary file and the final rename) goes through an
// os.Root, so a parent directory that is or becomes a symbolic link out of
// root is an error and nothing is created outside root. An output that is
// itself a symbolic link is refused rather than replaced.
func writeConfined(root, rel string, data []byte) error {
	if err := pathutil.ValidateRelative(rel); err != nil {
		return fmt.Errorf("output path %q: %w", rel, err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	if fi, err := r.Lstat(rel); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("output %s is a symbolic link", rel)
	}
	if err := manifest.WriteFileAtomicIn(r, rel, data, 0o644); err != nil {
		return fmt.Errorf("output %s: cannot write it inside the project root (does a directory on its path link outside the project root?): %w", rel, err)
	}
	return nil
}

// platform is one selected archive with its resolved URL.
type platform struct {
	archive manifest.Artifact
	url     string
}

func generate(in Input, b *config.Brew) (Formula, error) {
	f := Formula{Name: b.Name}
	var err error
	if f.ClassName, err = ClassName(b.Name); err != nil {
		return f, err
	}
	vars := in.Vars
	if vars.Tag == "" {
		vars.Tag = in.Tag
	}
	tag := in.Tag
	if tag == "" {
		tag = vars.Tag
	}
	warn := func(format string, args ...any) {
		f.Warnings = append(f.Warnings, fmt.Sprintf("formula %s: ", b.Name)+fmt.Sprintf(format, args...))
	}

	archives, sw, err := selectArchives(in.Manifest, b)
	for _, w := range sw {
		warn("%s", w)
	}
	if err != nil {
		return f, err
	}
	f.Archives = archives

	plats := make([]platform, len(archives))
	for i, a := range archives {
		u, err := archiveURL(in, b, vars, tag, a)
		if err != nil {
			return f, fmt.Errorf("archive %s: %w", a.Name, err)
		}
		plats[i] = platform{archive: a, url: u}
	}

	version, writeVersion, err := resolveVersion(b, vars, plats)
	if err != nil {
		return f, err
	}

	g := &gen{}
	g.line("# typed: false")
	g.line("# frozen_string_literal: true")
	g.line("")
	g.line("# This file was generated by Hephaestus. DO NOT EDIT.")
	if b.CustomRequire != "" {
		if !customRequireRe.MatchString(b.CustomRequire) {
			return f, fmt.Errorf("custom_require %q contains characters not allowed in a require path", b.CustomRequire)
		}
		g.line("require_relative " + mustQuote(b.CustomRequire))
	}
	g.line("")
	g.line("class " + f.ClassName + " < Formula")

	// Header: desc, homepage, version, license (no blank lines between).
	desc, err := tmpl.Render(b.Description, vars)
	if err != nil {
		return f, fmt.Errorf("description: %w", err)
	}
	for _, w := range lintDesc(desc, b.Name) {
		warn("%s", w)
	}
	if desc != "" {
		if err := g.attr("desc", desc); err != nil {
			return f, fmt.Errorf("description: %w", err)
		}
	}
	homepage, err := tmpl.Render(b.Homepage, vars)
	if err != nil {
		return f, fmt.Errorf("homepage: %w", err)
	}
	if homepage == "" {
		warn("homepage is empty; Homebrew requires a homepage")
	} else {
		if u, err := url.Parse(homepage); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return f, fmt.Errorf("homepage %q is not an http(s) URL", homepage)
		} else if u.Scheme != "https" {
			warn("homepage %q should use https", homepage)
		}
		if err := g.attr("homepage", homepage); err != nil {
			return f, fmt.Errorf("homepage: %w", err)
		}
	}
	// A noarch archive serves macOS and Linux, Intel and ARM: one top-level
	// url, placed where Homebrew's component order wants it (url, version,
	// sha256, then license and dependencies).
	noarch := len(plats) == 1 && IsNoarch(plats[0].archive)
	if noarch {
		if err := g.urlLine(b, vars, &plats[0], "  "); err != nil {
			return f, err
		}
	}
	if writeVersion {
		if err := g.attr("version", version); err != nil {
			return f, err
		}
	}
	if noarch {
		if err := g.sha256Line(&plats[0], "  "); err != nil {
			return f, err
		}
	}
	if b.License != "" {
		lic, err := licenseDSL(b.License)
		if err != nil {
			return f, err
		}
		g.line("  license " + lic)
	} else {
		warn("license is empty; set brews[].license or project.license")
	}

	// Dependencies.
	hasMac, hasLinux := false, false
	for _, p := range plats {
		hasMac = hasMac || p.archive.OS == "darwin"
		hasLinux = hasLinux || p.archive.OS == "linux"
	}
	deps := map[string][]config.BrewDependency{}
	for _, d := range b.Dependencies {
		if d.Version != "" {
			warn("dependency %s: version %q is ignored (Homebrew has no dependency version constraints; depend on a versioned formula such as name@version instead)", d.Name, d.Version)
		}
		switch d.OS {
		case "mac":
			if !hasMac {
				warn("dependency %s is for macOS but the formula has no macOS archives; skipped", d.Name)
				continue
			}
		case "linux":
			if !hasLinux {
				warn("dependency %s is for Linux but the formula has no Linux archives; skipped", d.Name)
				continue
			}
		}
		deps[d.OS] = append(deps[d.OS], d)
	}
	var top []string
	for _, d := range sortDeps(deps[""]) {
		s, err := depLine(d)
		if err != nil {
			return f, err
		}
		top = append(top, "  "+s)
	}
	switch {
	case hasMac && !hasLinux:
		top = append(top, "  depends_on :macos")
	case hasLinux && !hasMac:
		top = append(top, "  depends_on :linux")
	}
	if len(top) > 0 {
		g.line("")
		for _, l := range top {
			g.line(l)
		}
	}

	// Download blocks.
	for _, osb := range []struct{ goos, block, depOS string }{{"darwin", "on_macos", "mac"}, {"linux", "on_linux", "linux"}} {
		var arm, intel *platform
		for i := range plats {
			p := &plats[i]
			if p.archive.OS != osb.goos {
				continue
			}
			if p.archive.Arch == "arm64" {
				arm = p
			} else {
				intel = p
			}
		}
		if arm == nil && intel == nil {
			continue
		}
		g.line("")
		g.line("  " + osb.block + " do")
		for _, d := range sortDeps(deps[osb.depOS]) {
			s, err := depLine(d)
			if err != nil {
				return f, err
			}
			g.line("    " + s)
		}
		switch {
		case arm == nil && osb.goos == "darwin":
			// As GoReleaser: Apple Silicon Macs install the Intel build,
			// which runs under Rosetta 2. Homebrew does not allow url
			// directly in on_macos, so both CPU blocks name it.
			warn("no darwin/arm64 archive: Apple Silicon Macs install the darwin/amd64 build, which needs Rosetta 2")
			arm = intel
		case arm == nil:
			g.line("    depends_on arch: :x86_64")
			warn("no %s/arm64 archive: the formula is installable only on Intel %s", osb.goos, osName(osb.goos))
		case intel == nil:
			g.line("    depends_on arch: :arm64")
			warn("no %s/amd64 archive: the formula is installable only on ARM %s", osb.goos, osName(osb.goos))
		}
		for _, x := range []struct {
			p     *platform
			block string
		}{{arm, "on_arm"}, {intel, "on_intel"}} {
			if x.p == nil {
				continue
			}
			g.line("    " + x.block + " do")
			if err := g.urlStanza(b, vars, x.p, "      "); err != nil {
				return f, err
			}
			g.line("    end")
		}
		g.line("  end")
	}

	if len(b.Conflicts) > 0 {
		g.line("")
		for _, c := range b.Conflicts {
			g.line("  conflicts_with " + mustQuote(c))
		}
	}

	if b.CustomBlock != "" {
		code, err := g.code("custom_block", b.CustomBlock, vars, "  ")
		if err != nil {
			return f, err
		}
		if code != "" {
			g.line("")
			g.line(code)
		}
	}

	// def install
	install, err := installCode(b, vars, archives)
	if err != nil {
		return f, err
	}
	installBody, iw, err := codeBlock("install", install, "    ")
	if err != nil {
		return f, err
	}
	for _, w := range iw {
		warn("%s", w)
	}
	if installBody == "" {
		return f, errors.New("install is empty")
	}
	if err := checkCompletions(b, installBody, archives); err != nil {
		return f, err
	}
	g.line("")
	g.line("  def install")
	g.line(installBody)
	g.line("  end")

	if b.PostInstall != "" {
		code, err := g.code("post_install", b.PostInstall, vars, "    ")
		if err != nil {
			return f, err
		}
		if code != "" {
			warn("def post_install is deprecated since Homebrew 7.0 (removal planned for 2027-12-11); prefer post_install_steps in custom_block")
			g.line("")
			g.line("  def post_install")
			g.line(code)
			g.line("  end")
		}
	}

	if b.Caveats != "" {
		caveats, err := tmpl.Render(b.Caveats, vars)
		if err != nil {
			return f, fmt.Errorf("caveats: %w", err)
		}
		if lines := normalizeText(caveats); len(lines) > 0 {
			for _, r := range strings.Join(lines, "\n") {
				if r == 0x1b {
					warn("caveats contain ANSI escape sequences, which Homebrew rejects")
					break
				}
			}
			hd, err := rubyHeredoc(strings.Join(lines, "\n"), "    ")
			if err != nil {
				return f, fmt.Errorf("caveats: %w", err)
			}
			g.line("")
			g.line("  def caveats")
			g.line("    " + hd)
			g.line("  end")
		}
	}

	if b.Service != "" {
		code, err := g.code("service", b.Service, vars, "    ")
		if err != nil {
			return f, err
		}
		if code != "" {
			g.line("")
			g.line("  service do")
			g.line(code)
			g.line("  end")
		}
	}

	var testCode string
	if b.Test != "" {
		testCode, err = g.code("test", b.Test, vars, "    ")
	} else {
		// The default is generated, not a template: do not render it.
		testCode, _, err = codeBlock("test", defaultTest(installBody), "    ")
	}
	if err != nil {
		return f, err
	}
	if testCode == "" {
		warn("no test block: set brews[].test (Homebrew requires a test)")
	} else {
		g.line("")
		g.line("  test do")
		g.line(testCode)
		g.line("  end")
	}
	g.line("end")
	for _, w := range g.warnings {
		warn("%s", w)
	}
	f.Content = []byte(g.b.String())

	if b.VerifyEnabled() {
		vw, err := verifyInstall(b.Name, installBody, archives)
		f.Warnings = append(f.Warnings, vw...)
		if err != nil {
			return f, err
		}
	}

	f.Path, err = outputPath(in.Config, b, vars)
	if err != nil {
		return f, err
	}
	return f, nil
}

func osName(goos string) string {
	if goos == "darwin" {
		return "macOS"
	}
	return "Linux"
}

var customRequireRe = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

// gen accumulates formula source.
type gen struct {
	b        strings.Builder
	warnings []string
}

func (g *gen) line(s string) {
	g.b.WriteString(s)
	g.b.WriteByte('\n')
}

// attr writes `  name "value"` with value escaped.
func (g *gen) attr(name, value string) error {
	q, err := RubyString(value)
	if err != nil {
		return err
	}
	g.line("  " + name + " " + q)
	return nil
}

// code renders and checks a user Ruby block, indenting it with indent.
func (g *gen) code(field, src string, vars tmpl.Vars, indent string) (string, error) {
	rendered, err := renderCode(field, src, vars)
	if err != nil {
		return "", err
	}
	out, w, err := codeBlock(field, rendered, indent)
	g.warnings = append(g.warnings, w...)
	return out, err
}

// urlStanza writes the url and sha256 lines at indent.
func (g *gen) urlStanza(b *config.Brew, vars tmpl.Vars, p *platform, indent string) error {
	if err := g.urlLine(b, vars, p, indent); err != nil {
		return err
	}
	return g.sha256Line(p, indent)
}

func (g *gen) sha256Line(p *platform, indent string) error {
	if !manifest.IsSHA256(p.archive.SHA256) {
		return fmt.Errorf("archive %s has no valid sha256 in the manifest", p.archive.Name)
	}
	g.line(indent + "sha256 " + mustQuote(p.archive.SHA256))
	return nil
}

func (g *gen) urlLine(b *config.Brew, vars tmpl.Vars, p *platform, indent string) error {
	// Options go on continuation lines aligned under the URL, keys padded to
	// a table, as in homebrew-core (Layout/HashAlignment).
	type opt struct{ key, val string }
	var opts []opt
	if b.DownloadStrategy != "" {
		if !downloadStrategyRe.MatchString(b.DownloadStrategy) {
			return fmt.Errorf("download_strategy %q must be a Ruby class name or a :symbol", b.DownloadStrategy)
		}
		opts = append(opts, opt{"using:", b.DownloadStrategy})
	}
	if len(b.URLHeaders) > 0 {
		hv := vars.WithTarget(p.archive.OS, p.archive.Arch, p.archive.Arm)
		hs := make([]string, len(b.URLHeaders))
		for i, h := range b.URLHeaders {
			r, err := tmpl.Render(h, hv)
			if err != nil {
				return fmt.Errorf("url_headers[%d]: %w", i, err)
			}
			if strings.ContainsAny(r, "\r\n") {
				return fmt.Errorf("url_headers[%d]: header must be a single line", i)
			}
			q, err := RubyString(r)
			if err != nil {
				return fmt.Errorf("url_headers[%d]: %w", i, err)
			}
			hs[i] = q
		}
		opts = append(opts, opt{"headers:", "[" + strings.Join(hs, ", ") + "]"})
	}
	width := 0
	for _, o := range opts {
		width = max(width, len(o.key))
	}
	qurl, err := RubyString(p.url)
	if err != nil {
		return fmt.Errorf("download URL: %w", err)
	}
	s := indent + "url " + qurl
	for _, o := range opts {
		s += ",\n" + indent + "    " + o.key + strings.Repeat(" ", width-len(o.key)+1) + o.val
	}
	g.line(s)
	return nil
}

var downloadStrategyRe = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]*(::[A-Z][A-Za-z0-9_]*)*$|^:[a-z_]+$`)

// mustQuote quotes values already known to be valid UTF-8.
func mustQuote(s string) string {
	q, err := RubyString(s)
	if err != nil {
		// Only reachable for invalid UTF-8, which callers have excluded.
		panic(err)
	}
	return q
}

var depTypeOrder = map[string]int{"build": 0, "test": 1, "": 2, "recommended": 3, "optional": 4}

// sortDeps orders dependencies as FormulaAudit/DependencyOrder expects:
// build, test, runtime, recommended, optional; by name within each group.
func sortDeps(ds []config.BrewDependency) []config.BrewDependency {
	out := append([]config.BrewDependency(nil), ds...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if depTypeOrder[a.Type] != depTypeOrder[b.Type] {
			return depTypeOrder[a.Type] < depTypeOrder[b.Type]
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return out
}

var depNameRe = regexp.MustCompile(`^([a-z0-9][a-z0-9_-]*/[a-z0-9][a-z0-9_-]*/)?[a-z0-9][a-z0-9+_.-]*(@[0-9][0-9.]*)?$`)

func depLine(d config.BrewDependency) (string, error) {
	if !depNameRe.MatchString(d.Name) {
		return "", fmt.Errorf("dependency %q is not a valid formula name", d.Name)
	}
	s := "depends_on " + mustQuote(d.Name)
	switch d.Type {
	case "":
	case "build", "test", "optional", "recommended":
		s += " => :" + d.Type
	default:
		return "", fmt.Errorf("dependency %s: type %q must be build, test, optional or recommended", d.Name, d.Type)
	}
	return s, nil
}

// selectArchives picks the archives a formula installs: darwin/linux and
// amd64/arm64 only, one per OS/arch.
func selectArchives(m *manifest.Manifest, b *config.Brew) ([]manifest.Artifact, []string, error) {
	var warnings []string
	if b.Goarm != "" {
		warnings = append(warnings, "goarm is ignored: Homebrew does not support 32-bit ARM")
	}
	if b.Goamd64 != "" && b.Goamd64 != "v1" {
		warnings = append(warnings, fmt.Sprintf("goamd64 %q is ignored: the manifest does not record GOAMD64 variants", b.Goamd64))
	}
	byTarget := map[string][]manifest.Artifact{}
	var noarch []manifest.Artifact
	for _, a := range m.Filter(manifest.KindArchive, b.IDs...) {
		if IsNoarch(a) {
			noarch = append(noarch, a)
			continue
		}
		if a.OS != "darwin" && a.OS != "linux" {
			continue // Homebrew runs on macOS and Linux only
		}
		switch a.Arch {
		case "amd64", "arm64":
		default:
			warnings = append(warnings, fmt.Sprintf("archive %s (%s/%s%s) skipped: Homebrew supports only amd64 and arm64", a.Name, a.OS, a.Arch, armSuffix(a.Arm)))
			continue
		}
		key := a.OS + "/" + a.Arch
		byTarget[key] = append(byTarget[key], a)
	}
	if len(noarch) > 0 {
		names := make([]string, len(noarch))
		for i, a := range noarch {
			names[i] = a.Name
		}
		switch {
		case len(byTarget) > 0:
			return nil, warnings, fmt.Errorf("both platform-specific and noarch (os: all, arch: all) archives match (%s); select one kind with brews[].ids", strings.Join(names, ", "))
		case len(noarch) > 1:
			return nil, warnings, fmt.Errorf("several noarch archives (%s); select one with brews[].ids", strings.Join(names, ", "))
		}
		return noarch, warnings, nil
	}
	var out []manifest.Artifact
	var errs []error
	keys := make([]string, 0, len(byTarget))
	for k := range byTarget {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		as := byTarget[k]
		if len(as) > 1 {
			names := make([]string, len(as))
			for i, a := range as {
				names[i] = a.Name
			}
			errs = append(errs, fmt.Errorf("several archives for %s (%s); select one with brews[].ids", k, strings.Join(names, ", ")))
			continue
		}
		out = append(out, as[0])
	}
	if err := errors.Join(errs...); err != nil {
		return nil, warnings, err
	}
	if len(out) == 0 {
		sel := "any archive"
		if len(b.IDs) > 0 {
			sel = "archive ids " + strings.Join(b.IDs, ", ")
		}
		return nil, warnings, fmt.Errorf("no darwin or linux amd64/arm64 archives found in the manifest for %s", sel)
	}
	return out, warnings, nil
}

// IsNoarch reports whether an archive serves every platform: its target is
// os: all, arch: all (e.g. a script, or files that need no build).
func IsNoarch(a manifest.Artifact) bool { return a.OS == "all" && a.Arch == "all" }

func armSuffix(arm string) string {
	if arm == "" {
		return ""
	}
	return "v" + arm
}

// archiveURL resolves the download URL: url_template, then the manifest's
// download_url, then the GitHub release download URL.
func archiveURL(in Input, b *config.Brew, vars tmpl.Vars, tag string, a manifest.Artifact) (string, error) {
	var u string
	switch {
	case b.URLTemplate != "":
		av := vars.WithTarget(a.OS, a.Arch, a.Arm)
		av.ArtifactName, av.ArtifactPath, av.ArtifactExt = a.Name, a.Path, artifactExt(a)
		r, err := tmpl.Render(b.URLTemplate, av)
		if err != nil {
			return "", fmt.Errorf("url_template: %w", err)
		}
		u = r
	case a.DownloadURL != "":
		u = a.DownloadURL
	case in.Owner != "" && in.Repo != "" && tag != "":
		u = "https://github.com/" + url.PathEscape(in.Owner) + "/" + url.PathEscape(in.Repo) +
			"/releases/download/" + url.PathEscape(tag) + "/" + url.PathEscape(a.Name)
	default:
		return "", errors.New("no download URL: set brews[].url_template, publish the release first (download_url), or provide the GitHub owner, repository and tag")
	}
	if err := checkURL(u); err != nil {
		return "", err
	}
	return u, nil
}

func artifactExt(a manifest.Artifact) string {
	if a.Format != "" {
		return "." + a.Format
	}
	return path.Ext(a.Name)
}

func checkURL(s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("download URL %q is not valid UTF-8", s)
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f || needsUnicodeEscape(r) {
			return fmt.Errorf("download URL %q contains whitespace or control characters", s)
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("download URL %q: %w", s, err)
	}
	switch u.Scheme {
	case "https":
		if u.Host == "" {
			return fmt.Errorf("download URL %q has no host", s)
		}
		if u.User != nil {
			return fmt.Errorf("download URL %q must not contain credentials", s)
		}
	case "file":
		if os.Getenv(AllowFileURLsEnv) != "1" {
			return fmt.Errorf("download URL %q: file:// URLs are allowed only with %s=1", s, AllowFileURLsEnv)
		}
		if u.Path == "" || !strings.HasPrefix(u.Path, "/") {
			return fmt.Errorf("download URL %q must be an absolute file:// URL", s)
		}
	default:
		return fmt.Errorf("download URL %q must use https", s)
	}
	return nil
}

var (
	versionRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+~-]*(_[0-9A-Za-z.+~-]+)*$`)
	revSuffix = regexp.MustCompile(`_[0-9]+$`)
)

// resolveVersion returns the formula version and whether it must be written.
// Homebrew scans GitHub release download URLs for the tag; when every URL's
// tag is the version (with or without a leading v), an explicit version is
// redundant and `brew audit` reports it, so it is omitted. Other URLs (custom
// hosts, file://) are scanned unreliably, so the version is written.
func resolveVersion(b *config.Brew, vars tmpl.Vars, plats []platform) (string, bool, error) {
	v := vars.Version
	if b.Version != "" {
		r, err := tmpl.Render(b.Version, vars)
		if err != nil {
			return "", false, fmt.Errorf("version: %w", err)
		}
		v = strings.TrimSpace(r)
	}
	switch {
	case v == "":
		return "", false, errors.New("version is empty")
	case len(v) > 1 && v[0] == 'v' && v[1] >= '0' && v[1] <= '9':
		return "", false, fmt.Errorf("version %q must not start with \"v\" (Homebrew rejects it)", v)
	case !versionRe.MatchString(v):
		return "", false, fmt.Errorf("version %q contains characters Homebrew does not accept", v)
	case revSuffix.MatchString(v):
		return "", false, fmt.Errorf("version %q must not end with _<number> (Homebrew reserves it for revisions)", v)
	}
	for _, p := range plats {
		t, ok := githubReleaseTag(p.url)
		if !ok || (t != v && t != "v"+v) {
			return v, true, nil
		}
	}
	return v, false, nil
}

// githubReleaseTag returns the tag of a github.com release download URL.
func githubReleaseTag(s string) (string, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if len(parts) != 6 || parts[2] != "releases" || parts[3] != "download" {
		return "", false
	}
	t, err := url.PathUnescape(parts[4])
	if err != nil {
		return "", false
	}
	return t, true
}

// outputPath resolves brews[].output, defaulting to
// <dist>/homebrew/<directory>/<name>.rb.
func outputPath(c *config.Config, b *config.Brew, vars tmpl.Vars) (string, error) {
	var p string
	if b.Output != "" {
		r, err := tmpl.Render(b.Output, vars)
		if err != nil {
			return "", fmt.Errorf("output: %w", err)
		}
		p = r
	} else {
		dir, err := tmpl.Render(b.Directory, vars)
		if err != nil {
			return "", fmt.Errorf("directory: %w", err)
		}
		dist := c.Dist
		if dist == "" {
			dist = config.DefaultDist
		}
		p = pathutil.Join(pathutil.Join(dist, "homebrew"), pathutil.Join(dir, b.Name+".rb"))
	}
	if err := pathutil.ValidateRelative(p); err != nil {
		return "", fmt.Errorf("output path %q: %w", p, err)
	}
	if path.Ext(p) != ".rb" {
		return "", fmt.Errorf("output path %q must end in .rb", p)
	}
	return p, nil
}

// installCode returns brews[].install (rendered) or the default, followed by
// extra_install, as the body of the single `def install`. Like GoReleaser,
// which writes one install method per archive, every archive gets the code
// rendered for it: code that renders the same everywhere is written once,
// and code that differs is wrapped in `if OS.mac? && Hardware::CPU.arm?`
// style conditions that select the archives it was rendered for.
func installCode(b *config.Brew, vars tmpl.Vars, archives []manifest.Artifact) (string, error) {
	var parts []string
	if b.Install != "" {
		s, err := renderPerArchive("install", b.Install, vars, archives)
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	} else {
		s, err := defaultInstall(archives)
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	if b.ExtraInstall != "" {
		s, err := renderPerArchive("extra_install", b.ExtraInstall, vars, archives)
		if err != nil {
			return "", err
		}
		parts = append(parts, s)
	}
	if s := completionCode(b); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n"), nil
}

// renderPerArchive renders src for every archive. When the results differ,
// each distinct result is wrapped in a condition selecting the archives it
// belongs to, in archive order.
func renderPerArchive(field, src string, vars tmpl.Vars, archives []manifest.Artifact) (string, error) {
	var order []string
	groups := map[string][]manifest.Artifact{}
	for _, a := range archives {
		av := vars.WithTarget(a.OS, a.Arch, a.Arm)
		av.ArtifactName, av.ArtifactPath, av.ArtifactExt = a.Name, a.Path, artifactExt(a)
		s, err := renderCode(field, src, av)
		if err != nil {
			return "", err
		}
		s = strings.Join(normalizeText(s), "\n")
		if _, ok := groups[s]; !ok {
			order = append(order, s)
		}
		groups[s] = append(groups[s], a)
	}
	if len(order) == 1 {
		return order[0], nil
	}
	var out []string
	for _, code := range order {
		if strings.TrimSpace(code) == "" {
			continue
		}
		for _, cond := range platformConds(groups[code], archives) {
			out = append(out, "if "+cond)
			for _, l := range strings.Split(code, "\n") {
				if l != "" {
					l = "  " + l
				}
				out = append(out, l)
			}
			out = append(out, "end")
		}
	}
	return strings.Join(out, "\n"), nil
}

// defaultInstall installs every executable of every archive, as GoReleaser
// does, with paths relative to the directory Homebrew runs install in. An
// executable missing from some archives is installed under a condition
// selecting the archives that have it.
func defaultInstall(archives []manifest.Artifact) (string, error) {
	in := map[string][]manifest.Artifact{}
	var names []string
	for _, a := range archives {
		if len(a.Executables) == 0 {
			return "", fmt.Errorf("archive %s lists no executables in the manifest; set brews[].install", a.Name)
		}
		t := newArchiveTree(a)
		seen := map[string]bool{}
		for _, e := range a.Executables {
			v := t.visible(e)
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			if _, ok := in[v]; !ok {
				names = append(names, v)
			}
			in[v] = append(in[v], a)
		}
	}
	if len(names) == 0 {
		return "", errors.New("no executable is visible in the archives; set brews[].install")
	}
	sort.Slice(names, func(i, j int) bool {
		ci, cj := len(in[names[i]]) == len(archives), len(in[names[j]]) == len(archives)
		if ci != cj {
			return ci // unconditional lines first
		}
		return names[i] < names[j]
	})
	var lines []string
	for _, e := range names {
		for _, cond := range platformConds(in[e], archives) {
			l := "bin.install " + mustQuote(e)
			if cond != "" {
				l += " if " + cond
			}
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n"), nil
}

// platformConds returns Ruby conditions that together select exactly the
// archives in sel among all: [""] when sel is all of them, one CPU or OS
// predicate when that suffices, else one OS && CPU conjunction per archive.
// An OS with a single archive is selected by OS alone, since that archive
// may also serve the other CPU (Rosetta 2).
func platformConds(sel, all []manifest.Artifact) []string {
	if len(sel) == len(all) {
		return []string{""}
	}
	has := map[string]bool{}
	for _, a := range sel {
		has[a.Name] = true
	}
	exactly := func(pred func(manifest.Artifact) bool) bool {
		for _, a := range all {
			if pred(a) != has[a.Name] {
				return false
			}
		}
		return true
	}
	osCount := map[string]int{}
	for _, a := range all {
		osCount[a.OS]++
	}
	for _, arch := range []string{"arm64", "amd64"} {
		if exactly(func(a manifest.Artifact) bool { return a.Arch == arch && osCount[a.OS] > 1 }) {
			return []string{cpuPred(arch)}
		}
	}
	var out []string
	for _, goos := range []string{"darwin", "linux"} {
		var n int
		for _, a := range sel {
			if a.OS == goos {
				n++
			}
		}
		switch n {
		case 0:
		case osCount[goos]:
			out = append(out, osPred(goos))
		default:
			for _, a := range sel {
				if a.OS == goos {
					out = append(out, osPred(goos)+" && "+cpuPred(a.Arch))
				}
			}
		}
	}
	return out
}

func osPred(goos string) string {
	if goos == "darwin" {
		return "OS.mac?"
	}
	return "OS.linux?"
}

func cpuPred(arch string) string {
	if arch == "arm64" {
		return "Hardware::CPU.arm?"
	}
	return "Hardware::CPU.intel?"
}

// defaultTest asserts that every installed bin entry is executable. It does
// not run the program: not every CLI supports --version.
func defaultTest(installBody string) string {
	var names []string
	seen := map[string]bool{}
	for _, c := range analyzeInstall(installBody) {
		if c.receiver != "bin" || c.reason != "" {
			continue
		}
		for _, s := range c.srcs {
			n := path.Base(s)
			if d, ok := c.dests[s]; ok {
				n = d
			}
			if !seen[n] && pathutil.ValidateRelative(n) == nil && !strings.Contains(n, "/") {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	lines := make([]string, len(names))
	for i, n := range names {
		lines[i] = "assert_predicate bin/" + mustQuote(n) + ", :executable?"
	}
	return strings.Join(lines, "\n")
}
