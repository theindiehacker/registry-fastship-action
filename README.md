# 🔑 Private Terraform Registry credentials helper

GitHub Actions から、トークンを指定せずに、Private な Terraform Module Registry のモジュールを `terraform init` で取得するための Action です。

Action は Terraform の [credentials helper](https://developer.hashicorp.com/terraform/internals/credentials-helpers) を入れます。Terraform がモジュールを取得するときに、helper が GitHub Actions の ID トークン (OIDC) をその場で取って渡します。トークンの発行・保管や、`TF_TOKEN_<host>` の設定は要りません。

- 既定のホストは `registry.fastship.jp` です。ほかの会社の registry には `host` を指定します。
- PC では、この helper ではなく `terraform login <host>` を使います ([PC から helper を消す](#-pc-から-helper-を消す))。
- Docker イメージを会社の Artifact Registry に push する reusable workflow もあります ([Docker イメージの publish](#-docker-イメージの-publish))。

## 📖 使い方

```yaml
jobs:
  terraform:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      id-token: write  # ID トークンの取得に要る
    steps:
      - uses: actions/checkout@v7

      - uses: theindiehacker/registry-fastship-action@<版または SHA>
        with:
          host: registry.example.io  # 省略すると registry.fastship.jp

      - run: terraform init  # env は要らない
```

```hcl
module "rdb" {
  source  = "registry.example.io/<namespace>/<name>/<system>"
  version = "1.0.0"
}
```

| 入力 | 既定 | 説明 |
|---|---|---|
| `host` | `registry.fastship.jp` | registry のホスト名。スキーム・ポート・パスは書きません。helper は、このホストにだけ ID トークン (audience は `https://<host>/v2`) を渡します |

- 版はタグか、コミットの SHA で固定してください。
- Action は、固定したコミットのソースから helper をビルドします (20〜40 秒)。
  - ビルドのために `actions/setup-go` で Go を入れるため、後の step の Go の版が変わります。job で Go を使うときは、自分の `setup-go` をこの Action の後に置いてください。
- helper は、job の後の step からも使えます。ただし、`id-token: write` の job では、どの step も元から ID トークンを取れるため、取れる範囲は広がりません。

## 🔧 helper の動き

Terraform は `terraform-credentials-fastship --host=<host> get <ホスト>` を実行します。

- `<host>` のときだけ、GitHub Actions の ID トークン (audience は `https://<host>/v2`) を返します。ほかのホスト (`registry.terraform.io` など) には `{}` を返し、トークンを渡しません。
- トークンは保管しません (`terraform login` には対応していません)。
- GitHub Actions の外では動きません。

## ✅ 前提と注意

- **組織**: registry が受け付ける GitHub 組織のリポジトリのワークフローであること。ほかの組織では 401 になります。
- **イベント**: 受け付けるのは `push`・`pull_request`・`workflow_dispatch`・`schedule`・`merge_group`・`release`・`repository_dispatch` です。`pull_request_target`・`issue_comment`・`workflow_run` などは、外部の PR のコードが base のリポジトリの権限で動くことがあるため、registry が拒否します。
- **401 になったとき**: Terraform は helper のエラーを表示せずに、トークンなしで続けます。`TF_LOG=WARN terraform init` で、`Failed to get credentials` の行に helper のエラーが出ます。
- **トークンの有効期限**: ID トークンは `terraform init` のプロセスの中で使い回され、数分で切れます。1 回の `init` がそれより長くかかると、途中から 401 になります (やり直すと最初から取り直します)。ふつうは数分もかかりません。
- **`TF_TOKEN_<host>`**: 設定されていると、helper より優先されます (Action が警告を出します)。
- **`credentials_helper` は 1 つだけ**: CLI 設定に書けるのは 1 つです。ほかの helper とは併用できず、1 つの job で使える `host` も 1 つです。
- **`TF_CLI_CONFIG_FILE`**: 設定されていると、Terraform は `~/.terraform.d/*.tfrc` を読みません。Action はそのファイルに追記するので、`TF_CLI_CONFIG_FILE` は Action より前に設定してください。

## 🐳 Docker イメージの publish

`.github/workflows/publish.yml` は、Docker イメージをビルドして、会社の Artifact Registry (registry の bootstrap が作る `apps`) に push する reusable workflow です。認証は WIF で、鍵は要りません。

```yaml
# 呼び出し元のリポジトリの .github/workflows/publish.yml
name: "🐳 イメージの publish"

on:
  workflow_dispatch:
    inputs:
      version:
        description: "版 (vX.Y.Z)"
        required: true
        type: string

permissions: {}

jobs:
  publish:
    # 複数のイメージは matrix で並べる
    strategy:
      matrix:
        include:
          - image: api
            target: api
          - image: worker
            target: worker
    permissions:
      contents: read
      id-token: write  # WIF に要る
    uses: theindiehacker/registry-fastship-action/.github/workflows/publish.yml@<タグ>  # SHA では借りられない (下の注意)
    with:
      image: ${{ matrix.image }}
      version: ${{ inputs.version }}
      context: .
      file: docker/Dockerfile
      target: ${{ matrix.target }}
      repository: ${{ vars.IMAGE_REPOSITORY }}
      workload_identity_provider: ${{ vars.WIF_PROVIDER_NAME }}
      service_account: ${{ vars.IMAGE_SERVICE_ACCOUNT_EMAIL }}
```

| 入力 | 既定 | 説明 |
|---|---|---|
| `image` | (必須) | イメージの名前 (`api` など)。小文字・数字と `.` `_` `-` `/` だけ |
| `version` | (必須) | タグ (`v1.2.3` など)。`latest` は使えません |
| `context` | `.` | ビルドのコンテキスト |
| `file` | `<context>/Dockerfile` | Dockerfile のパス (リポジトリの根から) |
| `target` | 最後のステージ | Dockerfile の target |
| `repository` | (必須) | push 先 (`<リージョン>-docker.pkg.dev/<プロジェクト>/<リポジトリ>`) |
| `workload_identity_provider` | (必須) | WIF のプロバイダー |
| `service_account` | (必須) | push の SA |

| 出力 | 説明 |
|---|---|
| `digest` | イメージの digest (`sha256:…`) |
| `image` | digest で指定したイメージ (`<repository>/<image>@sha256:…`)。そのまま `docker pull` できます |
| `pushed` | push したら `true`、同じタグが既にあってスキップしたら `false` |

- **設定の値**: `repository`・`workload_identity_provider`・`service_account` は、registry の bootstrap の出力 `image_publish` (`IMAGE_REPOSITORY`・`WIF_PROVIDER_NAME`・`IMAGE_SERVICE_ACCOUNT_EMAIL`) です。組織の Actions の変数 (Variables) に入れておくと、どのリポジトリからも `vars` で使えます。
- **タグか main で指定する**: registry が push の SA を貸すのは、この workflow を `@refs/heads/main` か `@refs/tags/*` で使ったジョブ (ID トークンの `job_workflow_ref`) だけです。SHA で指定すると `job_workflow_ref` が `@<SHA>` になり、認証で断られます。タグはリリースのワークフローだけが作り、作り直しません。
- **やり直せる**: 同じタグが既にあれば、ビルドせずに、そのイメージの digest を出して成功します。matrix の一部が失敗したときは、同じ版でもう一度実行します。中身を変えるときは、次の版で publish します (タグは付け替えられません)。
- **matrix の出力**: matrix のジョブの出力は 1 つにまとまるため、イメージごとの digest は、各ジョブの Summary (`docker pull <イメージ>@sha256:…`) で見ます。
- **push できるジョブ**: 設定した組織のリポジトリなら、どのブランチから呼んでも push できます (ID トークンに、呼び出し元のデフォルトブランチを示すクレームが無いため)。版のイメージは、デフォルトブランチかタグから publish してください。自分の workflow で直接 push するジョブは、SA を借りられません。
- **イメージ名は早い者勝ち**: イメージ名は、呼び出し元のリポジトリに結び付けません (組織の中は信頼します。モジュールの上げ方と同じです)。組織のどのリポジトリからでも、まだ無いイメージ名・タグを先に push できます。一度 push したタグは付け替えられず、ほかのリポジトリからも上書きできません。イメージ名は、リポジトリの名前を含めるなどして、ぶつからないようにしてください。
- **イメージ**: ラベル `org.opencontainers.image.source` (リポジトリ)・`revision` (コミット)・`version` を付けます。プラットフォームは runner (`linux/amd64`) です。

## 💻 PC から helper を消す

PC では `terraform login <host>` を使います。前の版の手順で helper を入れた PC では、Terraform が login のトークンを helper に渡そうとして失敗するため、先に helper と `fastship.tfrc` を消してください。

```bash
# macOS・Linux
rm -f ~/.terraform.d/plugins/terraform-credentials-fastship ~/.terraform.d/fastship.tfrc
```

```powershell
# Windows
Remove-Item -ErrorAction SilentlyContinue "$env:APPDATA\terraform.d\plugins\terraform-credentials-fastship.exe", "$env:APPDATA\terraform.d\fastship.tfrc"
```

`TF_CLI_CONFIG_FILE` や `~/.terraformrc` (Windows は `%APPDATA%\terraform.rc`) に `credentials_helper "fastship"` を書いたときは、その行も消してください。そのあと `terraform login <host>` を実行します。

## 🔁 v1 からの移行

v1 と v2.0.0 は使えません。registry は、v2.1.0 以降の helper が取る audience (`https://<host>/v2`) のトークンだけを受け付けるため、401 になります。

v1 は ID トークンを出力し、`TF_TOKEN_registry_fastship_jp` に渡していました。v2 では `token` の出力を消しました。`id` と `env` を消してください。

```diff
-      - id: registry
-        uses: theindiehacker/registry-fastship-action@v1.0.0
+      - uses: theindiehacker/registry-fastship-action@<版または SHA>
       - run: terraform init
-        env:
-          TF_TOKEN_registry_fastship_jp: ${{ steps.registry.outputs.token }}
```

## 🛠️ 開発

```bash
go test ./...

# イメージの publish の reusable workflow (docker・jq・yq が要る。ローカルの registry:2 で動かす)
bash tests/publish_test.sh
```

### リリース

Actions → 📦 リリース (`release.yml`) を、main で `version: vX.Y.Z` を指定して実行します。タグは手で作りません。

- main の先頭にタグを付け、helper のバイナリ・`SHA256SUMS`・build provenance を付けた Release を作ります。
- 一度出した版は作り直しません。既にあるタグを指定すると失敗します。中身を直すときは、次の版を出してください。
- 利用者は SHA で固定するので、Release の画面に出るタグのコミット SHA を案内します。
