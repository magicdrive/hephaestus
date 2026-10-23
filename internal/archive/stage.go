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
	in.Manifest.RemoveKind(manifest.KindArchive, ids...)
	for _, a := range res.Archives {
		in.Manifest.Put(a)
	}
	return res, in.Manifest.Validate()
}
