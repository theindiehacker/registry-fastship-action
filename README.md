# 🔑 registry.fastship.jp credentials helper

[registry.fastship.jp](https://registry.fastship.jp) (FastShip の private な Terraform Module Registry) から、トークンを指定せずに `terraform init` でモジュールを取得するための、Terraform の [credentials helper](https://developer.hashicorp.com/terraform/internals/credentials-helpers) です。

Terraform がモジュールを取得するときに、helper が、動いている環境の ID の ID トークンをその場で取って渡します。トークンの発行・保管や、`TF_TOKEN_registry_fastship_jp` の設定は要りません。

| 環境 | ID トークンの取り方 |
|---|---|
| GitHub Actions | GitHub の OIDC (この Action が helper を入れる) |
| Google Cloud (Cloud Build・Compute Engine など) | メタデータサーバ (SA の ID トークン、`email` 入り) |
| ローカル | `gcloud` で、登録した SA になりすます |

## 📖 使い方

### GitHub Actions

```yaml
jobs:
  terraform:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      id-token: write  # ID トークンの取得に要る
    steps:
      - uses: actions/checkout@v7

      - uses: theindiehacker/registry-fastship-action@v2.0.0

      - run: terraform init  # env は要らない
```

- 版はタグ (`@v2.0.0`) か、コミットの SHA で固定してください。
- Action は、固定したコミットのソースから helper をビルドします (20〜40 秒)。
  - ビルドのために `actions/setup-go` で Go を入れるため、後の step の Go の版が変わります。job で Go を使うときは、自分の `setup-go` をこの Action の後に置いてください。
- helper は、job の後の step からも使えます。ただし、`id-token: write` の job では、どの step も元から ID トークンを取れるため、取れる範囲は広がりません。

### ローカル

Terraform の CLI 設定のディレクトリ (`~/.terraform.d`。Windows は `%APPDATA%\terraform.d`) に、helper と設定を置きます。

1. [Releases](https://github.com/theindiehacker/registry-fastship-action/releases) から、OS に合うバイナリを `plugins/terraform-credentials-fastship` (Windows は `.exe`) に置きます。
   ```bash
   os=$(uname -s | tr '[:upper:]' '[:lower:]'); arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
   helper=~/.terraform.d/plugins/terraform-credentials-fastship
   mkdir -p ~/.terraform.d/plugins
   curl -fsSL -o "$helper" "https://github.com/theindiehacker/registry-fastship-action/releases/download/v2.0.0/terraform-credentials-fastship_${os}_${arch}"
   chmod +x "$helper"
   gh attestation verify "$helper" -R theindiehacker/registry-fastship-action  # このリポジトリの release.yml がビルドしたものか確かめる
   ```
2. `fastship.tfrc` に、なりすます SA (registry.fastship.jp に登録した SA) を書きます。
   ```bash
   cat > ~/.terraform.d/fastship.tfrc <<'EOF'
   credentials_helper "fastship" {
     args = ["--impersonate-service-account=<登録した SA>"]
   }
   EOF
   ```
3. `gcloud auth login` でログインします。SA の `roles/iam.serviceAccountTokenCreator` が要ります。

あとは `terraform init` を実行するだけです。

### Google Cloud (Cloud Build・Compute Engine など)

ローカルの 1・2 と同じように置きます。`args` は書きません (`credentials_helper "fastship" {}`)。helper は、メタデータサーバから、実行している SA の ID トークンを取ります。
Cloud Build では、`HOME` (`/builder/home`) が step の間で引き継がれるため、helper を置く step と `terraform init` の step を分けてもかまいません。

## 🔧 helper の動き

Terraform は `terraform-credentials-fastship [args] get <ホスト>` を実行します。

- `registry.fastship.jp` のときだけ ID トークン (audience は `https://registry.fastship.jp`) を返します。ほかのホスト (`registry.terraform.io` など) には `{}` を返し、トークンを渡しません。
- 取り方は、`--impersonate-service-account` があればなりすまし、GitHub Actions なら GitHub の OIDC、それ以外ならメタデータサーバの順です。
  - GitHub Actions では、self-hosted のランナーが Google Cloud にあっても、ランナーの SA は使いません。
- トークンは保管しません (`terraform login` には対応していません)。

## ✅ 前提と注意

- **契約企業の登録**: GitHub の org (または SA) を、registry.fastship.jp の契約企業として登録済みであること。登録していないと 401 になります。
- **イベント**: GitHub Actions で受け付けるのは `push`・`pull_request`・`workflow_dispatch`・`schedule`・`merge_group`・`release`・`repository_dispatch` です。`pull_request_target`・`issue_comment`・`workflow_run` などは、外部の PR のコードが base のリポジトリの権限で動くことがあるため、registry が拒否します。
- **401 になったとき**: Terraform は helper のエラーを表示せずに、トークンなしで続けます。`TF_LOG=WARN terraform init` で、`Failed to get credentials` の行に helper のエラーが出ます。
- **トークンの有効期限**: helper の ID トークンは、`terraform init` のプロセスの中で使い回されます。GitHub Actions の ID トークンは数分で切れるため、1 回の `terraform init` がそれより長くかかると、途中から 401 になります。やり直せば、取得済みのモジュールは飛ばして続きます (トークンが要るのは versions と download だけで、zip の取得には要りません)。
- **`TF_TOKEN_registry_fastship_jp`**: 設定されていると、helper より優先されます。
- **`credentials_helper` は 1 つだけ**: CLI 設定に書けるのは 1 つです。ほかの helper (HCP Terraform 用など) とは併用できません。
- **`TF_CLI_CONFIG_FILE`**: 設定されていると、Terraform は `~/.terraform.d/*.tfrc` を読みません。Action はそのファイルに追記するので、`TF_CLI_CONFIG_FILE` は Action より前に設定してください。ローカルでは、そのファイルに `credentials_helper` を書いてください。

## 🔁 v1 からの移行

v1 は ID トークンを出力し、`TF_TOKEN_registry_fastship_jp` に渡していました。v2 では `token` の出力を消しました。`id` と `env` を消してください。

```diff
-      - id: registry
-        uses: theindiehacker/registry-fastship-action@v1.0.0
+      - uses: theindiehacker/registry-fastship-action@v2.0.0
       - run: terraform init
-        env:
-          TF_TOKEN_registry_fastship_jp: ${{ steps.registry.outputs.token }}
```

## 🛠️ 開発

```bash
go test ./...
```

### リリース

Actions → 📦 リリース (`release.yml`) を、main で `version: vX.Y.Z` を指定して実行します。タグは手で作りません。

- main の先頭にタグを付け、ローカル向けのバイナリ・`SHA256SUMS`・build provenance を付けた Release を作ります。
- 一度出した版は作り直しません。既にあるタグを指定すると失敗します。中身を直すときは、次の版を出してください。
- 利用者は SHA で固定するので、Release の画面に出るタグのコミット SHA を案内します。
