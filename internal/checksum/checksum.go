// Package checksum implements the checksum stage: a sha256sum-compatible file
// listing every archive in the manifest.
package checksum

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/pathutil"
	"github.com/magicdrive/hephaestus/internal/tmpl"
)

// Input is what the checksum stage needs.
type Input struct {
	Config   *config.Config
	Manifest *manifest.Manifest
	Vars     tmpl.Vars
}

// Run verifies every covered artifact on disk against the manifest (a
// checksum is never copied without being re-derived from the bytes), writes
// the checksums file and records it in the manifest.
func Run(in Input) (*manifest.Artifact, error) {
	if in.Config.Checksum.Disable {
		return nil, nil
	}
	name, err := tmpl.Render(in.Config.Checksum.Name, in.Vars)
	if err != nil {
		return nil, fmt.Errorf("checksum.name: %w", err)
	}
	if strings.Contains(name, "/") || pathutil.ValidateRelative(name) != nil {
		return nil, fmt.Errorf("checksum.name %q must be a plain file name", name)
	}
	root, err := os.OpenRoot(in.Config.Root)
	if err != nil {
		return nil, fmt.Errorf("open project root: %w", err)
	}
	defer root.Close()
	covered := in.Manifest.Filter(manifest.KindArchive)
	if len(covered) == 0 {
		return nil, fmt.Errorf("no archives in the manifest to checksum; run the archives stage or import archives first")
	}
	var lines []string
	seen := map[string]bool{}
	for _, a := range covered {
		if seen[a.Name] {
			return nil, fmt.Errorf("two artifacts are named %q", a.Name)
		}
		seen[a.Name] = true
		sum, err := FileSHA256In(root, a.Path)
		if err != nil {
			return nil, err
		}
		if a.SHA256 != "" && a.SHA256 != sum {
			return nil, fmt.Errorf("%s: file on disk (sha256 %s) does not match the manifest (%s); it was modified after it was produced", a.Path, sum, a.SHA256)
		}
		lines = append(lines, sum+"  "+a.Name+"\n")
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i][66:] < lines[j][66:] })
	content := []byte(strings.Join(lines, ""))

	rel := pathutil.Join(in.Config.Dist, name)
	if err := manifest.WriteFileAtomicIn(root, rel, content, 0o644); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(content)
	art := manifest.Artifact{
		Kind: manifest.KindChecksum, ID: "checksum", Name: name, Path: rel,
		SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content)),
		Source: manifest.Source{Stage: "checksum"},
	}
	in.Manifest.RemoveKind(manifest.KindChecksum)
	in.Manifest.Put(art)
	return &art, in.Manifest.Validate()
}

// FileSHA256In hashes the file at rel inside root; symbolic links are
// followed only while they stay inside root.
func FileSHA256In(root *os.Root, rel string) (string, error) {
	if err := pathutil.ValidateRelative(rel); err != nil {
		return "", fmt.Errorf("path %q: %w", rel, err)
	}
	f, err := root.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %s: %w", rel, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FileSHA256 hashes a file. It follows symbolic links anywhere; prefer
// FileSHA256In for paths under the project root.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
