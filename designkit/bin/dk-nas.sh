#!/usr/bin/env bash
# 把本机代码同步到 NAS 上的 Docker 环境跑起来。
#
# 为什么需要这个：
#   monica 这台 Mac 上没有 Docker、也没有 Go 编译器，本机既跑不起来也编译不了。
#   NAS（群晖，x86_64，28G 内存，Docker 24.0.2）上什么都有。
#   所以开发循环是：Mac 上写代码 → 同步到 NAS → 在 NAS 上编译/跑/测。
#
# 用法：
#   designkit/bin/dk-nas.sh sync          只同步代码
#   designkit/bin/dk-nas.sh up            同步 + 起开发环境（后台）
#   designkit/bin/dk-nas.sh down          停掉
#   designkit/bin/dk-nas.sh logs [服务名]  看日志
#   designkit/bin/dk-nas.sh build         同步 + 只编译后端（最快的对错检查）
#   designkit/bin/dk-nas.sh test          同步 + 跑单元测试
#   designkit/bin/dk-nas.sh ci            推送前必跑，完整预演 CI（gofmt+vet → golangci-lint → 单测 → 前端 lint/类型/关键 vitest）
#   designkit/bin/dk-nas.sh rebuild       改了代码之后：重新构建镜像并替换容器
#   designkit/bin/dk-nas.sh restart       改了「商品图设置」之后：重启后端让配置生效
#   designkit/bin/dk-nas.sh sh            开一个 NAS 上的 shell
#
# ⚠ up 和 rebuild 的区别（很容易搞错，搞错了会以为改动没生效）：
#   up      只是「把没起来的容器起起来」。compose 配置没变时 Docker 判定
#           「已是最新」，**不重建**——改了代码它一点反应都没有。
#   rebuild 重新打包镜像。18080 上跑的前端是**打包进镜像**的，所以改了
#           .vue/.ts 必须用它；而 13000 那个开发服务器挂的是源码、会热更新。
#           两个端口表现不一致，最容易在这里被骗。
#
# 认证：
#   SSH 用密钥 ~/.ssh/dk_nas（已装到 NAS 的 authorized_keys）。
#   NAS 上 docker 需要 sudo，密码从下面三处按顺序取，**绝不写进本文件**：
#     1. 环境变量 DK_NAS_PASS
#     2. 环境变量 DK_NAS_PASS_FILE 指向的文件
#     3. 交互式提示（不回显）
#
# 端口（刻意避开 NAS 上已占用的）：
#   正式入口 http://192.168.31.235:18080（前端和后端都在这个端口，对局域网开放）
#   热更新开发服务器 13000 只绑 NAS 回环（2026-08-23 收口，理由见 NAS_ENV 注释），
#   要在 Mac 上看它：ssh -L 13000:127.0.0.1:13000 dk-nas → 浏览器开 http://localhost:13000
#   注意 3000 被 moviepilot 占了、8090 是 sub2api、8787 是老 designkit，都不能碰。

set -euo pipefail

NAS_HOST="${DK_NAS_HOST:-192.168.31.235}"
NAS_USER="${DK_NAS_USER:-Monica}"
NAS_KEY="${DK_NAS_KEY:-$HOME/.ssh/dk_nas}"
REMOTE_DIR="${DK_NAS_DIR:-/volume3/docker/designkit-dev/src}"
DOCKER="/usr/local/bin/docker"

# ⚠ 必须同时加载两个文件。designkit-dev.yml 是**叠加文件**，单独用不起来：
#   它顶层没有 networks: 块（但服务里引用了 sub2api-network）、
#   它的 sub2api 条目只有 environment 没有 image/build、
#   postgres 和 redis 压根不在里面。
# 只带一个 -f 会直接报 "service refers to undefined network"。
COMPOSE_FILES="-f deploy/docker-compose.dev.yml -f deploy/docker-compose.designkit-dev.yml"

# NAS 上的端口和绑定地址。
# ⚠ 绑定地址是**两个**变量，2026-08-23 从一个 BIND_HOST 拆开的（两个端口原来共用它，
#   为了 18080 能被局域网访问只好连 13000 一起绑 0.0.0.0）：
#   - SERVER_BIND=0.0.0.0     18080 正式入口，运营要从局域网访问，必须对外
#   - DEV_BIND=127.0.0.1      13000 vite 开发服务器，**必须只绑回环**：
#     它的 /@fs/ 能读项目目录下任意文件，对局域网开放等于把源码整个敞开。
#     要在 Mac 上看热更新页面，开个隧道：ssh -L 13000:127.0.0.1:13000 dk-nas
# ⚠ DOCKER_BUILDKIT=0 只用在**不构建**的命令上（up / down / logs / config）。
#   这台 NAS 上 BuildKit 拉基础镜像的元数据时会走 IPv6 连 auth.docker.io 然后
#   超时（i/o timeout），而 `docker pull` 走守护进程自己的路径是好的。
#   现象很有迷惑性——同一台机器上 pull 能成、build 不能成。
NAS_ENV="DEV_BIND=127.0.0.1 SERVER_BIND=0.0.0.0 VITE_DEV_PORT=13000 SERVER_PORT=18080 DOCKER_BUILDKIT=0"

# ⚠⚠ 但**构建必须用 BuildKit**（DOCKER_BUILDKIT=1）。根目录的 Dockerfile 从头到尾
#   都是 BuildKit 专属语法，经典构建器一行都跑不了：
#     - `# syntax=docker/dockerfile:1.7`（第 1 行）
#     - `FROM --platform=${BUILDPLATFORM}`（BUILDPLATFORM 是 BuildKit 自动注入的 ARG，
#       经典构建器下恒为空 → 报 `"" is an invalid component of ""`，就死在这一步）
#     - `RUN --mount=type=cache,...`（3 处）
#
#   把 BUILDKIT=0 用在构建上是个**沉默的坑**：`up` 从来不构建，所以这个矛盾
#   一直没被触发；2026-08-14 第一次跑 rebuild 才炸出来，而且因为外层套了 `| tail`，
#   退出码还是 0，看起来像成功了。
#
#   前提：基础镜像必须已经在本地（见下面 require_base_images 的清单）。
#   都在的话 BuildKit 不需要联网解析，那个 IPv6 超时就不会发生。
BUILD_ENV="DEV_BIND=127.0.0.1 SERVER_BIND=0.0.0.0 VITE_DEV_PORT=13000 SERVER_PORT=18080 DOCKER_BUILDKIT=1"

# 构建要用到的基础镜像。缺了 BuildKit 会去联网解析，然后撞上那个 IPv6 超时，
# 报出来的错跟「镜像没拉」八竿子打不着，所以在这里先自己查一遍。
BASE_IMAGES="docker/dockerfile:1.7 node:24-alpine golang:1.26.6-alpine alpine:3.21 postgres:18-alpine python:3.12-slim-bookworm"

# ci 子命令用的 golangci-lint 镜像。版本必须跟 CI 一致
# （.github/workflows/backend-ci.yml 里 golangci-lint-action 钉的是 v2.9）。
GOLANGCI_IMAGE="golangci/golangci-lint:v2.9"

cd "$(git rev-parse --show-toplevel)"

# ---- 取 sudo 密码，只放进内存 ----
get_pass() {
  if [ -n "${DK_NAS_PASS:-}" ]; then printf '%s\n' "$DK_NAS_PASS"; return; fi
  if [ -n "${DK_NAS_PASS_FILE:-}" ] && [ -f "$DK_NAS_PASS_FILE" ]; then cat "$DK_NAS_PASS_FILE"; return; fi
  printf 'NAS sudo 密码: ' >&2
  local p; read -rs p; printf '\n' >&2
  printf '%s\n' "$p"
}

SSH_OPTS=(-i "$NAS_KEY" -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10)

# 在 NAS 上以 root 跑一条命令。密码走标准输入，不出现在任何命令行里。
nas_sudo() {
  get_pass | ssh "${SSH_OPTS[@]}" "$NAS_USER@$NAS_HOST" "sudo -S -p '' sh -c $(printf '%q' "$*")"
}

nas_plain() {
  ssh "${SSH_OPTS[@]}" "$NAS_USER@$NAS_HOST" "$@"
}

# NAS 上没有 git，所以同步是「照搬当前工作区」，不是 git clone。
# 这意味着：本机没保存的改动同步不过去，同步过去的也不受版本控制——
# NAS 上那份纯粹是「拿去编译和运行的副本」，不要在上面改代码。

# ⚠ 不要用 macOS 自带的 rsync。Darwin 24 把它换成了 openrsync（自称 2.6.9），
#   `-e`、`--rsh`、`RSYNC_RSH` 全都不生效——它会无视密钥去要密码然后失败。
#   homebrew 装的 GNU rsync 是好的，装了就用；没装就走 tar 管道（零依赖）。
#   仓库去掉 .git 和 node_modules 后约 62MB，内网几秒钟，不值得为增量同步再装东西。
do_sync() {
  # deploy/ 下那三个是 NAS 上的运行时数据（数据库文件、图片），
  # 以及 deploy/.env（密码，只在 NAS 上生成，绝不进仓库）。
  # 同步是单向的「本机 → NAS」，这些一律不碰。
  local excludes=(--exclude '.git' --exclude 'node_modules' --exclude 'frontend/dist'
                  --exclude 'deploy/postgres_data' --exclude 'deploy/redis_data'
                  --exclude 'deploy/data' --exclude 'deploy/.env' --exclude '.DS_Store'
                  --exclude '._*')
  if [ -x /opt/homebrew/bin/rsync ]; then
    echo "→ 同步代码到 $NAS_HOST:${REMOTE_DIR}（GNU rsync 增量）"
    /opt/homebrew/bin/rsync -az --delete "${excludes[@]}" \
      -e "ssh -i $NAS_KEY -o StrictHostKeyChecking=accept-new" \
      ./ "$NAS_USER@$NAS_HOST:$REMOTE_DIR/"
  else
    echo "→ 同步代码到 $NAS_HOST:${REMOTE_DIR}（tar 管道，全量）"
    nas_plain "mkdir -p $REMOTE_DIR"
    # COPYFILE_DISABLE=1 + --no-xattrs：不然 macOS 会塞一堆 com.apple.provenance
    # 扩展属性，NAS 上的 GNU tar 每个文件都要抱怨一行。
    # ⚠ COPYFILE_DISABLE=1 + --no-xattrs 缺一不可。少了它们，macOS 的 tar 会给每个
    #   带扩展属性的文件多打一个 `._原名` 的「资源分支」文件。后果不是不好看——
    #   上游用 `//go:embed *.sql` 收迁移文件，`._001_init.sql` 会被一起编进镜像，
    #   启动时当成迁移去跑，然后 `pq: invalid message format`，后端无限重启。
    #   （2026-08-13 真踩过：3849 个 `._` 文件，全是第一次同步留下的。）
    COPYFILE_DISABLE=1 tar --no-xattrs -czf - "${excludes[@]}" . \
      | nas_plain "cd $REMOTE_DIR && tar xzf -"
    # tar 只添加不删除，所以历史垃圾会一直留着。每次同步后扫一遍。
    nas_plain "find $REMOTE_DIR -name '._*' -delete 2>/dev/null; true"
  fi
  echo "✓ 同步完成"
  check_sync_drift
}

# ---- 反向清单对照（2026-08-24 加）：抓「本地已删、NAS 上还留着」的幽灵文件 ----
# tar 管道只添加不删除，本地删掉的文件会在 NAS 上一直活着；而 //go:embed 和
# go test 不看 git、只看目录里有什么——幽灵文件会被编进产物、让测试时好时坏。
# rsync --delete 那条路径理论上没这个问题，但两条路径都核一遍，顺手的事。
#
# 判定「幽灵」要过两道：① 不在本地 git ls-files 清单里（含未跟踪未忽略的新文件）；
# ② 本地磁盘上确实不存在。第二道不能省——被 .gitignore 吞掉但本地还在的文件
# （CLAUDE.md 第一节说的 scripts/tests 陷阱）只过得了第一道，删了它下次同步
# 又会传上去，白折腾还吓人。
check_sync_drift() {
  # 这些是 NAS 侧构建自然产生的产物目录，不是「本地删了没同步」的幽灵，
  # 永远排除（web 构建把前端打进 backend/internal/web/dist）：
  local DRIFT_EXCLUDE="backend/internal/web/dist/"
  local scope="backend/internal frontend/src designkit"
  local local_list nas_list ghosts f
  # shellcheck disable=SC2086  # scope 特意按空格拆成多个路径参数
  local_list=$(git ls-files --cached --others --exclude-standard -- $scope | LC_ALL=C sort)
  # 两端都用 LC_ALL=C 排序：Mac 和 NAS 的 locale 排序规则不一样，comm 会被带偏。
  # ssh 偶发失败时清单为空 → 本次跳过对照，不让它把刚成功的同步整个报废。
  nas_list=$( { nas_plain "cd $REMOTE_DIR 2>/dev/null && find $scope -type f 2>/dev/null; true" || true; } | LC_ALL=C sort )
  [ -n "$nas_list" ] || return 0

  ghosts=""
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    case "$f" in *..*|/*) continue ;; esac   # 防御：绝对路径或带 .. 的一律不碰
    case "$f" in "$DRIFT_EXCLUDE"*) continue ;; esac   # 构建产物目录，见上
    [ -e "$f" ] && continue                  # 本地还在（哪怕被 git 忽略）→ 不是幽灵
    ghosts="${ghosts}${f}"$'\n'
  done < <(comm -13 <(printf '%s\n' "$local_list") <(printf '%s\n' "$nas_list"))

  [ -n "$ghosts" ] || return 0

  if [ "${DK_SYNC_PRUNE:-0}" = "1" ]; then
    echo "→ DK_SYNC_PRUNE=1：删除 NAS 侧幽灵文件（只删下面逐个列出的文件，绝不 rm -rf 目录）"
    printf '%s' "$ghosts" | nas_plain "cd $REMOTE_DIR && while IFS= read -r f; do rm -f -- \"\$f\" && echo \"  已删除 \$f\" || echo \"  ✗ 删不掉 \$f\"; done"
    echo "✓ 幽灵文件清完（空目录会留下，无害）"
  else
    echo "⚠ NAS 侧幽灵文件（本地已删，NAS 还留着；go build / go:embed / 测试会把它们当真）："
    printf '%s' "$ghosts" | sed 's/^/    /'
    echo "  确认后加环境变量重跑一次即可删除（只删上面这些文件）："
    echo "    DK_SYNC_PRUNE=1 designkit/bin/dk-nas.sh sync"
  fi
}

# 在 NAS 上用一次性 Go 容器跑命令（本机没有 Go）。
#
# ⚠ 用 Debian 版 golang:1.26.6，不要用 -alpine。
#   alpine 版要先 `apk add git build-base`，而这一步在这台 NAS 上会**挂死**
#   （实测跑了 12 分钟没动静，容器停在 apk 上）。Debian 版自带 git 和 gcc，
#   一个包都不用装，直接开编。镜像大 ~600MB，但只拉一次。
#
# ⚠ GOPROXY 必须走国内镜像。实测容器内：
#   goproxy.cn → 200，proxy.golang.org → 000（不通）。
#   用 Go 默认的 proxy.golang.org 会卡到超时。
go_in_container() {
  local goflags="-e GOPROXY=https://goproxy.cn,direct -e GOFLAGS=-mod=mod"
  if [ -n "${DK_PROXY:-}" ]; then
    goflags="$goflags -e ALL_PROXY=$DK_PROXY -e HTTPS_PROXY=$DK_PROXY -e HTTP_PROXY=$DK_PROXY"
  fi
  # 缓存目录要先存在，否则 docker 报 "Bind mount failed: ... does not exist"。
  # 缓存留在 NAS 上，第二次编译就快得多。
  nas_sudo "mkdir -p /volume3/docker/designkit-dev/gocache /volume3/docker/designkit-dev/gomod && \
    $DOCKER run --rm \
    -v $REMOTE_DIR:/src -w /src/backend \
    -v /volume3/docker/designkit-dev/gocache:/root/.cache/go-build \
    -v /volume3/docker/designkit-dev/gomod:/go/pkg/mod \
    $goflags \
    golang:1.26.6 bash -c '$1'"
}

# ---- 下面几个是 check / ci 共用的步骤（2026-08-24 抽出来，供 ci 完整预演 CI）----

# gofmt + go vet。2026-08-24 起真的会失败（原来只打印退出码然后装没事）：
# CI 会卡这两项，本地也该卡住，不然「check 过了、CI 红了」这种事还会再来。
run_gofmt_vet() {
  echo "→ gofmt（CI 的 formatters 检查会卡这个）"
  go_in_container "bad=\$(gofmt -l ./internal/designkit/ ./internal/server/router.go); if [ -n \"\$bad\" ]; then echo \"这些文件需要 gofmt -w：\"; echo \"\$bad\"; exit 1; fi" || return 1
  echo "→ go vet"
  go_in_container "go vet ./internal/designkit/... 2>&1 | grep -v \"^go: downloading\" | head -40; exit \${PIPESTATUS[0]}" || return 1
}

# golangci-lint，跟 CI 完全同版（v2.9）、同参数（--timeout=30m）、同范围（backend 全仓）。
run_golangci() {
  if ! nas_sudo "$DOCKER image inspect $GOLANGCI_IMAGE >/dev/null 2>&1"; then
    echo "✗ NAS 上没有 golangci-lint 镜像（约 1GB，只拉一次）。先 pull 再重跑："
    echo "    sudo docker pull $GOLANGCI_IMAGE"
    echo "  ci 不代拉：拉镜像要好几分钟，代拉会让「预演 CI」看起来像卡死。"
    return 1
  fi
  # 缓存挂 /volume3/docker/designkit-dev/golangci：go build 缓存和 lint 缓存都在
  # 容器的 /root/.cache 下面，挂这一个目录就都保住了，第二次跑快一个数量级。
  # gomod 复用 go_in_container 的那个卷，模块不用重下。
  nas_sudo "mkdir -p /volume3/docker/designkit-dev/golangci && \
    $DOCKER run --rm \
    -v $REMOTE_DIR:/src -w /src/backend \
    -v /volume3/docker/designkit-dev/golangci:/root/.cache \
    -v /volume3/docker/designkit-dev/gomod:/go/pkg/mod \
    -e GOPROXY=https://goproxy.cn,direct -e GOFLAGS=-mod=mod \
    $GOLANGCI_IMAGE golangci-lint run --timeout=30m" || return 1
}

# 单元测试，跟 CI 的 make test-unit 相同（go test -tags=unit ./...）。
# 跟 test 子命令的区别：失败会真的失败（test 只打印退出码，方便人眼看；
# ci 要靠退出码停下来）。引号规矩见 test 子命令的注释：只能用转义双引号。
run_unit_tests_strict() {
  echo "→ 在 NAS 上跑单元测试（-tags=unit，跟 CI 的 make test-unit 相同）"
  go_in_container "go test -tags=unit ./... 2>&1 | grep -v \"^go: downloading\" | grep -v \"^ok  \" | grep -v \"no test files\" | head -60; exit \${PIPESTATUS[0]}"
}

# CI 的「关键 vitest」清单以根 Makefile 的 FRONTEND_CRITICAL_VITEST 为准，
# 运行时解析，不在这里抄一份——抄了就会漂移，CI 加测试这里不知道。
frontend_critical_specs() {
  awk '
    /^FRONTEND_CRITICAL_VITEST[ \t]*:=/ { grab = 1; sub(/^[^=]*=[ \t]*/, "") }
    grab {
      more = /\\[ \t]*$/
      gsub(/\\[ \t]*$/, "")
      gsub(/^[ \t]+|[ \t]+$/, "")
      if ($0 != "") printf "%s ", $0
      if (!more) exit
    }
  ' Makefile
}

# 前端三件套，跟 CI 的 make test-frontend 相同：lint:check → typecheck → 关键 vitest。
# 容器手法照 web 子命令（node:22-alpine + npx pnpm@9 + npmmirror + pnpm 缓存卷）。
run_web_ci() {
  local specs
  specs="$(frontend_critical_specs)"
  if [ -z "${specs// /}" ]; then
    echo "✗ 没能从根 Makefile 解析出 FRONTEND_CRITICAL_VITEST 清单（Makefile 改了格式？）。"
    echo "  不猜清单，直接停。修好解析或清单后重跑。"
    return 1
  fi
  echo "→ 前端 lint:check + typecheck + 关键 vitest（跟 make test-frontend 相同）"
  nas_sudo "mkdir -p /volume3/docker/designkit-dev/pnpm && \
    $DOCKER run --rm \
    -v $REMOTE_DIR:/src -w /src/frontend \
    -v /volume3/docker/designkit-dev/pnpm:/pnpm-store \
    -e npm_config_registry=https://registry.npmmirror.com \
    --entrypoint sh \
    node:22-alpine -c '
      set -e -o pipefail
      export PNPM_HOME=/pnpm-store
      npx --yes pnpm@9 config set store-dir /pnpm-store >/dev/null 2>&1 || true
      echo \"--- 装依赖 ---\"
      npx --yes pnpm@9 install --frozen-lockfile 2>&1 | tail -6
      echo \"--- eslint（lint:check）---\"
      npx --yes pnpm@9 run lint:check
      echo \"--- vue-tsc（typecheck）---\"
      npx --yes pnpm@9 run typecheck
      echo \"--- 关键 vitest（清单取自根 Makefile 的 FRONTEND_CRITICAL_VITEST）---\"
      npx --yes pnpm@9 exec vitest run $specs
    '" || return 1
}

ci_fail() {
  echo
  echo "✗ CI 预演挂在这一步：$1（后面的步骤没有跑）"
  echo "  修完重跑：designkit/bin/dk-nas.sh ci"
  exit 1
}

case "${1:-up}" in
  sync) do_sync ;;

  build)
    do_sync
    echo "→ 在 NAS 上编译后端（本机没有 Go）"
    # 滤掉 "go: downloading ..." 噪音，否则首次编译几百行下载会把真正的错误顶出屏幕
    go_in_container "go build ./... 2>&1 | grep -v \"^go: downloading\" | head -80; echo \"退出码=\${PIPESTATUS[0]}\""
    ;;

  test)
    do_sync
    echo "→ 在 NAS 上跑单元测试"
    # ⚠ 这里**只能用转义的双引号**，不能用单引号。go_in_container 最后是
    #   `bash -c '$1'`，调用方这串字符会被原样塞进一对单引号里 ——
    #   里面再出现一个单引号就提前闭合，远端实际执行的变成半截命令，
    #   而且退出码变成最后那个 grep 的（永远是 0），**go test 失败会被吞成成功**。
    #   2026-08-14 之前这一行用的是 '^go: downloading'，正是这个毛病。
    #   build/check 两条一直用的是转义双引号，所以没中招。
    go_in_container "go test -tags=unit ./... 2>&1 | grep -v \"^go: downloading\" | grep -v \"^ok  \" | grep -v \"no test files\" | head -60; echo \"退出码=\${PIPESTATUS[0]}\""
    ;;

  # 前端类型检查 + 构建。本机虽然有 node，但没装依赖，
  # 而且 CI 跑的是 pnpm，版本对不上会给出误导性的错。统一在 NAS 上跑。
  web)
    do_sync
    echo "→ 在 NAS 上装前端依赖并构建"
    nas_sudo "mkdir -p /volume3/docker/designkit-dev/pnpm && \
      $DOCKER run --rm \
      -v $REMOTE_DIR:/src -w /src/frontend \
      -v /volume3/docker/designkit-dev/pnpm:/pnpm-store \
      -e npm_config_registry=https://registry.npmmirror.com \
      --entrypoint sh \
      node:22-alpine -c '
        # package.json 里没有 packageManager 字段，corepack enable 装不出 pnpm 的
        # 可执行文件（实测 \"sh: pnpm: not found\"）。直接用 npx 拉一个固定大版本，
        # 仓库里是 pnpm-lock.yaml（v9 格式），所以钉 pnpm@9。
        set -e
        export PNPM_HOME=/pnpm-store
        npx --yes pnpm@9 config set store-dir /pnpm-store >/dev/null 2>&1 || true
        echo \"--- 装依赖 ---\"
        npx --yes pnpm@9 install --frozen-lockfile 2>&1 | tail -6
        echo \"--- 类型检查 + 构建（build = vue-tsc -b && vite build）---\"
        npx --yes pnpm@9 run build 2>&1 | tail -40
      '"
    ;;

  # 最快的推送前检查（格式 / vet）。完整预演 CI 用 ci。
  check)
    do_sync
    run_gofmt_vet
    ;;

  # 推送前必跑：完整预演 CI（.github/workflows/backend-ci.yml 的主体），
  # 任何一步挂了就地停下并指明是哪一步。
  # 没覆盖的三个 CI job（只在 GitHub 上跑）：integration 测试（testcontainers）、
  # imgsvc selfcheck、deploy 脚本检查。
  ci)
    do_sync
    echo
    echo "==== [1/4] gofmt + go vet ===="
    run_gofmt_vet || ci_fail "gofmt / go vet"
    echo
    echo "==== [2/4] golangci-lint（${GOLANGCI_IMAGE}，跟 CI 同版同参数）===="
    run_golangci || ci_fail "golangci-lint"
    echo
    echo "==== [3/4] 后端单元测试 ===="
    run_unit_tests_strict || ci_fail "后端单元测试"
    echo
    echo "==== [4/4] 前端 lint / 类型检查 / 关键 vitest ===="
    run_web_ci || ci_fail "前端 lint / 类型检查 / 关键 vitest"
    echo
    echo "✓ CI 预演全部通过。"
    echo "  （未含 GitHub CI 上才跑的：integration 测试、imgsvc selfcheck、deploy 脚本检查）"
    ;;

  config)
    do_sync
    echo "→ 渲染最终 compose（检查叠加是合并还是覆盖）"
    nas_sudo "cd $REMOTE_DIR && $NAS_ENV $DOCKER compose $COMPOSE_FILES config"
    ;;

  up)
    do_sync
    echo "→ 起开发环境"
    nas_sudo "cd $REMOTE_DIR && $NAS_ENV $DOCKER compose $COMPOSE_FILES up -d"
    echo
    echo "入口  http://$NAS_HOST:18080   （前端和后端都在这个端口）"
    echo "日志  designkit/bin/dk-nas.sh logs"
    echo "热更新开发服务器 13000 只绑 NAS 回环，要看它先开隧道："
    echo "  ssh -L 13000:127.0.0.1:13000 dk-nas    然后浏览器开 http://localhost:13000"
    ;;

  # 改了**代码**之后用这个（up 不行）。
  #
  # ⚠ `up -d` 不带 --build：compose 配置没变时 Docker 判定「容器已是最新」，
  # 直接跳过，镜像还是旧的。而 18080 那个端口跑的是**打包进镜像**的前端，
  # 所以改完 .vue / .ts 只跑 up，18080 上什么都不会变（13000 那个开发服务器
  # 是挂载源码的，会热更新——两个端口表现不一致，最容易在这里被骗）。
  rebuild)
    do_sync
    echo "→ 检查基础镜像是否都在本地（缺了 BuildKit 会联网解析然后超时）"
    missing=$(nas_sudo "for i in $BASE_IMAGES; do $DOCKER image inspect \$i >/dev/null 2>&1 || echo \$i; done")
    if [ -n "$missing" ]; then
      echo "✗ 本地缺这些基础镜像，先在 NAS 上 pull 下来再重来："
      for image in $missing; do echo "    sudo docker pull $image"; done
      echo "  （用 docker pull 而不是让 BuildKit 自己拉：这台 NAS 上 BuildKit"
      echo "    解析元数据会走 IPv6 连 auth.docker.io 超时，pull 走的是另一条路，没问题）"
      exit 1
    fi
    echo "→ 重新构建镜像并替换容器（前端会重新打包进去，要几分钟）"
    # ⚠ 用 BUILD_ENV 不是 NAS_ENV：构建必须开 BuildKit，理由见上面的长注释。
    nas_sudo "cd $REMOTE_DIR && $BUILD_ENV $DOCKER compose $COMPOSE_FILES up -d --build sub2api"
    echo
    echo "完成。改动生效在 http://$NAS_HOST:18080"
    ;;

  # 改了**数据库里的配置**（商品图设置那 9 项）之后用这个。
  #
  # 那几项是后端启动时读一次的，改完必须让进程重起。不用重新构建镜像，
  # 但也**不能**用 up —— 同样会被判成 up-to-date 而什么都不做。
  restart)
    echo "→ 重启后端（让「商品图设置」里改的并发数 / 超时 / 最长边生效）"
    nas_sudo "cd $REMOTE_DIR && $NAS_ENV $DOCKER compose $COMPOSE_FILES restart sub2api"
    echo
    echo "完成。等约 20 秒后端起完，再刷新页面。"
    ;;

  down)
    nas_sudo "cd $REMOTE_DIR && $NAS_ENV $DOCKER compose $COMPOSE_FILES down"
    ;;

  logs)
    nas_sudo "cd $REMOTE_DIR && $NAS_ENV $DOCKER compose $COMPOSE_FILES logs -f --tail=100 ${2:-}"
    ;;

  sh)
    nas_plain -t "cd $REMOTE_DIR && exec sh"
    ;;

  *)
    sed -n '3,31p' "$0"
    exit 1
    ;;
esac
