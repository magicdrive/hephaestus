package hephaestus

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
)

// initTemplates are the starting configurations of `hephaestus init`. They
// use [[ ]] as delimiters so that Hephaestus's own {{ }} templates pass
// through unchanged.
var initTemplates = map[string]string{
	"go": `# Release configuration for [[ .Name ]], read by Hephaestus
# (https://github.com/magicdrive/hephaestus): a Go CLI cross-compiled for
# macOS, Linux and Windows.
version: 1

project:
  name: [[ .Name ]]
  description: "[[ .Description ]]"
  homepage: https://github.com/[[ .Owner ]]/[[ .Repo ]]
  license: [[ .License ]]

builds:
  - id: [[ .Name ]]
    matrix:
      os: [darwin, linux, windows]
      arch: [amd64, arm64]
    command: [go, build, -trimpath, -ldflags, "-s -w -X main.version={{ .Version }}", -o, "dist/bin/{{ .OS }}_{{ .Arch }}/[[ .Name ]]{{ .Ext }}", .]
    env:
      GOOS: "{{ .OS }}"
      GOARCH: "{{ .Arch }}"
      CGO_ENABLED: "0"
    output: "dist/bin/{{ .OS }}_{{ .Arch }}/[[ .Name ]]{{ .Ext }}"

archives:
  - id: default
    format_overrides:
      - {os: windows, format: zip}
    files:
      - {src: "dist/bin/{{ .OS }}_{{ .Arch }}/[[ .Name ]]{{ .Ext }}", dst: "[[ .Name ]]{{ .Ext }}", mode: "0755"}
[[- range .Docs ]]
      - [[ . ]]
[[- end ]]
[[ template "tail" . ]]`,

	"rust": `# Release configuration for [[ .Name ]], read by Hephaestus
# (https://github.com/magicdrive/hephaestus): a Rust CLI. Hephaestus does
# not cross-compile: each target needs its Rust target installed
# (rustup target add ...) and a linker for it, or build each OS on its own
# runner and import the binaries with the existing provider.
version: 1

project:
  name: [[ .Name ]]
  description: "[[ .Description ]]"
  homepage: https://github.com/[[ .Owner ]]/[[ .Repo ]]
  license: [[ .License ]]

builds:
  - id: [[ .Name ]]
    matrix:
      os: [darwin, linux]
      arch: [amd64, arm64]
    command: [cargo, build, --release, --locked, --target, "{{ if eq .Arch \"arm64\" }}aarch64{{ else }}x86_64{{ end }}-{{ if eq .OS \"darwin\" }}apple-darwin{{ else }}unknown-linux-musl{{ end }}"]
    output: "target/{{ if eq .Arch \"arm64\" }}aarch64{{ else }}x86_64{{ end }}-{{ if eq .OS \"darwin\" }}apple-darwin{{ else }}unknown-linux-musl{{ end }}/release/[[ .Name ]]"

archives:
  - id: default
    files:
      - {src: "target/{{ if eq .Arch \"arm64\" }}aarch64{{ else }}x86_64{{ end }}-{{ if eq .OS \"darwin\" }}apple-darwin{{ else }}unknown-linux-musl{{ end }}/release/[[ .Name ]]", dst: "[[ .Name ]]", mode: "0755"}
[[- range .Docs ]]
      - [[ . ]]
[[- end ]]
[[ template "tail" . ]]`,

	"script": `# Release configuration for [[ .Name ]], read by Hephaestus
# (https://github.com/magicdrive/hephaestus): a script CLI, packaged once
# for every platform (a noarch archive) and installed through one formula
# url.
version: 1

project:
  name: [[ .Name ]]
  description: "[[ .Description ]]"
  homepage: https://github.com/[[ .Owner ]]/[[ .Repo ]]
  license: [[ .License ]]

archives:
  - id: default
    name: "{{ .Project }}_{{ .Version }}_noarch"
    targets: [{os: all, arch: all}]
    files:
      - {src: [[ .Name ]], dst: [[ .Name ]], mode: "0755"}
[[- range .Docs ]]
      - [[ . ]]
[[- end ]]
[[ template "tail" . ]]`,

	"prebuilt": `# Release configuration for [[ .Name ]], read by Hephaestus
# (https://github.com/magicdrive/hephaestus): executables built elsewhere,
# placed at prebuilt/<os>_<arch>/[[ .Name ]] before the run.
version: 1

project:
  name: [[ .Name ]]
  description: "[[ .Description ]]"
  homepage: https://github.com/[[ .Owner ]]/[[ .Repo ]]
  license: [[ .License ]]

builds:
  - id: [[ .Name ]]
    matrix:
      os: [darwin, linux]
      arch: [amd64, arm64]
    existing:
      path: "prebuilt/{{ .OS }}_{{ .Arch }}/[[ .Name ]]"

archives:
  - id: default
    files:
      - {src: "prebuilt/{{ .OS }}_{{ .Arch }}/[[ .Name ]]", dst: "[[ .Name ]]", mode: "0755"}
[[- range .Docs ]]
      - [[ . ]]
[[- end ]]
[[ template "tail" . ]]`,
}

// initTail is shared by every template: checksum, release, formula.
const initTail = `[[ define "tail" ]]
checksum:
  name: "{{ .Project }}_checksums.txt"

release:
  github:
    owner: [[ .Owner ]]
    name: [[ .Repo ]]
    generate_notes: true

brews:
  - name: [[ .Name ]]
    # Prereleases never update the tap.
    skip_upload: auto
    install: |
      bin.install "[[ .Name ]]"
    test: |
      system "#{bin}/[[ .Name ]]", "--version"
    repository:
      owner: [[ .TapOwner ]]
      name: [[ .TapName ]]
      pull_request:
        enabled: true
[[ end ]]`

var initNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

type initData struct {
	Name, Description, License string
	Owner, Repo                string
	TapOwner, TapName          string
	Docs                       []string
}

// initCmd runs `hephaestus init`: writes a starting hephaestus.yaml for a
// kind of project. It never overwrites a file.
func initCmd(_ context.Context, args []string, stdout, stderr io.Writer) int {
	var kind, name, desc, repoSpec, tap, license, out string
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&kind, "template", "", "go, rust, script (noarch) or prebuilt (required)")
	fs.StringVar(&name, "name", "", "command name (default: the directory name)")
	fs.StringVar(&desc, "description", "Command-line tool", "one-line description (Homebrew: capitalized, not starting with the name)")
	fs.StringVar(&repoSpec, "repo", "", "owner/name of the GitHub repository (default: $GITHUB_REPOSITORY)")
	fs.StringVar(&tap, "tap", "", "owner/name of the Homebrew tap (default: <owner>/homebrew-tap)")
	fs.StringVar(&license, "license", "MIT", "SPDX license of the project")
	fs.StringVar(&out, "output", "hephaestus.yaml", "file to write (never overwritten)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	src, ok := initTemplates[kind]
	if fs.NArg() > 0 || !ok {
		fmt.Fprintln(stderr, "usage: hephaestus init --template go|rust|script|prebuilt [--name NAME] [--repo owner/name] [--tap owner/name] [--license SPDX] [--output FILE]")
		return 2
	}
	dir := filepath.Dir(out)
	if name == "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			fmt.Fprintf(stderr, "hephaestus init: %v\n", err)
			return 1
		}
		name = strings.ToLower(filepath.Base(abs))
	}
	if !initNameRe.MatchString(name) {
		fmt.Fprintf(stderr, "hephaestus init: name %q: use lower-case letters, digits, '.', '_' and '-' (pass --name)\n", name)
		return 2
	}
	if repoSpec == "" {
		repoSpec = os.Getenv("GITHUB_REPOSITORY")
	}
	if desc == "" || strings.ContainsAny(desc, "\n\"") {
		fmt.Fprintln(stderr, "hephaestus init: --description must be one line without double quotes")
		return 2
	}
	d := initData{Name: name, Description: desc, License: license}
	var err error
	if d.Owner, d.Repo, err = splitRepo(repoSpec, "--repo"); err != nil {
		fmt.Fprintf(stderr, "hephaestus init: %v\n", err)
		return 2
	}
	if tap == "" {
		tap = d.Owner + "/homebrew-tap"
	}
	if d.TapOwner, d.TapName, err = splitRepo(tap, "--tap"); err != nil {
		fmt.Fprintf(stderr, "hephaestus init: %v\n", err)
		return 2
	}
	// Only documents that exist are listed: a missing file fails the archive.
	for _, doc := range []string{"README.md", "LICENSE"} {
		if fi, err := os.Stat(filepath.Join(dir, doc)); err == nil && fi.Mode().IsRegular() {
			d.Docs = append(d.Docs, doc)
		}
	}

	t := template.Must(template.New(kind).Delims("[[", "]]").Option("missingkey=error").Parse(src))
	template.Must(t.Parse(initTail))
	var b bytes.Buffer
	if err := t.Execute(&b, d); err != nil {
		fmt.Fprintf(stderr, "hephaestus init: %v\n", err)
		return 1
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			fmt.Fprintf(stderr, "hephaestus init: %s already exists; it is never overwritten\n", out)
			return 1
		}
		fmt.Fprintf(stderr, "hephaestus init: %v\n", err)
		return 1
	}
	if _, err := f.Write(b.Bytes()); err != nil {
		_ = f.Close()
		fmt.Fprintf(stderr, "hephaestus init: %v\n", err)
		return 1
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(stderr, "hephaestus init: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s (%s template)\nnext: review it, then run \"hephaestus check\" and \"hephaestus run --snapshot\"\n", out, kind)
	return 0
}

func splitRepo(spec, flagName string) (string, string, error) {
	owner, name, ok := strings.Cut(spec, "/")
	if spec == "" {
		return "", "", fmt.Errorf("the GitHub repository is unknown: pass %s owner/name", flagName)
	}
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || strings.ContainsAny(spec, " \t\n:\"'") {
		return "", "", fmt.Errorf("%s %q must be owner/name", flagName, spec)
	}
	return owner, name, nil
}
