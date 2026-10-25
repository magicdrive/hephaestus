package homebrew

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/magicdrive/hephaestus/internal/config"
	"github.com/magicdrive/hephaestus/internal/manifest"
)

// completionTargets are the names Homebrew expects in each completion
// directory for a command (generate_completions_from_executable uses the
// same names).
func completionTargets(exe string) map[string]string {
	return map[string]string{"bash": exe, "zsh": "_" + exe, "fish": exe + ".fish"}
}

func completionExecutable(b *config.Brew) string {
	if b.Completions.Executable != "" {
		return b.Completions.Executable
	}
	return b.Name
}

// homebrewDefaultShells mirrors Utils::ShellCompletion.default_completion_shells:
// the shells generate_completions_from_executable uses without shells:.
func homebrewDefaultShells(format string) []string {
	if format == "cobra" || format == "typer" {
		return []string{"bash", "zsh", "fish", "pwsh"}
	}
	return config.CompletionShells
}

// completionStatements returns the install statements for
// brews[].completions, one per line, or nil when it is not set:
//
//   - files: <shell>_completion.install "path" => "name" for each shipped
//     script, so Homebrew's own path helpers place it (bash_completion,
//     zsh_completion, fish_completion);
//   - generate: one generate_completions_from_executable call (Homebrew
//     runs the installed executable once per shell). shells: is written
//     only when it differs from Homebrew's default for the format, as brew
//     style requires.
//
// Every value is a quoted Ruby literal; nothing comes from a template.
func completionStatements(b *config.Brew) []string {
	c := b.Completions
	if c == nil {
		return nil
	}
	exe := completionExecutable(b)
	targets := completionTargets(exe)
	files := c.Files.ByShell()
	var lines []string
	for _, sh := range config.CompletionShells {
		f, ok := files[sh]
		if !ok {
			continue
		}
		l := sh + "_completion.install " + mustQuote(f)
		if path.Base(f) != targets[sh] {
			l += " => " + mustQuote(targets[sh])
		}
		lines = append(lines, l)
	}
	if g := c.Generate; g != nil {
		lines = append(lines, generateCall+"("+strings.Join(generateArgs(exe, g), ", ")+")")
	}
	return lines
}

const generateCall = "generate_completions_from_executable"

func generateArgs(exe string, g *config.BrewCompletionGenerate) []string {
	args := []string{"bin/" + mustQuote(exe)}
	for _, a := range g.Args {
		args = append(args, mustQuote(a))
	}
	if g.ShellParameterFormat != "" {
		args = append(args, "shell_parameter_format: :"+g.ShellParameterFormat)
	}
	var shells []string
	for _, sh := range config.CompletionShells { // canonical order
		if slices.Contains(g.GeneratedShells(), sh) {
			shells = append(shells, sh)
		}
	}
	if def := homebrewDefaultShells(g.ShellParameterFormat); !slices.Equal(shells, def) {
		syms := make([]string, len(shells))
		for i, sh := range shells {
			syms[i] = ":" + sh
		}
		args = append(args, "shells: ["+strings.Join(syms, ", ")+"]")
	}
	return args
}

// maxFormulaLine is brew style's Layout/LineLength limit.
const maxFormulaLine = 118

// completionCode returns completionStatements as install code ("" when
// unset). A generate call longer than brew style allows (the install body
// is indented by 4) is wrapped, continuation lines aligned with the first
// argument: positional arguments fill lines, then the keyword arguments
// share one line, or take one line each, so that their keys stay aligned
// (Layout/HashAlignment).
func completionCode(b *config.Brew) string {
	stmts := completionStatements(b)
	if len(stmts) == 0 {
		return ""
	}
	if g := b.Completions.Generate; g != nil && len("    "+stmts[len(stmts)-1]) > maxFormulaLine {
		stmts[len(stmts)-1] = wrapCall(generateArgs(completionExecutable(b), g))
	}
	return strings.Join(stmts, "\n")
}

func wrapCall(args []string) string {
	align := strings.Repeat(" ", len(generateCall)+1)
	fits := func(l string) bool { return len("    "+l) <= maxFormulaLine }
	var pos, kw []string
	for _, a := range args {
		if strings.Contains(a, ": ") && !strings.HasPrefix(a, `"`) {
			kw = append(kw, a)
		} else {
			pos = append(pos, a)
		}
	}
	var lines []string
	cur := generateCall + "(" + pos[0]
	for _, a := range pos[1:] {
		if fits(cur + ", " + a + ",") {
			cur += ", " + a
			continue
		}
		lines = append(lines, cur+",")
		cur = align + a
	}
	if len(kw) > 0 {
		lines = append(lines, cur+",")
		if one := align + strings.Join(kw, ", ") + ")"; fits(one) {
			cur = one
		} else {
			for _, k := range kw[:len(kw)-1] {
				lines = append(lines, align+k+",")
			}
			cur = align + kw[len(kw)-1] + ")"
		}
	} else {
		cur += ")"
	}
	return strings.Join(append(lines, cur), "\n")
}

// CompletionLines returns the install lines brews[].completions adds to the
// formula (none when it is unset), for verifying a published formula.
func CompletionLines(b *config.Brew) []string {
	return completionStatements(b)
}

// checkCompletions verifies brews[].completions against the formula's
// install code: the executable that generates the scripts must be
// installed into bin for every archive, and a generated shell must not
// also be installed by install or extra_install (Homebrew would overwrite
// one with the other). Shipped files are verified with the other install
// paths (verifyInstall).
func checkCompletions(b *config.Brew, installBody string, archives []manifest.Artifact) error {
	c := b.Completions
	if c == nil || c.Generate == nil {
		return nil
	}
	exe := completionExecutable(b)
	if n := strings.Count(installBody, generateCall); n > 1 {
		return fmt.Errorf("completions: install calls %s %d times (completions.generate writes one call); keep one", generateCall, n)
	}
	calls := analyzeInstall(installBody)
	generated := c.Generate.GeneratedShells()
	targets := completionTargets(exe)
	for _, call := range calls {
		sh, ok := strings.CutSuffix(call.receiver, "_completion")
		if !ok || !slices.Contains(generated, sh) || call.reason != "" {
			continue
		}
		for _, s := range call.srcs {
			name := path.Base(s)
			if d, ok := call.dests[s]; ok {
				name = d
			}
			if name == targets[sh] {
				return fmt.Errorf("completions: the %s completion %q is generated (completions.generate) and also installed by %s_completion.install on install line %d; keep one", sh, name, sh, call.line)
			}
		}
	}
	// bin.install_symlink, write_exec_script and the like also put a
	// command into bin; they are not analyzed, so the check is skipped.
	other := otherBinWriteRe.MatchString(withoutGenerateCall(installBody))
	for _, a := range archives {
		found, unknown := false, other
		for _, call := range calls {
			if call.receiver != "bin" {
				continue
			}
			if call.reason != "" {
				unknown = true
				continue
			}
			if !appliesTo(call.platforms, a) {
				continue
			}
			for _, s := range call.srcs {
				name := path.Base(s)
				if d, ok := call.dests[s]; ok {
					name = d
				}
				if name == exe {
					found = true
				}
			}
		}
		if !found && !unknown {
			return fmt.Errorf("completions: generate runs bin/%q, which the formula does not install for archive %s (set completions.executable to the installed command)", exe, a.Name)
		}
	}
	return nil
}

// otherBinWriteRe finds install code that puts a command into bin other
// than bin.install: install_symlink, write_*_script, or a bin/"..." path.
var otherBinWriteRe = regexp.MustCompile(`\bbin\.(install_symlink|write_exec_script|write_jar_script|write_env_script)\b|\bbin\s*/`)

// withoutGenerateCall drops the generate_completions_from_executable line
// (its bin/"exe" is not an install).
func withoutGenerateCall(body string) string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), generateCall+"(") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
