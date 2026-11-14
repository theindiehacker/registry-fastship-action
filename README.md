# 🔑 Private Terraform Registry credentials helper

GitHub Actions から、トークンを指定せずに、Private な Terraform Module Registry のモジュールを `terraform init` で取得するための Action です。

Action は Terraform の [credentials helper](https://developer.hashicorp.com/terraform/internals/credentials-helpers) を入れます。Terraform がモジュールを取得するときに、helper が GitHub Actions の ID トークン (OIDC) をその場で取って渡します。トークンの発行・保管や、`TF_TOKEN_<host>` の設定は要りません。

- 既定のホストは `registry.fastship.jp` です。ほかの会社の registry には `host` を指定します。
- PC では、この helper ではなく `terraform login <host>` を使います ([PC から helper を消す](#-pc-から-helper-を消す))。

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
```

### リリース

Actions → 📦 リリース (`release.yml`) を、main で `version: vX.Y.Z` を指定して実行します。タグは手で作りません。

- main の先頭にタグを付け、helper のバイナリ・`SHA256SUMS`・build provenance を付けた Release を作ります。
- 一度出した版は作り直しません。既にあるタグを指定すると失敗します。中身を直すときは、次の版を出してください。
- 利用者は SHA で固定するので、Release の画面に出るタグのコミット SHA を案内します。
