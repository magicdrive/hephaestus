// Package preflight checks, before a release tag is pushed, that the
// release can safely run: the local git state, the GitHub repository's
// release settings (environment, rulesets, CI on the exact commit, secret
// names), the Hephaestus configuration for that tag, and the Homebrew tap.
//
// It is read-only by construction. Git runs only through gitCLI, which
// allows a fixed set of read-only subcommands with optional locks disabled
// (so `git status` does not even refresh the index). GitHub is reached only
// through the GitHub interface, whose methods are all GETs. Secret values
// are never requested: only secret names.
//
// Every check has a stable ID and returns one of PASS, FAIL, WARN, SKIP or
// UNKNOWN. UNKNOWN means the check could not be made (permissions, rate
// limit, network), which is not the same as finding a problem (FAIL).
package preflight

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/magicdrive/hephaestus/internal/config"
)

// Status is a check outcome.
type Status string

const (
	Pass    Status = "PASS"
	Fail    Status = "FAIL"
	Warn    Status = "WARN"
	Skip    Status = "SKIP"
	Unknown Status = "UNKNOWN"
)

// Severity is how much a check matters: a FAIL of an "error" check means
// the release must not be tagged; "warning" checks report risks (their
// problems are WARN, never FAIL).
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Result is one check's outcome. Evidence holds what the decision was based
// on (never a secret value).
type Result struct {
	ID          string         `json:"id"`
	Group       string         `json:"group"`
	Title       string         `json:"title"`
	Status      Status         `json:"status"`
	Severity    Severity       `json:"severity"`
	Message     string         `json:"message"`
	Evidence    map[string]any `json:"evidence,omitempty"`
	Remediation string         `json:"remediation,omitempty"`
}

// outcome is what a check function returns; the engine adds the identity.
type outcome struct {
	status   Status
	msg      string
	evidence map[string]any
	fix      string
}

type ev = map[string]any

func pass(msg string, e ev) outcome { return outcome{status: Pass, msg: msg, evidence: e} }
func fail(msg, fix string, e ev) outcome {
	return outcome{status: Fail, msg: msg, fix: fix, evidence: e}
}
func warn(msg, fix string, e ev) outcome {
	return outcome{status: Warn, msg: msg, fix: fix, evidence: e}
}
func skip(msg string) outcome { return outcome{status: Skip, msg: msg} }
func unknownf(format string, a ...any) outcome {
	return outcome{status: Unknown, msg: fmt.Sprintf(format, a...)}
}

// def is a registered check.
type def struct {
	id, group, title string
	severity         Severity
	// needs are checks that must have passed (PASS or WARN) for this one to
	// run; otherwise it is skipped.
	needs []string
	// network marks checks that read GitHub or the git remote; --offline
	// skips them.
	network bool
	run     func(*state) outcome
}

// Checks lists every check ID in execution order, for documentation and
// for validating --skip.
func Checks() []string {
	var out []string
	for _, d := range registry {
		out = append(out, d.id)
	}
	return out
}

// Input is everything a preflight run reads.
type Input struct {
	Tag string
	// Config is the loaded configuration; ConfigErr is set instead when it
	// could not be loaded (config.load fails, config checks are skipped).
	Config     *config.Config
	ConfigErr  error
	ConfigPath string

	// Repo is "owner/name"; empty derives it from the configuration, then
	// from the git remote.
	Repo string
	// Settings from the command line, which override the configuration's
	// preflight section.
	Remote, Branch, Workflow, Environment, TapTokenSecret string
	Secrets, Skip                                         []string

	// Offline skips every check that needs the network.
	Offline bool
	// TokenPresent says whether GitHub requests are authenticated.
	TokenPresent bool

	Git    Git
	GitHub GitHub // nil when offline
}

// SchemaVersion is the version of the JSON report format.
const SchemaVersion = 1

// Report is the outcome of a run.
type Report struct {
	SchemaVersion int            `json:"schema_version"`
	Tool          string         `json:"tool"`
	ToolVersion   string         `json:"tool_version"`
	Repository    string         `json:"repository,omitempty"`
	Tag           string         `json:"tag"`
	Version       string         `json:"version,omitempty"`
	Prerelease    *bool          `json:"prerelease,omitempty"`
	Commit        string         `json:"commit,omitempty"`
	Strict        bool           `json:"strict"`
	Offline       bool           `json:"offline"`
	Status        string         `json:"status"` // pass, warn, unknown, fail
	Summary       map[Status]int `json:"summary"`
	Checks        []Result       `json:"checks"`
}

// Overall statuses.
const (
	OverallPass    = "pass"
	OverallWarn    = "warn"
	OverallUnknown = "unknown"
	OverallFail    = "fail"
)

// Exit codes of `hephaestus preflight`.
const (
	ExitPass       = 0 // no FAIL (strict: no WARN or UNKNOWN either)
	ExitFail       = 1 // a FAIL, or with --strict a WARN or UNKNOWN
	ExitUsage      = 2 // invalid arguments
	ExitIncomplete = 3 // no FAIL but UNKNOWN checks (not strict)
)

// ExitCode maps the report to the process exit code.
func (r *Report) ExitCode() int {
	switch r.Status {
	case OverallFail:
		return ExitFail
	case OverallUnknown:
		if r.Strict {
			return ExitFail
		}
		return ExitIncomplete
	case OverallWarn:
		if r.Strict {
			return ExitFail
		}
	}
	return ExitPass
}

// ErrUnknownCheck is returned for a --skip or preflight.skip entry that
// names no check.
var ErrUnknownCheck = errors.New("unknown check ID")

// Run executes every check in registry order. It never returns an error for
// a failed check; only invalid input is an error.
func Run(ctx context.Context, in Input, strict bool, toolVersion string) (*Report, error) {
	s := newState(ctx, in)
	ids := Checks()
	for _, id := range s.settings.skip {
		if !slices.Contains(ids, id) {
			return nil, fmt.Errorf("%w %q (known: %s)", ErrUnknownCheck, id, strings.Join(ids, ", "))
		}
	}
	rep := &Report{
		SchemaVersion: SchemaVersion, Tool: "hephaestus", ToolVersion: toolVersion,
		Tag: in.Tag, Strict: strict, Offline: in.Offline, Summary: map[Status]int{},
	}
	for _, d := range registry {
		o := s.runOne(d)
		r := Result{ID: d.id, Group: d.group, Title: d.title, Status: o.status, Severity: d.severity,
			Message: o.msg, Evidence: o.evidence, Remediation: o.fix}
		if r.Status == Fail && d.severity == SeverityWarning {
			r.Status = Warn
		}
		s.results[d.id] = r
		rep.Checks = append(rep.Checks, r)
		rep.Summary[r.Status]++
	}
	rep.Repository = s.repoFullName()
	rep.Commit = s.head
	if s.tagOK {
		rep.Version = s.info.Version
		p := s.prerelease
		rep.Prerelease = &p
	}
	switch {
	case rep.Summary[Fail] > 0:
		rep.Status = OverallFail
	case rep.Summary[Unknown] > 0:
		rep.Status = OverallUnknown
	case rep.Summary[Warn] > 0:
		rep.Status = OverallWarn
	default:
		rep.Status = OverallPass
	}
	return rep, nil
}

func (s *state) runOne(d def) outcome {
	if slices.Contains(s.settings.skip, d.id) {
		return skip("skipped by configuration (--skip or preflight.skip)")
	}
	for _, n := range d.needs {
		if r, ok := s.results[n]; ok && r.Status != Pass && r.Status != Warn {
			return skip(fmt.Sprintf("requires %s, which is %s", n, r.Status))
		}
	}
	if d.network && s.in.Offline {
		return skip("offline")
	}
	return d.run(s)
}
