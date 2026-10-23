package config

import (
	"fmt"
	"reflect"

	"gopkg.in/yaml.v3"
)

// Brew configures one Homebrew formula. Field names follow GoReleaser's
// `brews` entries so existing configurations can be copied over; see
// docs/compatibility/goreleaser.md for the field-by-field status.
type Brew struct {
	Name string `yaml:"name"`
	// IDs selects archive ids (GoReleaser: archive ids).
	IDs []string `yaml:"ids"`
	// Goarm / Goamd64 choose among ARM / AMD64 variants (GoReleaser).
	Goarm   string `yaml:"goarm"`
	Goamd64 string `yaml:"goamd64"`

	URLTemplate      string           `yaml:"url_template"`
	URLHeaders       []string         `yaml:"url_headers"`
	DownloadStrategy string           `yaml:"download_strategy"`
	CustomRequire    string           `yaml:"custom_require"`
	CommitAuthor     CommitAuthor     `yaml:"commit_author"`
	CommitMsg        string           `yaml:"commit_msg_template"`
	Directory        string           `yaml:"directory"`
	Folder           string           `yaml:"folder"` // deprecated GoReleaser name of directory
	Caveats          string           `yaml:"caveats"`
	Homepage         string           `yaml:"homepage"`
	Description      string           `yaml:"description"`
	License          string           `yaml:"license"`
	SkipUpload       string           `yaml:"skip_upload"`
	CustomBlock      string           `yaml:"custom_block"`
	Dependencies     []BrewDependency `yaml:"dependencies"`
	Conflicts        []string         `yaml:"conflicts"`
	Plist            string           `yaml:"plist"`
	Service          string           `yaml:"service"`
	Test             string           `yaml:"test"`
	Install          string           `yaml:"install"`
	ExtraInstall     string           `yaml:"extra_install"`
	PostInstall      string           `yaml:"post_install"`
	Repository       BrewRepository   `yaml:"repository"`
	Tap              *BrewRepository  `yaml:"tap"` // deprecated GoReleaser name of repository

	// Hephaestus extensions.

	// Version overrides the formula version (template); defaults to the
	// project version.
	Version string `yaml:"version"`
	// Output is where the formula is written locally (template). Defaults to
	// <dist>/homebrew/<directory>/<name>.rb.
	Output string `yaml:"output"`
	// Verify checks, against the manifest, that every path referenced by a
	// bin.install/man.install/... call exists in every archive.
	// Default true.
	Verify *bool `yaml:"verify"`
	// AllowDowngrade lets the tap stage replace a formula whose version is
	// newer than the one being published (e.g. when re-running an older
	// tag). By default such a formula is left alone. Default false.
	AllowDowngrade bool `yaml:"allow_downgrade"`
}

// VerifyEnabled applies the default.
func (b *Brew) VerifyEnabled() bool { return b.Verify == nil || *b.Verify }

// CommitAuthor mirrors GoReleaser's commit_author.
type CommitAuthor struct {
	Name    string     `yaml:"name"`
	Email   string     `yaml:"email"`
	Signing *yaml.Node `yaml:"signing"`
}

// BrewDependency is a depends_on entry. A bare string is the name.
type BrewDependency struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"` // "", "optional", "build", "test", "recommended"
	Version string `yaml:"version"`
	OS      string `yaml:"os"` // "", "mac", "linux"
}

// UnmarshalYAML accepts a bare string as the dependency name.
func (d *BrewDependency) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&d.Name)
	}
	type plain BrewDependency
	return decodeKnown(n, (*plain)(d))
}

// BrewRepository mirrors GoReleaser's repository.
type BrewRepository struct {
	Owner       string           `yaml:"owner"`
	Name        string           `yaml:"name"`
	Branch      string           `yaml:"branch"`
	Token       string           `yaml:"token"`
	TokenType   string           `yaml:"token_type"`
	Git         *yaml.Node       `yaml:"git"`
	PullRequest *BrewPullRequest `yaml:"pull_request"`
}

// BrewPullRequest mirrors GoReleaser's repository.pull_request.
type BrewPullRequest struct {
	Enabled    *bool       `yaml:"enabled"`
	Draft      bool        `yaml:"draft"`
	CheckBoxes bool        `yaml:"check_boxes"`
	Base       *BrewPRBase `yaml:"base"`
}

// BrewPRBase is the pull request's target repository.
type BrewPRBase struct {
	Owner  string `yaml:"owner"`
	Name   string `yaml:"name"`
	Branch string `yaml:"branch"`
}

// UnmarshalYAML accepts a bare string as the base branch, as GoReleaser does
// (`base: main`).
func (b *BrewPRBase) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&b.Branch)
	}
	type plain BrewPRBase
	return decodeKnown(n, (*plain)(b))
}

// decodeKnown decodes a mapping into v (a pointer to a struct), rejecting
// unknown and duplicate keys. Types with a custom UnmarshalYAML are skipped by
// checkKnown, so they must perform this check themselves.
func decodeKnown(n *yaml.Node, v any) error {
	if n.Kind != yaml.MappingNode {
		return &Error{Line: n.Line, Msg: "expected a mapping or a string"}
	}
	fields := yamlFields(reflect.TypeOf(v).Elem())
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		if seen[k.Value] {
			return &Error{Path: k.Value, Line: k.Line, Msg: "duplicate key"}
		}
		seen[k.Value] = true
		if _, ok := fields[k.Value]; !ok {
			return &Error{Path: k.Value, Line: k.Line, Msg: fmt.Sprintf("unknown field%s", suggest(k.Value, fields))}
		}
	}
	return decodeStrict(n, v)
}
