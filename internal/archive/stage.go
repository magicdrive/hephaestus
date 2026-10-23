package archive

import (
	"fmt"

	"github.com/magicdrive/hephaestus/internal/manifest"
)

// Result summarizes one archives stage run.
type Result struct {
	Archives []manifest.Artifact
	// Skipped maps archive name to optional sources that matched nothing.
	Skipped map[string][]string
}

// Run plans every archive, and only if all plans succeed, writes them and
// records them in in.Manifest (replacing archives with the same ids from a
// previous run). Planning first means a configuration error never leaves a
// partial set of new archives behind.
func Run(in Input) (*Result, error) {
	plans, err := BuildPlans(in)
	if err != nil {
		return nil, err
	}
	res := &Result{Skipped: map[string][]string{}}
	for _, p := range plans {
		a, err := Write(in.Config.Root, p)
		if err != nil {
			return res, fmt.Errorf("archive %s: %w", p.Name, err)
		}
		res.Archives = append(res.Archives, a)
		if len(p.Skipped) > 0 {
			res.Skipped[p.Name] = p.Skipped
		}
	}
	ids := in.IDs
	if len(ids) == 0 {
		for _, a := range in.Config.Archives {
			ids = append(ids, a.ID)
		}
	}
	// Replace only what this stage produced; archives imported by a build
	// provider (existing / goreleaser) may share an id and must survive.
	in.Manifest.RemoveFunc(func(a manifest.Artifact) bool {
		return a.Kind == manifest.KindArchive && a.Source.Stage == "archive" && contains(ids, a.ID)
	})
	for _, a := range res.Archives {
		in.Manifest.Put(a)
	}
	return res, in.Manifest.Validate()
}
