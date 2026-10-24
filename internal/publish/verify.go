package publish

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// VerifyInput is what a release is verified against.
type VerifyInput struct {
	Plan   *Plan
	Commit string // the commit the tag must point to
	// Run is the release workflow run, for its tap steps (optional).
	Run *Run
	// TapHeads are the taps' default branch heads before the release
	// (optional; recorded by publish's preflight).
	TapHeads map[string]string
}

// VerifyRelease checks the published release end to end: release state,
// tag, "latest", assets, checksums, provenance attestations and the
// Homebrew tap. It only reads (assets are downloaded to a temporary
// directory that is removed afterwards).
func VerifyRelease(ctx context.Context, d *Deps, p *Project, in VerifyInput) *Report {
	r := &Report{Commit: in.Commit}
	pl := in.Plan
	tag := pl.Version.Tag

	rel, ok := verifyReleaseState(ctx, d, p, pl, r)
	if c, exists, err := d.Git.RemoteTag(ctx, tag); err != nil {
		r.addErr("tag commit", err)
	} else {
		r.add("tag commit", exists && c == in.Commit, "%s -> %s (want %s)", tag, short(c), short(in.Commit))
	}
	var sums map[string]string
	if ok {
		sums = verifyAssets(ctx, d, p, pl, in.Commit, rel, r)
	}
	if in.Run != nil {
		verifyTapSteps(ctx, d, p, pl, in.Run, r)
	}
	for _, t := range pl.Taps {
		if t.Publish.Skip {
			verifyTapUntouched(ctx, d, pl, t, in.TapHeads, r)
		} else {
			verifyTapPullRequest(ctx, d, pl, t, rel, sums, r)
		}
	}
	return r
}

func verifyReleaseState(ctx context.Context, d *Deps, p *Project, pl *Plan, r *Report) (Release, bool) {
	tag := pl.Version.Tag
	rels, err := d.GH.Releases(ctx, p.Repo)
	if err != nil {
		r.addErr("release", err)
		return Release{}, false
	}
	i := slices.IndexFunc(rels, func(x Release) bool { return x.TagName == tag })
	if i < 0 {
		r.add("release", false, "no release for %s", tag)
		return Release{}, false
	}
	rel := rels[i]
	if rel.Draft {
		r.add("release", false, "the release for %s is still a draft: %s", tag, rel.HTMLURL)
		return rel, false
	}
	r.add("release", true, "published: %s", rel.HTMLURL)
	r.add("prerelease flag", rel.Prerelease == pl.Prerelease, "prerelease %t (want %t)", rel.Prerelease, pl.Prerelease)
	r.add("release notes", strings.TrimSpace(rel.Body) != "", "%d characters", len(rel.Body))

	wantLatest := !pl.Prerelease && p.Cfg.Release.GitHub.MakeLatest != "false"
	latest, err := d.GH.LatestRelease(ctx, p.Repo)
	switch {
	case err != nil && !errors.Is(err, ErrNotFound):
		r.addErr("latest", err)
	case wantLatest:
		r.add("latest", err == nil && latest.TagName == tag, "latest release is %q (want %s)", latest.TagName, tag)
	default:
		r.add("latest", err != nil || latest.TagName != tag, "latest release is %q (must not be %s)", latest.TagName, tag)
	}
	return rel, true
}

// verifyAssets returns the SHA-256 of every downloaded asset (nil when they
// could not all be downloaded).
func verifyAssets(ctx context.Context, d *Deps, p *Project, pl *Plan, commit string, rel Release, r *Report) map[string]string {
	want := pl.Assets()
	byName := map[string]Asset{}
	var got []string
	for _, a := range rel.Assets {
		byName[a.Name] = a
		got = append(got, a.Name)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		r.add("assets", false, "release has %v, want %v", got, want)
		return nil
	}
	for _, name := range want {
		if byName[name].State != "uploaded" {
			r.add("assets", false, "%s is %s", name, byName[name].State)
			return nil
		}
	}
	r.add("assets", true, "%d assets: %s", len(want), strings.Join(want, ", "))

	dir, err := os.MkdirTemp("", "hephaestus-publish-")
	if err != nil {
		r.addErr("download", err)
		return nil
	}
	defer os.RemoveAll(dir)
	if err := d.GH.DownloadRelease(ctx, p.Repo, pl.Version.Tag, dir); err != nil {
		r.addErr("download", err)
		return nil
	}
	sums := map[string]string{}
	for _, name := range want {
		sum, err := sha256File(filepath.Join(dir, name))
		if err != nil {
			r.addErr("download", err)
			return nil
		}
		if dg := byName[name].Digest; dg != "" && dg != "sha256:"+sum {
			r.add("asset digest", false, "%s: downloaded sha256:%s, GitHub reports %s", name, sum, dg)
			return nil
		}
		sums[name] = sum
	}
	r.add("download", true, "%d assets downloaded and hashed", len(want))

	if pl.Checksum != "" {
		listed, err := readChecksums(filepath.Join(dir, pl.Checksum))
		if err != nil {
			r.addErr("checksums", err)
		} else {
			ok, detail := checksumsMatch(listed, sums, pl.Archives)
			r.add("checksums", ok, "%s", detail)
		}
	}

	policy := AttestPolicy{
		Repo: p.Repo, SignerWorkflow: p.Repo.String() + "/.github/workflows/" + p.ReleaseFile,
		SourceRef: "refs/tags/" + pl.Version.Tag, SourceDigest: commit,
	}
	var failed []string
	for _, name := range want {
		if err := d.GH.VerifyAttestation(ctx, filepath.Join(dir, name), policy); err != nil {
			failed = append(failed, err.Error())
		}
	}
	if len(failed) > 0 {
		r.add("attestations", false, "%d of %d failed: %s", len(failed), len(want), strings.Join(failed, "; "))
	} else {
		r.add("attestations", true, "%d verified (signer %s, %s, commit %s, GitHub-hosted runner)", len(want), policy.SignerWorkflow, policy.SourceRef, short(commit))
	}
	return sums
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readChecksums parses "<sha256>  <name>" lines.
func readChecksums(p string) (map[string]string, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 || len(fields[0]) != 64 {
			return nil, fmt.Errorf("malformed checksum line %q", sc.Text())
		}
		out[strings.TrimPrefix(fields[1], "*")] = fields[0]
	}
	return out, sc.Err()
}

// checksumsMatch requires the checksum file to list exactly the archives,
// with the sums of the downloaded files.
func checksumsMatch(listed, sums map[string]string, archives []string) (bool, string) {
	if len(listed) != len(archives) {
		return false, fmt.Sprintf("checksum file lists %d files, want the %d archives", len(listed), len(archives))
	}
	for _, name := range archives {
		if listed[name] == "" {
			return false, fmt.Sprintf("checksum file does not list %s", name)
		}
		if listed[name] != sums[name] {
			return false, fmt.Sprintf("%s: checksum file says %s, downloaded file is %s", name, listed[name], sums[name])
		}
	}
	return true, fmt.Sprintf("%d archives match the checksum file", len(archives))
}

// verifyTapSteps checks that the release job ran the tap steps exactly
// when the tap is published.
func verifyTapSteps(ctx context.Context, d *Deps, p *Project, pl *Plan, run *Run, r *Report) {
	jobs, err := d.GH.Jobs(ctx, p.Repo, run.ID)
	if err != nil {
		r.addErr("tap steps", err)
		return
	}
	want := "success"
	if pl.TapSkipped() {
		want = "skipped"
	}
	found := 0
	for _, j := range jobs {
		for _, s := range j.Steps {
			if s.Name != TapPublishStep && s.Name != TapPreflightStep {
				continue
			}
			found++
			if s.Conclusion != want {
				r.add("tap steps", false, "step %q concluded %s, want %s", s.Name, s.Conclusion, want)
				return
			}
		}
	}
	if found != 2 {
		r.add("tap steps", false, "found %d of the 2 tap steps in run %d", found, run.ID)
		return
	}
	if want == "skipped" {
		r.add("tap steps", true, "tap rehearsal and publish steps skipped: the tap token was not used")
	} else {
		r.add("tap steps", true, "tap rehearsal and publish steps succeeded")
	}
}

// verifyTapUntouched proves that a skipped tap did not change: same
// default branch head as before the release (when recorded), no pull
// request, and no formula for this version.
func verifyTapUntouched(ctx context.Context, d *Deps, pl *Plan, t TapPlan, heads map[string]string, r *Report) {
	pub := t.Publish
	if pub.Owner == "" || pub.Name == "" {
		r.add("tap untouched", true, "%s: no tap configured", t.Formula)
		return
	}
	tap := Repo{Owner: pub.Owner, Name: pub.Name}
	info, err := d.GH.Repo(ctx, tap)
	if err != nil {
		r.addErr("tap untouched", err)
		return
	}
	if before, ok := heads[tap.String()]; ok {
		head, err := d.GH.BranchHead(ctx, tap, info.DefaultBranch)
		if err != nil {
			r.addErr("tap untouched", err)
			return
		}
		if head != before {
			r.add("tap untouched", false, "%s@%s moved from %s to %s", tap, info.DefaultBranch, short(before), short(head))
			return
		}
	}
	if pub.Branch != "" {
		prs, err := d.GH.PullRequests(ctx, tap, pub.Owner, pub.Branch)
		if err != nil {
			r.addErr("tap untouched", err)
			return
		}
		if len(prs) > 0 {
			r.add("tap untouched", false, "%s has a pull request for %s: %s", tap, pl.Version.Tag, prs[0].HTMLURL)
			return
		}
	}
	if b, err := d.GH.File(ctx, tap, pub.Path, info.DefaultBranch); err == nil && strings.Contains(string(b), "/"+pl.Version.Tag+"/") {
		r.add("tap untouched", false, "%s on %s already refers to %s", pub.Path, tap, pl.Version.Tag)
		return
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		r.addErr("tap untouched", err)
		return
	}
	r.add("tap untouched", true, "%s: skipped (%s); %s unchanged, no pull request", t.Formula, pub.SkipReason, tap)
}

var (
	formulaURLRe    = regexp.MustCompile(`^\s*url "([^"]+)"`)
	formulaSHA256Re = regexp.MustCompile(`^\s*sha256 "([0-9a-f]{64})"`)
)

// verifyTapPullRequest checks the formula pull request of a published tap:
// it exists, changes only the formula, and every download URL and sha256
// matches a release archive. It is never merged by publish.
func verifyTapPullRequest(ctx context.Context, d *Deps, pl *Plan, t TapPlan, rel Release, sums map[string]string, r *Report) {
	pub := t.Publish
	tap := Repo{Owner: pub.Owner, Name: pub.Name}
	if !pub.PullRequest {
		r.add("tap pull request", false, "%s: direct commits are not verified by publish", t.Formula)
		return
	}
	base := tap
	if pub.BaseOwner != "" && pub.BaseName != "" {
		base = Repo{Owner: pub.BaseOwner, Name: pub.BaseName}
	}
	prs, err := d.GH.PullRequests(ctx, base, pub.Owner, pub.Branch)
	if err != nil {
		r.addErr("tap pull request", err)
		return
	}
	if len(prs) == 0 {
		r.add("tap pull request", false, "no pull request from %s:%s on %s", pub.Owner, pub.Branch, base)
		return
	}
	pr := prs[0]
	files, err := d.GH.PullRequestFiles(ctx, base, pr.Number)
	if err != nil {
		r.addErr("tap pull request", err)
		return
	}
	if !slices.Equal(files, []string{pub.Path}) {
		r.add("tap pull request", false, "%s changes %v, want only %s", pr.HTMLURL, files, pub.Path)
		return
	}
	content, err := d.GH.File(ctx, tap, pub.Path, pr.Head.SHA)
	if err != nil {
		r.addErr("tap formula", err)
		return
	}
	// The downloaded files' hashes, else GitHub's digests.
	digests := map[string]string{}
	for _, a := range rel.Assets {
		digests[a.Name] = strings.TrimPrefix(a.Digest, "sha256:")
	}
	for name, sum := range sums {
		digests[name] = sum
	}
	ok, detail := formulaMatchesRelease(string(content), t.URLPrefix, pl.Archives, digests)
	r.add("tap formula", ok, "%s", detail)
	state := pr.State
	if pr.Merged {
		state = "merged"
	}
	r.add("tap pull request", true, "%s (%s; review and merge it on the tap, publish never merges)", pr.HTMLURL, state)
}

// formulaMatchesRelease requires every url in the formula to be a release
// archive under prefix, followed by that archive's sha256.
func formulaMatchesRelease(formula, prefix string, archives []string, digests map[string]string) (bool, string) {
	lines := strings.Split(formula, "\n")
	n := 0
	for i, l := range lines {
		m := formulaURLRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		u := m[1]
		if prefix == "" || !strings.HasPrefix(u, prefix) {
			return false, fmt.Sprintf("url %s is not under %s", u, prefix)
		}
		name := strings.TrimPrefix(u, prefix)
		if !slices.Contains(archives, name) {
			return false, fmt.Sprintf("url %s is not a release archive", u)
		}
		var sum string
		for _, next := range lines[i+1 : min(i+4, len(lines))] {
			if s := formulaSHA256Re.FindStringSubmatch(next); s != nil {
				sum = s[1]
				break
			}
		}
		if sum == "" || digests[name] == "" || sum != digests[name] {
			return false, fmt.Sprintf("%s: formula sha256 %q, release asset sha256 %q", name, sum, digests[name])
		}
		n++
	}
	if n == 0 {
		return false, "the formula has no download url"
	}
	return true, fmt.Sprintf("%d download urls and sha256 match the release", n)
}
