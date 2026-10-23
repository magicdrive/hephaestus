package main

// Repository consistency tests: they keep workflows, the Makefile, the
// action definition and the documentation true to the implementation, so a
// change that breaks one of those promises fails `go test ./...`.

import (
	"os"
	"path/filepath"
	"regexp"
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
	Steps       []struct {
		Name string `yaml:"name"`
		Uses string `yaml:"uses"`
		Run  string `yaml:"run"`
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

	rel := readWorkflow(t, ".github/workflows/release.yml")
	if j := rel.Jobs["ci"]; j == nil || j.Uses != "./.github/workflows/ci.yml" {
		t.Fatal("release.yml must run ci.yml as its ci job")
	}
	for name, j := range rel.Jobs {
		if name == "ci" {
			continue
		}
		if !slices.Contains(j.needs(), "ci") {
			t.Errorf("release job %q does not need ci", name)
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
			t.Errorf("release job %q publishes before verify-dist.sh", name)
		}
	}
}

// TestReleaseWorkflow_BuildsWithoutWriteTokens keeps the build/publish
// boundary: a job that runs project build commands holds only read access,
// so build code can neither write to the repository nor request an OIDC
// token to sign provenance.
func TestReleaseWorkflow_BuildsWithoutWriteTokens(t *testing.T) {
	rel := readWorkflow(t, ".github/workflows/release.yml")
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
			t.Errorf("job %q runs build commands with permissions %v, want only contents: read", name, j.Permissions)
		}
	}
	if builds == 0 {
		t.Error("no release job runs the build stage")
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
var configKeys = []string{"version", "project", "builds", "archives", "checksum", "release", "brews", "homebrew", "limits", "env", "dist", "manifest"}

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
