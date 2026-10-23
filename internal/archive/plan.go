// Package archive implements the archives stage: it resolves declared file
// lists into an exact entry plan, then writes byte-reproducible tar.gz and
// zip archives from that plan.
//
// Path semantics (normative; mirrored in docs/archives.md):
//
//   - src is relative to the project root (the directory holding the
//     configuration file). Absolute paths and ".." segments are rejected.
//   - A src without glob metacharacters is a literal file. Without dst it is
//     placed at its src path; with a dst ending in "/" it is placed at
//     dst + base name; with any other dst it is placed exactly at dst
//     (a rename).
//   - A src with metacharacters is a glob. Its base is the leading run of
//     literal directories ("docs" for "docs/**/*.md"). Without dst every match
//     keeps its project-relative path; with dst, the base is replaced by dst,
//     which is always a directory. strip_parent places matches at
//     dst + base name instead.
//   - Wildcards do not match dot files unless the pattern names them, and a
//     glob whose base lies outside the dist directory never matches files in
//     it, so previous outputs are never re-packaged.
//   - Two entries resolving to the same archive path, or paths that differ
//     only in case, or a file that is also needed as a directory, are
//     errors.
//   - Symbolic links are followed (content stored as a regular file) only
//     when the resolved target stays inside the project root; with
//     symlinks: reject any link is an error, including a linked parent
//     directory of a literal src and a linked glob base. Wildcards never
//     descend into linked directories. Sensitive-file rules apply to the
//     path as written and to the path after resolving every link.
package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/glob"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/pathutil"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// DefaultMTime is used when neither the configuration nor SOURCE_DATE_EPOCH
// sets a timestamp. It is the earliest time a zip file can represent.
var DefaultMTime = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// Entry types.
const (
	TypeFile = "file"
	TypeDir  = "dir"
)

// Entry is one planned archive member.
type Entry struct {
	Path   string // archive path, slash-separated, no trailing slash
	Type   string // TypeFile or TypeDir
	Mode   uint32 // permission bits only
	Source string // project-relative source (files only)
	Size   int64
	SHA256 string
	MTime  time.Time
	Owner  string
	Group  string
	// spec records which configuration entry produced the entry.
	spec string
}

// Plan is everything needed to write one archive, decided before any byte
// is written.
type Plan struct {
	ArchiveID string
	Target    config.Target
	Name      string // file name including extension
	Path      string // project-relative output path
	Format    string
	Entries   []Entry // sorted by Path; directories included
	// Skipped lists optional entries that matched nothing.
	Skipped []string
}

// Input is what the archives stage needs.
type Input struct {
	Config   *config.Config
	Manifest *manifest.Manifest
	Vars     tmpl.Vars // project-level variables, no target set
	// IDs restricts the stage to these archive ids (all when empty).
	IDs []string
	// SourceDateEpoch is the value of $SOURCE_DATE_EPOCH, if any.
	SourceDateEpoch string
}

var assetNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// BuildPlans resolves every configured archive for every target.
func BuildPlans(in Input) ([]*Plan, error) {
	cfg := in.Config
	root, err := os.OpenRoot(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("open project root: %w", err)
	}
	defer root.Close()

	var plans []*Plan
	var errs []error
	names := map[string]string{}
	for i := range cfg.Archives {
		a := &cfg.Archives[i]
		if len(in.IDs) > 0 && !contains(in.IDs, a.ID) {
			continue
		}
		where := fmt.Sprintf("archives[%d] (id %s)", i, a.ID)
		targets, err := archiveTargets(a, in.Manifest)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
			continue
		}
		for _, t := range targets {
			p, err := planOne(root, cfg, a, t, in)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s, target %s: %w", where, t, err))
				continue
			}
			if prev, dup := names[p.Name]; dup {
				errs = append(errs, fmt.Errorf("%s, target %s: archive name %q is also produced by %s; include {{ .OS }} and {{ .Arch }} in name", where, t, p.Name, prev))
				continue
			}
			names[p.Name] = fmt.Sprintf("%s target %s", where, t)
			plans = append(plans, p)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return plans, nil
}

// archiveTargets returns explicit targets, else the distinct targets of the
// selected build binaries in the manifest.
func archiveTargets(a *config.Archive, m *manifest.Manifest) ([]config.Target, error) {
	if len(a.Targets) > 0 {
		return a.Targets, nil
	}
	seen := map[string]bool{}
	var out []config.Target
	if m != nil {
		for _, b := range m.Filter(manifest.KindBinary, a.Builds...) {
			t := config.Target{OS: b.OS, Arch: b.Arch, Arm: b.Arm}
			if !seen[t.String()] {
				seen[t.String()] = true
				out = append(out, t)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no targets: run a build stage or import artifacts first, or declare targets on the archive")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

func planOne(root *os.Root, cfg *config.Config, a *config.Archive, t config.Target, in Input) (*Plan, error) {
	vars := in.Vars.WithTarget(t.OS, t.Arch, t.Arm)
	format := a.Format
	for _, o := range a.FormatOverrides {
		if o.OS == t.OS {
			format = o.Format
		}
	}
	base, err := tmpl.Render(a.Name, vars)
	if err != nil {
		return nil, fmt.Errorf("name: %w", err)
	}
	name := base + "." + format
	if !assetNameRe.MatchString(name) || len(name) > 255 {
		return nil, fmt.Errorf("archive name %q may only contain letters, digits, '.', '_' and '-' (GitHub would rename other characters)", name)
	}
	mtime, err := archiveMTime(a.MTime, vars, in.SourceDateEpoch)
	if err != nil {
		return nil, err
	}
	if format == "zip" && mtime.Before(DefaultMTime) {
		return nil, fmt.Errorf("mtime %s is before 1980-01-01, which zip cannot store", mtime.Format(time.RFC3339))
	}
	wrap := ""
	if a.WrapInDirectory.Bool {
		wrap = base
		if a.WrapInDirectory.Value != "" {
			if wrap, err = tmpl.Render(a.WrapInDirectory.Value, vars); err != nil {
				return nil, fmt.Errorf("wrap_in_directory: %w", err)
			}
		}
		if err := pathutil.ValidateRelative(wrap); err != nil || strings.Contains(wrap, "/") {
			return nil, fmt.Errorf("wrap_in_directory %q must be a single directory name", wrap)
		}
	}

	r := &resolver{root: root, cfg: cfg, archive: a, target: t, vars: vars, mtime: mtime, byPath: map[string]*Entry{}, folded: map[string]string{}}
	if a.FilesDeclared() {
		for i := range a.Files {
			if err := r.addSpec(i, &a.Files[i]); err != nil {
				return nil, fmt.Errorf("files[%d]: %w", i, err)
			}
		}
	} else if err := r.addBinaries(in.Manifest); err != nil {
		return nil, err
	}
	entries, err := r.finish(wrap)
	if err != nil {
		return nil, err
	}
	return &Plan{
		ArchiveID: a.ID, Target: t, Name: name, Format: format,
		Path: pathutil.Join(cfg.Dist, name), Entries: entries, Skipped: r.skipped,
	}, nil
}

func archiveMTime(spec string, vars tmpl.Vars, sde string) (time.Time, error) {
	if spec != "" {
		s, err := tmpl.Render(spec, vars)
		if err != nil {
			return time.Time{}, fmt.Errorf("mtime: %w", err)
		}
		return parseTime(s)
	}
	if sde != "" {
		t, err := parseTime(sde)
		if err != nil {
			return time.Time{}, fmt.Errorf("SOURCE_DATE_EPOCH: %w", err)
		}
		return t, nil
	}
	return DefaultMTime, nil
}

func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < 0 || n > 1<<33 {
			return time.Time{}, fmt.Errorf("timestamp %q is out of range", s)
		}
		return time.Unix(n, 0).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp %q is neither Unix seconds nor RFC 3339", s)
	}
	if t.Before(time.Unix(0, 0)) {
		return time.Time{}, fmt.Errorf("timestamp %q is before 1970", s)
	}
	return t.UTC().Truncate(time.Second), nil
}

type resolver struct {
	root    *os.Root
	cfg     *config.Config
	archive *config.Archive
	target  config.Target
	vars    tmpl.Vars
	mtime   time.Time
	byPath  map[string]*Entry
	folded  map[string]string
	skipped []string
	total   int64
}

// addBinaries places the target's binaries at the archive root; used when
// files is omitted.
func (r *resolver) addBinaries(m *manifest.Manifest) error {
	if m == nil {
		return errors.New("files is omitted and no manifest is available to take binaries from")
	}
	n := 0
	for _, b := range m.Filter(manifest.KindBinary, r.archive.Builds...) {
		if b.OS != r.target.OS || b.Arch != r.target.Arch || b.Arm != r.target.Arm {
			continue
		}
		mode := uint32(0o755)
		if err := r.addFile(b.Path, pathutil.Base(b.Path), &mode, nil, "binary "+b.Name, false); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return fmt.Errorf("files is omitted and the manifest has no binaries for %s", r.target)
	}
	return nil
}

func (r *resolver) addSpec(idx int, f *config.FileSpec) error {
	if !f.When.Matches(r.target) {
		return nil
	}
	label := fmt.Sprintf("files[%d] (src %q)", idx, f.Src)
	src, err := tmpl.Render(f.Src, r.vars)
	if err != nil {
		return err
	}
	if err := config.ValidateSrc(src); err != nil {
		return fmt.Errorf("src %q: %w", src, err)
	}
	dst, err := tmpl.Render(f.Dst, r.vars)
	if err != nil {
		return err
	}
	if err := config.ValidateDst(dst); err != nil {
		return fmt.Errorf("dst %q: %w", dst, err)
	}
	var mode *uint32
	if f.Mode != "" {
		m, err := config.ParseMode(f.Mode)
		if err != nil {
			return err
		}
		mode = &m
	}
	var info *entryInfo
	if f.Info != nil {
		info = &entryInfo{owner: f.Info.Owner, group: f.Info.Group}
		if f.Info.MTime != "" {
			s, err := tmpl.Render(f.Info.MTime, r.vars)
			if err != nil {
				return err
			}
			if info.mtime, err = parseTime(s); err != nil {
				return err
			}
		}
	}
	var excludes []*glob.Pattern
	for _, x := range f.Exclude {
		xs, err := tmpl.Render(x, r.vars)
		if err != nil {
			return err
		}
		p, err := glob.Compile(xs, glob.Options{Dot: true})
		if err != nil {
			return fmt.Errorf("exclude %q: %w", xs, err)
		}
		excludes = append(excludes, p)
	}

	if !glob.HasMeta(src) {
		target := src
		switch {
		case dst == "":
		case strings.HasSuffix(dst, "/"):
			target = pathutil.Join(dst, pathutil.Base(src))
		default:
			target = dst
		}
		for _, x := range excludes {
			if x.Match(src) {
				return nil
			}
		}
		if _, err := r.root.Lstat(src); errors.Is(err, fs.ErrNotExist) {
			if f.IsRequired() {
				return fmt.Errorf("required file %q does not exist", src)
			}
			r.skipped = append(r.skipped, src)
			return nil
		}
		return r.addFile(src, target, mode, info, label, f.AllowSensitive)
	}

	pat, err := glob.Compile(src, glob.Options{})
	if err != nil {
		return err
	}
	if base := pat.Base(); base != "" && r.archive.Symlinks == config.SymlinksReject {
		// fs.WalkDir follows a linked starting directory; refuse it (and
		// any linked directory above it) before walking.
		if _, _, linked, err := resolveInRoot(r.root, base); linked {
			return fmt.Errorf("glob %q starts in %q, which is or passes through a symbolic link, and symlinks: reject is set", src, base)
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("glob %q: %w", src, err)
		}
	}
	matches, err := glob.Walk(r.root.FS(), pat, r.cfg.Limits.MaxFiles+1)
	if errors.Is(err, glob.ErrTooManyMatches) {
		return fmt.Errorf("glob %q matches more than %d files (limits.max_files)", src, r.cfg.Limits.MaxFiles)
	}
	if err != nil {
		return fmt.Errorf("glob %q: %w", src, err)
	}
	base := pat.Base()
	dist := strings.TrimSuffix(r.cfg.Dist, "/")
	n := 0
	for _, m := range matches {
		if !within(base, dist) && within(m.Path, dist) {
			continue // never re-package outputs through a broad glob
		}
		excluded := false
		for _, x := range excludes {
			if x.Match(m.Path) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		var target string
		switch {
		case f.StripParent:
			target = pathutil.Join(strings.TrimSuffix(dst, "/"), pathutil.Base(m.Path))
		case dst == "":
			target = m.Path
		default:
			rel := m.Path
			if base != "" {
				rel = strings.TrimPrefix(m.Path, base+"/")
			}
			target = pathutil.Join(strings.TrimSuffix(dst, "/"), rel)
		}
		if err := r.addFile(m.Path, target, mode, info, label, f.AllowSensitive); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		if f.IsRequired() {
			return fmt.Errorf("glob %q matched no files (set required: false if that is expected)", src)
		}
		r.skipped = append(r.skipped, src)
	}
	return nil
}

type entryInfo struct {
	owner, group string
	mtime        time.Time
}

// within reports whether p is dir or below it.
func within(p, dir string) bool {
	return dir != "" && (p == dir || strings.HasPrefix(p, dir+"/"))
}

// addFile validates the source, hashes it and records the entry.
func (r *resolver) addFile(src, target string, mode *uint32, info *entryInfo, label string, allowSensitive bool) error {
	if err := pathutil.ValidateRelative(target); err != nil {
		return fmt.Errorf("archive path %q for %s: %w", target, src, err)
	}
	// Resolve every link on the path inside the root. With symlinks: reject
	// a link anywhere on the path (a linked parent directory included) is
	// an error; with symlinks: follow the resolved path must pass the same
	// sensitive-file rules as the path as written.
	resolved, dirs, linked, rerr := resolveInRoot(r.root, src)
	if linked && r.archive.Symlinks == config.SymlinksReject {
		return fmt.Errorf("%q is or passes through a symbolic link and symlinks: reject is set", src)
	}
	if !allowSensitive {
		if why := sensitivePath(src); why != "" {
			return fmt.Errorf("refusing %q because %s; set allow_sensitive: true on the entry if this is intended", src, why)
		}
	}
	lst, err := r.root.Lstat(src)
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	if lst.Mode()&fs.ModeSymlink != 0 {
		// os.Root refuses links whose target leaves the root.
		if _, err := r.root.Stat(src); err != nil {
			return fmt.Errorf("symbolic link %q resolves outside the project root or is broken: %w", src, err)
		}
	}
	if rerr != nil {
		return fmt.Errorf("resolve %q: %w", src, rerr)
	}
	if !allowSensitive && linked {
		why := sensitivePath(resolved)
		for _, d := range dirs {
			if why == "" {
				why = sensitiveDir(d)
			}
		}
		if why != "" {
			return fmt.Errorf("refusing %q because it resolves to %q and %s; set allow_sensitive: true on the entry if this is intended", src, resolved, why)
		}
	}
	file, err := r.root.Open(src)
	if err != nil {
		return fmt.Errorf("open %q: %w", src, err)
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		return err
	}
	switch {
	case st.IsDir() && lst.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%q is a symbolic link to a directory; links to directories are never followed", src)
	case st.IsDir():
		return fmt.Errorf("%q is a directory; use %q to include its contents", src, src+"/**")
	case !st.Mode().IsRegular():
		return fmt.Errorf("%q is not a regular file (%s)", src, st.Mode().Type())
	}
	if st.Size() > int64(r.cfg.Limits.MaxFileSize) {
		return fmt.Errorf("%q is %d bytes, over limits.max_file_size (%d)", src, st.Size(), r.cfg.Limits.MaxFileSize)
	}
	r.total += st.Size()
	if r.total > int64(r.cfg.Limits.MaxTotalSize) {
		return fmt.Errorf("archive content exceeds limits.max_total_size (%d bytes)", r.cfg.Limits.MaxTotalSize)
	}
	sum, size, keyFound, err := hashFile(file, int64(r.cfg.Limits.MaxFileSize))
	if err != nil {
		return fmt.Errorf("read %q: %w", src, err)
	}
	if keyFound != "" && !allowSensitive {
		return fmt.Errorf("refusing %q because it contains %s; set allow_sensitive: true on the entry if this is intended", src, keyFound)
	}

	perm := uint32(0o644)
	if st.Mode().Perm()&0o100 != 0 {
		perm = 0o755
	}
	if mode != nil {
		perm = *mode
	}
	e := &Entry{Path: target, Type: TypeFile, Mode: perm, Source: src, Size: size, SHA256: sum, MTime: r.mtime, spec: label}
	if info != nil {
		e.Owner, e.Group = info.owner, info.group
		if !info.mtime.IsZero() {
			e.MTime = info.mtime
		}
	}
	return r.put(e)
}

func (r *resolver) put(e *Entry) error {
	if prev, dup := r.byPath[e.Path]; dup {
		if prev.Type == TypeDir && e.Type == TypeDir {
			return nil
		}
		return fmt.Errorf("archive path %q collides: %s (from %s) and %s (from %s)", e.Path, describe(prev), prev.spec, describe(e), e.spec)
	}
	key := pathutil.FoldKey(e.Path)
	if other, dup := r.folded[key]; dup {
		return fmt.Errorf("archive paths %q and %q differ only in letter case and would collide on case-insensitive file systems", other, e.Path)
	}
	if len(r.byPath) >= r.cfg.Limits.MaxFiles {
		return fmt.Errorf("archive has more than %d entries (limits.max_files)", r.cfg.Limits.MaxFiles)
	}
	r.byPath[e.Path] = e
	r.folded[key] = e.Path
	return nil
}

func describe(e *Entry) string {
	if e.Type == TypeDir {
		return "a directory"
	}
	return fmt.Sprintf("%q", e.Source)
}

// finish applies wrapping, synthesizes parent directories, detects
// file/directory conflicts and sorts.
func (r *resolver) finish(wrap string) ([]Entry, error) {
	files := make([]*Entry, 0, len(r.byPath))
	for _, e := range r.byPath {
		files = append(files, e)
	}
	r.byPath = map[string]*Entry{}
	r.folded = map[string]string{}
	for _, e := range files {
		if wrap != "" {
			e.Path = wrap + "/" + e.Path
		}
		if err := r.put(e); err != nil {
			return nil, err
		}
	}
	for _, e := range files {
		for _, dir := range pathutil.Parents(e.Path) {
			if prev, ok := r.byPath[dir]; ok && prev.Type == TypeFile {
				return nil, fmt.Errorf("archive path %q is a file (from %s) but %q needs it to be a directory", dir, prev.spec, e.Path)
			}
			if err := r.put(&Entry{Path: dir, Type: TypeDir, Mode: 0o755, MTime: r.mtime, spec: "implied directory"}); err != nil {
				return nil, err
			}
		}
	}
	out := make([]Entry, 0, len(r.byPath))
	for _, e := range r.byPath {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if len(out) == 0 {
		return nil, errors.New("archive would be empty")
	}
	return out, nil
}

// hashFile hashes at most limit bytes of r and reports what kind of private
// key the content holds ("" for none).
func hashFile(r io.Reader, limit int64) (string, int64, string, error) {
	h := sha256.New()
	ks := &keyScanner{}
	n, err := io.Copy(io.MultiWriter(h, ks), io.LimitReader(r, limit+1))
	if err != nil {
		return "", 0, "", err
	}
	if n > limit {
		return "", 0, "", fmt.Errorf("file grew beyond %d bytes while reading", limit)
	}
	return hex.EncodeToString(h.Sum(nil)), n, ks.found, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
