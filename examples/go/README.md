# Go

[hephaestus.yaml](hephaestus.yaml) builds a Go CLI for macOS, Linux and
Windows (amd64, arm64) with `go build`, packages each target with its
license and README (zip on Windows), publishes a GitHub Release with
GitHub-generated notes, and opens a pull request on `acme/homebrew-tap`.

The Go toolchain cross-compiles by itself when `CGO_ENABLED=0`; `GOOS` and
`GOARCH` come from the build's `env`. Optional shell completions under
`completions/` are included when present (`required: false`).

```sh
hephaestus check
hephaestus run --snapshot --stages build,archive,checksum   # local dry build
hephaestus run --tag v1.2.3                                 # everything
```

Workflow: [../workflows/release-single-job.yml](../workflows/release-single-job.yml).
