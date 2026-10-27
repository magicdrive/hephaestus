package hephaestus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workflowRepo lays out magicdrive/t's configuration and workflows
// (internal/workflowcheck/testdata/t) as a git repository, with release.yml
// rewritten by edit, and returns its configuration path.
func workflowRepo(t *testing.T, git bool, edit func(string) string) string {
	t.Helper()
	src := filepath.Join("..", "..", "internal", "workflowcheck", "testdata", "t")
	root := t.TempDir()
	wf := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	if git {
		if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for from, to := range map[string]string{"hephaestus.yaml": "hephaestus.yaml", "release.yml": ".github/workflows/release.yml", "ci.yml": ".github/workflows/ci.yml"} {
		b, err := os.ReadFile(filepath.Join(src, from))
		if err != nil {
			t.Fatal(err)
		}
		if from == "release.yml" && edit != nil {
			b = []byte(edit(string(b)))
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(to)), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(root, "hephaestus.yaml")
}

func TestCheckWorkflows(t *testing.T) {
	// Without --workflows, check does not read the workflows.
	cfg := workflowRepo(t, true, func(s string) string { return strings.Replace(s, "    environment: release\n", "", 1) })
	if code, out, errs := runCLI(t, "check", "--config", cfg); code != 0 || strings.Contains(out, "workflow.") {
		t.Fatalf("check without --workflows: %d\n%s%s", code, out, errs)
	}

	cfg = workflowRepo(t, true, nil)
	code, out, errs := runCLI(t, "check", "--config", cfg, "--workflows")
	if code != 0 || !strings.Contains(out, "PASS  workflow.tap") || !strings.Contains(out, "PASS  workflow.attestation") || strings.Contains(out, "FAIL") {
		t.Fatalf("check --workflows on t: %d\n%s%s", code, out, errs)
	}

	cfg = workflowRepo(t, true, func(s string) string {
		return strings.Replace(s, "        if: steps.plan.outputs.tap == 'publish'\n        uses: magicdrive/hephaestus@", "        uses: magicdrive/hephaestus@", 1)
	})
	code, out, _ = runCLI(t, "check", "--config", cfg, "--workflows")
	if code != 1 || !strings.Contains(out, "FAIL  workflow.tap") || !strings.Contains(out, `"Rehearse the formula and tap pull request (read-only)" uses HOMEBREW_TAP_GITHUB_TOKEN with if: ""`) ||
		!strings.Contains(out, "→ plan the release first") {
		t.Fatalf("check --workflows, tap step without if: %d\n%s", code, out)
	}

	// Fail closed: no repository to find .github/workflows in.
	cfg = workflowRepo(t, false, nil)
	if code, _, errs := runCLI(t, "check", "--config", cfg, "--workflows"); code != 1 || !strings.Contains(errs, "no git repository holds") {
		t.Fatalf("check --workflows outside a repository: %d %s", code, errs)
	}
}
