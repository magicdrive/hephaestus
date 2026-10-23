// Package build implements the build stage: it produces or imports the
// per-target executables that later stages package and publish, and records
// them in the Artifact Manifest.
//
// Three providers exist, selected per build entry:
//
//   - command runs a user command once per target. Hephaestus does not
//     cross-compile anything by itself: it only tells the command which
//     target to build (HEPHAESTUS_OS, HEPHAESTUS_ARCH, HEPHAESTUS_ARM and
//     the template variables) and where to write the result
//     (HEPHAESTUS_OUTPUT). Declaring a matrix therefore does not make a
//     build portable; producing a correct executable for each target is the
//     command's responsibility. The output must exist after the command
//     exits, or the build fails.
//   - existing takes prebuilt files (executables, or release-ready archives
//     with kind: archive) from a path template evaluated per target.
//   - goreleaser imports binaries (and optionally archives) that GoReleaser
//     listed in dist/artifacts.json.
//
// Every recorded file is verified on disk: it must be a non-empty regular
// file inside the project root (a symbolic link is accepted only when it
// resolves inside the root) and no larger than limits.max_file_size. Size
// and SHA-256 are always computed from the bytes on disk; a checksum
// claimed by another tool is only compared, never trusted.
package build

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/pathutil"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// DefaultTimeout bounds one command run (one target) when the build does
// not set timeout.
const DefaultTimeout = 30 * time.Minute

// StageName is recorded as manifest Source.Stage.
const StageName = "build"

// Input is what the build stage needs.
type Input struct {
	Config   *config.Config
	Manifest *manifest.Manifest
	// Vars holds the project-level template variables (including Env); the
	// target fields are set per target.
	Vars tmpl.Vars
	// IDs restricts the stage to these build ids (all when empty).
	IDs []string
	// Environ is the base environment for commands, typically os.Environ().
	// Nothing else from the parent process environment is inherited.
	Environ []string
	// Stdout and Stderr receive the commands' output; nil discards it.
	Stdout, Stderr io.Writer
	// DryRun plans command builds without running them and leaves the
	// manifest unchanged. Import providers are still verified, read-only.
	DryRun bool
}

// PlannedCommand is one command invocation, as run or as planned in a dry
// run.
type PlannedCommand struct {
	BuildID string
	Target  config.Target
	// Argv is the exact argument vector; a shell command appears as
	// ["/bin/sh", "-c", script] or ["cmd.exe", "/d", "/s", "/c", script].
	Argv []string
	// Dir is the absolute working directory.
	Dir string
	// Env lists the KEY=VALUE pairs added to (or overriding) Input.Environ,
	// in the order they are applied.
	Env []string
	// Output is the project-relative path the command must produce.
	Output  string
	Timeout time.Duration
}

// Result summarizes one build stage run.
type Result struct {
	// Binaries are the kind=binary artifacts recorded (or, in a dry run,
	// that would be recorded by import providers).
	Binaries []manifest.Artifact
	// Planned lists every command invocation, in execution order.
	Planned []PlannedCommand
	// Imported are kind=archive artifacts taken from existing files or
	// GoReleaser.
	Imported []manifest.Artifact
}

// Run executes every selected build in configuration order, each target in
// declaration order. The manifest is updated only when every build
// succeeded: binary (and imported archive) entries of the processed build
// ids are replaced, so a re-run never accumulates stale entries. In a dry
// run the manifest is left untouched.
//
// On error the partial result describes what was completed.
func Run(ctx context.Context, in Input) (*Result, error) {
	if in.Config == nil {
		return nil, errors.New("build: no configuration")
	}
	if in.Manifest == nil && !in.DryRun {
		return nil, errors.New("build: no manifest")
	}
	rootDir, err := filepath.Abs(in.Config.Root)
	if err != nil {
		return nil, fmt.Errorf("project root: %w", err)
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, fmt.Errorf("open project root: %w", err)
	}
	defer root.Close()

	for _, id := range in.IDs {
		if !hasBuild(in.Config, id) {
			return nil, fmt.Errorf("unknown build id %q", id)
		}
	}
	s := &stage{in: in, root: root, rootDir: rootDir, res: &Result{}}
	// Commands write stdout and stderr from separate goroutines; callers
	// commonly pass the same writer for both, so serialize the writes.
	var mu sync.Mutex
	s.in.Stdout = &lockedWriter{mu: &mu, w: orDiscard(in.Stdout)}
	s.in.Stderr = &lockedWriter{mu: &mu, w: orDiscard(in.Stderr)}
	var ids []string
	for i := range in.Config.Builds {
		b := &in.Config.Builds[i]
		if len(in.IDs) > 0 && !contains(in.IDs, b.ID) {
			continue
		}
		ids = append(ids, b.ID)
		var err error
		switch b.Provider() {
		case config.ProviderCommand:
			err = s.runCommand(ctx, b)
		case config.ProviderExisting:
			err = s.runExisting(b)
		case config.ProviderGoReleaser:
			err = s.runGoReleaser(b)
		default:
			err = fmt.Errorf("unknown provider %q", b.Provider())
		}
		if err != nil {
			return s.res, fmt.Errorf("build %s: %w", b.ID, err)
		}
	}
	if in.DryRun {
		return s.res, nil
	}
	return s.res, s.record(ids)
}

type stage struct {
	in      Input
	root    *os.Root
	rootDir string
	res     *Result
}

// record replaces the entries of the processed builds in the manifest.
func (s *stage) record(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	m := s.in.Manifest
	// Refuse to shadow a binary of another build: Manifest.Put would
	// silently replace it.
	for _, a := range s.res.Binaries {
		for _, o := range m.Artifacts {
			if o.Kind == manifest.KindBinary && !contains(ids, o.ID) && o.Name == a.Name && o.OS == a.OS && o.Arch == a.Arch && o.Arm == a.Arm {
				return fmt.Errorf("build %s: binary %s for %s/%s is also recorded by build %s; give the outputs distinct names", a.ID, a.Name, a.OS, a.Arch, o.ID)
			}
		}
	}
	for _, a := range s.res.Imported {
		for _, o := range m.Artifacts {
			if o.Kind == manifest.KindArchive && o.Name == a.Name && !(contains(ids, o.ID) && o.Source.Stage == StageName) {
				return fmt.Errorf("build %s: archive %s is also recorded by %s %s", a.ID, a.Name, o.Source.Stage, o.ID)
			}
		}
	}
	m.RemoveKind(manifest.KindBinary, ids...)
	// Only archives this stage imported are replaced; archives stage output
	// may share an id (both commonly "default").
	keep := m.Artifacts[:0]
	for _, a := range m.Artifacts {
		if a.Kind == manifest.KindArchive && a.Source.Stage == StageName && contains(ids, a.ID) {
			continue
		}
		keep = append(keep, a)
	}
	m.Artifacts = keep
	for _, a := range s.res.Binaries {
		m.Put(a)
	}
	for _, a := range s.res.Imported {
		m.Put(a)
	}
	return m.Validate()
}

var (
	identRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_]*$`)
	armRe   = regexp.MustCompile(`^[0-9]*$`)
)

// targets expands and re-checks a build's targets; Run may be called with a
// configuration that was not loaded through config.Load.
func targets(b *config.Build) ([]config.Target, error) {
	ts := config.ExpandTargets(b.Targets, b.Matrix)
	if len(ts) == 0 {
		return nil, errors.New("no targets: declare targets or matrix")
	}
	seen := map[string]bool{}
	for _, t := range ts {
		if !identRe.MatchString(t.OS) || !identRe.MatchString(t.Arch) || !armRe.MatchString(t.Arm) {
			return nil, fmt.Errorf("invalid target %+v: os and arch must match %s and arm must be a number", t, identRe)
		}
		if seen[t.String()] {
			return nil, fmt.Errorf("target %s is declared twice", t)
		}
		seen[t.String()] = true
	}
	return ts, nil
}

// renderPath renders a project-relative path template and validates it.
func renderPath(field, s string, vars tmpl.Vars) (string, error) {
	p, err := tmpl.Render(s, vars)
	if err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	if err := pathutil.ValidateRelative(p); err != nil {
		return "", fmt.Errorf("%s %q must be a relative path inside the project root: %w", field, p, err)
	}
	return p, nil
}

// errMissing marks an output that does not exist.
var errMissing = errors.New("does not exist")

// verified is an opened, checked file inside the root.
type verified struct {
	file   *os.File
	size   int64
	sha256 string
}

// openVerified opens rel inside the root and checks it is a non-empty
// regular file within maxSize, then hashes it. The caller closes file.
// Content and hash come from the same open handle, so a file replaced
// during the check cannot pass with someone else's bytes.
func openVerified(root *os.Root, rel string, maxSize int64) (*verified, error) {
	if err := pathutil.ValidateRelative(rel); err != nil {
		return nil, fmt.Errorf("path %q must be a relative path inside the project root: %w", rel, err)
	}
	lst, err := root.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("%q %w", rel, errMissing)
	case errors.Is(err, fs.ErrPermission):
		return nil, fmt.Errorf("%q: permission denied: %w", rel, err)
	case err != nil:
		return nil, fmt.Errorf("%q: %w", rel, err)
	}
	if lst.Mode()&fs.ModeSymlink != 0 {
		// os.Root refuses links that leave the root or are absolute.
		if _, err := root.Stat(rel); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%q is a broken symbolic link", rel)
			}
			return nil, fmt.Errorf("symbolic link %q resolves outside the project root: %w", rel, err)
		}
	}
	f, err := root.Open(rel)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("%q: permission denied: %w", rel, err)
		}
		return nil, fmt.Errorf("open %q: %w", rel, err)
	}
	st, err := f.Stat()
	if err == nil {
		switch {
		case st.IsDir():
			err = fmt.Errorf("%q is a directory, not a file", rel)
		case !st.Mode().IsRegular():
			err = fmt.Errorf("%q is not a regular file (%s)", rel, st.Mode().Type())
		case st.Size() == 0:
			err = fmt.Errorf("%q is empty", rel)
		case st.Size() > maxSize:
			err = fmt.Errorf("%q is %d bytes, over limits.max_file_size (%d)", rel, st.Size(), maxSize)
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxSize+1))
	if err == nil && n > maxSize {
		err = fmt.Errorf("%q grew beyond limits.max_file_size (%d) while reading", rel, maxSize)
	}
	if err == nil && n != st.Size() {
		err = fmt.Errorf("%q changed size while reading (%d, then %d bytes)", rel, st.Size(), n)
	}
	if err != nil {
		f.Close()
		if errors.Is(err, fs.ErrPermission) {
			return nil, fmt.Errorf("%q: permission denied: %w", rel, err)
		}
		return nil, fmt.Errorf("read %q: %w", rel, err)
	}
	return &verified{file: f, size: n, sha256: hex.EncodeToString(h.Sum(nil))}, nil
}

// verifyFile is openVerified for callers that only need size and digest.
func verifyFile(root *os.Root, rel string, maxSize int64) (int64, string, error) {
	v, err := openVerified(root, rel, maxSize)
	if err != nil {
		return 0, "", err
	}
	v.file.Close()
	return v.size, v.sha256, nil
}

func (s *stage) maxFileSize() int64 {
	if m := int64(s.in.Config.Limits.MaxFileSize); m > 0 {
		return m
	}
	return int64(config.DefaultMaxFileSize)
}

func binaryArtifact(b *config.Build, provider, origin, rel string, t config.Target, size int64, sum string) manifest.Artifact {
	name := pathutil.Base(rel)
	return manifest.Artifact{
		Kind: manifest.KindBinary, ID: b.ID, Name: name, Path: rel,
		OS: t.OS, Arch: t.Arch, Arm: t.Arm, SHA256: sum, Size: size,
		Executables: []string{name},
		Source:      manifest.Source{Stage: StageName, Provider: provider, Origin: origin},
	}
}

func hasBuild(c *config.Config, id string) bool {
	for i := range c.Builds {
		if c.Builds[i].ID == id {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// lockedWriter serializes writes to a writer shared by several streams.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
