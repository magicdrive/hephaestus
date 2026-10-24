package preflight

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// WriteJSON writes the report as one JSON document (and nothing else).
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// WriteText writes the report for people.
func (r *Report) WriteText(w io.Writer) {
	kind := ""
	if r.Prerelease != nil {
		kind = " (stable)"
		if *r.Prerelease {
			kind = " (prerelease)"
		}
	}
	repo := r.Repository
	if repo == "" {
		repo = "unknown repository"
	}
	commit := ""
	if r.Commit != "" {
		commit = " at " + shortSHA(r.Commit)
	}
	fmt.Fprintf(w, "Release preflight: %s %s%s%s\n\n", repo, r.Tag, kind, commit)
	width := 0
	for _, c := range r.Checks {
		width = max(width, len(c.ID))
	}
	for _, c := range r.Checks {
		fmt.Fprintf(w, "  %-7s %-*s  %s\n", c.Status, width, c.ID, c.Message)
		if c.Remediation != "" && c.Status != Pass && c.Status != Skip {
			fmt.Fprintf(w, "  %-7s %-*s  → %s\n", "", width, "", c.Remediation)
		}
	}
	var parts []string
	for _, s := range []Status{Fail, Unknown, Warn, Pass, Skip} {
		if n := r.Summary[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, strings.ToLower(string(s))))
		}
	}
	mode := ""
	if r.Strict {
		mode = ", strict"
	}
	fmt.Fprintf(w, "\nResult: %s (%s%s)\n", strings.ToUpper(r.Status), strings.Join(parts, ", "), mode)
	switch r.ExitCode() {
	case ExitPass:
		fmt.Fprintf(w, "Nothing blocks tagging %s.\n", r.Tag)
	case ExitIncomplete:
		fmt.Fprintln(w, "Some checks could not be made (UNKNOWN); resolve them or run with --strict to treat them as failures.")
	default:
		fmt.Fprintf(w, "Do not push %s yet.\n", r.Tag)
	}
}
