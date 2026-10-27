package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// TestMakefile_FmtCheckFailsWhenGofmtFails runs make fmt-check (part of make
// lint, the local gate before a push) with a stub gofmt: gofmt prints
// nothing on stdout when it fails (a file it cannot parse, or no gofmt on
// PATH), and that empty output must not read as "every file is formatted".
func TestMakefile_FmtCheckFailsWhenGofmtFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Makefile targets macOS and Linux")
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	for _, tc := range []struct {
		name, gofmt string
		ok          bool
	}{
		{"formatted", "exit 0", true},
		{"unformatted", "echo main.go", false},
		{"gofmt fails", `echo "broken.go:1:1: expected 'package', found 'EOF'" >&2; exit 2`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "gofmt"), []byte("#!/bin/sh\n"+tc.gofmt+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("make", "--no-print-directory", "fmt-check")
			for _, kv := range os.Environ() {
				k, _, _ := strings.Cut(kv, "=")
				if !slices.Contains([]string{"MAKEFLAGS", "MFLAGS", "MAKELEVEL", "PATH"}, k) {
					cmd.Env = append(cmd.Env, kv)
				}
			}
			cmd.Env = append(cmd.Env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.ok {
				t.Errorf("make fmt-check: err %v, want ok %t\n%s", err, tc.ok, out)
			}
		})
	}
}
