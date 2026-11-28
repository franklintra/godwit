#!/usr/bin/env bash
# perfbench on the VPS, from the build copy vps-test made of a worktree.
#
#   [PERF_CGROUP=1] vps-test <worktree> 'scripts/perfbench/run.sh [--web] [perfbench run flags]'
#   vps-test <worktree> 'scripts/perfbench/run.sh compare <a.json> <b.json>'
#   vps-test <worktree> 'scripts/perfbench/run.sh show <a.json>'
#   vps-test <worktree> 'scripts/perfbench/run.sh list'
#
# Builds godwit and perfbench for Linux into $PERFBENCH_ROOT/bin/<worktree>
# (default ~/godwit-bench), then runs. --web also builds web/dist (vite) and
# measures the web pages. The fixture is made on first use (several minutes).
set -euo pipefail
name=$(basename "$PWD")
root=${PERFBENCH_ROOT:-$HOME/godwit-bench}
bin=$root/bin/$name
mkdir -p "$bin" "$root/results"
web=0
args=()
for a in "$@"; do
  case $a in
  --web) web=1 ;;
  *) args+=("$a") ;;
  esac
done
build_perfbench() { CGO_ENABLED=0 go build -o "$bin/perfbench" ./scripts/perfbench; }
case ${args[0]:-run} in
compare | show)
  go tool mygo generate >/dev/null
  build_perfbench
  exec "$bin/perfbench" "${args[@]}"
  ;;
list)
  ls -lt "$root/results" | head -30
  exit 0
  ;;
esac
# rsync drops generated files the Mac never has: generate them again
if [ $web = 1 ]; then
  (cd web && bun install --frozen-lockfile >/dev/null)
  go tool mygo generate >/dev/null
  (cd web && bunx vite build >/dev/null 2>&1)
  args+=(-web-dist "$PWD/web/dist")
else
  go tool mygo generate >/dev/null # the i18n catalog Go needs (rsync drops it)
fi
CGO_ENABLED=0 go build -o "$bin/godwit" ./cmd/godwit
build_perfbench
exec "$bin/perfbench" run -godwit "$bin/godwit" -label "$name" "${args[@]}"
