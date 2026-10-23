package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/manifest"
)

func inspectFile(t *testing.T, path, format string, lim InspectLimits) ([]manifest.File, error) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return Inspect(bytes.NewReader(data), int64(len(data)), format, lim)
}

// TestInspectRoundTrip inspects archives written by this package and checks
// the result matches the manifest entries recorded when writing them.
func TestInspectRoundTrip(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip"} {
		t.Run(format, func(t *testing.T) {
			root := fixture(t, map[string]string{
				"build/mycli*":      strings.Repeat("binary", 4096),
				"README.md":         "readme",
				"docs/guide/a.md":   "a",
				"docs/guide/b.md":   "",
				"docs/ref/deep.txt": "deep",
			})
			cfg := loadConfig(t, root, `
project: {name: mycli}
archives:
  - targets: [{os: linux, arch: amd64}]
    format: `+format+`
    wrap_in_directory: true
    files: [{src: build/mycli, dst: bin/}, README.md, "docs/**"]
`)
			res, _ := runOK(t, cfg)
			a := res.Archives[0]
			got, err := inspectFile(t, filepath.Join(root, a.Path), format, InspectLimits{})
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			want := append([]manifest.File(nil), a.Files...)
			for i := range want {
				want[i].Source = ""
			}
			if len(got) != len(want) {
				t.Fatalf("Inspect returned %d entries, manifest has %d:\n%+v\n%+v", len(got), len(want), got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
				}
			}
		})
	}
}

type tarEntry struct {
	name, link string
	typ        byte
	mode       int64
	body       string
}

func makeTarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: typ, Mode: mode, Format: tar.FormatPAX}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if typ == tar.TypeXGlobalHeader {
			h = &tar.Header{Typeflag: typ, PAXRecords: map[string]string{"comment": "commit"}}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("header %q: %v", e.name, err)
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
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type zipEntry struct {
	name string
	mode os.FileMode
	body string
}

func makeZip(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatalf("header %q: %v", e.name, err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func inspectBytes(data []byte, format string, lim InspectLimits) ([]manifest.File, error) {
	return Inspect(bytes.NewReader(data), int64(len(data)), format, lim)
}

func TestInspectAcceptsOrdinaryArchives(t *testing.T) {
	tgz := makeTarGz(t,
		tarEntry{name: "./", typ: tar.TypeDir, mode: 0o755},
		tarEntry{name: "./pkg/bin/tool", mode: 0o755, body: "TOOL"},
		tarEntry{name: "./pkg/README", body: "r"},
		tarEntry{name: "pax_global_header", typ: tar.TypeXGlobalHeader},
	)
	got, err := inspectBytes(tgz, "tar.gz", InspectLimits{})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	// Missing parent directories are not synthesized; "./" is dropped.
	if len(got) != 2 || got[0].Path != "pkg/README" || got[1].Path != "pkg/bin/tool" || got[1].Mode != "0755" || got[1].Size != 4 {
		t.Fatalf("entries = %+v", got)
	}
	z := makeZip(t,
		zipEntry{name: "b/", mode: os.ModeDir | 0o755},
		zipEntry{name: "b/x.exe", mode: 0o755, body: "MZ"},
		zipEntry{name: "a.txt", body: "a"},
	)
	got, err = inspectBytes(z, "zip", InspectLimits{})
	if err != nil {
		t.Fatalf("Inspect zip: %v", err)
	}
	if len(got) != 3 || got[0].Path != "a.txt" || got[1].Path != "b" || got[1].Type != "dir" || got[2].Mode != "0755" {
		t.Fatalf("zip entries = %+v", got)
	}
	if !manifest.IsSHA256(got[2].SHA256) {
		t.Errorf("sha256 = %q", got[2].SHA256)
	}
}

func TestInspectRejectsMaliciousTar(t *testing.T) {
	big := strings.Repeat("x", 2048)
	cases := []struct {
		name    string
		entries []tarEntry
		lim     InspectLimits
		want    string
	}{
		{"dot dot", []tarEntry{{name: "../evil", body: "x"}}, InspectLimits{}, `".." segment`},
		{"nested dot dot", []tarEntry{{name: "a/../../evil", body: "x"}}, InspectLimits{}, `".." segment`},
		{"absolute", []tarEntry{{name: "/etc/passwd", body: "x"}}, InspectLimits{}, "absolute"},
		{"drive letter", []tarEntry{{name: "C:/x", body: "x"}}, InspectLimits{}, "drive letter"},
		{"backslash", []tarEntry{{name: `a\..\..\evil`, body: "x"}}, InspectLimits{}, "separator"},
		{"symlink", []tarEntry{{name: "link", typ: tar.TypeSymlink, link: "/etc/passwd"}}, InspectLimits{}, "symbolic link"},
		{"hard link", []tarEntry{{name: "a", body: "x"}, {name: "b", typ: tar.TypeLink, link: "a"}}, InspectLimits{}, "hard link"},
		{"char device", []tarEntry{{name: "dev", typ: tar.TypeChar}}, InspectLimits{}, "device"},
		{"fifo", []tarEntry{{name: "fifo", typ: tar.TypeFifo}}, InspectLimits{}, "FIFO"},
		{"setuid", []tarEntry{{name: "su", mode: 0o4755, body: "x"}}, InspectLimits{}, "setuid"},
		{"duplicate", []tarEntry{{name: "a", body: "1"}, {name: "a", body: "2"}}, InspectLimits{}, "more than once"},
		{"duplicate dir", []tarEntry{{name: "d/", typ: tar.TypeDir}, {name: "./d/", typ: tar.TypeDir}}, InspectLimits{}, "more than once"},
		{"case collision", []tarEntry{{name: "README", body: "1"}, {name: "readme", body: "2"}}, InspectLimits{}, "letter case"},
		{"file as dir", []tarEntry{{name: "a", body: "1"}, {name: "a/b", body: "2"}}, InspectLimits{}, "needs it to be a directory"},
		{"too many entries", []tarEntry{{name: "a", body: "1"}, {name: "b", body: "2"}, {name: "c", body: "3"}}, InspectLimits{MaxEntries: 2}, "more than 2 entries"},
		{"file too large", []tarEntry{{name: "big", body: big}}, InspectLimits{MaxFileSize: 1024}, "limit per file"},
		{"total too large", []tarEntry{{name: "a", body: big}, {name: "b", body: big}}, InspectLimits{MaxTotalSize: 3000}, "exceeds 3000 bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := inspectBytes(makeTarGz(t, tc.entries...), "tar.gz", tc.lim)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
			if !errors.Is(err, ErrUnsafeArchive) {
				t.Errorf("err does not wrap ErrUnsafeArchive: %v", err)
			}
		})
	}
}

func TestInspectRejectsMaliciousZip(t *testing.T) {
	cases := []struct {
		name    string
		entries []zipEntry
		lim     InspectLimits
		want    string
	}{
		{"zip slip", []zipEntry{{name: "../../evil", body: "x"}}, InspectLimits{}, `".." segment`},
		{"absolute", []zipEntry{{name: "/abs", body: "x"}}, InspectLimits{}, "absolute"},
		{"backslash", []zipEntry{{name: `..\evil`, body: "x"}}, InspectLimits{}, "separator"},
		{"symlink", []zipEntry{{name: "link", mode: os.ModeSymlink | 0o777, body: "/etc/passwd"}}, InspectLimits{}, "symbolic link"},
		{"setgid", []zipEntry{{name: "x", mode: os.ModeSetgid | 0o755, body: "x"}}, InspectLimits{}, "setuid, setgid"},
		{"duplicate", []zipEntry{{name: "a", body: "1"}, {name: "a", body: "2"}}, InspectLimits{}, "more than once"},
		{"case collision", []zipEntry{{name: "Tool.exe", body: "1"}, {name: "tool.exe", body: "2"}}, InspectLimits{}, "letter case"},
		{"too many entries", []zipEntry{{name: "a"}, {name: "b"}, {name: "c"}}, InspectLimits{MaxEntries: 2}, "more than 2 entries"},
		// 8 MiB of zeros compresses to a few KiB: a small zip bomb.
		{"bomb", []zipEntry{{name: "zeros", body: strings.Repeat("\x00", 8<<20)}}, InspectLimits{MaxFileSize: 1 << 20}, "limit per file"},
		{"bomb total", []zipEntry{{name: "z1", body: strings.Repeat("\x00", 1<<20)}, {name: "z2", body: strings.Repeat("\x00", 1<<20)}}, InspectLimits{MaxTotalSize: 1<<20 + 10}, "exceeds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := inspectBytes(makeZip(t, tc.entries...), "zip", tc.lim)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
			if !errors.Is(err, ErrUnsafeArchive) {
				t.Errorf("err does not wrap ErrUnsafeArchive: %v", err)
			}
		})
	}
}

func TestInspectRejectsGarbage(t *testing.T) {
	for _, format := range []string{"tar.gz", "zip"} {
		if _, err := inspectBytes([]byte("not an archive at all"), format, InspectLimits{}); !errors.Is(err, ErrUnsafeArchive) {
			t.Errorf("%s: err = %v, want ErrUnsafeArchive", format, err)
		}
	}
	// A truncated tar.gz must fail rather than return a partial listing.
	tgz := makeTarGz(t, tarEntry{name: "a", body: strings.Repeat("abc", 10000)})
	if _, err := inspectBytes(tgz[:len(tgz)/2], "tar.gz", InspectLimits{}); err == nil {
		t.Error("truncated tar.gz accepted")
	}
	if _, err := inspectBytes(tgz, "tar.xz", InspectLimits{}); err == nil || !strings.Contains(err.Error(), "unsupported archive format") {
		t.Errorf("unsupported format: err = %v", err)
	}
}
