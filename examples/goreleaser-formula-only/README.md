# GoReleaser builds, Hephaestus publishes the formula

Keep GoReleaser for building, archiving and releasing, and let Hephaestus
take over the Homebrew formula (for example because GoReleaser deprecated
`brews`). [hephaestus.yaml](hephaestus.yaml) imports the archives GoReleaser
listed in `dist/artifacts.json` and contains the `brews` entry copied from
`.goreleaser.yaml`.

```sh
goreleaser release --clean
hephaestus run --stages build,formula,tap --fresh   # import, generate, publish
```

Archives are selected by GoReleaser archive id with `goreleaser.ids`;
`brews[].ids` may name the importing build (`goreleaser`). Instead of copying
the `brews` entry you can also point `brews_from: .goreleaser.yaml` at the
GoReleaser configuration.

Every imported archive is re-hashed, its `extra.Checksum` verified, and its
members inspected; the formula's URLs point at the release GoReleaser
published (`GITHUB_REPOSITORY` or `--repo`, and the tag).

Workflow: [../workflows/goreleaser-formula-only.yml](../workflows/goreleaser-formula-only.yml).
Migration guide: [docs/migration/from-goreleaser.md](../../docs/migration/from-goreleaser.md).
