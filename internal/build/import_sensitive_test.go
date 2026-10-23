package build

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"strings"
	"testing"
)

// concatTarGz decompresses tar.gz streams and recompresses their raw
// concatenation (each tar keeps its own end marker).
func concatTarGz(t *testing.T, parts ...string) string {
	t.Helper()
	var raw bytes.Buffer
	for _, p := range parts {
		zr, err := gzip.NewReader(strings.NewReader(p))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(&raw, zr); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	zw.Write(raw.Bytes())
	zw.Close()
	return out.String()
}

const testPEM = "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gunVTLw\n-----END RSA PRIVATE KEY-----\n"

const testGCP = `{"type":"service_account","private_key":"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKjMzEfYyjiWA4R4\n-----END PRIVATE KEY-----\n"}`

// F4: imported archives get the same sensitive-name and content checks as
// archives Hephaestus builds, unless the import sets allow_sensitive.
func TestExistingArchiveSensitive(t *testing.T) {
	cases := []struct {
		name, file, want string
		data             func(t *testing.T) string
	}{
		{"dot env", "pkg.tar.gz", "matches .env", func(t *testing.T) string {
			return tarGz(t, member{name: "app/bin", mode: 0o755, body: "ELF"}, member{name: "app/.ENV", mode: 0o644, body: "TOKEN=x"})
		}},
		{"git dir", "pkg.zip", "inside a .git directory", func(t *testing.T) string {
			return zipData(t, member{name: "app/.git/config", body: "[http]"})
		}},
		{"git dir entry", "pkg.tar.gz", "inside a .git directory", func(t *testing.T) string {
			return tarGz(t, member{name: "app/.git/", mode: 0o755})
		}},
		{"ssh key", "pkg.zip", "matches id_rsa", func(t *testing.T) string {
			return zipData(t, member{name: "keys/Id_Rsa", body: "k"})
		}},
		{"pem content", "pkg.tar.gz", "PEM private key", func(t *testing.T) string {
			return tarGz(t, member{name: "docs/notes.txt", mode: 0o644, body: testPEM})
		}},
		{"gcp json", "pkg.zip", "private key", func(t *testing.T) string {
			return zipData(t, member{name: "docs/gcp.json", body: testGCP})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := fixture(t, map[string]string{tc.file: tc.data(t)})
			yml := existingHeader + fmt.Sprintf("      kind: archive\n      path: %q\n    targets: [{os: linux, arch: amd64}]\n", tc.file)
			in, _ := newInput(loadConfig(t, root, yml))
			runErr(t, in, tc.want)
			// The opt-out accepts the same archive.
			yml = strings.Replace(yml, "kind: archive\n", "kind: archive\n      allow_sensitive: true\n", 1)
			in, _ = newInput(loadConfig(t, root, yml))
			if res := run(t, in); len(res.Imported) != 1 {
				t.Errorf("result = %+v", res)
			}
		})
	}
	if _, err := loadConfigErr(existingHeader + "      path: x\n      allow_sensitive: true\n    targets: [{os: linux, arch: amd64}]\n"); err == nil || !strings.Contains(err.Error(), "allow_sensitive") {
		t.Errorf("allow_sensitive on a binary import accepted: %v", err)
	}
}

func TestGoReleaserArchiveSensitive(t *testing.T) {
	tgz := tarGz(t,
		member{name: "app_1.2.3_linux_amd64/app", mode: 0o755, body: "ELF"},
		member{name: "app_1.2.3_linux_amd64/server.key", mode: 0o644, body: "k"},
	)
	root := grFixture(t, map[string]string{"dist/app_1.2.3_linux_amd64.tar.gz": tgz},
		`{"name":"app_1.2.3_linux_amd64.tar.gz","path":"dist/app_1.2.3_linux_amd64.tar.gz","goos":"linux","goarch":"amd64","type":"Archive","extra":{"ID":"app","Binaries":["app"],"WrappedIn":"app_1.2.3_linux_amd64","Format":"tar.gz"}}`)
	in, _ := newInput(loadConfig(t, root, grHeader+"      archives: true\n"))
	runErr(t, in, "matches *.key")
	in, _ = newInput(loadConfig(t, root, grHeader+"      archives: true\n      allow_sensitive: true\n"))
	if res := run(t, in); len(res.Imported) != 1 {
		t.Errorf("result = %+v", res)
	}
	if _, err := loadConfigErr(grHeader + "      allow_sensitive: true\n"); err == nil || !strings.Contains(err.Error(), "allow_sensitive") {
		t.Errorf("allow_sensitive without archives accepted: %v", err)
	}
}

// F5: an imported tar.gz with members hidden after the end marker is
// refused (secreview reproduction p4).
func TestExistingArchiveHiddenTarMembers(t *testing.T) {
	good := tarGz(t, member{name: "bin/demo", mode: 0o755, body: "hello"})
	// Re-compress the good tar followed by a hidden member.
	evil := tarGz(t, member{name: "../../../../tmp/heph-evil.txt", mode: 0o644, body: "pwnd"})
	data := concatTarGz(t, good, evil)
	root := fixture(t, map[string]string{"pkg.tar.gz": data})
	in, _ := newInput(loadConfig(t, root, existingHeader+"      kind: archive\n      path: pkg.tar.gz\n    targets: [{os: linux, arch: amd64}]\n"))
	runErr(t, in, "end-of-archive")
}
