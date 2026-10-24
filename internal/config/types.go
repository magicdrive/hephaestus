// Package config loads, defaults and validates hephaestus.yaml.
package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// CurrentVersion is the configuration schema version.
const CurrentVersion = 1

// Config is the root of hephaestus.yaml.
type Config struct {
	Version  int       `yaml:"version"`
	Project  Project   `yaml:"project"`
	Dist     string    `yaml:"dist"`
	Manifest string    `yaml:"manifest"`
	Builds   []Build   `yaml:"builds"`
	Archives []Archive `yaml:"archives"`
	Checksum Checksum  `yaml:"checksum"`
	Release  Release   `yaml:"release"`
	Brews    []Brew    `yaml:"brews"`
	// BrewsFrom names a GoReleaser configuration whose brews entries are
	// appended to Brews (see AddBrews).
	BrewsFrom string     `yaml:"brews_from"`
	Homebrew  *Brew      `yaml:"homebrew"`
	Limits    Limits     `yaml:"limits"`
	Env       []EnvEntry `yaml:"env"`

	// Root is the directory containing the configuration file. All source
	// paths are resolved against it. Not read from YAML.
	Root string `yaml:"-"`
	// node is the parsed YAML document, kept to locate validation errors.
	node     *yaml.Node
	warnings []string
}

// Project describes the software being released.
type Project struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Homepage    string `yaml:"homepage"`
	License     string `yaml:"license"`
}

// EnvEntry is a KEY=VALUE pair exposed to templates as .Env.KEY, in addition
// to the process environment. It is a list, like GoReleaser's env.
type EnvEntry string

// Target is one OS/architecture pair.
type Target struct {
	OS   string `yaml:"os"`
	Arch string `yaml:"arch"`
	Arm  string `yaml:"arm"`
}

func (t Target) String() string {
	if t.Arm != "" {
		return t.OS + "/" + t.Arch + "v" + t.Arm
	}
	return t.OS + "/" + t.Arch
}

// Matrix declares targets as a cross product with exclusions.
type Matrix struct {
	OS      []string `yaml:"os"`
	Arch    []string `yaml:"arch"`
	Exclude []Target `yaml:"exclude"`
}

// Build declares one build. Exactly one of Command, GoReleaser or Existing is
// set; it selects the provider.
type Build struct {
	ID      string   `yaml:"id"`
	Targets []Target `yaml:"targets"`
	Matrix  *Matrix  `yaml:"matrix"`

	// Command provider.
	Command Command           `yaml:"command"`
	Dir     string            `yaml:"dir"`
	Env     map[string]string `yaml:"env"`
	Timeout Duration          `yaml:"timeout"`
	// Output is the path of the built executable for each target (template).
	Output string `yaml:"output"`

	// GoReleaser provider.
	GoReleaser *GoReleaserImport `yaml:"goreleaser"`

	// Existing provider.
	Existing *ExistingImport `yaml:"existing"`
}

// Provider names.
const (
	ProviderCommand    = "command"
	ProviderGoReleaser = "goreleaser"
	ProviderExisting   = "existing"
)

// ImportsArchives reports whether the build imports prebuilt archives
// (existing kind archive, or goreleaser with archives: true). Such archives
// are recorded under the build's id and can be selected by brews[].ids.
func (b *Build) ImportsArchives() bool {
	return (b.Existing != nil && b.Existing.Kind == ExistingKindArchive) || (b.GoReleaser != nil && b.GoReleaser.Archives)
}

// Provider returns the provider this build uses.
func (b *Build) Provider() string {
	switch {
	case b.GoReleaser != nil:
		return ProviderGoReleaser
	case b.Existing != nil:
		return ProviderExisting
	default:
		return ProviderCommand
	}
}

// GoReleaserImport reads binaries (and optionally archives) from
// GoReleaser's dist/artifacts.json.
type GoReleaserImport struct {
	// Artifacts is the project-relative path of artifacts.json; defaults to
	// dist/artifacts.json.
	Artifacts string `yaml:"artifacts"`
	// IDs keeps only artifacts whose extra.ID is listed (GoReleaser build
	// ids for binaries, archive ids for archives). Empty keeps all.
	IDs []string `yaml:"ids"`
	// Archives also imports artifacts of type "Archive" as archive
	// artifacts, so GoReleaser-made archives can be published as they are.
	Archives bool `yaml:"archives"`
	// GoAMD64 selects the amd64 microarchitecture level ("v1" to "v4") to
	// import when GoReleaser built several; defaults to v1. Artifacts of
	// other levels are skipped because a target has no level dimension.
	GoAMD64 string `yaml:"goamd64"`
	// AllowSensitive accepts imported archives (archives: true) whose
	// members look like credentials: sensitive names or directories, or
	// content holding a private key. Off by default.
	AllowSensitive bool `yaml:"allow_sensitive"`
}

// Existing import kinds.
const (
	ExistingKindBinary  = "binary"
	ExistingKindArchive = "archive"
)

// ExistingImport takes prebuilt files from a path template evaluated for
// every target.
type ExistingImport struct {
	Path string `yaml:"path"`
	// Kind is "binary" (default: an executable) or "archive" (a .tar.gz,
	// .tgz or .zip imported as a release-ready archive).
	Kind string `yaml:"kind"`
	// AllowSensitive accepts imported archives (kind: archive) whose
	// members look like credentials: sensitive names or directories, or
	// content holding a private key. Off by default.
	AllowSensitive bool `yaml:"allow_sensitive"`
}

// Command is an argument vector. In YAML it is either a list (executed
// directly, no shell) or a string (executed with "sh -c" on Unix and
// "cmd /C" on Windows).
type Command struct {
	Args  []string
	Shell string
}

// IsZero reports whether no command was configured.
func (c Command) IsZero() bool { return len(c.Args) == 0 && c.Shell == "" }

// UnmarshalYAML accepts a string or a list of strings.
func (c *Command) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.Decode(&c.Shell)
	case yaml.SequenceNode:
		return n.Decode(&c.Args)
	}
	return fmt.Errorf("line %d: command must be a string or a list of strings", n.Line)
}

// Archive declares one family of archives, instantiated once per target.
type Archive struct {
	ID string `yaml:"id"`
	// Builds restricts targets (and default contents) to these build ids.
	Builds []string `yaml:"builds"`
	// Targets lists targets explicitly when there are no build artifacts.
	Targets []Target `yaml:"targets"`
	// Name is the archive file name without extension (template).
	Name            string           `yaml:"name"`
	Format          string           `yaml:"format"`
	FormatOverrides []FormatOverride `yaml:"format_overrides"`
	// WrapInDirectory places every entry under one top-level directory:
	// false (default), true (the archive name) or a template.
	WrapInDirectory BoolOrString `yaml:"wrap_in_directory"`
	// Files is the complete content list. When omitted, the archive holds
	// the target's binaries at its root.
	Files []FileSpec `yaml:"files"`
	// MTime sets every entry's modification time: RFC 3339 or Unix seconds
	// (template). Defaults to $SOURCE_DATE_EPOCH, else 1980-01-01T00:00:00Z.
	MTime string `yaml:"mtime"`
	// Symlinks is "follow" (default: store the target's content, which must
	// lie inside the project root) or "reject".
	Symlinks string `yaml:"symlinks"`

	filesSet bool
}

// FilesDeclared reports whether the YAML contained a files key, so that an
// explicit empty list is not mistaken for "use the default contents".
func (a *Archive) FilesDeclared() bool { return a.filesSet }

// FormatOverride switches the format for one OS (e.g. zip on windows).
type FormatOverride struct {
	OS     string `yaml:"os"`
	Format string `yaml:"format"`
}

// FileSpec selects source files and places them in the archive.
type FileSpec struct {
	Src     string   `yaml:"src"`
	Dst     string   `yaml:"dst"`
	Exclude []string `yaml:"exclude"`
	// Mode overrides the entry permission ("0644", "0755").
	Mode string `yaml:"mode"`
	// Required defaults to true: a literal src must exist and a glob must
	// match at least one file.
	Required *bool `yaml:"required"`
	// StripParent places glob matches by base name only (GoReleaser name).
	StripParent bool `yaml:"strip_parent"`
	// When limits the entry to some targets.
	When *Condition `yaml:"when"`
	// Info is accepted for GoReleaser compatibility.
	Info *FileInfo `yaml:"info"`
	// AllowSensitive permits files that look like credentials.
	AllowSensitive bool `yaml:"allow_sensitive"`
}

// IsRequired applies the default.
func (f *FileSpec) IsRequired() bool { return f.Required == nil || *f.Required }

// UnmarshalYAML accepts a bare string as shorthand for {src: <string>}.
func (f *FileSpec) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&f.Src)
	}
	type plain FileSpec
	return decodeStrict(n, (*plain)(f))
}

// FileInfo mirrors GoReleaser's files[].info.
type FileInfo struct {
	Owner string `yaml:"owner"`
	Group string `yaml:"group"`
	Mode  string `yaml:"mode"`
	MTime string `yaml:"mtime"`
}

// Condition restricts a file entry to matching targets. Empty lists match
// everything.
type Condition struct {
	OS   []string `yaml:"os"`
	Arch []string `yaml:"arch"`
}

// Matches reports whether t satisfies c.
func (c *Condition) Matches(t Target) bool {
	if c == nil {
		return true
	}
	return (len(c.OS) == 0 || containsStr(c.OS, t.OS)) && (len(c.Arch) == 0 || containsStr(c.Arch, t.Arch))
}

// Checksum configures the checksums file.
type Checksum struct {
	Disable bool   `yaml:"disable"`
	Name    string `yaml:"name"`
	// Algorithm is accepted for GoReleaser compatibility; only sha256.
	Algorithm string `yaml:"algorithm"`
}

// Release configures publication.
type Release struct {
	GitHub *GitHubRelease `yaml:"github"`
}

// GitHubRelease configures the GitHub Release publisher.
type GitHubRelease struct {
	Enabled *bool  `yaml:"enabled"`
	Owner   string `yaml:"owner"`
	Repo    string `yaml:"name"`
	// Name is the release title (template); defaults to the tag.
	Name       string `yaml:"name_template"`
	Draft      bool   `yaml:"draft"`
	Prerelease string `yaml:"prerelease"` // "auto" (default), "true", "false"
	MakeLatest string `yaml:"make_latest"`
	Notes      string `yaml:"notes"`
	NotesFile  string `yaml:"notes_file"`
	// GenerateNotes lets GitHub generate the notes when the release is
	// created and neither notes nor notes_file is set.
	GenerateNotes bool `yaml:"generate_notes"`
	// TargetCommitish is used only when the tag does not exist yet.
	TargetCommitish string `yaml:"target_commitish"`
	// ReplaceExistingAssets allows replacing a same-named asset whose
	// content differs. Identical assets are always a no-op.
	ReplaceExistingAssets bool `yaml:"replace_existing_assets"`
	// Include selects which manifest kinds are uploaded.
	Include []string `yaml:"include"`
	// APIURL and UploadURL support GitHub Enterprise Server.
	APIURL    string `yaml:"api_url"`
	UploadURL string `yaml:"upload_url"`
}

// IsEnabled applies the default (enabled when the section is present).
func (g *GitHubRelease) IsEnabled() bool { return g != nil && (g.Enabled == nil || *g.Enabled) }

// Limits bounds archive generation.
type Limits struct {
	MaxFileSize  ByteSize `yaml:"max_file_size"`
	MaxTotalSize ByteSize `yaml:"max_total_size"`
	MaxFiles     int      `yaml:"max_files"`
}

// Default limits.
const (
	DefaultMaxFileSize  ByteSize = 2 << 30
	DefaultMaxTotalSize ByteSize = 4 << 30
	DefaultMaxFiles              = 10000
)

// Duration is a time.Duration written as "90s" or "10m".
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil || v < 0 {
		return fmt.Errorf("line %d: invalid duration %q", n.Line, s)
	}
	*d = Duration(v)
	return nil
}

// ByteSize is a size written as a number of bytes or with a KiB/MiB/GiB unit.
type ByteSize int64

// UnmarshalYAML parses sizes such as 1048576, "512MiB" or "2GiB".
func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := ParseByteSize(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*b = v
	return nil
}

var sizeRe = regexp.MustCompile(`^([0-9]+)\s*(B|KiB|MiB|GiB|TiB)?$`)

// ParseByteSize parses "123", "10KiB", "512MiB", "2GiB".
func ParseByteSize(s string) (ByteSize, error) {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("invalid size %q (use bytes or KiB/MiB/GiB/TiB)", s)
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	shift := map[string]uint{"": 0, "B": 0, "KiB": 10, "MiB": 20, "GiB": 30, "TiB": 40}[m[2]]
	if n > (1<<62)>>shift {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return ByteSize(n << shift), nil
}

// BoolOrString holds YAML true/false or a string.
type BoolOrString struct {
	Set   bool
	Bool  bool
	Value string
}

// UnmarshalYAML accepts a boolean or a string.
func (v *BoolOrString) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: expected true, false or a string", n.Line)
	}
	v.Set = true
	if n.Tag == "!!bool" {
		return n.Decode(&v.Bool)
	}
	v.Bool = true
	return n.Decode(&v.Value)
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
