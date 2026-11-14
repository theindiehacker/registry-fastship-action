#!/usr/bin/env bash
# publish.yml (イメージの publish の reusable workflow) の step を抜き出し、ローカルの registry (registry:2) で動かして確かめる。
# Google Cloud の認証 (WIF) と docker login は確かめない (本物の Artifact Registry が要るため)。
# 要るもの: docker・jq・yq (jq の書き方が通るもの)・curl。使い方: bash tests/publish_test.sh
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
workflow="$root/.github/workflows/publish.yml"
work=$(mktemp -d)
port=${REGISTRY_PORT:-5000}
registry="localhost:$port"
container="publish-test-registry-$$"
failures=0

cleanup() {
  docker rm -f "$container" > /dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

# publish.yml の id の step の run を取り出す
step_script() {
  local script
  script=$(yq -r ".jobs.publish.steps[] | select(.id == \"$1\") | .run" "$workflow")
  if [ -z "$script" ] || [ "$script" = "null" ]; then
    echo "publish.yml に id: $1 の step がありません" >&2
    return 1
  fi
  printf '%s\n' "$script"
}

# publish.yml の id の step の if を取り出す
step_if() {
  yq -r ".jobs.publish.steps[] | select(.id == \"$1\") | .if" "$workflow"
}

# step を GitHub Actions の bash と同じオプションで動かす。出力は $work/output に書かれる
run_step() {
  local id=$1
  shift
  : > "$work/output"
  step_script "$id" > "$work/$id.sh"
  env GITHUB_OUTPUT="$work/output" GITHUB_STEP_SUMMARY="$work/summary" RUNNER_TEMP="$work" "$@" \
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

# ---- 入力の検査 (check) ----

check_env=(IMAGE=sample VERSION=v1.0.0 REPOSITORY=asia-northeast1-docker.pkg.dev/my-project/apps CONTEXT=. FILE=)

if run_step check "${check_env[@]}" CONTEXT=services/api FILE=services/api/Dockerfile.prod > /dev/null; then
  pass "check: リポジトリの中の相対パスの context・file を受け付ける"
else
  fail "check: リポジトリの中の context・file を断った"
fi

if run_step check "${check_env[@]}" > /dev/null; then
  if [ "$(output host)" = "asia-northeast1-docker.pkg.dev" ] &&
    [ "$(output image)" = "asia-northeast1-docker.pkg.dev/my-project/apps/sample" ] &&
    [ "$(output path)" = "my-project/apps/sample" ]; then
    pass "check: 正しい入力から、ホスト・イメージ・パスを出す"
  else
    fail "check: 出力が違う: $(cat "$work/output")"
  fi
else
  fail "check: 正しい入力を断った"
fi

if run_step check IMAGE=Sample VERSION=v1.0.0 REPOSITORY=asia-northeast1-docker.pkg.dev/my-project/apps > /dev/null 2>&1 &&
  [ "$(output image)" = "asia-northeast1-docker.pkg.dev/my-project/apps/sample" ]; then
  fail "check: 大文字のイメージ名を受け付けた"
else
  pass "check: 大文字のイメージ名を断る"
fi

for bad in \
  "IMAGE=../sample" \
  "IMAGE=sample:v1" \
  "IMAGE=" \
  "VERSION=latest" \
  "VERSION=" \
  "VERSION=-v1" \
  "VERSION=v1/2" \
  "REPOSITORY=docker.io/library" \
  "REPOSITORY=https://asia-northeast1-docker.pkg.dev/my-project/apps" \
  "REPOSITORY=asia-northeast1-docker.pkg.dev/my-project" \
  "REPOSITORY=asia-northeast1-docker.pkg.dev/my-project/apps/sample" \
  "CONTEXT=/home/runner" \
  "CONTEXT=.." \
  "CONTEXT=app/../.." \
  "CONTEXT=~" \
  "FILE=/etc/Dockerfile" \
  "FILE=../Dockerfile"; do
  if run_step check "${check_env[@]}" "$bad" > /dev/null 2>&1; then
    fail "check: $bad を受け付けた"
  else
    pass "check: $bad を断る"
  fi
done

# ---- 同じタグがあればスキップする (exists・build) ----

docker run -d --rm --name "$container" -p "$port:5000" registry:2 > /dev/null
for _ in $(seq 1 30); do
  curl -fsS "http://$registry/v2/" > /dev/null 2>&1 && break
  sleep 1
done

mkdir -p "$work/src/app"
cat > "$work/src/app/Dockerfile" <<'EOF'
FROM busybox:1.37 AS base
RUN echo base > /stage
FROM base AS api
RUN echo api > /stage
FROM base AS worker
RUN echo worker > /stage
EOF

image_path="test/apps/sample"
exists_env=(REGISTRY_URL="http://$registry" IMAGE_PATH="$image_path" VERSION=v1.0.0 TOKEN=dummy)
build_env=(IMAGE="$registry/$image_path" VERSION=v1.0.0 CONTEXT="$work/src" FILE="$work/src/app/Dockerfile" TARGET=api
  GITHUB_REPOSITORY=theindiehacker/example GITHUB_SHA=0123456789abcdef GITHUB_SERVER_URL=https://github.com)

if run_step exists "${exists_env[@]}" > /dev/null && [ "$(output exists)" = "false" ]; then
  pass "exists: タグが無ければ exists=false"
else
  fail "exists: タグが無いのに exists=false にならない: $(cat "$work/output")"
fi

if [ "$(step_if build)" = "steps.exists.outputs.exists == 'false'" ] && [ "$(step_if checkout)" = "steps.exists.outputs.exists == 'false'" ]; then
  pass "build・checkout は、タグが無いときだけ動く"
else
  fail "build・checkout の if が違う: $(step_if build) / $(step_if checkout)"
fi

built=""
if run_step build "${build_env[@]}"; then
  built=$(output digest)
  if [[ "$built" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    pass "build: target を指定してビルドして push し、digest を出す"
  else
    fail "build: digest が違う: $built"
  fi
else
  fail "build: ビルドか push に失敗した"
fi

if [ -n "$built" ]; then
  docker image rm -f "$registry/$image_path:v1.0.0" > /dev/null 2>&1 || true
  if docker pull -q "$registry/$image_path@$built" > /dev/null &&
    [ "$(docker run --rm "$registry/$image_path@$built" cat /stage)" = "api" ]; then
    pass "build: 出力の digest で pull でき、指定した target の中身になる"
  else
    fail "build: 出力の digest で pull できないか、target が違う"
  fi
  source_label=$(docker image inspect "$registry/$image_path@$built" --format '{{index .Config.Labels "org.opencontainers.image.source"}}')
  if [ "$source_label" = "https://github.com/theindiehacker/example" ]; then
    pass "build: イメージに、ビルドしたリポジトリのラベルを付ける"
  else
    fail "build: org.opencontainers.image.source が違う: $source_label"
  fi
fi

if run_step exists "${exists_env[@]}" > /dev/null && [ "$(output exists)" = "true" ] && [ "$(output digest)" = "$built" ]; then
  pass "exists: 同じタグがあれば exists=true で、push したときと同じ digest を出す"
else
  fail "exists: 同じタグがあるのに、exists=true と同じ digest にならない: $(cat "$work/output") (push: $built)"
fi

if run_step exists REGISTRY_URL="http://$registry" IMAGE_PATH="$image_path" VERSION=v2.0.0 TOKEN=dummy > /dev/null && [ "$(output exists)" = "false" ]; then
  pass "exists: ほかのタグは exists=false"
else
  fail "exists: ほかのタグなのに exists=false にならない"
fi

# registry を読めないときは、無いとみなしてビルドせず、失敗する
if run_step exists REGISTRY_URL="http://localhost:1" IMAGE_PATH="$image_path" VERSION=v1.0.0 TOKEN=dummy > /dev/null 2>&1; then
  fail "exists: registry に繋がらないのに成功した: $(cat "$work/output")"
else
  pass "exists: registry に繋がらなければ失敗する"
fi
# 401 (認証の失敗など) は、無いとみなさずに失敗する
python3 -c '
import http.server, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_HEAD(self):
        self.send_response(401)
        self.end_headers()
    def log_message(self, *args):
        pass
http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
' "$((port + 1))" &
denied_server=$!
sleep 1
if run_step exists REGISTRY_URL="http://127.0.0.1:$((port + 1))" IMAGE_PATH="$image_path" VERSION=v1.0.0 TOKEN=dummy > /dev/null 2>&1; then
  fail "exists: 401 なのに成功した: $(cat "$work/output")"
else
  pass "exists: 401 なら失敗する"
fi
kill "$denied_server"

echo
if [ "$failures" -gt 0 ]; then
  echo "$failures 件失敗" >&2
  exit 1
fi
echo "すべて通った"
