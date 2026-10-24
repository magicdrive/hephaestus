package pipeline

import (
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/manifest"
)

func TestParseStages(t *testing.T) {
	got, err := ParseStages("release, archive")
	if err != nil || len(got) != 2 || got[0] != Archive || got[1] != Release {
		t.Errorf("ParseStages = %v, %v", got, err)
	}
	if all, _ := ParseStages("all"); len(all) != len(Order) {
		t.Error("all")
	}
	for _, bad := range []string{"", "deploy", "build,build"} {
		if _, err := ParseStages(bad); err == nil {
			t.Errorf("ParseStages(%q) accepted", bad)
		}
	}
}

func full() Facts {
	return Facts{Manifest: manifest.New("p", "1", "", ""), HasBuilds: true, HasArchives: true, HasGitHubRelease: true,
		HasBrews: true, BrewsHaveTap: true, Tag: "v1", Repo: "o/r", Token: true, TapToken: true}
}

func TestCheckAllStages(t *testing.T) {
	if err := Check(Order, full()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckMissingInputs(t *testing.T) {
	cases := []struct {
		stages []Stage
		mod    func(*Facts)
		want   string
	}{
		{[]Stage{Archive}, func(f *Facts) {}, "no binaries"},
		{[]Stage{Archive, Release}, func(f *Facts) { f.HasBuilds = false }, "no binaries"},
		{[]Stage{Checksum}, func(f *Facts) {}, "no archives"},
		{[]Stage{Release}, func(f *Facts) { f.Token = false }, "token"},
		{[]Stage{Build, Archive, Release}, func(f *Facts) { f.Tag = "" }, "tag is required"},
		{[]Stage{Formula}, func(f *Facts) {}, "no archives"},
		{[]Stage{Tap}, func(f *Facts) {}, "no formulas"},
		{[]Stage{Formula, Tap}, func(f *Facts) { f.TapToken = false }, "GITHUB_TOKEN cannot write"},
		{[]Stage{Build}, func(f *Facts) { f.HasBuilds = false }, "no builds"},
	}
	for _, tc := range cases {
		f := full()
		tc.mod(&f)
		err := Check(tc.stages, f)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.stages, err, tc.want)
		}
	}
}

func TestCheckUsesManifest(t *testing.T) {
	f := full()
	f.Manifest.Put(manifest.Artifact{Kind: manifest.KindBinary, ID: "x", Name: "p", Path: "p", OS: "linux", Arch: "amd64", Source: manifest.Source{Stage: "build"}})
	if err := Check([]Stage{Archive}, f); err != nil {
		t.Errorf("imported binaries not considered: %v", err)
	}
	f.Token = false
	f.DryRun = true
	if err := Check([]Stage{Archive, Release}, f); err != nil {
		t.Errorf("dry run should not need a token: %v", err)
	}
}

func TestCheckBuildThatImportsArchives(t *testing.T) {
	f := full()
	if err := Check([]Stage{Build, Formula}, f); err == nil {
		t.Fatal("formula without archives accepted")
	}
	f.BuildsArchives = true
	if err := Check([]Stage{Build, Formula}, f); err != nil {
		t.Errorf("archives imported by the build were not counted: %v", err)
	}
}
