#!/usr/bin/env bash
#
# bench.sh — measure ansible-galaxy vs go-galaxy across requirements-{1,10,100}.yml.
#
# Scenarios per file:
#   1. cold cache  — both caches and the install dir are wiped before each run
#   2. warm cache  — caches primed once, only install dir wiped between runs
#   3. frozen      — go-galaxy only: warm + lockfile + --frozen --offline
#
# Output:
#   dist/bench/cold-{N}.md
#   dist/bench/warm-{N}.md
#   dist/bench/frozen-{N}.md
#   dist/bench/summary.md  (concatenation, suitable for README inclusion)
#
# Env knobs:
#   RUNS=5         number of measured runs per command (hyperfine --runs)
#   WARMUP=1       hyperfine warmup runs for warm/frozen scenarios
#   SIZES="1 10 100"  which requirements files to bench (space-separated)
#   SCENARIOS="cold warm frozen"  which scenarios to run

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

GG="$ROOT/dist/go-galaxy"
AG="$ROOT/.venv/bin/ansible-galaxy"
TARGET="${TMPDIR:-/tmp}/go-galaxy-bench-target"
GG_CACHE="${HOME}/.cache/go-galaxy"
AG_CACHE="${HOME}/.ansible/galaxy_cache"

RUNS="${RUNS:-5}"
WARMUP="${WARMUP:-1}"
SIZES="${SIZES:-1 10 100}"
SCENARIOS="${SCENARIOS:-cold warm frozen}"

OUT="$ROOT/dist/bench"
mkdir -p "$OUT"

require() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing: $1" >&2; exit 1; }
}
require hyperfine
[ -x "$GG" ] || { echo "missing: $GG (run: go build -o ./dist/go-galaxy ./cmd/go-galaxy)" >&2; exit 1; }
[ -x "$AG" ] || { echo "missing: $AG (run: python3 -m venv .venv && .venv/bin/pip install ansible-core)" >&2; exit 1; }

run_cold() {
  local n="$1" req="$2"
  echo "=== cold cache: requirements-${n}.yml ==="
  hyperfine --runs "$RUNS" \
    --prepare "rm -rf '$TARGET' '$GG_CACHE' '$AG_CACHE'" \
    --export-markdown "$OUT/cold-${n}.md" \
    -n "ansible-galaxy" "ANSIBLE_COLLECTIONS_PATH=$TARGET $AG collection install --no-deps -r $req -p $TARGET" \
    -n "go-galaxy"      "$GG install --no-deps -r $req -p $TARGET"
}

run_warm() {
  local n="$1" req="$2"
  echo "=== warm cache: requirements-${n}.yml ==="
  rm -rf "$TARGET" "$GG_CACHE" "$AG_CACHE"
  ANSIBLE_COLLECTIONS_PATH="$TARGET" "$AG" collection install --no-deps -r "$req" -p "$TARGET" >/dev/null 2>&1 || true
  rm -rf "$TARGET"
  "$GG" install --no-deps -r "$req" -p "$TARGET" >/dev/null 2>&1 || true
  rm -rf "$TARGET"
  hyperfine --runs "$RUNS" --warmup "$WARMUP" \
    --prepare "rm -rf '$TARGET'" \
    --export-markdown "$OUT/warm-${n}.md" \
    -n "ansible-galaxy" "ANSIBLE_COLLECTIONS_PATH=$TARGET $AG collection install --no-deps -r $req -p $TARGET" \
    -n "go-galaxy"      "$GG install --no-deps -r $req -p $TARGET"
}

run_frozen() {
  local n="$1" req="$2"
  local lock_dir="$OUT/lock-${n}"
  mkdir -p "$lock_dir"
  cp "$req" "$lock_dir/requirements.yml"
  echo "=== frozen+offline: requirements-${n}.yml ==="
  "$GG" lock --no-deps -r "$lock_dir/requirements.yml" >/dev/null 2>&1
  "$GG" warm --no-deps -r "$lock_dir/requirements.yml" --frozen >/dev/null 2>&1
  hyperfine --runs "$RUNS" --warmup "$WARMUP" \
    --prepare "rm -rf '$TARGET'" \
    --export-markdown "$OUT/frozen-${n}.md" \
    -n "go-galaxy --frozen --offline" \
    "$GG install --no-deps -r $lock_dir/requirements.yml -p $TARGET --frozen --offline"
}

scenario_in_list() {
  case " $SCENARIOS " in
    *" $1 "*) return 0 ;;
    *) return 1 ;;
  esac
}

main() {
  for n in $SIZES; do
    REQ="$ROOT/testing/requirements-${n}.yml"
    [ -f "$REQ" ] || { echo "missing: $REQ" >&2; exit 1; }
    scenario_in_list cold   && run_cold   "$n" "$REQ"
    scenario_in_list warm   && run_warm   "$n" "$REQ"
    scenario_in_list frozen && run_frozen "$n" "$REQ"
  done

  rm -rf "$TARGET"

  {
    echo "# Benchmark summary"
    echo
    echo "Generated: $(date -u +%Y-%m-%dT%H:%M:%SZ) | runs=$RUNS warmup=$WARMUP sizes=\"$SIZES\""
    echo "Host: $(uname -srm)"
    echo "ansible-galaxy: $("$AG" --version 2>/dev/null | head -1)"
    echo "go-galaxy: $("$GG" --version 2>/dev/null | head -1)"
    echo
    for n in $SIZES; do
      echo "## requirements-${n}.yml"
      echo
      for kind in cold warm frozen; do
        f="$OUT/${kind}-${n}.md"
        if [ -f "$f" ]; then
          echo "### ${kind} cache"
          echo
          cat "$f"
          echo
        fi
      done
    done
  } > "$OUT/summary.md"

  echo
  echo "Done. Summary: $OUT/summary.md"
}

main "$@"
