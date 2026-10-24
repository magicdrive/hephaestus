package archive

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const gcpKey = `{"type":"service_account","project_id":"p","private_key_id":"0","private_key":"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQC7VJTUt9Us8cKjMzEfYyjiWA4R4\n-----END PRIVATE KEY-----\n","client_email":"x@p.iam.gserviceaccount.com"}`

const pgpKey = "-----BEGIN PGP PRIVATE KEY BLOCK-----\nVersion: GnuPG v2\nComment: backup\n\nlQOYBF8AAAABCADGdJ3QmA0b3t8Q8kW0V3gmkZfQ7O0eE2vZ9uYc8R9ZfY4sQ3Zb\n=abcd\n-----END PGP PRIVATE KEY BLOCK-----\n"

func skipNoSymlinks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
}

// F4: case variants of sensitive names are refused, since macOS and Windows
// open ".Env" as ".env".
func TestSensitiveNamesIgnoreCase(t *testing.T) {
	root := fixture(t, map[string]string{
		".Env":          "TOKEN=x",
		"keys/ID_RSA":   "k",
		"certs/TLS.PEM": "p",
		".GIT/config":   "c",
		"a/.SSH/x":      "x",
		".envrc":        "export X=1",
		"k/AuthKey.p8":  "k",
		"putty.PPK":     "k",
		"ok/readme.txt": "fine",
	})
	for src, want := range map[string]string{
		".Env":          "matches .env",
		"keys/ID_RSA":   "matches id_rsa",
		"certs/TLS.PEM": "matches *.pem",
		".GIT/config":   "inside a .GIT directory",
		"a/.SSH/x":      "inside a .SSH directory",
		".envrc":        "matches .envrc",
		"k/AuthKey.p8":  "matches *.p8",
		"putty.PPK":     "matches *.ppk",
	} {
		runErr(t, loadConfig(t, root, header+"    files: ["+src+"]\n"), want)
	}
	runOK(t, loadConfig(t, root, header+"    files: [ok/readme.txt, {src: .Env, allow_sensitive: true}]\n"))
}

// F4: an innocuous name linking to a sensitive file, a link to a sensitive
// directory, and a glob through a linked .git are all refused.
func TestSensitiveThroughLinks(t *testing.T) {
	skipNoSymlinks(t)
	root := fixture(t, map[string]string{
		".env":              "TOKEN=x",
		".git/config":       "[http]\n\textraheader = AUTHORIZATION: basic xyz\n",
		"docs/notes.txt":    "->../.env",
		"docs/index.md":     "ok",
		"share":             "->.git",
		"deep/a":            "->../.git",
		"hop/one":           "->two",
		"hop/two":           "->../.env",
		"chain/x/README.md": "->../../.git/../docs/index.md",
	})
	runErr(t, loadConfig(t, root, header+"    files: [docs/notes.txt]\n"), `resolves to ".env"`)
	runErr(t, loadConfig(t, root, header+"    files: [\"docs/**\"]\n"), `resolves to ".env"`)
	runErr(t, loadConfig(t, root, header+"    files: [\"share/**\"]\n"), "inside a .git directory")
	runErr(t, loadConfig(t, root, header+"    files: [share/config]\n"), "inside a .git directory")
	runErr(t, loadConfig(t, root, header+"    files: [deep/a/config]\n"), "inside a .git directory")
	runErr(t, loadConfig(t, root, header+"    files: [hop/one]\n"), "matches .env")
	// Every directory passed through counts, even one a ".." leaves again.
	runErr(t, loadConfig(t, root, header+"    files: [chain/x/README.md]\n"), "inside a .git directory")
	// allow_sensitive still opts out.
	runOK(t, loadConfig(t, root, header+"    files: [{src: docs/notes.txt, allow_sensitive: true}]\n"))
	// A link to an ordinary file is still followed.
	root2 := fixture(t, map[string]string{"real/readme.txt": "r", "alias": "->real"})
	runOK(t, loadConfig(t, root2, header+"    files: [alias/readme.txt]\n"))
}

// F4: private keys hidden from the PEM line-break check.
func TestSensitiveContent(t *testing.T) {
	root := fixture(t, map[string]string{
		"docs/gcp.json":    gcpKey,
		"docs/backup.asc":  pgpKey,
		"docs/sa.json":     `{"type": "service_account", "private_key": "short"}`,
		"docs/escaped.txt": `key: "-----BEGIN RSA PRIVATE KEY-----\r\nMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gunVTLw\r\n"`,
		"docs/pub.asc":     "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmQENBF8AAAABCADGdJ3QmA0b3t8Q8kW0V3gmkZfQ7O0eE2vZ9uYc8R9ZfY4sQ3Zb\n",
		"docs/mention.txt": "Never commit -----BEGIN PGP PRIVATE KEY BLOCK----- or \"private_key\": \"...\" files.",
		"docs/other.json":  `{"type": "authorized_user", "client_id": "x"}`,
	})
	runErr(t, loadConfig(t, root, header+"    files: [docs/gcp.json]\n"), "PEM private key")
	runErr(t, loadConfig(t, root, header+"    files: [docs/backup.asc]\n"), "PGP private key")
	runErr(t, loadConfig(t, root, header+"    files: [docs/sa.json]\n"), "service account private key")
	runErr(t, loadConfig(t, root, header+"    files: [docs/escaped.txt]\n"), "PEM private key")
	runOK(t, loadConfig(t, root, header+"    files: [docs/pub.asc, docs/mention.txt, docs/other.json]\n"))
	runOK(t, loadConfig(t, root, header+"    files: [{src: docs/gcp.json, allow_sensitive: true}]\n"))
}

// F4: a key split across read buffers is still found.
func TestKeyScannerAcrossWrites(t *testing.T) {
	data := strings.Repeat("x", 40000) + gcpKey
	for _, step := range []int{1, 7, 4096, 32 * 1024} {
		k := &keyScanner{}
		for i := 0; i < len(data); i += step {
			end := min(i+step, len(data))
			_, _ = k.Write([]byte(data[i:end]))
		}
		if k.found == "" {
			t.Errorf("step %d: key not found", step)
		}
	}
}

// F8: with symlinks: reject, a link anywhere on the path is an error, for
// literal sources and for glob bases; follow mode is unchanged.
func TestSymlinksRejectWholePath(t *testing.T) {
	skipNoSymlinks(t)
	root := fixture(t, map[string]string{
		"real/f.txt":   "f",
		"real/sub/g":   "g",
		"linkdir":      "->real",
		"nested/inner": "->../real/sub",
	})
	reject := header + "    symlinks: reject\n    files: "
	runErr(t, loadConfig(t, root, reject+"[linkdir/f.txt]\n"), "symlinks: reject")
	runErr(t, loadConfig(t, root, reject+"[nested/inner/g]\n"), "symlinks: reject")
	runErr(t, loadConfig(t, root, reject+"[\"linkdir/**\"]\n"), "symlinks: reject")
	runErr(t, loadConfig(t, root, reject+"[\"nested/inner/*\"]\n"), "symlinks: reject")
	// An optional glob whose base is a link is still refused, not skipped.
	runErr(t, loadConfig(t, root, reject+"[{src: \"linkdir/*.none\", required: false}]\n"), "symlinks: reject")
	runOK(t, loadConfig(t, root, reject+"[real/f.txt, \"real/sub/*\"]\n"))
	// follow keeps following linked parents of literal paths and linked
	// glob bases.
	runOK(t, loadConfig(t, root, header+"    files: [linkdir/f.txt]\n"))
	cfg := loadConfig(t, root, header+"    files: [\"nested/inner/*\"]\n")
	runOK(t, cfg)
	names, _ := readArchive(t, filepath.Join(root, "dist", "mycli_1.2.3_linux_amd64.tar.gz"))
	if strings.Join(names, ",") != "nested,nested/inner,nested/inner/g" {
		t.Errorf("entries = %v", names)
	}
}

func TestResolveInRoot(t *testing.T) {
	skipNoSymlinks(t)
	outside := t.TempDir()
	root := fixture(t, map[string]string{
		"a/b/c":   "x",
		"l1":      "->a/b",
		"loop1":   "->loop2",
		"loop2":   "->loop1",
		"abs":     "->" + outside,
		"up":      "->../x",
		"dotdot":  "->a/../a/b/c",
		"selfdir": "->.",
	})
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for in, want := range map[string]string{"a/b/c": "a/b/c", "l1/c": "a/b/c", "dotdot": "a/b/c", "selfdir/a/b/c": "a/b/c"} {
		got, _, _, err := resolveInRoot(r, in)
		if err != nil || got != want {
			t.Errorf("resolve(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"loop1", "abs/x", "up", "missing/x", "selfdir"} {
		if got, _, _, err := resolveInRoot(r, in); err == nil {
			t.Errorf("resolve(%q) = %q, want error", in, got)
		}
	}
}

// F7: a dist directory linking out of the project root is an error and
// nothing (not even a temporary file or directory) is created outside.
func TestWriteConfinedToRoot(t *testing.T) {
	skipNoSymlinks(t)
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	root := fixture(t, map[string]string{"README.md": "r"})
	if err := os.Symlink(outside, filepath.Join(root, "dist")); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(t, root, header+"    files: [README.md]\n")
	if _, err := Run(input(cfg)); err == nil {
		t.Fatal("archive written through a dist link that leaves the root")
	}
	// Also with a nested dist whose parent is the link.
	cfg = loadConfig(t, root, "dist: dist/out\n"+header+"    files: [README.md]\n")
	if _, err := Run(input(cfg)); err == nil {
		t.Fatal("archive written below a dist link that leaves the root")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("created outside the root: %v", entries)
	}
}
