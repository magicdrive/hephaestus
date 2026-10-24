# Zig

[hephaestus.yaml](hephaestus.yaml) builds a Zig CLI for macOS, Linux
(musl) and Windows from one host. `build.zig` must call
`b.standardTargetOptions(.{})` so `-Dtarget` is honored; the command installs
into `dist/zig/<os>_<arch>`, and `output` points at the installed
executable.

Archives place the executable at `bin/mycli`, so the formula installs
`bin.install "bin/mycli"`; Hephaestus verifies that path against every
archive's recorded contents before writing the formula.
