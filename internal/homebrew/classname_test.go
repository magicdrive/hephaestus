package homebrew

import (
	"regexp"
	"strings"
	"testing"
)

func TestClassName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"hephaestus", "Hephaestus"},
		{"foo-bar", "FooBar"},
		{"my_tool", "MyTool"},
		{"foo.bar", "FooBar"},
		{"node@20", "NodeAT20"},
		{"python@3.12", "PythonAT312"},
		{"go-1password", "Go1password"},
		{"foo-2fa", "Foo2fa"},
		{"a+b", "Axb"},
		{"c++tool", "Cxxtool"},
		{"x264", "X264"},
		{"openssl@3", "OpensslAT3"},
		{"lib-a.b_c", "LibABC"},
	}
	for _, tt := range tests {
		got, err := ClassName(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("ClassName(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	for _, bad := range []string{"", "7zip", "1password", "Foo", "a--b", "a b", "a/b", "a@", "a@x", "-a", "a\n", "ä"} {
		if got, err := ClassName(bad); err == nil {
			t.Errorf("ClassName(%q) = %q, want error", bad, got)
		}
	}
}

// rubyClassS is an independent transcription of Formulary.class_s using
// regular expressions, used as a differential oracle.
func rubyClassS(name string) string {
	s := strings.ToUpper(name[:1]) + strings.ToLower(name[1:])
	s = regexp.MustCompile(`[-_.\s][a-zA-Z0-9]`).ReplaceAllStringFunc(s, func(m string) string {
		return strings.ToUpper(m[len(m)-1:])
	})
	s = strings.ReplaceAll(s, "+", "x")
	re := regexp.MustCompile(`(.)@(\d)`)
	if loc := re.FindStringSubmatchIndex(s); loc != nil {
		s = s[:loc[0]] + s[loc[2]:loc[3]] + "AT" + s[loc[4]:loc[5]] + s[loc[1]:]
	}
	return s
}

func FuzzClassName(f *testing.F) {
	for _, s := range []string{"node@20", "python@3.12", "go-1password", "a+b", "c++tool", "7zip", "a--b", "x_y.z-w", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		got, err := ClassName(name)
		if err != nil {
			return
		}
		if !rubyConstRe.MatchString(got) {
			t.Fatalf("ClassName(%q) = %q is not a Ruby constant", name, got)
		}
		if want := rubyClassS(name); got != want {
			t.Fatalf("ClassName(%q) = %q, Formulary.class_s gives %q", name, got, want)
		}
		if again, _ := ClassName(name); again != got {
			t.Fatal("not deterministic")
		}
	})
}
