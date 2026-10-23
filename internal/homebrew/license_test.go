package homebrew

import (
	"strings"
	"testing"
)

func TestLicenseDSL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"MIT", `"MIT"`},
		{"Apache-2.0", `"Apache-2.0"`},
		{"GPL-2.0-or-later", `"GPL-2.0-or-later"`},
		{"LicenseRef-Proprietary", `"LicenseRef-Proprietary"`},
		{"MIT OR Apache-2.0", `any_of: ["MIT", "Apache-2.0"]`},
		{"MIT or Apache-2.0", `any_of: ["MIT", "Apache-2.0"]`},
		{"MIT AND BSD-3-Clause", `all_of: ["MIT", "BSD-3-Clause"]`},
		{"(MIT OR Apache-2.0) OR 0BSD", `any_of: ["MIT", "Apache-2.0", "0BSD"]`},
		{"MIT OR (Apache-2.0 AND BSD-2-Clause)", `any_of: ["MIT", { all_of: ["Apache-2.0", "BSD-2-Clause"] }]`},
		{"MIT AND Apache-2.0 OR 0BSD", `any_of: [{ all_of: ["MIT", "Apache-2.0"] }, "0BSD"]`},
		{"Apache-2.0 WITH LLVM-exception", `"Apache-2.0" => { with: "LLVM-exception" }`},
		{"MIT OR Apache-2.0 WITH LLVM-exception", `any_of: ["MIT", { "Apache-2.0" => { with: "LLVM-exception" } }]`},
	}
	for _, tt := range tests {
		got, err := licenseDSL(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("licenseDSL(%q) = %s, %v; want %s", tt.in, got, err, tt.want)
		}
	}
	for _, bad := range []string{"", " ", "MIT OR", "OR MIT", "(MIT", "MIT)", "MIT AND AND BSD", `MIT"`, "MIT; rm", "MIT\n", "#{x}", "MIT WITH", "MIT WITH OR", "a b"} {
		if got, err := licenseDSL(bad); err == nil {
			t.Errorf("licenseDSL(%q) = %s, want error", bad, got)
		}
	}
}

func TestLicenseRubySyntax(t *testing.T) {
	b := baseBrew()
	b.License = "MIT OR (Apache-2.0 WITH LLVM-exception AND BSD-2-Clause)"
	f := generateOne(t, input(b, fourPlatforms(false)...))
	if !strings.Contains(string(f.Content), `license any_of: ["MIT", { all_of: [{ "Apache-2.0" => { with: "LLVM-exception" } }, "BSD-2-Clause"] }]`) {
		t.Errorf("license line:\n%s", f.Content)
	}
	checkRubySyntax(t, f.Content)
}
