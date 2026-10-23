package homebrew

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxDescLength is Homebrew's limit for desc (rubocops/shared/desc_helper.rb).
const MaxDescLength = 80

var (
	descArticleRe     = regexp.MustCompile(`(?i)^(?:the|an?)\s`)
	descCommandLineRe = regexp.MustCompile(`(?i)command ?line`)
)

// lintDesc reports violations of Homebrew's desc rules (FormulaAudit/Desc).
// They are warnings: the formula is still generated, but `brew audit` and
// `brew style` will flag it.
func lintDesc(desc, formulaName string) []string {
	var w []string
	add := func(format string, args ...any) {
		w = append(w, "desc: "+fmt.Sprintf(format, args...))
	}
	if desc == "" {
		add("is empty; Homebrew requires a description")
		return w
	}
	if strings.ContainsAny(desc, "\r\n") {
		add("must be a single line")
	}
	if strings.TrimSpace(desc) != desc {
		add("has leading or trailing whitespace")
	}
	if descArticleRe.MatchString(desc) {
		add("should not start with an article")
	}
	if desc[0] >= 'a' && desc[0] <= 'z' && !validLowercaseFirstWord(desc) {
		add("should start with a capital letter")
	}
	if formulaName != "" {
		chars := []string{}
		for _, r := range strings.ReplaceAll(formulaName, "-", "") {
			chars = append(chars, regexp.QuoteMeta(string(r)))
		}
		if re, err := regexp.Compile(`(?i)^` + strings.Join(chars, `[\s\-]?`) + `\b`); err == nil && re.MatchString(desc) {
			add("should not start with the formula name")
		}
	}
	if strings.HasSuffix(desc, ".") && !strings.HasSuffix(desc, "etc.") {
		add("should not end with a full stop")
	}
	if descCommandLineRe.MatchString(desc) {
		add(`write "command-line" instead of "command line"`)
	}
	for _, r := range desc {
		if unicode.Is(unicode.So, r) {
			add("should not contain emoji or symbols (%q)", r)
			break
		}
	}
	if n := utf8.RuneCountInString(desc); n > MaxDescLength {
		add("is %d characters long; Homebrew allows at most %d", n, MaxDescLength)
	}
	return w
}

// validLowercaseFirstWord mirrors VALID_LOWERCASE_WORDS in desc_helper.rb.
func validLowercaseFirstWord(desc string) bool {
	first := strings.Fields(desc)
	if len(first) == 0 {
		return false
	}
	switch first[0] {
	case "iOS", "iPhone", "macOS":
		return true
	}
	return false
}
