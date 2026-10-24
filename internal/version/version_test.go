package version

import "testing"

func TestParse(t *testing.T) {
	i, err := Parse("v1.2.3-rc.1+build.5")
	if err != nil || i.Version != "1.2.3-rc.1+build.5" || i.Major != "1" || i.Patch != "3" || i.Prerelease != "rc.1" || !i.IsPrerelease() {
		t.Errorf("Parse = %+v, %v", i, err)
	}
	if i.Build != "build.5" || !i.IsSemver() {
		t.Errorf("build metadata = %+v", i)
	}
	i, err = Parse("2026.10")
	if err != nil || i.Version != "2026.10" || i.Major != "" || i.IsPrerelease() {
		t.Errorf("non-semver = %+v, %v", i, err)
	}
	// Malformed pre-release or build parts are not SemVer, so the tag is
	// not a prerelease anywhere (release stage, skip_upload: auto).
	for _, tag := range []string{"v1.0.0-rc.", "v1.0.0-", "v1.0.0-.rc", "v1.0.0+", "v01.0.0-rc.1", "v1.0-rc.1"} {
		if i, err := Parse(tag); err != nil || i.IsSemver() || i.IsPrerelease() {
			t.Errorf("Parse(%q) = %+v, %v; want a non-SemVer tag", tag, i, err)
		}
	}
	for _, bad := range []string{"", "v1 2", "../v1", "v1/2", "-v1", "v1;rm", "a..b"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}
