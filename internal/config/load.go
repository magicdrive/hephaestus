package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// MaxConfigSize bounds the configuration file.
const MaxConfigSize = 1 << 20

// DefaultFiles are probed, in order, when no configuration path is given.
var DefaultFiles = []string{"hephaestus.yaml", "hephaestus.yml", ".hephaestus.yaml", ".hephaestus.yml"}

// Error is a configuration problem tied to a location.
type Error struct {
	Path string // e.g. "archives[0].files[2].dst"
	Line int    // 1-based; 0 when unknown
	Msg  string
}

func (e *Error) Error() string {
	switch {
	case e.Line > 0 && e.Path != "":
		return fmt.Sprintf("line %d: %s: %s", e.Line, e.Path, e.Msg)
	case e.Path != "":
		return fmt.Sprintf("%s: %s", e.Path, e.Msg)
	default:
		return e.Msg
	}
}

// LoadFile reads, defaults and validates the configuration at path.
func LoadFile(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Load(f, filepath.Dir(abs))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Find returns the first default configuration file present in dir.
func Find(dir string) (string, error) {
	for _, name := range DefaultFiles {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("no configuration file found in %s (looked for %s)", dir, strings.Join(DefaultFiles, ", "))
}

// Load parses a configuration document whose relative paths resolve against
// root.
func Load(r io.Reader, root string) (*Config, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxConfigSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxConfigSize {
		return nil, fmt.Errorf("configuration exceeds %d bytes", MaxConfigSize)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, err
	}
	cfg.Root = root
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Parse decodes YAML strictly without defaulting or validating. Unknown keys
// anywhere in the document are errors, reported with their line.
func Parse(data []byte) (*Config, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("configuration is empty")
		}
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("configuration must contain a single YAML document")
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, &Error{Line: root.Line, Msg: "configuration must be a mapping"}
	}
	if err := rejectAliases(root); err != nil {
		return nil, err
	}
	var errs []error
	checkKnown(root, reflect.TypeOf(Config{}), "", &errs)
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	var cfg Config
	if err := root.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	cfg.node = root
	if archives := mapValue(root, "archives"); archives != nil && archives.Kind == yaml.SequenceNode {
		for i, n := range archives.Content {
			if i < len(cfg.Archives) && mapValue(n, "files") != nil {
				cfg.Archives[i].filesSet = true
			}
		}
	}
	return &cfg, nil
}

// rejectAliases refuses YAML anchors/aliases: they enable "billion laughs"
// expansion and make it harder to see what a configuration does.
func rejectAliases(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode {
		return &Error{Line: n.Line, Msg: "YAML aliases are not supported"}
	}
	for _, c := range n.Content {
		if err := rejectAliases(c); err != nil {
			return err
		}
	}
	return nil
}

var (
	unmarshalerType = reflect.TypeOf((*yaml.Unmarshaler)(nil)).Elem()
	fileSpecType    = reflect.TypeOf(FileSpec{})
)

// checkKnown walks n alongside type t and records unknown mapping keys and
// duplicate keys.
func checkKnown(n *yaml.Node, t reflect.Type, path string, errs *[]error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == fileSpecType:
		if n.Kind == yaml.ScalarNode {
			return
		}
	case reflect.PointerTo(t).Implements(unmarshalerType):
		return // custom scalar-ish types validate themselves
	}
	switch t.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
				return
			}
			*errs = append(*errs, &Error{Path: path, Line: n.Line, Msg: "expected a mapping"})
			return
		}
		fields := yamlFields(t)
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if seen[k.Value] {
				*errs = append(*errs, &Error{Path: join(path, k.Value), Line: k.Line, Msg: "duplicate key"})
				continue
			}
			seen[k.Value] = true
			ft, ok := fields[k.Value]
			if !ok {
				*errs = append(*errs, &Error{Path: join(path, k.Value), Line: k.Line, Msg: "unknown field" + suggest(k.Value, fields)})
				continue
			}
			checkKnown(v, ft, join(path, k.Value), errs)
		}
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode {
			return // the decoder reports the type error
		}
		for i, c := range n.Content {
			checkKnown(c, t.Elem(), fmt.Sprintf("%s[%d]", path, i), errs)
		}
	case reflect.Map:
		if n.Kind != yaml.MappingNode {
			return
		}
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if seen[k.Value] {
				*errs = append(*errs, &Error{Path: join(path, k.Value), Line: k.Line, Msg: "duplicate key"})
			}
			seen[k.Value] = true
			checkKnown(n.Content[i+1], t.Elem(), join(path, k.Value), errs)
		}
	}
}

func yamlFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if tag == "" || tag == "-" || !f.IsExported() {
			continue
		}
		out[tag] = f.Type
	}
	return out
}

func suggest(key string, fields map[string]reflect.Type) string {
	best, bestD := "", 3
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if d := levenshtein(key, k); d < bestD {
			best, bestD = k, d
		}
	}
	if best != "" {
		return fmt.Sprintf(" (did you mean %q?)", best)
	}
	return ""
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func mapValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// lineOf finds the line of a dotted/indexed path such as
// "archives[0].files[2].dst"; 0 when it cannot be located.
func (c *Config) lineOf(path string) int {
	n := c.node
	if n == nil {
		return 0
	}
	line := n.Line
	for _, part := range strings.Split(path, ".") {
		key, idx := part, -1
		if i := strings.IndexByte(part, '['); i >= 0 {
			key = part[:i]
			_, _ = fmt.Sscanf(part[i:], "[%d]", &idx) // on parse failure idx stays -1
		}
		if key != "" {
			v := mapValue(n, key)
			if v == nil {
				return line
			}
			n = v
			line = v.Line
		}
		if idx >= 0 {
			if n.Kind != yaml.SequenceNode || idx >= len(n.Content) {
				return line
			}
			n = n.Content[idx]
			line = n.Line
		}
	}
	return line
}

// errAt builds a located error.
func (c *Config) errAt(path, format string, args ...any) error {
	return &Error{Path: path, Line: c.lineOf(path), Msg: fmt.Sprintf(format, args...)}
}

func decodeStrict(n *yaml.Node, v any) error { return n.Decode(v) }
