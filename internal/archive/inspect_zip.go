package archive

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"sort"
)

// Zip record signatures (APPNOTE.TXT 4.3).
const (
	zipSigLocal     = 0x04034b50
	zipSigCentral   = 0x02014b50
	zipSigEnd       = 0x06054b50
	zipSigEnd64     = 0x06064b50
	zipSigLocator64 = 0x07064b50
	zipSigDataDesc  = 0x08074b50

	zipEndLen       = 22
	zipLocator64Len = 20
	zipEnd64MinLen  = 56
	zipCentralLen   = 46
	zipLocalLen     = 30
	zipMaxComment   = 0xffff
	// zipMaxPadding bounds the zero bytes accepted after the end record:
	// bsdtar writing to a pipe pads its output to 10 KiB blocks.
	zipMaxPadding   = 64 << 10
	zipZip64ExtraID = 0x0001
	// zipUnicodePathID is the Info-ZIP Unicode Path extra field: a UTF-8
	// name that some extractors (bsdtar, 7-Zip) use instead of the header
	// name.
	zipUnicodePathID = 0x7075
	zipUint16Max     = 0xffff
	zipUint32Max     = 0xffffffff

	// Flag bits that change how an entry is read: encryption (0), data
	// descriptor (3) and UTF-8 names (11).
	zipFlagsSignificant = 0x0001 | 0x0008 | 0x0800
	zipFlagDataDesc     = 0x0008
)

// zipCDEntry is one central directory record as parsed here, independently
// of archive/zip.
type zipCDEntry struct {
	name         []byte
	flags        uint16
	method       uint16
	crc          uint32
	csize, usize uint64
	offset       uint64
	zip64Sizes   bool
}

// checkZipLayout makes sure every reader sees the same members as
// archive/zip, which reads only the central directory. Streaming extractors
// (bsdtar reading stdin, Java's ZipInputStream, ...) read the local file
// headers in order instead, so the two views must agree exactly:
//
//   - exactly one end of central directory record, ending the file or
//     followed only by up to 64 KiB of zero padding (a comment is allowed
//     but may not contain another record signature);
//     a zip64 end record must sit right before its locator and agree with
//     the 32-bit record;
//   - the central directory ends where the end record begins, and its
//     offset is the physical one: no data before the first entry
//     (self-extracting stubs and other prefixes are rejected), no
//     concatenated archives;
//   - the local entries tile [0, central directory) without gaps or
//     overlaps, so no member is hidden from the central directory;
//   - every local header has the central directory's name, method,
//     significant flags and, when present, CRC-32 and sizes, and every
//     data descriptor agrees too;
//   - an Info-ZIP Unicode Path extra field (0x7075), in the local header
//     or the central directory, names exactly the header name and carries
//     that name's CRC-32; otherwise extractors honoring it would create a
//     different file than the one inspected. Other extra fields carry no
//     name that common extractors use (the Unicode Comment field 0x6375
//     only affects the comment) and are not interpreted;
//   - the declared number of entries (zip64 included) is at most
//     maxEntries, checked before anything is allocated for them.
func checkZipLayout(r io.ReaderAt, size int64, files []*zip.File, maxEntries int) error {
	cds, cdOffset, err := readZipDirectory(r, size, maxEntries)
	if err != nil {
		return err
	}
	if len(cds) != len(files) {
		return unsafeErr("zip central directory lists %d entries but %d were read", len(cds), len(files))
	}
	for i, e := range cds {
		f := files[i]
		if f.Name != string(e.name) || f.Method != e.method || f.Flags != e.flags || f.CRC32 != e.crc ||
			f.CompressedSize64 != e.csize || f.UncompressedSize64 != e.usize {
			return unsafeErr("zip central directory entry %q is ambiguous", f.Name)
		}
	}
	order := make([]*zipCDEntry, len(cds))
	for i := range cds {
		order[i] = &cds[i]
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].offset < order[j].offset })
	var pos uint64
	for _, e := range order {
		if e.offset != pos {
			if e.offset < pos {
				return unsafeErr("zip entry %q overlaps another entry", e.name)
			}
			if pos == 0 {
				return unsafeErr("zip archive has %d bytes of data before its first entry (self-extracting or prefixed archives are not accepted)", e.offset)
			}
			return unsafeErr("zip archive has %d unlisted bytes before entry %q", e.offset-pos, e.name)
		}
		if pos, err = checkZipLocal(r, e, cdOffset); err != nil {
			return err
		}
	}
	if pos != cdOffset {
		return unsafeErr("zip archive has %d unlisted bytes before its central directory", cdOffset-pos)
	}
	return nil
}

func readFull(r io.ReaderAt, off uint64, n int, limit uint64) ([]byte, error) {
	if n < 0 || off > limit || uint64(n) > limit-off { //nolint:gosec // G115: n is checked non-negative first
		return nil, unsafeErr("zip record at offset %d runs past its bounds", off)
	}
	b := make([]byte, n)
	if _, err := r.ReadAt(b, int64(off)); err != nil { //nolint:gosec // G115: off < limit, which is an int64 size
		return nil, unsafeErr("reading zip record at offset %d: %v", off, err)
	}
	return b, nil
}

// readZipDirectory locates the end records and parses the central
// directory, returning its entries in directory order and its offset.
func readZipDirectory(r io.ReaderAt, size int64, maxEntries int) ([]zipCDEntry, uint64, error) {
	if size < zipEndLen {
		return nil, 0, unsafeErr("zip archive is too short")
	}
	usize := uint64(size)
	tailLen := min(usize, zipEndLen+zipMaxComment+zipMaxPadding)
	tail, err := readFull(r, usize-tailLen, int(tailLen), usize) //nolint:gosec // G115: tailLen is below 140 KiB
	if err != nil {
		return nil, 0, err
	}
	// The end record is the last signature whose comment ends the file or
	// is followed by zero padding only.
	padStart := len(tail)
	for padStart > 0 && tail[padStart-1] == 0 && len(tail)-padStart < zipMaxPadding {
		padStart--
	}
	at := -1
	for i := len(tail) - zipEndLen; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) != zipSigEnd {
			continue
		}
		if stop := i + zipEndLen + int(binary.LittleEndian.Uint16(tail[i+20:])); stop >= padStart && stop <= len(tail) {
			at = i
			break
		}
	}
	if at < 0 {
		return nil, 0, unsafeErr("zip end of central directory record not found at the end of the file (trailing data?)")
	}
	tail = tail[:at+zipEndLen+int(binary.LittleEndian.Uint16(tail[at+20:]))]
	end := tail[at:]
	comment := end[zipEndLen:]
	for _, sig := range []uint32{zipSigEnd, zipSigEnd64, zipSigLocator64, zipSigCentral} {
		var s [4]byte
		binary.LittleEndian.PutUint32(s[:], sig)
		if bytes.Contains(comment, s[:]) {
			return nil, 0, unsafeErr("zip comment contains a record signature (several end records)")
		}
	}
	endOff := usize - tailLen + uint64(at) //nolint:gosec // G115: at >= 0
	disk := binary.LittleEndian.Uint16(end[4:])
	cdDisk := binary.LittleEndian.Uint16(end[6:])
	count := uint64(binary.LittleEndian.Uint16(end[10:]))
	diskCount := uint64(binary.LittleEndian.Uint16(end[8:]))
	cdSize := uint64(binary.LittleEndian.Uint32(end[12:]))
	cdOffset := uint64(binary.LittleEndian.Uint32(end[16:]))
	if disk != 0 || cdDisk != 0 || diskCount != count {
		return nil, 0, unsafeErr("multi-disk zip archives are not supported")
	}
	cdEnd := endOff
	isLocator := endOff >= zipLocator64Len
	if isLocator {
		loc, err := readFull(r, endOff-zipLocator64Len, zipLocator64Len, usize)
		if err != nil {
			return nil, 0, err
		}
		isLocator = binary.LittleEndian.Uint32(loc) == zipSigLocator64
		if isLocator {
			if binary.LittleEndian.Uint32(loc[4:]) != 0 || binary.LittleEndian.Uint32(loc[16:]) > 1 {
				return nil, 0, unsafeErr("multi-disk zip archives are not supported")
			}
			off64 := binary.LittleEndian.Uint64(loc[8:])
			locOff := endOff - zipLocator64Len
			e64, err := readFull(r, off64, zipEnd64MinLen, locOff)
			if err != nil {
				return nil, 0, err
			}
			recLen := binary.LittleEndian.Uint64(e64[4:])
			if binary.LittleEndian.Uint32(e64) != zipSigEnd64 || recLen < zipEnd64MinLen-12 || recLen > locOff || off64+12+recLen != locOff {
				return nil, 0, unsafeErr("zip64 end of central directory record is malformed or not where its locator says")
			}
			if binary.LittleEndian.Uint32(e64[16:]) != 0 || binary.LittleEndian.Uint32(e64[20:]) != 0 ||
				binary.LittleEndian.Uint64(e64[24:]) != binary.LittleEndian.Uint64(e64[32:]) {
				return nil, 0, unsafeErr("multi-disk zip archives are not supported")
			}
			c64 := binary.LittleEndian.Uint64(e64[32:])
			s64 := binary.LittleEndian.Uint64(e64[40:])
			o64 := binary.LittleEndian.Uint64(e64[48:])
			if (count != zipUint16Max && count != c64) || (cdSize != zipUint32Max && cdSize != s64) || (cdOffset != zipUint32Max && cdOffset != o64) {
				return nil, 0, unsafeErr("zip64 end record disagrees with the end of central directory record")
			}
			count, cdSize, cdOffset, cdEnd = c64, s64, o64, off64
		}
	}
	if !isLocator && (count == zipUint16Max || cdSize == zipUint32Max || cdOffset == zipUint32Max) {
		return nil, 0, unsafeErr("zip end record needs a zip64 record that is missing")
	}
	if cdOffset > cdEnd || cdSize != cdEnd-cdOffset {
		return nil, 0, unsafeErr("zip central directory is not where the end record says (prefixed, concatenated or padded archive)")
	}
	if maxEntries < 0 || count > uint64(maxEntries) {
		return nil, 0, unsafeErr("zip end record declares %d entries, more than the limit of %d", count, maxEntries)
	}
	if count > cdSize/zipCentralLen {
		return nil, 0, unsafeErr("zip end record claims %d entries in a %d-byte central directory", count, cdSize)
	}
	br := bufio.NewReader(io.NewSectionReader(r, int64(cdOffset), int64(cdSize))) //nolint:gosec // G115: both are below size
	entries := make([]zipCDEntry, 0, count)
	var read uint64
	for range count {
		hdr := make([]byte, zipCentralLen)
		if _, err := io.ReadFull(br, hdr); err != nil {
			return nil, 0, unsafeErr("reading zip central directory: %v", err)
		}
		if binary.LittleEndian.Uint32(hdr) != zipSigCentral {
			return nil, 0, unsafeErr("zip central directory record has a bad signature")
		}
		nameLen := int(binary.LittleEndian.Uint16(hdr[28:]))
		extraLen := int(binary.LittleEndian.Uint16(hdr[30:]))
		commentLen := int(binary.LittleEndian.Uint16(hdr[32:]))
		rest := make([]byte, nameLen+extraLen+commentLen)
		if _, err := io.ReadFull(br, rest); err != nil {
			return nil, 0, unsafeErr("reading zip central directory: %v", err)
		}
		read += uint64(zipCentralLen + len(rest))
		if err := checkUnicodePath(rest[:nameLen], rest[nameLen:nameLen+extraLen], "central directory"); err != nil {
			return nil, 0, err
		}
		e := zipCDEntry{
			name:   rest[:nameLen],
			flags:  binary.LittleEndian.Uint16(hdr[8:]),
			method: binary.LittleEndian.Uint16(hdr[10:]),
			crc:    binary.LittleEndian.Uint32(hdr[16:]),
			csize:  uint64(binary.LittleEndian.Uint32(hdr[20:])),
			usize:  uint64(binary.LittleEndian.Uint32(hdr[24:])),
			offset: uint64(binary.LittleEndian.Uint32(hdr[42:])),
		}
		diskStart := uint32(binary.LittleEndian.Uint16(hdr[34:]))
		if e.usize == zipUint32Max || e.csize == zipUint32Max || e.offset == zipUint32Max || diskStart == zipUint16Max {
			z, ok := zip64Extra(rest[nameLen : nameLen+extraLen])
			fields := []*uint64{}
			if e.usize == zipUint32Max {
				fields = append(fields, &e.usize)
			}
			if e.csize == zipUint32Max {
				fields = append(fields, &e.csize)
			}
			if e.offset == zipUint32Max {
				fields = append(fields, &e.offset)
			}
			if !ok || len(z) < 8*len(fields) {
				return nil, 0, unsafeErr("zip entry %q lacks its zip64 extra field", e.name)
			}
			for i, f := range fields {
				*f = binary.LittleEndian.Uint64(z[8*i:])
			}
			if diskStart == zipUint16Max {
				if len(z) < 8*len(fields)+4 {
					return nil, 0, unsafeErr("zip entry %q lacks its zip64 extra field", e.name)
				}
				diskStart = binary.LittleEndian.Uint32(z[8*len(fields):])
			}
		}
		if diskStart != 0 {
			return nil, 0, unsafeErr("multi-disk zip archives are not supported")
		}
		e.zip64Sizes = e.usize >= zipUint32Max || e.csize >= zipUint32Max
		entries = append(entries, e)
	}
	if read != cdSize {
		return nil, 0, unsafeErr("zip central directory has %d bytes of unlisted data", cdSize-read)
	}
	return entries, cdOffset, nil
}

// checkUnicodePath accepts an Info-ZIP Unicode Path extra field only when
// it is version 1, its CRC-32 is that of the header name, and its UTF-8 name
// is the header name byte for byte. A malformed extra block ends parsing
// the way extractors end it.
func checkUnicodePath(name, extra []byte, where string) error {
	seen := false
	for len(extra) >= 4 {
		id := binary.LittleEndian.Uint16(extra)
		n := int(binary.LittleEndian.Uint16(extra[2:]))
		if 4+n > len(extra) {
			if id == zipUnicodePathID {
				return unsafeErr("zip entry %q has a truncated Unicode Path extra field in its %s", name, where)
			}
			return nil
		}
		if id == zipUnicodePathID {
			d := extra[4 : 4+n]
			if seen {
				return unsafeErr("zip entry %q has several Unicode Path extra fields in its %s", name, where)
			}
			seen = true
			if len(d) < 5 || d[0] != 1 || binary.LittleEndian.Uint32(d[1:]) != crc32.ChecksumIEEE(name) || !bytes.Equal(d[5:], name) {
				return unsafeErr("zip entry %q has a Unicode Path extra field in its %s naming another file; extractors honoring it would use that name", name, where)
			}
		}
		extra = extra[4+n:]
	}
	return nil
}

// zip64Extra returns the data of the zip64 extended information field.
func zip64Extra(extra []byte) ([]byte, bool) {
	for len(extra) >= 4 {
		id := binary.LittleEndian.Uint16(extra)
		n := int(binary.LittleEndian.Uint16(extra[2:]))
		if 4+n > len(extra) {
			return nil, false
		}
		if id == zipZip64ExtraID {
			return extra[4 : 4+n], true
		}
		extra = extra[4+n:]
	}
	return nil, false
}

// checkZipLocal compares the local header (and data descriptor) at e.offset
// with the central directory record and returns the offset just past the
// entry. Nothing may extend to limit (the central directory).
func checkZipLocal(r io.ReaderAt, e *zipCDEntry, limit uint64) (uint64, error) {
	hdr, err := readFull(r, e.offset, zipLocalLen, limit)
	if err != nil {
		return 0, err
	}
	if binary.LittleEndian.Uint32(hdr) != zipSigLocal {
		return 0, unsafeErr("zip entry %q has no local file header where the central directory says", e.name)
	}
	nameLen := int(binary.LittleEndian.Uint16(hdr[26:]))
	extraLen := int(binary.LittleEndian.Uint16(hdr[28:]))
	rest, err := readFull(r, e.offset+zipLocalLen, nameLen+extraLen, limit)
	if err != nil {
		return 0, err
	}
	if !bytes.Equal(rest[:nameLen], e.name) {
		return 0, unsafeErr("zip entry %q is named %q in its local file header; streaming extractors would use that name", e.name, rest[:nameLen])
	}
	// The local Unicode Path, if any, must name the same file as well
	// (the central one was checked while reading the directory).
	if err := checkUnicodePath(rest[:nameLen], rest[nameLen:], "local file header"); err != nil {
		return 0, err
	}
	flags := binary.LittleEndian.Uint16(hdr[6:])
	if flags&zipFlagsSignificant != e.flags&zipFlagsSignificant || binary.LittleEndian.Uint16(hdr[8:]) != e.method {
		return 0, unsafeErr("zip entry %q has different flags or method in its local file header", e.name)
	}
	zip64Local := false
	if flags&zipFlagDataDesc == 0 {
		crc := binary.LittleEndian.Uint32(hdr[14:])
		csize := uint64(binary.LittleEndian.Uint32(hdr[18:]))
		usize := uint64(binary.LittleEndian.Uint32(hdr[22:]))
		if csize == zipUint32Max || usize == zipUint32Max {
			z, ok := zip64Extra(rest[nameLen:])
			if !ok || len(z) < 16 {
				return 0, unsafeErr("zip entry %q lacks the zip64 extra field in its local file header", e.name)
			}
			usize, csize = binary.LittleEndian.Uint64(z), binary.LittleEndian.Uint64(z[8:])
		}
		if crc != e.crc || csize != e.csize || usize != e.usize {
			return 0, unsafeErr("zip entry %q has a different CRC-32 or size in its local file header", e.name)
		}
	} else {
		_, zip64Local = zip64Extra(rest[nameLen:])
	}
	dataStart := e.offset + zipLocalLen + uint64(nameLen+extraLen) //nolint:gosec // G115: lengths are uint16 sums
	if e.csize > limit || dataStart > limit-e.csize {
		return 0, unsafeErr("zip entry %q runs into the central directory", e.name)
	}
	pos := dataStart + e.csize
	if flags&zipFlagDataDesc == 0 {
		return pos, nil
	}
	// Data descriptor: optional signature, CRC-32, then 4- or 8-byte sizes.
	sizeLen := 4
	if e.zip64Sizes || zip64Local {
		sizeLen = 8
	}
	n := 4 + 2*sizeLen
	sig, err := readFull(r, pos, 4, limit)
	if err != nil {
		return 0, unsafeErr("zip entry %q: data descriptor missing", e.name)
	}
	if binary.LittleEndian.Uint32(sig) == zipSigDataDesc {
		pos += 4
	}
	dd, err := readFull(r, pos, n, limit)
	if err != nil {
		return 0, unsafeErr("zip entry %q: data descriptor missing", e.name)
	}
	crc := binary.LittleEndian.Uint32(dd)
	var csize, usize uint64
	if sizeLen == 8 {
		csize, usize = binary.LittleEndian.Uint64(dd[4:]), binary.LittleEndian.Uint64(dd[12:])
	} else {
		csize, usize = uint64(binary.LittleEndian.Uint32(dd[4:])), uint64(binary.LittleEndian.Uint32(dd[8:]))
	}
	if crc != e.crc || csize != e.csize || usize != e.usize {
		return 0, unsafeErr("zip entry %q has a data descriptor that disagrees with the central directory", e.name)
	}
	return pos + uint64(n), nil //nolint:gosec // G115: n is at most 20
}
