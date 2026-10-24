package version

import "testing"

func TestParse(t *testing.T) {
	i, err := Parse("v1.2.3-rc.1+build.5")
	if err != nil || i.Version != "1.2.3-rc.1+build.5" || i.Major != "1" || i.Patch != "3" || i.Prerelease != "rc.1" || !i.IsPrerelease() {
		t.Errorf("Parse = %+v, %v", i, err)
	}
	i, err = Parse("2026.10")
	if err != nil || i.Version != "2026.10" || i.Major != "" || i.IsPrerelease() {
		t.Errorf("non-semver = %+v, %v", i, err)
	}
	for _, bad := range []string{"", "v1 2", "../v1", "v1/2", "-v1", "v1;rm", "a..b"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}
