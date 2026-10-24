package homebrew

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/magicdrive/hephaestus/internal/config"
)

var rubyConstRe = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]*$`)

// ClassName returns the Ruby class name Homebrew expects for a formula file
// named formulaName + ".rb". It mirrors Formulary.class_s:
//
//	class_name = name.capitalize
//	class_name.gsub!(/[-_.\s][a-zA-Z0-9]/) { |m| m.chars.last.upcase }
//	class_name.tr!("+", "x")
//	class_name.sub!(/(.)@(\d)/, "\\1AT\\2")
//
// so "node@20" becomes "NodeAT20", "python@3.12" "PythonAT312",
// "go-1password" "Go1password" and "c++tool" "Cxxtool".
//
// Names that are not valid formula names, or whose class name would not be a
// Ruby constant (a leading digit such as "7zip", or "a--b"), are rejected:
// Homebrew cannot load them.
func ClassName(formulaName string) (string, error) {
	if !config.FormulaNameRe().MatchString(formulaName) {
		return "", fmt.Errorf("%q is not a valid Homebrew formula name (lowercase letters, digits, +_.- and an optional @version)", formulaName)
	}
	s := capitalize(formulaName)
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c == '-' || c == '_' || c == '.' || isRubySpace(c)) && i+1 < len(s) && isAlnum(s[i+1]) {
			b.WriteByte(upper(s[i+1]))
			i++
			continue
		}
		b.WriteByte(c)
	}
	s = strings.ReplaceAll(b.String(), "+", "x")
	for i := 1; i+1 < len(s); i++ {
		if s[i] == '@' && s[i+1] >= '0' && s[i+1] <= '9' {
			s = s[:i] + "AT" + s[i+1:]
			break
		}
	}
	if !rubyConstRe.MatchString(s) {
		return "", fmt.Errorf("formula name %q yields class name %q, which is not a valid Ruby constant (formula names must start with a letter)", formulaName, s)
	}
	return s, nil
}

// capitalize is Ruby's String#capitalize for ASCII: first character upper
// case, the rest lower case.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	b := []byte(strings.ToLower(s))
	b[0] = upper(b[0])
	return string(b)
}

func upper(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - 'a' + 'A'
	}
	return c
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isRubySpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}
