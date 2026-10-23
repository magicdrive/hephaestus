package glob

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

func TestMatch(t *testing.T) {
	tests := []struct {
		pat  string
		dot  bool
		name string
		want bool
	}{
		{"README.md", false, "README.md", true},
		{"README.md", false, "docs/README.md", false},
		{"*.md", false, "a.md", true},
		{"*.md", false, "docs/a.md", false},
		{"docs/**/*.md", false, "docs/a.md", true},
		{"docs/**/*.md", false, "docs/x/y/a.md", true},
		{"docs/**/*.md", false, "docsx/a.md", false},
		{"docs/**", false, "docs/a/b", true},
		{"**/*.go", false, "a.go", true},
		{"**/*.go", false, "x/.hidden/a.go", false},
		{"**/*.go", true, "x/.hidden/a.go", true},
		{"*", false, ".env", false},
		{".*", false, ".env", true},
		{"*", true, ".env", true},
		{"{README,LICENSE}*", false, "LICENSE.txt", true},
		{"{README,LICENSE}*", false, "CHANGELOG", false},
		{"bin/[ab]?", false, "bin/a1", true},
		{"bin/[!ab]?", false, "bin/a1", false},
		{"docs/internal/**", true, "docs/internal/x/y.md", true},
	}
	for _, tt := range tests {
		p, err := Compile(tt.pat, Options{Dot: tt.dot})
		if err != nil {
			t.Fatalf("Compile(%q): %v", tt.pat, err)
		}
		if got := p.Match(tt.name); got != tt.want {
			t.Errorf("%q.Match(%q) = %v, want %v", tt.pat, tt.name, got, tt.want)
		}
	}
}

func TestCompileErrors(t *testing.T) {
	for _, pat := range []string{
		"", "/abs/*", `a\b`, "a/../b", "./a", "a//b", "a/", "a**b", "{a,b", "a}b",
		"{a,{b}}", "[", "**/**/**/**/**", "{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}",
	} {
		if _, err := Compile(pat, Options{}); err == nil {
			t.Errorf("Compile(%q) succeeded, want error", pat)
		}
	}
}

func TestBase(t *testing.T) {
	tests := map[string]string{
		"docs/**/*.md":       "docs",
		"README.md":          "",
		"dist/linux/x/mycli": "dist/linux/x",
		"{a,b}/x":            "",
		"a/{b,c}/x":          "a",
		"a/b/*":              "a/b",
	}
	for pat, want := range tests {
		if got := MustCompile(pat, Options{}).Base(); got != want {
			t.Errorf("Base(%q) = %q, want %q", pat, got, want)
		}
	}
}

func TestCouldMatchUnder(t *testing.T) {
	p := MustCompile("docs/**/*.md", Options{})
	for dir, want := range map[string]bool{"docs": true, "docs/a/b": true, "src": false, "docs/.git": false} {
		if got := p.CouldMatchUnder(dir); got != want {
			t.Errorf("CouldMatchUnder(%q) = %v, want %v", dir, got, want)
		}
	}
	if MustCompile("a/b", Options{}).CouldMatchUnder("a/b") {
		t.Error("literal file path cannot have matches below it")
	}
}

func TestWalk(t *testing.T) {
	fsys := fstest.MapFS{
		"docs/a.md":         {},
		"docs/x/b.md":       {},
		"docs/x/c.txt":      {},
		"docs/.secret/d.md": {},
		"docs/link.md":      {Mode: fs.ModeSymlink},
		"src/e.md":          {},
	}
	got, err := Walk(fsys, MustCompile("docs/**/*.md", Options{}), 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docs/a.md", "docs/link.md", "docs/x/b.md"}
	if len(got) != len(want) {
		t.Fatalf("Walk = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].Path != want[i] {
			t.Errorf("Walk[%d] = %q, want %q", i, got[i].Path, want[i])
		}
	}
	if got[1].Type&fs.ModeSymlink == 0 {
		t.Error("symlink type not reported")
	}
	if _, err := Walk(fsys, MustCompile("**/*.md", Options{}), 2); err != ErrTooManyMatches {
		t.Errorf("limit: err = %v", err)
	}
	none, err := Walk(fsys, MustCompile("missing/**", Options{}), 10)
	if err != nil || len(none) != 0 {
		t.Errorf("missing base: %v %v", none, err)
	}
}

func FuzzCompileMatch(f *testing.F) {
	f.Add("docs/**/*.md", "docs/a/b.md")
	f.Add("{a,b}/[x-z]*", "a/y1")
	f.Fuzz(func(t *testing.T, pat, name string) {
		p, err := Compile(pat, Options{})
		if err != nil {
			return
		}
		_ = p.Match(name)
		_ = p.CouldMatchUnder(name)
		_ = p.Base()
	})
}
