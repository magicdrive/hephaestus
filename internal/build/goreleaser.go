package build

import (
	"bytes"
	"crypto/md5"  //nolint:gosec // G501: md5 checksums offered for GoReleaser compatibility, not for security
	"crypto/sha1" //nolint:gosec // G505: sha1 checksums offered for GoReleaser compatibility, not for security
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/pathutil"
)

// MaxGoReleaserArtifactsSize bounds the artifacts.json file.
const MaxGoReleaserArtifactsSize = 64 << 20

// DefaultGoReleaserArtifacts is the default artifacts.json location.
const DefaultGoReleaserArtifacts = "dist/artifacts.json"

// GoReleaser artifact type strings (artifacts.json "type").
const (
	grTypeBinary  = "Binary"
	grTypeArchive = "Archive"
)

// grArtifact is the subset of a GoReleaser artifacts.json element that
// Hephaestus reads. Unknown fields are ignored: the schema belongs to
// GoReleaser and grows over time.
type grArtifact struct {
	Name    string  `json:"name"`
	Path    string  `json:"path"`
	GOOS    string  `json:"goos"`
	GOARCH  string  `json:"goarch"`
	GOARM   string  `json:"goarm"`
	GOAMD64 string  `json:"goamd64"`
	Type    string  `json:"type"`
	Extra   grExtra `json:"extra"`
}

type grExtra struct {
	ID        string   `json:"ID"`
	Binary    string   `json:"Binary"`
	Binaries  []string `json:"Binaries"`
	Format    string   `json:"Format"`
	WrappedIn string   `json:"WrappedIn"`
	Checksum  string   `json:"Checksum"`
}

// runGoReleaser imports GoReleaser output.
//
// Mapping rules (normative; mirrored in docs):
//
//   - Artifacts of type "Binary" become binary artifacts; with
//     goreleaser.archives: true, artifacts of type "Archive" become archive
//     artifacts. Every other type is ignored.
//   - goreleaser.ids keeps only artifacts whose extra.ID is listed.
//   - goos, goarch and goarm map to os, arch and arm. A darwin universal
//     binary keeps GoReleaser's goarch "all".
//   - Hephaestus targets have no microarchitecture level. For amd64 only
//     artifacts whose goamd64 equals goreleaser.goamd64 (default v1) are
//     imported; other levels are skipped. Other level fields (goarm64,
//     go386, gomips, goppc64, goriscv64) are not read: when GoReleaser
//     built several variants of one target they collide and the import
//     fails with a duplicate error, to be narrowed with goreleaser.ids.
//   - "Binary" artifacts with extra.Format "binary" are GoReleaser's
//     uploadable copies (archives format: binary) of a binary already
//     listed; they are skipped.
//   - Paths are relative to the project root and must stay inside it.
//     Sizes and SHA-256 are computed from disk. An extra.Checksum
//     ("algorithm:hex") is verified against the file and a mismatch is an
//     error.
func (s *stage) runGoReleaser(b *config.Build) error {
	g := b.GoReleaser
	src := g.Artifacts
	if src == "" {
		src = DefaultGoReleaserArtifacts
	}
	level := g.GoAMD64
	if level == "" {
		level = "v1"
	}
	list, err := s.readGoReleaserArtifacts(src)
	if err != nil {
		return err
	}
	seen := map[string]int{}
	n := 0
	for i, ga := range list {
		var kind manifest.Kind
		switch {
		case ga.Type == grTypeBinary && ga.Extra.Format != "binary":
			kind = manifest.KindBinary
		case ga.Type == grTypeArchive && g.Archives:
			kind = manifest.KindArchive
		default:
			continue
		}
		if len(g.IDs) > 0 && !contains(g.IDs, ga.Extra.ID) {
			continue
		}
		if ga.GOARCH == "amd64" && ga.GOAMD64 != "" && ga.GOAMD64 != level {
			continue
		}
		where := fmt.Sprintf("%s[%d] (%s %q)", src, i, ga.Type, ga.Name)
		t, rel, err := grTarget(ga)
		if err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		key := string(kind) + "\x00" + pathutil.Base(rel) + "\x00" + t.String()
		if kind == manifest.KindArchive {
			key = string(kind) + "\x00" + pathutil.Base(rel)
		}
		if j, dup := seen[key]; dup {
			return fmt.Errorf("%s: %s %q for %s is also listed at %s[%d]; restrict goreleaser.ids (or goamd64) so each target has one", where, kind, pathutil.Base(rel), t, src, j)
		}
		seen[key] = i

		var a manifest.Artifact
		if kind == manifest.KindBinary {
			size, sum, err := verifyFile(s.root, rel, s.maxFileSize())
			if err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
			a = binaryArtifact(b, config.ProviderGoReleaser, src, rel, t, size, sum)
		} else {
			format, err := grArchiveFormat(ga.Extra.Format)
			if err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
			var declared []string
			for _, bin := range ga.Extra.Binaries {
				p := bin
				if ga.Extra.WrappedIn != "" {
					p = ga.Extra.WrappedIn + "/" + bin
				}
				p = strings.TrimPrefix(path.Clean(strings.ReplaceAll(p, `\`, "/")), "./")
				if err := pathutil.ValidateRelative(p); err != nil {
					return fmt.Errorf("%s: extra.Binaries entry %q: %w", where, bin, err)
				}
				declared = append(declared, p)
			}
			if a, err = s.importArchive(b, config.ProviderGoReleaser, src, rel, format, t, declared); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
		}
		if ga.Extra.Checksum != "" {
			if err := s.verifyChecksum(rel, ga.Extra.Checksum, a.SHA256); err != nil {
				return fmt.Errorf("%s: %w", where, err)
			}
		}
		if kind == manifest.KindBinary {
			s.res.Binaries = append(s.res.Binaries, a)
		} else {
			s.res.Imported = append(s.res.Imported, a)
		}
		n++
	}
	if n == 0 {
		what := "Binary"
		if g.Archives {
			what = "Binary or Archive"
		}
		filter := ""
		if len(g.IDs) > 0 {
			filter = fmt.Sprintf(" with ids %v", g.IDs)
		}
		return fmt.Errorf("%s lists no %s artifacts%s (goamd64 %s); run goreleaser build or release first", src, what, filter, level)
	}
	return nil
}

func (s *stage) readGoReleaserArtifacts(src string) ([]grArtifact, error) {
	if err := pathutil.ValidateRelative(src); err != nil {
		return nil, fmt.Errorf("goreleaser.artifacts %q: %w", src, err)
	}
	f, err := s.root.Open(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s does not exist; run goreleaser (build or release) before importing", src)
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", src, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxGoReleaserArtifactsSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", src, err)
	}
	if len(data) > MaxGoReleaserArtifactsSize {
		return nil, fmt.Errorf("%s exceeds %d bytes", src, MaxGoReleaserArtifactsSize)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var list []grArtifact
	if err := dec.Decode(&list); err != nil {
		return nil, fmt.Errorf("%s is not a GoReleaser artifacts list: %w", src, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: trailing data after JSON array", src)
	}
	if list == nil {
		return nil, fmt.Errorf("%s is not a GoReleaser artifacts list: expected a JSON array", src)
	}
	return list, nil
}

// grTarget validates an artifact's platform and path.
func grTarget(ga grArtifact) (config.Target, string, error) {
	t := config.Target{OS: ga.GOOS, Arch: ga.GOARCH}
	if ga.GOARCH == "arm" {
		t.Arm = ga.GOARM
	}
	if !identRe.MatchString(t.OS) || !identRe.MatchString(t.Arch) || !armRe.MatchString(t.Arm) {
		return t, "", fmt.Errorf("invalid or missing platform (goos %q, goarch %q, goarm %q)", ga.GOOS, ga.GOARCH, ga.GOARM)
	}
	rel := strings.ReplaceAll(ga.Path, `\`, "/")
	if rel == "" {
		return t, "", errors.New("path is empty")
	}
	if err := pathutil.ValidateRelative(rel); err != nil {
		return t, "", fmt.Errorf("path %q must be relative to the project root and stay inside it: %w", ga.Path, err)
	}
	return t, rel, nil
}

func grArchiveFormat(f string) (string, error) {
	switch f {
	case "tar.gz", "tgz":
		return "tar.gz", nil
	case "zip":
		return "zip", nil
	case "":
		return "", nil // derived from the file name
	}
	return "", fmt.Errorf("archive format %q is not supported (only tar.gz, tgz and zip); exclude it with goreleaser.ids", f)
}

// verifyChecksum compares a GoReleaser "algorithm:hex" checksum with the
// file. sha256 reuses the digest already computed; other algorithms
// re-read the file.
func (s *stage) verifyChecksum(rel, claim, sha256Hex string) error {
	algo, want, ok := strings.Cut(claim, ":")
	want = strings.ToLower(strings.TrimSpace(want))
	if !ok || want == "" {
		return fmt.Errorf("extra.Checksum %q is not of the form algorithm:hex", claim)
	}
	var h hash.Hash
	switch strings.ToLower(algo) {
	case "sha256":
		if want != sha256Hex {
			return fmt.Errorf("checksum mismatch for %q: artifacts.json says sha256:%s but the file has sha256:%s (the file changed after GoReleaser ran)", rel, want, sha256Hex)
		}
		return nil
	case "sha512":
		h = sha512.New()
	case "sha384":
		h = sha512.New384()
	case "sha224":
		h = sha256.New224()
	case "sha1":
		h = sha1.New() //nolint:gosec // G401: user-selected checksum algorithm (GoReleaser compatibility)
	case "md5":
		h = md5.New() //nolint:gosec // G401: user-selected checksum algorithm (GoReleaser compatibility)
	case "crc32":
		h = crc32.NewIEEE()
	default:
		return fmt.Errorf("cannot verify extra.Checksum: algorithm %q is not supported", algo)
	}
	v, err := openVerified(s.root, rel, s.maxFileSize())
	if err != nil {
		return err
	}
	defer v.file.Close()
	if _, err := v.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(h, io.LimitReader(v.file, v.size)); err != nil {
		return fmt.Errorf("read %q: %w", rel, err)
	}
	if v.sha256 != sha256Hex {
		return fmt.Errorf("%q changed while it was being imported", rel)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("checksum mismatch for %q: artifacts.json says %s:%s but the file has %s:%s", rel, algo, want, algo, got)
	}
	return nil
}
