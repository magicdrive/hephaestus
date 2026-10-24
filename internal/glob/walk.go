package glob

import (
	"errors"
	"io/fs"
	"sort"
)

// Match is one file system entry selected by Walk.
type Match struct {
	// Path is slash-separated and relative to the walked file system.
	Path string
	// Type is the entry's type bits (0 for regular files, fs.ModeSymlink for
	// symbolic links, and so on). Walk does not follow symbolic links.
	Type fs.FileMode
}

// ErrTooManyMatches is returned when a walk exceeds its limit.
var ErrTooManyMatches = errors.New("glob matched too many files")

// Walk returns every non-directory entry of fsys that matches p, sorted by
// path. Directories are descended only when a match below them is possible;
// symbolic links to directories are reported as matches (when they match)
// but never descended, so cycles and escapes through linked directories are
// impossible. At most limit matches are returned; exceeding it is an error.
func Walk(fsys fs.FS, p *Pattern, limit int) ([]Match, error) {
	start := p.Base()
	if start == "" {
		start = "."
	}
	if _, err := fs.Stat(fsys, start); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var out []Match
	err := fs.WalkDir(fsys, start, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := name
		if rel == "." {
			rel = ""
		}
		if d.IsDir() {
			if rel != "" && !p.CouldMatchUnder(rel) {
				return fs.SkipDir
			}
			return nil
		}
		if !p.Match(rel) {
			return nil
		}
		if len(out) >= limit {
			return ErrTooManyMatches
		}
		out = append(out, Match{Path: rel, Type: d.Type()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
