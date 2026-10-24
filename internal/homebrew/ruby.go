package homebrew

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"text/template"
	"text/template/parse"
	"unicode"
	"unicode/utf8"

	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// RubyString returns s as a Ruby double-quoted string literal whose value is
// exactly s. Backslashes, double quotes, interpolation sequences (#{, #@, #$),
// control characters and invisible formatting characters (including the
// bidirectional overrides used in "Trojan Source" attacks) are escaped, so the
// literal can never terminate early, interpolate, or hide code.
//
// s must be valid UTF-8: generated formulae are UTF-8 source files and an
// invalid byte sequence is a Ruby syntax error.
func RubyString(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", errors.New("value is not valid UTF-8")
	}
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	writeRubyEscaped(&b, s, true)
	b.WriteByte('"')
	return b.String(), nil
}

// writeRubyEscaped writes s escaped for the body of an interpolating Ruby
// literal (a double-quoted string, or a squiggly heredoc when quote is false).
func writeRubyEscaped(b *strings.Builder, s string, quote bool) {
	for i, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"' && quote:
			b.WriteString(`\"`)
		case r == '#':
			if i+1 < len(s) && (s[i+1] == '{' || s[i+1] == '@' || s[i+1] == '$') {
				b.WriteString(`\#`)
			} else {
				b.WriteByte('#')
			}
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case needsUnicodeEscape(r):
			fmt.Fprintf(b, `\u{%x}`, r)
		default:
			b.WriteRune(r)
		}
	}
}

// needsUnicodeEscape reports characters that must not appear literally in
// generated source: C0/C1 controls, DEL, format characters (bidi overrides,
// zero-width characters, BOM) and line/paragraph separators.
func needsUnicodeEscape(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) ||
		unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029
}

// rubyHeredoc renders text as a squiggly heredoc (<<~EOS) whose value is the
// text's lines joined by newlines plus a trailing newline. Each line is
// escaped like a double-quoted string body; the terminator is chosen so that
// no content line can end the heredoc early. indent is the indentation of
// the "<<~" line's owner; content lines get two more spaces.
func rubyHeredoc(text, indent string) (string, error) {
	if !utf8.ValidString(text) {
		return "", errors.New("value is not valid UTF-8")
	}
	lines := strings.Split(text, "\n")
	escaped := make([]string, len(lines))
	for i, l := range lines {
		var b strings.Builder
		writeRubyEscaped(&b, strings.TrimRight(l, " "), false)
		escaped[i] = b.String()
	}
	term := "EOS"
	for n := 1; ; n++ {
		clash := false
		for _, l := range escaped {
			if strings.TrimSpace(l) == term {
				clash = true
				break
			}
		}
		if !clash {
			break
		}
		term = fmt.Sprintf("EOS%d", n)
	}
	var b strings.Builder
	b.WriteString("<<~" + term + "\n")
	for _, l := range escaped {
		if l == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(indent + "  " + l + "\n")
	}
	b.WriteString(indent + term)
	return b.String(), nil
}

// normalizeText converts CRLF to LF, drops leading and trailing blank lines,
// trims trailing whitespace and removes the indentation common to all
// non-blank lines.
func normalizeText(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	prefix := ""
	first := true
	for _, l := range lines {
		if l == "" {
			continue
		}
		ws := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
		if first {
			prefix, first = ws, false
			continue
		}
		for !strings.HasPrefix(ws, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(l, prefix)
	}
	return lines
}

var (
	heredocRe = regexp.MustCompile("<<([~-]?)([\"'`]?)([A-Za-z_][A-Za-z0-9_]*)([\"'`]?)")
	endRe     = regexp.MustCompile(`^__END__\s*$`)
)

// codeBlock validates user-supplied Ruby code (install, test, service, ...)
// after template rendering and returns it re-indented with indent. The code
// itself is trusted configuration, as in GoReleaser, but constructs that
// would silently change meaning once embedded and re-indented are rejected:
// NUL and other control characters, invisible formatting characters, __END__
// (which would truncate the formula), =begin/=end documentation blocks and
// plain <<ID heredocs (both are column-sensitive), and unterminated heredocs
// (which would swallow the rest of the formula).
func codeBlock(field, code, indent string) (string, []string, error) {
	var warnings []string
	if !utf8.ValidString(code) {
		return "", nil, fmt.Errorf("%s: code is not valid UTF-8", field)
	}
	code = strings.ReplaceAll(code, "\r\n", "\n")
	for _, r := range code {
		if r == '\n' || r == '\t' {
			continue
		}
		if needsUnicodeEscape(r) {
			return "", nil, fmt.Errorf("%s: code contains the control or invisible character %U", field, r)
		}
	}
	lines := normalizeText(code)
	if len(lines) == 0 {
		return "", nil, nil
	}
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if endRe.MatchString(t) {
			return "", nil, fmt.Errorf("%s: line %d: __END__ is not allowed (it would end the formula source)", field, i+1)
		}
		if strings.HasPrefix(t, "=begin") || strings.HasPrefix(t, "=end") {
			return "", nil, fmt.Errorf("%s: line %d: =begin/=end blocks are not allowed (they depend on column 0); use # comments", field, i+1)
		}
		if strings.HasPrefix(t, "#") {
			continue
		}
		for _, m := range heredocRe.FindAllStringSubmatch(lines[i], -1) {
			kind, open, id, closeQ := m[1], m[2], m[3], m[4]
			if open != closeQ {
				continue // not a heredoc opener, e.g. "a <<b" inside other syntax
			}
			switch kind {
			case "":
				return "", nil, fmt.Errorf("%s: line %d: heredoc <<%s must be written as <<~%s (a plain heredoc depends on column 0 and breaks when the block is indented)", field, i+1, id, id)
			case "-":
				warnings = append(warnings, fmt.Sprintf("%s: line %d: the body of heredoc <<-%s is re-indented; use <<~%s to keep its content unchanged", field, i+1, id, id))
			}
			found := false
			for j := i + 1; j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) == id {
					found = true
					break
				}
			}
			if !found {
				return "", nil, fmt.Errorf("%s: line %d: heredoc %s is not terminated inside the block", field, i+1, id)
			}
		}
	}
	var b strings.Builder
	for i, l := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		if l != "" {
			b.WriteString(indent + l)
		}
	}
	return b.String(), warnings, nil
}

// safeCodeValueRe is the alphabet allowed for template values substituted into
// Ruby code. It excludes every character that can end a string literal,
// start an interpolation, a comment, a heredoc or a command, or a new line.
var safeCodeValueRe = regexp.MustCompile(`^[A-Za-z0-9._+\-/@:~=,%&?]*$`)

// renderCode renders a code template, after checking that every template
// variable it references holds a value from the safe alphabet. The values
// themselves (project name, version, tag, environment) are data, so a value
// that could change the meaning of the surrounding Ruby is an error rather
// than something to escape: the template author decides the quoting context.
func renderCode(field, src string, v tmpl.Vars) (string, error) {
	if !strings.Contains(src, "{{") {
		return src, nil
	}
	if err := checkCodeVars(src, v); err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	out, err := tmpl.Render(src, v)
	if err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	return out, nil
}

func checkCodeVars(src string, v tmpl.Vars) error {
	fm := template.FuncMap{}
	for _, n := range tmpl.FuncNames() {
		fm[n] = func(...any) string { return "" }
	}
	t, err := template.New("code").Funcs(fm).Parse(src)
	if err != nil {
		return fmt.Errorf("invalid template: %w", err)
	}
	refs := &varRefs{env: map[string]bool{}}
	for _, tt := range t.Templates() {
		if tt.Tree != nil {
			refs.walk(tt.Tree.Root)
		}
	}
	if refs.err != nil {
		return refs.err
	}
	check := func(name, val string) error {
		if !safeCodeValueRe.MatchString(val) {
			return fmt.Errorf("template value .%s = %q cannot be substituted into Ruby code: only letters, digits and ._+-/@:~=,%%&? are allowed", name, val)
		}
		return nil
	}
	rv := reflect.ValueOf(v)
	if refs.all {
		for i := 0; i < rv.NumField(); i++ {
			f := rv.Type().Field(i)
			if f.Type.Kind() == reflect.String {
				if err := check(f.Name, rv.Field(i).String()); err != nil {
					return err
				}
			}
		}
	}
	for _, name := range refs.fields {
		val, ok := varValue(rv, name)
		if !ok {
			continue // an unknown field; Render reports it
		}
		if err := check(name, val); err != nil {
			return err
		}
	}
	for k := range refs.env {
		if val, ok := v.Env[k]; ok {
			if err := check("Env."+k, val); err != nil {
				return err
			}
		}
	}
	return nil
}

func varValue(rv reflect.Value, name string) (string, bool) {
	if f := rv.FieldByName(name); f.IsValid() && f.Kind() == reflect.String {
		return f.String(), true
	}
	if m := rv.MethodByName(name); m.IsValid() && m.Type().NumIn() == 0 && m.Type().NumOut() == 1 && m.Type().Out(0).Kind() == reflect.String {
		return m.Call(nil)[0].String(), true
	}
	return "", false
}

// varRefs collects the template fields a code template reads. A bare "." (as
// in {{ with .Version }}{{ . }}{{ end }}) may stand for any field, so it
// marks every field as referenced. Environment access must name the key
// literally so that only that value is validated.
type varRefs struct {
	fields []string
	env    map[string]bool
	all    bool
	err    error
}

func (r *varRefs) walk(n parse.Node) {
	if n == nil || reflect.ValueOf(n).IsNil() {
		return
	}
	switch n := n.(type) {
	case *parse.ListNode:
		for _, c := range n.Nodes {
			r.walk(c)
		}
	case *parse.ActionNode:
		r.walk(n.Pipe)
	case *parse.IfNode:
		r.walk(n.Pipe)
		r.walk(n.List)
		r.walk(n.ElseList)
	case *parse.RangeNode:
		r.walk(n.Pipe)
		r.walk(n.List)
		r.walk(n.ElseList)
	case *parse.WithNode:
		r.walk(n.Pipe)
		r.walk(n.List)
		r.walk(n.ElseList)
	case *parse.TemplateNode:
		r.walk(n.Pipe)
	case *parse.PipeNode:
		for _, c := range n.Cmds {
			r.walk(c)
		}
	case *parse.CommandNode:
		// index .Env "KEY"
		if len(n.Args) == 3 {
			if id, ok := n.Args[0].(*parse.IdentifierNode); ok && id.Ident == "index" {
				if f, ok := n.Args[1].(*parse.FieldNode); ok && len(f.Ident) == 1 && f.Ident[0] == "Env" {
					if s, ok := n.Args[2].(*parse.StringNode); ok {
						r.env[s.Text] = true
						return
					}
				}
			}
		}
		for _, c := range n.Args {
			r.walk(c)
		}
	case *parse.FieldNode:
		r.ident(n.Ident)
	case *parse.VariableNode:
		if len(n.Ident) > 1 && n.Ident[0] == "$" {
			r.ident(n.Ident[1:])
		} else {
			r.all = true // a variable may hold any value
		}
	case *parse.ChainNode:
		r.walk(n.Node)
	case *parse.DotNode:
		r.all = true
	}
}

func (r *varRefs) ident(id []string) {
	if len(id) == 0 {
		return
	}
	if id[0] == "Env" {
		if len(id) >= 2 {
			r.env[id[1]] = true
		} else if r.err == nil {
			r.err = errors.New(`code templates must reference environment variables as .Env.NAME or (index .Env "NAME") so the value can be validated`)
		}
		return
	}
	r.fields = append(r.fields, id[0])
}
