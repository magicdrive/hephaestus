package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/publish"
)

var repoConfig = filepath.Join("..", "..", "..", "hephaestus.yaml")

// command is the configuration of `hephaestus publish`.
func command(env map[string]string) Config {
	return Config{Name: "hephaestus publish", Usage: CommandUsage, Help: CommandHelp, ImpliedPublish: true, DirectHints: true,
		Getenv: func(k string) string { return env[k] }}
}

func run(t *testing.T, c Config, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), args, &out, &errb, c)
	return code, out.String(), errb.String()
}

// Every case stops before git or GitHub is used: a usage error, a version
// error, plan, or a configuration that cannot be read.
func TestArguments(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	for _, tc := range []struct {
		name   string
		env    map[string]string
		args   []string
		code   int
		stdout string // substring
		stderr string // substring
	}{
		{"no arguments", nil, nil, 2, "", "usage: hephaestus publish [command] [flags] <version>"},
		{"help", nil, []string{"help"}, 0, "releases <project.name>", ""},
		{"-h prints the flags", nil, []string{"-h"}, 0, "", "-from-make"},
		{"-h after a command", nil, []string{"watch", "-h"}, 0, "", "-watch-timeout"},
		{"unknown command", nil, []string{"deploy", "v1.0.0"}, 2, "", `unknown command "deploy"`},
		{"missing version", nil, []string{"-yes"}, 2, "", "a version is required: hephaestus publish v1.2.3"},
		{"missing version of check", nil, []string{"check"}, 2, "", "a version is required: hephaestus publish check v1.2.3"},
		{"invalid version", nil, []string{"v1.0"}, 2, "", "is not SemVer"},
		{"version without v", nil, []string{"1.0.0"}, 2, "", "did you mean v1.0.0"},
		{"build metadata", nil, []string{"status", "v1.0.0+build.1"}, 2, "", "build metadata"},
		{"two versions", nil, []string{"v1.0.0", "v2.0.0"}, 2, "", "one version is expected"},
		{"flag after the version", nil, []string{"v1.0.0", "-yes"}, 2, "", "one version is expected"},
		{"unknown flag", nil, []string{"-force", "v1.0.0"}, 2, "", "flag provided but not defined: -force"},
		{"implied publish reads the configuration", nil, []string{"-config", missing, "v1.0.0"}, 1, "", "missing.yaml"},
		{"explicit publish", nil, []string{"publish", "-config", missing, "v1.0.0"}, 1, "", "missing.yaml"},
		{"plan", nil, []string{"plan", "-config", repoConfig, "v1.0.0-rc.1"}, 0, "tag=v1.0.0-rc.1\nversion=1.0.0-rc.1\nprerelease=true\ntap=skip\n", ""},
		{"from make", map[string]string{EnvArgs: "v1.0.0"}, []string{"plan", "-config", repoConfig, "-from-make"}, 0, "tap=publish", ""},
		{"from make, VERSION", map[string]string{EnvVersion: "v1.0.0"}, []string{"plan", "-config", repoConfig, "-from-make"}, 0, "tag=v1.0.0", ""},
		{"from make, tag", map[string]string{EnvTag: "v1.0.0"}, []string{"plan", "-config", repoConfig, "-from-make"}, 0, "tag=v1.0.0", ""},
		{"from make without a version", nil, []string{"-from-make"}, 2, "", "a version is required: make publish v1.2.3"},
		{"from make, given twice", map[string]string{EnvArgs: "v1.0.0", EnvVersion: "v1.0.0"}, []string{"-from-make"}, 2, "", "give the version once"},
		{"from make with an argument", map[string]string{EnvArgs: "v1.0.0"}, []string{"-from-make", "v1.0.0"}, 2, "", "environment only"},
		{"from make, shell metacharacters", map[string]string{EnvArgs: "v1;false"}, []string{"-from-make"}, 2, "", "hephaestus publish:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errs := run(t, command(tc.env), tc.args...)
			if code != tc.code || !strings.Contains(out, tc.stdout) || !strings.Contains(errs, tc.stderr) {
				t.Fatalf("%q: %d\nstdout %q\nstderr %q", tc.args, code, out, errs)
			}
			if code == 2 && out != "" {
				t.Errorf("usage error wrote to stdout: %q", out)
			}
		})
	}
}

// TestHelpIsProjectNeutral: the help and usage of hephaestus publish name
// no project, repository or asset count.
func TestHelpIsProjectNeutral(t *testing.T) {
	for _, text := range []string{CommandHelp, CommandUsage} {
		for _, s := range []string{"Hephaestus", "magicdrive", "hephaestus_", "7 assets", "docs/releasing.md"} {
			if strings.Contains(text, s) {
				t.Errorf("%q in\n%s", s, text)
			}
		}
	}
	for _, c := range Commands {
		if !strings.Contains(CommandUsage, "\n  "+c.Name+" ") {
			t.Errorf("usage does not list %s", c.Name)
		}
	}
	for _, s := range []string{"never moved or deleted", "hephaestus publish watch", "make publish-status", "docs/publish.md"} {
		if !strings.Contains(CommandHelp, s) {
			t.Errorf("help lacks %q", s)
		}
	}
}

// TestCommandsAreDispatched: every command shell completion offers is one
// Run accepts.
func TestCommandsAreDispatched(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	for _, c := range Commands {
		code, _, errs := run(t, command(nil), c.Name, "-config", missing, "v1.0.0")
		if strings.Contains(errs, "unknown command") || code == 2 {
			t.Errorf("%s: %d %s", c.Name, code, errs)
		}
	}
}

// TestGate: the gate runs for publish and check only, right after the
// version is resolved, and stops them with exit code 1.
func TestGate(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	for _, cmd := range []string{"publish", "check", "status", "watch", "plan"} {
		var gated []string
		c := command(nil)
		c.Gate = func(cfg string, v publish.Version) error {
			gated = append(gated, cfg+" "+v.Tag)
			return errors.New("gate closed")
		}
		code, _, errs := run(t, c, cmd, "-config", missing, "v1.0.0")
		want := cmd == "publish" || cmd == "check"
		if want != (len(gated) == 1) || want != strings.Contains(errs, "hephaestus publish: gate closed") || code != 1 {
			t.Errorf("%s: gated %v, %d %s", cmd, gated, code, errs)
		}
		if want && gated[0] != missing+" v1.0.0" {
			t.Errorf("%s: gate got %v", cmd, gated)
		}
	}
	// An invalid version never reaches the gate.
	c := command(nil)
	c.Gate = func(string, publish.Version) error { t.Error("gate called"); return nil }
	if code, _, _ := run(t, c, "v1.0"); code != 2 {
		t.Errorf("invalid version: %d", code)
	}
}

// TestSelfReleaseConfig: without ImpliedPublish and DirectHints (the
// repository's own release tool), the command is required, -h is the help
// and messages name the make targets.
func TestSelfReleaseConfig(t *testing.T) {
	c := Config{Name: "publish", Usage: "usage: publish\n", Help: "help text\n", Getenv: func(string) string { return "" }}
	if code, _, errs := run(t, c, "v1.0.0"); code != 2 || !strings.Contains(errs, `publish: unknown command "v1.0.0"`) {
		t.Errorf("version first: %d %s", code, errs)
	}
	if code, out, _ := run(t, c, "-h"); code != 0 || out != "help text\n" {
		t.Errorf("-h: %d %q", code, out)
	}
	if code, _, errs := run(t, c, "check"); code != 2 || !strings.Contains(errs, "a version is required: make publish v1.2.3") {
		t.Errorf("no version: %d %s", code, errs)
	}
}

func TestImpliesPublish(t *testing.T) {
	for arg, want := range map[string]bool{"v1.0.0": true, "1.0.0": true, "-yes": true, "--config": true, "v": false, "deploy": false, "version": false, "": false} {
		if got := impliesPublish(arg); got != want {
			t.Errorf("%q: %t", arg, got)
		}
	}
	if !slices.ContainsFunc(Commands, func(c Command) bool { return c.Name == "plan" }) || isCommand("deploy") || !isCommand("publish") {
		t.Error("isCommand")
	}
}
