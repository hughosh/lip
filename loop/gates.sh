#!/bin/bash
# gates.sh -- the loop's oracle. The single command that decides whether the
# tree is green, and the only authority the loop has on that question.
#
# It is deliberately NOT an LLM. Two agents reviewing each other can agree on
# something false; `go test -race` cannot be talked round. Every gate below is
# either a compiler, a test, or a checksum.
#
# Usage:  loop/gates.sh [--quick]
#   --quick  skip the mutation negative control (minutes), for inner iterations.
#            A unit may NOT advance on a --quick pass.
#
# Exit 0 = green. Any non-zero = the tree is not in a state anything may build
# on, and the only admissible next work is making it green again.
#
# Output is a machine-readable verdict block on stdout, so the conductor never
# has to parse prose or ask a model what happened.

set -uo pipefail
cd "$(dirname "$0")/.." || exit 99

PY=/Users/hugh/kek/.venv/bin/python
QUICK=0
[ "${1:-}" = "--quick" ] && QUICK=1

fail=0
declare -a results

run() {                       # run <name> <cmd...>
    local name="$1"; shift
    local out rc
    out="$("$@" 2>&1)"; rc=$?
    if [ $rc -eq 0 ]; then
        results+=("PASS  $name")
    else
        results+=("FAIL  $name (rc=$rc)")
        # Keep only the tail: a full go test dump is noise the conductor pays
        # for in tokens on every single iteration.
        printf '%s\n' "----- $name -----" >&2
        printf '%s\n' "$out" | tail -40 >&2
        fail=1
    fi
}

# 1. It compiles. Nothing else means anything if this fails.
cd go || exit 99
run "build"      env CGO_ENABLED=0 go build ./...
run "vet"        go vet ./harness/... ./cmd/harness/...
run "gofmt"      bash -c '[ -z "$(gofmt -l harness cmd/harness)" ]'

# 2. The tests, with the race detector. -count=1 defeats the test cache: a
#    cached PASS is a claim about a tree that no longer exists.
run "test-race"  env CGO_ENABLED=0 go test -race -count=1 ./harness/... ./cmd/harness/...

cd .. || exit 99

# 3. §9 + H-TOP-2. Refuses stub panics, t.Skip, testing.Short, discarded
#    errors, missing confidence trailers, and any edit to a frozen artifact or
#    a read-only Go tree. This is what stops an agent faking progress.
run "check.py"   "$PY" scripts/check.py

# 4. The gate on the gate (§17 V5). Slow -- it rebuilds a mutated copy of the
#    tree per mutation -- so it is skippable for inner iterations but NOT for
#    advancing a unit. A mutation that stops being caught is a silent loss of
#    verification, and it is the only check here that can detect one.
if [ $QUICK -eq 0 ]; then
    run "negative-control" "$PY" scripts/harness_negative_control.py
else
    results+=("SKIP  negative-control (--quick; unit may not advance)")
fi

echo "===GATES==="
printf '%s\n' "${results[@]}"
if [ $fail -eq 0 ]; then
    echo "VERDICT: GREEN"
    [ $QUICK -eq 1 ] && echo "ADVANCE_ELIGIBLE: NO (quick run)" || echo "ADVANCE_ELIGIBLE: YES"
    exit 0
fi
echo "VERDICT: RED"
echo "ADVANCE_ELIGIBLE: NO"
exit 1
