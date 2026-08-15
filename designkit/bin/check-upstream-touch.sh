#!/usr/bin/env bash
# 清点：相对分出来的基线，我们改动过哪些上游文件。
#
# ⚠ 2026-08-13 起这个脚本**只清点、不拦截**，退出码恒为 0。
#
# 为什么改：原来它是硬性守卫，前提是「这个 fork 要一直跟随上游」。
#   monica 明确了实际情况——这是她自己的项目，只是起点是 Sub2API，
#   以后按自己的节奏走版本；需要上游某个更新时再单独读进来，不做整体合并。
#   前提没了，拦截就没有意义，反而会逼人为了绕开它写更绕的代码。
#
# 那它现在有什么用：
#   等她说「把上游那个新功能加进来」时，下面这份清单正好指出
#   哪几个文件会撞上、要重点看。不用现翻 git log。
#
# 用法：
#   designkit/bin/check-upstream-touch.sh            # 对比 .designkit-baseline 里的 tag
#   designkit/bin/check-upstream-touch.sh v0.1.180   # 对比指定 tag
#
# 退出码：0 = 正常（无论改了多少上游文件）；2 = 环境问题（基线取不到等）

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

BASELINE="${1:-}"
if [ -z "${BASELINE}" ]; then
  [ -f .designkit-baseline ] || { echo "找不到 .designkit-baseline"; exit 2; }
  BASELINE="$(grep -vE '^\s*#|^\s*$' .designkit-baseline | head -1 | tr -d '[:space:]')"
fi
[ -n "${BASELINE}" ] || { echo "基线为空"; exit 2; }

if ! git rev-parse -q --verify "${BASELINE}^{commit}" >/dev/null; then
  echo "取不到基线 ${BASELINE} —— 先跑：git fetch upstream --tags"
  exit 2
fi

# 「我们自己的」路径。命中这些的文件不出现在清单里——它们本来就是我们写的。
#
# 注意 migrations 的正则：必须正好 4 位数字、且必须紧跟 designkit_，
# 否则会被当成上游文件列进清单（9001_dk_x.sql、901_designkit_x.sql 都不匹配）。
#
# 刻意不含 backend/ent/schema/designkit_*.go：我们的表不建 ent schema
#   （新增一个再跑 go generate 会重写 backend/ent/ 下 200+ 个生成文件，
#    一次改动几百个文件没法 review）。上游自己新增的表也是这么干的。
#   我们的表 = 迁移 SQL + 手写 repository。
OURS='^(backend/internal/designkit/|backend/migrations/9[0-9]{3}_designkit_|frontend/src/features/designkit/|frontend/public/designkit/|frontend/src/i18n/locales/[a-z-]+/designkit\.ts$|frontend/src/views/admin/__tests__/RedeemView\.batchDelete\.spec\.ts$|deploy/docker-compose\.designkit-|designkit/|\.github/workflows/designkit-|NOTICE\.md$|COPYING$|CLAUDE\.md$|\.designkit-baseline$|data/\.gitignore$)'

# ⚠ 2026-08-14 补了两条：
#   frontend/public/designkit/        我们放的静态资源（登录页那 10 个平台标识 SVG）
#   .../RedeemView.batchDelete.spec.ts 我们**新建**的测试文件
# 不补的话它们会被算成「改动过的上游文件」——而它们是我们从零建的，
# 跟「吸收上游更新时要重点看」毫无关系。清点数虚高会让这份清单失去意义
# （NOTICE.md 要照它如实披露）。
#
# 判据是「这个路径下的东西是不是我们从零建的」，不是「文件在不在上游仓库里」。

# 「常规接入点」：把新功能挂进上游必然要碰的几处，单独分一组只是为了让清单更好读。
# 改这几个不需要理由；改别的也不需要理由，只是会单独列出来提醒「吸收上游更新时看这里」。
HOTSPOTS='^(backend/internal/server/router\.go|frontend/src/router/index\.ts|frontend/src/components/layout/AppSidebar\.vue|frontend/src/i18n/locales/(zh|en)/index\.ts|\.gitignore)$'

# 跟基线的共同祖先比。用 merge-base 而不是 BASELINE 本身，
# 这样跟版合并之后仍然只看「我们改了什么」。
#
# ⚠ 2026-08-15 起仓库历史已重置为单提交（GitHub 独立化），跟上游 tag
# 没有共同祖先，merge-base 会失败——这时直接跟基线 tag 的树比，语义一样：
# 「相对我们分出来的那个版本，改了什么」。git diff 比较两棵树不需要共同祖先。
BASE_COMMIT="$(git merge-base "${BASELINE}" HEAD 2>/dev/null || true)"
[ -n "${BASE_COMMIT}" ] || BASE_COMMIT="${BASELINE}"

# 三样都要看，少一样这个检查就是空转：
#   1. 已提交的改动          diff <merge-base>
#   2. 暂存区和工作区的改动    同上（不带 ...HEAD 就会带上工作区）
#   3. 还没 git add 的新文件   ls-files --others
CHANGED="$(
  {
    git diff --name-only "$BASE_COMMIT"
    git ls-files --others --exclude-standard
  } | sort -u
)"

if [ -z "$CHANGED" ]; then
  echo "相对 ${BASELINE} 没有任何改动。"
  exit 0
fi

violations=""
hotspots=""
while IFS= read -r f; do
  [ -n "$f" ] || continue
  if printf '%s' "$f" | grep -qE "$OURS"; then
    continue
  elif printf '%s' "$f" | grep -qE "$HOTSPOTS"; then
    hotspots="${hotspots}${f}"$'\n'
  else
    violations="${violations}${f}"$'\n'
  fi
done <<< "$CHANGED"

if [ -n "$hotspots" ]; then
  echo "改动过的上游注册点："
  printf '%s' "$hotspots" | sed 's/^/    /'
  echo "  → 这几个是「把新功能挂进去」的常规接入点。"
  echo
fi

if [ -n "$violations" ]; then
  echo "改动过的其它上游文件（不是错误，只是清点）："
  printf '%s' "$violations" | sed 's/^/    /'
  echo
  echo "  以后要吸收上游某次更新时，重点看上面这几个。"
  echo
fi

total="$(printf '%s%s' "$hotspots" "$violations" | grep -c . || true)"
if [ "${total:-0}" -eq 0 ]; then
  echo "✓ 相对 ${BASELINE} 没有改动任何上游文件。"
else
  echo "共改动 ${total} 个上游文件（相对 ${BASELINE}）。"
  echo
  echo "新代码优先放自建目录——不是为了避免冲突，是为了「哪些是我们写的」一眼可见："
  echo "    后端    backend/internal/designkit/{handler,service,repository}/"
  echo "    数据表  backend/migrations/9xxx_designkit_*.sql（只写迁移 SQL，不建 ent schema）"
  echo "    前端    frontend/src/features/designkit/"
  echo "    文案    frontend/src/i18n/locales/<语言>/designkit.ts"
  echo "    脚本文档 designkit/{bin,docs}/"
  echo
  echo "⚠ 建目录避开 scripts / tests 这两个名字：上游 .gitignore 里那两条规则不带斜杠，"
  echo "  会在任意层级命中同名目录并悄悄吞掉文件。"
fi
exit 0
