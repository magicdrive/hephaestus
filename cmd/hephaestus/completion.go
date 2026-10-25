package hephaestus

import (
	_ "embed"
	"flag"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/magicdrive/hephaestus/internal/pipeline"
	"github.com/magicdrive/hephaestus/internal/preflight"
)

// command is one top-level command. The usage text and shell completion are
// both built from commands, and each command's flags come from the same
// constructor its parser uses, so completion cannot offer a command or flag
// the CLI does not accept.
type command struct {
	name, summary string
	// flags returns the command's flag set (nil: the command takes no
	// flags).
	flags func() *flag.FlagSet
	// args are the values of the command's only positional argument.
	args []string
	// subcommands are offered as the first word after the command (before
	// its flags).
	subcommands func() []candidate
}

// commands in usage order.
var commands = []command{
	{name: "init", summary: "Write a starting hephaestus.yaml (--template go|rust|script|prebuilt)", flags: func() *flag.FlagSet { return new(initArgs).flags() }},
	{name: "check", summary: "Validate the configuration and print the resolved plan", flags: stageFlags("check")},
	{name: "run", summary: "Run stages (default: every configured stage)", flags: stageFlags("run")},
	{name: "build", summary: "Run the build stage only", flags: stageFlags("build")},
	{name: "archive", summary: "Run the archive stage only", flags: stageFlags("archive")},
	{name: "checksum", summary: "Run the checksum stage only", flags: stageFlags("checksum")},
	{name: "release", summary: "Run the GitHub release stage only", flags: stageFlags("release")},
	{name: "formula", summary: "Run the Homebrew formula stage only", flags: stageFlags("formula")},
	{name: "tap", summary: "Run the Homebrew tap publication stage only", flags: stageFlags("tap")},
	{name: "preflight", summary: "Read-only release safety checks before pushing a tag (--tag vX.Y.Z)", flags: func() *flag.FlagSet { return new(preflightArgs).flags() }},
	{name: "status", summary: "Read-only state of a tag's release and the next safe action (--tag vX.Y.Z)", flags: func() *flag.FlagSet { return new(statusArgs).flags() }},
	{name: "publish", summary: "Release a version: check, tag, push, follow the workflow, verify (vX.Y.Z)", flags: publishFlags, subcommands: publishSubcommands},
	{name: "completion", summary: "Print the shell completion script (bash, zsh or fish)", args: completionShells},
	{name: "version", summary: "Print the version"},
	{name: "help", summary: "Print this help"},
}

func stageFlags(cmd string) func() *flag.FlagSet {
	return func() *flag.FlagSet {
		var stages, phase string
		return runFlags(cmd, &Options{}, &stages, &phase)
	}
}

// usage is the top-level help, listing commands.
var usage = func() string {
	var b strings.Builder
	b.WriteString("hephaestus — forge release artifacts into reliable distributions\n\nUsage:\n  hephaestus <command> [flags]\n\nCommands:\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  %-11s %s\n", c.name, c.summary)
	}
	b.WriteString("\nRun \"hephaestus <command> -h\" for flags.\n")
	return b.String()
}()

// outputFormats are the values of preflight's and status's --format.
var outputFormats = []string{"text", "json"}

// completionShells are the shells `hephaestus completion` writes a script
// for.
var completionShells = []string{"bash", "zsh", "fish"}

// flagValues describes the values a flag takes, for completion. A flag that
// is not listed takes a free-form value (a tag, a repository, a name), which
// is not completed.
type flagValues struct {
	values func() []string // fixed choices
	list   bool            // comma-separated list of values
	file   bool            // a path: completed by the shell
}

var completionValues = map[string]flagValues{
	"config":   {file: true},
	"report":   {file: true},
	"output":   {file: true},
	"format":   {values: func() []string { return outputFormats }},
	"template": {values: func() []string { return slices.Sorted(maps.Keys(initTemplates)) }},
	"phase":    {values: func() []string { return []string{phaseLocal, phasePublish} }},
	"stages": {list: true, values: func() []string {
		var out []string
		for _, s := range pipeline.Order {
			out = append(out, string(s))
		}
		return append(out, "all")
	}},
	"skip": {list: true, values: preflight.Checks},
}

// Completion directives, the last line of `hephaestus __complete` output.
const (
	directiveDefault = 0 // the candidates above, nothing else
	directiveFile    = 2 // complete a path (the value after "=" for --flag=value)
)

//go:embed completions/hephaestus.bash
var bashCompletion string

//go:embed completions/hephaestus.zsh
var zshCompletion string

//go:embed completions/hephaestus.fish
var fishCompletion string

// completionCmd runs `hephaestus completion SHELL`: it prints the script
// and nothing else on stdout. It reads no configuration, environment,
// repository or network.
func completionCmd(args []string, stdout, stderr io.Writer) int {
	const use = "usage: hephaestus completion bash|zsh|fish"
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "-help") {
		fmt.Fprintf(stdout, "%s\n\nPrint the completion script for a shell. Load it with:\n  bash:  source <(hephaestus completion bash)\n  zsh:   source <(hephaestus completion zsh)\n  fish:  hephaestus completion fish | source\n", use)
		return 0
	}
	if len(args) != 1 {
		fmt.Fprintln(stderr, use)
		return 2
	}
	switch args[0] {
	case "bash":
		fmt.Fprint(stdout, bashCompletion)
	case "zsh":
		fmt.Fprint(stdout, zshCompletion)
	case "fish":
		fmt.Fprint(stdout, fishCompletion)
	default:
		fmt.Fprintf(stderr, "hephaestus completion: unknown shell %q (bash, zsh or fish)\n", args[0])
		return 2
	}
	return 0
}

// candidate is one completion: the word and an optional description.
type candidate struct{ word, desc string }

// completeCmd runs the hidden `hephaestus __complete WORD...`, called by the
// completion scripts. The words are the command line after "hephaestus" up
// to the cursor; the last one is the word being completed (possibly
// empty). It prints one candidate per line ("word<TAB>description") and a
// last line ":<directive>". It only reads the flag definitions.
func completeCmd(args []string, stdout io.Writer) int {
	cands, directive := complete(args)
	for _, c := range cands {
		if c.desc != "" {
			fmt.Fprintf(stdout, "%s\t%s\n", c.word, c.desc)
		} else {
			fmt.Fprintln(stdout, c.word)
		}
	}
	fmt.Fprintf(stdout, ":%d\n", directive)
	return 0
}

// complete computes the candidates for words (see completeCmd).
func complete(words []string) ([]candidate, int) {
	if len(words) == 0 {
		words = []string{""}
	}
	cur, before := words[len(words)-1], words[:len(words)-1]
	if len(before) == 0 {
		if strings.HasPrefix(cur, "-") {
			return filter([]candidate{{"--help", "Print this help"}, {"--version", "Print the version"}}, cur), directiveDefault
		}
		var out []candidate
		for _, c := range commands {
			out = append(out, candidate{c.name, c.summary})
		}
		return filter(out, cur), directiveDefault
	}
	i := slices.IndexFunc(commands, func(c command) bool { return c.name == before[0] })
	if i < 0 {
		return nil, directiveDefault
	}
	cmd := commands[i]
	if cmd.flags == nil {
		if len(before) == 1 && !strings.HasPrefix(cur, "-") {
			var out []candidate
			for _, a := range cmd.args {
				out = append(out, candidate{word: a})
			}
			return filter(out, cur), directiveDefault
		}
		return nil, directiveDefault
	}
	if cmd.subcommands != nil && len(before) == 1 && !strings.HasPrefix(cur, "-") {
		return filter(cmd.subcommands(), cur), directiveDefault
	}
	fs := cmd.flags()

	// Walk the words before the cursor: which flags are set, and whether
	// the last one still waits for its value.
	seen := map[string]bool{}
	var pending *flag.Flag
	for _, w := range before[1:] {
		if pending != nil {
			pending = nil
			continue
		}
		if w == "--" {
			return nil, directiveDefault // no positional arguments
		}
		name, _, hasValue := flagName(w)
		if name == "" {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		seen[name] = true
		if !hasValue && !isBool(f) {
			pending = f
		}
	}
	if pending != nil {
		return values(pending.Name, "", cur)
	}
	if name, value, hasValue := flagName(cur); name != "" && hasValue {
		f := fs.Lookup(name)
		if f == nil || isBool(f) {
			return nil, directiveDefault
		}
		return values(name, cur[:len(cur)-len(value)], value)
	}
	if cur != "" && !strings.HasPrefix(cur, "-") {
		return nil, directiveDefault // no completed positional arguments
	}
	dash := "--"
	if strings.HasPrefix(cur, "-") && !strings.HasPrefix(cur, "--") && len(cur) > 1 {
		dash = "-" // keep the single-dash form the user started with
	}
	var out []candidate
	fs.VisitAll(func(f *flag.Flag) {
		if seen[f.Name] && !repeatable(f) {
			return
		}
		out = append(out, candidate{dash + f.Name, firstLine(f.Usage)})
	})
	out = append(out, candidate{dash + "help", "Print the flags"})
	return filter(out, cur), directiveDefault
}

// values completes the value of flag name: prefix is prepended to every
// candidate ("--flag=" for the --flag=value form), partial is what has been
// typed of the value.
func values(name, prefix, partial string) ([]candidate, int) {
	v, ok := completionValues[name]
	switch {
	case !ok:
		return nil, directiveDefault
	case v.file:
		return nil, directiveFile
	}
	head, last := "", partial
	if v.list {
		if i := strings.LastIndex(partial, ","); i >= 0 {
			head, last = partial[:i+1], partial[i+1:]
		}
	}
	used := map[string]bool{}
	for _, x := range strings.Split(head, ",") {
		used[x] = true
	}
	if used["all"] {
		return nil, directiveDefault // "all" stands alone
	}
	var out []candidate
	for _, x := range v.values() {
		if x == "all" && head != "" {
			continue // "all" stands alone
		}
		if !used[x] && strings.HasPrefix(x, last) {
			out = append(out, candidate{word: prefix + head + x})
		}
	}
	return out, directiveDefault
}

// flagName splits a flag word: "--name=value" -> name, value, true;
// "--name" or "-name" -> name, "", false; anything else -> "".
func flagName(w string) (name, value string, hasValue bool) {
	if len(w) < 2 || w[0] != '-' || w == "--" {
		return "", "", false
	}
	n := strings.TrimPrefix(strings.TrimPrefix(w, "-"), "-")
	if n == "" || n[0] == '-' || n[0] == '=' {
		return "", "", false
	}
	if i := strings.IndexByte(n, '='); i >= 0 {
		return n[:i], n[i+1:], true
	}
	return n, "", false
}

func isBool(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// repeatable reports whether f may be given more than once with effect
// (stringList accumulates; other flags keep the last value).
func repeatable(f *flag.Flag) bool {
	_, ok := f.Value.(*stringList)
	return ok
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

func filter(cands []candidate, prefix string) []candidate {
	return slices.DeleteFunc(cands, func(c candidate) bool { return !strings.HasPrefix(c.word, prefix) })
}
