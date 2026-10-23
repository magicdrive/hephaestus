package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/pathutil"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// Environment variables set for every command run.
const (
	EnvOS     = "HEPHAESTUS_OS"
	EnvArch   = "HEPHAESTUS_ARCH"
	EnvArm    = "HEPHAESTUS_ARM"
	EnvOutput = "HEPHAESTUS_OUTPUT" // absolute path of the expected output
)

// stderrTail is how much of a failed command's stderr is quoted in errors.
const stderrTail = 4 << 10

// waitDelay bounds how long Wait waits for output pipes after the process
// exits or is killed (a detached grandchild may keep them open).
const waitDelay = 10 * time.Second

// runCommand plans every target of a command build first, so a template or
// path error is reported before anything runs, then runs the targets in
// order. A target that fails stops the build.
//
// Before a target runs, a previous output file is removed, so a stale file
// from an earlier run can never pass as this run's result, and the output's
// parent directory is created.
func (s *stage) runCommand(ctx context.Context, b *config.Build) error {
	ts, err := targets(b)
	if err != nil {
		return err
	}
	timeout := time.Duration(b.Timeout)
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	var plans []PlannedCommand
	outputs := map[string]config.Target{}
	for _, t := range ts {
		p, err := s.planCommand(b, t, timeout)
		if err != nil {
			return fmt.Errorf("target %s: %w", t, err)
		}
		key := pathutil.FoldKey(p.Output)
		if prev, dup := outputs[key]; dup {
			return fmt.Errorf("targets %s and %s both write output %q; include {{ .OS }}, {{ .Arch }} and {{ .Arm }} in output", prev, t, p.Output)
		}
		outputs[key] = t
		plans = append(plans, p)
	}
	for _, p := range plans {
		s.res.Planned = append(s.res.Planned, p)
		if s.in.DryRun {
			continue
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("target %s: not started: %w", p.Target, err)
		}
		a, err := s.execute(ctx, b, p)
		if err != nil {
			return fmt.Errorf("target %s: %w", p.Target, err)
		}
		s.res.Binaries = append(s.res.Binaries, a)
	}
	return nil
}

func (s *stage) planCommand(b *config.Build, t config.Target, timeout time.Duration) (PlannedCommand, error) {
	vars := s.in.Vars.WithTarget(t.OS, t.Arch, t.Arm)
	p := PlannedCommand{BuildID: b.ID, Target: t, Timeout: timeout}

	out, err := renderPath("output", b.Output, vars)
	if err != nil {
		return p, err
	}
	p.Output = out

	p.Dir = s.rootDir
	if b.Dir != "" {
		dir, err := renderPath("dir", b.Dir, vars)
		if err != nil {
			return p, err
		}
		st, err := s.root.Stat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return p, fmt.Errorf("dir %q does not exist", dir)
		case err != nil:
			return p, fmt.Errorf("dir %q must be a directory inside the project root: %w", dir, err)
		case !st.IsDir():
			return p, fmt.Errorf("dir %q is not a directory", dir)
		}
		p.Dir = filepath.Join(s.rootDir, filepath.FromSlash(dir))
	}

	switch {
	case len(b.Command.Args) > 0:
		for i, a := range b.Command.Args {
			r, err := tmpl.Render(a, vars)
			if err != nil {
				return p, fmt.Errorf("command[%d]: %w", i, err)
			}
			p.Argv = append(p.Argv, r)
		}
		if p.Argv[0] == "" {
			return p, errors.New("command: program name renders to an empty string")
		}
	case b.Command.Shell != "":
		script, err := tmpl.Render(b.Command.Shell, vars)
		if err != nil {
			return p, fmt.Errorf("command: %w", err)
		}
		if strings.TrimSpace(script) == "" {
			return p, errors.New("command: renders to an empty script")
		}
		p.Argv = shellArgv(script)
	default:
		return p, errors.New("command is empty")
	}

	for _, e := range s.in.Config.Env {
		k, v, ok := strings.Cut(string(e), "=")
		if !ok || k == "" {
			return p, fmt.Errorf("env entry %q must be KEY=VALUE", e)
		}
		p.Env = append(p.Env, k+"="+v)
	}
	keys := make([]string, 0, len(b.Env))
	for k := range b.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return p, fmt.Errorf("env: invalid variable name %q", k)
		}
		v, err := tmpl.Render(b.Env[k], vars)
		if err != nil {
			return p, fmt.Errorf("env.%s: %w", k, err)
		}
		p.Env = append(p.Env, k+"="+v)
	}
	p.Env = append(p.Env,
		EnvOS+"="+t.OS,
		EnvArch+"="+t.Arch,
		EnvArm+"="+t.Arm,
		EnvOutput+"="+filepath.Join(s.rootDir, filepath.FromSlash(out)),
	)
	for _, kv := range p.Env {
		if strings.ContainsRune(kv, 0) {
			k, _, _ := strings.Cut(kv, "=")
			return p, fmt.Errorf("env.%s contains a NUL byte", k)
		}
	}
	for _, a := range p.Argv {
		if strings.ContainsRune(a, 0) {
			return p, errors.New("command contains a NUL byte")
		}
	}
	return p, nil
}

// execute runs one planned command and verifies its output.
func (s *stage) execute(ctx context.Context, b *config.Build, p PlannedCommand) (manifest.Artifact, error) {
	if err := s.prepareOutput(p.Output); err != nil {
		return manifest.Artifact{}, err
	}
	env := mergeEnv(s.in.Environ, p.Env)
	prog, err := resolveProgram(p.Argv[0], p.Dir, env)
	if err != nil {
		return manifest.Artifact{}, err
	}

	tctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, prog, p.Argv[1:]...)
	cmd.Args[0] = p.Argv[0]
	cmd.Dir = p.Dir
	cmd.Env = env
	cmd.Stdin = nil // the null device: builds are never interactive
	cmd.Stdout = s.in.Stdout
	tail := &tailBuffer{max: stderrTail}
	cmd.Stderr = io.MultiWriter(s.in.Stderr, tail)
	cmd.WaitDelay = waitDelay
	configureProcess(cmd)

	runErr := cmd.Run()
	switch {
	case runErr == nil:
	case tctx.Err() == context.DeadlineExceeded && ctx.Err() == nil:
		return manifest.Artifact{}, fmt.Errorf("command timed out after %s and was killed with its process group (raise timeout on the build if it needs longer)%s", p.Timeout, tail.quote())
	case ctx.Err() != nil:
		return manifest.Artifact{}, fmt.Errorf("command was cancelled: %w", ctx.Err())
	default:
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			status := fmt.Sprintf("exit code %d", ee.ExitCode())
			if ee.ExitCode() < 0 {
				status = ee.ProcessState.String() // killed by a signal
			}
			return manifest.Artifact{}, fmt.Errorf("command %s failed with %s%s", quoteArgv(p.Argv), status, tail.quote())
		}
		if errors.Is(runErr, exec.ErrWaitDelay) {
			return manifest.Artifact{}, fmt.Errorf("command exited but a child process kept its output open for more than %s; do not leave background processes running", waitDelay)
		}
		return manifest.Artifact{}, fmt.Errorf("cannot run command %s: %w", quoteArgv(p.Argv), runErr)
	}

	size, sum, err := verifyFile(s.root, p.Output, s.maxFileSize())
	if errors.Is(err, errMissing) {
		return manifest.Artifact{}, fmt.Errorf("command succeeded but did not create output %q (HEPHAESTUS_OUTPUT); Hephaestus does not cross-compile: the command must build target %s and write it there", p.Output, p.Target)
	}
	if err != nil {
		return manifest.Artifact{}, fmt.Errorf("output: %w", err)
	}
	return binaryArtifact(b, config.ProviderCommand, "", p.Output, p.Target, size, sum), nil
}

// prepareOutput removes a previous output and creates its parent directory,
// all within the project root.
func (s *stage) prepareOutput(rel string) error {
	st, err := s.root.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("output %q: %w", rel, err)
	case st.IsDir():
		return fmt.Errorf("output %q is an existing directory", rel)
	default:
		if err := s.root.Remove(rel); err != nil {
			return fmt.Errorf("remove previous output %q: %w", rel, err)
		}
	}
	for _, dir := range pathutil.Parents(rel) {
		err := s.root.Mkdir(dir, 0o755)
		if err == nil || errors.Is(err, fs.ErrExist) {
			st, serr := s.root.Stat(dir)
			if serr != nil {
				return fmt.Errorf("output directory %q: %w", dir, serr)
			}
			if !st.IsDir() {
				return fmt.Errorf("output directory %q is not a directory", dir)
			}
			continue
		}
		return fmt.Errorf("create output directory %q: %w", dir, err)
	}
	return nil
}

// mergeEnv returns base with overrides applied in order; a later value for
// the same key wins. Keys compare case-insensitively on Windows.
func mergeEnv(base, overrides []string) []string {
	over := map[string]bool{}
	for _, kv := range overrides {
		k, _, _ := strings.Cut(kv, "=")
		over[envKey(k)] = true
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || over[envKey(k)] {
			continue
		}
		out = append(out, kv)
	}
	// Later overrides win: keep only the last occurrence of each key.
	last := map[string]int{}
	for i, kv := range overrides {
		k, _, _ := strings.Cut(kv, "=")
		last[envKey(k)] = i
	}
	for i, kv := range overrides {
		k, _, _ := strings.Cut(kv, "=")
		if last[envKey(k)] == i {
			out = append(out, kv)
		}
	}
	return out
}

// lookupEnv returns the last value of key in env.
func lookupEnv(env []string, key string) (string, bool) {
	val, found := "", false
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && envKey(k) == envKey(key) {
			val, found = v, true
		}
	}
	return val, found
}

// resolveProgram finds the program to execute. A name containing a path
// separator is used as is (relative to dir); a bare name is searched in the
// PATH of the command's own environment, not Hephaestus's. Relative PATH
// entries are ignored so a program is never picked up from the working
// directory by accident.
func resolveProgram(name, dir string, env []string) (string, error) {
	if strings.ContainsAny(name, `/\`) {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		return p, nil
	}
	path, _ := lookupEnv(env, "PATH")
	for _, d := range filepath.SplitList(path) {
		if d == "" || !filepath.IsAbs(d) {
			continue
		}
		if p, ok := findExecutable(filepath.Join(d, name), env); ok {
			return p, nil
		}
	}
	return "", fmt.Errorf("program %q not found in the command's PATH", name)
}

func quoteArgv(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = fmt.Sprintf("%q", a)
	}
	return "[" + strings.Join(q, " ") + "]"
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max       int
	buf       []byte
	truncated bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= t.max {
		t.truncated = t.truncated || len(t.buf) > 0 || len(p) > t.max
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		return n, nil
	}
	if over := len(t.buf) + len(p) - t.max; over > 0 {
		t.buf = t.buf[over:]
		t.truncated = true
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

// quote formats the captured stderr for an error message.
func (t *tailBuffer) quote() string {
	s := strings.TrimRight(string(t.buf), "\r\n")
	if strings.TrimSpace(s) == "" {
		return " (no stderr output)"
	}
	head := "stderr"
	if t.truncated {
		head = fmt.Sprintf("stderr (last %d bytes)", t.max)
	}
	return "\n" + head + ":\n" + s
}
