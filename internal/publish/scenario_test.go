package publish

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/preflight"
)

// otherConfig is a project other than Hephaestus, shaped like the ones
// released with `hephaestus publish`: another name and repository, five
// platforms (no windows/arm64), no completions.
const otherConfig = `
version: 1
project: {name: kjak, description: d, homepage: "https://example.com", license: MIT}
builds:
  - id: kjak
    matrix:
      os: [darwin, linux, windows]
      arch: [amd64, arm64]
      exclude: [{os: windows, arch: arm64}]
    command: [go, build, -o, "dist/bin/{{ .OS }}_{{ .Arch }}/kjak{{ .Ext }}", ./main.go]
    output: "dist/bin/{{ .OS }}_{{ .Arch }}/kjak{{ .Ext }}"
archives:
  - id: kjak
    name: "{{ .Project }}_{{ .Version }}_{{ .OS }}_{{ .Arch }}"
    format: tar.gz
    format_overrides: [{os: windows, format: zip}]
checksum: {name: "{{ .Project }}_checksums.txt"}
release:
  github: {owner: magicdrive, name: kjak, prerelease: auto, generate_notes: true}
brews:
  - name: kjak
    skip_upload: auto
    repository: {owner: magicdrive, name: homebrew-tap, pull_request: {enabled: true}}
`

func loadProject(t *testing.T, yaml string) *Project {
	t.Helper()
	cfg, err := config.Load(strings.NewReader(yaml), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProject(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// otherHarness is a harness for a clone of magicdrive/<name>.
func otherHarness(t *testing.T, name string) *harness {
	h := newHarness(t)
	h.git.origin = "https://github.com/magicdrive/" + name + ".git"
	h.gh.repo = "magicdrive/" + name
	return h
}

func runProject(t *testing.T, h *harness, p *Project, tag string, opts Options) (*Outcome, *Plan) {
	t.Helper()
	pl := testPlan(t, p, tag)
	h.git.onPush = h.gh.startRelease(pl, h.d.Now)
	h.gh.runPolls = 2
	return Publish(context.Background(), h.d, p, pl, opts), pl
}

// TestOtherProject: publish releases a project that is not Hephaestus. The
// assets, attestations, formula and pull request come from its own
// configuration (five archives, not Hephaestus's six).
func TestOtherProject(t *testing.T) {
	for _, tag := range []string{"v0.2.0-rc.1", "v0.2.0"} {
		t.Run(tag, func(t *testing.T) {
			h := otherHarness(t, "kjak")
			p := loadProject(t, otherConfig)
			o, pl := runProject(t, h, p, tag, fastOpts)
			if o.State != Completed {
				t.Fatalf("outcome %+v\n%s", o, h.out)
			}
			if len(pl.Archives) != 5 || pl.Checksum != "kjak_checksums.txt" || slices.ContainsFunc(pl.Archives, func(a string) bool { return strings.Contains(a, "windows_arm64") }) {
				t.Errorf("plan %v %s", pl.Archives, pl.Checksum)
			}
			out := h.out.String()
			for _, want := range []string{"Repository:     magicdrive/kjak", "Assets:         6 (kjak_0.2.0", "6 assets: kjak_0.2.0", "6 verified (signer magicdrive/kjak/.github/workflows/release.yml"} {
				if !strings.Contains(out, want) {
					t.Errorf("output lacks %q\n%s", want, out)
				}
			}
			if strings.Contains(strings.ToLower(out), "hephaestus_") {
				t.Errorf("Hephaestus's asset names in the output\n%s", out)
			}
			if pl.Prerelease != strings.Contains(tag, "-") || pl.TapSkipped() != pl.Prerelease {
				t.Errorf("prerelease %t, tap skipped %t", pl.Prerelease, pl.TapSkipped())
			}
			if !pl.Prerelease && !strings.Contains(out, "[ok  ] tap formula") {
				t.Errorf("formula not verified\n%s", out)
			}
		})
	}
}

// TestProjectWithoutFormula: without brews there is no tap to verify; tap
// steps may be absent from the release job, and must not have run when
// present.
func TestProjectWithoutFormula(t *testing.T) {
	cfg := otherConfig[:strings.Index(otherConfig, "brews:")]
	t.Run("no tap steps", func(t *testing.T) {
		h := otherHarness(t, "kjak")
		h.gh.noTapSteps, h.gh.secrets = true, nil
		o, _ := runProject(t, h, loadProject(t, cfg), "v0.2.0", fastOpts)
		if o.State != Completed || !strings.Contains(h.out.String(), "Homebrew Tap:   none configured") || !strings.Contains(h.out.String(), "no formula configured and no tap step") {
			t.Fatalf("outcome %+v\n%s", o, h.out)
		}
	})
	t.Run("skipped tap steps", func(t *testing.T) {
		h := otherHarness(t, "kjak")
		o, _ := runProject(t, h, loadProject(t, cfg), "v0.2.0", fastOpts)
		if o.State != Completed || !strings.Contains(h.out.String(), "tap token was not used") {
			t.Fatalf("outcome %+v\n%s", o, h.out)
		}
	})
	t.Run("a tap step ran", func(t *testing.T) {
		h := otherHarness(t, "kjak")
		p := loadProject(t, cfg)
		pl := testPlan(t, p, "v0.2.0")
		start := h.gh.startRelease(pl, h.d.Now)
		h.git.onPush = func(tag string) { start(tag); h.gh.jobs[0].Steps[2].Conclusion = "success" }
		h.gh.runPolls = 1
		if o := Publish(context.Background(), h.d, p, pl, fastOpts); o.State != Failed || !strings.Contains(h.out.String(), "[FAIL] tap steps") {
			t.Fatalf("outcome %+v\n%s", o, h.out)
		}
	})
}

// TestBrewsFromRejected: brews_from would hide the formula from publish,
// which could then not verify the tap.
func TestBrewsFromRejected(t *testing.T) {
	cfg, err := config.Load(strings.NewReader(otherConfig), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.BrewsFrom = ".goreleaser.yaml"
	if _, err := NewProject(cfg); err == nil || !strings.Contains(err.Error(), "brews_from") {
		t.Fatalf("brews_from accepted: %v", err)
	}
}

// TestPublishScenarios names the release scenarios publish must stop
// before anything is tagged (the others: TestPublishRefusals).
func TestPublishScenarios(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *harness)
		want  string
	}{
		{"remote divergence: origin/main ahead", func(h *harness) { h.git.originMain = otherSHA }, "[FAIL] in sync with origin"},
		{"remote divergence: local main ahead", func(h *harness) { h.git.head = otherSHA }, "unpushed or missing commits"},
		{"fetch of origin/main fails", func(h *harness) { h.git.fetchErr = errors.New("could not read from remote") }, "[FAIL] fetch origin/main"},
		{"missing token: gh not logged in", func(h *harness) {
			h.gh.authErr = &CmdError{Cmd: "gh auth status", Code: 1, Stderr: "You are not logged into any GitHub hosts. To log in, run: gh auth login"}
		}, "[FAIL] gh auth"},
		{"missing token: no gh token for the generic preflight", func(h *harness) {
			h.d.Generic = func(context.Context, string) (*preflight.Report, error) {
				return nil, errors.New("gh auth token: no oauth token found for github.com")
			}
		}, "no oauth token"},
		{"failed CI on the commit", func(h *harness) { h.gh.ciRuns[0].Conclusion = "failure" }, "ci.yml concluded failure"},
		{"main ruleset without ci-gate", func(h *harness) { h.gh.rulesets[0].Rules[2].Parameters.RequiredStatusChecks[0].Context = "test" }, "[FAIL] main ruleset"},
		{"published release exists", func(h *harness) {
			h.gh.releases = []Release{{TagName: "v0.1.0-rc.1", HTMLURL: "https://release"}}
		}, "a published release for v0.1.0-rc.1 already exists"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.setup(h)
			o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts)
			if o.State != Failed || o.FailedAt != NotStarted || o.ExitCode() != 1 {
				t.Fatalf("outcome %+v\n%s", o, h.out)
			}
			if len(h.git.created)+len(h.git.pushed) != 0 {
				t.Errorf("created %v pushed %v", h.git.created, h.git.pushed)
			}
			if !strings.Contains(h.out.String(), tc.want) {
				t.Errorf("want %q in\n%s", tc.want, h.out)
			}
		})
	}
}

// TestHint: the next steps name the command the user ran publish with.
func TestHint(t *testing.T) {
	h := newHarness(t)
	h.git.remoteTags["v0.1.0-rc.1"] = testCommit
	h.d.Hint = func(c string) string {
		if c == "publish" {
			return "hephaestus publish"
		}
		return "hephaestus publish " + c
	}
	o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts)
	all := h.out.String() + strings.Join(o.Next, "\n")
	for _, want := range []string{"see hephaestus publish status v0.1.0-rc.1", "run hephaestus publish v0.1.0-rc.1 again"} {
		if !strings.Contains(all, want) {
			t.Errorf("lacks %q\n%s", want, all)
		}
	}
	if strings.Contains(all, "make publish") {
		t.Errorf("names make targets\n%s", all)
	}
}

// flushRecorder records whether the prompt was flushed before the answer
// was read.
type flushRecorder struct {
	bytes.Buffer
	flushed bool
}

func (f *flushRecorder) Flush() { f.flushed = true }

type answer struct{ out *flushRecorder }

func (a answer) Read(p []byte) (int, error) {
	if !a.out.flushed || !strings.HasSuffix(a.out.String(), "[y/N] ") {
		return 0, errors.New("the prompt was not flushed before reading")
	}
	return copy(p, "y\n"), nil
}

// TestConfirmFlushesPrompt: output held back until a newline (hephaestus's
// redacting writer) is flushed before the prompt waits for an answer.
func TestConfirmFlushesPrompt(t *testing.T) {
	h := newHarness(t)
	out := &flushRecorder{}
	h.d.Out, h.d.In = out, answer{out}
	if o, _ := publishRun(t, h, "v0.1.0-rc.1", fastOpts); o.State != Completed {
		t.Fatalf("outcome %+v\n%s", o, out.String())
	}
}
