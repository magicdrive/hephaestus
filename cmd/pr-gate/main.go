// Command pr-gate decides whether a pull request may be merged and, only
// with -merge, merges it pinned to the expected head commit. Without -merge
// it is read-only (a dry run): it reports every condition and exits 0 only
// when all of them pass. See internal/prgate for the conditions and
// docs/pr-gate.md for usage.
//
//	GH_TOKEN=$(gh auth token) pr-gate -repo OWNER/REPO -pr N \
//	    -head HEAD_SHA -base main -base-sha BASE_SHA [-require NAME]... [-merge]
//
// Exit status: 0 the gate passed (and, with -merge, the pull request was
// merged and the merge commit verified); 1 a condition failed (nothing was
// merged); 2 usage error; 3 a condition could not be decided (API error or
// unknown state; nothing was merged); 4 the merge request was sent and the
// result needs a human: merged but not verified, or unknown.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/magicdrive/hephaestus/internal/github"
	"github.com/magicdrive/hephaestus/internal/prgate"
)

// Exit statuses.
const (
	exitPass      = 0
	exitFail      = 1
	exitUsage     = 2
	exitUndecided = 3
	exitAlarm     = 4
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, nil)
	stop()
	os.Exit(code)
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// run is main without the process: clientOpts, when set, adjusts the GitHub
// client (tests).
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, clientOpts func(*github.Options)) int {
	fs := flag.NewFlagSet("pr-gate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		cfg     prgate.Config
		repo    string
		require multiFlag
	)
	fs.StringVar(&repo, "repo", "", "the repository, OWNER/REPO (required)")
	fs.IntVar(&cfg.Number, "pr", 0, "the pull request number (required)")
	fs.StringVar(&cfg.HeadSHA, "head", "", "the expected head commit, full SHA (required)")
	fs.StringVar(&cfg.BaseRef, "base", "main", "the expected base branch")
	fs.StringVar(&cfg.BaseSHA, "base-sha", "", "the expected base branch commit, full SHA (required)")
	fs.StringVar(&cfg.CIGate, "ci-gate", prgate.DefaultCIGate, "the check that aggregates CI; it must succeed")
	fs.Int64Var(&cfg.CIGateApp, "ci-gate-app", prgate.GitHubActionsAppID, "the app ID that must report the CI gate check (0: any source)")
	fs.Var(&require, "require", "a check that must succeed besides the branch's required checks (repeatable; needed when the branch requires none)")
	fs.IntVar(&cfg.MergeableReads, "mergeable-reads", prgate.DefaultMergeableRead, "reads of the pull request while GitHub computes mergeability")
	fs.DurationVar(&cfg.MergeableWait, "mergeable-wait", prgate.DefaultMergeableWait, "wait between those reads")
	merge := fs.Bool("merge", false, "merge when every condition passes (default: dry run, read-only)")
	method := fs.String("method", "merge", "merge method with -merge: merge or squash")
	jsonOut := fs.Bool("json", false, "print the report as JSON")
	apiURL := fs.String("api-url", "", "GitHub API URL (default https://api.github.com)")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall time limit")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: pr-gate -repo OWNER/REPO -pr N -head SHA -base-sha SHA [-base main] [-require NAME]... [-merge]")
		fmt.Fprintln(stderr, "The token is read from GH_TOKEN or GITHUB_TOKEN. Without -merge nothing is written.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "pr-gate: unexpected arguments: %s\n", strings.Join(fs.Args(), " "))
		return exitUsage
	}
	cfg.Require = require
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || strings.Contains(name, "/") {
		fmt.Fprintf(stderr, "pr-gate: -repo %q must be OWNER/REPO\n", repo)
		return exitUsage
	}
	cfg.Owner, cfg.Repo = owner, name
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "pr-gate: %v\n", err)
		return exitUsage
	}
	if *method != "merge" && *method != "squash" {
		fmt.Fprintf(stderr, "pr-gate: -method must be merge or squash\n")
		return exitUsage
	}
	token := getenv("GH_TOKEN")
	if token == "" {
		token = getenv("GITHUB_TOKEN")
	}
	if token == "" {
		fmt.Fprintln(stderr, "pr-gate: no token: set GH_TOKEN (for example GH_TOKEN=$(gh auth token)) or GITHUB_TOKEN")
		return exitUsage
	}
	o := github.Options{Token: token, APIURL: *apiURL, UserAgent: "hephaestus-pr-gate"}
	if clientOpts != nil {
		clientOpts(&o)
	}
	gh, err := github.New(o)
	if err != nil {
		fmt.Fprintf(stderr, "pr-gate: %v\n", err)
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	g := &prgate.Gate{GH: gh}
	// The verdict line goes with the report, except that JSON keeps stdout
	// to itself.
	say := stdout
	if *jsonOut {
		say = stderr
	}

	if !*merge {
		rep, err := g.Evaluate(ctx, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "pr-gate: %v\n", err)
			return exitUsage
		}
		printReport(stdout, rep, nil, *jsonOut)
		if rep.Passed {
			fmt.Fprintln(say, "GATE PASSED (dry run: nothing was merged; add -merge to merge)")
			return exitPass
		}
		return failCode(rep, stderr)
	}

	unlock, err := lock(owner, name, cfg.BaseRef)
	if err != nil {
		fmt.Fprintf(stderr, "pr-gate: %v\n", err)
		return exitUndecided
	}
	defer unlock()
	out, err := g.Merge(ctx, cfg, *method)
	if err != nil {
		fmt.Fprintf(stderr, "pr-gate: %v\n", err)
		return exitUsage
	}
	printReport(stdout, out.Report, out, *jsonOut)
	switch {
	case out.Merged && out.Verified:
		fmt.Fprintf(say, "MERGED %s#%d as %s (parents verified)\n", out.Report.Repository, cfg.Number, out.MergeCommitSHA)
		return exitPass
	case out.Merged:
		fmt.Fprintf(say, "MERGED BUT NOT VERIFIED %s#%d: %s\n", out.Report.Repository, cfg.Number, out.Problem)
		return exitAlarm
	case out.Unknown:
		fmt.Fprintf(say, "MERGE RESULT UNKNOWN %s#%d: %s\n", out.Report.Repository, cfg.Number, out.Problem)
		return exitAlarm
	case out.Attempted:
		fmt.Fprintf(say, "NOT MERGED %s#%d: %s\n", out.Report.Repository, cfg.Number, out.Problem)
		return exitFail
	case !out.Report.Passed:
		return failCode(out.Report, stderr)
	default:
		fmt.Fprintf(say, "NOT MERGED %s#%d: %s\n", out.Report.Repository, cfg.Number, out.Problem)
		return exitFail
	}
}

func failCode(rep *prgate.Report, stderr io.Writer) int {
	for _, f := range rep.Failed() {
		if f.Status == prgate.Fail {
			fmt.Fprintln(stderr, "GATE FAILED")
			return exitFail
		}
	}
	fmt.Fprintln(stderr, "GATE UNDECIDED (an API error or unknown state; nothing was merged)")
	return exitUndecided
}

func printReport(w io.Writer, rep *prgate.Report, out *prgate.MergeOutcome, asJSON bool) {
	if asJSON {
		var v any = rep
		if out != nil {
			v = out
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v) // nothing actionable when stdout fails
		return
	}
	fmt.Fprintf(w, "pr-gate %s#%d head=%s base=%s@%s\n", rep.Repository, rep.Number, rep.HeadSHA, rep.BaseRef, rep.BaseSHA)
	fmt.Fprintf(w, "required: %s\n", strings.Join(rep.Required, "; "))
	for _, f := range rep.Findings {
		fmt.Fprintf(w, "%-5s %-18s %s\n", f.Status, f.ID, f.Detail)
	}
}

// lock keeps two pr-gate processes on this machine from merging into the
// same base branch at once: the second would merge on a base the first just
// moved. It is advisory and local; the parent check after the merge is what
// detects a base moved by anyone else.
func lock(owner, repo, base string) (func(), error) {
	name := "pr-gate-" + strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(owner+"-"+repo+"-"+base) + ".lock"
	path := filepath.Join(os.TempDir(), name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("another pr-gate is merging into %s/%s %s (lock %s); if none is running, remove the lock", owner, repo, base, path)
		}
		return nil, fmt.Errorf("lock: %w", err)
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	_ = f.Close() // the lock is the file's existence
	return func() { _ = os.Remove(path) }, nil
}
