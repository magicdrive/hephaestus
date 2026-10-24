# Hephaestus

**リリース成果物を、信頼できる配布物へ鍛え上げる。**

[English](README.md) · [設定](docs/configuration.md) ·
[アーカイブ](docs/archives.md) · [GitHub Action](docs/github-action.md) ·
[GoReleaser 互換性](docs/compatibility/goreleaser.md) ·
| [Compatibility policy](docs/compatibility/policy.md) | SemVer、安定 API の範囲、非推奨の扱い |
[アーキテクチャ](ARCHITECTURE.md)

Hephaestus は、どの言語で書かれたコマンドラインツールにも使えるリリース自動化ツールです。ビルドが作った実行ファイルを受け取る（またはターゲットごとにあなたのコマンドを実行してビルドする）と、再現可能なアーカイブにまとめ、チェックサムファイルを書き、GitHub Release を公開し、Homebrew Formula を生成して Tap へ Pull Request で届けます。

単一の Go バイナリと GitHub Action として提供され、設定は `hephaestus.yaml` 1 つです。

> 詳細なドキュメント（`docs/` 以下）は英語で提供しています。

## なぜ Hephaestus か

CLI の配布作業は、どの言語でもほぼ同じです。プラットフォームごとのバイナリ、アーカイブ、チェックサム、GitHub Release、Homebrew Formula。これを手作業や場当たり的なスクリプトで行うと、決まった形で失敗します。

- **アーカイブに、たまたまそこにあった物が入る。** glob が紛れ込んだ `.env` を拾い、別のファイルを足すと配置が動き、macOS では 2 つのエントリーが上書きし合う。
- **同じコミットから違うバイトができる。** タイムスタンプ、ファイル順、パーミッションがマシンごとに違うため、チェックサムを再現できない。
- **リリースが中途半端に公開される。** アップロードが失敗すると asset の欠けたリリースが公開され、再試行するとリリースや asset が重複する。
- **Formula が別の物を指す。** URL やチェックサムを、アップロードしたファイルからではなく命名規則から組み立ててしまう。`bin.install` がアーカイブにないパスを指す。
- **トークンがどこにでもある。** サードパーティのコードを実行するビルドが、リリースと Tap を書き換えられるトークンを持っている。

Hephaestus の答えは次のとおりです。

| 問題 | Hephaestus がすること |
|---|---|
| アーカイブの中身が不明確 | `files` が宣言された完全な一覧で、パスの意味は厳密に決まっている。衝突、大文字小文字だけの衝突、危険なモード、資格情報らしいファイルはエラー |
| バイトが再現できない | エントリーの整列、固定タイムスタンプ（`SOURCE_DATE_EPOCH`）、モードと所有者の正規化、圧縮ヘッダーの固定 |
| 中途半端な公開 | まず draft を作り、asset を 1 つずつアップロードして検証し、最後に公開する。再実行は続きから再開し、同一の asset は何もしない |
| Formula のデータが間違う | URL とチェックサムは Artifact Manifest から取る。`install` のパスは各アーカイブの記録済みの内容と照合する |
| トークンの過剰共有 | リリース用と Tap 用のトークンを分ける。ビルドコマンドの環境からは取り除き、Action ではトークンのない step でビルドする |

## 設計方針

- **計画してから書く。** 選んだすべてのステージの入力を、何かを実行する前に確かめる。1 つでも計画できないターゲットがあれば何も書かない。
- **データを捏造しない。** マニフェストには観測したことだけを記録する。ダイジェストはディスク上のバイトから計算し、URL は検証済みのアップロードの後でだけ記録する。
- **推測せず宣言する。** アーカイブには列挙した物だけが入る。README や LICENSE を暗黙に足すことも、ヒューリスティックもない。
- **言語に依存しない。** Hephaestus 自身は何もコンパイルしない。あなたのコマンドを実行するか、ビルド済みのファイルを取り込むか、GoReleaser の出力を取り込む。
- **再実行しても安全。** 公開のどの手順も、書く前に読む。

## 仕組み

```text
hephaestus.yaml
      │
      ▼
設定 + 事前検査        厳格な YAML、全テンプレートを検査、各ステージの入力を実行前に確認
      │
      ▼
build       ターゲットごとのコマンド │ 既存ファイル │ GoReleaser の artifacts.json
      ▼
archive     宣言したファイル → バイト単位で再現可能な tar.gz / zip
      ▼
checksum    <project>_<version>_checksums.txt（sha256sum 形式）
      ▼
release     GitHub Release: draft → アップロード + 検証 → 公開
      ▼
formula     マニフェストのチェックサムと内容から Homebrew Formula を生成
      ▼
tap         Tap へ Pull Request（既定）または直接コミット

            すべてのステージが dist/hephaestus-manifest.json を読み書きする
```

ステージは常にこの順で動きます。どのステージも単独で、あるいは後のジョブで実行でき、マニフェストから続きを再開します。詳細は [docs/architecture/overview.md](docs/architecture/overview.md) を参照してください。

## クイックスタート

**1. インストール** — [インストール](#インストール)を参照。

**2. 設定** — リポジトリのルートに `hephaestus.yaml` を置きます。プロジェクトの種類に合ったテンプレート（`go`、`rust`、noarch の Bash/Python/Node CLI 向けの `script`、他所でビルドした実行ファイル向けの `prebuilt`）から始められます。既存のファイルは上書きしません。

```sh
hephaestus init --template go --repo acme/mycli
```

手で書く場合は次のとおりです。

```yaml
version: 1

project:
  name: mycli
  description: Friendly tool for managing widgets
  homepage: https://github.com/acme/mycli
  license: MIT

builds:
  - id: mycli
    matrix:
      os: [darwin, linux, windows]
      arch: [amd64, arm64]
    command: [go, build, -trimpath, -o, "dist/bin/{{ .OS }}_{{ .Arch }}/mycli{{ .Ext }}", .]
    env:
      GOOS: "{{ .OS }}"
      GOARCH: "{{ .Arch }}"
      CGO_ENABLED: "0"
    output: "dist/bin/{{ .OS }}_{{ .Arch }}/mycli{{ .Ext }}"

archives:
  - id: default
    format_overrides:
      - os: windows
        format: zip
    files:
      - src: "dist/bin/{{ .OS }}_{{ .Arch }}/mycli{{ .Ext }}"
        dst: "mycli{{ .Ext }}"
        mode: "0755"
      - LICENSE
      - README.md

release:
  github:
    owner: acme
    name: mycli

brews:
  - name: mycli
    directory: Formula
    repository:
      owner: acme
      name: homebrew-tap
```

**3. 検査してローカルで試す**

```sh
hephaestus check
hephaestus run --snapshot --stages build,archive,checksum
ls dist/
```

**4. リリース** — タグを push して [GitHub Action](#github-action) を動かすか、次を実行します。

```sh
GITHUB_TOKEN=... HOMEBREW_TAP_GITHUB_TOKEN=... hephaestus run --tag v1.2.3
```

ほかの設定例は [examples/](examples/) にあります（Go、Rust、Zig、C++、GoReleaser、ビルド済み成果物、ワークフロー）。

## インストール

```sh
go install github.com/magicdrive/hephaestus@latest   # Go 1.26 以降
```

最初のリリース後は、次の方法も使えます。

```sh
brew install magicdrive/tap/hephaestus
```

GitHub Releases からアーカイブを取得することもできます。リリースのアーカイブとチェックサムファイルにはビルド来歴の attestation が付きます（`gh attestation verify <file> --repo magicdrive/hephaestus`）。

GitHub Actions では Action を使います（固定したコミットから Hephaestus をビルドします）。

```yaml
- uses: magicdrive/hephaestus@<full-commit-sha>
```

## 使い方

```text
hephaestus init --template go  最初の hephaestus.yaml を書き出す（go, rust, script, prebuilt）
hephaestus check              設定を検査し、計画を表示する
hephaestus run                設定されたすべてのステージを実行する（--stages a,b で指定）
hephaestus build|archive|checksum|release|formula|tap   1 つのステージを実行する
hephaestus preflight --tag vX.Y.Z   タグを push する前の読み取り専用リリースチェック
hephaestus status --tag vX.Y.Z      リリースの現状と次に取るべき安全な操作（読み取り専用）
hephaestus version
```

| フラグ | 意味 |
|---|---|
| `--config PATH` | 設定ファイル（既定はカレントディレクトリの `hephaestus.yaml`） |
| `--tag TAG` | リリースタグ（既定はタグ push 時の `$GITHUB_REF_NAME`。`--snapshot` 時は使わない）。`--snapshot` 以外では必須 |
| `--snapshot` | バージョン `0.0.0-SNAPSHOT`、タグなし。設定済みの release と tap ステージは飛ばし、`--stages` で明示した場合はエラー |
| `--stages LIST` | `run` のみ。カンマ区切りのステージ、または `all` |
| `--phase local\|publish` | `run` のみ。選んだステージのうち一方の段だけを実行する。`local` は build・archive・checksum、`publish` は release・formula・tap。ビルドと公開を別プロセスで実行するためのもの（Action はこれを使う） |
| `--dry-run` | release と tap の書き込みを計画だけして実行しない |
| `--fresh` | 既存のマニフェストを使わず新しく始める |
| `--commit SHA` | コミット（既定は `$GITHUB_SHA`） |
| `--repo OWNER/NAME` | リリース先リポジトリ（既定は設定、次に `$GITHUB_REPOSITORY`） |
| `--token-env NAME`、`--tap-token-env NAME` | トークンを持つ環境変数名（既定 `GITHUB_TOKEN`、`HOMEBREW_TAP_GITHUB_TOKEN`） |

実行するとアーカイブ、チェックサムファイル、Formula の SHA-256 を表示し、最後に `==> published`、`no-op`、`dry-run`、`failed` のいずれかを出力します。

## 設定

YAML ファイル 1 つです。未知のキーは行番号と候補つきのエラーになり、すべてのテンプレートは作業を始める前に試し描画されます。既定値、制約、テンプレート変数を含む完全なリファレンスは [docs/configuration.md](docs/configuration.md) です。

## カスタムビルド

`command` プロバイダーは、ターゲットごとにあなたのコマンドを 1 回実行します。ターゲットはテンプレート変数（`{{ .OS }}`、`{{ .Arch }}`、`{{ .Ext }}`）と環境変数（`HEPHAESTUS_OS`、`HEPHAESTUS_ARCH`、`HEPHAESTUS_ARM`、`HEPHAESTUS_OUTPUT`）で渡されます。リストはシェルを介さずに実行し、文字列は `sh -c`（Windows では `cmd /C`）で実行します。コマンドは `output` を作らなければなりません。

Hephaestus はクロスコンパイルをしません。ターゲットの宣言は、コマンドに何をビルドするかを伝えるだけです。例: [Go](examples/go/README.md)、[Rust](examples/rust/README.md)、[Zig](examples/zig/README.md)、[C++](examples/cpp/README.md)。

## GoReleaser との連携

- **GoReleaser の出力を取り込む**: `goreleaser:` を指定した build は `dist/artifacts.json` を読み、バイナリ（`archives: true` ならアーカイブも）を記録します。すべてのファイルを再ハッシュし、GoReleaser のチェックサムを検証します。
- **`brews` をコピーする**: Hephaestus は GoReleaser の `brews` のフィールド名を読みます。各フィールドの対応状況（対応、差異あり、拒否）は [docs/compatibility/goreleaser.md](docs/compatibility/goreleaser.md) にあります。
- **段階的に移行する**: 「GoReleaser がリリースし、Hephaestus が Formula を公開する」構成から全面移行まで、[docs/migration/from-goreleaser.md](docs/migration/from-goreleaser.md) と [examples/goreleaser-formula-only](examples/goreleaser-formula-only/README.md) を参照してください。

## 既存成果物の取り込み

`existing` プロバイダーは、別の場所（別のジョブ、プラットフォームごとのネイティブランナー、別のビルドシステム）で作ったファイルを、ターゲットごとのパステンプレートで取り込みます。`kind: archive` にすると、配布可能な `.tar.gz` / `.zip` をそのまま取り込みます。アーカイブは展開せずに検査し、パストラバーサル、リンク、特殊ファイル、危険なモードを含むものは拒否します。[examples/existing-artifacts](examples/existing-artifacts/README.md) を参照してください。

## アーカイブ

`files` は各アーカイブの中身の完全な一覧で、リテラルパス、glob、リネーム、`strip_parent`、除外、ターゲットごとの条件、モードの規則が厳密に決まっています。出力はバイト単位で再現可能です。リファレンスは [docs/archives.md](docs/archives.md) です。

## GitHub Releases

- 何かを送る前に、すべての asset をマニフェストと照合して再ハッシュします。
- 新しいリリースは **draft として**作成し、asset をアップロードして GitHub が返すサイズとダイジェストで検証してから公開します。不変（immutable）リリースとも両立します。
- 再実行しても安全です。同一の asset はそのまま、不完全な asset はアップロードし直し、内容の違う asset は `replace_existing_assets: true` がない限りエラーにします。応答が失われた場合は照会し直し、重複して作成することはありません。
- `prerelease: auto` はセマンティックバージョンのプレリリースを判定します。`make_latest`、`name_template`、`notes`、`notes_file`、`generate_notes`（新規リリースで GitHub の自動生成ノートを使う）に対応し、GitHub Enterprise Server には `api_url` / `upload_url` で対応します。

## Homebrew Formula

Formula はマニフェストから生成します。URL は公開したリリース（または `url_template`）から、SHA-256 はアーカイブから取ります。出力は現行の Homebrew のスタイル（`on_macos` / `on_linux` の中に `on_arm` / `on_intel`、`def install` は 1 つ、SPDX の `license` 式）に従い、CI で `brew audit --strict`、`brew install`、`brew test` を実行して確かめています。すべての値はエスケープした Ruby リテラルとして出力し、`install` のパスは各アーカイブの内容と照合します。既定の `test` ブロックは、インストールした実行ファイルの存在を確かめます。

## Tap への公開

- **Pull Request モード（既定）**: Formula をブランチ（`<project>-<version>`）にコミットし、Pull Request を開きます（開いているものがあれば再利用します）。フォーク元など別リポジトリへの Pull Request にも対応します。
- **直接モード**（`repository.pull_request.enabled: false`）: 指定したブランチ、または既定ブランチへ 1 回コミットします。
- 内容が同じならコミットも Pull Request も作りません。参照は作成するだけで、force update はしません。並行した更新は blob SHA で検出して再試行します。
- Tap には専用のトークン（`HOMEBREW_TAP_GITHUB_TOKEN` または `repository.token`）が必要です。`GITHUB_TOKEN` は別リポジトリに書き込めません。

## GitHub Action

```yaml
- uses: magicdrive/hephaestus@<full-commit-sha>
  with:
    command: run
    stages: formula,tap
    github-token: ""
    tap-token: ${{ secrets.HOMEBREW_TAP_GITHUB_TOKEN }}
```

| 入力 | 既定値 | 意味 |
|---|---|---|
| `command` | `run` | `run`、`check`、`build`、`archive`、`checksum`、`release`、`formula`、`tap` のいずれか |
| `stages` | 設定されたすべて | `run` のとき、カンマ区切りのステージまたは `all` |
| `config` | `hephaestus.yaml` | 設定ファイル（`working-directory` からの相対パス） |
| `tag` | push されたタグ | リリースタグ |
| `snapshot` | `"false"` | タグなしでビルドする |
| `dry-run` | `"false"` | GitHub への書き込みを計画だけする |
| `fresh` | `"false"` | 既存のマニフェストを無視する |
| `working-directory` | `.` | 実行するディレクトリ |
| `github-token` | `${{ github.token }}` | リリース用トークン（公開には `contents: write`） |
| `tap-token` | `""` | Tap 用トークン（Tap に対する `contents: write`、`pull-requests: write`） |

| 出力 | 値 |
|---|---|
| `status` | `published`、`no-op`、`dry-run`、`failed` |
| `no-op` | GitHub 上で何も変わらなかったとき `"true"` |
| `version` | リリースするバージョン |
| `artifacts` | アーカイブとチェックサムファイルの JSON 一覧（`name`、`path`、`sha256`、`url`） |
| `release-url` | GitHub Release の URL |
| `release-id` | GitHub Release の ID |
| `uploaded-assets` | この実行でアップロードまたは置換した asset 名の JSON 一覧 |
| `formula-paths` | 生成した Formula のパス（改行区切り） |
| `formula-commit-sha` | Tap へのコミット SHA（改行区切り） |
| `pull-request-url` | Tap の Pull Request の URL（改行区切り） |

権限、トークンの設計、信頼できない Pull Request の扱い、完全なワークフロー（単一ジョブ、Build と Publish の分離、GoReleaser 併用、Matrix）は [docs/github-action.md](docs/github-action.md) を参照してください。

## セキュリティ

- ソースは `os.Root` 経由で読みます。どんなパス、シンボリックリンク、競合でもプロジェクトのルート外は読めません。アーカイブにリンクのエントリーは入らず、危険なモード、大文字小文字だけの衝突、資格情報らしいファイルは拒否します。
- 公開用のトークンはビルドコマンドの環境から取り除きます。テンプレートからも参照できないようにします（資格情報らしい名前の `.Env` は見えず、展開結果にトークンが含まれればエラー）。送信は https で自分のオリジンにだけ行い、エラー・サマリー・出力では `***` に置き換えます（base64、URL エンコード、引用符でのエスケープの形も対象）。ただしビルドのコードは同じユーザーとして動くため、周囲のプロセスの環境を読める可能性があります。公開する場合は **Build と Publish をジョブごと分けてください**（[docs/github-action.md](docs/github-action.md#separate-build-and-publish-jobs)）。
- リリースは draft から始めて検証し、Tap は force push なしで更新します。
- **`pull_request` や `pull_request_target` で公開系のステージを動かさないでください。フォークのコードに秘密を渡さないでください。** 書き込み権限のないジョブでビルドし、別のジョブで、タグの push からだけ公開してください。Action はコミット SHA で固定してください。

脅威モデル: [docs/security/threat-model.md](docs/security/threat-model.md)。脆弱性の報告: [SECURITY.md](SECURITY.md)。

## 制限事項

- **自前のコンパイルはしない。** クロスコンパイルはビルドコマンドの仕事で、ターゲットはそのパラメーターにすぎません。
- **アーカイブ形式**は `tar.gz` と `zip` だけです。`tar.xz`、`tar.zst`、ターゲットごとの素のバイナリ asset（一意な名前での `include: [binary]` を除く）、deb/rpm、Docker イメージ、署名、SBOM はありません。
- **公開先**は GitHub Releases と GitHub 上の Homebrew Tap だけです。GitLab、Gitea、Homebrew Cask、Scoop、winget などには対応しません。
- **Homebrew**: darwin/linux の `amd64` / `arm64` のアーカイブだけを使います。32 ビット ARM、386、macOS の universal バイナリは飛ばします。`def install` は全プラットフォームで 1 つです。Tap の更新は GitHub API で行います（素の git/SSH、コミット署名の設定はありません）。
- **テンプレート**の変数と関数は少数です（日時、git のメタデータ、ファイル読み込みはありません）。[GoReleaser 互換性](docs/compatibility/goreleaser.md#template-variables)を参照してください。
- **リリースノート**: 変更履歴の生成はしません。`notes`、`notes_file`、`generate_notes` を使ってください。
- **チェックサム**は SHA-256 だけで、対象はアーカイブだけです（バイナリは載りません）。
- **パス**: Unicode 正規化（NFC/NFD）の違いによる衝突は検出しません。

## ドキュメント

| ドキュメント | 内容 |
|---|---|
| [Configuration](docs/configuration.md) | すべてのフィールド、既定値、制約、テンプレート変数 |
| [Archives](docs/archives.md) | ファイル一覧、パスの意味論、パーミッション、決定性 |
| [GitHub Action](docs/github-action.md) | 入力、出力、権限、トークン、ワークフロー |
| [GoReleaser compatibility](docs/compatibility/goreleaser.md) | `brews`、archives、テンプレートのフィールドごとの対応状況 |
| [Migrating from GoReleaser](docs/migration/from-goreleaser.md) | 段階的な移行手順 |
| [Architecture overview](docs/architecture/overview.md) | ステージ、Artifact Manifest、所有権、データの流れ |
| [Threat model](docs/security/threat-model.md) | 資産、攻撃者、対策、残余リスク |
| [Development](docs/development.md) | make ターゲット、テスト、golden ファイル、fuzz、E2E |
| [Release preflight](docs/preflight.md) | `hephaestus preflight --tag vX.Y.Z`：タグを push する前の読み取り専用チェック |
| [Recovering a release](docs/recovery.md) | `hephaestus status`、失敗箇所ごとの安全な復旧 |
| [Releasing](docs/releasing.md) | Hephaestus 自身のリリース手順（`make publish vX.Y.Z`、プレリリース、失敗時の復旧） |
| [Repository conventions](docs/architecture/repository-conventions.md) | Ark・Enma と共有する規約 |
| [Architecture](ARCHITECTURE.md) | コントリビューター向けの不変条件 |
| [Security policy](SECURITY.md) | サポート対象のバージョン、報告方法 |

## 開発

```sh
make build      # ./build/hephaestus
make test       # go test -count=1 ./...
make lint       # gofmt、vet、golangci-lint、テスト、race、govulncheck
make snapshot   # Hephaestus が自分自身をパッケージして結果を検証する
```

[docs/development.md](docs/development.md) と [CONTRIBUTING.md](CONTRIBUTING.md) を参照してください。AI コーディングエージェント向けには [AGENTS.md](AGENTS.md) があります。

## コントリビューション

バグ報告と Pull Request は [GitHub](https://github.com/magicdrive/hephaestus/issues) で歓迎します。ステージを変更する前に [ARCHITECTURE.md](ARCHITECTURE.md) を読んでください。もっともらしい変更が壊しうる不変条件と、それを守るテストが書かれています。

## ライセンス

[MIT](LICENSE) © 2026 magicdrive (Hiroshi IKEGAMI)
