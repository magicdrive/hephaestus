package homebrew

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRubyString(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", `"plain"`},
		{`a"b`, `"a\"b"`},
		{`back\slash`, `"back\\slash"`},
		{"#{x}", `"\#{x}"`},
		{"#@ivar #$gvar", `"\#@ivar \#$gvar"`},
		{"C# and #1", `"C# and #1"`},
		{"line1\nline2\ttab\r", `"line1\nline2\ttab\r"`},
		{"nul\x00esc\x1bdel\x7f", `"nul\u{0}esc\u{1b}del\u{7f}"`},
		{"bidi\u202eoverride\u200b", `"bidi\u{202e}override\u{200b}"`},
		{"日本語 ✓", `"日本語 ✓"`},
	}
	for _, tt := range tests {
		got, err := RubyString(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("RubyString(%q) = %s, %v; want %s", tt.in, got, err, tt.want)
		}
	}
	if _, err := RubyString("bad\xff"); err == nil {
		t.Error("invalid UTF-8 accepted")
	}
}

// unescapeRuby decodes the escapes RubyString produces, as Ruby would.
func unescapeRuby(lit string) (string, error) {
	if len(lit) < 2 || lit[0] != '"' || lit[len(lit)-1] != '"' {
		return "", errors.New("not quoted")
	}
	body := lit[1 : len(lit)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c == '"':
			return "", errors.New("unescaped quote")
		case c == '#' && i+1 < len(body) && strings.IndexByte("{@$", body[i+1]) >= 0:
			return "", errors.New("unescaped interpolation")
		case c < 0x20 || c == 0x7f:
			return "", errors.New("raw control character")
		case c != '\\':
			b.WriteByte(c)
		case i+1 >= len(body):
			return "", errors.New("dangling backslash")
		default:
			i++
			switch body[i] {
			case '\\', '"', '#':
				b.WriteByte(body[i])
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'u':
				end := strings.IndexByte(body[i:], '}')
				if body[i+1] != '{' || end < 0 {
					return "", errors.New("bad \\u")
				}
				n, err := strconv.ParseUint(body[i+2:i+end], 16, 32)
				if err != nil {
					return "", err
				}
				b.WriteRune(rune(n))
				i += end
			default:
				return "", errors.New("unexpected escape")
			}
		}
	}
	return b.String(), nil
}

func FuzzRubyString(f *testing.F) {
	for _, s := range []string{"", `"; system("rm -rf /"); "`, "#{`id`}", "a\nb", "\\", "#", "#@x", "\u202e", "__END__", "\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		lit, err := RubyString(s)
		if !utf8.ValidString(s) {
			if err == nil {
				t.Fatal("invalid UTF-8 accepted")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(lit, "\n\r\x00") {
			t.Fatalf("literal %q contains a raw line break or NUL", lit)
		}
		got, err := unescapeRuby(lit)
		if err != nil {
			t.Fatalf("literal %q: %v", lit, err)
		}
		if got != s {
			t.Fatalf("round trip: %q -> %s -> %q", s, lit, got)
		}
	})
}

func FuzzRubyHeredoc(f *testing.F) {
	for _, s := range []string{"", "EOS", "a\nEOS\nb", "  EOS  ", "#{x}", "\\", "line\r\nx"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		hd, err := rubyHeredoc(s, "    ")
		if !utf8.ValidString(s) {
			if err == nil {
				t.Fatal("invalid UTF-8 accepted")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(hd, "\n")
		term := strings.TrimPrefix(lines[0], "<<~")
		if strings.TrimSpace(lines[len(lines)-1]) != term {
			t.Fatalf("heredoc does not end with its terminator: %q", hd)
		}
		for _, l := range lines[1 : len(lines)-1] {
			if strings.TrimSpace(l) == term {
				t.Fatalf("content line ends the heredoc early: %q", hd)
			}
			for i := 0; i < len(l); i++ {
				if l[i] == '\\' {
					i++
					continue
				}
				if l[i] == '#' && i+1 < len(l) && strings.IndexByte("{@$", l[i+1]) >= 0 {
					t.Fatalf("unescaped interpolation in %q", l)
				}
				if l[i] < 0x20 || l[i] == 0x7f {
					t.Fatalf("raw control character in %q", l)
				}
			}
		}
	})
}

func TestCodeBlock(t *testing.T) {
	got, _, err := codeBlock("install", "\n\t\n    bin.install \"a\"\n      # nested\n    x = <<~EOS\n      text\n    EOS   \n\n", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want := "  bin.install \"a\"\n    # nested\n  x = <<~EOS\n    text\n  EOS"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	for _, tc := range []struct{ code, err string }{
		{"bin.install \"a\"\n__END__\nputs 1", "__END__"},
		{"  __END__", "__END__"},
		{"=begin\nx\n=end", "=begin"},
		{"x = <<EOS\ny\nEOS", "<<~EOS"},
		{"x = <<~EOS\ny\n", "not terminated"},
		{"x = <<~'EOS'\ny", "not terminated"},
		{"a\x00b", "U+0000"},
		{"a\x1bb", "U+001B"},
		{"a\u202eb", "U+202E"},
		{"a\rb", "U+000D"},
		{"bad\xff", "UTF-8"},
	} {
		if _, _, err := codeBlock("test", tc.code, "  "); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("codeBlock(%q) err = %v, want %q", tc.code, err, tc.err)
		}
	}
	if _, w, err := codeBlock("test", "x = <<-EOS\n  y\n  EOS", "  "); err != nil || len(w) != 1 {
		t.Errorf("<<- heredoc: warnings %v, err %v", w, err)
	}
	if _, _, err := codeBlock("test", "a = [1] << 2\nb <<~X\nX", "  "); err != nil {
		t.Errorf("valid code rejected: %v", err)
	}
}

func TestRenderCodeValidatesValues(t *testing.T) {
	v := testVars()
	v.Env["SAFE"] = "abc-1.2"
	v.Env["EVIL"] = `"; system("id"); "`
	v.Env["UNUSED"] = "anything \" goes #{here}"
	ok := []string{
		`system bin/"x", "{{ .Version }}"`,
		`x = "{{ .Env.SAFE }}"`,
		`x = "{{ index .Env "SAFE" }}"`,
		`x = "{{ .FullCommit }} {{ .Tag }} {{ .ProjectName }}"`,
		`x = "{{ with .Version }}{{ . }}{{ end }}"`,
		`no template`,
	}
	for _, s := range ok {
		if _, err := renderCode("test", s, v); err != nil {
			t.Errorf("renderCode(%q): %v", s, err)
		}
	}
	bad := []struct{ src, err string }{
		{`x = "{{ .Env.EVIL }}"`, "Env.EVIL"},
		{`x = "{{ index .Env "EVIL" }}"`, "Env.EVIL"},
		{`{{ range $k, $v := .Env }}{{ $v }}{{ end }}`, "NAME"},
		{`{{ .Nope }}`, "Nope"},
	}
	for _, tc := range bad {
		if _, err := renderCode("test", tc.src, v); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("renderCode(%q) err = %v, want %q", tc.src, err, tc.err)
		}
	}
	v.Version = "1.0\"; system(\"id"
	if _, err := renderCode("test", `x = "{{ .Version }}"`, v); err == nil {
		t.Error("unsafe version accepted")
	}
	if _, err := renderCode("test", `x = "{{ with .Tag }}{{ . }}{{ end }}"`, v); err == nil {
		t.Error("a bare dot must validate every field")
	}
}

// rubyProbe loads a generated formula with a stub Formula class and prints
// desc, homepage and caveats as Ruby sees them. Nothing in the formula is
// executed beyond its class body and #caveats.
const rubyProbe = `
require "json"
class Formula
  def self.method_missing(name, *args, **kw, &blk)
    (@calls ||= {})[name] = args
    nil
  end
  def self.respond_to_missing?(*) = true
  def self.calls = @calls || {}
  def self.test(*) = nil
  def method_missing(*) = nil
end
load ARGV[0]
k = Object.const_get(ARGV[1])
c = k.allocate
puts JSON.generate(
  "desc" => k.calls[:desc]&.first,
  "homepage" => k.calls[:homepage]&.first,
  "caveats" => (c.respond_to?(:caveats) ? c.caveats : nil),
)
`

func TestRubyInjection(t *testing.T) {
	ruby := requireRuby(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "pwned")
	payloads := []string{
		`"; system("touch ` + marker + `"); "`,
		`#{system("touch ` + marker + `")}`,
		`#{File.write("` + marker + `", "x")}`,
		"line1\nend\nsystem(\"touch " + marker + "\")\n",
		"__END__",
		"x\n__END__\ny",
		`\"; system("touch ` + marker + `") #`,
		"EOS\nsystem(\"touch " + marker + "\")\nEOS",
		"#@foo #$0 \\#{x} \\",
		"\x00\x1b[31mred\u202e",
	}
	probe := filepath.Join(dir, "probe.rb")
	if err := os.WriteFile(probe, []byte(rubyProbe), 0o644); err != nil {
		t.Fatal(err)
	}
	for i, p := range payloads {
		b := baseBrew()
		b.Description = p
		b.Caveats = p
		f := generateOne(t, input(b, fourPlatforms(false)...))
		file := filepath.Join(dir, "f"+strconv.Itoa(i)+".rb")
		if err := os.WriteFile(file, f.Content, 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(ruby, "-c", file).CombinedOutput(); err != nil {
			t.Fatalf("payload %q: ruby -c: %v\n%s\n%s", p, err, out, f.Content)
		}
		out, err := exec.Command(ruby, probe, file, f.ClassName).Output()
		if err != nil {
			var stderr []byte
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = ee.Stderr
			}
			t.Fatalf("payload %q: probe: %v\n%s\n%s", p, err, stderr, f.Content)
		}
		var got struct{ Desc, Homepage, Caveats string }
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("probe output %q: %v", out, err)
		}
		if got.Desc != p {
			t.Errorf("desc round trip: got %q, want %q", got.Desc, p)
		}
		wantCaveats := strings.Join(normalizeText(p), "\n") + "\n"
		if got.Caveats != wantCaveats {
			t.Errorf("caveats round trip: got %q, want %q\n%s", got.Caveats, wantCaveats, f.Content)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("payload %q executed code", p)
		}
	}
}

func TestRubyInjectionInCodeFields(t *testing.T) {
	// User Ruby is code by design, but template values substituted into it
	// are validated.
	b := baseBrew()
	b.Test = `assert_match "{{ .Env.VERSION_LABEL }}", shell_output("#{bin}/mycli version")`
	in := input(b, fourPlatforms(false)...)
	in.Vars.Env["VERSION_LABEL"] = `x"); system("id"); ("`
	if _, err := Generate(in); err == nil || !strings.Contains(err.Error(), "cannot be substituted") {
		t.Errorf("err = %v", err)
	}
	b.Test = "assert_predicate bin/\"mycli\", :executable?\n__END__\n"
	if _, err := Generate(input(b, fourPlatforms(false)...)); err == nil || !strings.Contains(err.Error(), "__END__") {
		t.Errorf("err = %v", err)
	}
}
