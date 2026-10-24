package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Runner runs an external command (git or gh) and returns its standard
// output. A failure is a *CmdError. Tests replace it with a fake.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// CmdError is a failed command.
type CmdError struct {
	Cmd    string
	Code   int // exit code; -1 when the command did not run
	Stderr string
}

func (e *CmdError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", e.Code)
	}
	return e.Cmd + ": " + msg
}

// ExecRunner runs commands with os/exec in the current directory.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err == nil {
		return out.Bytes(), nil
	}
	ce := &CmdError{Cmd: name + " " + firstArgs(args), Code: -1, Stderr: errb.String()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		ce.Code = ee.ExitCode()
	} else if ce.Stderr == "" {
		ce.Stderr = err.Error()
	}
	return out.Bytes(), ce
}

// firstArgs names a command for error messages without its whole argument
// list (which may be long).
func firstArgs(args []string) string {
	if len(args) > 3 {
		args = args[:3]
	}
	return strings.Join(args, " ")
}

// exitCode returns the exit code of a *CmdError, or -1.
func exitCode(err error) int {
	var ce *CmdError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return -1
}

var httpStatusRe = regexp.MustCompile(`\(HTTP (\d{3})\)`)

// httpStatus extracts the HTTP status gh reports ("... (HTTP 404)"), or 0.
func httpStatus(err error) int {
	var ce *CmdError
	if !errors.As(err, &ce) {
		return 0
	}
	if m := httpStatusRe.FindStringSubmatch(ce.Stderr); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// ErrNotFound is returned for a GitHub resource that does not exist (or
// that the credentials cannot see).
var ErrNotFound = errors.New("not found")

// transient reports whether a gh failure is worth retrying: no HTTP status
// (network), 5xx, 429, or a rate-limit 403.
func transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ce *CmdError
	if !errors.As(err, &ce) {
		return false
	}
	switch s := httpStatus(err); {
	case s == 0:
		// gh ran but got no HTTP response: a network problem.
		return ce.Code > 0
	case s >= 500, s == 429:
		return true
	case s == 403:
		return strings.Contains(strings.ToLower(ce.Stderr), "rate limit")
	}
	return false
}
