// Package status reports, read-only, where the release of a tag stands:
// the tag on GitHub, the release (none, draft, published, immutable), its
// assets compared with the local Artifact Manifest, and each formula's tap
// pull request. It decides the recovery state and the next safe action. It
// only issues GET requests.
package status

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/github"
	"github.com/magicdrive/hephaestus/internal/homebrew"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// GitHub is the read-only access status needs (all GETs).
type GitHub interface {
	GetRef(ctx context.Context, owner, repo, ref string) (*github.Reference, error)
	ListReleases(ctx context.Context, owner, repo string) ([]github.Release, error)
	ListPulls(ctx context.Context, owner, repo string, o github.ListPullsOptions) ([]github.PullRequest, error)
}

// States of a release, from nothing done to done.
const (
	NotStarted          = "not-started"          // no tag, no release
	Tagged              = "tagged"               // tag pushed, no release yet (workflow running or failed before the release)
	Draft               = "draft"                // draft release: a re-run resumes it
	Conflict            = "conflict"             // an asset differs from the local manifest
	PublishedIncomplete = "published-incomplete" // published, assets missing
	Published           = "published"            // published, assets complete (as far as known)
	Completed           = "completed"            // published and every tap pull request merged
)

// Asset compares one release asset with the local manifest.
type Asset struct {
	Name   string `json:"name"`
	Status string `json:"status"` // identical, different, missing, extra, incomplete, unknown
	Detail string `json:"detail,omitempty"`
}

// Tap is one formula's tap publication.
type Tap struct {
	Formula     string `json:"formula"`
	Repository  string `json:"repository"`
	Mode        string `json:"mode"`   // pull-request, direct, skipped
	Status      string `json:"status"` // none, open, merged, closed, skipped, unknown
	PullRequest string `json:"pull_request,omitempty"`
}

// Report is the status of one tag.
type Report struct {
	Repository string  `json:"repository"`
	Tag        string  `json:"tag"`
	TagCommit  string  `json:"tag_object,omitempty"`
	Release    string  `json:"release"` // none, draft, published
	Immutable  bool    `json:"immutable"`
	Prerelease bool    `json:"prerelease"`
	ReleaseURL string  `json:"release_url,omitempty"`
	Manifest   string  `json:"manifest"` // used, absent, other-version
	Assets     []Asset `json:"assets"`
	Taps       []Tap   `json:"taps"`
	State      string  `json:"state"`
	Next       string  `json:"next"`
}

// Input is what Run reads.
type Input struct {
	Config      *config.Config
	Owner, Repo string
	Tag         string
	Vars        tmpl.Vars
	// Manifest is the local Artifact Manifest, nil when absent or of
	// another version (ManifestNote says which).
	Manifest     *manifest.Manifest
	ManifestNote string
	GitHub       GitHub
}

// Run reads the state of in.Tag. An error means GitHub could not be read.
func Run(ctx context.Context, in Input) (*Report, error) {
	r := &Report{Repository: in.Owner + "/" + in.Repo, Tag: in.Tag, Release: "none", Manifest: in.ManifestNote}
	if r.Manifest == "" {
		r.Manifest = "used"
	}
	ref, err := in.GitHub.GetRef(ctx, in.Owner, in.Repo, "tags/"+in.Tag)
	switch {
	case err == nil:
		r.TagCommit = ref.Object.SHA
	case !errors.Is(err, github.ErrNotFound):
		return nil, fmt.Errorf("reading tag %s: %w", in.Tag, err)
	}
	rels, err := in.GitHub.ListReleases(ctx, in.Owner, in.Repo)
	if err != nil {
		return nil, fmt.Errorf("reading releases: %w", err)
	}
	var rel *github.Release
	for i := range rels {
		if rels[i].TagName == in.Tag && (rel == nil || rel.Draft && !rels[i].Draft) {
			rel = &rels[i]
		}
	}
	if rel != nil {
		r.Release, r.Immutable, r.Prerelease, r.ReleaseURL = "published", rel.Immutable, rel.Prerelease, rel.HTMLURL
		if rel.Draft {
			r.Release = "draft"
		}
		r.Assets = compareAssets(rel.Assets, in.Manifest)
	}
	if r.Taps, err = taps(ctx, in); err != nil {
		return nil, err
	}
	r.State, r.Next = decide(r)
	return r, nil
}

// compareAssets compares release assets with the manifest's archives and
// checksum files (by digest, as GitHub reports it).
func compareAssets(assets []github.Asset, m *manifest.Manifest) []Asset {
	byName := map[string]github.Asset{}
	for _, a := range assets {
		byName[a.Name] = a
	}
	var out []Asset
	want := map[string]bool{}
	if m != nil {
		for _, a := range m.Artifacts {
			if a.Kind != manifest.KindArchive && a.Kind != manifest.KindChecksum {
				continue
			}
			want[a.Name] = true
			got, ok := byName[a.Name]
			switch {
			case !ok:
				out = append(out, Asset{Name: a.Name, Status: "missing"})
			case got.State != github.AssetStateUploaded:
				out = append(out, Asset{Name: a.Name, Status: "incomplete", Detail: "state " + got.State})
			case got.Digest == nil || *got.Digest == "":
				out = append(out, Asset{Name: a.Name, Status: "unknown", Detail: "GitHub reports no digest"})
			case *got.Digest != "sha256:"+a.SHA256:
				out = append(out, Asset{Name: a.Name, Status: "different", Detail: fmt.Sprintf("release %s, local sha256:%s", *got.Digest, a.SHA256)})
			default:
				out = append(out, Asset{Name: a.Name, Status: "identical"})
			}
		}
	}
	for _, a := range assets {
		if want[a.Name] {
			continue
		}
		st := "extra"
		if m == nil {
			st = "unknown"
		}
		out = append(out, Asset{Name: a.Name, Status: st, Detail: "not in the local manifest"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func taps(ctx context.Context, in Input) ([]Tap, error) {
	var out []Tap
	for i := range in.Config.Brews {
		b := &in.Config.Brews[i]
		pub, err := homebrew.ResolvePublish(b, in.Vars)
		if err != nil {
			return nil, fmt.Errorf("brews[%d] (%s): %w", i, b.Name, err)
		}
		t := Tap{Formula: b.Name, Repository: pub.Owner + "/" + pub.Name}
		switch {
		case pub.Skip:
			t.Mode, t.Status = "skipped", "skipped"
		case pub.Owner == "" || pub.Name == "":
			t.Mode, t.Status = "skipped", "skipped"
		case !pub.PullRequest:
			t.Mode, t.Status = "direct", "unknown"
		default:
			t.Mode = "pull-request"
			bo, bn := pub.Owner, pub.Name
			if pub.BaseOwner != "" && pub.BaseName != "" {
				bo, bn = pub.BaseOwner, pub.BaseName
			}
			prs, err := in.GitHub.ListPulls(ctx, bo, bn, github.ListPullsOptions{Head: pub.Owner + ":" + pub.Branch, State: "all"})
			if err != nil {
				return nil, fmt.Errorf("reading pull requests of %s/%s: %w", bo, bn, err)
			}
			t.Status = "none"
			if len(prs) > 0 {
				pr := prs[0]
				t.Status, t.PullRequest = pr.State, pr.HTMLURL
				if pr.MergedAt != nil {
					t.Status = "merged"
				}
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// decide classifies the release and names the next safe action.
func decide(r *Report) (string, string) {
	count := map[string]int{}
	for _, a := range r.Assets {
		count[a.Status]++
	}
	switch {
	case r.Release == "none" && r.TagCommit == "":
		return NotStarted, "nothing is published: push the tag (after the preflight) to start the release"
	case r.Release == "none":
		return Tagged, "the tag exists but no release: check the release workflow run; re-run it (failed jobs) to continue — never move or delete the tag"
	case count["different"] > 0:
		if r.Release == "draft" {
			return Conflict, "a draft asset differs from the local build: do not overwrite it; find out why the build differs (it should be reproducible), then delete the draft deliberately or release a new version"
		}
		return Conflict, "a published asset differs from the local build: never replace it; release a new version"
	case r.Release == "draft":
		return Draft, "a draft release exists: re-run the release (it resumes: identical assets are skipped, missing ones uploaded, then it is published)"
	case count["missing"]+count["incomplete"] > 0:
		if r.Immutable {
			return Conflict, "the release is published and immutable but incomplete: assets can no longer be added; release a new version"
		}
		return PublishedIncomplete, "the release is published but incomplete: re-run the release stage to upload the missing assets"
	}
	for _, t := range r.Taps {
		switch t.Status {
		case "none":
			return Published, "the release is complete; the tap pull request was not opened: re-run only the formula and tap stages (hephaestus run --stages formula,tap)"
		case "open":
			return Published, "the release is complete; review and merge the tap pull request: " + t.PullRequest
		case "closed":
			return Published, "the release is complete; the tap pull request was closed without merging: " + t.PullRequest
		case "unknown":
			return Published, "the release is complete; check the tap (direct commits are not tracked by status)"
		}
	}
	return Completed, "nothing to do"
}

// WriteText writes the report for people.
func (r *Report) WriteText(w io.Writer) {
	fmt.Fprintf(w, "Release status: %s %s\n\n", r.Repository, r.Tag)
	tag := "absent"
	if r.TagCommit != "" {
		tag = r.TagCommit
	}
	fmt.Fprintf(w, "  tag        %s\n", tag)
	rel := r.Release
	if r.Immutable {
		rel += ", immutable"
	}
	if r.Release != "none" && r.Prerelease {
		rel += ", prerelease"
	}
	if r.ReleaseURL != "" {
		rel += " " + r.ReleaseURL
	}
	fmt.Fprintf(w, "  release    %s\n", rel)
	fmt.Fprintf(w, "  manifest   %s\n", r.Manifest)
	for _, a := range r.Assets {
		d := ""
		if a.Detail != "" {
			d = " (" + a.Detail + ")"
		}
		fmt.Fprintf(w, "  asset      %-10s %s%s\n", a.Status, a.Name, d)
	}
	for _, t := range r.Taps {
		pr := ""
		if t.PullRequest != "" {
			pr = " " + t.PullRequest
		}
		fmt.Fprintf(w, "  tap        %-10s %s → %s (%s)%s\n", t.Status, t.Formula, t.Repository, t.Mode, pr)
	}
	fmt.Fprintf(w, "\nState: %s\nNext:  %s\n", strings.ToUpper(r.State), r.Next)
}
