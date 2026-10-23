package archive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"

	"github.com/magicdrive/hephaestus/internal/manifest"
	"github.com/magicdrive/hephaestus/internal/pathutil"
)

// Default inspection limits, used for zero fields of InspectLimits. They
// match the configuration defaults (limits.*).
const (
	DefaultInspectMaxEntries   = 10000
	DefaultInspectMaxFileSize  = 2 << 30
	DefaultInspectMaxTotalSize = 4 << 30
)

// InspectLimits bounds the work Inspect does on an untrusted archive.
// Zero fields take the defaults above.
type InspectLimits struct {
	// MaxEntries bounds the number of members, counting every header.
	MaxEntries int
	// MaxFileSize bounds the uncompressed size of one member.
	MaxFileSize int64
	// MaxTotalSize bounds the sum of uncompressed member sizes.
	MaxTotalSize int64
}

func (l InspectLimits) withDefaults() InspectLimits {
	if l.MaxEntries <= 0 {
		l.MaxEntries = DefaultInspectMaxEntries
	}
	if l.MaxFileSize <= 0 {
		l.MaxFileSize = DefaultInspectMaxFileSize
	}
	if l.MaxTotalSize <= 0 {
		l.MaxTotalSize = DefaultInspectMaxTotalSize
	}
	return l
}

// ErrUnsafeArchive is wrapped by every Inspect error caused by the archive's
// content (as opposed to an I/O failure), so callers can tell a malicious or
// malformed archive from a read error.
var ErrUnsafeArchive = errors.New("unsafe or malformed archive")

// Inspect reads a tar.gz or zip archive and describes its members: path,
// type ("file" or "dir"), permission mode, and for files the uncompressed
// size and SHA-256 of the content. The result is sorted by path.
//
// Inspect never extracts anything, but it treats the archive as hostile and
// fails (wrapping ErrUnsafeArchive) on anything an extractor could be
// tricked by or that a release archive has no reason to contain:
//
//   - member names that are absolute, contain ".." or are otherwise not
//     portable relative paths (pathutil.ValidateRelative); a leading "./"
//     is tolerated and removed, and a bare "./" root entry is skipped;
//   - symbolic links, hard links, device files, FIFOs and sparse files;
//   - setuid, setgid or sticky permission bits;
//   - two members with the same path, paths that differ only in letter
//     case, and a file that another member needs as a directory;
//   - more than lim.MaxEntries members, a member larger than
//     lim.MaxFileSize, or more than lim.MaxTotalSize bytes in total
//     (decompression bombs). Sizes are checked against the declared header
//     size before reading and against the actual bytes while reading.
//
// Parent directories without their own entry are allowed and are not
// synthesized: the result lists exactly the members present. PAX global
// headers (written by git archive) carry no member and are ignored.
//
// format is "tar.gz" or "zip".
func Inspect(r io.ReaderAt, size int64, format string, lim InspectLimits) ([]manifest.File, error) {
	if size < 0 {
		return nil, fmt.Errorf("%w: negative archive size", ErrUnsafeArchive)
	}
	lim = lim.withDefaults()
	ins := &inspector{lim: lim, byPath: map[string]int{}, folded: map[string]string{}}
	var err error
	switch format {
	case "tar.gz":
		err = ins.tarGz(io.NewSectionReader(r, 0, size))
	case "zip":
		err = ins.zip(r, size)
	default:
		return nil, fmt.Errorf("unsupported archive format %q (use tar.gz or zip)", format)
	}
	if err != nil {
		return nil, err
	}
	if err := ins.checkParents(); err != nil {
		return nil, err
	}
	sort.Slice(ins.files, func(i, j int) bool { return ins.files[i].Path < ins.files[j].Path })
	return ins.files, nil
}

type inspector struct {
	lim     InspectLimits
	files   []manifest.File
	byPath  map[string]int
	folded  map[string]string
	entries int
	total   int64
}

func unsafeErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrUnsafeArchive, fmt.Sprintf(format, args...))
}

// count enforces the entry limit; it is called for every header.
func (ins *inspector) count() error {
	ins.entries++
	if ins.entries > ins.lim.MaxEntries {
		return unsafeErr("archive has more than %d entries", ins.lim.MaxEntries)
	}
	return nil
}

// normalize validates a raw member name and returns its clean path, whether
// it names a directory, and whether it is the archive root (to be skipped).
func normalize(raw string, isDir bool) (string, bool, bool, error) {
	name := raw
	if strings.HasSuffix(name, "/") {
		isDir = true
		name = strings.TrimSuffix(name, "/")
	}
	if strings.HasPrefix(raw, "/") {
		return "", isDir, false, unsafeErr("entry %q has an absolute path", raw)
	}
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	if name == "." && isDir {
		return "", isDir, true, nil
	}
	if err := pathutil.ValidateRelative(name); err != nil {
		return "", isDir, false, unsafeErr("entry %q: %v", raw, err)
	}
	return name, isDir, false, nil
}

// checkMode rejects special permission bits and returns the permission part.
func checkMode(raw string, m fs.FileMode) (fs.FileMode, error) {
	if m&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
		return 0, unsafeErr("entry %q has setuid, setgid or sticky bits (%s)", raw, m)
	}
	return m.Perm(), nil
}

func (ins *inspector) add(f manifest.File) error {
	if _, dup := ins.byPath[f.Path]; dup {
		return unsafeErr("entry %q appears more than once", f.Path)
	}
	key := pathutil.FoldKey(f.Path)
	if other, dup := ins.folded[key]; dup {
		return unsafeErr("entries %q and %q differ only in letter case and would collide on case-insensitive file systems", other, f.Path)
	}
	ins.byPath[f.Path] = len(ins.files)
	ins.folded[key] = f.Path
	ins.files = append(ins.files, f)
	return nil
}

// reserve enforces the per-file and total size limits for a declared size.
func (ins *inspector) reserve(name string, declared int64) error {
	if declared < 0 {
		return unsafeErr("entry %q has a negative size", name)
	}
	if declared > ins.lim.MaxFileSize {
		return unsafeErr("entry %q is %d bytes, over the %d-byte limit per file", name, declared, ins.lim.MaxFileSize)
	}
	if declared > ins.lim.MaxTotalSize-ins.total {
		return unsafeErr("archive content exceeds %d bytes uncompressed", ins.lim.MaxTotalSize)
	}
	ins.total += declared
	return nil
}

// hash reads exactly declared bytes from r and fails when the content is
// longer or shorter than declared.
func hashExact(name string, r io.Reader, declared int64) (string, error) {
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, declared+1))
	if err != nil {
		return "", fmt.Errorf("entry %q: %w", name, err)
	}
	if n != declared {
		return "", unsafeErr("entry %q holds %d bytes but its header declares %d", name, n, declared)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (ins *inspector) tarGz(r io.Reader) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return unsafeErr("not a gzip stream: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return unsafeErr("reading tar: %v", err)
		}
		if err := ins.count(); err != nil {
			return err
		}
		var isDir bool
		switch hdr.Typeflag {
		case tar.TypeXGlobalHeader:
			continue
		case tar.TypeDir:
			isDir = true
		case tar.TypeReg, '\x00': // '\x00' is the pre-POSIX regular file flag
		case tar.TypeSymlink:
			return unsafeErr("entry %q is a symbolic link to %q; links are not allowed", hdr.Name, hdr.Linkname)
		case tar.TypeLink:
			return unsafeErr("entry %q is a hard link to %q; links are not allowed", hdr.Name, hdr.Linkname)
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			return unsafeErr("entry %q is a device or FIFO; only files and directories are allowed", hdr.Name)
		case tar.TypeGNUSparse:
			return unsafeErr("entry %q is a sparse file; only regular files are allowed", hdr.Name)
		default:
			return unsafeErr("entry %q has unsupported tar type %q", hdr.Name, hdr.Typeflag)
		}
		name, isDir, skip, err := normalize(hdr.Name, isDir)
		if err != nil {
			return err
		}
		if isDir != (hdr.Typeflag == tar.TypeDir) {
			return unsafeErr("entry %q has a directory name but is not a directory", hdr.Name)
		}
		if hdr.Mode < 0 || hdr.Mode > 0o7777 {
			return unsafeErr("entry %q has invalid mode %o", hdr.Name, hdr.Mode)
		}
		perm, err := checkMode(hdr.Name, hdr.FileInfo().Mode())
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		f := manifest.File{Path: name, Type: TypeDir, Mode: fmt.Sprintf("%04o", perm)}
		if !isDir {
			if err := ins.reserve(name, hdr.Size); err != nil {
				return err
			}
			f.Type, f.Size = TypeFile, hdr.Size
			if f.SHA256, err = hashExact(name, tr, hdr.Size); err != nil {
				return err
			}
		}
		if err := ins.add(f); err != nil {
			return err
		}
	}
}

func (ins *inspector) zip(r io.ReaderAt, size int64) error {
	zr, err := zip.NewReader(r, size)
	// ErrInsecurePath still returns a usable reader; every name is
	// validated below with a more precise message.
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return unsafeErr("not a zip archive: %v", err)
	}
	if len(zr.File) > ins.lim.MaxEntries {
		return unsafeErr("archive has more than %d entries", ins.lim.MaxEntries)
	}
	for _, zf := range zr.File {
		if err := ins.count(); err != nil {
			return err
		}
		mode := zf.Mode()
		switch {
		case mode&fs.ModeSymlink != 0:
			return unsafeErr("entry %q is a symbolic link; links are not allowed", zf.Name)
		case mode&(fs.ModeDevice|fs.ModeCharDevice|fs.ModeNamedPipe|fs.ModeSocket|fs.ModeIrregular) != 0:
			return unsafeErr("entry %q is a device, FIFO or socket; only files and directories are allowed", zf.Name)
		}
		name, isDir, skip, err := normalize(zf.Name, mode.IsDir())
		if err != nil {
			return err
		}
		perm, err := checkMode(zf.Name, mode)
		if err != nil {
			return err
		}
		if skip {
			continue
		}
		f := manifest.File{Path: name, Type: TypeDir, Mode: fmt.Sprintf("%04o", perm)}
		if !isDir {
			if zf.UncompressedSize64 > uint64(ins.lim.MaxFileSize) {
				return unsafeErr("entry %q is %d bytes, over the %d-byte limit per file", name, zf.UncompressedSize64, ins.lim.MaxFileSize)
			}
			declared := int64(zf.UncompressedSize64)
			if err := ins.reserve(name, declared); err != nil {
				return err
			}
			rc, err := zf.Open()
			if err != nil {
				return unsafeErr("entry %q: %v", name, err)
			}
			sum, err := hashExact(name, rc, declared)
			if err == nil {
				// Drain to EOF so the zip reader verifies the CRC-32.
				if _, cerr := io.Copy(io.Discard, rc); cerr != nil {
					err = unsafeErr("entry %q: %v", name, cerr)
				}
			}
			rc.Close()
			if err != nil {
				return err
			}
			f.Type, f.Size, f.SHA256 = TypeFile, declared, sum
		} else if zf.UncompressedSize64 != 0 {
			return unsafeErr("directory entry %q has content", zf.Name)
		}
		if err := ins.add(f); err != nil {
			return err
		}
	}
	return nil
}

// checkParents rejects a file that another entry needs as a directory.
func (ins *inspector) checkParents() error {
	for _, f := range ins.files {
		for _, dir := range pathutil.Parents(f.Path) {
			if i, ok := ins.byPath[dir]; ok && ins.files[i].Type == TypeFile {
				return unsafeErr("entry %q is a file but %q needs it to be a directory", dir, f.Path)
			}
		}
	}
	return nil
}
