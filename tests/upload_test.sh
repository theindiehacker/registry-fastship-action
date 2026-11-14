#!/usr/bin/env bash
# upload.yml (モジュールを上げる reusable workflow) の step を抜き出し、ローカルの偽の registry と ID トークンの発行元で動かして確かめる。
# 本物の registry への upload は確かめない (test.yml の upload-real が、pull_request のトークンを断ることだけを確かめる)。
# 要るもの: python3・jq・yq (jq の書き方が通るもの)・curl。使い方: bash tests/upload_test.sh
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
workflow="$root/.github/workflows/upload.yml"
work=$(mktemp -d)
port=${UPLOAD_TEST_PORT:-5100}
failures=0
server=""

cleanup() {
  if [ -n "$server" ]; then kill "$server" 2> /dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT

# upload.yml の id の step の run を取り出す
step_script() {
  local script
  script=$(yq -r ".jobs.upload.steps[] | select(.id == \"$1\") | .run" "$workflow")
  if [ -z "$script" ] || [ "$script" = "null" ]; then
    echo "upload.yml に id: $1 の step がありません" >&2
    return 1
  fi
  printf '%s\n' "$script"
}

# step を GitHub Actions の bash と同じオプションで動かす。出力は $work/output に書かれる
run_step() {
  local id=$1
  shift
  : > "$work/output"
  step_script "$id" > "$work/$id.sh"
  env GITHUB_OUTPUT="$work/output" GITHUB_STEP_SUMMARY="$work/summary" RUNNER_TEMP="$work/tmp" "$@" \
    bash --noprofile --norc -eo pipefail "$work/$id.sh"
}

output() {
  grep -E "^$1=" "$work/output" | tail -1 | cut -d= -f2-
}

pass() { echo "ok - $1"; }
fail() {
  echo "not ok - $1" >&2
  failures=$((failures + 1))
}

mkdir -p "$work/tmp"

# ---- 入力の検査 (check) ----

check_env=(HOST=registry.fastship.jp NAMESPACE=acme NAME=network SYSTEM=google VERSION=1.2.3 MODULE_PATH=modules/network)

if run_step check "${check_env[@]}" > /dev/null; then
  if [ "$(output host)" = "registry.fastship.jp" ] &&
    [ "$(output module)" = "acme/network/google" ] &&
    [ "$(output version)" = "1.2.3" ] &&
    [ "$(output source)" = "registry.fastship.jp/acme/network/google" ]; then
    pass "check: 正しい入力から、ホスト・モジュール・版・source を出す"
  else
    fail "check: 出力が違う: $(cat "$work/output")"
  fi
else
  fail "check: 正しい入力を断った"
fi

if run_step check "${check_env[@]}" HOST=Registry.Example.IO VERSION=v2.0.0 > /dev/null &&
  [ "$(output host)" = "registry.example.io" ] && [ "$(output version)" = "2.0.0" ]; then
  pass "check: ホストを小文字にし、版の先頭の v を外す (タグの名前をそのまま渡せる)"
else
  fail "check: ホストの小文字か、版の v の扱いが違う: $(cat "$work/output")"
fi

for good in "MODULE_PATH=." "NAMESPACE=my-org_1" "NAME=a" "SYSTEM=aws" "VERSION=0.0.0" "VERSION=10.20.30"; do
  if run_step check "${check_env[@]}" "$good" > /dev/null 2>&1; then
    pass "check: $good を受け付ける"
  else
    fail "check: $good を断った"
  fi
done

for bad in \
  "HOST=https://registry.fastship.jp" \
  "HOST=registry.fastship.jp:443" \
  "HOST=registry.fastship.jp/v1" \
  "HOST=localhost" \
  "HOST=" \
  "NAMESPACE=" \
  "NAMESPACE=Acme" \
  "NAMESPACE=acme-" \
  "NAMESPACE=net.work" \
  "NAMESPACE=a/b" \
  "NAME=network_" \
  "NAME=$(printf 'a%.0s' $(seq 1 65))" \
  "SYSTEM=my-cloud" \
  "SYSTEM=my_cloud" \
  "SYSTEM=Google" \
  "VERSION=1.2" \
  "VERSION=1.2.3-rc.1" \
  "VERSION=01.2.3" \
  "VERSION=latest" \
  "VERSION=" \
  "MODULE_PATH=" \
  "MODULE_PATH=/home/runner" \
  "MODULE_PATH=.." \
  "MODULE_PATH=modules/../.." \
  "MODULE_PATH=~"; do
  if run_step check "${check_env[@]}" "$bad" > /dev/null 2>&1; then
    fail "check: $bad を受け付けた"
  else
    pass "check: $bad を断る"
  fi
done

# ---- 指定したディレクトリだけを zip にする (package) ----

src="$work/src"
mkdir -p "$src/modules/network/templates" "$src/modules/network/.terraform/modules" "$src/modules/other" "$src/.git"
echo 'variable "name" {}' > "$src/modules/network/main.tf"
echo '# network' > "$src/modules/network/README.md"
echo 'tpl' > "$src/modules/network/templates/a.tpl"
echo 'cache' > "$src/modules/network/.terraform/modules/x"
echo 'other' > "$src/modules/other/main.tf"
echo 'root' > "$src/main.tf"
echo 'git' > "$src/.git/config"

package_env=(MODULE_PATH=modules/network GITHUB_WORKSPACE="$src")

archive="$work/tmp/module.zip"
if (cd "$src" && run_step package "${package_env[@]}") > /dev/null; then
  if [ "$(output archive)" = "$archive" ] && [ -f "$archive" ]; then
    entries=$(python3 -c 'import sys, zipfile; print("\n".join(sorted(zipfile.ZipFile(sys.argv[1]).namelist())))' "$archive")
    expected=$(printf '%s\n' README.md main.tf templates/a.tpl)
    if [ "$entries" = "$expected" ]; then
      pass "package: 指定したディレクトリの中身だけを、zip の直下に入れる (.terraform は入れない)"
    else
      fail "package: zip の中身が違う: $(tr '\n' ' ' <<< "$entries")"
    fi
    if [ "$(output size)" = "$(stat -c %s "$archive")" ] && [ "$(output sha256)" = "$(sha256sum "$archive" | cut -d' ' -f1)" ]; then
      pass "package: zip の大きさと sha256 を出す"
    else
      fail "package: 大きさか sha256 が違う: $(cat "$work/output")"
    fi
    first=$(sha256sum "$archive" | cut -d' ' -f1)
    touch -d '2020-01-01' "$src/modules/network/main.tf"
    (cd "$src" && run_step package "${package_env[@]}") > /dev/null
    if [ "$(sha256sum "$archive" | cut -d' ' -f1)" = "$first" ]; then
      pass "package: 同じ中身からは、同じ zip を作る (更新日時に左右されない)"
    else
      fail "package: 更新日時で zip が変わる"
    fi
  else
    fail "package: zip を作らない: $(cat "$work/output")"
  fi
else
  fail "package: 正しいディレクトリで失敗した"
fi

if (cd "$src" && run_step package MODULE_PATH=. GITHUB_WORKSPACE="$src") > /dev/null &&
  python3 -c 'import sys, zipfile; n = zipfile.ZipFile(sys.argv[1]).namelist(); sys.exit(0 if "main.tf" in n and "modules/other/main.tf" in n and not any(x.startswith(".git/") for x in n) else 1)' "$archive"; then
  pass "package: path が . なら、リポジトリの根を入れる (.git は入れない)"
else
  fail "package: path が . のときの中身が違う"
fi

# 失敗する場合
mkdir -p "$src/modules/empty/sub" "$work/outside"
echo 'x' > "$src/modules/empty/sub/main.tf"
echo 'secret' > "$work/outside/credentials"
echo 'outside' > "$work/outside/main.tf"
for bad in "modules/missing" "modules/network/main.tf" "modules/empty"; do
  if (cd "$src" && run_step package MODULE_PATH="$bad" GITHUB_WORKSPACE="$src") > /dev/null 2>&1; then
    fail "package: $bad で zip を作った"
  else
    pass "package: $bad (無い・ディレクトリでない・直下に .tf が無い) は断る"
  fi
done

ln -s "$work/outside" "$src/modules/escape"
if (cd "$src" && run_step package MODULE_PATH=modules/escape GITHUB_WORKSPACE="$src") > /dev/null 2>&1; then
  fail "package: リポジトリの外を指すシンボリックリンクのディレクトリで zip を作った"
else
  pass "package: リポジトリの外を指すシンボリックリンクのディレクトリは断る"
fi
rm "$src/modules/escape"

ln -s "$work/outside/credentials" "$src/modules/network/credentials"
if (cd "$src" && run_step package "${package_env[@]}") > /dev/null 2>&1; then
  fail "package: 中にシンボリックリンクがあるのに zip を作った"
else
  pass "package: 中にシンボリックリンクがあれば断る (registry も断るため)"
fi
rm "$src/modules/network/credentials"

mkdir -p "$src/modules/big"
echo 'variable "x" {}' > "$src/modules/big/main.tf"
head -c $((5 * 1024 * 1024)) /dev/urandom > "$src/modules/big/blob.bin"
if (cd "$src" && run_step package MODULE_PATH=modules/big GITHUB_WORKSPACE="$src") > /dev/null 2>&1; then
  fail "package: 4 MB を超える zip を作った"
else
  pass "package: zip が 4 MB を超えれば断る"
fi

# ---- 上げる (upload) ----

# 偽の ID トークンの発行元 (GET /token) と registry (POST /v1/modules/...)。
# 返す状態は $work/status、受けたリクエストは $work/request.json と $work/request.body に書く
cat > "$work/server.py" <<'PY'
import http.server, json, sys, urllib.parse

work = sys.argv[2]

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        url = urllib.parse.urlparse(self.path)
        if url.path != "/token" or self.headers.get("Authorization") != "bearer request-token":
            self.send_response(401)
            self.end_headers()
            return
        audience = urllib.parse.parse_qs(url.query).get("audience", [""])[0]
        with open(f"{work}/token-audience", "w") as f:
            f.write(audience)
        body = json.dumps({"value": "header.payload.signature"}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        with open(f"{work}/request.json", "w") as f:
            json.dump({"path": self.path, "authorization": self.headers.get("Authorization"),
                       "content_type": self.headers.get("Content-Type")}, f)
        with open(f"{work}/request.body", "wb") as f:
            f.write(body)
        with open(f"{work}/status") as f:
            status = int(f.read().strip())
        if status == 201:
            payload = {"namespace": "acme", "name": "network", "system": "google", "version": "1.2.3"}
        else:
            payload = {"type": "/errors/TEST", "title": f"テストの {status}", "status": status}
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass

http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
PY
python3 "$work/server.py" "$port" "$work" &
server=$!
for _ in $(seq 1 30); do
  curl -s -o /dev/null "http://127.0.0.1:$port/" && break
  sleep 0.2
done

(cd "$src" && run_step package "${package_env[@]}") > /dev/null
upload_env=(REGISTRY_URL="http://127.0.0.1:$port" AUDIENCE="https://registry.example.io/v2" MODULE=acme/network/google VERSION=1.2.3
  ARCHIVE="$archive" ACTIONS_ID_TOKEN_REQUEST_URL="http://127.0.0.1:$port/token?api-version=2.0" ACTIONS_ID_TOKEN_REQUEST_TOKEN=request-token)

echo 201 > "$work/status"
if log=$(run_step upload "${upload_env[@]}" 2>&1); then
  request=$(cat "$work/request.json")
  if [ "$(output status)" = "201" ] &&
    [ "$(jq -r .path <<< "$request")" = "/v1/modules/acme/network/google/1.2.3" ] &&
    [ "$(jq -r .authorization <<< "$request")" = "Bearer header.payload.signature" ] &&
    [ "$(jq -r .content_type <<< "$request")" = "application/zip" ] &&
    cmp -s "$work/request.body" "$archive"; then
    pass "upload: ID トークンを付けて、zip を POST /v1/modules/<namespace>/<name>/<system>/<版> に送る"
  else
    fail "upload: リクエストが違う: $request (status=$(output status))"
  fi
  if [ "$(cat "$work/token-audience")" = "https://registry.example.io/v2" ]; then
    pass "upload: ID トークンの audience は https://<host>/v2"
  else
    fail "upload: audience が違う: $(cat "$work/token-audience")"
  fi
  if grep -qF '::add-mask::header.payload.signature' <<< "$log"; then
    pass "upload: ID トークンをログで隠す"
  else
    fail "upload: ID トークンを add-mask しない"
  fi
else
  fail "upload: 201 なのに失敗した: $log"
fi

for status in 409 401 403 422 503; do
  echo "$status" > "$work/status"
  if log=$(run_step upload "${upload_env[@]}" 2>&1); then
    fail "upload: $status なのに成功した"
  elif [ "$(output status)" = "$status" ] && grep -q "::error::" <<< "$log"; then
    pass "upload: $status なら失敗する"
  else
    fail "upload: $status のときの出力が違う: $log"
  fi
done

echo 409 > "$work/status"
if log=$(run_step upload "${upload_env[@]}" 2>&1); then
  fail "upload: 409 なのに成功した"
elif grep -q "上書きしません" <<< "$log"; then
  pass "upload: 409 なら、同じ版は上書きしないと案内する"
else
  fail "upload: 409 の案内が違う: $log"
fi

echo 422 > "$work/status"
if log=$(run_step upload "${upload_env[@]}" 2>&1); then
  fail "upload: 422 なのに成功した"
elif grep -qF "テストの 422" <<< "$log"; then
  pass "upload: registry の返したエラーの title を出す"
else
  fail "upload: registry のエラーの title を出さない: $log"
fi

if run_step upload "${upload_env[@]}" ACTIONS_ID_TOKEN_REQUEST_URL= ACTIONS_ID_TOKEN_REQUEST_TOKEN= > "$work/log" 2>&1; then
  fail "upload: id-token: write が無いのに成功した"
elif grep -q "id-token: write" "$work/log"; then
  pass "upload: ID トークンを取れなければ、id-token: write を案内して失敗する"
else
  fail "upload: ID トークンを取れないときの案内が違う: $(cat "$work/log")"
fi

if run_step upload "${upload_env[@]}" ACTIONS_ID_TOKEN_REQUEST_TOKEN=wrong > /dev/null 2>&1; then
  fail "upload: ID トークンを取れないのに成功した"
else
  pass "upload: ID トークンの発行元が断れば失敗する"
fi

if run_step upload "${upload_env[@]}" REGISTRY_URL="http://127.0.0.1:1" > /dev/null 2>&1; then
  fail "upload: registry に繋がらないのに成功した"
else
  pass "upload: registry に繋がらなければ失敗する"
fi

echo
if [ "$failures" -gt 0 ]; then
  echo "$failures 件失敗" >&2
  exit 1
fi
echo "すべて通った"
