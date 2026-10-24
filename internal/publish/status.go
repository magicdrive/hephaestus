package publish

import (
	"context"
	"fmt"
	"slices"
)

// Status is what exists for a version, observed read-only.
type Status struct {
	LocalTag  string // commit, "" when absent
	RemoteTag string // commit, "" when absent
	Run       *Run
	Release   *Release
	// Verify is set when the release is published.
	Verify *Report
	State  State
	Detail string
}

// Observe reports the state of version pl.Version from git and GitHub. It
// never writes. When the release is published it is fully verified
// (assets, checksums, attestations, tap).
func Observe(ctx context.Context, d *Deps, p *Project, pl *Plan) (*Status, error) {
	tag := pl.Version.Tag
	s := &Status{}
	var err error
	if s.LocalTag, err = d.Git.Commit(ctx, "refs/tags/"+tag); err != nil {
		return nil, err
	}
	remote, exists, err := d.Git.RemoteTag(ctx, tag)
	if err != nil {
		return nil, err
	}
	if exists {
		s.RemoteTag = remote
	}
	rels, err := d.GH.Releases(ctx, p.Repo)
	if err != nil {
		return nil, err
	}
	if i := slices.IndexFunc(rels, func(r Release) bool { return r.TagName == tag }); i >= 0 {
		s.Release = &rels[i]
	}
	if s.RemoteTag != "" {
		runs, err := d.GH.Runs(ctx, p.Repo, p.ReleaseFile, "push", tag, s.RemoteTag)
		if err != nil {
			return nil, err
		}
		for i := range runs {
			r := runs[i]
			if r.HeadBranch == tag && r.HeadSHA == s.RemoteTag && r.Event == "push" && (s.Run == nil || r.CreatedAt.After(s.Run.CreatedAt)) {
				s.Run = &r
			}
		}
	}

	switch {
	case s.RemoteTag == "" && s.LocalTag == "":
		s.State, s.Detail = NotStarted, "no tag"
		if s.Release != nil {
			s.State, s.Detail = Failed, "a release exists without a tag (a leftover draft?): "+s.Release.HTMLURL
		}
	case s.RemoteTag == "":
		s.State, s.Detail = TagCreatedLocally, "the tag exists only locally; nothing was published"
	case s.LocalTag != "" && s.LocalTag != s.RemoteTag:
		s.State, s.Detail = Failed, fmt.Sprintf("local tag -> %s differs from origin -> %s", short(s.LocalTag), short(s.RemoteTag))
	case s.Run == nil:
		s.State, s.Detail = TagPushed, "no release run found for the tag"
	case s.Run.Status != "completed":
		s.State, s.Detail = WorkflowRunning, "release run is "+s.Run.Status
	case s.Run.Conclusion != "success":
		s.State, s.Detail = Failed, "release run concluded "+s.Run.Conclusion
		if s.Release != nil && s.Release.Draft {
			s.Detail += "; a draft release was left (re-running the failed jobs resumes it)"
		} else if s.Release != nil {
			s.Detail += "; the release is published"
		}
	case s.Release == nil:
		s.State, s.Detail = Failed, "release run succeeded but there is no release"
	case s.Release.Draft:
		s.State, s.Detail = Failed, "release run succeeded but the release is still a draft"
	default:
		s.State = WorkflowSucceeded
		s.Verify = VerifyRelease(ctx, d, p, VerifyInput{Plan: pl, Commit: s.RemoteTag, Run: s.Run})
		if s.Verify.OK() {
			s.State, s.Detail = Completed, "released and verified"
		} else {
			s.State, s.Detail = Failed, "the release does not verify"
		}
	}
	return s, nil
}

// Print writes the status.
func (s *Status) Print(d *Deps, pl *Plan) {
	tagState := func(c string) string {
		if c == "" {
			return "absent"
		}
		return c
	}
	d.logf("Version:      %s (%s)", pl.Version.Tag, pl.Version.Kind())
	d.logf("Local tag:    %s", tagState(s.LocalTag))
	d.logf("Remote tag:   %s", tagState(s.RemoteTag))
	if s.Run != nil {
		d.logf("Release run:  %d %s/%s %s", s.Run.ID, s.Run.Status, orDefault(s.Run.Conclusion, "-"), s.Run.HTMLURL)
	} else {
		d.logf("Release run:  none")
	}
	if s.Release != nil {
		kind := "published"
		if s.Release.Draft {
			kind = "draft"
		}
		d.logf("Release:      %s, prerelease %t, %d assets %s", kind, s.Release.Prerelease, len(s.Release.Assets), s.Release.HTMLURL)
	} else {
		d.logf("Release:      none")
	}
	tap := "skipped for this version"
	if !pl.TapSkipped() {
		tap = "published by pull request"
	}
	d.logf("Homebrew tap: %s", tap)
	if s.Verify != nil {
		d.logf("Verification (assets, checksums, attestations, tap):")
		s.Verify.Print(d.Out)
	}
	d.logf("State:        %s (%s)", s.State, s.Detail)
}
