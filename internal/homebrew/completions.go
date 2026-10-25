package homebrew

import (
	"fmt"
	"path"
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

// completionCode returns the install lines for brews[].completions, or ""
// when it is not set:
//
//   - files: <shell>_completion.install "path" => "name" for each shipped
//     script, so Homebrew's own path helpers place it (bash_completion,
//     zsh_completion, fish_completion);
//   - generate: one generate_completions_from_executable call (Homebrew
//     runs the installed executable, offline, once per shell). shells: is
//     written only when it differs from Homebrew's default for the format,
//     as brew style requires.
//
// Every value is a quoted Ruby literal; nothing comes from a template.
func completionCode(b *config.Brew) string {
	c := b.Completions
	if c == nil {
		return ""
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
		lines = append(lines, "generate_completions_from_executable("+strings.Join(args, ", ")+")")
	}
	return strings.Join(lines, "\n")
}

// CompletionLines returns the install lines brews[].completions adds to the
// formula (none when it is unset), for verifying a published formula.
func CompletionLines(b *config.Brew) []string {
	s := completionCode(b)
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
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
	calls := analyzeInstall(installBody)
	generated := c.Generate.GeneratedShells()
	for _, call := range calls {
		if sh, ok := strings.CutSuffix(call.receiver, "_completion"); ok && slices.Contains(generated, sh) {
			return fmt.Errorf("completions: the %s completion is generated (completions.generate) and also installed by %s_completion.install on install line %d; keep one", sh, sh, call.line)
		}
	}
	for _, a := range archives {
		found, unknown := false, false
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
