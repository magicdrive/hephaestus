package hephaestus

import (
	"flag"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/pipeline"
	"github.com/magicdrive/hephaestus/internal/preflight"
)

func words(cands []candidate) []string {
	var out []string
	for _, c := range cands {
		out = append(out, c.word)
	}
	return out
}

func TestComplete(t *testing.T) {
	allCommands := []string{"init", "check", "run", "build", "archive", "checksum", "release", "formula", "tap", "preflight", "status", "publish", "completion", "version", "help"}
	for _, tc := range []struct {
		name      string
		words     []string
		want      []string
		directive int
	}{
		{"empty input", nil, allCommands, directiveDefault},
		{"empty word", []string{""}, allCommands, directiveDefault},
		{"partial command", []string{"pr"}, []string{"preflight"}, directiveDefault},
		{"top-level flags", []string{"--"}, []string{"--help", "--version"}, directiveDefault},
		{"unknown command", []string{"nope", ""}, nil, directiveDefault},
		{"completion shells", []string{"completion", ""}, []string{"bash", "zsh", "fish"}, directiveDefault},
		{"completion partial shell", []string{"completion", "z"}, []string{"zsh"}, directiveDefault},
		{"completion takes one shell", []string{"completion", "bash", ""}, nil, directiveDefault},
		{"version takes nothing", []string{"version", ""}, nil, directiveDefault},
		{"long flag", []string{"status", "--fo"}, []string{"--format"}, directiveDefault},
		{"single-dash flag keeps the dash", []string{"status", "-fo"}, []string{"-format"}, directiveDefault},
		{"flag after a flag value", []string{"status", "--tag", "v1.0.0", "--to"}, []string{"--token-env"}, directiveDefault},
		{"enum value, separate word", []string{"preflight", "--format", ""}, []string{"text", "json"}, directiveDefault},
		{"enum value, partial", []string{"preflight", "--format", "j"}, []string{"json"}, directiveDefault},
		{"enum value, --flag=value", []string{"status", "--format="}, []string{"--format=text", "--format=json"}, directiveDefault},
		{"enum value, --flag=partial", []string{"status", "--format=t"}, []string{"--format=text"}, directiveDefault},
		{"enum value, -flag=value", []string{"status", "-format=j"}, []string{"-format=json"}, directiveDefault},
		{"template", []string{"init", "--template", ""}, []string{"go", "prebuilt", "rust", "script"}, directiveDefault},
		{"phase", []string{"run", "--phase="}, []string{"--phase=local", "--phase=publish"}, directiveDefault},
		{"stages list", []string{"run", "--stages", "build,a"}, []string{"build,archive"}, directiveDefault},
		{"stages first", []string{"run", "--stages", "t"}, []string{"tap"}, directiveDefault},
		{"stages list skips used", []string{"run", "--stages=build,archive,"}, []string{"--stages=build,archive,checksum", "--stages=build,archive,release", "--stages=build,archive,formula", "--stages=build,archive,tap"}, directiveDefault},
		{"skip list", []string{"preflight", "--skip", "git.b"}, []string{"git.branch"}, directiveDefault},
		{"nothing after all", []string{"run", "--stages=all,"}, nil, directiveDefault},
		{"file value", []string{"check", "--config", ""}, nil, directiveFile},
		{"file value, --flag=value", []string{"preflight", "--report=out"}, nil, directiveFile},
		{"free-form value", []string{"status", "--tag", ""}, nil, directiveDefault},
		{"free-form value is not a flag", []string{"status", "--tag", "--fo"}, nil, directiveDefault},
		{"bool flag takes no value", []string{"run", "--snapshot", "--fr"}, []string{"--fresh"}, directiveDefault},
		{"bool flag with =", []string{"run", "--snapshot="}, nil, directiveDefault},
		{"no repeated flag", []string{"status", "--format", "json", "--fo"}, nil, directiveDefault},
		{"no repeated flag, = form", []string{"status", "--format=json", "--fo"}, nil, directiveDefault},
		{"repeatable flag", []string{"preflight", "--skip", "git.branch", "--sk"}, []string{"--skip"}, directiveDefault},
		{"after --", []string{"run", "--", "--fr"}, nil, directiveDefault},
		{"no positional", []string{"run", "x"}, nil, directiveDefault},
		{"stage commands share run's flags without --stages", []string{"build", "--sta"}, nil, directiveDefault},
		{"help flag", []string{"init", "--he"}, []string{"--help"}, directiveDefault},
		{"publish commands", []string{"publish", ""}, []string{"check", "status", "watch", "plan", "help"}, directiveDefault},
		{"publish partial command", []string{"publish", "w"}, []string{"watch"}, directiveDefault},
		{"publish flags", []string{"publish", "-"}, []string{"--config", "--discover-timeout", "--from-make", "--poll", "--watch-timeout", "--yes", "--help"}, directiveDefault},
		{"publish command flags", []string{"publish", "check", "--f"}, []string{"--from-make"}, directiveDefault},
		{"publish --config is a file", []string{"publish", "watch", "--config", ""}, nil, directiveFile},
		{"publish version is not completed", []string{"publish", "check", "v"}, nil, directiveDefault},
		{"publish no command after a flag", []string{"publish", "--yes", ""}, []string{"--config", "--discover-timeout", "--from-make", "--poll", "--watch-timeout", "--help"}, directiveDefault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, d := complete(tc.words)
			if !slices.Equal(words(got), tc.want) || d != tc.directive {
				t.Fatalf("complete(%q) = %q, %d; want %q, %d", tc.words, words(got), d, tc.want, tc.directive)
			}
		})
	}
}

// TestCompleteFlagsMatchParser: the flags offered for each command are
// exactly those its parser accepts, plus --help.
func TestCompleteFlagsMatchParser(t *testing.T) {
	for _, c := range commands {
		if c.flags == nil {
			continue
		}
		var want []string
		c.flags().VisitAll(func(f *flag.Flag) { want = append(want, "--"+f.Name) })
		want = append(want, "--help")
		got, _ := complete([]string{c.name, "--"})
		if !slices.Equal(words(got), want) {
			t.Errorf("%s: %q, want %q", c.name, words(got), want)
		}
		// Each offered flag is accepted by the command (only -h stops it).
		for _, w := range want {
			if w == "--help" {
				continue
			}
			f := c.flags().Lookup(strings.TrimPrefix(w, "--"))
			if f == nil {
				t.Errorf("%s: %s not defined", c.name, w)
			}
		}
	}
}

// TestCompletionValuesAreAccepted: every fixed value offered is one the
// command accepts, and every accepted value is offered.
func TestCompletionValuesAreAccepted(t *testing.T) {
	if got, want := completionValues["template"].values(), slices.Sorted(maps.Keys(initTemplates)); !slices.Equal(got, want) {
		t.Errorf("templates %q, want %q", got, want)
	}
	for _, s := range completionValues["stages"].values() {
		if _, err := pipeline.ParseStages(s); err != nil {
			t.Errorf("stage %q: %v", s, err)
		}
	}
	if got := completionValues["skip"].values(); !slices.Equal(got, preflight.Checks()) {
		t.Errorf("skip %q", got)
	}
	// --format is validated against outputFormats itself.
	if got := completionValues["format"].values(); !slices.Equal(got, outputFormats) {
		t.Errorf("format %q", got)
	}
	for _, p := range completionValues["phase"].values() {
		if code, _, errs := runCLI(t, "run", "--config", "missing.yaml", "--phase", p); strings.Contains(errs, "--phase must be") {
			t.Errorf("--phase %s rejected (%d)", p, code)
		}
	}
	// Every flag with fixed values exists on some command.
	for name := range completionValues {
		found := false
		for _, c := range commands {
			if c.flags != nil && c.flags().Lookup(name) != nil {
				found = true
			}
		}
		if !found {
			t.Errorf("completion values for unknown flag --%s", name)
		}
	}
}

// TestCommandsMatchDispatch: the registry (usage and completion) and the
// dispatch switch in mainCmd name the same commands.
func TestCommandsMatchDispatch(t *testing.T) {
	src, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	body = body[strings.Index(body, "func mainCmd("):]
	body = body[:strings.Index(body, "\n}\n")]
	var dispatched []string
	for _, m := range regexp.MustCompile(`case ((?:"[^"]+",? ?)+):`).FindAllStringSubmatch(body, -1) {
		for _, q := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1) {
			if !strings.HasPrefix(q[1], "-") && q[1] != "__complete" {
				dispatched = append(dispatched, q[1])
			}
		}
	}
	var registered []string
	for _, c := range commands {
		registered = append(registered, c.name)
	}
	slices.Sort(dispatched)
	slices.Sort(registered)
	if !slices.Equal(dispatched, registered) {
		t.Fatalf("dispatched %q, registered %q", dispatched, registered)
	}
	for _, c := range commands {
		if !strings.Contains(usage, "\n  "+c.name+" ") {
			t.Errorf("usage does not list %s", c.name)
		}
	}
}

func TestCompletionCommand(t *testing.T) {
	old := Version
	Version = "v9.8.7-completion-test"
	t.Cleanup(func() { Version = old })
	for _, sh := range completionShells {
		code, out, errs := runCLI(t, "completion", sh)
		if code != 0 || errs != "" || !strings.Contains(out, "__complete") {
			t.Errorf("%s: %d, stderr %q", sh, code, errs)
		}
		// Deterministic and free of local paths or the version.
		_, again, _ := runCLI(t, "completion", sh)
		if again != out {
			t.Errorf("%s: output differs between runs", sh)
		}
		wd, _ := os.Getwd()
		home, _ := os.UserHomeDir()
		for _, s := range []string{wd, home, Version} {
			if s != "" && strings.Contains(out, s) {
				t.Errorf("%s: script contains %q", sh, s)
			}
		}
	}
	for _, args := range [][]string{{"completion", "powershell"}, {"completion"}, {"completion", "bash", "zsh"}} {
		code, out, errs := runCLI(t, args...)
		if code != 2 || out != "" || errs == "" {
			t.Errorf("%q: %d, stdout %q, stderr %q", args, code, out, errs)
		}
	}
	if code, out, _ := runCLI(t, "completion", "--help"); code != 0 || !strings.Contains(out, "usage: hephaestus completion") {
		t.Errorf("--help: %d %q", code, out)
	}
}

func TestCompleteCommandOutput(t *testing.T) {
	code, out, errs := runCLI(t, "__complete", "status", "--format=")
	if code != 0 || errs != "" || out != "--format=text\n--format=json\n:0\n" {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	code, out, _ = runCLI(t, "__complete", "st")
	if code != 0 || out != "status\tRead-only state of a tag's release and the next safe action (--tag vX.Y.Z)\n:0\n" {
		t.Fatalf("%d %q", code, out)
	}
}

// TestCompletionIsReadOnly: completion reads no configuration, token or
// repository: it works in an empty directory with tokens set and writes
// nothing there.
func TestCompletionIsReadOnly(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("GITHUB_TOKEN", fakeTokenForCompletion)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1") // any network use would fail
	for _, args := range [][]string{{"completion", "bash"}, {"__complete", "run", "--config", ""}, {"__complete", "preflight", "--skip", ""}} {
		code, out, errs := runCLI(t, args...)
		if code != 0 || errs != "" || strings.Contains(out, fakeTokenForCompletion) {
			t.Errorf("%q: %d %q", args, code, errs)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("wrote %v", entries)
	}
}

const fakeTokenForCompletion = "ghp_completionFAKE0123456789abcdefghij"
