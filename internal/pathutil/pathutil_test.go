package pathutil

import (
	"strings"
	"testing"
)

func TestValidateRelative(t *testing.T) {
	good := []string{"a", "a/b", "bin/mycli", "docs/man/mycli.1", "日本語/ファイル.txt", ".github/x"}
	for _, p := range good {
		if err := ValidateRelative(p); err != nil {
			t.Errorf("ValidateRelative(%q) = %v", p, err)
		}
	}
	bad := []string{
		"", "/etc/passwd", "../x", "a/../../x", "a/./b", ".", "..", "a//b", "a/",
		`a\b`, `..\x`, "C:/x", "c:x", "a\x00b", "a\nb", "a\x7f", "a:b", "a/b?", "a*",
		"con. ", "x/y.", "\xff", strings.Repeat("a", MaxPathLength+1), strings.Repeat("b", MaxSegmentLength+1),
	}
	for _, p := range bad {
		if err := ValidateRelative(p); err == nil {
			t.Errorf("ValidateRelative(%q) accepted", p)
		}
	}
}

func TestHelpers(t *testing.T) {
	if Join("", "a") != "a" || Join("d/", "a") != "d/a" || Join("d", "") != "d" {
		t.Error("Join")
	}
	if Base("a/b/c") != "c" || Base("c") != "c" {
		t.Error("Base")
	}
	got := Parents("a/b/c")
	if len(got) != 2 || got[0] != "a" || got[1] != "a/b" {
		t.Errorf("Parents = %v", got)
	}
}

func FuzzValidateRelative(f *testing.F) {
	for _, s := range []string{"a/b", "../x", "/abs", "a\\b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if ValidateRelative(p) != nil {
			return
		}
		// Anything accepted must be unable to escape its base directory.
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." || seg == "." || seg == "" {
				t.Fatalf("accepted escaping path %q", p)
			}
		}
		if strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
			t.Fatalf("accepted unsafe path %q", p)
		}
	})
}
