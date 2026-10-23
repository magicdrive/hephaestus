package hephaestus

import (
	"io"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/magicdrive/hephaestus/internal/build"
	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// redacted replaces a known secret in messages, outputs and the summary.
const redacted = "***"

// Shortest values treated as secrets: a known token, and the value of a
// variable that only looks like a credential by name (a short value there
// is more likely a flag such as "1" or "false" than a secret).
const (
	minSecretLen       = 4
	minLikelySecretLen = 8
)

// redactor replaces known secret values with "***". GitHub masks registered
// secrets in logs, but not in the step summary, outputs or values derived
// from a secret, and not at all outside GitHub Actions.
type redactor struct {
	mu      sync.RWMutex
	secrets []string // longest first
}

// add registers secret values; short or empty values are ignored.
func (r *redactor) add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range values {
		v = strings.TrimSpace(v)
		if len(v) < minSecretLen {
			continue
		}
		dup := false
		for _, s := range r.secrets {
			if s == v {
				dup = true
				break
			}
		}
		if !dup {
			r.secrets = append(r.secrets, v)
		}
	}
	sort.SliceStable(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
}

// addEnviron registers the credentials in environ: the variables named in
// names and build.DefaultScrubEnv, and long values of variables whose name
// looks like a credential.
func (r *redactor) addEnviron(environ []string, names ...string) {
	known := map[string]bool{}
	for _, n := range append(append([]string(nil), build.DefaultScrubEnv...), names...) {
		known[strings.ToUpper(n)] = true
	}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch {
		case known[strings.ToUpper(k)]:
			r.add(v)
		case tmpl.IsCredentialName(k) && len(strings.TrimSpace(v)) >= minLikelySecretLen:
			r.add(v)
		}
	}
}

// redact returns s with every known secret replaced.
func (r *redactor) redact(s string) string {
	if r == nil {
		return s
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, v := range r.secrets {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, redacted)
		}
	}
	return s
}

func (r *redactor) maxLen() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.secrets) == 0 {
		return 0
	}
	return len(r.secrets[0])
}

// writer returns a writer that redacts what it forwards to w. Output is
// held back until a newline, so a secret split across writes is still
// caught; Flush writes what is left. It is safe for concurrent use (build
// commands write stdout and stderr from separate goroutines).
func (r *redactor) writer(w io.Writer) *redactWriter {
	return &redactWriter{r: r, w: w}
}

// redactLineMax bounds how much output without a newline is held back.
const redactLineMax = 64 << 10

type redactWriter struct {
	r   *redactor
	w   io.Writer
	mu  sync.Mutex
	buf []byte
}

func (rw *redactWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.buf = append(rw.buf, p...)
	if i := strings.LastIndexByte(string(rw.buf), '\n'); i >= 0 {
		line := string(rw.buf[:i+1])
		rw.buf = append(rw.buf[:0], rw.buf[i+1:]...)
		if _, err := io.WriteString(rw.w, rw.r.redact(line)); err != nil {
			return 0, err
		}
	}
	if len(rw.buf) > redactLineMax {
		// A very long line: emit all but a tail that could hold the start
		// of a secret. Any secret starting before the tail is complete in
		// the buffer and replaced.
		keep := max(rw.r.maxLen()-1, 0)
		out := rw.r.redact(string(rw.buf))
		if keep > len(out) {
			keep = len(out)
		}
		tail := out[len(out)-keep:]
		if _, err := io.WriteString(rw.w, out[:len(out)-keep]); err != nil {
			return 0, err
		}
		rw.buf = append(rw.buf[:0], tail...)
	}
	return len(p), nil
}

// Flush writes held-back output.
func (rw *redactWriter) Flush() {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if len(rw.buf) > 0 {
		_, _ = io.WriteString(rw.w, rw.r.redact(string(rw.buf))) // nothing to do on a failed final write
		rw.buf = rw.buf[:0]
	}
}

// githubTokenPrefixes start GitHub personal access, OAuth and App tokens.
var githubTokenPrefixes = []string{"ghp_", "github_pat_", "gho_", "ghs_", "ghu_", "ghr_"}

// literalTokens finds GitHub tokens written literally in the configuration
// and returns a warning for each and the values, which are then redacted.
// A token in a configuration file is committed to the repository and
// published to anyone who can read it.
func literalTokens(cfg *config.Config) (warnings, values []string) {
	walkStrings(reflect.ValueOf(cfg).Elem(), "", func(path, s string) {
		v := s
		if path == "env" || strings.HasPrefix(path, "env[") {
			_, v, _ = strings.Cut(s, "=")
		}
		v = strings.TrimSpace(v)
		for _, p := range githubTokenPrefixes {
			if strings.HasPrefix(v, p) && len(v) > len(p)+8 && !strings.Contains(v, "{{") {
				warnings = append(warnings, path+" looks like a GitHub token written literally in the configuration: revoke it, store it as a secret and reference it as {{ .Env.NAME }}")
				values = append(values, v)
				return
			}
		}
	})
	for i, b := range cfg.Brews {
		if t := strings.TrimSpace(b.Repository.Token); t != "" && !strings.Contains(t, "{{") {
			w := "brews[" + strconv.Itoa(i) + "].repository.token is a literal value: reference an environment variable ({{ .Env.NAME }}) instead of writing a token in the configuration"
			if !slices.Contains(warnings, w) {
				warnings = append(warnings, w)
			}
			values = append(values, t)
		}
	}
	return warnings, values
}

// walkStrings calls fn for every string reachable from v, with a YAML-like
// path.
func walkStrings(v reflect.Value, path string, fn func(path, s string)) {
	switch v.Kind() {
	case reflect.String:
		fn(path, v.String())
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			walkStrings(v.Elem(), path, fn)
		}
	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name == "-" {
				continue
			}
			p := path
			if name != "" {
				if p != "" {
					p += "."
				}
				p += name
			}
			walkStrings(v.Field(i), p, fn)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			walkStrings(v.Index(i), path+"["+strconv.Itoa(i)+"]", fn)
		}
	case reflect.Map:
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		for _, k := range keys {
			walkStrings(v.MapIndex(k), path+"."+k.String(), fn)
		}
	}
}
