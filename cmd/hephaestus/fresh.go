package hephaestus

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/pipeline"
)

// staleStages maps the stage recorded in an artifact's source to the
// pipeline stage that writes that file again.
var staleStages = map[string]pipeline.Stage{
	"archive":  pipeline.Archive,
	"checksum": pipeline.Checksum,
	"homebrew": pipeline.Formula,
}

// removeStaleOutputs runs with --fresh, before the new manifest replaces the
// old one. It deletes the archives, checksum files and formulae that the
// previous run's manifest records, when a stage of this run writes them again,
// so that dist/ does not keep an earlier version's archive next to the new one
// (a script that packs or uploads dist/*.tar.gz would pick it up). Only files
// that Hephaestus wrote itself are deleted: inside dist, unchanged since
// (same SHA-256), and not imported (builds' existing or goreleaser). Anything
// else in dist/ is left alone. It returns the deleted paths.
func removeStaleOutputs(cfg *config.Config, stages []pipeline.Stage) ([]string, error) {
	root, err := os.OpenRoot(cfg.Root)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	old, err := manifest.ReadFileIn(root, cfg.Manifest)
	if err != nil {
		// No previous manifest, or one this version cannot read: nothing is
		// known to be Hephaestus's own output.
		return nil, nil
	}
	dist := strings.TrimSuffix(path.Clean(cfg.Dist), "/") + "/"
	var removed []string
	for _, a := range old.Artifacts {
		stage, ok := staleStages[a.Source.Stage]
		if !ok || !slices.Contains(stages, stage) || a.SHA256 == "" ||
			!strings.HasPrefix(path.Clean(a.Path), dist) {
			continue
		}
		sum, err := fileSHA256(root, a.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, err
		}
		if sum != a.SHA256 {
			continue // changed since: not ours to delete
		}
		if err := root.Remove(a.Path); err != nil {
			return removed, fmt.Errorf("removing stale %s: %w", a.Path, err)
		}
		removed = append(removed, a.Path)
	}
	return removed, nil
}

func fileSHA256(root *os.Root, name string) (string, error) {
	f, err := root.Open(name)
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
