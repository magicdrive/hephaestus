package homebrew

import (
	"strings"
)

// This file holds a deliberately small Ruby lexer used only to find install
// calls with literal arguments. It is not a Ruby parser: anything it does not
// understand becomes an "other" token, which makes the surrounding call
// unverifiable (reported as a warning) rather than misread.

type tokKind int

const (
	tkNL     tokKind = iota // end of a logical line (or ";")
	tkIdent                 // identifier, keyword, constant, @ivar, $gvar
	tkString                // string literal; dyn reports interpolation
	tkWords                 // %w[] / %i[] word array
	tkPunct                 // . , ( ) [ ] { } => :: ! && || / and other operators
	tkOther                 // numbers, symbols, heredocs, unknown literals
)

type token struct {
	kind  tokKind
	text  string   // identifier / punctuation text, or the string value
	words []string // tkWords
	dyn   bool     // tkString/tkWords: contains interpolation or escapes we do not decode
	line  int      // 1-based
}

func lexRuby(src string) []token {
	l := &lexer{src: src, line: 1}
	l.run()
	return l.toks
}

type lexer struct {
	src      string
	pos      int
	line     int
	toks     []token
	heredocs []heredocSpec
}

type heredocSpec struct {
	id       string
	indented bool
}

func (l *lexer) emit(k tokKind, text string) {
	l.toks = append(l.toks, token{kind: k, text: text, line: l.line})
}

func (l *lexer) peek(off int) byte {
	if l.pos+off < len(l.src) {
		return l.src[l.pos+off]
	}
	return 0
}

func isIdentStart(c byte) bool { return c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z') || c >= 0x80 }
func isIdentChar(c byte) bool  { return isIdentStart(c) || (c >= '0' && c <= '9') }

func (l *lexer) run() {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			l.pos++
		case c == '\\' && l.peek(1) == '\n':
			l.pos += 2
			l.line++
		case c == '\n' || c == ';':
			l.emit(tkNL, "")
			l.pos++
			if c == '\n' {
				l.line++
				l.skipHeredocBodies()
			}
		case c == '#':
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
		case c == '"' || c == '`':
			l.pos++
			s, dyn := l.readQuoted(c, true)
			l.toks = append(l.toks, token{kind: tkString, text: s, dyn: dyn || c == '`', line: l.line})
		case c == '\'':
			l.pos++
			s, _ := l.readQuoted('\'', false)
			l.toks = append(l.toks, token{kind: tkString, text: s, line: l.line})
		case c == '%' && (l.peek(1) == 'w' || l.peek(1) == 'W' || l.peek(1) == 'i' || l.peek(1) == 'I') && closer(l.peek(2)) != 0:
			interp := l.peek(1) == 'W' || l.peek(1) == 'I'
			open := l.peek(2)
			l.pos += 3
			body := l.readDelimited(open, closer(open))
			t := token{kind: tkWords, words: strings.Fields(body), line: l.line}
			t.dyn = (interp && strings.Contains(body, "#")) || strings.Contains(body, `\`)
			l.toks = append(l.toks, t)
		case c == '<' && l.peek(1) == '<' && l.heredocStart():
		case isIdentStart(c) || ((c == '@' || c == '$') && isIdentStart(l.peek(1))):
			start := l.pos
			l.pos++
			for l.pos < len(l.src) && (isIdentChar(l.src[l.pos]) || l.src[l.pos] == '@') {
				l.pos++
			}
			if l.pos < len(l.src) && (l.src[l.pos] == '?' || l.src[l.pos] == '!') && l.peek(1) != '=' {
				l.pos++
			}
			l.emit(tkIdent, l.src[start:l.pos])
		case c >= '0' && c <= '9':
			for l.pos < len(l.src) {
				d := l.src[l.pos]
				if isIdentChar(d) || (d == '.' && l.peek(1) >= '0' && l.peek(1) <= '9') {
					l.pos++
					continue
				}
				break
			}
			l.emit(tkOther, "number")
		case c == ':' && l.peek(1) == ':':
			l.emit(tkPunct, "::")
			l.pos += 2
		case c == ':' && (isIdentStart(l.peek(1)) || l.peek(1) == '"'):
			l.pos++
			if l.src[l.pos] == '"' {
				l.pos++
				l.readQuoted('"', true)
			} else {
				for l.pos < len(l.src) && (isIdentChar(l.src[l.pos]) || l.src[l.pos] == '?' || l.src[l.pos] == '!') {
					l.pos++
				}
			}
			l.emit(tkOther, "symbol")
		case c == '=' && l.peek(1) == '>':
			l.emit(tkPunct, "=>")
			l.pos += 2
		case (c == '&' && l.peek(1) == '&') || (c == '|' && l.peek(1) == '|'):
			l.emit(tkPunct, l.src[l.pos:l.pos+2])
			l.pos += 2
		default:
			l.emit(tkPunct, string(c))
			l.pos++
		}
	}
	l.emit(tkNL, "")
}

func closer(open byte) byte {
	switch open {
	case '[':
		return ']'
	case '(':
		return ')'
	case '{':
		return '}'
	case '<':
		return '>'
	case '|', '!', '/':
		return open
	}
	return 0
}

// readDelimited reads up to the matching close delimiter, honoring nesting.
func (l *lexer) readDelimited(open, close byte) string {
	depth := 1
	start := l.pos
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == '\\':
			l.pos++
		case c == '\n':
			l.line++
		case c == close && open != close:
			depth--
		case c == open && open != close:
			depth++
		case c == close:
			depth = 0
		}
		l.pos++
		if depth == 0 {
			return l.src[start : l.pos-1]
		}
	}
	return l.src[start:]
}

// readQuoted reads a string body after the opening quote. For interpolating
// strings, "#{", "#@" and "#$" and escapes other than \\ \" \' \# set dyn.
func (l *lexer) readQuoted(q byte, interp bool) (string, bool) {
	var b strings.Builder
	dyn := false
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == q:
			l.pos++
			return b.String(), dyn
		case c == '\\' && l.pos+1 < len(l.src):
			n := l.src[l.pos+1]
			switch {
			case n == '\\' || n == q:
				b.WriteByte(n)
			case interp && (n == '#' || n == '"' || n == '\''):
				b.WriteByte(n)
			case !interp:
				b.WriteByte('\\')
				b.WriteByte(n)
			default:
				dyn = true
				b.WriteByte(n)
			}
			if n == '\n' {
				l.line++
			}
			l.pos += 2
		case interp && c == '#' && l.pos+1 < len(l.src) && (l.src[l.pos+1] == '{' || l.src[l.pos+1] == '@' || l.src[l.pos+1] == '$'):
			dyn = true
			if l.src[l.pos+1] == '{' {
				l.pos += 2
				l.readDelimited('{', '}')
			} else {
				l.pos++
			}
		default:
			if c == '\n' {
				l.line++
			}
			b.WriteByte(c)
			l.pos++
		}
	}
	return b.String(), dyn
}

// heredocStart recognizes <<~ID, <<-ID and <<ID (optionally quoted) and
// queues the body to be skipped after the current line.
func (l *lexer) heredocStart() bool {
	m := heredocRe.FindStringSubmatchIndex(l.src[l.pos:])
	if m == nil || m[0] != 0 {
		return false
	}
	sub := func(i int) string {
		if m[2*i] < 0 {
			return ""
		}
		return l.src[l.pos+m[2*i] : l.pos+m[2*i+1]]
	}
	if sub(2) != sub(4) {
		return false
	}
	l.heredocs = append(l.heredocs, heredocSpec{id: sub(3), indented: sub(1) != ""})
	l.pos += m[1]
	l.emit(tkOther, "heredoc")
	return true
}

func (l *lexer) skipHeredocBodies() {
	for len(l.heredocs) > 0 {
		h := l.heredocs[0]
		l.heredocs = l.heredocs[1:]
		for l.pos < len(l.src) {
			end := strings.IndexByte(l.src[l.pos:], '\n')
			var line string
			if end < 0 {
				line = l.src[l.pos:]
				l.pos = len(l.src)
			} else {
				line = l.src[l.pos : l.pos+end]
				l.pos += end + 1
				l.line++
			}
			if (h.indented && strings.TrimSpace(line) == h.id) || line == h.id {
				break
			}
		}
	}
}
