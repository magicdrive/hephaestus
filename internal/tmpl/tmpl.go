// Package tmpl renders the Go text/template expressions allowed in Hephaestus
// configuration values.
//
// Rendering is strict: referencing an unknown field or a missing key is an
// error rather than "<no value>", so typos fail loudly at validation time.
// Only a small, side-effect-free function set is available.
package tmpl

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"text/template"
	"text/template/parse"
)

// Vars is the data passed to every template. Fields are documented in
// docs/configuration.md; keep the two in sync.
type Vars struct {
	Project     string // project.name
	ProjectName string // alias of Project (GoReleaser compatibility)
	Version     string // version without a leading "v"
	Tag         string // git tag, e.g. "v1.2.3"
	RawVersion  string // version exactly as supplied
	Major       string
	Minor       string
	Patch       string
	Prerelease  string
	Commit      string
	ShortCommit string
	OS          string // target OS (GOOS-style name), empty outside a target
	Os          string // alias of OS (GoReleaser compatibility)
	Arch        string // target architecture (GOARCH-style name)
	Arm         string // ARM version for arch=arm, if any
	Ext         string // executable extension for the target (".exe" on windows)
	// ArtifactName, ArtifactPath and ArtifactExt describe the artifact a
	// value is rendered for (GoReleaser compatibility); empty elsewhere.
	ArtifactName string // file name, e.g. "mycli_1.2.3_darwin_arm64.tar.gz"
	ArtifactPath string // project-relative path
	ArtifactExt  string // extension including the dot, e.g. ".tar.gz"
	// Env is .Env in templates. Callers leave credentials out of it (see
	// IsCredentialName) because most rendered values end up published.
	Env map[string]string

	// secretEnv is the complete environment, credentials included. It is
	// unexported, and nothing on Vars returns it, so templates cannot reach
	// it: only RenderSecret, for the few values whose purpose is to read a
	// credential, renders with it as .Env.
	secretEnv map[string]string
}

// WithSecretEnv returns a copy of v that carries env, the complete
// environment including credentials, for RenderSecret. It is a function, not
// a method, so templates cannot call it.
func WithSecretEnv(v Vars, env map[string]string) Vars {
	v.secretEnv = env
	return v
}

// secretVars returns a copy of v whose Env is the complete environment set
// with WithSecretEnv, overlaid with v.Env. Without one, v is returned as is.
func secretVars(v Vars) Vars {
	if v.secretEnv == nil {
		return v
	}
	env := make(map[string]string, len(v.secretEnv)+len(v.Env))
	for k, val := range v.secretEnv {
		env[k] = val
	}
	for k, val := range v.Env {
		env[k] = val
	}
	v.Env = env
	return v
}

// Substrings and "_"-separated name segments that mark an environment
// variable name as a credential, and names that match them but are set by
// shells to a directory (PWD is "print working directory", not a password).
var (
	credentialSubstrings = []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "PASSPHRASE", "CREDENTIAL", "PRIVATE_KEY", "API_KEY", "APIKEY", "ACCESS_KEY"}
	credentialSegments   = []string{"PAT", "AUTH", "KEY", "PASS", "PWD"}
	notCredentialNames   = []string{"PWD", "OLDPWD"}
)

// IsCredentialName reports whether an environment variable name looks like
// it holds a credential, ignoring case: it contains TOKEN, SECRET, PASSWORD,
// PASSWD, PASSPHRASE, CREDENTIAL, PRIVATE_KEY, API_KEY, APIKEY or
// ACCESS_KEY, or one of its "_"-separated segments is PAT, AUTH, KEY, PASS
// or PWD (so TAP_PAT, GPG_KEY and DB_PWD match, but PATH and AUTHOR do not).
// The shell variables PWD and OLDPWD are not credentials.
//
// It is a heuristic: a credential under an unusual name is not recognized.
func IsCredentialName(name string) bool {
	u := strings.ToUpper(name)
	if slices.Contains(notCredentialNames, u) {
		return false
	}
	for _, w := range credentialSubstrings {
		if strings.Contains(u, w) {
			return true
		}
	}
	for _, seg := range strings.Split(u, "_") {
		if slices.Contains(credentialSegments, seg) {
			return true
		}
	}
	return false
}

// The secret registry: values (tokens) that must never appear in the output
// of Render, which produces values that are published (formulae, release
// notes, commit messages, pull request titles).
var (
	secretsMu sync.RWMutex
	secrets   []string
)

// RegisterSecret records values that Render must refuse to output. Empty
// values are ignored.
func RegisterSecret(values ...string) {
	secretsMu.Lock()
	defer secretsMu.Unlock()
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" && !slices.Contains(secrets, v) {
			secrets = append(secrets, v)
		}
	}
}

// containsSecret reports whether s contains a registered secret.
func containsSecret(s string) bool {
	secretsMu.RLock()
	defer secretsMu.RUnlock()
	for _, v := range secrets {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}

// ErrSecretInOutput is returned (wrapped) by Render when a rendered value
// contains a registered secret.
var ErrSecretInOutput = errors.New("template output contains a credential")

// FullCommit is an alias of Commit (GoReleaser compatibility). It is a method
// so that every Vars value provides it without extra wiring.
func (v Vars) FullCommit() string { return v.Commit }

// WithTarget returns a copy of v with the target fields set.
func (v Vars) WithTarget(os, arch, arm string) Vars {
	v.OS, v.Os, v.Arch, v.Arm = os, os, arch, arm
	v.Ext = ""
	if os == "windows" {
		v.Ext = ".exe"
	}
	return v
}

var funcs = template.FuncMap{
	"tolower":    strings.ToLower,
	"toupper":    strings.ToUpper,
	"lower":      strings.ToLower,
	"upper":      strings.ToUpper,
	"trim":       strings.TrimSpace,
	"trimprefix": func(s, prefix string) string { return strings.TrimPrefix(s, prefix) },
	"trimsuffix": func(s, suffix string) string { return strings.TrimSuffix(s, suffix) },
	"replace":    func(s, old, new string) string { return strings.ReplaceAll(s, old, new) },
	// urlPathEscape escapes one URL path segment (GoReleaser compatibility).
	"urlPathEscape": url.PathEscape,
	"title": func(s string) string {
		if s == "" {
			return s
		}
		return strings.ToUpper(s[:1]) + s[1:]
	},
}

// FuncNames lists the available template functions, sorted.
func FuncNames() []string {
	names := make([]string, 0, len(funcs))
	for k := range funcs {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// MaxOutput bounds a rendered value; configuration values are short strings.
const MaxOutput = 64 << 10

// Render evaluates s as a template against v. Strings without "{{" are
// returned unchanged without parsing. A missing .Env key is an error, and so
// is an output containing a value registered with RegisterSecret: rendered
// values are published.
func Render(s string, v Vars) (string, error) {
	out, err := render(s, v, "missingkey=error")
	if err != nil {
		return "", err
	}
	if strings.Contains(s, "{{") && containsSecret(out) {
		return "", fmt.Errorf("cannot render template %q: %w", s, ErrSecretInOutput)
	}
	return out, nil
}

// RenderSecret evaluates s like Render, but .Env is the complete environment
// set with WithSecretEnv, credentials included, and the output may contain
// registered secrets. Use it only for values meant to hold a credential
// (brews[].repository.token, builds[].env), never for values that are
// published.
func RenderSecret(s string, v Vars) (string, error) {
	return render(s, secretVars(v), "missingkey=error")
}

// EnvRefs returns the names of the environment variables s reads, as
// .Env.NAME or index .Env "NAME", sorted. A name built at run time is not
// found.
func EnvRefs(s string) ([]string, error) {
	if !strings.Contains(s, "{{") {
		return nil, nil
	}
	t, err := template.New("value").Funcs(funcs).Parse(s)
	if err != nil {
		return nil, fmt.Errorf("invalid template %q: %w", s, cleanErr(err))
	}
	set := map[string]bool{}
	walkNode(t.Tree.Root, func(n parse.Node) {
		switch n := n.(type) {
		case *parse.FieldNode:
			if len(n.Ident) >= 2 && n.Ident[0] == "Env" {
				set[n.Ident[1]] = true
			}
		case *parse.CommandNode:
			// index .Env "NAME"
			if len(n.Args) >= 3 {
				if id, ok := n.Args[0].(*parse.IdentifierNode); ok && id.Ident == "index" {
					if f, ok := n.Args[1].(*parse.FieldNode); ok && len(f.Ident) == 1 && f.Ident[0] == "Env" {
						if s, ok := n.Args[2].(*parse.StringNode); ok {
							set[s.Text] = true
						}
					}
				}
			}
		}
	})
	names := make([]string, 0, len(set))
	for k := range set {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}

// walkNode calls fn for n and every node below it.
func walkNode(n parse.Node, fn func(parse.Node)) {
	if n == nil || reflect.ValueOf(n).IsNil() {
		return
	}
	fn(n)
	switch n := n.(type) {
	case *parse.ListNode:
		for _, c := range n.Nodes {
			walkNode(c, fn)
		}
	case *parse.ActionNode:
		walkNode(n.Pipe, fn)
	case *parse.PipeNode:
		for _, d := range n.Decl {
			walkNode(d, fn)
		}
		for _, c := range n.Cmds {
			walkNode(c, fn)
		}
	case *parse.CommandNode:
		for _, a := range n.Args {
			walkNode(a, fn)
		}
	case *parse.ChainNode:
		walkNode(n.Node, fn)
	case *parse.IfNode:
		walkBranch(&n.BranchNode, fn)
	case *parse.RangeNode:
		walkBranch(&n.BranchNode, fn)
	case *parse.WithNode:
		walkBranch(&n.BranchNode, fn)
	case *parse.TemplateNode:
		walkNode(n.Pipe, fn)
	}
}

func walkBranch(b *parse.BranchNode, fn func(parse.Node)) {
	walkNode(b.Pipe, fn)
	walkNode(b.List, fn)
	walkNode(b.ElseList, fn)
}

// Check parses s and renders it against sample data so typos in field or
// function names surface during configuration validation, before any work
// starts. Environment variables are not required to exist at check time.
func Check(s string) error {
	_, err := render(s, Vars{
		Project: "p", ProjectName: "p", Version: "0.0.0", Tag: "v0.0.0", RawVersion: "v0.0.0",
		Major: "0", Minor: "0", Patch: "0", Commit: "0", ShortCommit: "0",
		OS: "linux", Os: "linux", Arch: "amd64",
		ArtifactName: "p_0.0.0_linux_amd64.tar.gz", ArtifactPath: "dist/p_0.0.0_linux_amd64.tar.gz", ArtifactExt: ".tar.gz",
	}, "missingkey=zero")
	return err
}

func render(s string, v Vars, missing string) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	t, err := template.New("value").Option(missing).Funcs(funcs).Parse(s)
	if err != nil {
		return "", fmt.Errorf("invalid template %q: %w", s, cleanErr(err))
	}
	if v.Env == nil {
		v.Env = map[string]string{}
	}
	var buf limitedBuffer
	if err := t.Execute(&buf, v); err != nil {
		return "", fmt.Errorf("cannot render template %q: %w", s, cleanErr(err))
	}
	return buf.String(), nil
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > MaxOutput {
		return 0, fmt.Errorf("template output exceeds %d bytes", MaxOutput)
	}
	return b.Buffer.Write(p)
}

func cleanErr(err error) error {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "template: value:")
	return fmt.Errorf("%s", strings.TrimSpace(msg))
}
