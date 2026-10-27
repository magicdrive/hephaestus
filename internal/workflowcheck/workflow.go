package workflowcheck

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// workflow is the part of a GitHub Actions workflow file the checks read.
type workflow struct {
	path        string
	on          *yaml.Node
	permissions *yaml.Node
	env         *yaml.Node
	jobs        []*job // in file order
}

type job struct {
	id          string
	node        *yaml.Node
	needs       []string
	cond        string // if
	uses        string // reusable workflow call
	usesNode    *yaml.Node
	environment string
	hasEnv      bool // environment key present
	env         *yaml.Node
	secrets     *yaml.Node // secrets passed to a reusable workflow
	permissions *yaml.Node
	contOnErr   *yaml.Node
	steps       []*step
}

type step struct {
	index    int
	node     *yaml.Node
	id, name string
	cond     string
	uses     string
	usesNode *yaml.Node
	// usesComment is the line comment of `uses:` (e.g. "# v1.3.0").
	usesComment string
	run         string
	with        map[string]string
	env         *yaml.Node
	contOnErr   *yaml.Node
}

// label names a step in messages: its name, else its id, uses or position.
func (s *step) label() string {
	switch {
	case s.name != "":
		return fmt.Sprintf("%q", s.name)
	case s.id != "":
		return "id " + s.id
	case s.uses != "":
		return "uses " + s.uses
	}
	return fmt.Sprintf("#%d", s.index+1)
}

// parseWorkflow parses a workflow file. Anything the checks cannot read
// unambiguously (a non-mapping job, steps that are not a list, an anchor
// or alias) is an error: the checks fail closed rather than guess.
func parseWorkflow(path string, src []byte) (*workflow, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: not a workflow (a YAML mapping)", path)
	}
	if err := noAliases(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	w := &workflow{path: path}
	root := doc.Content[0]
	var jobs *yaml.Node
	for k, v := range pairs(root) {
		switch k {
		case "on", "true": // YAML 1.1 readers turn `on` into true; yaml.v3 does not, but be safe.
			w.on = v
		case "permissions":
			w.permissions = v
		case "env":
			w.env = v
		case "jobs":
			jobs = v
		}
	}
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: no jobs mapping", path)
	}
	for id, jn := range pairs(jobs) {
		j, err := parseJob(id, jn)
		if err != nil {
			return nil, fmt.Errorf("%s: job %s: %w", path, id, err)
		}
		w.jobs = append(w.jobs, j)
	}
	return w, nil
}

func parseJob(id string, n *yaml.Node) (*job, error) {
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("not a mapping")
	}
	j := &job{id: id, node: n}
	for k, v := range pairs(n) {
		switch k {
		case "needs":
			switch v.Kind {
			case yaml.ScalarNode:
				j.needs = []string{v.Value}
			case yaml.SequenceNode:
				for _, e := range v.Content {
					if e.Kind != yaml.ScalarNode {
						return nil, fmt.Errorf("needs: not a list of job IDs")
					}
					j.needs = append(j.needs, e.Value)
				}
			default:
				return nil, fmt.Errorf("needs: not a job ID or a list of them")
			}
		case "if":
			j.cond = scalar(v)
		case "uses":
			j.uses, j.usesNode = scalar(v), v
		case "environment":
			j.hasEnv = true
			switch v.Kind {
			case yaml.ScalarNode:
				j.environment = v.Value
			case yaml.MappingNode:
				for ek, ev := range pairs(v) {
					if ek == "name" {
						j.environment = scalar(ev)
					}
				}
			}
		case "env":
			j.env = v
		case "secrets":
			j.secrets = v
		case "permissions":
			j.permissions = v
		case "continue-on-error":
			j.contOnErr = v
		case "steps":
			if v.Kind != yaml.SequenceNode {
				return nil, fmt.Errorf("steps: not a list")
			}
			for i, sn := range v.Content {
				s, err := parseStep(i, sn)
				if err != nil {
					return nil, fmt.Errorf("step #%d: %w", i+1, err)
				}
				j.steps = append(j.steps, s)
			}
		}
	}
	return j, nil
}

func parseStep(i int, n *yaml.Node) (*step, error) {
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("not a mapping")
	}
	s := &step{index: i, node: n, with: map[string]string{}}
	for k := 0; k+1 < len(n.Content); k += 2 {
		if n.Content[k].Value == "uses" {
			s.usesComment = n.Content[k].LineComment + n.Content[k+1].LineComment
		}
	}
	for k, v := range pairs(n) {
		switch k {
		case "id":
			s.id = scalar(v)
		case "name":
			s.name = scalar(v)
		case "if":
			s.cond = scalar(v)
		case "uses":
			s.uses, s.usesNode = scalar(v), v
		case "run":
			s.run = scalar(v)
		case "env":
			s.env = v
		case "continue-on-error":
			s.contOnErr = v
		case "with":
			if v.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("with: not a mapping")
			}
			for wk, wv := range pairs(v) {
				if wv.Kind != yaml.ScalarNode {
					return nil, fmt.Errorf("with.%s: not a scalar", wk)
				}
				s.with[wk] = wv.Value
			}
		}
	}
	return s, nil
}

// pairs iterates over a mapping node's key/value pairs, in order.
func pairs(n *yaml.Node) func(yield func(string, *yaml.Node) bool) {
	return func(yield func(string, *yaml.Node) bool) {
		if n == nil || n.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if !yield(n.Content[i].Value, n.Content[i+1]) {
				return
			}
		}
	}
}

func scalar(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// noAliases rejects anchors, aliases and merge keys: what a step really
// contains would then depend on YAML features GitHub resolves its own way.
func noAliases(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return fmt.Errorf("line %d: YAML anchors and aliases are not supported by the workflow checks", n.Line)
	}
	for _, c := range n.Content {
		if err := noAliases(c); err != nil {
			return err
		}
	}
	return nil
}

// scalars returns every scalar value under n (keys included), the text
// GitHub may evaluate expressions in.
func scalars(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}
	var out []string
	for _, c := range n.Content {
		out = append(out, scalars(c)...)
	}
	return out
}

var exprRe = regexp.MustCompile(`\$\{\{(.*?)\}\}`)

// expressions returns the expressions of a node: every ${{ ... }} in its
// values, plus the value of each `if` key (an implicit expression).
func expressions(n *yaml.Node) []string {
	var out []string
	for _, v := range scalars(n) {
		for _, m := range exprRe.FindAllStringSubmatch(v, -1) {
			out = append(out, strings.TrimSpace(m[1]))
		}
	}
	var conds func(*yaml.Node)
	conds = func(n *yaml.Node) {
		if n == nil {
			return
		}
		if n.Kind == yaml.MappingNode {
			for k, v := range pairs(n) {
				if k == "if" && v.Kind == yaml.ScalarNode && !strings.Contains(v.Value, "${{") {
					out = append(out, strings.TrimSpace(v.Value))
				}
			}
		}
		for _, c := range n.Content {
			conds(c)
		}
	}
	conds(n)
	return out
}

// normalizeCond turns an `if` into a comparable expression: without a
// ${{ }} wrapper, with single spaces.
func normalizeCond(c string) string {
	c = strings.TrimSpace(c)
	if strings.HasPrefix(c, "${{") && strings.HasSuffix(c, "}}") && strings.Count(c, "${{") == 1 {
		c = strings.TrimSpace(c[3 : len(c)-2])
	}
	return strings.Join(strings.Fields(c), " ")
}

// truthy reports whether a continue-on-error value is (or may be) true.
func truthy(n *yaml.Node) bool {
	if n == nil {
		return false
	}
	return !(n.Kind == yaml.ScalarNode && n.Value == "false")
}

func (w *workflow) job(id string) *job {
	i := slices.IndexFunc(w.jobs, func(j *job) bool { return j.id == id })
	if i < 0 {
		return nil
	}
	return w.jobs[i]
}

// ancestors returns the jobs id needs, directly or transitively.
func (w *workflow) ancestors(id string) []string {
	var out []string
	var walk func(string)
	walk = func(id string) {
		j := w.job(id)
		if j == nil {
			return
		}
		for _, n := range j.needs {
			if !slices.Contains(out, n) {
				out = append(out, n)
				walk(n)
			}
		}
	}
	walk(id)
	return out
}
