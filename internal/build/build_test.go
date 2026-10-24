package build

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// The test binary doubles as the build command: when HEPH_TEST_HELPER is
// set (through the build's env), TestMain runs a helper mode instead of the
// tests. This keeps the tests independent of a shell or Go toolchain.
const helperEnv = "HEPH_TEST_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(helper(mode))
	}
	os.Exit(m.Run())
}

func helper(mode string) int {
	out := os.Getenv(EnvOutput)
	switch mode {
	case "write":
		// The output records what the command saw, for assertions.
		wd, _ := os.Getwd()
		report := fmt.Sprintf("os=%s\narch=%s\narm=%s\nFOO=%s\nBAR=%s\nargs=%s\ncwd=%s\n",
			os.Getenv(EnvOS), os.Getenv(EnvArch), os.Getenv(EnvArm),
			os.Getenv("FOO"), os.Getenv("BAR"), strings.Join(os.Args[1:], " "), wd)
		if err := os.WriteFile(out, []byte(report), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if os.Getenv("HEPH_TEST_CHMOD0") != "" {
			os.Chmod(out, 0)
		}
		return 0
	case "env":
		// The output lists which credential variables the command saw.
		var b strings.Builder
		for _, k := range scrubProbe {
			if v, ok := os.LookupEnv(k); ok {
				fmt.Fprintf(&b, "%s=%s\n", k, v)
			}
		}
		return exitOn(os.WriteFile(out, []byte(b.String()), 0o755))
	case "empty":
		return exitOn(os.WriteFile(out, nil, 0o755))
	case "noop":
		fmt.Println("doing nothing")
		return 0
	case "fail":
		fmt.Fprintln(os.Stderr, "compiling...")
		fmt.Fprintln(os.Stderr, "boom: undefined: frobnicate")
		return 3
	case "symlink":
		return exitOn(os.Symlink(os.Getenv("HEPH_TEST_LINK"), out))
	case "sleep":
		time.Sleep(2 * time.Minute)
		return 0
	case "spawn":
		// Start a grandchild in the same process group, record its pid,
		// then hang: a timeout must kill both.
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), helperEnv+"=sleep")
		if err := c.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		if err := os.WriteFile(os.Getenv("HEPH_TEST_PIDFILE"), []byte(fmt.Sprint(c.Process.Pid)), 0o644); err != nil {
			return 2
		}
		fmt.Fprintln(os.Stderr, "still building")
		time.Sleep(2 * time.Minute)
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
	return 2
}

func exitOn(err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	return 0
}

// fixture creates files under a temporary project root.
func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	// Resolve /var -> /private/var on macOS so path comparisons hold.
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func loadConfig(t *testing.T, root, yml string) *config.Config {
	t.Helper()
	cfg, err := config.Load(strings.NewReader(yml), root)
	if err != nil {
		t.Fatalf("config: %v\n%s", err, yml)
	}
	return cfg
}

func newInput(cfg *config.Config) (Input, *bytes.Buffer) {
	var out bytes.Buffer
	return Input{
		Config:   cfg,
		Manifest: manifest.New(cfg.Project.Name, "1.2.3", "v1.2.3", ""),
		Vars:     tmpl.Vars{Project: cfg.Project.Name, ProjectName: cfg.Project.Name, Version: "1.2.3", Tag: "v1.2.3"},
		Environ:  os.Environ(),
		Stdout:   &out,
		Stderr:   &out,
	}, &out
}

func run(t *testing.T, in Input) *Result {
	t.Helper()
	res, err := Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func runErr(t *testing.T, in Input, want ...string) error {
	t.Helper()
	_, err := Run(context.Background(), in)
	if err == nil {
		t.Fatalf("Run succeeded, want error containing %q", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("Run error = %v\nwant containing %q", err, w)
		}
	}
	return err
}

// self is the test binary as a YAML string, used as the command.
func self(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%q", exe)
}

// helperBuild returns a single command build running helper mode.
func helperBuild(t *testing.T, mode, extra string) string {
	return fmt.Sprintf(`
project: {name: app}
builds:
  - command: [%s, "--target={{ .OS }}/{{ .Arch }}"]
    targets: [{os: linux, arch: amd64}]
    output: "dist/{{ .OS }}_{{ .Arch }}/app{{ .Ext }}"
    env: {%s: %s}
%s`, self(t), helperEnv, mode, extra)
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestCommandMatrixTemplatesAndEnv(t *testing.T) {
	root := fixture(t, map[string]string{"sub/.keep": ""})
	cfg := loadConfig(t, root, fmt.Sprintf(`
project: {name: app}
env: [BAR=from-config]
builds:
  - id: cli
    command: [%s, "--os={{ .OS }}", "--ver={{ .Version }}"]
    targets: [{os: linux, arch: arm, arm: "7"}]
    matrix:
      os: [linux, windows]
      arch: [amd64, arm64]
      exclude: [{os: windows, arch: arm64}]
    dir: sub
    output: "dist/{{ .OS }}_{{ .Arch }}{{ with .Arm }}v{{ . }}{{ end }}/app{{ .Ext }}"
    env:
      %s: write
      FOO: "foo-{{ .OS }}-{{ .Arch }}"
`, self(t), helperEnv))
	in, _ := newInput(cfg)
	in.Environ = append(in.Environ, "BAR=from-base", "FOO=from-base")
	res := run(t, in)

	wantTargets := []string{"linux/armv7", "linux/amd64", "linux/arm64", "windows/amd64"}
	if len(res.Binaries) != len(wantTargets) || len(res.Planned) != len(wantTargets) {
		t.Fatalf("binaries = %+v", res.Binaries)
	}
	for i, a := range res.Binaries {
		tgt := config.Target{OS: a.OS, Arch: a.Arch, Arm: a.Arm}
		if tgt.String() != wantTargets[i] {
			t.Errorf("binary %d is for %s, want %s (declaration order)", i, tgt, wantTargets[i])
		}
		data, err := os.ReadFile(filepath.Join(root, a.Path))
		if err != nil {
			t.Fatal(err)
		}
		armSuffix := ""
		if a.Arm != "" {
			armSuffix = "v" + a.Arm
		}
		wantName := "app"
		if a.OS == "windows" {
			wantName = "app.exe"
		}
		wantReport := fmt.Sprintf("os=%s\narch=%s\narm=%s\nFOO=foo-%s-%s\nBAR=from-config\nargs=--os=%s --ver=1.2.3\ncwd=%s\n",
			a.OS, a.Arch, a.Arm, a.OS, a.Arch, a.OS, filepath.Join(root, "sub"))
		if string(data) != wantReport {
			t.Errorf("%s: command saw\n%s\nwant\n%s", tgt, data, wantReport)
		}
		if a.Path != fmt.Sprintf("dist/%s_%s%s/%s", a.OS, a.Arch, armSuffix, wantName) || a.Name != wantName {
			t.Errorf("path/name = %s / %s", a.Path, a.Name)
		}
		if a.Kind != manifest.KindBinary || a.ID != "cli" || a.SHA256 != sha(data) || a.Size != int64(len(data)) {
			t.Errorf("artifact = %+v", a)
		}
		if len(a.Executables) != 1 || a.Executables[0] != a.Name || a.Source != (manifest.Source{Stage: "build", Provider: "command"}) {
			t.Errorf("executables/source = %v %+v", a.Executables, a.Source)
		}
	}
	if got := in.Manifest.Filter(manifest.KindBinary, "cli"); len(got) != 4 {
		t.Errorf("manifest binaries = %d", len(got))
	}
	p := res.Planned[0]
	if p.Dir != filepath.Join(root, "sub") || p.Output != "dist/linux_armv7/app" || p.Timeout != DefaultTimeout {
		t.Errorf("plan = %+v", p)
	}
	wantEnv := []string{"BAR=from-config", "FOO=foo-linux-arm", helperEnv + "=write", "HEPHAESTUS_OS=linux", "HEPHAESTUS_ARCH=arm", "HEPHAESTUS_ARM=7", "HEPHAESTUS_OUTPUT=" + filepath.Join(root, "dist", "linux_armv7", "app")}
	if strings.Join(p.Env, "\n") != strings.Join(wantEnv, "\n") {
		t.Errorf("plan env =\n%s\nwant\n%s", strings.Join(p.Env, "\n"), strings.Join(wantEnv, "\n"))
	}
}

func TestShellCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell syntax")
	}
	root := fixture(t, nil)
	cfg := loadConfig(t, root, `
project: {name: app}
builds:
  - command: 'printf "%s-%s|%s" "$HEPHAESTUS_OS" "$HEPHAESTUS_ARCH" "{{ .Version }}" > "$HEPHAESTUS_OUTPUT"'
    targets: [{os: darwin, arch: arm64}]
    output: dist/app-{{ .OS }}
`)
	in, _ := newInput(cfg)
	res := run(t, in)
	data, _ := os.ReadFile(filepath.Join(root, "dist", "app-darwin"))
	if string(data) != "darwin-arm64|1.2.3" {
		t.Errorf("output = %q", data)
	}
	if argv := res.Planned[0].Argv; len(argv) != 3 || argv[0] != "/bin/sh" || argv[1] != "-c" || !strings.Contains(argv[2], `"1.2.3"`) {
		t.Errorf("argv = %q", argv)
	}

	cfg = loadConfig(t, root, `
project: {name: app}
builds:
  - command: 'echo "cannot link" >&2; exit 7'
    targets: [{os: darwin, arch: arm64}]
    output: dist/app
`)
	in, _ = newInput(cfg)
	runErr(t, in, "build default", "target darwin/arm64", "exit code 7", "cannot link")
}

func TestCommandFailure(t *testing.T) {
	root := fixture(t, nil)
	in, out := newInput(loadConfig(t, root, helperBuild(t, "fail", "")))
	runErr(t, in, "build default: target linux/amd64: command", "failed with exit code 3", "stderr:\ncompiling...\nboom: undefined: frobnicate")
	if !strings.Contains(out.String(), "boom") {
		t.Errorf("stderr was not streamed: %q", out.String())
	}
	if len(in.Manifest.Artifacts) != 0 {
		t.Error("manifest modified after a failed build")
	}
}

func TestMissingOutput(t *testing.T) {
	root := fixture(t, nil)
	in, out := newInput(loadConfig(t, root, helperBuild(t, "noop", "")))
	runErr(t, in, `did not create output "dist/linux_amd64/app"`, "does not cross-compile", "target linux/amd64")
	if !strings.Contains(out.String(), "doing nothing") {
		t.Errorf("stdout was not streamed: %q", out.String())
	}

	// A stale file from an earlier run must not satisfy the build.
	stale := fixture(t, map[string]string{"dist/linux_amd64/app": "OLD"})
	in, _ = newInput(loadConfig(t, stale, helperBuild(t, "noop", "")))
	runErr(t, in, "did not create output")
	if _, err := os.Stat(filepath.Join(stale, "dist", "linux_amd64", "app")); !os.IsNotExist(err) {
		t.Errorf("stale output was not removed: %v", err)
	}
}

func TestInvalidOutputs(t *testing.T) {
	cases := []struct{ output, want string }{
		{"../outside/app", `".." segment`},
		{"dist/../../app", `".." segment`},
		{"/tmp/app", "relative"},
		{"{{ .OS }}/../../app", `".." segment`},
		{"dist/", "trailing slash"},
	}
	for _, tc := range cases {
		t.Run(tc.output, func(t *testing.T) {
			root := fixture(t, nil)
			yml := strings.Replace(helperBuild(t, "write", ""), `"dist/{{ .OS }}_{{ .Arch }}/app{{ .Ext }}"`, fmt.Sprintf("%q", tc.output), 1)
			in, _ := newInput(loadConfig(t, root, yml))
			runErr(t, in, "output", tc.want)
		})
	}
	root := fixture(t, nil)
	in, _ := newInput(loadConfig(t, root, helperBuild(t, "empty", "")))
	runErr(t, in, "is empty")

	in, _ = newInput(loadConfig(t, root, helperBuild(t, "write", "limits: {max_file_size: 16}\n")))
	runErr(t, in, "over limits.max_file_size (16)")

	// Two targets writing the same file are caught before anything runs.
	yml := strings.Replace(helperBuild(t, "fail", ""), "targets: [{os: linux, arch: amd64}]", "targets: [{os: linux, arch: amd64}, {os: linux, arch: arm64}]", 1)
	yml = strings.Replace(yml, `"dist/{{ .OS }}_{{ .Arch }}/app{{ .Ext }}"`, "dist/{{ .OS }}/app", 1)
	in, _ = newInput(loadConfig(t, root, yml))
	runErr(t, in, "targets linux/amd64 and linux/arm64 both write output")
}

func TestOutputSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("not yours"), 0o644)
	root := fixture(t, map[string]string{"real/app": "INSIDE"})

	in, _ := newInput(loadConfig(t, root, helperBuild(t, "symlink", "")))
	in.Environ = append(in.Environ, "HEPH_TEST_LINK="+outside)
	runErr(t, in, "outside the project root")

	in, _ = newInput(loadConfig(t, root, helperBuild(t, "symlink", "")))
	in.Environ = append(in.Environ, "HEPH_TEST_LINK=../../real/app")
	res := run(t, in)
	if res.Binaries[0].SHA256 != sha([]byte("INSIDE")) {
		t.Errorf("link inside the root not followed: %+v", res.Binaries[0])
	}

	in, _ = newInput(loadConfig(t, root, helperBuild(t, "symlink", "")))
	in.Environ = append(in.Environ, "HEPH_TEST_LINK=../../missing")
	runErr(t, in, "broken symbolic link")
}

func TestPermissionDenied(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix permissions and a non-root user")
	}
	root := fixture(t, nil)
	in, _ := newInput(loadConfig(t, root, helperBuild(t, "write", "")))
	in.Environ = append(in.Environ, "HEPH_TEST_CHMOD0=1")
	runErr(t, in, "permission denied")
}

func TestDirMustBeInsideRoot(t *testing.T) {
	root := fixture(t, map[string]string{"file": "x"})
	for dir, want := range map[string]string{"missing": "does not exist", "file": "not a directory", "../x": `".." segment`} {
		in, _ := newInput(loadConfig(t, root, helperBuild(t, "write", "")))
		in.Config.Builds[0].Dir = dir
		runErr(t, in, want)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape")); err != nil {
			t.Fatal(err)
		}
		in, _ := newInput(loadConfig(t, root, helperBuild(t, "write", "    dir: escape\n")))
		runErr(t, in, "must be a directory inside the project root")
	}
}

func TestProgramNotFound(t *testing.T) {
	root := fixture(t, nil)
	cfg := loadConfig(t, root, `
project: {name: app}
builds:
  - command: [hephaestus-no-such-program-xyz, build]
    targets: [{os: linux, arch: amd64}]
    output: dist/app
`)
	in, _ := newInput(cfg)
	runErr(t, in, `program "hephaestus-no-such-program-xyz" not found in the command's PATH`)
}

func TestTimeout(t *testing.T) {
	root := fixture(t, nil)
	pidFile := filepath.Join(t.TempDir(), "pid")
	in, _ := newInput(loadConfig(t, root, helperBuild(t, "spawn", "    timeout: 1s\n")))
	in.Environ = append(in.Environ, "HEPH_TEST_PIDFILE="+pidFile)
	start := time.Now()
	runErr(t, in, "timed out after 1s", "killed with its process group", "still building")
	if d := time.Since(start); d > 30*time.Second {
		t.Errorf("timeout took %s", d)
	}
	checkGrandchildKilled(t, pidFile)
}

func TestCancelledContext(t *testing.T) {
	root := fixture(t, nil)
	in, _ := newInput(loadConfig(t, root, helperBuild(t, "sleep", "")))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	_, err := Run(ctx, in)
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("err = %v", err)
	}
}

func TestDryRun(t *testing.T) {
	root := fixture(t, nil)
	yml := strings.Replace(helperBuild(t, "fail", ""), "targets: [{os: linux, arch: amd64}]", "matrix: {os: [linux, windows], arch: [amd64]}", 1)
	in, _ := newInput(loadConfig(t, root, yml))
	in.DryRun = true
	before, _ := in.Manifest.Marshal()
	res := run(t, in)
	if len(res.Planned) != 2 || len(res.Binaries) != 0 {
		t.Fatalf("result = %+v", res)
	}
	p := res.Planned[1]
	if p.Target.String() != "windows/amd64" || p.Output != "dist/windows_amd64/app.exe" || p.Argv[1] != "--target=windows/amd64" {
		t.Errorf("plan = %+v", p)
	}
	if !contains(p.Env, "HEPHAESTUS_OUTPUT="+filepath.Join(root, "dist", "windows_amd64", "app.exe")) {
		t.Errorf("env = %v", p.Env)
	}
	if _, err := os.Stat(filepath.Join(root, "dist")); !os.IsNotExist(err) {
		t.Error("dry run created files")
	}
	after, _ := in.Manifest.Marshal()
	if !bytes.Equal(before, after) {
		t.Error("dry run changed the manifest")
	}
}

func TestRerunReplacesManifestEntries(t *testing.T) {
	root := fixture(t, nil)
	yml := fmt.Sprintf(`
project: {name: app}
builds:
  - id: a
    command: [%[1]s]
    targets: [{os: linux, arch: amd64}, {os: linux, arch: arm64}]
    output: "dist/a_{{ .Arch }}"
    env: {%[2]s: write}
  - id: b
    command: [%[1]s]
    targets: [{os: linux, arch: amd64}]
    output: "dist/b_{{ .Arch }}"
    env: {%[2]s: write}
`, self(t), helperEnv)
	in, _ := newInput(loadConfig(t, root, yml))
	// An archive from the archives stage sharing the id must survive.
	in.Manifest.Put(manifest.Artifact{Kind: manifest.KindArchive, ID: "a", Name: "a.tar.gz", Path: "dist/a.tar.gz", OS: "linux", Arch: "amd64", Format: "tar.gz", SHA256: strings.Repeat("0", 64), Source: manifest.Source{Stage: "archive"}})
	run(t, in)
	if n := len(in.Manifest.Filter(manifest.KindBinary)); n != 3 {
		t.Fatalf("binaries = %d", n)
	}
	// Re-run only build a with one target: its old entries are replaced,
	// build b's entries are kept.
	in.Config.Builds[0].Targets = in.Config.Builds[0].Targets[:1]
	in.IDs = []string{"a"}
	run(t, in)
	if got := in.Manifest.Filter(manifest.KindBinary, "a"); len(got) != 1 || got[0].Arch != "amd64" {
		t.Errorf("build a entries = %+v", got)
	}
	if got := in.Manifest.Filter(manifest.KindBinary, "b"); len(got) != 1 {
		t.Errorf("build b entries = %+v", got)
	}
	if got := in.Manifest.Filter(manifest.KindArchive); len(got) != 1 {
		t.Errorf("archive entries = %+v", got)
	}

	in.IDs = []string{"nope"}
	runErr(t, in, `unknown build id "nope"`)
}

func TestBinaryNameClashAcrossBuilds(t *testing.T) {
	root := fixture(t, nil)
	in, _ := newInput(loadConfig(t, root, helperBuild(t, "write", "")))
	in.Manifest.Put(manifest.Artifact{Kind: manifest.KindBinary, ID: "other", Name: "app", Path: "x/app", OS: "linux", Arch: "amd64", Source: manifest.Source{Stage: "build"}})
	runErr(t, in, "is also recorded by build other")
}

func TestInvalidTargets(t *testing.T) {
	root := fixture(t, nil)
	in, _ := newInput(loadConfig(t, root, helperBuild(t, "write", "")))
	in.Config.Builds[0].Targets = []config.Target{{OS: "Linux;rm", Arch: "amd64"}}
	runErr(t, in, "invalid target")
	in.Config.Builds[0].Targets = nil
	runErr(t, in, "no targets")
}

func TestMergeEnv(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2", "malformed", "C=3"}, []string{"B=x", "D=4", "B=y"})
	want := "A=1,C=3,D=4,B=y"
	if strings.Join(got, ",") != want {
		t.Errorf("mergeEnv = %v, want %s", got, want)
	}
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{max: 8}
	tb.Write([]byte("hello "))
	tb.Write([]byte("world!"))
	if string(tb.buf) != "o world!" || !tb.truncated {
		t.Errorf("tail = %q truncated=%v", tb.buf, tb.truncated)
	}
	tb = &tailBuffer{max: 8}
	tb.Write([]byte("0123456789"))
	if string(tb.buf) != "23456789" || !tb.truncated {
		t.Errorf("tail = %q", tb.buf)
	}
}

// scrubProbe are the variables the "env" helper reports.
var scrubProbe = []string{
	"GITHUB_TOKEN", "HOMEBREW_TAP_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_PAT",
	"ACTIONS_RUNTIME_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_URL",
	"MY_RELEASE_VAR", "MY_TAP_VAR", "TAP_PAT", "NPM_AUTH", "SENTRY_API_KEY", "MY_PASSWORD", "KEEP_ME",
}

// TestCommandEnvScrubsCredentials is a regression test: build commands used
// to inherit the release and tap tokens, so any build-time code (go
// generate, npm postinstall, build.rs) could publish releases or push to the
// tap.
func TestCommandEnvScrubsCredentials(t *testing.T) {
	root := fixture(t, nil)
	cfg := loadConfig(t, root, helperBuild(t, "env", ""))
	in, _ := newInput(cfg)
	for _, k := range scrubProbe {
		in.Environ = append(in.Environ, k+"=secret-"+k)
	}
	in.ScrubEnv = []string{"MY_RELEASE_VAR", "MY_TAP_VAR", ""}
	res := run(t, in)
	data, err := os.ReadFile(filepath.Join(root, res.Binaries[0].Path))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "KEEP_ME=secret-KEEP_ME\n" {
		t.Fatalf("command saw:\n%s\nwant only KEEP_ME", got)
	}
}

// TestCommandEnvOptInPassesCredential checks the explicit opt-in: a build's
// env may pass a scrubbed credential, read through .Env even though .Env
// hides credentials from other templates.
func TestCommandEnvOptInPassesCredential(t *testing.T) {
	root := fixture(t, nil)
	cfg := loadConfig(t, root, fmt.Sprintf(`
project: {name: app}
builds:
  - command: [%s]
    targets: [{os: linux, arch: amd64}]
    output: dist/app
    env:
      %s: env
      GITHUB_TOKEN: "{{ .Env.GITHUB_TOKEN }}"
`, self(t), helperEnv))
	in, _ := newInput(cfg)
	in.Environ = append(in.Environ, "GITHUB_TOKEN=ghs_optin", "GH_TOKEN=ghs_other")
	// .Env (published values) hides the token; the full environment is
	// available only through Secret.
	in.Vars.Env = map[string]string{}
	in.Vars = tmpl.WithSecretEnv(in.Vars, map[string]string{"GITHUB_TOKEN": "ghs_optin"})
	res := run(t, in)
	data, err := os.ReadFile(filepath.Join(root, res.Binaries[0].Path))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "GITHUB_TOKEN=ghs_optin\n" {
		t.Fatalf("command saw:\n%s", got)
	}
}

func TestScrubEnv(t *testing.T) {
	got := scrubEnv([]string{"A=1", "GITHUB_TOKEN=x", "X_VAR=y", "B=2", "ACTIONS_RUNTIME_TOKEN=z", "TAP_PAT=p", "PATH=/bin"}, []string{"X_VAR"})
	if strings.Join(got, ",") != "A=1,B=2,PATH=/bin" {
		t.Fatalf("scrubEnv = %v", got)
	}
}
