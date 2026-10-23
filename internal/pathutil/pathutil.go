// Package pathutil validates and normalizes the slash-separated relative paths
// Hephaestus uses for project sources and archive entries.
//
// All functions are purely lexical: they never touch the file system. Callers
// that open files must still confine access to the project root (see os.Root).
package pathutil

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxPathLength is the longest relative path accepted for an archive entry or
// a project source path. It keeps entries portable to every supported
// extractor (PAX handles longer names, but Windows tooling does not).
const MaxPathLength = 1024

// MaxSegmentLength is the longest single path component accepted.
const MaxSegmentLength = 255

// ErrEmpty is returned for an empty path where one is required.
var ErrEmpty = errors.New("path is empty")

// ValidateRelative checks that p is a clean, portable, slash-separated relative
// path that cannot escape the directory it is resolved against.
//
// Rejected: empty paths, absolute paths (Unix "/x", Windows "C:\x", "\\host"),
// backslashes, "." or ".." segments, empty segments ("a//b"), trailing
// slashes, NUL and other control characters, Unicode format characters
// (category Cf: bidirectional overrides, zero-width characters and the like,
// which let a listing show a name other than the one extracted), invalid
// UTF-8, Windows reserved characters, and Windows reserved device names
// (CON, PRN, AUX, NUL, COM1-COM9, LPT1-LPT9, in any letter case and with any
// extension) that would make the entry unextractable on Windows.
//
// Names that differ only in Unicode normalization (NFC versus NFD) are not
// detected: folding them needs Unicode normalization tables that the
// standard library does not provide.
func ValidateRelative(p string) error {
	if p == "" {
		return ErrEmpty
	}
	if len(p) > MaxPathLength {
		return fmt.Errorf("path is longer than %d bytes", MaxPathLength)
	}
	if !utf8.ValidString(p) {
		return errors.New("path is not valid UTF-8")
	}
	if strings.HasPrefix(p, "/") {
		return errors.New("path must be relative, not absolute")
	}
	if len(p) >= 2 && p[1] == ':' && isASCIILetter(p[0]) {
		return errors.New("path must not start with a drive letter")
	}
	for _, r := range p {
		switch {
		case r == '\\':
			return errors.New(`path must use "/" as separator, not "\"`)
		case unicode.IsControl(r):
			return fmt.Errorf("path contains control character %U", r)
		case unicode.Is(unicode.Cf, r):
			return fmt.Errorf("path contains invisible formatting character %U", r)
		case r == ':' || r == '<' || r == '>' || r == '"' || r == '|' || r == '?' || r == '*':
			return fmt.Errorf("path contains character %q, which is not portable", r)
		}
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
			return errors.New("path contains an empty segment or trailing slash")
		case ".", "..":
			return fmt.Errorf("path contains a %q segment", seg)
		}
		if len(seg) > MaxSegmentLength {
			return fmt.Errorf("path segment is longer than %d bytes", MaxSegmentLength)
		}
		if strings.HasSuffix(seg, " ") || strings.HasSuffix(seg, ".") {
			return fmt.Errorf("path segment %q ends with a space or dot, which is not portable", seg)
		}
		if isWindowsReserved(seg) {
			return fmt.Errorf("path segment %q is a reserved device name on Windows", seg)
		}
	}
	return nil
}

// isWindowsReserved reports whether a path segment names a Windows device.
// Windows ignores everything from the first dot (and trailing spaces before
// it), so "nul.txt" and "Com1 .tar.gz" are reserved too.
func isWindowsReserved(seg string) bool {
	stem := seg
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.ToUpper(strings.TrimRight(stem, " "))
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) {
		return stem[3] >= '1' && stem[3] <= '9'
	}
	return false
}

// Join joins a directory prefix (which may be empty) and a relative path with
// a single slash. Neither argument may contain "." or ".." segments; callers
// validate the result with ValidateRelative.
func Join(dir, name string) string {
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		return name
	}
	if name == "" {
		return dir
	}
	return dir + "/" + name
}

// Base returns the last segment of a slash-separated path.
func Base(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Parents returns every proper ancestor directory of p, shortest first.
// Parents("a/b/c") == ["a", "a/b"].
func Parents(p string) []string {
	var out []string
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			out = append(out, p[:i])
		}
	}
	return out
}

// FoldKey returns a key under which two paths collide on a case-insensitive
// file system (macOS and Windows defaults). Unicode normalization differences
// (NFC versus NFD) are not folded, so two names that differ only in
// normalization are a known gap: the standard library has no normalization
// tables. Hephaestus does not rewrite names, so identical bytes from the
// source tree stay identical.
func FoldKey(p string) string {
	return strings.ToLower(p)
}

func isASCIILetter(b byte) bool {
	return ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z')
}
