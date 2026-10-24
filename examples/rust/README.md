# Rust

[hephaestus.yaml](hephaestus.yaml) builds a Rust CLI for Linux (musl) and
macOS with [cargo-zigbuild](https://github.com/rust-cross/cargo-zigbuild).
Hephaestus does not cross-compile: the shell command maps
`HEPHAESTUS_OS`/`HEPHAESTUS_ARCH` to a Rust target triple, builds it, and
copies the executable to `HEPHAESTUS_OUTPUT`.

Requirements on the build machine: `rustup target add` for the four
triples, `cargo install cargo-zigbuild`, and Zig. macOS targets link most
reliably on a macOS host.

The formula uses the default `install` (`bin.install "mycli"`, the
executable present in every archive) and a `--version` test. The license is
an SPDX expression, written as `license any_of: ["MIT", "Apache-2.0"]`.

To build natively on several runners instead, see
[../existing-artifacts](../existing-artifacts/README.md).
