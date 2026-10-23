// Package manifest defines the Artifact Manifest: the build-system-independent
// contract every Hephaestus stage reads and writes.
//
// Ownership: each stage only adds artifacts of its own kind, or fills fields
// it owns on existing artifacts (see the ownership table in docs/architecture/overview.md). No stage
// invents data it did not observe: a checksum is present only when it was
// computed from the bytes on disk or verified against them.
package manifest

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/magicdrive/hephaestus/internal/pathutil"
)

// SchemaVersion is the manifest schema this build reads and writes.
const SchemaVersion = 1

// DefaultPath is where stages read and write the manifest, relative to the
// project root, unless configured otherwise.
const DefaultPath = "dist/hephaestus-manifest.json"

// MaxSize bounds a manifest file read from disk.
const MaxSize = 32 << 20

// Kind classifies an artifact.
type Kind string

// Artifact kinds.
const (
	KindBinary   Kind = "binary"   // an executable produced by a build provider
	KindArchive  Kind = "archive"  // a tar.gz or zip produced by the archives stage
	KindChecksum Kind = "checksum" // a checksums file covering other artifacts
	KindFormula  Kind = "formula"  // a generated Homebrew formula
)

func (k Kind) valid() bool {
	switch k {
	case KindBinary, KindArchive, KindChecksum, KindFormula:
		return true
	}
	return false
}

// Manifest is the root document.
type Manifest struct {
	SchemaVersion int        `json:"schema_version"`
	Project       string     `json:"project"`
	Version       string     `json:"version"`
	Tag           string     `json:"tag,omitempty"`
	Commit        string     `json:"commit,omitempty"`
	Artifacts     []Artifact `json:"artifacts"`
}

// Artifact is one file produced or imported by a stage.
type Artifact struct {
	Kind Kind `json:"kind"`
	// ID is the configuration entry (build or archive id) that produced it.
	ID string `json:"id"`
	// Name is the file name, which is also the release asset name.
	Name string `json:"name"`
	// Path is the slash-separated location relative to the project root.
	Path   string `json:"path"`
	OS     string `json:"os,omitempty"`
	Arch   string `json:"arch,omitempty"`
	Arm    string `json:"arm,omitempty"`
	Format string `json:"format,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
	// DownloadURL is set by the release stage after a verified upload, or by
	// an import of an existing release.
	DownloadURL string `json:"download_url,omitempty"`
	// Executables lists executable paths: for a binary, its own file name;
	// for an archive, entry paths inside the archive.
	Executables []string `json:"executables,omitempty"`
	// Files describes archive contents in entry order.
	Files  []File `json:"files,omitempty"`
	Source Source `json:"source"`
}

// File is one archive entry.
type File struct {
	Path   string `json:"path"`
	Type   string `json:"type"` // "file" or "dir"
	Mode   string `json:"mode"` // four-digit octal, e.g. "0755"
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	// Source is the project-relative path the entry was read from.
	Source string `json:"source,omitempty"`
}

// Source records where an artifact came from, for traceability.
type Source struct {
	Stage    string `json:"stage"`              // build, archive, checksum, release, homebrew
	Provider string `json:"provider,omitempty"` // command, goreleaser, existing, github-release
	Origin   string `json:"origin,omitempty"`   // e.g. the GoReleaser artifacts.json path
}

// New returns an empty manifest.
func New(project, version, tag, commit string) *Manifest {
	return &Manifest{SchemaVersion: SchemaVersion, Project: project, Version: version, Tag: tag, Commit: commit, Artifacts: []Artifact{}}
}

// Read parses and validates a manifest. Unknown fields are rejected so that a
// manifest from a newer Hephaestus is not silently misread.
func Read(r io.Reader) (*Manifest, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxSize {
		return nil, fmt.Errorf("manifest exceeds %d bytes", MaxSize)
	}
	var probe struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("manifest is not valid JSON: %w", err)
	}
	if probe.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("manifest schema_version %d is not supported (this build supports %d)", probe.SchemaVersion, SchemaVersion)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if dec.More() {
		return nil, errors.New("manifest: trailing data after JSON document")
	}
	if m.Artifacts == nil {
		m.Artifacts = []Artifact{}
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// ReadFile reads a manifest from disk.
func ReadFile(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m, err := Read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// Marshal returns the canonical serialization: artifacts sorted, two-space
// indentation, trailing newline. Equal manifests always serialize equally.
func (m *Manifest) Marshal() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	c := *m
	c.Artifacts = append([]Artifact(nil), m.Artifacts...)
	sortArtifacts(c.Artifacts)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(&c); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WriteFile writes the canonical serialization atomically (temp file and
// rename in the same directory) so a crash never leaves a torn manifest.
//
// WriteFile follows symbolic links in path; to write under a project
// directory that must confine the output, use WriteFileIn.
func (m *Manifest) WriteFile(path string) error {
	data, err := m.Marshal()
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o644)
}

// WriteFileAtomic writes data to path via a temporary file and rename. It
// follows symbolic links in path; use WriteFileAtomicIn to confine the write
// to a root directory.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close() // already failing; the original error takes precedence
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close() // already failing; the original error takes precedence
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close() // already failing; the original error takes precedence
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Validate checks structural invariants.
func (m *Manifest) Validate() error {
	var errs []error
	if m.SchemaVersion != SchemaVersion {
		errs = append(errs, fmt.Errorf("schema_version must be %d", SchemaVersion))
	}
	if m.Project == "" {
		errs = append(errs, errors.New("project is required"))
	}
	if m.Version == "" {
		errs = append(errs, errors.New("version is required"))
	}
	names := map[string]int{}
	for i, a := range m.Artifacts {
		where := fmt.Sprintf("artifacts[%d] (%s)", i, a.Name)
		if err := a.validate(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
		}
		// Release asset names must be unique; binaries are not uploaded by
		// name but must still be unambiguous per target.
		key := string(a.Kind) + "\x00" + a.Name
		if a.Kind == KindBinary {
			key += "\x00" + a.OS + "\x00" + a.Arch + "\x00" + a.Arm
		}
		if j, dup := names[key]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicates artifacts[%d]", where, j))
		}
		names[key] = i
	}
	return errors.Join(errs...)
}

func (a *Artifact) validate() error {
	var errs []error
	if !a.Kind.valid() {
		errs = append(errs, fmt.Errorf("unknown kind %q", a.Kind))
	}
	if a.Name == "" || strings.Contains(a.Name, "/") {
		errs = append(errs, errors.New("name must be a non-empty file name"))
	} else if err := pathutil.ValidateRelative(a.Name); err != nil {
		errs = append(errs, fmt.Errorf("name: %w", err))
	}
	if err := pathutil.ValidateRelative(a.Path); err != nil {
		errs = append(errs, fmt.Errorf("path %q: %w", a.Path, err))
	}
	if a.SHA256 != "" && !IsSHA256(a.SHA256) {
		errs = append(errs, errors.New("sha256 must be 64 lowercase hex characters"))
	}
	if a.Size < 0 {
		errs = append(errs, errors.New("size must not be negative"))
	}
	if a.Source.Stage == "" {
		errs = append(errs, errors.New("source.stage is required"))
	}
	switch a.Kind {
	case KindBinary:
		if a.OS == "" || a.Arch == "" {
			errs = append(errs, errors.New("binary requires os and arch"))
		}
	case KindArchive:
		if a.OS == "" || a.Arch == "" {
			errs = append(errs, errors.New("archive requires os and arch"))
		}
		if a.Format != "tar.gz" && a.Format != "zip" {
			errs = append(errs, fmt.Errorf("archive format %q is not tar.gz or zip", a.Format))
		}
		if a.SHA256 == "" {
			errs = append(errs, errors.New("archive requires sha256"))
		}
	}
	for _, e := range a.Executables {
		if err := pathutil.ValidateRelative(e); err != nil {
			errs = append(errs, fmt.Errorf("executable %q: %w", e, err))
		}
	}
	seen := map[string]bool{}
	for _, f := range a.Files {
		if err := pathutil.ValidateRelative(f.Path); err != nil {
			errs = append(errs, fmt.Errorf("file %q: %w", f.Path, err))
		}
		if seen[f.Path] {
			errs = append(errs, fmt.Errorf("file %q listed twice", f.Path))
		}
		seen[f.Path] = true
		if f.Type != "file" && f.Type != "dir" {
			errs = append(errs, fmt.Errorf("file %q: type must be file or dir", f.Path))
		}
		if f.SHA256 != "" && !IsSHA256(f.SHA256) {
			errs = append(errs, fmt.Errorf("file %q: invalid sha256", f.Path))
		}
	}
	return errors.Join(errs...)
}

// IsSHA256 reports whether s is a lowercase hex SHA-256 digest.
func IsSHA256(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// Put inserts a or replaces the artifact with the same kind, name and target.
func (m *Manifest) Put(a Artifact) {
	for i, b := range m.Artifacts {
		if b.Kind == a.Kind && b.Name == a.Name && b.OS == a.OS && b.Arch == a.Arch && b.Arm == a.Arm {
			m.Artifacts[i] = a
			return
		}
	}
	m.Artifacts = append(m.Artifacts, a)
}

// RemoveKind drops every artifact of kind k with one of the given ids (all ids
// when none are given). Stages call it before regenerating their outputs so a
// re-run replaces rather than accumulates.
func (m *Manifest) RemoveKind(k Kind, ids ...string) {
	keep := m.Artifacts[:0]
	for _, a := range m.Artifacts {
		if a.Kind == k && (len(ids) == 0 || contains(ids, a.ID)) {
			continue
		}
		keep = append(keep, a)
	}
	m.Artifacts = keep
}

// RemoveFunc drops every artifact for which drop returns true.
func (m *Manifest) RemoveFunc(drop func(Artifact) bool) {
	keep := m.Artifacts[:0]
	for _, a := range m.Artifacts {
		if !drop(a) {
			keep = append(keep, a)
		}
	}
	m.Artifacts = keep
}

// Filter returns the artifacts of kind k, optionally restricted to ids.
func (m *Manifest) Filter(k Kind, ids ...string) []Artifact {
	var out []Artifact
	for _, a := range m.Artifacts {
		if a.Kind == k && (len(ids) == 0 || contains(ids, a.ID)) {
			out = append(out, a)
		}
	}
	sortArtifacts(out)
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

var kindOrder = map[Kind]int{KindBinary: 0, KindArchive: 1, KindChecksum: 2, KindFormula: 3}

func sortArtifacts(a []Artifact) {
	sort.SliceStable(a, func(i, j int) bool {
		x, y := a[i], a[j]
		if x.Kind != y.Kind {
			return kindOrder[x.Kind] < kindOrder[y.Kind]
		}
		if x.Name != y.Name {
			return x.Name < y.Name
		}
		if x.OS != y.OS {
			return x.OS < y.OS
		}
		if x.Arch != y.Arch {
			return x.Arch < y.Arch
		}
		return x.Arm < y.Arm
	})
}
