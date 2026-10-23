package archive

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/flate"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/magicdrive/hephaestus/internal/manifest"
)

// Compression settings are fixed so output does not depend on caller choices.
// Byte-for-byte reproducibility holds for a given Hephaestus build; a Go
// release that changes compress/flate could change compressed bytes (never
// archive contents), which is why the toolchain is pinned for releases.
const compressionLevel = flate.BestCompression

// Write creates the archive described by p under root and returns its
// manifest entry. The file appears atomically; on error nothing is left.
// Every source is re-read and must still match the size and SHA-256 recorded
// in the plan, so a file modified between planning and writing is an error
// rather than a silently different archive.
func Write(root string, p *Plan) (manifest.Artifact, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return manifest.Artifact{}, err
	}
	defer r.Close()

	out := filepath.Join(root, filepath.FromSlash(p.Path))
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return manifest.Artifact{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(out), ".tmp-"+p.Name+"-*")
	if err != nil {
		return manifest.Artifact{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	h := sha256.New()
	counter := &countWriter{}
	bw := bufio.NewWriter(io.MultiWriter(tmp, h, counter))
	switch p.Format {
	case "tar.gz":
		err = writeTarGz(bw, r, p)
	case "zip":
		err = writeZip(bw, r, p)
	default:
		err = fmt.Errorf("unsupported format %q", p.Format)
	}
	if err == nil {
		err = bw.Flush()
	}
	if err == nil {
		err = tmp.Chmod(0o644)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return manifest.Artifact{}, fmt.Errorf("write %s: %w", p.Name, err)
	}
	if err := os.Rename(tmpName, out); err != nil {
		return manifest.Artifact{}, err
	}
	return p.artifact(hex.EncodeToString(h.Sum(nil)), counter.n), nil
}

func (p *Plan) artifact(sum string, size int64) manifest.Artifact {
	a := manifest.Artifact{
		Kind: manifest.KindArchive, ID: p.ArchiveID, Name: p.Name, Path: p.Path,
		OS: p.Target.OS, Arch: p.Target.Arch, Arm: p.Target.Arm, Format: p.Format,
		SHA256: sum, Size: size, Source: manifest.Source{Stage: "archive"},
	}
	for _, e := range p.Entries {
		f := manifest.File{Path: e.Path, Type: e.Type, Mode: fmt.Sprintf("%04o", e.Mode), Size: e.Size, SHA256: e.SHA256, Source: e.Source}
		a.Files = append(a.Files, f)
		if e.Type == TypeFile && e.Mode&0o111 != 0 {
			a.Executables = append(a.Executables, e.Path)
		}
	}
	return a
}

func writeTarGz(w io.Writer, root *os.Root, p *Plan) error {
	gz, err := gzip.NewWriterLevel(w, compressionLevel)
	if err != nil {
		return err
	}
	// Header fields are left zero: no name, no mtime; Go writes OS=unknown.
	tw := tar.NewWriter(gz)
	for _, e := range p.Entries {
		hdr := &tar.Header{
			Name:    e.Path,
			Mode:    int64(e.Mode),
			ModTime: e.MTime.UTC().Truncate(1e9),
			Uname:   e.Owner,
			Gname:   e.Group,
		}
		// USTAR is preferred when it can represent the entry exactly; the
		// choice depends only on the entry, so it is deterministic.
		hdr.Format = tar.FormatUSTAR
		if len(e.Path) > 100 || !isASCII(e.Path) || len(e.Owner) > 31 || len(e.Group) > 31 {
			hdr.Format = tar.FormatPAX
		}
		if e.Type == TypeDir {
			hdr.Typeflag = tar.TypeDir
			hdr.Name += "/"
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = e.Size
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("%s: %w", e.Path, err)
		}
		if e.Type == TypeFile {
			if err := copyVerified(tw, root, &e); err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func writeZip(w io.Writer, root *os.Root, p *Plan) error {
	zw := zip.NewWriter(w)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, compressionLevel)
	})
	for _, e := range p.Entries {
		hdr := &zip.FileHeader{Name: e.Path, Modified: e.MTime.UTC().Truncate(1e9)}
		if e.Type == TypeDir {
			hdr.Name += "/"
			hdr.Method = zip.Store
			hdr.SetMode(os.ModeDir | os.FileMode(e.Mode))
		} else {
			hdr.Method = zip.Deflate
			hdr.SetMode(os.FileMode(e.Mode))
		}
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return fmt.Errorf("%s: %w", e.Path, err)
		}
		if e.Type == TypeFile {
			if err := copyVerified(fw, root, &e); err != nil {
				return err
			}
		}
	}
	return zw.Close()
}

// ErrSourceChanged reports a source that no longer matches its plan.
var ErrSourceChanged = errors.New("source file changed after planning")

func copyVerified(w io.Writer, root *os.Root, e *Entry) error {
	f, err := root.Open(e.Source)
	if err != nil {
		return fmt.Errorf("open %q: %w", e.Source, err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(f, e.Size))
	if err != nil {
		return fmt.Errorf("read %q: %w", e.Source, err)
	}
	if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return fmt.Errorf("%q: %w", e.Source, ErrSourceChanged)
	}
	// A file that grew is also a change.
	var one [1]byte
	if k, _ := f.Read(one[:]); k > 0 {
		return fmt.Errorf("%q: %w", e.Source, ErrSourceChanged)
	}
	return nil
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
