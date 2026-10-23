// Package ghaction writes GitHub Actions step outputs and the step summary.
package ghaction

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// MaxSummary is GitHub's per-step summary limit.
const MaxSummary = 1 << 20

var outputNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)

// WriteOutputs appends outputs to the file named by $GITHUB_OUTPUT using the
// heredoc syntax with a random delimiter, so values cannot inject further
// outputs. It is a no-op when the variable is unset.
func WriteOutputs(outputs map[string]string) error {
	path := os.Getenv("GITHUB_OUTPUT")
	if path == "" {
		return nil
	}
	return writeOutputs(path, outputs)
}

func writeOutputs(path string, outputs map[string]string) error {
	keys := make([]string, 0, len(outputs))
	for k := range outputs {
		if !outputNameRe.MatchString(k) {
			return fmt.Errorf("invalid output name %q", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		delim, err := delimiter(outputs[k])
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s<<%s\n%s\n%s\n", k, delim, outputs[k], delim)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644) //nolint:gosec // G703: path is the file GitHub Actions provides via $GITHUB_OUTPUT/$GITHUB_STEP_SUMMARY
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close() // already failing; the original error takes precedence
		return err
	}
	return f.Close()
}

func delimiter(value string) (string, error) {
	for {
		var buf [16]byte
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		d := "ghadelimiter_" + hex.EncodeToString(buf[:])
		if !strings.Contains(value, d) {
			return d, nil
		}
	}
}

// AppendSummary appends Markdown to $GITHUB_STEP_SUMMARY, truncating to
// GitHub's limit. It is a no-op when the variable is unset.
func AppendSummary(markdown string) error {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return nil
	}
	if len(markdown) > MaxSummary {
		markdown = markdown[:MaxSummary-64] + "\n\n_(summary truncated)_\n"
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644) //nolint:gosec // G703: path is the file GitHub Actions provides via $GITHUB_OUTPUT/$GITHUB_STEP_SUMMARY
	if err != nil {
		return err
	}
	if _, err := f.WriteString(markdown); err != nil {
		_ = f.Close() // already failing; the original error takes precedence
		return err
	}
	return f.Close()
}

// MaskValue asks the runner to mask a secret in logs.
func MaskValue(v string) {
	if v != "" && os.Getenv("GITHUB_ACTIONS") == "true" {
		for _, line := range strings.Split(v, "\n") {
			if line != "" {
				fmt.Fprintf(os.Stdout, "::add-mask::%s\n", Escape(line))
			}
		}
	}
}

// Warning emits a warning annotation in GitHub Actions, shown on the
// workflow run page. It is a no-op outside GitHub Actions.
func Warning(msg string) {
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		fmt.Fprintf(os.Stdout, "::warning::%s\n", Escape(msg))
	}
}

// Escape escapes a workflow command value.
func Escape(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}

// Table renders a Markdown table, escaping cell content.
func Table(header []string, rows [][]string) string {
	var b strings.Builder
	cell := func(s string) string {
		s = strings.NewReplacer("|", `\|`, "\n", " ", "\r", " ", "<", "&lt;", ">", "&gt;").Replace(s)
		return s
	}
	b.WriteString("|")
	for _, h := range header {
		b.WriteString(" " + cell(h) + " |")
	}
	b.WriteString("\n|")
	for range header {
		b.WriteString(" --- |")
	}
	b.WriteString("\n")
	for _, r := range rows {
		b.WriteString("|")
		for _, c := range r {
			b.WriteString(" " + cell(c) + " |")
		}
		b.WriteString("\n")
	}
	return b.String()
}
