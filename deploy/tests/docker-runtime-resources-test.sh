#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"

fail() {
  printf 'docker runtime resources test failed: %s\n' "$1" >&2
  exit 1
}

assert_line() {
  file=$1
  line=$2
  grep -Fqx "$line" "$file" || fail "$file is missing: $line"
}

assert_count() {
  file=$1
  line=$2
  expected=$3
  actual=$(grep -Fxc "$line" "$file" || true)
  [ "$actual" -eq "$expected" ] || fail "$file has $actual occurrences of '$line', expected $expected"
}

test -s backend/resources/model-pricing/model_prices_and_context_window.json || \
  fail 'fallback pricing data is missing or empty'

# 2026-08-23 改：Dockerfile.goreleaser / .goreleaser*.yaml 已随 GitHub 独立化删除
# （上游的发布产线，本项目不用，见 NOTICE.md），相应断言一并移除。
# 保留的两条守的是「离线兜底价目表要进镜像」——真正在用的两个 Dockerfile。
assert_line deploy/Dockerfile 'COPY --from=backend-builder --chown=sub2api:sub2api /app/backend/resources /app/resources'
assert_line Dockerfile 'COPY --from=backend-builder --chown=sub2api:sub2api /app/backend/resources /app/resources'

printf 'docker runtime resources test passed\n'
