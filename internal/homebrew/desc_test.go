package homebrew

import (
	"strings"
	"testing"
)

func TestLintDesc(t *testing.T) {
	tests := []struct {
		desc, want string // want "" means no warning
	}{
		{"Friendly tool for managing widgets", ""},
		{"macOS menu bar helper", ""},
		{"Tool for this, that, etc.", ""},
		{"", "empty"},
		{" Leading space", "whitespace"},
		{"An example tool", "article"},
		{"the tool", "article"},
		{"lowercase start", "capital"},
		{"Mycli does things", "formula name"},
		{"My-cli does things", "formula name"},
		{"My cli does things", "formula name"},
		{"Ends with a period.", "full stop"},
		{"Fast command line tool", "command-line"},
		{"Fast commandline tool", "command-line"},
		{"Rocket tool 🚀", "emoji"},
		{strings.Repeat("A", 81), "81 characters"},
		{"Two\nlines", "single line"},
	}
	for _, tt := range tests {
		got := strings.Join(lintDesc(tt.desc, "mycli"), "; ")
		if tt.want == "" {
			if got != "" {
				t.Errorf("lintDesc(%q) = %s, want no warning", tt.desc, got)
			}
			continue
		}
		if !strings.Contains(got, tt.want) {
			t.Errorf("lintDesc(%q) = %q, want %q", tt.desc, got, tt.want)
		}
	}
	if w := lintDesc(strings.Repeat("A", 80), "x"); len(w) != 0 {
		t.Errorf("80 characters must be accepted: %v", w)
	}
}
