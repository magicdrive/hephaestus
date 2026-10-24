package archive

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tarBlockFor builds one raw ustar header block.
func tarBlockFor(typ byte, name, prefix, link string, size int64) []byte {
	b := make([]byte, tarBlock)
	copy(b[0:100], name)
	copy(b[100:108], "0000644\x00")
	copy(b[108:116], "0000000\x00")
	copy(b[116:124], "0000000\x00")
	copy(b[124:136], fmt.Sprintf("%011o\x00", size))
	copy(b[136:148], "00000000000\x00")
	b[156] = typ
	copy(b[157:257], link)
	copy(b[257:265], "ustar\x0000")
	copy(b[345:500], prefix)
	copy(b[148:156], "        ")
	var sum int64
	for _, c := range b {
		sum += int64(c)
	}
	copy(b[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return b
}

func padBlock(data []byte) []byte {
	out := append([]byte{}, data...)
	if r := len(out) % tarBlock; r != 0 {
		out = append(out, make([]byte, tarBlock-r)...)
	}
	return out
}

func paxRecords(kv ...string) []byte {
	var b bytes.Buffer
	for i := 0; i < len(kv); i += 2 {
		rec := " " + kv[i] + "=" + kv[i+1] + "\n"
		n := len(rec) + 1
		for len(fmt.Sprint(n))+len(rec) != n {
			n++
		}
		fmt.Fprintf(&b, "%d%s", n, rec)
	}
	return b.Bytes()
}

// rawEntry is one member of a hand-built tar stream.
type rawEntry struct {
	ext    [][]byte // extended header blocks with their data, in order
	name   string   // ustar name
	prefix string   // ustar prefix
	link   string
	typ    byte
	body   string
	size   int64 // ustar size; -1 = len(body)
}

func extHeader(typ byte, data []byte) []byte {
	return append(tarBlockFor(typ, "././@ext", "", "", int64(len(data))), padBlock(data)...)
}

func rawTarGz(t testing.TB, entries ...rawEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, e := range entries {
		for _, x := range e.ext {
			b.Write(x)
		}
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		size := e.size
		if size < 0 {
			size = int64(len(e.body))
		}
		b.Write(tarBlockFor(typ, e.name, e.prefix, e.link, size))
		b.Write(padBlock([]byte(e.body)))
	}
	b.Write(make([]byte, 2*tarBlock))
	return gzipBytes(t, b.Bytes())
}

// Extractors that ignore PAX records or GNU long names use the ustar header,
// so the two must name the same file.
func TestInspectTarPAXNameMismatch(t *testing.T) {
	long := "pkg/" + strings.Repeat("d", 120) + "/tool"
	cases := map[string]rawEntry{
		"pax path over ../ name":              {ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("path", "bin/tool"))}, name: "../evil", body: "x", size: -1},
		"pax path over other file":            {ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("path", "bin/tool"))}, name: "bin/.git/config", body: "x", size: -1},
		"gnu long name over other file":       {ext: [][]byte{extHeader(tar.TypeGNULongName, []byte("bin/tool\x00"))}, name: "../../evil", body: "x", size: -1},
		"long pax path, short other name":     {ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("path", long))}, name: "bin/tool", body: "x", size: -1},
		"long pax path, wrong prefix":         {ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("path", long))}, prefix: "etc", name: "tool", body: "x", size: -1},
		"short base under long dir":           {ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("path", "pkg/"+strings.Repeat("d", 200)+"/"+strings.Repeat("t", 120)))}, prefix: "pkg", name: "t", body: "x", size: -1},
		"gnu long name differs from pax path": {ext: [][]byte{extHeader(tar.TypeGNULongName, []byte(long+"x\x00")), extHeader(tar.TypeXHeader, paxRecords("path", long))}, name: long[:100], body: "x", size: -1},
		"pax path differs from gnu long name": {ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("path", long+"x")), extHeader(tar.TypeGNULongName, []byte(long+"\x00"))}, name: long[:100], body: "x", size: -1},
		"two pax headers":                     {ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("mtime", "1")), extHeader(tar.TypeXHeader, paxRecords("path", "a"))}, name: "a", body: "x", size: -1},
		"pax size hides data":                 {ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("size", "1536"))}, name: "a", body: strings.Repeat("y", 1536), size: 0},
		"sparse records":                      {ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("GNU.sparse.name", "a"))}, name: "a", body: "x", size: -1},
	}
	for name, e := range cases {
		wantUnsafe(t, name, rawTarGz(t, e), "tar.gz", "")
	}
	// A global header setting a path.
	g := rawEntry{typ: tar.TypeXGlobalHeader, name: "pax_global_header", body: string(paxRecords("path", "evil")), size: -1}
	wantUnsafe(t, "global path", rawTarGz(t, g, rawEntry{name: "a", body: "x", size: -1}), "tar.gz", "global PAX header")
	// An abbreviated ustar name that is another member's path.
	longFile := "pkg/" + strings.Repeat("d", 96) + "/" + strings.Repeat("f", 30)
	wantUnsafe(t, "abbreviation collides", rawTarGz(t,
		rawEntry{name: longFile[:100], body: "legit", size: -1},
		rawEntry{ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("path", longFile))}, name: longFile[:100], body: "evil", size: -1},
	), "tar.gz", "also the path")

	// Accepted abbreviations.
	good := map[string]rawEntry{
		"gnu truncation":       {ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("path", long))}, name: long[:100], body: "x", size: -1},
		"gnu long name":        {ext: [][]byte{extHeader(tar.TypeGNULongName, []byte(long+"\x00"))}, name: long[:100], body: "x", size: -1},
		"empty ustar name":     {ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("path", long))}, name: "", body: "x", size: -1},
		"go non-ascii":         {ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("path", "docs/café.txt"))}, name: "docs/caf.txt", body: "x", size: -1},
		"libarchive prefix":    {ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("path", "p/"+strings.Repeat("a", 100)+"/"+strings.Repeat("b", 100)+"/c.txt"))}, prefix: "p/" + strings.Repeat("a", 100), name: "c.txt", body: "x", size: -1},
		"libarchive long base": {ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("path", "pkg/"+strings.Repeat("f", 110)+".txt"))}, prefix: "pkg", name: strings.Repeat("f", 98), body: "x", size: -1},
		"pax size agrees":      {ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("size", "1"))}, name: "a", body: "x", size: -1},
		"pax mtime only":       {ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("mtime", "1.5"))}, name: "a", body: "x", size: -1},
	}
	for name, e := range good {
		wantSafe(t, name, rawTarGz(t, e), "tar.gz")
	}
}

// Archives written by archive/tar (as GoReleaser does) and bsdtar with long
// and non-ASCII names stay accepted.
func TestInspectTarPAXFromWriters(t *testing.T) {
	names := []string{
		"pkg/" + strings.Repeat("d", 120) + "/" + strings.Repeat("f", 110) + ".txt",
		"p/" + strings.Repeat("a", 100) + "/" + strings.Repeat("b", 100) + "/c.txt",
		"q/" + strings.Repeat("h", 150) + ".txt",
		"docs/café.txt",
		"日本語/ファイル.txt",
		"日本語/別の.txt",
	}
	for _, format := range []tar.Format{tar.FormatPAX, tar.FormatGNU} {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		for _, n := range names {
			if format == tar.FormatGNU && !isASCII(n) {
				continue
			}
			if err := tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: 1, Format: format}); err != nil {
				t.Fatal(err)
			}
			_, _ = tw.Write([]byte("x"))
		}
		long := "pkg/" + strings.Repeat("d", 120) + "/"
		if err := tw.WriteHeader(&tar.Header{Name: long, Typeflag: tar.TypeDir, Mode: 0o755, Format: format}); err != nil {
			t.Fatal(err)
		}
		_ = tw.Close()
		wantSafe(t, fmt.Sprint("archive/tar ", format), gzipBytes(t, b.Bytes()), "tar.gz")
	}

	if _, err := exec.LookPath("bsdtar"); err != nil {
		t.Skip("bsdtar not installed")
	}
	src := t.TempDir()
	for _, n := range names {
		p := filepath.Join(src, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, format := range []string{"pax", "gnutar"} {
		out := filepath.Join(t.TempDir(), "a.tar.gz")
		cmd := exec.Command("bsdtar", "--format", format, "-czf", out, "pkg", "p", "q", "docs", "日本語")
		cmd.Dir = src
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bsdtar: %v\n%s", err, b)
		}
		if _, err := inspectFile(t, out, "tar.gz", InspectLimits{}); err != nil {
			t.Errorf("bsdtar %s: %v", format, err)
		}
	}
}
