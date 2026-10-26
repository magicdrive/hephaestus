package main

// Repository consistency tests: they keep workflows, the Makefile, the
// action definition and the documentation true to the implementation, so a
// change that breaks one of those promises fails `go test ./...`.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/magicdrive/hephaestus/internal/config"
)

type workflow struct {
	On          yaml.Node               `yaml:"on"`
	Permissions map[string]string       `yaml:"permissions"`
	Env         map[string]string       `yaml:"env"`
	Jobs        map[string]*workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	Needs       yaml.Node         `yaml:"needs"`
	Uses        string            `yaml:"uses"`
	Permissions map[string]string `yaml:"permissions"`
	Environment string            `yaml:"environment"`
	Steps       []struct {
		ID   string            `yaml:"id"`
		Name string            `yaml:"name"`
		If   string            `yaml:"if"`
		Uses string            `yaml:"uses"`
		Run  string            `yaml:"run"`
		Env  map[string]string `yaml:"env"`
	} `yaml:"steps"`
}

func readWorkflow(t *testing.T, path string) *workflow {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var w workflow
	if err := yaml.Unmarshal(data, &w); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return &w
}

func (j *workflowJob) needs() []string {
	var out []string
	switch j.Needs.Kind {
	case yaml.ScalarNode:
		out = append(out, j.Needs.Value)
	case yaml.SequenceNode:
		for _, n := range j.Needs.Content {
			out = append(out, n.Value)
		}
	}
	return out
}

func workflowFiles(t *testing.T) []string {
	files, err := filepath.Glob(".github/workflows/*.yml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows: %v", err)
	}
	return files
}

var pinnedUse = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}$`)

func TestWorkflows_ActionsArePinnedToCommitSHAs(t *testing.T) {
	files := append(workflowFiles(t), "action.yml")
	re := regexp.MustCompile(`(?m)^\s*(?:-\s*)?uses:\s*(\S+)(.*)$`)
	for _, f := range files {
		data, _ := os.ReadFile(f)
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			use, comment := m[1], m[2]
			if strings.HasPrefix(use, "./") {
				continue
			}
			if !pinnedUse.MatchString(use) {
				t.Errorf("%s: %q is not pinned to a commit SHA", f, use)
			}
			if !strings.Contains(comment, "# v") {
				t.Errorf("%s: %q lacks a \"# vX.Y.Z\" comment naming the pinned release", f, use)
			}
		}
	}
}

func TestWorkflows_DefaultPermissionsAreReadOnly(t *testing.T) {
	for _, f := range workflowFiles(t) {
		w := readWorkflow(t, f)
		if len(w.Permissions) != 1 || w.Permissions["contents"] != "read" {
			t.Errorf("%s: top-level permissions = %v, want only contents: read", f, w.Permissions)
		}
	}
}

func TestReleaseWorkflow_PublishesOnlyAfterEveryGate(t *testing.T) {
	ci := readWorkflow(t, ".github/workflows/ci.yml")
	if !strings.Contains(nodeString(&ci.On), "workflow_call") {
		t.Fatal("ci.yml must be reusable (workflow_call) so the release can gate on it")
	}
	gate := ci.Jobs["ci-gate"]
	if gate == nil {
		t.Fatal("ci.yml has no ci-gate job")
	}
	for name := range ci.Jobs {
		if name != "ci-gate" && !slices.Contains(gate.needs(), name) {
			t.Errorf("ci-gate does not wait for job %q", name)
		}
	}

	for _, f := range releaseWorkflows {
		rel := readWorkflow(t, f)
		if j := rel.Jobs["ci"]; j == nil || j.Uses != "./.github/workflows/ci.yml" {
			t.Fatalf("%s must run ci.yml as its ci job", f)
		}
		for name, j := range rel.Jobs {
			if name == "ci" {
				continue
			}
			if !slices.Contains(j.needs(), "ci") {
				t.Errorf("%s: job %q does not need ci", f, name)
			}
			verify, publish := -1, -1
			for i, s := range j.Steps {
				if strings.Contains(s.Run, "verify-dist.sh") && verify < 0 {
					verify = i
				}
				if strings.Contains(s.Run, "--stages release") && publish < 0 {
					publish = i
				}
			}
			if publish >= 0 && (verify < 0 || verify > publish) {
				t.Errorf("%s: job %q publishes before verify-dist.sh", f, name)
			}
		}
	}
}

// TestReleaseWorkflow_CIJobGrantsWhatCINeeds checks that the ci job calling
// ci.yml grants every permission a ci.yml job asks for: GitHub refuses to
// start a workflow whose called jobs request more than the caller allows.
func TestReleaseWorkflow_CIJobGrantsWhatCINeeds(t *testing.T) {
	ci := readWorkflow(t, ".github/workflows/ci.yml")
	for _, f := range releaseWorkflows {
		granted := readWorkflow(t, f).Jobs["ci"].Permissions
		if granted == nil {
			granted = readWorkflow(t, f).Permissions
		}
		for name, j := range ci.Jobs {
			for scope, level := range j.Permissions {
				if level == "write" && granted[scope] != "write" || level == "read" && granted[scope] == "" {
					t.Errorf("%s: ci job grants %s: %q, but ci.yml job %q needs %q", f, scope, granted[scope], name, level)
				}
			}
		}
	}
}

// releaseWorkflows are the workflows that publish (or rehearse publishing).
var releaseWorkflows = []string{".github/workflows/release.yml", ".github/workflows/release-rehearsal.yml"}

// TestReleaseWorkflow_BuildsWithoutWriteTokens keeps the build/publish
// boundary: a job that runs project build commands holds only read access,
// so build code can neither write to the repository nor request an OIDC
// token to sign provenance.
func TestReleaseWorkflow_BuildsWithoutWriteTokens(t *testing.T) {
	for _, f := range releaseWorkflows {
		checkBuildJobsReadOnly(t, f)
	}
}

func checkBuildJobsReadOnly(t *testing.T, f string) {
	t.Helper()
	rel := readWorkflow(t, f)
	builds := 0
	for name, j := range rel.Jobs {
		runsBuild := false
		for _, s := range j.Steps {
			if strings.Contains(s.Run, "--stages build") || strings.Contains(s.Run, "make snapshot") {
				runsBuild = true
			}
		}
		if !runsBuild {
			continue
		}
		builds++
		if len(j.Permissions) != 1 || j.Permissions["contents"] != "read" {
			t.Errorf("%s: job %q runs build commands with permissions %v, want only contents: read", f, name, j.Permissions)
		}
	}
	if builds == 0 {
		t.Errorf("%s: no job runs the build stage", f)
	}
}

// TestRehearsalConfigMatchesRelease keeps hephaestus.rehearsal.yaml equal to
// hephaestus.yaml except for the settings that keep a rehearsal private:
// a draft release and a sandbox tap.
func TestRehearsalConfigMatchesRelease(t *testing.T) {
	load := func(path string) map[string]any {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := yaml.Unmarshal(data, &m); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return m
	}
	rel, reh := load("hephaestus.yaml"), load("hephaestus.rehearsal.yaml")
	gh := func(m map[string]any) map[string]any {
		return m["release"].(map[string]any)["github"].(map[string]any)
	}
	brew := func(m map[string]any) map[string]any {
		return m["brews"].([]any)[0].(map[string]any)
	}
	repo := func(m map[string]any) map[string]any { return brew(m)["repository"].(map[string]any) }

	if gh(reh)["draft"] != true {
		t.Error("rehearsal: release.github.draft must be true")
	}
	if name := repo(reh)["name"]; name == repo(rel)["name"] || !strings.Contains(fmt.Sprint(name), "sandbox") {
		t.Errorf("rehearsal: brews[0].repository.name = %v, want a sandbox tap", name)
	}
	if brew(reh)["skip_upload"] != "false" {
		t.Error(`rehearsal: brews[0].skip_upload must be "false"`)
	}
	delete(gh(reh), "draft")
	delete(brew(reh), "skip_upload")
	delete(brew(rel), "skip_upload")
	repo(reh)["name"] = repo(rel)["name"]
	if !reflect.DeepEqual(rel, reh) {
		a, _ := yaml.Marshal(rel)
		b, _ := yaml.Marshal(reh)
		t.Errorf("hephaestus.rehearsal.yaml drifted from hephaestus.yaml:\n--- release\n%s\n--- rehearsal\n%s", a, b)
	}
}

// TestCompatibilityDocTestsExist keeps docs/compatibility/goreleaser.md
// honest: every test it cites exists, and every golden case it names
// (`TestGolden` (`case`)) has a golden file.
func TestCompatibilityDocTestsExist(t *testing.T) {
	doc, err := os.ReadFile("docs/compatibility/goreleaser.md")
	if err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	funcRe := regexp.MustCompile(`(?m)^func ((?:Test|Fuzz)[A-Za-z0-9_]+)\(`)
	err = filepath.WalkDir(".", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, "_test.go") {
			return err
		}
		data, err := os.ReadFile(p)
		for _, m := range funcRe.FindAllSubmatch(data, -1) {
			defined[string(m[1])] = true
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	cited := regexp.MustCompile("`((?:Test|Fuzz)[A-Za-z0-9_]+)`").FindAllSubmatch(doc, -1)
	if len(cited) < 20 {
		t.Fatalf("only %d tests cited; is the table still there?", len(cited))
	}
	for _, m := range cited {
		if !defined[string(m[1])] {
			t.Errorf("docs/compatibility/goreleaser.md cites %s, which does not exist", m[1])
		}
	}
	for _, m := range regexp.MustCompile("`TestGolden` \\(([^)]*)\\)").FindAllSubmatch(doc, -1) {
		for _, c := range regexp.MustCompile("`([a-z0-9_]+)`").FindAllSubmatch(m[1], -1) {
			if _, err := os.Stat(filepath.Join("internal/homebrew/testdata", string(c[1])+".golden")); err != nil {
				t.Errorf("docs/compatibility/goreleaser.md cites golden case %q: %v", c[1], err)
			}
		}
	}
}

func nodeString(n *yaml.Node) string {
	out, _ := yaml.Marshal(n)
	return string(out)
}

func TestMakefile_ToolVersionsMatchCI(t *testing.T) {
	mk, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	ci := readWorkflow(t, ".github/workflows/ci.yml")
	for _, name := range []string{"GOLANGCI_LINT_VERSION", "GOVULNCHECK_VERSION"} {
		m := regexp.MustCompile(`(?m)^` + name + `\s*=\s*(\S+)`).FindSubmatch(mk)
		if m == nil {
			t.Errorf("Makefile does not set %s", name)
			continue
		}
		if got := ci.Env[name]; got != string(m[1]) {
			t.Errorf("%s: Makefile %s, ci.yml %s", name, m[1], got)
		}
	}
}

func TestActionInputsAreDocumented(t *testing.T) {
	var action struct {
		Inputs  map[string]any `yaml:"inputs"`
		Outputs map[string]any `yaml:"outputs"`
	}
	data, err := os.ReadFile("action.yml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &action); err != nil {
		t.Fatal(err)
	}
	readme := readFileOrSkip(t, "README.md")
	ref := readme + readFileOrSkip(t, "docs/github-action.md")
	for name := range action.Inputs {
		if !strings.Contains(ref, "`"+name+"`") {
			t.Errorf("action input %q is not documented in README.md or docs/github-action.md", name)
		}
	}
	for name := range action.Outputs {
		if !strings.Contains(ref, "`"+name+"`") {
			t.Errorf("action output %q is not documented in README.md or docs/github-action.md", name)
		}
	}
}

func readFileOrSkip(t *testing.T, path string) string {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func markdownFiles(t *testing.T) []string {
	var files []string
	for _, pattern := range []string{"*.md", "docs/*.md", "docs/*/*.md", "examples/*.md", "examples/*/*.md"} {
		m, _ := filepath.Glob(pattern)
		files = append(files, m...)
	}
	return files
}

func TestDocs_RelativeLinksResolve(t *testing.T) {
	link := regexp.MustCompile(`\]\(([^)#\s]+)(#[^)]*)?\)`)
	for _, f := range markdownFiles(t) {
		data, _ := os.ReadFile(f)
		for _, m := range link.FindAllStringSubmatch(string(data), -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(f), target)); err != nil {
				t.Errorf("%s: broken link %q", f, target)
			}
		}
	}
}

// configKeys are the top-level keys that mark a YAML block as Hephaestus
// configuration (or a fragment of one).
var configKeys = []string{"version", "project", "builds", "archives", "checksum", "release", "brews", "homebrew", "limits", "env", "dist", "manifest", "preflight"}

func TestDocs_ConfigurationExamplesAreValid(t *testing.T) {
	fence := regexp.MustCompile("(?s)```ya?ml\n(.*?)```")
	checked := 0
	for _, f := range append(markdownFiles(t), "hephaestus.yaml", "testdata/e2e/fixture/hephaestus.yaml") {
		data, _ := os.ReadFile(f)
		blocks := [][]string{{"", string(data)}}
		if strings.HasSuffix(f, ".md") {
			blocks = fence.FindAllStringSubmatch(string(data), -1)
		}
		for _, b := range blocks {
			doc := b[1]
			var top map[string]yaml.Node
			if yaml.Unmarshal([]byte(doc), &top) != nil || len(top) == 0 {
				continue
			}
			isConfig := true
			for k := range top {
				if !slices.Contains(configKeys, k) {
					isConfig = false
				}
			}
			if !isConfig {
				continue // workflows, GoReleaser excerpts, manifests...
			}
			if _, ok := top["project"]; !ok {
				doc = "project: {name: example}\n" + doc
			}
			if _, err := config.Load(strings.NewReader(doc), "/example"); err != nil {
				t.Errorf("%s: configuration example is invalid:\n%v\n---\n%s", f, err, b[1])
			}
			checked++
		}
	}
	if checked < 2 {
		t.Errorf("only %d configuration examples checked", checked)
	}
}

// The action runs the release binary of the version in action/VERSION
// (action/install-hephaestus.sh). The release workflow must refuse a tag
// that differs from it: otherwise the action at a release commit would look
// for another release's binary, or for none.
func TestAction_VersionNamesTheRelease(t *testing.T) {
	data, err := os.ReadFile("action/VERSION")
	if err != nil {
		t.Fatal(err)
	}
	v := strings.TrimSpace(string(data))
	if !regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`).MatchString(v) || string(data) != v+"\n" {
		t.Errorf("action/VERSION = %q, want one release tag and a newline", data)
	}
	release, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(release), `[[ "$(tr -d '\r\n' < action/VERSION)" == "$GITHUB_REF_NAME" ]]`) {
		t.Error("release.yml does not check that action/VERSION names the tag being released")
	}
}

// The action must not change the caller's environment or toolchain
// (action-isolation and action-install in ci.yml check the behavior): no
// setup-go or other toolchain, no compilation, and nothing written to
// GITHUB_PATH or GITHUB_ENV.
func TestAction_DoesNotChangeTheCallersEnvironment(t *testing.T) {
	for _, f := range []string{"action.yml", "action/install-hephaestus.sh"} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		code := regexp.MustCompile(`(?m)^\s*#.*$`).ReplaceAllString(string(data), "")
		for _, bad := range []string{"actions/setup-go", "GITHUB_PATH", "GITHUB_ENV", "go build", "go install", "GOTOOLCHAIN", "dl.google.com", "go.dev/dl"} {
			if strings.Contains(code, bad) {
				t.Errorf("%s uses %s", f, bad)
			}
		}
	}
}

// TestFuzzScript_StopsWhenListingFails runs scripts/fuzz.sh with a stub go:
// a package list or a fuzz target list that cannot be read fails the script
// (and the CI fuzz job) instead of fuzzing nothing.
func TestFuzzScript_StopsWhenListingFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	stub := `#!/bin/sh
echo "$*" >> "$STUB_LOG"
case "$1 $2" in
"list ./...") [ "$STUB_FAIL" = list ] && { echo "go: broken module" >&2; exit 1; }; echo example.com/p ;;
"test -list") [ "$STUB_FAIL" = test-list ] && { echo "p_test.go: syntax error" >&2; exit 1; }
  [ "$STUB_FAIL" = none ] || echo FuzzParse; echo "ok  example.com/p 0.1s" ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		fail   string
		ok     bool
		fuzzed bool
	}{
		{"", true, true},
		{"none", true, false},
		{"list", false, false},
		{"test-list", false, false},
	} {
		os.Remove(log)
		cmd := exec.Command("bash", "scripts/fuzz.sh", "1s")
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "STUB_LOG="+log, "STUB_FAIL="+tc.fail)
		out, err := cmd.CombinedOutput()
		calls, _ := os.ReadFile(log)
		fuzzed := strings.Contains(string(calls), "-fuzz ^FuzzParse$")
		if (err == nil) != tc.ok || fuzzed != tc.fuzzed {
			t.Errorf("STUB_FAIL=%q: err %v, fuzzed %t\ncalls:\n%s\noutput:\n%s", tc.fail, err, fuzzed, calls, out)
		}
	}
}

// TestGoEnvSnapshot_RunsGoOutsideTheModule runs scripts/go-env-snapshot.sh
// with a stub go from the repository root, as CI does: every go command must
// run outside any module. Inside this module, go.mod's toolchain line makes
// a caller's older Go (GOTOOLCHAIN=auto, no setup-go) download that
// toolchain, so the snapshot would depend on the network and report the
// downloaded Go instead of the caller's. A failing go still fails the
// script.
func TestGoEnvSnapshot_RunsGoOutsideTheModule(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	stub := `#!/bin/sh
echo "$PWD: $*" >> "$STUB_LOG"
d="$PWD"
while :; do
  [ -f "$d/go.mod" ] && { echo "go: downloading go9.9.9 (stub: run in the module at $d)" >&2; exit 1; }
  [ "$d" = / ] && break
  d="$(dirname "$d")"
done
[ "$STUB_FAIL" = "$1" ] && { echo "go: $1 failed" >&2; exit 1; }
case "$1" in
version) echo "go version go1.24.13 linux/amd64" ;;
env) printf 'go1.24.13\nauto\n/usr/lib/go\n' ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		fail string
		ok   bool
	}{
		{"", true},
		{"version", false},
		{"env", false},
	} {
		os.Remove(log)
		cmd := exec.Command("bash", "scripts/go-env-snapshot.sh")
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "STUB_LOG="+log, "STUB_FAIL="+tc.fail)
		out, err := cmd.CombinedOutput()
		calls, _ := os.ReadFile(log)
		if (err == nil) != tc.ok {
			t.Errorf("STUB_FAIL=%q: err %v\ncalls:\n%s\noutput:\n%s", tc.fail, err, calls, out)
		}
		if tc.ok && (!strings.Contains(string(out), "go version=go version go1.24.13 linux/amd64") || !strings.Contains(string(out), "go env=go1.24.13 auto /usr/lib/go")) {
			t.Errorf("snapshot lacks the caller's Go:\n%s", out)
		}
	}
}
