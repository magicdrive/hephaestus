package archive

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/magicdrive/hephaestus/internal/glob"
)

// The sensitive-file checks are a best-effort safety net against publishing
// credentials by accident. They are not a security boundary: a determined
// author can always package a secret under an innocuous name and encoding.

// sensitiveNames are base-name patterns of files that usually hold
// credentials, written in lower case: names are compared case-insensitively
// because macOS and Windows treat ".Env" and ".env" as the same file.
// Matching files are refused unless the entry sets allow_sensitive: true.
var sensitiveNames = compileAll(
	".env", ".env.*", "*.env", ".envrc",
	".netrc", "_netrc", ".npmrc", ".pypirc", ".git-credentials", ".htpasswd", ".pgpass",
	".vault-token", ".dockercfg", "kubeconfig",
	"*.pem", "*.key", "*.p12", "*.pfx", "*.p8", "*.ppk", "*.jks", "*.keystore", "*.kdbx", "*.ovpn",
	"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "id_ecdsa_sk", "id_ed25519_sk",
	"secring.*", "credentials", "credentials.json", "service-account*.json",
	"*.tfstate", "*.tfstate.backup", "*.tfvars",
)

// sensitiveDirs are directory names (lower case) whose contents are always
// refused.
var sensitiveDirs = map[string]bool{
	".git": true, ".ssh": true, ".aws": true, ".gnupg": true, ".docker": true, ".kube": true, ".azure": true, ".config": true,
}

func compileAll(pats ...string) []*glob.Pattern {
	out := make([]*glob.Pattern, len(pats))
	for i, p := range pats {
		out[i] = glob.MustCompile(p, glob.Options{Dot: true})
	}
	return out
}

// sensitiveDir returns why a directory name is sensitive, or "".
func sensitiveDir(name string) string {
	if sensitiveDirs[strings.ToLower(name)] {
		return "it is inside a " + name + " directory"
	}
	return ""
}

// sensitivePath returns why a slash-separated relative file path looks
// sensitive, or "". Directory and file names are compared ignoring case.
func sensitivePath(rel string) string {
	segs := strings.Split(rel, "/")
	for _, s := range segs[:len(segs)-1] {
		if why := sensitiveDir(s); why != "" {
			return why
		}
	}
	base := strings.ToLower(segs[len(segs)-1])
	if strings.HasSuffix(base, ".pub") {
		return "" // public keys are fine
	}
	for _, p := range sensitiveNames {
		if p.Match(base) {
			return "its name matches " + p.String()
		}
	}
	return ""
}

// SensitiveEntry returns why an archive member path looks like it holds
// credentials (see the archives documentation), or "" when it does not.
// isDir reports whether the member is a directory, in which case every
// segment is checked as a directory name. Imported archives use it to apply
// the same rules as archives Hephaestus builds.
func SensitiveEntry(path string, isDir bool) string {
	if isDir {
		for _, s := range strings.Split(path, "/") {
			if why := sensitiveDir(s); why != "" {
				return why
			}
		}
		return ""
	}
	return sensitivePath(path)
}

// maxLinkHops bounds symbolic link resolution, like the kernel's ELOOP.
const maxLinkHops = 40

// errLinkEscapes reports a link whose target leaves the root.
var errLinkEscapes = errors.New("symbolic link resolves outside the project root")

// resolveInRoot resolves every symbolic link in the slash-separated path
// rel (in any component, not only the last) without leaving root. It
// returns the resolved root-relative path, every directory name the
// resolution passed through (including ones a later ".." left again), and
// whether any component was a link. linked is accurate even when err is not
// nil, so callers rejecting links can do so before reporting other errors.
func resolveInRoot(root *os.Root, rel string) (resolved string, dirs []string, linked bool, err error) {
	pending := strings.Split(rel, "/")
	var done []string
	hops := 0
	for len(pending) > 0 {
		seg := pending[0]
		pending = pending[1:]
		switch seg {
		case "", ".":
			continue
		case "..":
			if len(done) == 0 {
				return "", dirs, linked, errLinkEscapes
			}
			done = done[:len(done)-1]
			continue
		}
		cur := strings.Join(append(done[:len(done):len(done)], seg), "/")
		fi, err := root.Lstat(cur)
		if err != nil {
			return "", dirs, linked, err
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			if len(pending) > 0 {
				dirs = append(dirs, seg)
			}
			done = append(done, seg)
			continue
		}
		linked = true
		if hops++; hops > maxLinkHops {
			return "", dirs, linked, fmt.Errorf("%s: too many levels of symbolic links", cur)
		}
		target, err := root.Readlink(cur)
		if err != nil {
			return "", dirs, linked, err
		}
		target = filepath.ToSlash(target)
		if strings.HasPrefix(target, "/") || filepath.IsAbs(target) || filepath.VolumeName(target) != "" {
			return "", dirs, linked, errLinkEscapes
		}
		pending = append(strings.Split(target, "/"), pending...)
	}
	if len(done) == 0 {
		return "", dirs, linked, errLinkEscapes
	}
	return strings.Join(done, "/"), dirs, linked, nil
}

// nl matches a line break, either real or escaped inside a JSON or similar
// string ("\n", "\r\n" written as backslash sequences).
const nl = `(?:\r?\n|\\r\\n|\\n)`

// privateKeyRe finds a PEM private key block with real key material, not just
// the header string that TLS libraries embed. Line breaks may be escaped,
// as in a GCP service account JSON file.
var privateKeyRe = regexp.MustCompile(`-----BEGIN ([A-Z0-9]+ )*PRIVATE KEY-----` + nl +
	`(?:(?:Proc-Type|DEK-Info):[^\n\\]*` + nl + `|` + nl + `)*[A-Za-z0-9+/=]{40,}`)

// pgpPrivateKeyRe finds an ASCII-armored OpenPGP secret key with key
// material after the armor headers.
var pgpPrivateKeyRe = regexp.MustCompile(`-----BEGIN PGP PRIVATE KEY BLOCK-----` + nl +
	`(?:[A-Za-z][A-Za-z-]*: [^\n\\]*` + nl + `)*` + nl + `[A-Za-z0-9+/=]{40,}`)

// serviceAccountRe and privateKeyFieldRe together identify a cloud service
// account key file even when the key itself is too short or oddly encoded
// for privateKeyRe.
var (
	serviceAccountRe  = regexp.MustCompile(`"type"\s*:\s*"service_account"`)
	privateKeyFieldRe = regexp.MustCompile(`"private_key"\s*:\s*"`)
)

// keyScanner looks for embedded private keys while content streams through
// it. It keeps a small overlap between writes so a key split across buffer
// boundaries is still found.
type keyScanner struct {
	tail []byte
	// found describes what was found ("" while nothing was).
	found          string
	serviceAccount bool
	keyField       bool
}

const scanOverlap = 4096

func (k *keyScanner) Write(p []byte) (int, error) {
	if k.found != "" {
		return len(p), nil
	}
	buf := append(k.tail, p...)
	switch {
	case bytes.Contains(buf, []byte("PRIVATE KEY")) && privateKeyRe.Match(buf):
		k.found = "a PEM private key"
	case bytes.Contains(buf, []byte("PGP PRIVATE KEY BLOCK")) && pgpPrivateKeyRe.Match(buf):
		k.found = "a PGP private key"
	}
	if !k.serviceAccount && bytes.Contains(buf, []byte("service_account")) {
		k.serviceAccount = serviceAccountRe.Match(buf)
	}
	if !k.keyField && bytes.Contains(buf, []byte("private_key")) {
		k.keyField = privateKeyFieldRe.Match(buf)
	}
	if k.found == "" && k.serviceAccount && k.keyField {
		k.found = "a service account private key"
	}
	if len(buf) > scanOverlap {
		buf = buf[len(buf)-scanOverlap:]
	}
	k.tail = append(k.tail[:0], buf...)
	return len(p), nil
}

var _ io.Writer = (*keyScanner)(nil)
