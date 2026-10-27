package workflowcheck

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/magicdrive/hephaestus/internal/config"
)

// The release workflows of the projects released with Hephaestus, verbatim
// from their default branches (testdata/<repo>/, with their hephaestus.yaml
// and ci.yml): magicdrive/ark, enma, goreg, kirke, kjak and t. Each passes
// every check as it is.
var fixtures = []string{"ark", "enma", "goreg", "kirke", "kjak", "t"}

// repo lays out a fixture as a repository: .github/workflows/release.yml and
// ci.yml, with release.yml rewritten by edit (nil keeps it), and returns the
// options its configuration gives for tag v1.2.3.
func repo(t *testing.T, name string, editRelease, editCI func(string) string) Options {
	t.Helper()
	dir := t.TempDir()
	wf := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	for file, edit := range map[string]func(string) string{"release.yml": editRelease, "ci.yml": editCI} {
		b, err := os.ReadFile(filepath.Join("testdata", name, file))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if edit != nil {
			if s = edit(s); s == string(b) {
				t.Fatalf("%s: the mutation does not apply to %s", name, file)
			}
		}
		if err := os.WriteFile(filepath.Join(wf, file), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return configOptions(t, filepath.Join("testdata", name, "hephaestus.yaml"), dir)
}

func configOptions(t *testing.T, cfgPath, dir string) Options {
	t.Helper()
	f, err := os.Open(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, err := config.Load(f, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vars, err := TagVars(cfg, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	return FromConfig(Options{Dir: dir}, cfg, vars)
}

func byID(rs []Result) map[string]Result {
	m := map[string]Result{}
	for _, r := range rs {
		m[r.ID] = r
	}
	return m
}

func TestFixturesPass(t *testing.T) {
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			o := repo(t, name, nil, nil)
			if !o.Formulae || len(o.Assets) < 2 || o.AssetsErr != nil {
				t.Fatalf("options %+v", o)
			}
			rs := Run(o)
			if len(rs) != len(IDs()) {
				t.Fatalf("%d results", len(rs))
			}
			for i, r := range rs {
				if r.ID != IDs()[i] {
					t.Errorf("result %d is %s", i, r.ID)
				}
				if r.Status != Pass {
					t.Errorf("%s: %s %s", r.ID, r.Status, r.Message)
				}
				t.Logf("%s: %s", r.ID, r.Message)
			}
		})
	}
}

// TestHephaestusOwnWorkflowsPass: Hephaestus's release workflow, which
// runs hephaestus from source in scripts rather than the action, passes.
func TestHephaestusOwnWorkflowsPass(t *testing.T) {
	root := filepath.Join("..", "..")
	o := configOptions(t, filepath.Join(root, "hephaestus.yaml"), root)
	for _, r := range Run(o) {
		if r.Status != Pass {
			t.Errorf("%s: %s %s", r.ID, r.Status, r.Message)
		}
	}
}

const (
	tSHA   = "e544acf59c79cbafa04a48002b0f30f91a02f8ba"
	tapIf  = "        if: steps.plan.outputs.tap == 'publish'\n"
	tapTok = "          tap-token: ${{ secrets.HOMEBREW_TAP_GITHUB_TOKEN }}\n"
)

func sub(old, new string) func(string) string {
	return func(s string) string { return strings.Replace(s, old, new, 1) }
}

func subAll(old, new string) func(string) string {
	return func(s string) string { return strings.ReplaceAll(s, old, new) }
}

func chain(fs ...func(string) string) func(string) string {
	return func(s string) string {
		for _, f := range fs {
			s = f(s)
		}
		return s
	}
}

// TestViolationsAreDetected: each mutation of magicdrive/t's workflows
// breaks one convention, and the check for it fails with a message naming
// the problem.
func TestViolationsAreDetected(t *testing.T) {
	cases := []struct {
		name        string
		release, ci func(string) string
		opts        func(*Options)
		id, want    string
	}{
		// workflow.pins
		{"action pinned to a tag", sub("actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", "actions/checkout@v7"), nil, nil,
			IDPins, "uses actions/checkout@v7 is not pinned to a full commit SHA"},
		{"action pinned to a short SHA", sub("actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", "actions/checkout@3d3c42e"), nil, nil,
			IDPins, "is not pinned to a full commit SHA"},
		{"CI action unpinned", nil, sub("actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1", "actions/checkout@main"), nil,
			IDPins, ".github/workflows/ci.yml: job"},
		{"Hephaestus pinned to a tag", sub("magicdrive/hephaestus@"+tSHA+" # v1.3.0", "magicdrive/hephaestus@v1.3.0"), nil, nil,
			IDPins, "uses magicdrive/hephaestus@v1.3.0 is not pinned"},
		{"CI on another Hephaestus commit", nil, sub("magicdrive/hephaestus@"+tSHA, "magicdrive/hephaestus@160990d32bbc83af702bde8a63f49ffcac32972c"), nil,
			IDPins, "the release workflow e544acf59c79cbafa04a48002b0f30f91a02f8ba: CI must check what the release publishes"},
		{"release on two Hephaestus commits", sub("magicdrive/hephaestus@"+tSHA, "magicdrive/hephaestus@160990d32bbc83af702bde8a63f49ffcac32972c"), nil, nil,
			IDPins, "release.yml runs Hephaestus at 2 different commits"},
		{"Hephaestus pin without its version", sub("@"+tSHA+" # v1.3.0", "@"+tSHA), nil, nil,
			IDPins, "without its release as a comment"},
		{"Hephaestus pins name two versions", sub("@"+tSHA+" # v1.3.0", "@"+tSHA+" # v1.2.0"), nil, nil,
			IDPins, "name different releases (v1.2.0, v1.3.0)"},
		{"docker image by tag", sub("      - name: Verify dist\n", "      - uses: docker://alpine:3\n      - name: Verify dist\n"), nil, nil,
			IDPins, "uses docker://alpine:3 is not pinned to an image digest"},

		// workflow.tap
		{"rehearsal without the plan condition", sub("        # branch before the release becomes public.\n"+tapIf, "        # branch before the release becomes public.\n"), nil, nil,
			IDTap, `step "Rehearse the formula and tap pull request (read-only)" uses HOMEBREW_TAP_GITHUB_TOKEN with if: ""`},
		{"tap step on the tag name", sub("      - name: Generate the formula and open a tap pull request\n"+tapIf, "      - name: Generate the formula and open a tap pull request\n        if: ${{ !contains(github.ref_name, '-') }}\n"), nil, nil,
			IDTap, "want exactly if: steps.plan.outputs.tap == 'publish'"},
		{"tap condition widened", sub("      - name: Generate the formula and open a tap pull request\n"+tapIf, "      - name: Generate the formula and open a tap pull request\n        if: always() && steps.plan.outputs.tap == 'publish'\n"), nil, nil,
			IDTap, "with if: \"always() && steps.plan.outputs.tap == 'publish'\""},
		{"token check on another condition", sub("      - name: Check the tap token\n"+tapIf, "      - name: Check the tap token\n        if: always()\n"), nil, nil,
			IDTap, `step "Check the tap token" uses HOMEBREW_TAP_GITHUB_TOKEN with if: "always()"`},
		{"no plan step id", sub("        id: plan\n", ""), nil, nil,
			IDTap, "reads steps.plan, but no step with id: plan runs before it"},
		{"plan step runs another command", sub("          command: plan\n", "          command: check\n"), nil, nil,
			IDTap, "does not run the plan"},
		{"plan step gets the tap token", sub("          command: plan\n", "          command: plan\n"+tapTok), nil, nil,
			IDTap, "the plan step gets HOMEBREW_TAP_GITHUB_TOKEN"},
		{"plan step made conditional", sub("        id: plan\n", "        id: plan\n        if: always()\n"), nil, nil,
			IDTap, `has if: "always()"; it must always run`},
		{"plan after a tap step", chain(sub("      - name: Plan the release\n        id: plan\n", "      - name: Plan the release\n"),
			sub("          stages: formula,tap\n          github-token: \"\"\n          tap-token: ${{ secrets.HOMEBREW_TAP_GITHUB_TOKEN }}\n",
				"          stages: formula,tap\n          github-token: \"\"\n          tap-token: ${{ secrets.HOMEBREW_TAP_GITHUB_TOKEN }}\n      - id: plan\n        uses: magicdrive/hephaestus@"+tSHA+" # v1.3.0\n        with:\n          command: plan\n")), nil, nil,
			IDTap, "before the plan step"},
		{"tap step renamed", sub("- name: Generate the formula and open a tap pull request", "- name: Generate the formula and open the tap pull request"), nil, nil,
			IDTap, `no step named "Generate the formula and open a tap pull request"`},
		{"token given to another step", sub("          command: release\n", "          command: release\n"+tapTok), nil, nil,
			IDTap, `step "Publish the GitHub release" gets the value of HOMEBREW_TAP_GITHUB_TOKEN; only the steps`},
		{"token in the job env", sub("    environment: release\n", "    environment: release\n    env:\n      TAP: ${{ secrets.HOMEBREW_TAP_GITHUB_TOKEN }}\n"), nil, nil,
			IDTap, "job release env passes HOMEBREW_TAP_GITHUB_TOKEN to every step"},
		{"token in the workflow env", sub("permissions:\n  contents: read\n", "permissions:\n  contents: read\nenv:\n  TAP: ${{ secrets.HOMEBREW_TAP_GITHUB_TOKEN }}\n"), nil, nil,
			IDTap, "the workflow env passes"},
		{"secrets inherited by CI", sub("    uses: ./.github/workflows/ci.yml\n", "    uses: ./.github/workflows/ci.yml\n    secrets: inherit\n"), nil, nil,
			IDTap, "job ci passes HOMEBREW_TAP_GITHUB_TOKEN to the workflow it calls (secrets: inherit)"},
		{"secrets serialized", sub("          command: release\n", "          command: release\n          tap-token: ${{ toJSON(secrets) }}\n"), nil, nil,
			IDTap, "indexes or serializes secrets"},
		{"secrets indexed", sub("          command: release\n", "          command: release\n          tap-token: ${{ secrets['HOMEBREW_TAP_GITHUB_TOKEN'] }}\n"), nil, nil,
			IDTap, "indexes or serializes secrets"},

		{"rehearsal publishes", sub("          stages: formula,tap\n          dry-run: \"true\"\n", "          stages: formula,tap\n"), nil, nil,
			IDTap, `step "Rehearse the formula and tap pull request (read-only)" is not a dry run`},

		// workflow.release
		{"release step made conditional", sub("      - name: Publish the GitHub release\n", "      - name: Publish the GitHub release\n        if: steps.plan.outputs.tap == 'publish'\n"), nil, nil,
			IDRelease, `step "Publish the GitHub release" publishes the release with if:`},
		{"release step may fail", sub("      - name: Publish the GitHub release\n", "      - name: Publish the GitHub release\n        continue-on-error: true\n"), nil, nil,
			IDRelease, "publishes the release with continue-on-error"},
		{"another trigger", sub("on:\n  push:\n    tags: [\"v*\"]\n", "on:\n  push:\n    tags: [\"v*\"]\n  workflow_dispatch:\n"), nil, nil,
			IDRelease, "runs on workflow_dispatch"},
		{"branch pushes", sub("    tags: [\"v*\"]\n", "    tags: [\"v*\"]\n    branches: [master]\n"), nil, nil,
			IDRelease, "on.push.branches"},
		{"every push", sub("on:\n  push:\n    tags: [\"v*\"]\n", "on: push\n"), nil, nil,
			IDRelease, "runs on more than tag pushes"},
		{"write by default", sub("permissions:\n  contents: read\n", "permissions:\n  contents: write\n"), nil, nil,
			IDRelease, "top-level permissions grant contents: write"},
		{"no default permissions", sub("permissions:\n  contents: read\n\nconcurrency", "concurrency"), nil, nil,
			IDRelease, "no top-level permissions"},
		{"no environment", sub("    environment: release\n", ""), nil, nil,
			IDRelease, "the publishing job release runs in no environment"},
		{"another environment", nil, nil, func(o *Options) { o.Environment = "production" },
			IDRelease, "runs in environment release, want production"},
		{"release does not need CI", chain(sub("    needs: [ci, build]\n", "    needs: [build]\n"), sub("    needs: [ci]\n", "")), nil, nil,
			IDRelease, "does not need a job that runs the CI workflow"},
		{"two publishing jobs", sub("      - name: Verify dist\n", "      - uses: magicdrive/hephaestus@"+tSHA+" # v1.3.0\n        with:\n          command: release\n      - name: Verify dist\n"), nil, nil,
			IDRelease, "jobs build, release all publish"},
		{"build publishes everything", sub("          stages: archive,checksum\n", "          stages: all\n"), nil, nil,
			IDRelease, "jobs build, release all publish"},
		{"nothing publishes", chain(sub("          command: release\n", "          command: check\n"), subAll("          stages: formula,tap\n          github-token: \"\"\n", "          stages: formula,tap\n          dry-run: \"true\"\n          github-token: \"\"\n")), nil, nil,
			IDRelease, "no job publishes the release"},

		// workflow.attestation
		{"no attestation", func(s string) string {
			i, j := strings.Index(s, "      - name: Attest build provenance\n"), strings.Index(s, "      - name: Publish the GitHub release\n")
			return s[:i] + s[j:]
		}, nil, nil, IDAttestation, "has no actions/attest-build-provenance step"},
		{"checksum file not attested", sub("            dist/t_checksums.txt\n", ""), nil, nil,
			IDAttestation, "does not cover t_checksums.txt"},
		{"archives not attested", sub("            dist/t_*.tar.gz\n", ""), nil, nil,
			IDAttestation, "does not cover t_1.2.3_darwin_amd64.tar.gz"},
		{"attestation skipped", sub("        uses: actions/attest-build-provenance@", "        if: steps.plan.outputs.tap == 'publish'\n        uses: actions/attest-build-provenance@"), nil, nil,
			IDAttestation, "a skipped attestation passes the job"},
		{"subject is an expression", sub("            dist/t_checksums.txt\n", "            ${{ env.SUBJECTS }}\n"), nil, nil,
			IDAttestation, "is an expression"},
		{"attestation after publishing", func(s string) string {
			i, j := strings.Index(s, "      - name: Attest build provenance\n"), strings.Index(s, "      - name: Publish the GitHub release\n")
			k := strings.Index(s, "      - name: Generate the formula and open a tap pull request\n")
			return s[:i] + s[j:k] + s[i:j] + s[k:]
		}, nil, nil, IDAttestation, "attests after step \"Publish the GitHub release\" publishes"},
		{"asset names unknown", nil, nil, func(o *Options) { o.AssetsErr = errors.New("archives[0].name: bad template") },
			IDAttestation, "the release asset names are not known"},

		// workflow.ci-gate
		{"ci-gate misses a job", nil, sub("    needs: [lint, smoke, package, homebrew]\n", "    needs: [lint, smoke, package]\n"), nil,
			IDCIGate, "ci-gate does not need homebrew"},
		{"ci-gate not always", nil, sub("    if: always()\n    needs: [lint, smoke, package, homebrew]\n", "    needs: [lint, smoke, package, homebrew]\n"), nil,
			IDCIGate, `ci-gate has if: ""; want if: always()`},
		{"ci-gate ignores the results", nil, sub("${{ join(needs.*.result, ' ') }}", "success"), nil,
			IDCIGate, "reads the needed jobs' results"},
		{"no ci-gate", nil, sub("  ci-gate:\n", "  gate:\n"), nil,
			IDCIGate, "no job ci-gate"},
		{"ci.yml unreadable", nil, func(string) string { return "jobs: [" }, nil,
			IDCIGate, "cannot check .github/workflows/ci.yml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := repo(t, "t", c.release, c.ci)
			if c.opts != nil {
				c.opts(&o)
			}
			r := byID(Run(o))[c.id]
			if r.Status != Fail || !strings.Contains(r.Message, c.want) {
				t.Errorf("%s: %s %s\nwant FAIL with %q", c.id, r.Status, r.Message, c.want)
			}
			if r.Fix == "" {
				t.Errorf("%s: no remediation", c.id)
			}
		})
	}
}

// TestUnreadableWorkflowsFailClosed: a workflow the checks cannot read
// fails every check; only a release workflow that is neither configured
// nor present skips them.
func TestUnreadableWorkflowsFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		release func(string) string
		opts    func(*Options)
		status  Status
		want    string
	}{
		{"not YAML", func(string) string { return "on: [\n" }, nil, Fail, "cannot check the release workflow"},
		{"anchors", sub("    permissions:\n      contents: read\n    steps:", "    permissions: &ro\n      contents: read\n    steps:"), nil, Fail, "anchors and aliases are not supported"},
		{"steps not a list", sub("    steps:\n      - uses: actions/checkout@", "    steps: {}\n    x:\n      - uses: actions/checkout@"), nil, Fail, "steps: not a list"},
		{"configured workflow missing", nil, func(o *Options) { o.Workflow = "publish.yml" }, Fail, ".github/workflows/publish.yml does not exist"},
		{"workflow name with a path", nil, func(o *Options) { o.Workflow = "../release.yml" }, Fail, "want a file name"},
		{"no release workflow", nil, func(o *Options) { o.Dir = t.TempDir(); o.Workflow = "" }, Skip, "no .github/workflows/release.yml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := repo(t, "t", c.release, nil)
			o.Workflow = ""
			if c.opts != nil {
				c.opts(&o)
			}
			for _, r := range Run(o) {
				if r.Status != c.status || !strings.Contains(r.Message, c.want) {
					t.Errorf("%s: %s %s; want %s with %q", r.ID, r.Status, r.Message, c.status, c.want)
				}
			}
		})
	}
}

// TestPublishingSteps pins which steps count as publishing.
func TestPublishingSteps(t *testing.T) {
	cases := []struct {
		s    step
		want bool
	}{
		{step{uses: "magicdrive/hephaestus@x", with: map[string]string{"command": "release"}}, true},
		{step{uses: "magicdrive/hephaestus@x", with: map[string]string{}}, true}, // run, every stage
		{step{uses: "magicdrive/hephaestus@x", with: map[string]string{"command": "run", "stages": "archive,checksum"}}, false},
		{step{uses: "magicdrive/hephaestus@x", with: map[string]string{"command": "run", "stages": "formula,tap"}}, true},
		{step{uses: "magicdrive/hephaestus@x", with: map[string]string{"command": "run", "stages": "formula,tap", "dry-run": "true"}}, false},
		{step{uses: "magicdrive/hephaestus@x", with: map[string]string{"command": "${{ inputs.cmd }}"}}, true},
		{step{uses: "magicdrive/hephaestus@x", with: map[string]string{"command": "plan"}}, false},
		{step{run: "./build/hephaestus run --stages release"}, true},
		{step{run: "go run github.com/magicdrive/hephaestus@v1.3.0 run"}, true},
		{step{run: "./build/hephaestus run --stages formula,tap --dry-run"}, false},
		{step{run: "./build/hephaestus run --stages build,archive,checksum --fresh"}, false},
		{step{run: "hephaestus check"}, false},
	}
	for _, c := range cases {
		if got := publishes(&c.s); got != c.want {
			t.Errorf("publishes(%q %v %q) = %t", c.s.uses, c.s.with, c.s.run, got)
		}
	}
}
