# Existing artifacts

[hephaestus.yaml](hephaestus.yaml) packages executables that were built
elsewhere — another job, a native runner per platform, another build system.
The `existing` provider reads `prebuilt/<os>_<arch>/mycli` for each declared
target, checks that it is a non-empty regular file inside the project root,
hashes it, and records it in the manifest. Archives, checksums, the release
and the formula then work as for any other build.

Workflow artifacts lose the executable bit, so the archive entry sets
`mode: "0755"` explicitly.

Prebuilt release-ready archives (`.tar.gz`, `.tgz`, `.zip`) can be imported
with `existing.kind: archive` instead: they are inspected without extraction
and published as they are.

Workflow: [../workflows/matrix-then-publish.yml](../workflows/matrix-then-publish.yml).
