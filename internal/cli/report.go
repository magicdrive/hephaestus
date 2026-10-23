package cli

import (
	"fmt"
	"strings"

	"github.com/magicdrive/hephaestus/internal/archive"
	"github.com/magicdrive/hephaestus/internal/build"
	"github.com/magicdrive/hephaestus/internal/ghaction"
	"github.com/magicdrive/hephaestus/internal/homebrew"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/pipeline"
	"github.com/magicdrive/hephaestus/internal/release"
	"github.com/magicdrive/hephaestus/internal/tap"
)

// report gathers results for the step summary and action outputs.
type report struct {
	Stages   []pipeline.Stage
	Ran      []pipeline.Stage
	Failed   pipeline.Stage
	DryRun   bool
	Version  string
	Manifest *manifest.Manifest
	Build    *build.Result
	Archive  *archive.Result
	Release  *release.Result
	Formulas []homebrew.Formula
	Tap      []tap.Result
	Warnings []string
	Err      error
}

func (r *report) status() string {
	switch {
	case r.Err != nil:
		return "failed"
	case r.DryRun:
		return "dry-run"
	case r.noop():
		return "no-op"
	default:
		return "published"
	}
}

// noop reports whether nothing was changed on GitHub.
func (r *report) noop() bool {
	if r.Release != nil && !r.Release.NoOp {
		return false
	}
	for _, t := range r.Tap {
		if !t.NoOp {
			return false
		}
	}
	return true
}

func (r *report) outputs() map[string]string {
	out := map[string]string{
		"status":  r.status(),
		"no-op":   fmt.Sprint(r.Err == nil && r.noop()),
		"version": r.Version,
	}
	if r.Manifest != nil {
		var arts []map[string]string
		for _, a := range r.Manifest.Artifacts {
			if a.Kind == manifest.KindArchive || a.Kind == manifest.KindChecksum {
				arts = append(arts, map[string]string{"name": a.Name, "path": a.Path, "sha256": a.SHA256, "url": a.DownloadURL})
			}
		}
		out["artifacts"] = jsonString(arts)
	}
	if rel := r.Release; rel != nil {
		out["release-url"] = rel.ReleaseURL
		out["release-id"] = fmt.Sprint(rel.ReleaseID)
		var names []string
		for _, a := range rel.Assets {
			if a.Status == "uploaded" || a.Status == "replaced" {
				names = append(names, a.Name)
			}
		}
		out["uploaded-assets"] = jsonString(names)
	}
	var paths, prs, shas []string
	for _, f := range r.Formulas {
		paths = append(paths, f.Path)
	}
	for _, t := range r.Tap {
		if t.PullRequestURL != "" {
			prs = append(prs, t.PullRequestURL)
		}
		if t.CommitSHA != "" {
			shas = append(shas, t.CommitSHA)
		}
	}
	out["formula-paths"] = strings.Join(paths, "\n")
	out["pull-request-url"] = strings.Join(prs, "\n")
	out["formula-commit-sha"] = strings.Join(shas, "\n")
	return out
}

func (r *report) summary() string {
	var b strings.Builder
	icon := map[string]string{"failed": "❌", "dry-run": "🧪", "no-op": "✅", "published": "✅"}[r.status()]
	fmt.Fprintf(&b, "## %s Hephaestus %s — %s\n\n", icon, r.Version, r.status())
	var ran []string
	for _, s := range r.Ran {
		mark := "✓"
		if s == r.Failed {
			mark = "✗"
		}
		ran = append(ran, fmt.Sprintf("%s %s", mark, s))
	}
	fmt.Fprintf(&b, "**Stages:** %s\n\n", strings.Join(ran, " · "))
	if r.Err != nil {
		fmt.Fprintf(&b, "### Error\n\n```\n%s\n```\n\n", strings.ReplaceAll(r.Err.Error(), "```", "'''"))
	}
	if r.Manifest != nil {
		var rows [][]string
		for _, a := range r.Manifest.Artifacts {
			if a.Kind == manifest.KindArchive || a.Kind == manifest.KindChecksum {
				target := ""
				if a.OS != "" {
					target = a.OS + "/" + a.Arch
				}
				rows = append(rows, []string{a.Name, target, fmt.Sprint(a.Size), "`" + a.SHA256 + "`"})
			}
		}
		if len(rows) > 0 {
			b.WriteString("### Artifacts\n\n")
			b.WriteString(ghaction.Table([]string{"Name", "Target", "Bytes", "SHA-256"}, rows))
			b.WriteString("\n")
		}
	}
	if rel := r.Release; rel != nil {
		fmt.Fprintf(&b, "### GitHub Release\n\n%s", rel.ReleaseURL)
		if rel.NoOp {
			b.WriteString(" (no changes)")
		}
		b.WriteString("\n\n")
		var rows [][]string
		for _, a := range rel.Assets {
			e := ""
			if a.Err != nil {
				e = a.Err.Error()
			}
			rows = append(rows, []string{a.Name, a.Status, e})
		}
		b.WriteString(ghaction.Table([]string{"Asset", "Status", "Error"}, rows))
		b.WriteString("\n")
	}
	if len(r.Formulas) > 0 {
		b.WriteString("### Homebrew formulas\n\n")
		var rows [][]string
		for _, f := range r.Formulas {
			rows = append(rows, []string{f.Name, f.Path})
		}
		b.WriteString(ghaction.Table([]string{"Formula", "Path"}, rows))
		b.WriteString("\n")
	}
	if len(r.Tap) > 0 {
		b.WriteString("### Tap publication\n\n")
		var rows [][]string
		for _, t := range r.Tap {
			rows = append(rows, []string{t.Formula, t.Mode, t.Status, t.PullRequestURL + t.CommitSHA})
		}
		b.WriteString(ghaction.Table([]string{"Formula", "Mode", "Status", "PR / Commit"}, rows))
		b.WriteString("\n")
	}
	if len(r.Warnings) > 0 {
		b.WriteString("### Warnings\n\n")
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "- %s\n", strings.ReplaceAll(w, "\n", " "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (r *report) publish() error {
	if err := ghaction.WriteOutputs(r.outputs()); err != nil {
		return err
	}
	return ghaction.AppendSummary(r.summary())
}
