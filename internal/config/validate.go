package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/magicdrive/hephaestus/internal/glob"
	"github.com/magicdrive/hephaestus/internal/pathutil"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// Defaults applied by ApplyDefaults.
const (
	DefaultDist         = "dist"
	DefaultArchiveName  = "{{ .Project }}_{{ .Version }}_{{ .OS }}_{{ .Arch }}{{ with .Arm }}v{{ . }}{{ end }}"
	DefaultChecksumName = "{{ .Project }}_{{ .Version }}_checksums.txt"
	DefaultFormat       = "tar.gz"
	SymlinksFollow      = "follow"
	SymlinksReject      = "reject"
)

var (
	identRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_]*$`)
	idRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	projectRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	armRe     = regexp.MustCompile(`^[0-9]*$`)
	envKeyRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Warnings collects non-fatal findings (deprecated GoReleaser names and
// accepted-but-ignored fields). Callers must surface them to the user.
func (c *Config) Warnings() []string { return c.warnings }

// ApplyDefaults fills unset fields. It is idempotent.
func (c *Config) ApplyDefaults() {
	if c.Version == 0 {
		c.Version = CurrentVersion
	}
	if c.Dist == "" {
		c.Dist = DefaultDist
	}
	if c.Manifest == "" {
		c.Manifest = c.Dist + "/hephaestus-manifest.json"
	}
	if c.Limits.MaxFileSize == 0 {
		c.Limits.MaxFileSize = DefaultMaxFileSize
	}
	if c.Limits.MaxTotalSize == 0 {
		c.Limits.MaxTotalSize = DefaultMaxTotalSize
	}
	if c.Limits.MaxFiles == 0 {
		c.Limits.MaxFiles = DefaultMaxFiles
	}
	for i := range c.Builds {
		if c.Builds[i].ID == "" && len(c.Builds) == 1 {
			c.Builds[i].ID = "default"
		}
	}
	for i := range c.Archives {
		a := &c.Archives[i]
		if a.ID == "" && len(c.Archives) == 1 {
			a.ID = "default"
		}
		if a.Name == "" {
			a.Name = DefaultArchiveName
		}
		if a.Format == "" {
			a.Format = DefaultFormat
		}
		if a.Symlinks == "" {
			a.Symlinks = SymlinksFollow
		}
	}
	if c.Checksum.Name == "" {
		c.Checksum.Name = DefaultChecksumName
	}
	if g := c.Release.GitHub; g != nil {
		if g.Prerelease == "" {
			g.Prerelease = "auto"
		}
		if len(g.Include) == 0 {
			g.Include = []string{"archive", "checksum"}
		}
	}
	if c.Homebrew != nil {
		c.Brews = append(c.Brews, *c.Homebrew)
		c.Homebrew = nil
	}
	for i := range c.Brews {
		b := &c.Brews[i]
		if b.Name == "" {
			b.Name = c.Project.Name
		}
		if b.Tap != nil && b.Repository == (BrewRepository{}) {
			b.Repository = *b.Tap
			b.Tap = nil
			c.warnings = append(c.warnings, fmt.Sprintf("brews[%d].tap is deprecated; use repository", i))
		}
		if b.Folder != "" && b.Directory == "" {
			b.Directory = b.Folder
			b.Folder = ""
			c.warnings = append(c.warnings, fmt.Sprintf("brews[%d].folder is deprecated; use directory", i))
		}
		if b.Homepage == "" {
			b.Homepage = c.Project.Homepage
		}
		if b.Description == "" {
			b.Description = c.Project.Description
		}
		if b.License == "" {
			b.License = c.Project.License
		}
	}
}

// Validate checks the whole configuration and returns every problem found.
func (c *Config) Validate() error {
	var errs []error
	add := func(path, format string, args ...any) {
		errs = append(errs, c.errAt(path, format, args...))
	}
	checkTmpl := func(path, s string) {
		if err := tmpl.Check(s); err != nil {
			add(path, "%v", err)
		}
	}

	if c.Version != CurrentVersion {
		add("version", "unsupported configuration version %d (this build supports %d)", c.Version, CurrentVersion)
	}
	if c.Project.Name == "" {
		add("project.name", "is required")
	} else if !projectRe.MatchString(c.Project.Name) {
		add("project.name", "%q must match %s", c.Project.Name, projectRe)
	}
	for _, p := range []struct{ path, v string }{{"dist", c.Dist}, {"manifest", c.Manifest}} {
		if err := pathutil.ValidateRelative(p.v); err != nil {
			add(p.path, "%q: %v", p.v, err)
		}
	}
	for i, e := range c.Env {
		k, _, ok := strings.Cut(string(e), "=")
		if !ok || !envKeyRe.MatchString(k) {
			add(fmt.Sprintf("env[%d]", i), "must be KEY=VALUE")
		}
	}
	if c.Limits.MaxFiles < 0 || c.Limits.MaxFileSize < 0 || c.Limits.MaxTotalSize < 0 {
		add("limits", "limits must be positive")
	}

	buildIDs := map[string]bool{}
	for i := range c.Builds {
		b := &c.Builds[i]
		p := fmt.Sprintf("builds[%d]", i)
		switch {
		case b.ID == "":
			add(p+".id", "is required when there is more than one build")
		case !idRe.MatchString(b.ID):
			add(p+".id", "%q must match %s", b.ID, idRe)
		case buildIDs[b.ID]:
			add(p+".id", "duplicate id %q", b.ID)
		}
		buildIDs[b.ID] = true

		n := 0
		if !b.Command.IsZero() {
			n++
		}
		if b.GoReleaser != nil {
			n++
		}
		if b.Existing != nil {
			n++
		}
		if n != 1 {
			add(p, "exactly one of command, goreleaser or existing is required")
			continue
		}
		switch b.Provider() {
		case ProviderCommand:
			if b.Output == "" {
				add(p+".output", "is required for a command build")
			}
			checkTmpl(p+".output", b.Output)
			checkTmpl(p+".command", b.Command.Shell)
			for j, a := range b.Command.Args {
				checkTmpl(fmt.Sprintf("%s.command[%d]", p, j), a)
			}
			if len(b.Command.Args) > 0 && b.Command.Args[0] == "" {
				add(p+".command", "program name is empty")
			}
			for k, v := range b.Env {
				if !envKeyRe.MatchString(k) {
					add(p+".env."+k, "invalid environment variable name")
				}
				checkTmpl(p+".env."+k, v)
			}
			if b.Dir != "" {
				if err := pathutil.ValidateRelative(b.Dir); err != nil {
					add(p+".dir", "%v", err)
				}
			}
		case ProviderExisting:
			if b.Existing.Path == "" {
				add(p+".existing.path", "is required")
			}
			checkTmpl(p+".existing.path", b.Existing.Path)
		case ProviderGoReleaser:
			if b.Targets != nil || b.Matrix != nil {
				add(p, "targets come from GoReleaser's artifacts.json; remove targets/matrix")
			}
			if b.GoReleaser.Artifacts == "" {
				b.GoReleaser.Artifacts = "dist/artifacts.json"
			}
			if err := pathutil.ValidateRelative(b.GoReleaser.Artifacts); err != nil {
				add(p+".goreleaser.artifacts", "%v", err)
			}
		}
		if b.Provider() != ProviderGoReleaser {
			if len(b.Targets) == 0 && b.Matrix == nil {
				add(p, "declare targets or matrix")
			}
			c.validateTargets(p, b.Targets, b.Matrix, add)
		}
		if b.Binary != "" && (strings.Contains(b.Binary, "/") || pathutil.ValidateRelative(b.Binary) != nil) {
			add(p+".binary", "%q must be a plain file name", b.Binary)
		}
	}

	archiveIDs := map[string]bool{}
	for i := range c.Archives {
		c.validateArchive(i, buildIDs, archiveIDs, add, checkTmpl)
	}

	if c.Checksum.Algorithm != "" && c.Checksum.Algorithm != "sha256" {
		add("checksum.algorithm", "only sha256 is supported")
	}
	checkTmpl("checksum.name", c.Checksum.Name)

	if g := c.Release.GitHub; g != nil {
		switch g.Prerelease {
		case "auto", "true", "false":
		default:
			add("release.github.prerelease", "must be auto, true or false")
		}
		switch g.MakeLatest {
		case "", "true", "false", "legacy":
		default:
			add("release.github.make_latest", "must be true, false or legacy")
		}
		for j, k := range g.Include {
			if k != "archive" && k != "checksum" && k != "binary" {
				add(fmt.Sprintf("release.github.include[%d]", j), "must be archive, checksum or binary")
			}
		}
		if g.Notes != "" && g.NotesFile != "" {
			add("release.github", "notes and notes_file are mutually exclusive")
		}
		if g.NotesFile != "" {
			if err := pathutil.ValidateRelative(g.NotesFile); err != nil {
				add("release.github.notes_file", "%v", err)
			}
		}
		checkTmpl("release.github.name_template", g.Name)
		checkTmpl("release.github.notes", g.Notes)
		for _, f := range []struct{ path, v string }{{"release.github.owner", g.Owner}, {"release.github.name", g.Repo}} {
			if f.v != "" && !idRe.MatchString(f.v) {
				add(f.path, "%q is not a valid GitHub name", f.v)
			}
		}
	}

	brewNames := map[string]bool{}
	for i := range c.Brews {
		c.validateBrew(i, archiveIDs, brewNames, add, checkTmpl)
	}
	return errors.Join(errs...)
}

func (c *Config) validateTargets(p string, targets []Target, m *Matrix, add func(string, string, ...any)) {
	check := func(path string, t Target) {
		if !identRe.MatchString(t.OS) {
			add(path+".os", "%q must match %s", t.OS, identRe)
		}
		if !identRe.MatchString(t.Arch) {
			add(path+".arch", "%q must match %s", t.Arch, identRe)
		}
		if !armRe.MatchString(t.Arm) {
			add(path+".arm", "%q must be a number", t.Arm)
		}
	}
	for j, t := range targets {
		check(fmt.Sprintf("%s.targets[%d]", p, j), t)
	}
	if m != nil {
		if len(m.OS) == 0 || len(m.Arch) == 0 {
			add(p+".matrix", "os and arch must both be non-empty")
		}
		for _, os := range m.OS {
			for _, arch := range m.Arch {
				check(p+".matrix", Target{OS: os, Arch: arch})
			}
		}
	}
	seen := map[string]bool{}
	for _, t := range ExpandTargets(targets, m) {
		if seen[t.String()] {
			add(p, "target %s is declared twice", t)
		}
		seen[t.String()] = true
	}
}

// ExpandTargets returns explicit targets followed by the matrix product minus
// exclusions, in declaration order.
func ExpandTargets(targets []Target, m *Matrix) []Target {
	out := append([]Target(nil), targets...)
	if m == nil {
		return out
	}
	for _, os := range m.OS {
		for _, arch := range m.Arch {
			t := Target{OS: os, Arch: arch}
			excluded := false
			for _, x := range m.Exclude {
				if (x.OS == "" || x.OS == os) && (x.Arch == "" || x.Arch == arch) {
					excluded = true
				}
			}
			if !excluded {
				out = append(out, t)
			}
		}
	}
	return out
}

func validFormat(f string) bool { return f == "tar.gz" || f == "zip" }

func (c *Config) validateArchive(i int, buildIDs, ids map[string]bool, add func(string, string, ...any), checkTmpl func(string, string)) {
	a := &c.Archives[i]
	p := fmt.Sprintf("archives[%d]", i)
	switch {
	case a.ID == "":
		add(p+".id", "is required when there is more than one archive")
	case !idRe.MatchString(a.ID):
		add(p+".id", "%q must match %s", a.ID, idRe)
	case ids[a.ID]:
		add(p+".id", "duplicate id %q", a.ID)
	}
	ids[a.ID] = true
	for j, b := range a.Builds {
		if !buildIDs[b] {
			add(fmt.Sprintf("%s.builds[%d]", p, j), "unknown build id %q", b)
		}
	}
	c.validateTargets(p, a.Targets, nil, add)
	checkTmpl(p+".name", a.Name)
	checkTmpl(p+".mtime", a.MTime)
	if a.WrapInDirectory.Set && !a.WrapInDirectory.Bool && a.WrapInDirectory.Value == "" {
		// explicit false
	} else if a.WrapInDirectory.Value != "" {
		checkTmpl(p+".wrap_in_directory", a.WrapInDirectory.Value)
	}
	if !validFormat(a.Format) {
		add(p+".format", "%q is not supported (use tar.gz or zip)", a.Format)
	}
	for j, o := range a.FormatOverrides {
		op := fmt.Sprintf("%s.format_overrides[%d]", p, j)
		if !identRe.MatchString(o.OS) {
			add(op+".os", "%q must match %s", o.OS, identRe)
		}
		if !validFormat(o.Format) {
			add(op+".format", "%q is not supported (use tar.gz or zip)", o.Format)
		}
	}
	if a.Symlinks != SymlinksFollow && a.Symlinks != SymlinksReject {
		add(p+".symlinks", "must be %q or %q", SymlinksFollow, SymlinksReject)
	}
	if a.FilesDeclared() && len(a.Files) == 0 {
		add(p+".files", "is empty; omit it to package the target's binaries, or list files")
	}
	for j := range a.Files {
		validateFileSpec(fmt.Sprintf("%s.files[%d]", p, j), &a.Files[j], add, checkTmpl)
	}
}

func validateFileSpec(p string, f *FileSpec, add func(string, string, ...any), checkTmpl func(string, string)) {
	if f.Src == "" {
		add(p+".src", "is required")
		return
	}
	checkTmpl(p+".src", f.Src)
	checkTmpl(p+".dst", f.Dst)
	// Static checks when no template is involved; templated values are
	// checked again after rendering.
	if !strings.Contains(f.Src, "{{") {
		if err := ValidateSrc(f.Src); err != nil {
			add(p+".src", "%v", err)
		}
	}
	if !strings.Contains(f.Dst, "{{") {
		if err := ValidateDst(f.Dst); err != nil {
			add(p+".dst", "%v", err)
		}
	}
	for k, x := range f.Exclude {
		if _, err := glob.Compile(x, glob.Options{Dot: true}); err != nil {
			add(fmt.Sprintf("%s.exclude[%d]", p, k), "%v", err)
		}
	}
	if f.Info != nil {
		if f.Info.Mode != "" {
			if f.Mode != "" && f.Mode != f.Info.Mode {
				add(p+".info.mode", "conflicts with mode")
			}
			if f.Mode == "" {
				f.Mode = f.Info.Mode
			}
		}
		checkTmpl(p+".info.mtime", f.Info.MTime)
		for _, o := range []struct{ k, v string }{{"owner", f.Info.Owner}, {"group", f.Info.Group}} {
			if o.v != "" && !regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`).MatchString(o.v) {
				add(p+".info."+o.k, "%q is not a valid user or group name", o.v)
			}
		}
	}
	if f.Mode != "" {
		if _, err := ParseMode(f.Mode); err != nil {
			add(p+".mode", "%v", err)
		}
	}
	if f.When != nil {
		for _, os := range f.When.OS {
			if !identRe.MatchString(os) {
				add(p+".when.os", "%q must match %s", os, identRe)
			}
		}
		for _, arch := range f.When.Arch {
			if !identRe.MatchString(arch) {
				add(p+".when.arch", "%q must match %s", arch, identRe)
			}
		}
	}
}

// ValidateSrc checks a rendered src value: a project-relative path or glob
// that cannot leave the project root.
func ValidateSrc(src string) error {
	if glob.HasMeta(src) {
		_, err := glob.Compile(src, glob.Options{})
		return err
	}
	return pathutil.ValidateRelative(src)
}

// ValidateDst checks a rendered dst value: empty (archive root), a directory
// ending in "/", or a file path.
func ValidateDst(dst string) error {
	if dst == "" {
		return nil
	}
	if err := pathutil.ValidateRelative(strings.TrimSuffix(dst, "/")); err != nil {
		return err
	}
	return nil
}

// ParseMode parses an octal permission string and enforces the safety
// policy: no setuid, setgid or sticky bits, not group- or world-writable,
// and readable by the owner.
func ParseMode(s string) (uint32, error) {
	if !regexp.MustCompile(`^0?[0-7]{3}$`).MatchString(s) {
		return 0, fmt.Errorf("mode %q must be an octal string such as \"0644\" or \"0755\"", s)
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, err
	}
	m := uint32(v)
	switch {
	case m&0o022 != 0:
		return 0, fmt.Errorf("mode %q is group- or world-writable", s)
	case m&0o400 == 0:
		return 0, fmt.Errorf("mode %q is not readable by its owner", s)
	}
	return m, nil
}

var formulaNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9+_.-]*(@[0-9][0-9.]*)?$`)

// FormulaNameRe is the accepted Homebrew formula name syntax.
func FormulaNameRe() *regexp.Regexp { return formulaNameRe }

func (c *Config) validateBrew(i int, archiveIDs, names map[string]bool, add func(string, string, ...any), checkTmpl func(string, string)) {
	b := &c.Brews[i]
	p := fmt.Sprintf("brews[%d]", i)
	if !formulaNameRe.MatchString(b.Name) {
		add(p+".name", "%q is not a valid Homebrew formula name (lowercase letters, digits, +_.- and an optional @version)", b.Name)
	}
	if names[b.Name] {
		add(p+".name", "duplicate formula %q", b.Name)
	}
	names[b.Name] = true
	for j, id := range b.IDs {
		if !archiveIDs[id] {
			add(fmt.Sprintf("%s.ids[%d]", p, j), "unknown archive id %q", id)
		}
	}
	for _, f := range []struct{ path, v string }{
		{".url_template", b.URLTemplate}, {".commit_msg_template", b.CommitMsg}, {".caveats", b.Caveats},
		{".description", b.Description}, {".homepage", b.Homepage}, {".install", b.Install},
		{".extra_install", b.ExtraInstall}, {".post_install", b.PostInstall}, {".test", b.Test},
		{".custom_block", b.CustomBlock}, {".service", b.Service}, {".version", b.Version},
		{".output", b.Output}, {".directory", b.Directory}, {".repository.owner", b.Repository.Owner},
		{".repository.name", b.Repository.Name}, {".repository.branch", b.Repository.Branch},
		{".repository.token", b.Repository.Token},
	} {
		checkTmpl(p+f.path, f.v)
	}
	if b.Plist != "" {
		add(p+".plist", "is not supported: Homebrew removed plist blocks; use service")
	}
	if b.Repository.Git != nil {
		add(p+".repository.git", "is not supported: Hephaestus publishes through the GitHub API")
	}
	if b.CommitAuthor.Signing != nil {
		add(p+".commit_author.signing", "is not supported: commits are created through the GitHub API (GitHub signs API commits made with a GitHub App token)")
	}
	switch b.Repository.TokenType {
	case "", "github":
	default:
		add(p+".repository.token_type", "%q is not supported; only github", b.Repository.TokenType)
	}
	switch b.SkipUpload {
	case "", "true", "false", "auto":
	default:
		add(p+".skip_upload", "must be true, false or auto")
	}
	if b.DownloadStrategy != "" && !regexp.MustCompile(`^[A-Z][A-Za-z0-9_]*(::[A-Z][A-Za-z0-9_]*)*$|^:[a-z_]+$`).MatchString(b.DownloadStrategy) {
		add(p+".download_strategy", "%q must be a Ruby class name or a :symbol", b.DownloadStrategy)
	}
	if b.CustomRequire != "" && !regexp.MustCompile(`^[A-Za-z0-9_./-]+$`).MatchString(b.CustomRequire) {
		add(p+".custom_require", "%q contains characters not allowed in a require path", b.CustomRequire)
	}
	for j, d := range b.Dependencies {
		dp := fmt.Sprintf("%s.dependencies[%d]", p, j)
		if !formulaDepRe.MatchString(d.Name) {
			add(dp+".name", "%q is not a valid formula name", d.Name)
		}
		switch d.Type {
		case "", "optional", "build", "test", "recommended":
		default:
			add(dp+".type", "must be optional, build, test or recommended")
		}
		switch d.OS {
		case "", "mac", "linux":
		default:
			add(dp+".os", "must be mac or linux")
		}
		if d.Version != "" && !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(d.Version) {
			add(dp+".version", "%q is not a valid version", d.Version)
		}
	}
	for j, x := range b.Conflicts {
		if !formulaDepRe.MatchString(x) {
			add(fmt.Sprintf("%s.conflicts[%d]", p, j), "%q is not a valid formula name", x)
		}
	}
	if pr := b.Repository.PullRequest; pr != nil && pr.CheckBoxes {
		c.warnings = append(c.warnings, p+".repository.pull_request.check_boxes is accepted but has no effect")
	}
	if len(b.URLHeaders) > 0 {
		for j, h := range b.URLHeaders {
			if strings.ContainsAny(h, "\r\n") {
				add(fmt.Sprintf("%s.url_headers[%d]", p, j), "must be a single line")
			}
			checkTmpl(fmt.Sprintf("%s.url_headers[%d]", p, j), h)
		}
	}
}

// formulaDepRe allows tap-qualified names such as "owner/tap/formula".
var formulaDepRe = regexp.MustCompile(`^([a-z0-9][a-z0-9_-]*/[a-z0-9][a-z0-9_-]*/)?[a-z0-9][a-z0-9+_.-]*(@[0-9][0-9.]*)?$`)
