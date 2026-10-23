package build

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/magicdrive/hephaestus/internal/archive"
	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/pathutil"
)

// runExisting records prebuilt files, one per target. With kind: archive
// each file must be a .tar.gz, .tgz or .zip; its members are inspected
// (and the archive rejected if unsafe) and it is recorded as a
// release-ready archive.
func (s *stage) runExisting(b *config.Build) error {
	ts, err := targets(b)
	if err != nil {
		return err
	}
	kind := b.Existing.Kind
	if kind == "" {
		kind = config.ExistingKindBinary
	}
	if kind != config.ExistingKindBinary && kind != config.ExistingKindArchive {
		return fmt.Errorf("existing.kind %q must be %q or %q", kind, config.ExistingKindBinary, config.ExistingKindArchive)
	}
	paths := map[string]config.Target{}
	for _, t := range ts {
		vars := s.in.Vars.WithTarget(t.OS, t.Arch, t.Arm)
		rel, err := renderPath("existing.path", b.Existing.Path, vars)
		if err != nil {
			return fmt.Errorf("target %s: %w", t, err)
		}
		if prev, dup := paths[pathutil.FoldKey(rel)]; dup {
			return fmt.Errorf("targets %s and %s both resolve existing.path to %q; include {{ .OS }} and {{ .Arch }} in path", prev, t, rel)
		}
		paths[pathutil.FoldKey(rel)] = t
		if kind == config.ExistingKindArchive {
			a, err := s.importArchive(b, config.ProviderExisting, "", rel, "", t, nil)
			if err != nil {
				return fmt.Errorf("target %s: %w", t, err)
			}
			s.res.Imported = append(s.res.Imported, a)
			continue
		}
		size, sum, err := verifyFile(s.root, rel, s.maxFileSize())
		if err != nil {
			if errors.Is(err, errMissing) {
				return fmt.Errorf("target %s: prebuilt file %w (existing.path)", t, err)
			}
			return fmt.Errorf("target %s: %w", t, err)
		}
		s.res.Binaries = append(s.res.Binaries, binaryArtifact(b, config.ProviderExisting, "", rel, t, size, sum))
	}
	return nil
}

// assetNameRe matches names GitHub keeps unchanged as release asset names.
var assetNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)

// archiveFormat derives the archive format from a file name.
func archiveFormat(name string) (string, error) {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return "tar.gz", nil
	case strings.HasSuffix(lower, ".zip"):
		return "zip", nil
	}
	return "", fmt.Errorf("%q is not a .tar.gz, .tgz or .zip archive", name)
}

// importArchive verifies, hashes and inspects an archive and returns its
// manifest entry. format, when non-empty, is the format another tool
// claimed and must agree with the file name. declared lists executables the
// producer says the archive contains; each must be a file member.
func (s *stage) importArchive(b *config.Build, provider, origin, rel, format string, t config.Target, declared []string) (manifest.Artifact, error) {
	name := pathutil.Base(rel)
	if !assetNameRe.MatchString(name) {
		return manifest.Artifact{}, fmt.Errorf("archive name %q may only contain letters, digits, '.', '_' and '-' (GitHub would rename other characters)", name)
	}
	byName, err := archiveFormat(name)
	if err != nil {
		return manifest.Artifact{}, err
	}
	if format != "" && format != byName {
		return manifest.Artifact{}, fmt.Errorf("archive %q is declared as %s but its name says %s", rel, format, byName)
	}
	v, err := openVerified(s.root, rel, s.maxFileSize())
	if err != nil {
		if errors.Is(err, errMissing) {
			return manifest.Artifact{}, fmt.Errorf("archive %w", err)
		}
		return manifest.Artifact{}, err
	}
	defer v.file.Close()
	lim := s.in.Config.Limits
	files, err := archive.Inspect(io.NewSectionReader(v.file, 0, v.size), v.size, byName, archive.InspectLimits{
		MaxEntries: lim.MaxFiles, MaxFileSize: int64(lim.MaxFileSize), MaxTotalSize: int64(lim.MaxTotalSize),
	})
	if err != nil {
		return manifest.Artifact{}, fmt.Errorf("archive %q: %w", rel, err)
	}
	// Inspect read through the same handle; make sure the file was not
	// rewritten under us between hashing and inspection.
	if st, err := v.file.Stat(); err != nil || st.Size() != v.size {
		return manifest.Artifact{}, fmt.Errorf("archive %q changed while it was being read", rel)
	}
	if len(files) == 0 {
		return manifest.Artifact{}, fmt.Errorf("archive %q is empty", rel)
	}
	isFile := map[string]bool{}
	execs := map[string]bool{}
	for _, f := range files {
		if f.Type != archive.TypeFile {
			continue
		}
		isFile[f.Path] = true
		if mode, err := parseOctal(f.Mode); err == nil && mode&0o111 != 0 {
			execs[f.Path] = true
		}
	}
	for _, d := range declared {
		if !isFile[d] {
			return manifest.Artifact{}, fmt.Errorf("archive %q does not contain the declared binary %q", rel, d)
		}
		execs[d] = true // e.g. a Windows zip without Unix permission bits
	}
	a := manifest.Artifact{
		Kind: manifest.KindArchive, ID: b.ID, Name: name, Path: rel,
		OS: t.OS, Arch: t.Arch, Arm: t.Arm, Format: byName,
		SHA256: v.sha256, Size: v.size, Files: files,
		Source: manifest.Source{Stage: StageName, Provider: provider, Origin: origin},
	}
	for e := range execs {
		a.Executables = append(a.Executables, e)
	}
	sort.Strings(a.Executables)
	return a, nil
}

func parseOctal(s string) (uint32, error) {
	var m uint32
	if s == "" {
		return 0, errors.New("empty mode")
	}
	for _, c := range s {
		if c < '0' || c > '7' {
			return 0, fmt.Errorf("invalid mode %q", s)
		}
		m = m*8 + uint32(c-'0')
	}
	return m, nil
}
