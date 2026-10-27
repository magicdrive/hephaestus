// Package workflowcheck checks a repository's release workflow against the
// conventions every project released with Hephaestus relies on
// (docs/publish.md#workflows): it publishes only from tag pushes, after the
// full CI and from one job in a protected environment; it attests every
// release asset; the tap token reaches only the two tap steps, and only when
// the plan publishes the tap; every action is pinned to a commit; and the CI
// workflow's ci-gate needs every job.
//
// It reads the workflow files only (YAML, whatever language the project is
// written in) and never runs anything. What it cannot read unambiguously —
// an expression where a path or a condition is expected, an anchor, a step
// that indexes `secrets` — is reported as a problem: the checks fail closed.
package workflowcheck

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Check IDs, in the order Run returns them. They are part of the preflight
// report and of preflight.skip.
const (
	IDRelease     = "workflow.release"
	IDPins        = "workflow.pins"
	IDTap         = "workflow.tap"
	IDAttestation = "workflow.attestation"
	IDCIGate      = "workflow.ci-gate"
)

// IDs lists every check ID in order.
func IDs() []string { return []string{IDRelease, IDPins, IDTap, IDAttestation, IDCIGate} }

// Titles are the checks' one-line descriptions.
var Titles = map[string]string{
	IDRelease:     "Release workflow publishes from one job, after CI, on tag pushes",
	IDPins:        "Workflow actions are pinned to commit SHAs",
	IDTap:         "Tap token reaches only the planned tap steps",
	IDAttestation: "Every release asset is attested before publishing",
	IDCIGate:      "CI's ci-gate needs every CI job",
}

// Conventions shared with hephaestus publish (internal/publish).
const (
	DefaultWorkflow       = "release.yml"
	DefaultTapTokenSecret = "HOMEBREW_TAP_GITHUB_TOKEN" //nolint:gosec // a secret name, not a credential
	GateJob               = "ci-gate"
	TapCondition          = "steps.plan.outputs.tap == 'publish'"
	TapRehearsalStep      = "Rehearse the formula and tap pull request (read-only)"
	TapPublishStep        = "Generate the formula and open a tap pull request"
	hephaestusAction      = "magicdrive/hephaestus"
	workflowsDir          = ".github/workflows"
)

// Options says what to check.
type Options struct {
	// Dir is the repository root, the directory holding .github/workflows.
	Dir string
	// Workflow is the release workflow's file name under .github/workflows.
	// Empty means release.yml when it exists; when it does not, every
	// check is skipped. A named workflow that does not exist fails.
	Workflow string
	// Environment is the environment the publishing job must run in; empty
	// accepts any environment (but requires one).
	Environment string
	// TapTokenSecret is the tap token's secret name (default
	// HOMEBREW_TAP_GITHUB_TOKEN).
	TapTokenSecret string
	// Formulae is true when a formula is published to a tap: the two tap
	// steps must then exist.
	Formulae bool
	// Assets are the release asset names (archives and the checksum file)
	// the attestation must cover. AssetsErr, when set, says why they are
	// not known; Unknown lists assets named only after building.
	Assets    []string
	AssetsErr error
	Unknown   []string
}

// Status is a check outcome.
type Status string

const (
	Pass Status = "PASS"
	Fail Status = "FAIL"
	Skip Status = "SKIP"
)

// Result is one check's outcome. Problems lists each violation, Message
// summarizes them (or what passed).
type Result struct {
	ID       string
	Status   Status
	Message  string
	Problems []string
	Fix      string
	Evidence map[string]any
}

// Run runs every check and returns one result per ID, in IDs() order.
func Run(o Options) []Result {
	c, skipMsg, err := load(o)
	results := make([]Result, 0, len(IDs()))
	for _, id := range IDs() {
		switch {
		case err != nil:
			results = append(results, Result{ID: id, Status: Fail, Message: err.Error(), Problems: []string{err.Error()},
				Fix: "fix the workflow file, or set preflight.workflow to the release workflow's file name"})
		case skipMsg != "":
			results = append(results, Result{ID: id, Status: Skip, Message: skipMsg})
		default:
			results = append(results, c.run(id))
		}
	}
	return results
}

// checker holds the parsed workflows and what was derived from them.
type checker struct {
	o       Options
	release *workflow
	// ci are the local workflows (./.github/workflows/...) that jobs the
	// publishing job needs call, by path; ciErr records those that could
	// not be read.
	ci       map[string]*workflow
	ciErr    map[string]error
	pubJob   *job    // the publishing job; nil when none or ambiguous
	pubSteps []*step // its publishing steps
	pubErr   string  // why there is no publishing job
}

func load(o Options) (*checker, string, error) {
	name, required := o.Workflow, o.Workflow != ""
	if name == "" {
		name = DefaultWorkflow
	}
	if name != filepath.Base(name) || name == "." || name == ".." || !(strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")) {
		return nil, "", fmt.Errorf("release workflow %q: want a file name under %s, e.g. release.yml", name, workflowsDir)
	}
	rel := workflowsDir + "/" + name
	src, err := os.ReadFile(filepath.Join(o.Dir, filepath.FromSlash(rel)))
	switch {
	case errors.Is(err, fs.ErrNotExist) && !required:
		return nil, "no " + rel + " (set preflight.workflow to check another release workflow)", nil
	case errors.Is(err, fs.ErrNotExist):
		return nil, "", fmt.Errorf("%s does not exist (preflight.workflow / --workflow names it)", rel)
	case err != nil:
		return nil, "", fmt.Errorf("cannot read %s: %w", rel, err)
	}
	w, err := parseWorkflow(rel, src)
	if err != nil {
		return nil, "", fmt.Errorf("cannot check the release workflow: %w", err)
	}
	if o.TapTokenSecret == "" {
		o.TapTokenSecret = DefaultTapTokenSecret
	}
	c := &checker{o: o, release: w, ci: map[string]*workflow{}, ciErr: map[string]error{}}
	c.findPublishingJob()
	if c.pubJob != nil {
		for _, id := range c.release.ancestors(c.pubJob.id) {
			j := c.release.job(id)
			if j == nil || !strings.HasPrefix(j.uses, "./") {
				continue
			}
			p := path.Clean(strings.TrimPrefix(j.uses, "./"))
			if _, done := c.ci[p]; done || c.ciErr[p] != nil {
				continue
			}
			src, err := os.ReadFile(filepath.Join(o.Dir, filepath.FromSlash(p)))
			if err == nil {
				var cw *workflow
				if cw, err = parseWorkflow(p, src); err == nil {
					c.ci[p] = cw
					continue
				}
			}
			c.ciErr[p] = err
		}
	}
	return c, "", nil
}

func (c *checker) run(id string) Result {
	var r Result
	switch id {
	case IDRelease:
		r = c.checkRelease()
	case IDPins:
		r = c.checkPins()
	case IDTap:
		r = c.checkTap()
	case IDAttestation:
		r = c.checkAttestation()
	case IDCIGate:
		r = c.checkCIGate()
	}
	r.ID = id
	if r.Status == "" {
		r.Status = Pass
		if len(r.Problems) > 0 {
			r.Status = Fail
			r.Message = strings.Join(r.Problems, "; ")
		}
	}
	return r
}

// --- publishing steps ---

var cliPublishRe = regexp.MustCompile(`(?m)\bhephaestus(?:@\S+)?\s+(run|release|tap)\b([^\n]*)`)
var stagesFlagRe = regexp.MustCompile(`--stages[= ]\s*(\S+)`)

// isHephaestusAction reports whether uses names the Hephaestus action.
func isHephaestusAction(uses string) bool {
	repo, _, ok := strings.Cut(uses, "@")
	return ok && strings.EqualFold(repo, hephaestusAction)
}

// stagesPublish reports whether a stage list (comma-separated, "" or "all"
// for every stage) includes a publishing stage.
func stagesPublish(stages string) bool {
	if stages == "" || stages == "all" {
		return true
	}
	for _, s := range strings.Split(stages, ",") {
		if s = strings.TrimSpace(s); s == "release" || s == "tap" || s == "all" || strings.Contains(s, "${{") {
			return true
		}
	}
	return false
}

// publishes reports whether a step publishes (the release or the tap):
// the action with command release or tap, or run with a publishing stage,
// not dry-run; or a run script that calls hephaestus run, release or tap
// that way. An expression where a command or a stage is expected counts as
// publishing (fail closed).
func publishes(s *step) bool {
	if isHephaestusAction(s.uses) {
		cmd := s.with["command"]
		if strings.Contains(cmd, "${{") || strings.Contains(s.with["dry-run"], "${{") {
			return true
		}
		if s.with["dry-run"] == "true" {
			return false
		}
		switch cmd {
		case "release", "tap":
			return true
		case "", "run":
			return stagesPublish(s.with["stages"])
		}
		return false
	}
	for _, m := range cliPublishRe.FindAllStringSubmatch(s.run, -1) {
		rest := m[2]
		if strings.Contains(rest, "--dry-run") {
			continue
		}
		if m[1] != "run" {
			return true
		}
		st := stagesFlagRe.FindStringSubmatch(rest)
		if st == nil || stagesPublish(strings.Trim(st[1], `"'`)) {
			return true
		}
	}
	return false
}

// publishesRelease reports whether a publishing step publishes the GitHub
// release (not only the tap).
func publishesRelease(s *step) bool {
	if isHephaestusAction(s.uses) {
		cmd, st := s.with["command"], s.with["stages"]
		return cmd == "release" || ((cmd == "" || cmd == "run") && (st == "" || st == "all" || strings.Contains(st, "release")))
	}
	return strings.Contains(s.run, "release")
}

// dryRun reports whether a step runs Hephaestus as a dry run only.
func dryRun(s *step) bool {
	if isHephaestusAction(s.uses) {
		return s.with["dry-run"] == "true"
	}
	ms := cliPublishRe.FindAllStringSubmatch(s.run, -1)
	if len(ms) == 0 {
		return strings.Contains(s.run, "--dry-run")
	}
	for _, m := range ms {
		if !strings.Contains(m[2], "--dry-run") {
			return false
		}
	}
	return true
}

func (c *checker) findPublishingJob() {
	var jobs []*job
	for _, j := range c.release.jobs {
		var ps []*step
		for _, s := range j.steps {
			if publishes(s) {
				ps = append(ps, s)
			}
		}
		if len(ps) > 0 {
			jobs = append(jobs, j)
			if c.pubJob == nil {
				c.pubJob, c.pubSteps = j, ps
			}
		}
	}
	switch len(jobs) {
	case 0:
		c.pubErr = c.release.path + ": no job publishes the release: no step runs the Hephaestus action with command: release (or run with the release stage), nor hephaestus run/release in a script"
	case 1:
	default:
		var ids []string
		for _, j := range jobs {
			ids = append(ids, j.id)
		}
		c.pubJob, c.pubSteps = nil, nil
		c.pubErr = fmt.Sprintf("%s: jobs %s all publish; exactly one job may publish, so that one environment and one set of gates guard every write", c.release.path, strings.Join(ids, ", "))
	}
}

// --- workflow.release ---

func (c *checker) checkRelease() Result {
	w := c.release
	var p []string
	// Triggers: push of tags only.
	switch {
	case w.on == nil:
		p = append(p, w.path+": no `on:` triggers")
	case w.on.Kind != yaml.MappingNode:
		p = append(p, fmt.Sprintf("%s: on: %s runs on more than tag pushes; use on: push: tags: [\"v*\"]", w.path, strings.Join(scalars(w.on), ", ")))
	default:
		for ev, v := range pairs(w.on) {
			if ev != "push" {
				p = append(p, fmt.Sprintf("%s: runs on %s; the release workflow must run only on push of tags (a release from another event skips the tag's protections)", w.path, ev))
				continue
			}
			hasTags := false
			for k := range pairs(v) {
				switch k {
				case "tags":
					hasTags = len(scalars(pairsValue(v, "tags"))) > 0
				case "branches", "branches-ignore", "tags-ignore":
					p = append(p, fmt.Sprintf("%s: on.push.%s: the release workflow must run only on push of tags", w.path, k))
				}
			}
			if !hasTags {
				p = append(p, w.path+": on.push has no tags filter: every push would run the release workflow; use on: push: tags: [\"v*\"]")
			}
		}
	}
	// Default token permissions: read-only.
	if bad := writePermissions(w.permissions); w.permissions == nil {
		p = append(p, w.path+": no top-level permissions: the default token may be able to write; set permissions: contents: read and grant writes per job")
	} else if bad != "" {
		p = append(p, fmt.Sprintf("%s: top-level permissions grant %s: grant writes only to the publishing job", w.path, bad))
	}
	ev := map[string]any{"workflow": w.path}
	if c.pubJob == nil {
		p = append(p, c.pubErr)
		return Result{Problems: p, Fix: "publish from one job (the action's command: release), in environment release, that needs the job running the CI workflow", Evidence: ev}
	}
	j := c.pubJob
	ev["publishing_job"] = j.id
	want := c.o.Environment
	switch {
	case !j.hasEnv || j.environment == "":
		p = append(p, fmt.Sprintf("%s: the publishing job %s runs in no environment: its secrets and write token are not guarded by the environment's tag-only deployment policy; set environment: %s", w.path, j.id, orDefault(want, "release")))
	case strings.Contains(j.environment, "${{"):
		p = append(p, fmt.Sprintf("%s: the publishing job %s's environment is an expression (%s); it must name the environment", w.path, j.id, j.environment))
	case want != "" && j.environment != want:
		p = append(p, fmt.Sprintf("%s: the publishing job %s runs in environment %s, want %s (preflight.environment)", w.path, j.id, j.environment, want))
	}
	if strings.Contains(j.environment, "${{") || j.environment != "" {
		ev["environment"] = j.environment
	}
	var calls []string
	for _, id := range w.ancestors(j.id) {
		if a := w.job(id); a != nil && strings.HasPrefix(a.uses, "./") {
			calls = append(calls, id+" ("+a.uses+")")
		}
	}
	if len(calls) == 0 {
		p = append(p, fmt.Sprintf("%s: the publishing job %s does not need a job that runs the CI workflow (a job with uses: ./%s/ci.yml): the release could publish what CI never passed", w.path, j.id, workflowsDir))
	} else {
		ev["ci_jobs"] = calls
	}
	if truthy(j.contOnErr) {
		p = append(p, fmt.Sprintf("%s: the publishing job %s has continue-on-error", w.path, j.id))
	}
	for _, s := range c.pubSteps {
		if !publishesRelease(s) {
			continue
		}
		if truthy(s.contOnErr) {
			p = append(p, fmt.Sprintf("%s: job %s step %s publishes the release with continue-on-error: a failed release would pass the run", w.path, j.id, s.label()))
		}
		if cond := normalizeCond(s.cond); cond != "" && cond != "success()" {
			p = append(p, fmt.Sprintf("%s: job %s step %s publishes the release with if: %q: a skipped release step passes the run", w.path, j.id, s.label(), s.cond))
		}
	}
	return Result{Problems: p, Message: fmt.Sprintf("job %s publishes, in environment %s, after %s", j.id, j.environment, strings.Join(calls, ", ")),
		Fix: "run the release workflow only on push of v* tags, with permissions: contents: read at the top, and publish from one job in environment release that needs the CI job", Evidence: ev}
}

func pairsValue(n *yaml.Node, key string) *yaml.Node {
	for k, v := range pairs(n) {
		if k == key {
			return v
		}
	}
	return nil
}

// writePermissions returns the write grants of a permissions node ("" for
// none): write-all, or each scope set to write.
func writePermissions(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	if n.Kind == yaml.ScalarNode {
		if n.Value == "read-all" || n.Value == "{}" || n.Value == "" {
			return ""
		}
		return n.Value
	}
	var w []string
	for k, v := range pairs(n) {
		if scalar(v) != "read" && scalar(v) != "none" {
			w = append(w, k+": "+scalar(v))
		}
	}
	return strings.Join(w, ", ")
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// --- workflow.pins ---

var (
	shaRe       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	dockerRe    = regexp.MustCompile(`^docker://[^@\s]+@sha256:[0-9a-f]{64}$`)
	versionCmRe = regexp.MustCompile(`^#\s*(v\d+\.\d+\.\d+\S*)`)
)

// pinProblem says why uses is not pinned to an immutable reference, or "".
func pinProblem(uses string) string {
	switch {
	case strings.HasPrefix(uses, "./"):
		return ""
	case strings.Contains(uses, "${{"):
		return "is an expression"
	case strings.HasPrefix(uses, "docker://"):
		if dockerRe.MatchString(uses) {
			return ""
		}
		return "is not pinned to an image digest (@sha256:...)"
	}
	_, ref, ok := strings.Cut(uses, "@")
	if !ok {
		return "has no @ref"
	}
	if !shaRe.MatchString(ref) {
		return "is not pinned to a full commit SHA"
	}
	return ""
}

func (c *checker) workflows() []*workflow {
	out := []*workflow{c.release}
	var keys []string
	for k := range c.ci {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, c.ci[k])
	}
	return out
}

func (c *checker) ciReadProblems() []string {
	var keys []string
	for k := range c.ciErr {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var p []string
	for _, k := range keys {
		p = append(p, fmt.Sprintf("cannot check %s, which the release workflow runs: %v", k, c.ciErr[k]))
	}
	return p
}

func (c *checker) checkPins() Result {
	var p []string
	type pin struct {
		where, sha, comment string
		release             bool // in the release workflow
	}
	var heph []pin
	n := 0
	for _, w := range c.workflows() {
		for _, j := range w.jobs {
			if j.uses != "" {
				n++
				if why := pinProblem(j.uses); why != "" {
					p = append(p, fmt.Sprintf("%s: job %s: uses %s %s", w.path, j.id, j.uses, why))
				}
			}
			for _, s := range j.steps {
				if s.uses == "" {
					continue
				}
				n++
				if why := pinProblem(s.uses); why != "" {
					p = append(p, fmt.Sprintf("%s: job %s step %s: uses %s %s", w.path, j.id, s.label(), s.uses, why))
				}
				if isHephaestusAction(s.uses) {
					_, ref, _ := strings.Cut(s.uses, "@")
					cm := ""
					if m := versionCmRe.FindStringSubmatch(strings.TrimSpace(s.usesComment)); m != nil {
						cm = m[1]
					}
					heph = append(heph, pin{fmt.Sprintf("%s: job %s step %s", w.path, j.id, s.label()), ref, cm, w == c.release})
				}
			}
		}
	}
	p = append(p, c.ciReadProblems()...)
	// The Hephaestus the release runs, which CI must run too: the
	// archives CI checks are then the ones the release publishes. A release
	// that runs hephaestus from source (no action) leaves CI free to test
	// other releases of the action.
	shas, comments := map[string]bool{}, map[string]bool{}
	for _, h := range heph {
		if h.comment == "" {
			p = append(p, fmt.Sprintf("%s: uses %s@%s without its release as a comment (# vX.Y.Z): the version it runs cannot be told from the workflow", h.where, hephaestusAction, h.sha))
		}
		if h.release {
			shas[h.sha] = true
			if h.comment != "" {
				comments[h.comment] = true
			}
		}
	}
	if len(shas) > 0 {
		for _, h := range heph {
			if !h.release && !shas[h.sha] {
				p = append(p, fmt.Sprintf("%s: uses %s@%s, the release workflow %s: CI must check what the release publishes with the same Hephaestus", h.where, hephaestusAction, h.sha, strings.Join(sortedKeys(shas), ", ")))
			} else if !h.release && h.comment != "" {
				comments[h.comment] = true
			}
		}
	}
	if len(shas) > 1 {
		p = append(p, fmt.Sprintf("%s runs Hephaestus at %d different commits (%s)", c.release.path, len(shas), strings.Join(sortedKeys(shas), ", ")))
	}
	if len(comments) > 1 {
		p = append(p, fmt.Sprintf("the workflows' Hephaestus pins name different releases (%s)", strings.Join(sortedKeys(comments), ", ")))
	}
	ev := map[string]any{"actions": n}
	msg := fmt.Sprintf("%d action references pinned to commit SHAs", n)
	if len(shas) > 0 {
		ev["hephaestus"] = sortedKeys(shas)
		msg += fmt.Sprintf("; Hephaestus %s at one commit", strings.Join(sortedKeys(comments), ", "))
	}
	return Result{Problems: p, Message: msg, Evidence: ev,
		Fix: "pin every action to a full commit SHA with its release as a comment (uses: owner/action@<40-hex SHA> # vX.Y.Z), the same Hephaestus commit everywhere"}
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- workflow.tap ---

var secretsWordRe = regexp.MustCompile(`\bsecrets\b`)

// secretUse classifies the expressions of a node for the tap token:
// whether it is referenced, whether its value is passed (anything but a
// comparison with ”), and whether secrets are indexed or serialized,
// which hides what is referenced (counted as referencing the token).
type secretUse struct {
	refs, value, opaque bool
}

func (c *checker) tapUse(n *yaml.Node) secretUse {
	var u secretUse
	name := regexp.QuoteMeta(c.o.TapTokenSecret)
	refRe := regexp.MustCompile(`(?i)\bsecrets\s*\.\s*` + name + `\b`)
	presenceRe := regexp.MustCompile(`(?i)^secrets\s*\.\s*` + name + `\s*(!=|==)\s*''$`)
	for _, e := range expressions(n) {
		for _, loc := range secretsWordRe.FindAllStringIndex(e, -1) {
			rest := strings.TrimLeft(e[loc[1]:], " \t")
			if !strings.HasPrefix(rest, ".") {
				u.opaque, u.refs, u.value = true, true, true
			}
		}
		if refRe.MatchString(e) {
			u.refs = true
			if !presenceRe.MatchString(e) {
				u.value = true
			}
		}
	}
	return u
}

// isPlanStep reports whether s computes the plan: the action's plan
// command, or a script running `hephaestus publish plan` into
// $GITHUB_OUTPUT.
func isPlanStep(s *step) bool {
	if isHephaestusAction(s.uses) {
		return s.with["command"] == "plan"
	}
	return strings.Contains(s.run, "publish plan") && strings.Contains(s.run, "GITHUB_OUTPUT")
}

func (c *checker) checkTap() Result {
	w := c.release
	var p []string
	secret := c.o.TapTokenSecret
	if u := c.tapUse(w.env); u.refs {
		p = append(p, fmt.Sprintf("%s: the workflow env passes %s to every step of every job", w.path, secret))
	}
	tokenSteps, refSteps := 0, 0
	named := map[string]*step{}
	for _, j := range w.jobs {
		if u := c.tapUse(j.env); u.refs {
			p = append(p, fmt.Sprintf("%s: job %s env passes %s to every step of the job", w.path, j.id, secret))
		}
		if j.secrets != nil && (scalar(j.secrets) == "inherit" || c.tapUse(j.secrets).refs) {
			p = append(p, fmt.Sprintf("%s: job %s passes %s to the workflow it calls (secrets: %s)", w.path, j.id, secret, orDefault(scalar(j.secrets), "...")))
		}
		plan := -1
		var plans []*step
		for i, s := range j.steps {
			if s.id == "plan" {
				plans = append(plans, s)
				if plan < 0 {
					plan = i
				}
			}
			if s.name == TapRehearsalStep || s.name == TapPublishStep {
				if named[s.name] != nil {
					p = append(p, fmt.Sprintf("%s: two steps are named %q; hephaestus publish verifies the tap by these names", w.path, s.name))
				}
				named[s.name] = s
			}
		}
		needsPlan := false
		for i, s := range j.steps {
			u := c.tapUse(s.node)
			readsPlan := slices.ContainsFunc(expressions(s.node), func(e string) bool { return strings.Contains(e, "steps.plan.") })
			if s.id == "plan" {
				if u.refs {
					p = append(p, fmt.Sprintf("%s: job %s: the plan step gets %s; it must run without secrets", w.path, j.id, secret))
				}
				continue
			}
			if readsPlan || u.refs {
				needsPlan = true
			}
			if readsPlan && (plan < 0 || plan > i) {
				p = append(p, fmt.Sprintf("%s: job %s step %s reads steps.plan, but no step with id: plan runs before it", w.path, j.id, s.label()))
			}
			if !u.refs {
				continue
			}
			refSteps++
			where := fmt.Sprintf("%s: job %s step %s", w.path, j.id, s.label())
			if u.opaque {
				p = append(p, where+": indexes or serializes secrets (secrets[...], toJSON(secrets)), so whether it gets the tap token cannot be told; name the secret")
			}
			if plan < 0 || plan > i {
				p = append(p, where+" uses "+secret+" before the plan step (id: plan) has decided whether the tap is published")
			}
			if got := normalizeCond(s.cond); got != TapCondition {
				p = append(p, fmt.Sprintf("%s uses %s with if: %q; want exactly if: %s, so that a prerelease (tap skipped) never gets the token", where, secret, s.cond, TapCondition))
			}
			if u.value {
				tokenSteps++
				if s.name != TapRehearsalStep && s.name != TapPublishStep {
					p = append(p, fmt.Sprintf("%s gets the value of %s; only the steps %q and %q may (hephaestus publish verifies the tap by these names)", where, secret, TapRehearsalStep, TapPublishStep))
				}
			}
		}
		if needsPlan || len(plans) > 0 {
			switch {
			case len(plans) == 0:
				// reported per step above
			case len(plans) > 1:
				p = append(p, fmt.Sprintf("%s: job %s has %d steps with id: plan", w.path, j.id, len(plans)))
			default:
				s := plans[0]
				where := fmt.Sprintf("%s: job %s plan step %s", w.path, j.id, s.label())
				if !isPlanStep(s) {
					p = append(p, where+" does not run the plan: use the Hephaestus action with command: plan (or hephaestus publish plan written to $GITHUB_OUTPUT)")
				}
				if s.cond != "" {
					p = append(p, fmt.Sprintf("%s has if: %q; it must always run, or the tap steps are skipped without a failure", where, s.cond))
				}
				if truthy(s.contOnErr) {
					p = append(p, where+" has continue-on-error: a failed plan would skip the tap steps without failing the run")
				}
			}
		}
	}
	for _, n := range []string{TapRehearsalStep, TapPublishStep} {
		s := named[n]
		switch {
		case s == nil && c.o.Formulae:
			p = append(p, fmt.Sprintf("%s: no step named %q: hephaestus publish verifies the tap by the names %q and %q, each with if: %s", w.path, n, TapRehearsalStep, TapPublishStep, TapCondition))
		case s != nil && normalizeCond(s.cond) != TapCondition:
			p = append(p, fmt.Sprintf("%s: step %q has if: %q; want exactly if: %s", w.path, n, s.cond, TapCondition))
		}
		if s != nil && n == TapRehearsalStep && !dryRun(s) {
			p = append(p, fmt.Sprintf("%s: step %q is not a dry run (dry-run: \"true\" / --dry-run): it would write to the tap before the release is published", w.path, n))
		}
	}
	msg := fmt.Sprintf("%s reaches %d planned steps (%d get its value), each with if: %s", secret, refSteps, tokenSteps, TapCondition)
	if refSteps == 0 && len(p) == 0 {
		msg = "no step uses " + secret + " (no formula is published to a tap)"
	}
	return Result{Problems: p, Message: msg, Evidence: map[string]any{"secret": secret, "steps": refSteps},
		Fix: "plan the release first (a step with id: plan running the action's command: plan, no if, no secrets), then pass the tap token only to the steps \"" + TapRehearsalStep + "\" and \"" + TapPublishStep + "\", each with if: " + TapCondition}
}

// --- workflow.attestation ---

func (c *checker) checkAttestation() Result {
	w := c.release
	if c.pubJob == nil {
		return Result{Problems: []string{c.pubErr}, Fix: "publish from one job, which attests every asset before the release step"}
	}
	j := c.pubJob
	firstPub := c.pubSteps[0].index
	var p []string
	var patterns []string
	attests := 0
	for _, s := range j.steps {
		repo, _, _ := strings.Cut(s.uses, "@")
		if repo != "actions/attest-build-provenance" && repo != "actions/attest" {
			continue
		}
		attests++
		where := fmt.Sprintf("%s: job %s step %s", w.path, j.id, s.label())
		if s.index > firstPub {
			p = append(p, fmt.Sprintf("%s attests after step %s publishes: a release could be public without its provenance", where, c.pubSteps[0].label()))
		}
		if cond := normalizeCond(s.cond); cond != "" && cond != "success()" {
			p = append(p, fmt.Sprintf("%s has if: %q: a skipped attestation passes the job", where, s.cond))
		}
		if truthy(s.contOnErr) {
			p = append(p, where+" has continue-on-error: a failed attestation would pass the job")
		}
		sp, ok := s.with["subject-path"]
		if !ok {
			p = append(p, where+" has no subject-path; the assets it covers cannot be checked (subject-digest and subject-checksums are not read)")
			continue
		}
		for _, f := range strings.FieldsFunc(sp, func(r rune) bool { return r == '\n' || r == ',' }) {
			if f = strings.TrimSpace(f); f == "" {
				continue
			}
			if strings.Contains(f, "${{") {
				p = append(p, fmt.Sprintf("%s: subject-path %q is an expression; the assets it covers cannot be checked", where, f))
				continue
			}
			patterns = append(patterns, f)
		}
	}
	if attests == 0 {
		p = append(p, fmt.Sprintf("%s: the publishing job %s has no actions/attest-build-provenance step: hephaestus publish verifies the provenance of every asset", w.path, j.id))
	}
	ev := map[string]any{"subject_paths": patterns}
	switch {
	case c.o.AssetsErr != nil:
		p = append(p, "the release asset names are not known, so the attestation cannot be checked: "+c.o.AssetsErr.Error())
	case len(c.o.Assets) == 0 && len(c.o.Unknown) == 0:
		p = append(p, "the configuration plans no release asset, so the attestation cannot be checked")
	}
	for _, u := range c.o.Unknown {
		p = append(p, "the attestation cannot be checked for an asset named only when it is built: "+u)
	}
	var missed []string
	for _, a := range c.o.Assets {
		if !slices.ContainsFunc(patterns, func(pat string) bool {
			ok, err := path.Match(path.Base(pat), a)
			return err == nil && ok
		}) {
			missed = append(missed, a)
		}
	}
	if len(missed) > 0 && attests > 0 {
		p = append(p, fmt.Sprintf("%s: subject-path (%s) does not cover %s", w.path, strings.Join(patterns, ", "), strings.Join(missed, ", ")))
	}
	ev["assets"] = len(c.o.Assets)
	return Result{Problems: p, Message: fmt.Sprintf("%d assets attested in job %s before it publishes", len(c.o.Assets), j.id), Evidence: ev,
		Fix: "attest every archive and the checksum file with actions/attest-build-provenance (subject-path), unconditionally, before the release step"}
}

// --- workflow.ci-gate ---

func (c *checker) checkCIGate() Result {
	var p []string
	p = append(p, c.ciReadProblems()...)
	if len(c.ci) == 0 && len(c.ciErr) == 0 {
		if c.pubJob == nil {
			p = append(p, c.pubErr)
		} else {
			p = append(p, fmt.Sprintf("%s: the publishing job %s needs no job that runs the CI workflow, so no ci-gate guards the release", c.release.path, c.pubJob.id))
		}
	}
	var checked []string
	for _, w := range c.workflows()[1:] {
		checked = append(checked, w.path)
		g := w.job(GateJob)
		if g == nil {
			p = append(p, fmt.Sprintf("%s: no job %s: the release branch's ruleset requires %s, which must need every other job", w.path, GateJob, GateJob))
			continue
		}
		if cond := normalizeCond(g.cond); cond != "always()" {
			p = append(p, fmt.Sprintf("%s: %s has if: %q; want if: always(), or a failed or cancelled job skips the gate, and a skipped required check passes", w.path, GateJob, g.cond))
		}
		var missing []string
		for _, o := range w.jobs {
			if o.id != GateJob && !slices.Contains(g.needs, o.id) {
				missing = append(missing, o.id)
			}
		}
		if len(missing) > 0 {
			p = append(p, fmt.Sprintf("%s: %s does not need %s: a failure there would not stop the release", w.path, GateJob, strings.Join(missing, ", ")))
		}
		readsNeeds := slices.ContainsFunc(g.steps, func(s *step) bool {
			return slices.ContainsFunc(expressions(s.node), func(e string) bool { return regexp.MustCompile(`\bneeds\b`).MatchString(e) })
		})
		if !readsNeeds {
			p = append(p, fmt.Sprintf("%s: no step of %s reads the needed jobs' results (e.g. ${{ join(needs.*.result, ' ') }}), so it cannot fail when one of them did not succeed", w.path, GateJob))
		}
		if truthy(g.contOnErr) || slices.ContainsFunc(g.steps, func(s *step) bool { return truthy(s.contOnErr) }) {
			p = append(p, fmt.Sprintf("%s: %s has continue-on-error", w.path, GateJob))
		}
	}
	return Result{Problems: p, Message: fmt.Sprintf("%s in %s needs every other job and fails unless all succeeded", GateJob, strings.Join(checked, ", ")),
		Evidence: map[string]any{"workflows": checked},
		Fix:      "give the CI workflow a job ci-gate with if: always(), needs: [every other job], failing unless each result is success"}
}
