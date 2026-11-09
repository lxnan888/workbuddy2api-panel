#!/usr/bin/env bash
# 项目测试入口：测试文件全部放在 tests/ 镜像目录，运行时注入临时源码副本，
# 不污染 internal/、cmd/ 等前后端源码目录，也不需要任何账号、token 或代理凭据。
#
# 目录约定：
#   tests/internal/... 与 tests/cmd/... 镜像保存 *_test.go / testdata
#   运行时由本脚本复制源码到临时目录，再把测试文件按相对路径注入
#
# 用法：
#   ./tests/run.sh go        运行全部 Go 测试（默认）
#   ./tests/run.sh unit      只跑限速/燃烧/配置等定向包
#   ./tests/run.sh vet       运行 go vet
#   ./tests/run.sh race      运行定向包 race 检测
#   ./tests/run.sh build     只做全仓库构建
#   ./tests/run.sh example   校验 config.example.json 可被解析
# 可选环境变量：
#   GO        go 可执行文件（默认 go）
#   GOPROXY   Go 模块代理（默认 https://goproxy.cn,direct）
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO="${GO:-go}"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

TMP="$(mktemp -d "${TMPDIR:-/tmp}/workbuddy2api-tests.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

# 只复制编译与测试所需的源码与配置样例；不复制 auths/、data/、config.json
# 等可能含凭据的文件，避免测试环境接触真实账号。
cp -a "$ROOT/cmd" "$ROOT/internal" "$ROOT/go.mod" "$ROOT/go.sum" "$TMP/"
cp -a "$ROOT/config.example.json" "$TMP/"

# 把 tests/ 下的测试文件按相对路径注入临时源码镜像：
#   tests/internal/upstream/x_test.go -> $TMP/internal/upstream/x_test.go
#   tests/cmd/server/y_test.go        -> $TMP/cmd/server/y_test.go
while IFS= read -r -d '' test_file; do
  relative="${test_file#"$ROOT/tests/"}"
  destination="$TMP/${relative%/*}"
  mkdir -p "$destination"
  cp "$test_file" "$destination/"
done < <(find "$ROOT/tests" -type f -name '*_test.go' -print0)

# 测试基准数据（testdata 目录）同样注入。
while IFS= read -r -d '' data_dir; do
  relative="${data_dir#"$ROOT/tests/"}"
  destination="$TMP/${relative%/*}"
  mkdir -p "$destination"
  cp -a "$data_dir" "$destination/"
done < <(find "$ROOT/tests" -type d -name testdata -print0)

cd "$TMP"

TARGET_PKGS="./cmd/server ./internal/upstream ./internal/server ./internal/pool ./internal/usage"

cmd="${1:-go}"
case "$cmd" in
  go)
    "$GO" build ./...
    "$GO" test ./... -count=1
    ;;
  unit)
    "$GO" test $TARGET_PKGS -count=1
    ;;
  vet)
    "$GO" vet ./...
    ;;
  race)
    "$GO" test $TARGET_PKGS -race -count=1
    ;;
  build)
    "$GO" build ./...
    ;;
  example)
    "$GO" test ./cmd/server -run 'TestExampleConfigParses' -count=1
    ;;
  *)
    echo "未知子命令: $cmd" >&2
    echo "可用: go | unit | vet | race | build | example" >&2
    exit 2
    ;;
esac
