package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// rawTar returns an uncompressed tar stream (with its end marker).
func rawTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: typ, Mode: 0o644}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipBytes(t testing.TB, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func wantUnsafe(t *testing.T, name string, data []byte, format, want string) {
	t.Helper()
	_, err := inspectBytes(data, format, InspectLimits{})
	if !errors.Is(err, ErrUnsafeArchive) || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: err = %v, want ErrUnsafeArchive containing %q", name, err, want)
	}
}

func wantSafe(t *testing.T, name string, data []byte, format string) {
	t.Helper()
	if _, err := inspectBytes(data, format, InspectLimits{}); err != nil {
		t.Errorf("%s: %v", name, err)
	}
}

// F5: members after the tar end-of-archive marker are invisible to
// archive/tar but extracted by "tar --ignore-zeros" and by readers of
// concatenated archives.
func TestInspectTarTrailer(t *testing.T) {
	good := rawTar(t, tarEntry{name: "bin/demo", body: "hello"})
	evil := rawTar(t,
		tarEntry{name: "../../../../tmp/heph-evil.txt", body: "pwnd"},
		tarEntry{name: "lnk", typ: tar.TypeSymlink, link: "/etc/passwd"},
	)
	wantUnsafe(t, "hidden members", gzipBytes(t, append(append([]byte{}, good...), evil...)), "tar.gz", "end-of-archive")
	wantUnsafe(t, "one stray byte", gzipBytes(t, append(append([]byte{}, good...), 0, 0, 1)), "tar.gz", "end-of-archive")
	// A second gzip member holding the evil tar.
	wantUnsafe(t, "second gzip member", append(gzipBytes(t, good), gzipBytes(t, evil)...), "tar.gz", "end-of-archive")
	// Garbage after the gzip stream.
	wantUnsafe(t, "garbage after gzip", append(gzipBytes(t, good), []byte("PK\x03\x04junk")...), "tar.gz", "end-of-archive")
	// Too much zero padding.
	wantUnsafe(t, "huge padding", gzipBytes(t, append(append([]byte{}, good...), make([]byte, maxTarTrailer+512)...)), "tar.gz", "padding")

	// Record padding as written by GNU tar and bsdtar (10 KiB records) is
	// fine, as is a stream without any end marker.
	padded := append(append([]byte{}, good...), make([]byte, 10240-len(good)%10240)...)
	wantSafe(t, "record padding", gzipBytes(t, padded), "tar.gz")
	wantSafe(t, "no end marker", gzipBytes(t, good[:len(good)-1024]), "tar.gz")
}

// rawZipEntry describes one stored member for rawZip.
type rawZipEntry struct {
	name      string // name in the central directory
	localName string // name in the local header ("" = name)
	body      string
	hidden    bool // local entry only, absent from the central directory
	zip64     bool // sizes (and offset) through zip64 extra fields
	localCRC  uint32
	desc      bool // sizes and CRC in a data descriptor
	offset    int  // >0: central directory offset of entry offset-1
	// localExtra and centralExtra are appended to the extra fields.
	localExtra, centralExtra []byte
}

type rawZipOptions struct {
	prefix   []byte // bytes before the first entry; offsets stay physical
	zip64End bool   // write zip64 end records
	comment  string
	trailer  []byte // bytes after the end record
}

// rawZip builds a stored-only zip archive byte by byte so tests can produce
// layouts archive/zip refuses to write.
func rawZip(entries []rawZipEntry, o rawZipOptions) []byte {
	le := binary.LittleEndian
	var b bytes.Buffer
	b.Write(o.prefix)
	type cd struct {
		e      rawZipEntry
		crc    uint32
		offset uint64
	}
	var dir []cd
	var offsets []uint64
	for _, e := range entries {
		off := uint64(b.Len())
		offsets = append(offsets, off)
		crc := crc32.ChecksumIEEE([]byte(e.body))
		local := e.localName
		if local == "" {
			local = e.name
		}
		lcrc := crc
		if e.localCRC != 0 {
			lcrc = e.localCRC
		}
		size := uint32(len(e.body))
		var extra []byte
		if e.zip64 {
			size = 0xffffffff
			extra = le.AppendUint16(extra, 1)
			extra = le.AppendUint16(extra, 16)
			extra = le.AppendUint64(extra, uint64(len(e.body)))
			extra = le.AppendUint64(extra, uint64(len(e.body)))
		}
		extra = append(extra, e.localExtra...)
		var flags uint16
		if e.desc {
			flags = 0x8
		}
		h := le.AppendUint32(nil, zipSigLocal)
		h = le.AppendUint16(h, 45)
		h = le.AppendUint16(h, flags)
		h = le.AppendUint16(h, 0) // store
		h = le.AppendUint32(h, 0) // time, date
		if e.desc {
			h = le.AppendUint32(h, 0)
			h = le.AppendUint32(h, 0)
			h = le.AppendUint32(h, 0)
		} else {
			h = le.AppendUint32(h, lcrc)
			h = le.AppendUint32(h, size)
			h = le.AppendUint32(h, size)
		}
		h = le.AppendUint16(h, uint16(len(local)))
		h = le.AppendUint16(h, uint16(len(extra)))
		b.Write(h)
		b.WriteString(local)
		b.Write(extra)
		b.WriteString(e.body)
		if e.desc {
			d := le.AppendUint32(nil, zipSigDataDesc)
			d = le.AppendUint32(d, crc)
			if e.zip64 {
				d = le.AppendUint64(d, uint64(len(e.body)))
				d = le.AppendUint64(d, uint64(len(e.body)))
			} else {
				d = le.AppendUint32(d, uint32(len(e.body)))
				d = le.AppendUint32(d, uint32(len(e.body)))
			}
			b.Write(d)
		}
		if !e.hidden {
			dir = append(dir, cd{e, crc, off})
		}
	}
	cdOff := uint64(b.Len())
	for _, c := range dir {
		e := c.e
		off := c.offset
		if e.offset > 0 {
			off = offsets[e.offset-1]
		}
		size, off32 := uint32(len(e.body)), uint32(off)
		var extra []byte
		if e.zip64 {
			size, off32 = 0xffffffff, 0xffffffff
			extra = le.AppendUint16(extra, 1)
			extra = le.AppendUint16(extra, 24)
			extra = le.AppendUint64(extra, uint64(len(e.body)))
			extra = le.AppendUint64(extra, uint64(len(e.body)))
			extra = le.AppendUint64(extra, off)
		}
		extra = append(extra, e.centralExtra...)
		var flags uint16
		if e.desc {
			flags = 0x8
		}
		h := le.AppendUint32(nil, zipSigCentral)
		h = le.AppendUint16(h, 3<<8|45)
		h = le.AppendUint16(h, 45)
		h = le.AppendUint16(h, flags)
		h = le.AppendUint16(h, 0)
		h = le.AppendUint32(h, 0)
		h = le.AppendUint32(h, c.crc)
		h = le.AppendUint32(h, size)
		h = le.AppendUint32(h, size)
		h = le.AppendUint16(h, uint16(len(e.name)))
		h = le.AppendUint16(h, uint16(len(extra)))
		h = le.AppendUint16(h, 0) // comment
		h = le.AppendUint16(h, 0) // disk
		h = le.AppendUint16(h, 0) // internal attributes
		h = le.AppendUint32(h, 0o100644<<16)
		h = le.AppendUint32(h, off32)
		b.Write(h)
		b.WriteString(e.name)
		b.Write(extra)
	}
	cdSize := uint64(b.Len()) - cdOff
	count := uint64(len(dir))
	c16, s32, o32 := uint16(count), uint32(cdSize), uint32(cdOff)
	if o.zip64End {
		end64 := uint64(b.Len())
		h := le.AppendUint32(nil, zipSigEnd64)
		h = le.AppendUint64(h, 44)
		h = le.AppendUint16(h, 45)
		h = le.AppendUint16(h, 45)
		h = le.AppendUint32(h, 0)
		h = le.AppendUint32(h, 0)
		h = le.AppendUint64(h, count)
		h = le.AppendUint64(h, count)
		h = le.AppendUint64(h, cdSize)
		h = le.AppendUint64(h, cdOff)
		h = le.AppendUint32(h, zipSigLocator64)
		h = le.AppendUint32(h, 0)
		h = le.AppendUint64(h, end64)
		h = le.AppendUint32(h, 1)
		b.Write(h)
		c16, s32, o32 = 0xffff, 0xffffffff, 0xffffffff
	}
	h := le.AppendUint32(nil, zipSigEnd)
	h = le.AppendUint16(h, 0)
	h = le.AppendUint16(h, 0)
	h = le.AppendUint16(h, c16)
	h = le.AppendUint16(h, c16)
	h = le.AppendUint32(h, s32)
	h = le.AppendUint32(h, o32)
	h = le.AppendUint16(h, uint16(len(o.comment)))
	b.Write(h)
	b.WriteString(o.comment)
	b.Write(o.trailer)
	return b.Bytes()
}

// F5: the zip builder's well-formed output, including zip64 records and
// data descriptors, is accepted by archive/zip and by Inspect.
func TestInspectZipWellFormedLayouts(t *testing.T) {
	two := []rawZipEntry{{name: "bin/demo", body: "hello"}, {name: "README", body: "r"}}
	cases := map[string][]byte{
		"plain":       rawZip(two, rawZipOptions{}),
		"comment":     rawZip(two, rawZipOptions{comment: "built by test"}),
		"padding":     rawZip(two, rawZipOptions{comment: "c", trailer: make([]byte, 10240)}),
		"zip64 end":   rawZip(two, rawZipOptions{zip64End: true}),
		"zip64 entry": rawZip([]rawZipEntry{{name: "a", body: "abc", zip64: true}, {name: "b", body: "x"}}, rawZipOptions{zip64End: true}),
		"descriptor":  rawZip([]rawZipEntry{{name: "a", body: "abc", desc: true}, {name: "b", body: "x", desc: true, zip64: true}}, rawZipOptions{}),
		"empty":       rawZip(nil, rawZipOptions{}),
	}
	for name, data := range cases {
		if _, err := zip.NewReader(bytes.NewReader(data), int64(len(data))); err != nil {
			t.Errorf("%s: archive/zip rejects the fixture: %v", name, err)
			continue
		}
		wantSafe(t, name, data, "zip")
	}
}

// F5: layouts where a streaming extractor would see other members (or other
// names) than archive/zip's central directory view.
func TestInspectZipMismatches(t *testing.T) {
	ok := []rawZipEntry{{name: "bin/demo", body: "hello"}}
	// The secreview reproduction: only the local header name is changed.
	wantUnsafe(t, "local name", rawZip([]rawZipEntry{{name: "AAAAAAAAAAAAAAAAAAAAAAAAAAAA/demo", localName: "../../../../../tmp/x/demodemo", body: "hello"}}, rawZipOptions{}), "zip", "local file header")
	wantUnsafe(t, "local CRC", rawZip([]rawZipEntry{{name: "a", body: "hello", localCRC: 1}}, rawZipOptions{}), "zip", "CRC-32")
	wantUnsafe(t, "hidden local entry", rawZip([]rawZipEntry{{name: "a", body: "1"}, {name: "../evil", body: "pwnd", hidden: true}, {name: "b", body: "2"}}, rawZipOptions{}), "zip", "unlisted bytes")
	wantUnsafe(t, "hidden last entry", rawZip([]rawZipEntry{{name: "a", body: "1"}, {name: "../evil", body: "pwnd", hidden: true}}, rawZipOptions{}), "zip", "unlisted bytes")
	wantUnsafe(t, "overlap", rawZip([]rawZipEntry{{name: "a", body: "1"}, {name: "a", body: "1", offset: 1}}, rawZipOptions{}), "zip", "overlaps")
	wantUnsafe(t, "trailing data", rawZip(ok, rawZipOptions{trailer: []byte("trailing")}), "zip", "end of central directory")
	wantUnsafe(t, "comment with end record", rawZip(ok, rawZipOptions{comment: "PK\x05\x06 fake"}), "zip", "several end records")
	wantUnsafe(t, "fake end record in comment", rawZip(ok, rawZipOptions{comment: "PK\x05\x06" + strings.Repeat("\x00", 18)}), "zip", "")
	wantUnsafe(t, "too much padding", rawZip(ok, rawZipOptions{trailer: make([]byte, zipMaxPadding+1)}), "zip", "end of central directory")
	// A self-extracting stub or other prefix, with physical offsets.
	wantUnsafe(t, "prefix", rawZip(ok, rawZipOptions{prefix: []byte("MZ\x90\x00 stub")}), "zip", "before its first entry")
	// archive/zip.Writer.SetOffset (as SFX builders do).
	var buf bytes.Buffer
	buf.WriteString("#!/bin/sh\nexit 0\n")
	zw := zip.NewWriter(&buf)
	zw.SetOffset(int64(buf.Len()))
	w, _ := zw.Create("x")
	_, _ = w.Write([]byte("x"))
	_ = zw.Close()
	wantUnsafe(t, "SetOffset prefix", buf.Bytes(), "zip", "before its first entry")
	// A prefix with offsets relative to the archive (archive/zip copes by
	// computing a base offset; others do not).
	plain := makeZip(t, zipEntry{name: "x", body: "x"})
	wantUnsafe(t, "relative prefix", append([]byte("JUNKJUNK"), plain...), "zip", "prefixed")
	// Two archives concatenated: archive/zip sees only the second.
	evil := makeZip(t, zipEntry{name: "evil", body: "pwnd"})
	wantUnsafe(t, "concatenated", append(append([]byte{}, evil...), plain...), "zip", "")
	// zip64 end record that disagrees with the 32-bit record.
	z := rawZip(ok, rawZipOptions{zip64End: true})
	bad := append([]byte{}, z...)
	// Patch the 32-bit entry count (not a sentinel) to disagree.
	binary.LittleEndian.PutUint16(bad[len(bad)-22+10:], 7)
	binary.LittleEndian.PutUint16(bad[len(bad)-22+8:], 7)
	wantUnsafe(t, "zip64 disagreement", bad, "zip", "")
}

// F4: Inspect can scan member content for private keys.
func TestInspectScanSecrets(t *testing.T) {
	tgz := makeTarGz(t, tarEntry{name: "docs/gcp.json", body: gcpKey})
	z := makeZip(t, zipEntry{name: "docs/backup.asc", body: pgpKey})
	for format, data := range map[string][]byte{"tar.gz": tgz, "zip": z} {
		if _, err := inspectBytes(data, format, InspectLimits{}); err != nil {
			t.Errorf("%s without scanning: %v", format, err)
		}
		_, err := inspectBytes(data, format, InspectLimits{ScanSecrets: true})
		if !errors.Is(err, ErrSensitiveContent) {
			t.Errorf("%s: err = %v, want ErrSensitiveContent", format, err)
		}
	}
}

func TestSensitiveEntry(t *testing.T) {
	for p, dir := range map[string]bool{".env": false, "pkg/.Git/config": false, "pkg/.git": true, "x/.SSH": true, "a/ID_RSA": false} {
		if SensitiveEntry(p, dir) == "" {
			t.Errorf("SensitiveEntry(%q, %v) = \"\"", p, dir)
		}
	}
	for p, dir := range map[string]bool{"bin/tool": false, "docs": true, "id_rsa.pub": false, ".github": true} {
		if why := SensitiveEntry(p, dir); why != "" {
			t.Errorf("SensitiveEntry(%q, %v) = %q", p, dir, why)
		}
	}
}

// F5: archives produced by common tools must keep passing the stricter
// checks.
func TestInspectArchivesFromSystemTools(t *testing.T) {
	src := t.TempDir()
	for name, body := range map[string]string{"pkg/bin/tool": "TOOL", "pkg/README.md": strings.Repeat("readme ", 500)} {
		p := filepath.Join(src, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	out := t.TempDir()
	type tool struct {
		format string
		args   []string
	}
	tools := map[string]tool{
		"zip":          {"zip", []string{"zip", "-qr", "OUT", "pkg"}},
		"zip-stream":   {"zip", []string{"sh", "-c", "zip -qr - pkg | cat > OUT"}},
		"bsdtar-zip":   {"zip", []string{"bsdtar", "--format", "zip", "-cf", "OUT", "pkg"}},
		"bsdtar-tgz":   {"tar.gz", []string{"bsdtar", "-czf", "OUT", "pkg"}},
		"tar-tgz":      {"tar.gz", []string{"tar", "-czf", "OUT", "pkg"}},
		"ditto-zip":    {"zip", []string{"ditto", "-c", "-k", "--norsrc", "--keepParent", "pkg", "OUT"}},
		"git-tgz":      {"tar.gz", []string{"sh", "-c", "git init -q . && git add pkg && git -c user.name=t -c user.email=t@t commit -qm x && git archive --format=tar.gz -o OUT HEAD"}},
		"git-zip":      {"zip", []string{"sh", "-c", "git archive --format=zip -o OUT HEAD"}},
		"bsdtar-stdin": {"zip", []string{"sh", "-c", "bsdtar --format zip -cf - pkg > OUT"}},
	}
	for _, name := range []string{"zip", "zip-stream", "bsdtar-zip", "bsdtar-stdin", "bsdtar-tgz", "tar-tgz", "ditto-zip", "git-tgz", "git-zip"} {
		tl := tools[name]
		t.Run(name, func(t *testing.T) {
			bin := tl.args[0]
			if bin == "sh" {
				bin = strings.Fields(tl.args[2])[0]
			}
			if _, err := exec.LookPath(bin); err != nil {
				t.Skipf("%s not installed", bin)
			}
			dest := filepath.Join(out, name+"."+tl.format)
			args := make([]string, len(tl.args))
			for i, a := range tl.args {
				args[i] = strings.ReplaceAll(a, "OUT", dest)
			}
			cmd := exec.Command(args[0], args[1:]...)
			cmd.Dir = src
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Skipf("%v: %v\n%s", args, err, b)
			}
			files, err := inspectFile(t, dest, tl.format, InspectLimits{ScanSecrets: true})
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			found := false
			for _, f := range files {
				found = found || f.Path == "pkg/bin/tool"
			}
			if !found {
				t.Errorf("entries = %+v", files)
			}
		})
	}
}
