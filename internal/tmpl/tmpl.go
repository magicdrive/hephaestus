// Package tmpl renders the Go text/template expressions allowed in Hephaestus
// configuration values.
//
// Rendering is strict: referencing an unknown field or a missing key is an
// error rather than "<no value>", so typos fail loudly at validation time.
// Only a small, side-effect-free function set is available.
package tmpl

import (
	"bytes"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"text/template"
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
	// unexported, so templates cannot reach it; Secret exposes it as .Env
	// for the few values whose purpose is to read a credential.
	secretEnv map[string]string
}

// WithSecretEnv returns a copy of v that carries env, the complete
// environment including credentials, for Secret.
func (v Vars) WithSecretEnv(env map[string]string) Vars {
	v.secretEnv = env
	return v
}

// Secret returns a copy of v whose Env is the complete environment set with
// WithSecretEnv, overlaid with v.Env. Use it only to render values meant to
// hold a credential (brews[].repository.token, builds[].env), never values
// that are published.
func (v Vars) Secret() Vars {
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

// credentialWords mark an environment variable name as a credential.
var credentialWords = []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "PRIVATE_KEY", "CREDENTIAL"}

// IsCredentialName reports whether an environment variable name looks like
// it holds a credential: it contains TOKEN, SECRET, PASSWORD, PASSWD,
// PRIVATE_KEY or CREDENTIAL, ignoring case.
func IsCredentialName(name string) bool {
	u := strings.ToUpper(name)
	for _, w := range credentialWords {
		if strings.Contains(u, w) {
			return true
		}
	}
	return false
}

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
// returned unchanged without parsing. A missing .Env key is an error.
func Render(s string, v Vars) (string, error) {
	return render(s, v, "missingkey=error")
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
