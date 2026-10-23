// Package goreleaser reads the parts of a GoReleaser configuration that
// Hephaestus can take over: the project name and the `brews` entries.
//
// The `brews` schema is GoReleaser v2's (OSS). Every field is checked:
// unknown fields, GoReleaser Pro fields, fields removed in GoReleaser v2 and
// template syntax that Hephaestus cannot render are reported precisely
// instead of being silently ignored.
package goreleaser

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// MaxConfigSize bounds the GoReleaser configuration file.
const MaxConfigSize = config.MaxConfigSize

// field describes how one GoReleaser key is handled.
type field struct {
	kind fieldKind
	// tmpl: the value is a Hephaestus template (checked with tmpl.Check);
	// literal: template syntax is rejected because Hephaestus uses the value
	// verbatim.
	tmpl, literal bool
	// scalarOK allows a scalar where children describe a mapping (a bare
	// pull_request.base branch, or a bare dependency name in a list).
	scalarOK bool
	// children describes a nested mapping (or the mapping items of a list).
	children map[string]field
	msg      string // for pro/removed/unsupported/noop
}

type fieldKind int

const (
	supported   fieldKind = iota
	proOnly               // GoReleaser Pro only: error
	removed               // removed in GoReleaser v2 and unsupported: error
	removedConv           // removed in GoReleaser v2, converted with a warning
	unsupported           // OSS field Hephaestus does not implement: error
	noop                  // accepted with a warning, no effect
)

var (
	text    = field{tmpl: true}
	literal = field{literal: true}

	baseFields = map[string]field{"owner": text, "name": text, "branch": text}

	pullRequestFields = map[string]field{
		"enabled":     literal,
		"draft":       literal,
		"base":        {tmpl: true, scalarOK: true, children: baseFields},
		"check_boxes": {kind: proOnly},
		"body":        {kind: unsupported, msg: "pull request bodies are not supported"},
		"token":       {kind: unsupported, msg: "a separate pull request token is not supported; use repository.token"},
	}

	repositoryFields = map[string]field{
		"owner":        text,
		"name":         text,
		"branch":       text,
		"token":        text,
		"token_type":   {kind: proOnly},
		"git":          {kind: unsupported, msg: "publishing over plain git (SSH) is not supported; Hephaestus publishes through the GitHub API"},
		"pull_request": {children: pullRequestFields},
	}

	brewFields = map[string]field{
		"name":                literal,
		"ids":                 literal,
		"goarm":               literal,
		"goamd64":             literal,
		"url_template":        text,
		"url_headers":         text,
		"download_strategy":   literal,
		"custom_require":      literal,
		"commit_msg_template": text,
		"directory":           text,
		"caveats":             text,
		"homepage":            text,
		"description":         text,
		"license":             literal,
		"skip_upload":         literal,
		"custom_block":        text,
		"dependencies": {literal: true, scalarOK: true, children: map[string]field{
			"name": literal, "type": literal, "version": literal, "os": literal,
		}},
		"conflicts":     literal,
		"service":       text,
		"test":          text,
		"install":       text,
		"extra_install": text,
		"post_install":  text,
		"commit_author": {children: map[string]field{
			"name":                 text,
			"email":                text,
			"signing":              {kind: unsupported, msg: "commit signing is not supported; commits created through the GitHub API with a GitHub App token are signed by GitHub"},
			"use_github_app_token": {kind: noop, msg: "has no effect: Hephaestus always commits through the GitHub API"},
		}},
		"repository":        {children: repositoryFields},
		"alternative_names": {kind: proOnly},
		"app":               {kind: proOnly},
		"templated_files":   {kind: proOnly},
		"tap":               {kind: removedConv, msg: "was removed in GoReleaser v2 (renamed to repository); read as repository", children: repositoryFields},
		"folder":            {kind: removedConv, msg: "was removed in GoReleaser v2 (renamed to directory); read as directory", tmpl: true},
		"plist":             {kind: removed, msg: "was removed in GoReleaser v2 and Homebrew no longer supports plist blocks; use service"},
	}
)

// perArtifactVars are template fields GoReleaser sets per artifact. In
// install/extra_install they would select per-platform code; Hephaestus
// writes one `def install`, so such templates must render identically for
// every archive.
var perArtifactVars = []string{".Os", ".OS", ".Arch", ".Arm", ".Amd64", ".ArtifactName", ".ArtifactPath", ".ArtifactExt", ".Target"}

// LoadBrews reads project_name and brews from a GoReleaser configuration
// file. Other top-level keys are ignored. The returned entries are not
// defaulted or validated as Hephaestus configuration; callers merge them into
// a config.Config and run its ApplyDefaults and Validate.
//
// Errors cover YAML problems, unknown fields (with a suggestion), GoReleaser
// Pro fields, fields removed in GoReleaser v2 that cannot be converted, and
// template syntax Hephaestus cannot render. Warnings cover deprecations
// (including GoReleaser's own deprecation of brews) and accepted fields that
// behave differently.
func LoadBrews(path string) (projectName string, brews []config.Brew, warnings []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxConfigSize+1))
	if err != nil {
		return "", nil, nil, err
	}
	if len(data) > MaxConfigSize {
		return "", nil, nil, fmt.Errorf("%s: exceeds %d bytes", path, MaxConfigSize)
	}
	projectName, brews, warnings, err = parse(data)
	if err != nil {
		return "", nil, warnings, fmt.Errorf("%s: %w", path, err)
	}
	return projectName, brews, warnings, nil
}

func parse(data []byte) (string, []config.Brew, []string, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return "", nil, nil, errors.New("configuration is empty")
		}
		return "", nil, nil, fmt.Errorf("invalid YAML: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", nil, nil, errors.New("configuration must contain a single YAML document")
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return "", nil, nil, &config.Error{Line: root.Line, Msg: "configuration must be a mapping"}
	}

	l := &loader{}
	if v := mapValue(root, "version"); v == nil {
		l.warn("", 0, "no version key: reading brews with the GoReleaser v2 schema")
	} else if v.Value != "2" {
		l.warn("version", v.Line, "GoReleaser configuration version %s: reading brews with the GoReleaser v2 schema", v.Value)
	}

	var project string
	if v := mapValue(root, "project_name"); v != nil {
		if v.Kind != yaml.ScalarNode {
			l.fail("project_name", v.Line, "must be a string")
		} else {
			project = v.Value
			if strings.Contains(project, "{{") {
				l.fail("project_name", v.Line, "templates are not supported in project_name")
			}
		}
	}
	if v := mapValue(root, "homebrew_casks"); v != nil {
		l.warn("homebrew_casks", v.Line, "is ignored: Hephaestus generates formulae from brews, not casks")
	}

	bn := mapValue(root, "brews")
	if bn == nil || (bn.Kind == yaml.ScalarNode && bn.Tag == "!!null") {
		return project, nil, l.warnings, l.err()
	}
	if bn.Kind != yaml.SequenceNode {
		l.fail("brews", bn.Line, "must be a list")
		return "", nil, l.warnings, l.err()
	}
	if err := rejectAliases(bn); err != nil {
		return "", nil, nil, err
	}
	if len(bn.Content) > 0 {
		l.warn("brews", bn.Line, "is deprecated in GoReleaser (since v2.10, fully since v2.16) in favor of homebrew_casks; Hephaestus keeps generating Homebrew formulae from it")
	}
	var brews []config.Brew
	for i, n := range bn.Content {
		p := fmt.Sprintf("brews[%d]", i)
		if n.Kind != yaml.MappingNode {
			l.fail(p, n.Line, "must be a mapping")
			continue
		}
		l.check(n, brewFields, p)
		l.checkPerArtifact(n, p)
		if len(l.errs) > 0 {
			continue
		}
		var b config.Brew
		if err := n.Decode(&b); err != nil {
			l.fail(p, n.Line, "%v", err)
			continue
		}
		// Convert the removed names here so ApplyDefaults does not warn twice.
		if b.Tap != nil {
			if b.Repository.Owner != "" || b.Repository.Name != "" {
				l.fail(p+".tap", n.Line, "cannot be combined with repository")
				continue
			}
			b.Repository, b.Tap = *b.Tap, nil
		}
		if b.Folder != "" {
			if b.Directory != "" {
				l.fail(p+".folder", n.Line, "cannot be combined with directory")
				continue
			}
			b.Directory, b.Folder = b.Folder, ""
		}
		brews = append(brews, b)
	}
	if err := l.err(); err != nil {
		return "", nil, l.warnings, err
	}
	return project, brews, l.warnings, nil
}

type loader struct {
	errs     []error
	warnings []string
}

func (l *loader) fail(path string, line int, format string, args ...any) {
	l.errs = append(l.errs, &config.Error{Path: path, Line: line, Msg: fmt.Sprintf(format, args...)})
}

func (l *loader) warn(path string, line int, format string, args ...any) {
	l.warnings = append(l.warnings, (&config.Error{Path: path, Line: line, Msg: fmt.Sprintf(format, args...)}).Error())
}

func (l *loader) err() error { return errors.Join(l.errs...) }

// check validates the keys of mapping n against fields.
func (l *loader) check(n *yaml.Node, fields map[string]field, path string) {
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		p := path + "." + k.Value
		if seen[k.Value] {
			l.fail(p, k.Line, "duplicate key")
			continue
		}
		seen[k.Value] = true
		f, ok := fields[k.Value]
		if !ok {
			l.fail(p, k.Line, "unknown field%s", suggest(k.Value, fields))
			continue
		}
		switch f.kind {
		case proOnly:
			l.fail(p, k.Line, "is a GoReleaser Pro only field, not supported")
			continue
		case removed:
			l.fail(p, k.Line, "%s", f.msg)
			continue
		case unsupported:
			l.fail(p, k.Line, "is not supported: %s", f.msg)
			continue
		case noop:
			l.warn(p, k.Line, "%s", f.msg)
			continue
		case removedConv:
			l.warn(p, k.Line, "%s", f.msg)
		}
		l.checkValue(v, f, p)
	}
}

func (l *loader) checkValue(v *yaml.Node, f field, path string) {
	switch v.Kind {
	case yaml.MappingNode:
		if f.children == nil {
			l.fail(path, v.Line, "expected a scalar or a list, not a mapping")
			return
		}
		l.check(v, f.children, path)
	case yaml.SequenceNode:
		for i, c := range v.Content {
			p := fmt.Sprintf("%s[%d]", path, i)
			if c.Kind == yaml.MappingNode && f.children != nil {
				l.check(c, f.children, p)
				continue
			}
			l.checkScalar(c, f, p)
		}
	case yaml.ScalarNode:
		if f.children != nil && !f.scalarOK && v.Tag != "!!null" {
			l.fail(path, v.Line, "expected a mapping")
			return
		}
		l.checkScalar(v, f, path)
	}
}

func (l *loader) checkScalar(v *yaml.Node, f field, path string) {
	if v.Kind != yaml.ScalarNode {
		l.fail(path, v.Line, "expected a scalar")
		return
	}
	switch {
	case f.literal:
		if strings.Contains(v.Value, "{{") {
			l.fail(path, v.Line, "templates are not supported here: Hephaestus uses the value verbatim")
		}
	case f.tmpl:
		if err := tmpl.Check(v.Value); err != nil {
			l.fail(path, v.Line, "template is not supported by Hephaestus: %v (available functions: %s)", err, strings.Join(tmpl.FuncNames(), ", "))
		}
	}
}

// checkPerArtifact warns when install code uses per-artifact template
// variables, which GoReleaser evaluates per platform.
func (l *loader) checkPerArtifact(n *yaml.Node, path string) {
	for _, key := range []string{"install", "extra_install"} {
		v := mapValue(n, key)
		if v == nil || v.Kind != yaml.ScalarNode {
			continue
		}
		for _, name := range perArtifactVars {
			if strings.Contains(v.Value, name) {
				l.warn(path+"."+key, v.Line, "uses the per-artifact variable %s: Hephaestus writes a single def install, so the template must render identically for every archive (branch in Ruby with OS.mac? / Hardware::CPU.arm? instead)", name)
				break
			}
		}
	}
}

func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func rejectAliases(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode {
		return &config.Error{Line: n.Line, Msg: "YAML aliases are not supported in brews"}
	}
	for _, c := range n.Content {
		if err := rejectAliases(c); err != nil {
			return err
		}
	}
	return nil
}

func suggest(key string, fields map[string]field) string {
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	best, bestD := "", 3
	for _, k := range names {
		if d := levenshtein(key, k); d < bestD {
			best, bestD = k, d
		}
	}
	if best != "" {
		return fmt.Sprintf(" (did you mean %q?)", best)
	}
	return ""
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
