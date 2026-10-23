package archive

import (
	"bytes"
	"io"
	"regexp"
	"strings"

	"github.com/magicdrive/hephaestus/internal/glob"
)

// sensitiveNames are base-name patterns of files that usually hold
// credentials. Matching files are refused unless the entry sets
// allow_sensitive: true.
var sensitiveNames = compileAll(
	".env", ".env.*", "*.env",
	".netrc", "_netrc", ".npmrc", ".pypirc", ".git-credentials", ".htpasswd",
	"*.pem", "*.key", "*.p12", "*.pfx", "*.jks", "*.keystore", "*.kdbx", "*.ovpn",
	"id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", "id_ecdsa_sk", "id_ed25519_sk",
	"secring.*", "credentials", "credentials.json", "service-account*.json",
	"*.tfstate", "*.tfstate.backup", "*.tfvars",
)

// sensitiveDirs are directory names whose contents are always refused.
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

// sensitivePath returns why a project-relative path looks sensitive, or "".
func sensitivePath(rel string) string {
	segs := strings.Split(rel, "/")
	for _, s := range segs[:len(segs)-1] {
		if sensitiveDirs[s] {
			return "it is inside a " + s + " directory"
		}
	}
	base := segs[len(segs)-1]
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

// privateKeyRe finds a PEM private key block with real key material, not just
// the header string that TLS libraries embed.
var privateKeyRe = regexp.MustCompile(`-----BEGIN ([A-Z0-9]+ )*PRIVATE KEY-----\r?\n(Proc-Type:[^\n]*\n|DEK-Info:[^\n]*\n|\r?\n)*[A-Za-z0-9+/=]{40,}`)

// keyScanner looks for embedded private keys while content streams through
// it. It keeps a small overlap between writes so a key split across buffer
// boundaries is still found.
type keyScanner struct {
	tail  []byte
	found bool
}

const scanOverlap = 4096

func (k *keyScanner) Write(p []byte) (int, error) {
	if k.found {
		return len(p), nil
	}
	buf := append(k.tail, p...)
	if bytes.Contains(buf, []byte("PRIVATE KEY-----")) && privateKeyRe.Match(buf) {
		k.found = true
	}
	if len(buf) > scanOverlap {
		buf = buf[len(buf)-scanOverlap:]
	}
	k.tail = append(k.tail[:0], buf...)
	return len(p), nil
}

var _ io.Writer = (*keyScanner)(nil)
