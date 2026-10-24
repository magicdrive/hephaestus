package archive

import (
	"archive/tar"
	"bytes"
	"io"
	"strings"

	"github.com/magicdrive/hephaestus/internal/pathutil"
)

// Raw tar header layout (POSIX ustar / GNU).
const (
	tarBlock       = 512
	tarNameLen     = 100
	tarPrefixLen   = 155
	tarMaxOctal    = 0o77777777777 // largest size an 11-digit octal field holds
	maxTarHeaderIO = 4 << 20       // bytes of extended headers recorded per entry
)

// headerTap records what tar.Reader reads while it parses headers, so the
// raw ustar fields can be compared with the values archive/tar derived from
// PAX records and GNU long names (which it applies before returning).
// archive/tar reads headers block by block with io.ReadFull and never reads
// past an entry's data, so the bytes recorded during one Next call are the
// previous entry's padding followed by this entry's header blocks.
type headerTap struct {
	r        io.Reader
	on       bool
	buf      []byte
	overflow bool
}

func (t *headerTap) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if t.on && n > 0 {
		if len(t.buf)+n > maxTarHeaderIO {
			t.overflow = true
		} else {
			t.buf = append(t.buf, p[:n]...)
		}
	}
	return n, err
}

func (t *headerTap) start() {
	t.on, t.buf, t.overflow = true, t.buf[:0], false
}

// rawTarHeader is what an extractor that ignores PAX records and GNU long
// names would see.
type rawTarHeader struct {
	prefix, name, linkname string
	size                   int64
	sizeOK                 bool
	pax, longName, longLnk int // number of 'x', 'L' and 'K' blocks
	gnuName, gnuLink       string
}

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// tarNumber parses an octal or base-256 numeric field.
func tarNumber(b []byte) (int64, bool) {
	if len(b) > 0 && b[0]&0x80 != 0 {
		if b[0] != 0x80 || len(b) < 2 {
			return 0, false
		}
		var v int64
		for _, c := range b[1:] {
			if v > (1<<55)-1 {
				return 0, false
			}
			v = v<<8 | int64(c)
		}
		return v, true
	}
	s := strings.Trim(string(b), " \x00")
	if s == "" {
		return 0, true
	}
	var v int64
	for _, c := range s {
		if c < '0' || c > '7' || v > tarMaxOctal {
			return 0, false
		}
		v = v*8 + int64(c-'0')
	}
	return v, true
}

// parseRawTar walks the recorded header blocks of one entry.
func parseRawTar(buf []byte) (rawTarHeader, bool) {
	var h rawTarHeader
	off := len(buf) % tarBlock // skip the previous entry's padding
	for off+tarBlock <= len(buf) {
		blk := buf[off : off+tarBlock]
		if off+tarBlock == len(buf) { // the entry's own header
			h.name = cstr(blk[0:100])
			h.linkname = cstr(blk[157:257])
			if string(blk[257:263]) == "ustar\x00" { // POSIX: prefix field
				h.prefix = cstr(blk[345:500])
			}
			h.size, h.sizeOK = tarNumber(blk[124:136])
			return h, true
		}
		size, ok := tarNumber(blk[124:136])
		if !ok || size < 0 || size > int64(len(buf)) {
			return h, false
		}
		data := buf[off+tarBlock:]
		if int64(len(data)) < size {
			return h, false
		}
		data = data[:size]
		switch blk[156] {
		case tar.TypeXHeader:
			h.pax++
		case tar.TypeGNULongName:
			h.longName++
			h.gnuName = cstr(data)
		case tar.TypeGNULongLink:
			h.longLnk++
			h.gnuLink = cstr(data)
		default:
			return h, false
		}
		off += tarBlock + int((size+tarBlock-1)/tarBlock*tarBlock)
	}
	return h, false
}

// toASCII drops non-ASCII characters, as archive/tar does for the ustar
// fields of a PAX entry.
func toASCII(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x80 && r != 0 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func splitDirBase(p string) (string, string) {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i], p[i+1:]
	}
	return "", p
}

// abbreviates reports whether the raw ustar name (prefix, name) is a form
// that GNU tar, bsdtar or archive/tar write for full when full needs an
// extended header:
//
//   - full itself, or an empty name;
//   - GNU tar and libarchive without a prefix: the first 100 bytes (a
//     directory may keep its trailing slash after 99 bytes);
//   - archive/tar: the first 100 bytes after dropping non-ASCII characters;
//   - libarchive with a prefix: a leading run of whole directories of full
//     in prefix, and in name the base name, or its first 97 or more bytes.
//
// Any other raw name names a different file for extractors that ignore
// extended headers.
func abbreviates(prefix, name, full string) bool {
	raw := name
	if prefix != "" {
		raw = prefix + "/" + name
	}
	if raw == full || raw == "" {
		return true
	}
	if isASCII(full) && len(full) <= tarNameLen {
		return false // every writer stores such a name exactly
	}
	if prefix == "" {
		if name == truncate(full, tarNameLen) || name == truncate(toASCII(full), tarNameLen) {
			return true
		}
		return strings.HasSuffix(full, "/") && len(full) > tarNameLen && name == full[:tarNameLen-1]+"/"
	}
	dir, base := splitDirBase(full)
	n := strings.TrimSuffix(name, "/")
	if strings.HasSuffix(name, "/") != strings.HasSuffix(full, "/") {
		return false
	}
	if prefix != dir && !strings.HasPrefix(dir, prefix+"/") {
		return false
	}
	return n == base || (len(n) >= 97 && strings.HasPrefix(base, n))
}

// checkRawHeader compares what archive/tar returned with the raw header, so
// extractors that ignore PAX records or GNU long names cannot be steered to
// another file. It returns the raw name when it is an accepted
// abbreviation of hdr.Name ("" when they are equal).
func checkRawHeader(t *headerTap, hdr *tar.Header) (string, error) {
	if t.overflow {
		return "", unsafeErr("entry %q has more than %d bytes of extended headers", hdr.Name, maxTarHeaderIO)
	}
	raw, ok := parseRawTar(t.buf)
	if !ok {
		return "", unsafeErr("entry %q has extended headers that could not be reconciled with its ustar header", hdr.Name)
	}
	if raw.pax > 1 || raw.longName > 1 || raw.longLnk > 1 {
		return "", unsafeErr("entry %q has several extended headers of one kind", hdr.Name)
	}
	for k := range hdr.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return "", unsafeErr("entry %q has GNU sparse records; only regular files are allowed", hdr.Name)
		}
	}
	if p, ok := hdr.PAXRecords["path"]; ok && p != hdr.Name {
		return "", unsafeErr("entry %q has a PAX path %q that differs from its GNU long name", hdr.Name, p)
	}
	if p, ok := hdr.PAXRecords["linkpath"]; ok && p != hdr.Linkname {
		return "", unsafeErr("entry %q has a PAX link path that differs from its GNU long link name", hdr.Name)
	}
	if raw.longName == 1 && raw.gnuName != hdr.Name {
		return "", unsafeErr("entry %q has a GNU long name %q that differs from its PAX path", hdr.Name, raw.gnuName)
	}
	if raw.longLnk == 1 && raw.gnuLink != hdr.Linkname {
		return "", unsafeErr("entry %q has a GNU long link name that differs from its PAX link path", hdr.Name)
	}
	if !abbreviates(raw.prefix, raw.name, hdr.Name) {
		return "", unsafeErr("entry %q is named %q in its ustar header; extractors that ignore extended headers would use that name", hdr.Name, pathutil.Join(raw.prefix, raw.name))
	}
	if !abbreviates("", raw.linkname, hdr.Linkname) {
		return "", unsafeErr("entry %q has link name %q in its ustar header but %q in its extended header", hdr.Name, raw.linkname, hdr.Linkname)
	}
	if _, ok := hdr.PAXRecords["size"]; ok || !raw.sizeOK {
		if !raw.sizeOK || (raw.size != hdr.Size && (raw.size != 0 || hdr.Size <= tarMaxOctal)) {
			return "", unsafeErr("entry %q has size %d in its extended header but not in its ustar header; extractors that ignore extended headers would read its data as further members", hdr.Name, hdr.Size)
		}
	}
	rawName := pathutil.Join(raw.prefix, raw.name)
	if rawName == hdr.Name {
		return "", nil
	}
	return rawName, nil
}

// checkAbbreviations rejects an abbreviated ustar name that is the path of
// another member: an extractor ignoring extended headers would overwrite
// that member.
func (ins *inspector) checkAbbreviations() error {
	for _, a := range ins.abbrev {
		u := strings.TrimSuffix(a, "/")
		for strings.HasPrefix(u, "./") {
			u = strings.TrimPrefix(u, "./")
		}
		if u == "" {
			continue
		}
		if other, ok := ins.folded[pathutil.FoldKey(u)]; ok {
			return unsafeErr("abbreviated ustar name %q of a long entry is also the path of entry %q", a, other)
		}
	}
	return nil
}
