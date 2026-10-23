package archive

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"
	"unicode/utf16"
)

// unicodePathExtra builds an Info-ZIP Unicode Path (0x7075) extra field.
func unicodePathExtra(headerName, unicodeName string) []byte {
	le := binary.LittleEndian
	d := []byte{1}
	d = le.AppendUint32(d, crc32.ChecksumIEEE([]byte(headerName)))
	d = append(d, unicodeName...)
	b := le.AppendUint16(nil, zipUnicodePathID)
	b = le.AppendUint16(b, uint16(len(d)))
	return append(b, d...)
}

// N4: an Info-ZIP Unicode Path extra field naming another file is used by
// bsdtar and 7-Zip instead of the header name (secreview p12).
func TestInspectZipUnicodePath(t *testing.T) {
	const name = "bin/tool"
	evil := unicodePathExtra(name, "bin/.git/config")
	same := unicodePathExtra(name, name)
	badCRC := unicodePathExtra("other", name)
	badVersion := append([]byte{}, same...)
	badVersion[4] = 2
	other := []byte{0x75, 0x63, 5, 0, 1, 0, 0, 0, 0} // 0x6375 Unicode Comment
	for desc, e := range map[string]rawZipEntry{
		"central path":       {name: name, body: "x", centralExtra: evil},
		"local path":         {name: name, body: "x", localExtra: evil},
		"both paths":         {name: name, body: "x", localExtra: evil, centralExtra: evil},
		"wrong crc":          {name: name, body: "x", centralExtra: badCRC},
		"wrong version":      {name: name, body: "x", localExtra: badVersion},
		"duplicate":          {name: name, body: "x", centralExtra: append(append([]byte{}, same...), same...)},
		"truncated":          {name: name, body: "x", centralExtra: same[:len(same)-2], localExtra: nil},
		"local differs only": {name: name, body: "x", localExtra: unicodePathExtra(name, "bin/tool2"), centralExtra: same},
	} {
		if desc == "truncated" {
			// Declared length runs past the extra block.
			e.centralExtra = append([]byte{}, same...)
			binary.LittleEndian.PutUint16(e.centralExtra[2:], uint16(len(same)))
		}
		wantUnsafe(t, desc, rawZip([]rawZipEntry{e}, rawZipOptions{}), "zip", "Unicode Path")
	}
	for desc, e := range map[string]rawZipEntry{
		"matching":         {name: name, body: "x", localExtra: same, centralExtra: same},
		"comment field":    {name: name, body: "x", centralExtra: other},
		"zipalign-style":   {name: name, body: "x", localExtra: []byte{0, 0, 0}},
		"matching + zip64": {name: name, body: "x", zip64: true, localExtra: same, centralExtra: same},
	} {
		wantSafe(t, desc, rawZip([]rawZipEntry{e}, rawZipOptions{zip64End: e.zip64}), "zip")
	}
}

// N15: the declared entry count (zip64 included) is checked against
// MaxEntries before anything is allocated for it.
func TestInspectZipDeclaredCountLimit(t *testing.T) {
	z := rawZip([]rawZipEntry{{name: "a", body: "1"}}, rawZipOptions{zip64End: true})
	// Patch the zip64 end record's counts to 1 + 65536*1000: archive/zip
	// compares only the low 16 bits.
	end64 := len(z) - 22 - 20 - 56
	if binary.LittleEndian.Uint32(z[end64:]) != zipSigEnd64 {
		t.Fatal("fixture layout changed")
	}
	n := uint64(1 + 65536*1000)
	binary.LittleEndian.PutUint64(z[end64+24:], n)
	binary.LittleEndian.PutUint64(z[end64+32:], n)
	if _, _, err := readZipDirectory(bytes.NewReader(z), int64(len(z)), 64); err == nil || !bytes.Contains([]byte(err.Error()), []byte("more than the limit")) {
		t.Errorf("err = %v", err)
	}
	wantUnsafe(t, "inspect", z, "zip", "")
	allocs := testing.AllocsPerRun(5, func() { _, _, _ = readZipDirectory(bytes.NewReader(z), int64(len(z)), 64) })
	if allocs > 20 {
		t.Errorf("allocations = %v", allocs)
	}
}

func utf16Bytes(s string, bigEndian bool) []byte {
	var out []byte
	for _, u := range utf16.Encode([]rune(s)) {
		if bigEndian {
			out = binary.BigEndian.AppendUint16(out, u)
		} else {
			out = binary.LittleEndian.AppendUint16(out, u)
		}
	}
	return out
}

// N9: a private key saved as UTF-16 (secreview p14).
func TestKeyScannerUTF16(t *testing.T) {
	const key = "-----BEGIN RSA PRIVATE KEY-----\r\nMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gunVTLw\r\n-----END RSA PRIVATE KEY-----\r\n"
	for _, be := range []bool{false, true} {
		for _, prefix := range [][]byte{nil, {0xff, 0xfe}, {0xfe, 0xff}, {'x'}} {
			k := &keyScanner{}
			_, _ = k.Write(append(append([]byte{}, prefix...), utf16Bytes(key, be)...))
			if k.found == "" {
				t.Errorf("big endian %v, prefix %x: not found", be, prefix)
			}
		}
		// The header alone (as .NET string literals hold it) is not a key.
		k := &keyScanner{}
		_, _ = k.Write(utf16Bytes("load -----BEGIN RSA PRIVATE KEY----- blocks", be))
		if k.found != "" {
			t.Errorf("big endian %v: header only flagged", be)
		}
	}
	root := fixture(t, map[string]string{"docs/readme.txt": string(utf16Bytes(key, false))})
	runErr(t, loadConfig(t, root, header+"    files: [docs/readme.txt]\n"), "UTF-16 encoded private key")
}
