#!/usr/bin/env bash
# perfbench on the VPS, from the build copy vps-test made of a worktree.
#
#   [PERF_CGROUP=1] vps-test <worktree> 'scripts/perfbench/run.sh [--web] [perfbench run flags]'
#   vps-test <worktree> 'scripts/perfbench/run.sh compare <a.json> <b.json>'
#   vps-test <worktree> 'scripts/perfbench/run.sh show <a.json>'
#   vps-test <worktree> 'scripts/perfbench/run.sh list'
#   vps-test <worktree> 'scripts/perfbench/run.sh -db <file> [perfbench run flags]'
#
# -db measures a copy of a real database instead of the fixture. The run
# goes in a container with no network but its own loopback (docker
# --network none), with the source mounted read-only and one scratch
# directory, scratch-<label>, beside the source; the scratch directory is
# deleted when the run ends. See README.md, "A real database".
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
db="" label=$name
for ((i = 0; i < ${#args[@]}; i++)); do
  case ${args[i]} in
  -db | --db) db=${args[i + 1]:-} ;;
  -db=* | --db=*) db=${args[i]#*=} ;;
  -label | --label) label=${args[i + 1]:-} ;;
  -label=* | --label=*) label=${args[i]#*=} ;;
  esac
done
if [ -z "$db" ]; then
  exec "$bin/perfbench" run -godwit "$bin/godwit" -label "$name" "${args[@]}"
fi
src=$(realpath -e "$db")
slug=$(printf %s "$label" | tr -c 'A-Za-z0-9_.-' _)
scratch=$(dirname "$src")/scratch-$slug
cname=perfbench-realdb-$slug
if [ -e "$scratch" ]; then
  echo "run.sh: $scratch exists: another run uses it, or a run died; delete it first" >&2
  exit 1
fi
cleanup() {
  docker rm -f "$cname" >/dev/null 2>&1 || true
  rm -rf "$scratch"
}
trap cleanup EXIT
trap 'exit 130' INT TERM
mkdir -m 700 "$scratch"
# mygo's native SQLite: the build fetched it into the cache, and the
# loader finds it from the plugin's module source; the container cannot
# download it
mkdir -p "$scratch/home/.cache"
cp -a "${XDG_CACHE_HOME:-$HOME/.cache}/mygo" "$scratch/home/.cache/"
egoist=$(go env GOMODCACHE)/github.com/egoist
# the host's /usr (merged /usr: libraries included) over the image's, so
# the App's text layout finds the same libraries as in the fixture mode
docker run --rm --name "$cname" --pull never --network none --user "$(id -u):$(id -g)" \
  --memory 3g --memory-swap 3g --cpus 3 --security-opt no-new-privileges \
  -e HOME=/scratch/home -e GOMAXPROCS=3 -e PERFBENCH_ROOT=/bench \
  -v "$bin:/pb:ro" -v "$src:/source/godwit.db:ro" -v "$scratch:/scratch" -v "$root/results:/bench/results" \
  -v "$egoist:$egoist:ro" \
  -v /usr:/usr:ro -v /etc/fonts:/etc/fonts:ro \
  --entrypoint /pb/perfbench "${PERFBENCH_IMAGE:-eclipse-temurin:21-jre}" \
  run -godwit /pb/godwit -label "$name" "${args[@]}" -db /source/godwit.db -scratch /scratch -native-cache /scratch/home/.cache/mygo
echo "results: $root/results (the container calls it /bench/results)"
