package hephaestus

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The completion scripts call "hephaestus __complete". The shell tests put
// this test binary on PATH as "hephaestus": with the variable below set,
// TestMain runs the CLI instead of the tests.
const completionMainEnv = "HEPHAESTUS_TEST_RUN_CLI"

func TestMain(m *testing.M) {
	if os.Getenv(completionMainEnv) == "1" {
		os.Exit(Main(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// completionShellsRequired makes a missing shell a failure instead of a
// skip (CI installs bash, zsh and fish).
const completionShellsRequired = "HEPHAESTUS_COMPLETION_SHELLS"

type shellCase struct {
	name  string
	words []string // the last one is being completed
	want  []string // every inserted word, in any order
	// contains: want must be among the candidates (files: the shell adds
	// others).
	contains bool
}

var shellCases = []shellCase{
	{name: "top-level command", words: []string{"hephaestus", ""}, want: []string{"init", "check", "run", "build", "archive", "checksum", "release", "formula", "tap", "preflight", "status", "completion", "version", "help"}},
	{name: "partial command", words: []string{"hephaestus", "pre"}, want: []string{"preflight"}},
	{name: "completion shells", words: []string{"hephaestus", "completion", ""}, want: []string{"bash", "zsh", "fish"}},
	{name: "completion partial shell", words: []string{"hephaestus", "completion", "f"}, want: []string{"fish"}},
	{name: "invalid shell has no candidates", words: []string{"hephaestus", "completion", "nope"}, want: nil},
	{name: "long flag", words: []string{"hephaestus", "status", "--fo"}, want: []string{"--format"}},
	{name: "flag after a quoted value with a space", words: []string{"hephaestus", "status", "--repo", "a b", "--fo"}, want: []string{"--format"}},
	{name: "enum value, separate word", words: []string{"hephaestus", "preflight", "--format", ""}, want: []string{"text", "json"}},
	{name: "enum value, partial", words: []string{"hephaestus", "init", "--template", "p"}, want: []string{"prebuilt"}},
	{name: "enum value, --flag=", words: []string{"hephaestus", "status", "--format="}, want: []string{"--format=text", "--format=json"}},
	{name: "enum value, --flag=partial", words: []string{"hephaestus", "status", "--format=j"}, want: []string{"--format=json"}},
	{name: "comma list", words: []string{"hephaestus", "run", "--stages", "build,a"}, want: []string{"build,archive"}},
	{name: "comma list, --flag=", words: []string{"hephaestus", "run", "--stages=build,archive,ch"}, want: []string{"--stages=build,archive,checksum"}},
	{name: "file value", words: []string{"hephaestus", "check", "--config", "heph"}, want: []string{"hephaestus.yaml"}, contains: true},
	{name: "file value, --flag=", words: []string{"hephaestus", "check", "--config=heph"}, want: []string{"--config=hephaestus.yaml"}, contains: true},
	{name: "no repeated flag", words: []string{"hephaestus", "status", "--format", "json", "--fo"}, want: nil},
	{name: "after --", words: []string{"hephaestus", "run", "--", "--fr"}, want: nil},
}

func TestCompletionShells(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no bash, zsh or fish completion on Windows")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(exe, filepath.Join(bin, "hephaestus")); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "hephaestus.yaml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	scripts := t.TempDir()
	for _, sh := range completionShells {
		code, out, _ := runCLI(t, "completion", sh)
		if code != 0 {
			t.Fatalf("completion %s: %d", sh, code)
		}
		name := map[string]string{"bash": "hephaestus", "zsh": "_hephaestus", "fish": "hephaestus.fish"}[sh]
		if err := os.WriteFile(filepath.Join(scripts, name), []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	testdata, err := filepath.Abs(filepath.Join("testdata", "completion"))
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), completionMainEnv+"=1", "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "HOME="+t.TempDir(), "XDG_CONFIG_HOME="+t.TempDir())

	bash := func(sh string, c shellCase) *exec.Cmd {
		return exec.Command(sh, append([]string{"--norc", "--noprofile", filepath.Join(testdata, "complete.bash"), filepath.Join(scripts, "hephaestus")}, c.words...)...)
	}
	shells := []struct {
		name string
		run  func(sh string, c shellCase) *exec.Cmd
	}{
		{"bash", bash},
		{"zsh", func(sh string, c shellCase) *exec.Cmd {
			return exec.Command(sh, "-f", filepath.Join(testdata, "capture.zsh"), scripts, shellLine(c.words))
		}},
		{"fish", func(sh string, c shellCase) *exec.Cmd {
			return exec.Command(sh, "--no-config", "-c", "source "+filepath.Join(scripts, "hephaestus.fish")+"; complete -C "+fishQuote(shellLine(c.words)))
		}},
	}
	if runtime.GOOS == "darwin" {
		// macOS's own bash is 3.2.
		shells = append(shells, struct {
			name string
			run  func(sh string, c shellCase) *exec.Cmd
		}{"/bin/bash", bash})
	}
	for _, s := range shells {
		t.Run(s.name, func(t *testing.T) {
			sh, err := exec.LookPath(s.name)
			if err != nil {
				if os.Getenv(completionShellsRequired) != "" {
					t.Fatalf("%s is required: %v", s.name, err)
				}
				t.Skipf("%s is not installed", s.name)
			}
			for _, c := range shellCases {
				t.Run(c.name, func(t *testing.T) {
					cmd := s.run(sh, c)
					cmd.Dir, cmd.Env = work, env
					var stderr bytes.Buffer
					cmd.Stderr = &stderr
					out, err := cmd.Output()
					if err != nil {
						t.Fatalf("%v: %v\n%s", cmd.Args, err, stderr.String())
					}
					got := parseShellOutput(filepath.Base(s.name), string(out))
					if c.contains {
						for _, w := range c.want {
							if !slices.Contains(got, w) {
								t.Fatalf("%q: %q does not contain %q", shellLine(c.words), got, w)
							}
						}
						return
					}
					slices.Sort(got)
					want := slices.Clone(c.want)
					slices.Sort(want)
					if !slices.Equal(got, want) {
						t.Fatalf("%q: got %q, want %q", shellLine(c.words), got, want)
					}
				})
			}
		})
	}
}

// parseShellOutput returns the inserted words a harness printed.
func parseShellOutput(shell, out string) []string {
	var got []string
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if shell == "zsh" {
		if len(lines) == 0 || lines[len(lines)-1] != "END" {
			return []string{"<zsh capture did not finish>"}
		}
		lines = lines[:len(lines)-1]
	}
	for _, l := range lines {
		if shell == "fish" {
			l, _, _ = strings.Cut(l, "\t") // fish prints "word<TAB>description"
		}
		if l != "" && !slices.Contains(got, l) {
			got = append(got, l)
		}
	}
	return got
}

// shellLine joins words into a command line, single-quoting a word with a
// space; the last (current) word is left open.
func shellLine(words []string) string {
	var parts []string
	for _, w := range words {
		if strings.ContainsAny(w, " '") {
			w = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
		}
		parts = append(parts, w)
	}
	return strings.Join(parts, " ")
}

func fishQuote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}
