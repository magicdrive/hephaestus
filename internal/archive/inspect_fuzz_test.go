package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/magicdrive/hephaestus/internal/pathutil"
)

// FuzzInspect feeds arbitrary bytes to the tar.gz and zip inspectors. Inputs
// come from untrusted imported archives, so the inspector must never panic or
// run away, and anything it accepts must be safe to extract.
func FuzzInspect(f *testing.F) {
	var tgz bytes.Buffer
	gz := gzip.NewWriter(&tgz)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "bin/tool", Typeflag: tar.TypeReg, Mode: 0o755, Size: 4})
	_, _ = tw.Write([]byte("tool"))
	_ = tw.Close()
	_ = gz.Close()
	f.Add(tgz.Bytes(), false)

	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create("bin/tool")
	_, _ = w.Write([]byte("tool"))
	_ = zw.Close()
	f.Add(zb.Bytes(), true)

	// zip64 end records and zip64 extra fields, data descriptors (also
	// zip64), and Info-ZIP Unicode Path fields, matching and not.
	f.Add(rawZip([]rawZipEntry{{name: "bin/tool", body: "tool", zip64: true}, {name: "README", body: "r"}}, rawZipOptions{zip64End: true}), true)
	f.Add(rawZip([]rawZipEntry{{name: "a", body: "abc", desc: true}, {name: "b", body: "x", desc: true, zip64: true}}, rawZipOptions{}), true)
	var dd bytes.Buffer
	zw = zip.NewWriter(&dd)
	w, _ = zw.CreateHeader(&zip.FileHeader{Name: "bin/tool", Method: zip.Deflate})
	_, _ = w.Write(bytes.Repeat([]byte("tool"), 100))
	_ = zw.Close()
	f.Add(dd.Bytes(), true)
	f.Add(rawZip([]rawZipEntry{{name: "bin/tool", body: "tool", localExtra: unicodePathExtra("bin/tool", "bin/tool"), centralExtra: unicodePathExtra("bin/tool", "bin/tool")}}, rawZipOptions{}), true)
	f.Add(rawZip([]rawZipEntry{{name: "bin/tool", body: "tool", centralExtra: unicodePathExtra("bin/tool", "bin/.git/config")}}, rawZipOptions{}), true)

	// PAX headers: a long path, a global header, and records overriding
	// the ustar name.
	var pax bytes.Buffer
	gz = gzip.NewWriter(&pax)
	tw = tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "commit"}})
	long := "pkg/" + string(bytes.Repeat([]byte("d"), 120)) + "/tool"
	_ = tw.WriteHeader(&tar.Header{Name: long, Typeflag: tar.TypeReg, Mode: 0o755, Size: 4, Format: tar.FormatPAX})
	_, _ = tw.Write([]byte("tool"))
	_ = tw.WriteHeader(&tar.Header{Name: "docs/caf\u00e9.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1, Format: tar.FormatPAX, PAXRecords: map[string]string{"mtime": "1.5"}})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	f.Add(pax.Bytes(), false)

	// Extended headers that disagree with the ustar header (and one that
	// agrees), built byte by byte.
	tb := testing.TB(f)
	f.Add(rawTarGz(tb, rawEntry{ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("path", "bin/tool"))}, name: "../evil", body: "x", size: -1}), false)
	f.Add(rawTarGz(tb, rawEntry{ext: [][]byte{extHeader(tar.TypeGNULongName, []byte("bin/tool\x00"))}, name: "bin/.git/config", body: "x", size: -1}), false)
	f.Add(rawTarGz(tb, rawEntry{ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("size", "1536"))}, name: "a", body: string(bytes.Repeat([]byte("y"), 1536)), size: 0}), false)
	longPath := "pkg/" + string(bytes.Repeat([]byte("d"), 120)) + "/tool"
	f.Add(rawTarGz(tb, rawEntry{ext: [][]byte{extHeader(tar.TypeXHeader, paxRecords("path", longPath))}, name: longPath[:100], body: "x", size: -1}), false)

	lim := InspectLimits{MaxEntries: 64, MaxFileSize: 1 << 20, MaxTotalSize: 4 << 20, ScanSecrets: true}
	f.Fuzz(func(t *testing.T, data []byte, isZip bool) {
		format := "tar.gz"
		if isZip {
			format = "zip"
		}
		files, err := Inspect(bytes.NewReader(data), int64(len(data)), format, lim)
		if err != nil {
			return
		}
		if len(files) > lim.MaxEntries {
			t.Fatalf("accepted %d entries, limit %d", len(files), lim.MaxEntries)
		}
		for _, fl := range files {
			if err := pathutil.ValidateRelative(fl.Path); err != nil {
				t.Fatalf("accepted unsafe path %q: %v", fl.Path, err)
			}
			if fl.Type != "file" && fl.Type != "dir" {
				t.Fatalf("accepted entry type %q", fl.Type)
			}
		}
	})
}
