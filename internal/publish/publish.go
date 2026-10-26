package publish

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// State is how far a publish got. After TagPushed every state is
// observable on GitHub, and nothing is rolled back: a pushed tag starts the
// release workflow, which publishes on its own.
type State string

const (
	NotStarted         State = "NotStarted"
	PreflightPassed    State = "PreflightPassed"
	TagCreatedLocally  State = "TagCreatedLocally"
	TagPushed          State = "TagPushed"
	WorkflowDiscovered State = "WorkflowDiscovered"
	WorkflowRunning    State = "WorkflowRunning"
	WorkflowSucceeded  State = "WorkflowSucceeded"
	ReleaseVerified    State = "ReleaseVerified"
	Completed          State = "Completed"
	Failed             State = "Failed"
)

// Outcome is the result of publish or watch.
type Outcome struct {
	// State is the last state reached; FailedAt is set with State Failed.
	State    State
	FailedAt State
	Err      error
	// MonitorLost is true when the outcome on GitHub is unknown because
	// watching failed (network, timeout, interrupt), not the release.
	MonitorLost bool
	Commit      string
	Run         *Run
	Next        []string // what to do next
}

// ExitCode maps an outcome to a process exit code: 0 completed, 1 failed,
// 3 unknown (monitoring stopped; the release may still be running). make
// reports any non-zero status as its own failure.
func (o *Outcome) ExitCode() int {
	switch {
	case o.State == Completed:
		return 0
	case o.MonitorLost:
		return 3
	}
	return 1
}

// Options tune publish and watch.
type Options struct {
	// Yes skips the confirmation prompt (explicit non-interactive mode).
	Yes bool
	// DiscoverTimeout bounds the wait for the release run to appear.
	DiscoverTimeout time.Duration
	// WatchTimeout bounds the wait for the run to finish.
	WatchTimeout time.Duration
	// Poll is the polling interval.
	Poll time.Duration
	// QueuedWarning is how long a run may stay queued before a warning.
	QueuedWarning time.Duration
	// MaxAPIErrors is how many consecutive API failures end monitoring.
	MaxAPIErrors int
}

func (o *Options) defaults() {
	if o.DiscoverTimeout == 0 {
		o.DiscoverTimeout = 5 * time.Minute
	}
	if o.WatchTimeout == 0 {
		o.WatchTimeout = 90 * time.Minute
	}
	if o.Poll == 0 {
		o.Poll = 15 * time.Second
	}
	if o.QueuedWarning == 0 {
		o.QueuedWarning = 10 * time.Minute
	}
	if o.MaxAPIErrors == 0 {
		o.MaxAPIErrors = 8
	}
}

// ErrDeclined is returned when the confirmation is not "y".
var ErrDeclined = errors.New("not confirmed: nothing was created or pushed")

// Publish releases pl.Version: preflight, confirmation, annotated tag,
// tag push, then it finds and watches the release workflow run that the
// push started and verifies the published release. The release workflow
// is the only publisher; Publish's only writes are the tag (local and
// pushed) and a fetch of origin/<the release branch>.
func Publish(ctx context.Context, d *Deps, p *Project, pl *Plan, opts Options) *Outcome {
	opts.defaults()
	o := &Outcome{State: NotStarted}
	tag := pl.Version.Tag
	fail := func(err error, next ...string) *Outcome {
		o.FailedAt, o.State, o.Err = o.State, Failed, err
		o.Next = append(o.Next, next...)
		return o
	}

	unlock, err := lock(ctx, d)
	if err != nil {
		return fail(err)
	}
	defer unlock()

	d.logf("==> preflight %s", tag)
	rep := Preflight(ctx, d, p, pl)
	rep.Print(d.Out)
	if !rep.OK() {
		return fail(errors.New("preflight failed: nothing was created or pushed"),
			"fix the failed checks and run "+d.Command("publish")+" "+tag+" again",
			d.Command("status")+" "+tag+" shows an existing tag, run or release")
	}
	o.State, o.Commit = PreflightPassed, rep.Commit

	if err := confirm(d, p, pl, rep.Commit, opts.Yes); err != nil {
		return fail(err)
	}

	d.logf("==> creating annotated tag %s at %s", tag, rep.Commit)
	if err := d.Git.CreateTag(ctx, tag, rep.Commit, "Release "+tag); err != nil {
		return fail(fmt.Errorf("create tag: %w", err), "nothing was pushed; check `git tag -l "+tag+"`")
	}
	if c, err := d.Git.Commit(ctx, "refs/tags/"+tag); err != nil || c != rep.Commit {
		o.State = TagCreatedLocally
		return fail(fmt.Errorf("tag %s points to %q, want %s (%v)", tag, c, rep.Commit, err),
			"the tag was not pushed; inspect it and delete the local tag with `git tag -d "+tag+"`")
	}
	o.State = TagCreatedLocally

	// Re-check right before the irreversible step: origin/<branch> and the
	// remote tag may have changed while the prompt was open.
	if err := d.Git.FetchBranch(ctx, p.Branch); err != nil {
		return fail(fmt.Errorf("re-fetch origin/%s: %w", p.Branch, err), unpushedHint(d, tag)...)
	}
	if remote, err := d.Git.Commit(ctx, "refs/remotes/origin/"+p.Branch); err != nil || remote != rep.Commit {
		return fail(fmt.Errorf("origin/%s moved to %q since the preflight (%v)", p.Branch, remote, err), unpushedHint(d, tag)...)
	}
	if c, exists, err := d.Git.RemoteTag(ctx, tag); err != nil || exists {
		return fail(fmt.Errorf("remote tag %s appeared (-> %q) or cannot be checked (%v)", tag, c, err), unpushedHint(d, tag)...)
	}

	d.logf("==> pushing tag %s (not forced)", tag)
	pushedAt := d.Now()
	// The push and the check of its result are not cut short by Ctrl-C:
	// whether the tag reached origin decides everything that follows.
	pctx := context.WithoutCancel(ctx)
	perr := d.Git.PushTag(pctx, tag)
	c, exists, rerr := d.Git.RemoteTag(pctx, tag)
	switch {
	case rerr != nil:
		// Unknown: the push may have reached GitHub.
		o.Err, o.MonitorLost = fmt.Errorf("cannot tell whether %s reached origin (push: %v; check: %w)", tag, perr, rerr), true
		o.Next = []string{"run " + d.Command("status") + " " + tag + " before anything else: if the tag is on origin, the release is running"}
		return o
	case !exists:
		if perr == nil {
			perr = errors.New("git reported success but origin has no such tag")
		}
		return fail(fmt.Errorf("push tag: %w", perr), unpushedHint(d, tag)...)
	case c != rep.Commit:
		o.State = TagPushed
		return fail(fmt.Errorf("origin has %s at %s, not %s", tag, c, rep.Commit), "do not move the tag; inspect with "+d.Command("status")+" "+tag)
	case perr != nil:
		d.logf("    git reported %v, but origin has %s at %s: continuing", perr, tag, short(rep.Commit))
	}
	o.State = TagPushed
	d.logf("    %s is on origin: the release workflow now runs on GitHub; it cannot be called back from here", tag)

	// The window allows for a local clock ahead of GitHub's; the tag and
	// commit identify the run on their own.
	return follow(ctx, d, p, pl, o, pushedAt.Add(-5*time.Minute), rep.TapHeads, opts)
}

// Watch follows the release of an already pushed tag: it finds the run,
// waits for it and verifies the release. It never creates or pushes tags.
func Watch(ctx context.Context, d *Deps, p *Project, pl *Plan, opts Options) *Outcome {
	opts.defaults()
	o := &Outcome{State: NotStarted}
	tag := pl.Version.Tag
	c, exists, err := d.Git.RemoteTag(ctx, tag)
	if err != nil || !exists {
		o.State, o.FailedAt, o.Err = Failed, NotStarted, fmt.Errorf("tag %s is not on origin (%v): nothing to watch", tag, err)
		o.Next = []string{d.Command("publish") + " " + tag + " creates and pushes it"}
		return o
	}
	o.State, o.Commit = TagPushed, c
	return follow(ctx, d, p, pl, o, time.Time{}, nil, opts)
}

func unpushedHint(d *Deps, tag string) []string {
	return []string{
		"the tag exists only locally and nothing was published",
		"inspect it with `git show " + tag + "`; delete it with `git tag -d " + tag + "` and run " + d.Command("publish") + " " + tag + " again",
	}
}

// follow discovers, watches and verifies the release run of o.Commit.
func follow(ctx context.Context, d *Deps, p *Project, pl *Plan, o *Outcome, since time.Time, tapHeads map[string]string, opts Options) *Outcome {
	tag := pl.Version.Tag
	lost := func(err error) *Outcome {
		o.Err, o.MonitorLost = err, true
		o.Next = append(o.Next,
			"monitoring stopped; this does not mean the release failed",
			"resume with "+d.Command("watch")+" "+tag+", or check "+d.Command("status")+" "+tag)
		if o.Run != nil {
			o.Next = append(o.Next, "run: "+o.Run.HTMLURL)
		} else {
			o.Next = append(o.Next, "runs: https://github.com/"+p.Repo.String()+"/actions/workflows/"+p.ReleaseFile)
		}
		return o
	}
	fail := func(err error, next ...string) *Outcome {
		o.FailedAt, o.State, o.Err = o.State, Failed, err
		o.Next = append(o.Next, next...)
		return o
	}

	d.logf("==> waiting for the %s run of %s", p.ReleaseFile, tag)
	run, err := discover(ctx, d, p, tag, o.Commit, since, opts)
	if err != nil {
		return lost(err)
	}
	o.Run, o.State = &run, WorkflowDiscovered
	d.logf("    run %d: %s", run.ID, run.HTMLURL)

	run, err = watch(ctx, d, p, run, o, opts)
	if err != nil {
		return lost(err)
	}
	o.Run = &run
	if run.Conclusion != "success" {
		return fail(fmt.Errorf("release run concluded %s: %s", run.Conclusion, run.HTMLURL), failureHints(ctx, d, p, tag, run)...)
	}
	o.State = WorkflowSucceeded

	d.logf("==> verifying the release")
	rep := VerifyRelease(ctx, d, p, VerifyInput{Plan: pl, Commit: o.Commit, Run: &run, TapHeads: tapHeads})
	rep.Print(d.Out)
	if rep.Inconclusive() {
		return lost(errors.New("the release run succeeded but verification could not complete (API or network)"))
	}
	if !rep.OK() {
		return fail(errors.New("the release workflow succeeded but the release does not verify"),
			"do not delete or replace published assets; investigate the failed checks",
			"if the release is wrong, release a new version (e.g. the next rc) instead of changing this one")
	}
	o.State = ReleaseVerified
	for _, t := range pl.Taps {
		if !t.Publish.Skip {
			o.Next = append(o.Next, "review and merge the formula pull request on "+t.Publish.Owner+"/"+t.Publish.Name+" (publish never merges it)")
		}
	}
	o.State = Completed
	return o
}

// discover finds the release workflow run for tag at commit: same
// workflow file, push event, head branch = tag, head SHA = commit, created
// after since. It never settles for "the latest run".
func discover(ctx context.Context, d *Deps, p *Project, tag, commit string, since time.Time, opts Options) (Run, error) {
	deadline := d.Now().Add(opts.DiscoverTimeout)
	errs := 0
	for {
		runs, err := d.GH.Runs(ctx, p.Repo, p.ReleaseFile, "push", tag, commit)
		if err != nil {
			if errs++; errs >= opts.MaxAPIErrors {
				return Run{}, fmt.Errorf("listing release runs failed %d times: %w", errs, err)
			}
		} else {
			errs = 0
			var match []Run
			for _, r := range runs {
				if r.Path == ".github/workflows/"+p.ReleaseFile && r.Event == "push" && r.HeadBranch == tag &&
					r.HeadSHA == commit && !r.CreatedAt.Before(since) {
					match = append(match, r)
				}
			}
			if len(match) > 0 {
				best := match[0]
				for _, r := range match[1:] {
					if r.CreatedAt.After(best.CreatedAt) {
						best = r
					}
				}
				if len(match) > 1 {
					d.logf("    warning: %d runs match %s at %s; following the newest, %d", len(match), tag, short(commit), best.ID)
				}
				return best, nil
			}
		}
		if !d.Now().Before(deadline) {
			return Run{}, fmt.Errorf("no %s run for %s at %s appeared within %s", p.ReleaseFile, tag, short(commit), opts.DiscoverTimeout)
		}
		if err := d.Sleep(ctx, opts.Poll); err != nil {
			return Run{}, err
		}
	}
}

// watch polls run until it completes.
func watch(ctx context.Context, d *Deps, p *Project, run Run, o *Outcome, opts Options) (Run, error) {
	deadline := d.Now().Add(opts.WatchTimeout)
	queuedSince, warned := d.Now(), false
	errs := 0
	last := ""
	for {
		if run.Status == "completed" {
			d.logf("    run %d completed: %s", run.ID, run.Conclusion)
			return run, nil
		}
		if run.Status != last {
			d.logf("    run %d is %s", run.ID, run.Status)
			last = run.Status
		}
		switch run.Status {
		case "queued", "pending", "requested", "waiting":
			if !warned && d.Now().Sub(queuedSince) >= opts.QueuedWarning {
				d.logf("    warning: run %d has been %s for %s (runner availability or an environment approval?)", run.ID, run.Status, opts.QueuedWarning)
				warned = true
			}
		default:
			o.State = WorkflowRunning
		}
		if !d.Now().Before(deadline) {
			return run, fmt.Errorf("run %d still %s after %s", run.ID, run.Status, opts.WatchTimeout)
		}
		if err := d.Sleep(ctx, opts.Poll); err != nil {
			return run, err
		}
		next, err := d.GH.RunByID(ctx, p.Repo, run.ID)
		if err != nil {
			if errs++; errs >= opts.MaxAPIErrors {
				return run, fmt.Errorf("reading run %d failed %d times: %w", run.ID, errs, err)
			}
			continue
		}
		errs = 0
		run = next
	}
}

// failureHints says what a failed run left behind and how to recover.
func failureHints(ctx context.Context, d *Deps, p *Project, tag string, run Run) []string {
	var hints []string
	if jobs, err := d.GH.Jobs(ctx, p.Repo, run.ID); err == nil {
		for _, j := range jobs {
			if j.Conclusion != "" && j.Conclusion != "success" && j.Conclusion != "skipped" {
				step := ""
				for _, s := range j.Steps {
					if s.Conclusion == "failure" {
						step = ": step " + s.Name
						break
					}
				}
				hints = append(hints, fmt.Sprintf("job %s %s%s", j.Name, j.Conclusion, step))
			}
		}
	}
	rels, err := d.GH.Releases(ctx, p.Repo)
	published, draft := false, false
	if err == nil {
		for _, r := range rels {
			if r.TagName == tag {
				published, draft = !r.Draft, r.Draft
			}
		}
	}
	switch {
	case published:
		hints = append(hints, "the release is published: do not delete it or replace assets; fix forward with a new version")
	case draft:
		hints = append(hints, "a draft release was left: re-running is safe and resumes it: gh run rerun "+fmt.Sprint(run.ID)+" --failed --repo "+p.Repo.String())
	default:
		hints = append(hints, "nothing was published; for a transient failure: gh run rerun "+fmt.Sprint(run.ID)+" --failed --repo "+p.Repo.String(),
			"for a code fix, merge it to main and release a new version; never move or delete the pushed tag")
	}
	return append(hints, "watch again with "+d.Command("watch")+" "+tag)
}

// confirm shows what is about to happen and asks; the default is no.
func confirm(d *Deps, p *Project, pl *Plan, commit string, yes bool) error {
	kind := "Stable"
	if pl.Prerelease {
		kind = "Prerelease (not marked latest)"
	}
	tap := "SKIP"
	if !pl.TapSkipped() {
		var to []string
		for _, t := range pl.Taps {
			if !t.Publish.Skip {
				to = append(to, t.Publish.Owner+"/"+t.Publish.Name)
			}
		}
		tap = "PUBLISH: pull request to " + strings.Join(to, ", ") + " (reviewed and merged by you)"
	}
	if len(pl.Taps) == 0 {
		tap = "none configured"
	}
	fmt.Fprintf(d.Out, `
Repository:     %s
Branch:         %s
Commit:         %s
Version:        %s
Release type:   %s
Assets:         %d (%s)
Homebrew Tap:   %s
GitHub Actions: %s

Pushing the tag starts the release workflow; a pushed tag is never moved or deleted.
`, p.Repo, p.Branch, commit, pl.Version.Tag, kind, len(pl.Assets()), strings.Join(pl.Assets(), ", "), tap, p.ReleaseFile)
	if yes {
		fmt.Fprintln(d.Out, "Create and push this release tag? yes (--yes)")
		return nil
	}
	if !d.Interactive || d.In == nil {
		return errors.New("confirmation needs a terminal; pass YES=1 (make) or --yes to confirm non-interactively")
	}
	fmt.Fprint(d.Out, "Create and push this release tag? [y/N] ")
	// Output may be held back until a newline (hephaestus redacts its
	// output line by line): the question must be visible before the read.
	if f, ok := d.Out.(interface{ Flush() }); ok {
		f.Flush()
	}
	line, _ := bufio.NewReader(d.In).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return ErrDeclined
}

func (d *Deps) logf(format string, args ...any) {
	fmt.Fprintf(d.Out, format+"\n", args...)
}

// lock prevents two publish processes in one clone (make -j, a second
// terminal). The lock file lives in the git directory.
func lock(ctx context.Context, d *Deps) (func(), error) {
	dir, err := d.Git.CommonDir(ctx)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "hephaestus-publish.lock")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("another publish is running (%s exists; remove it if no publish is running)", p)
		}
		return nil, err
	}
	_, werr := fmt.Fprintf(f, "pid %d\n", os.Getpid())
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(p)
		return nil, err
	}
	return func() { _ = os.Remove(p) }, nil
}
