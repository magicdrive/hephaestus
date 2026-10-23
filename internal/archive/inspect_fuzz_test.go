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
