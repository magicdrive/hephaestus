package homebrew

import (
	"fmt"
	"regexp"
	"strings"
)

// spdxIDRe matches an SPDX license or exception identifier, including
// LicenseRef-/DocumentRef- references and the deprecated "+" suffix.
var spdxIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*\+?$|^(DocumentRef-[A-Za-z0-9.-]+:)?LicenseRef-[A-Za-z0-9.-]+$`)

// licenseDSL converts an SPDX license expression into the arguments of
// Homebrew's license stanza:
//
//	MIT                         -> "MIT"
//	MIT OR Apache-2.0           -> any_of: ["MIT", "Apache-2.0"]
//	MIT AND BSD-3-Clause        -> all_of: ["MIT", "BSD-3-Clause"]
//	Apache-2.0 WITH LLVM-exception -> "Apache-2.0" => { with: "LLVM-exception" }
//
// Nested parenthesized expressions become nested hashes. Only identifier
// characters, parentheses, whitespace and the AND/OR/WITH operators are
// accepted, so the result is always a safe Ruby expression. Homebrew rejects
// an SPDX expression written as a plain string ("MIT OR Apache-2.0"), which is
// why the expression is translated rather than quoted.
func licenseDSL(expr string) (string, error) {
	toks, err := spdxTokens(expr)
	if err != nil {
		return "", err
	}
	p := &spdxParser{toks: toks}
	n, err := p.or()
	if err != nil {
		return "", fmt.Errorf("license %q: %w", expr, err)
	}
	if p.pos != len(p.toks) {
		return "", fmt.Errorf("license %q: unexpected %q", expr, p.toks[p.pos])
	}
	switch n.op {
	case "":
		if n.with != "" {
			return fmt.Sprintf("%q => { with: %q }", n.id, n.with), nil
		}
		return fmt.Sprintf("%q", n.id), nil
	default:
		return n.ruby(true), nil
	}
}

type spdxNode struct {
	op       string // "", "any_of", "all_of"
	id, with string
	kids     []*spdxNode
}

// ruby renders the node; top-level compound nodes are written without braces
// (license any_of: [...]), nested ones as hashes ({ all_of: [...] }).
func (n *spdxNode) ruby(top bool) string {
	switch n.op {
	case "":
		if n.with != "" {
			return fmt.Sprintf("{ %q => { with: %q } }", n.id, n.with)
		}
		return fmt.Sprintf("%q", n.id)
	default:
		parts := make([]string, len(n.kids))
		for i, k := range n.kids {
			parts[i] = k.ruby(false)
		}
		s := fmt.Sprintf("%s: [%s]", n.op, strings.Join(parts, ", "))
		if top {
			return s
		}
		return "{ " + s + " }"
	}
}

func spdxTokens(expr string) ([]string, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, fmt.Errorf("license is empty")
	}
	var toks []string
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for _, r := range expr {
		switch {
		case r == '(' || r == ')':
			flush()
			toks = append(toks, string(r))
		case r == ' ' || r == '\t':
			flush()
		case r < 0x80 && (r == '.' || r == '-' || r == '+' || r == ':' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')):
			cur.WriteRune(r)
		default:
			return nil, fmt.Errorf("license %q contains %q, which is not allowed in an SPDX expression", expr, r)
		}
	}
	flush()
	return toks, nil
}

type spdxParser struct {
	toks []string
	pos  int
}

func (p *spdxParser) peekOp(op string) bool {
	if p.pos >= len(p.toks) {
		return false
	}
	t := p.toks[p.pos]
	return t == op || t == strings.ToLower(op)
}

func (p *spdxParser) or() (*spdxNode, error) {
	return p.binary("OR", "any_of", p.and)
}

func (p *spdxParser) and() (*spdxNode, error) {
	return p.binary("AND", "all_of", p.atom)
}

func (p *spdxParser) binary(op, dsl string, next func() (*spdxNode, error)) (*spdxNode, error) {
	n, err := next()
	if err != nil {
		return nil, err
	}
	if !p.peekOp(op) {
		return n, nil
	}
	out := &spdxNode{op: dsl}
	out.add(n)
	for p.peekOp(op) {
		p.pos++
		k, err := next()
		if err != nil {
			return nil, err
		}
		out.add(k)
	}
	return out, nil
}

// add flattens same-operator children: (A OR B) OR C == any_of [A, B, C].
func (n *spdxNode) add(k *spdxNode) {
	if k.op == n.op {
		n.kids = append(n.kids, k.kids...)
		return
	}
	n.kids = append(n.kids, k)
}

func (p *spdxParser) atom() (*spdxNode, error) {
	if p.pos >= len(p.toks) {
		return nil, fmt.Errorf("unexpected end of expression")
	}
	t := p.toks[p.pos]
	p.pos++
	if t == "(" {
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.pos >= len(p.toks) || p.toks[p.pos] != ")" {
			return nil, fmt.Errorf("missing )")
		}
		p.pos++
		return n, nil
	}
	if isSPDXOperator(t) || t == ")" || !spdxIDRe.MatchString(t) {
		return nil, fmt.Errorf("%q is not a license identifier", t)
	}
	n := &spdxNode{id: t}
	if p.peekOp("WITH") {
		p.pos++
		if p.pos >= len(p.toks) || isSPDXOperator(p.toks[p.pos]) || !spdxIDRe.MatchString(p.toks[p.pos]) {
			return nil, fmt.Errorf("WITH must be followed by an exception identifier")
		}
		n.with = p.toks[p.pos]
		p.pos++
	}
	return n, nil
}

func isSPDXOperator(t string) bool {
	switch t {
	case "AND", "OR", "WITH", "and", "or", "with":
		return true
	}
	return false
}
