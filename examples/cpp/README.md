# C++ (CMake)

[hephaestus.yaml](hephaestus.yaml) configures and builds a CMake project once
per target, with a toolchain file per target
(`cmake/toolchains/linux-amd64.cmake`, `linux-arm64.cmake`), and packages the
executable and its man page under one top-level directory
(`wrap_in_directory: true`).

Cross-compiling C++ needs a cross toolchain (clang with `--target` and a
sysroot, or `zig cc`). When that is impractical, build on native runners and
package the results with the `existing` provider:
[../existing-artifacts](../existing-artifacts/README.md) and
[../workflows/matrix-then-publish.yml](../workflows/matrix-then-publish.yml).

Homebrew changes into the single top-level directory before `install` runs,
so the formula refers to `bin/mycli` and `share/man/man1/mycli.1`.
