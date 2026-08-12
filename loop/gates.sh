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
ROOT="$PWD"

PY=/Users/hugh/kek/.venv/bin/python
QUICK=0
[ "${1:-}" = "--quick" ] && QUICK=1

# lip-8k3. run() keeps the TAIL of a failed step, which is only the right half
# to keep if the stream is in chronological order. Python block-buffers stdout
# when it is a pipe but never buffers stderr, so an unbuffered script's
# diagnostic is emitted immediately while the stdout that preceded it sits in a
# buffer until exit: the merged stream then BEGINS with the explanation and ENDS
# with the progress chatter, and tail -40 keeps exactly the wrong end. That cost
# a full ~5h round to diagnose on 2026-08-11 -- the round reported FAIL
# negative-control with a 40-line tail ending at "preflight: 273 selected
# mutation(s) compile" and no reason, and the reason had to be recovered by
# re-running the gate rather than by reading its report.
#
# Fixing the ORDERING is preferred over keeping head as well as tail: an
# interleaved head+tail of a mis-ordered stream is still misleading, it just
# misleads with more text. With ordering restored the diagnostic is last,
# which is what tail already keeps.
export PYTHONUNBUFFERED=1

# Every step's FULL output is also written here, so a truncated tail is never
# the only copy. Gitignored; the path is printed with the failure.
GATES_OUT="$ROOT/loop/gates-out"
rm -rf "$GATES_OUT" && mkdir -p "$GATES_OUT" || exit 99

fail=0
declare -a results

# lip-8k3 RELATED. A round that runs out of disk looks EXACTLY like a rotted
# mutation catalogue -- DID-NOT-BUILD cascades and empty failure lists -- and
# the Go build cache has twice grown past 190GB and killed consecutive rounds.
# The negative control rebuilds a mutated copy of the tree per mutation, so the
# free space at the END is the number that matters, not the one at the start;
# both are reported. This does NOT gate: a full volume is not a defect in the
# tree, and reporting it as VERDICT: RED would send the loop hunting a phantom
# code bug. It is reported so the reader can tell the two apart.
free_gb() { df -k "$ROOT" | awk 'NR==2 {printf "%d", $4/1048576}'; }
DISK_START="$(free_gb)"

run() {                       # run <name> <cmd...>
    local name="$1"; shift
    local out rc log
    log="$GATES_OUT/$name.log"
    out="$("$@" 2>&1)"; rc=$?
    printf '%s\n' "$out" > "$log"
    if [ $rc -eq 0 ]; then
        results+=("PASS  $name")
    else
        results+=("FAIL  $name (rc=$rc)")
        # Keep only the tail: a full go test dump is noise the conductor pays
        # for in tokens on every single iteration. The full copy is on disk at
        # the path below when 40 lines is not enough.
        printf '%s\n' "----- $name -----" >&2
        printf '%s\n' "$out" | tail -40 >&2
        printf '%s\n' "----- full output: $log -----" >&2
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

# 3b. The gate on the CATALOGUE (lip-xl7). A mutation's replacement text is a
#     second copy of whatever production signature it names, and nothing else
#     reads it: the cheap anchor audit validates `old`, which goes on matching
#     while `new` goes stale. The result is a DID-NOT-BUILD, which does not
#     count as caught, discovered at minute 95 of a ~105-minute round. It cost
#     two rounds before this existed.
run "catalogue-tests" "$PY" -m unittest scripts.test_harness_negative_control

# 4. The gate on the gate (§17 V5). Slow -- it rebuilds a mutated copy of the
#    tree per mutation -- so it is skippable for inner iterations but NOT for
#    advancing a unit. A mutation that stops being caught is a silent loss of
#    verification, and it is the only check here that can detect one.
if [ $QUICK -eq 0 ]; then
    # Normal mode runs its OWN preflight before the first test, so the whole
    # catalogue is known to compile before ~105 minutes are committed to it.
    run "negative-control" "$PY" scripts/harness_negative_control.py
else
    # A quick pass still compiles every mutation. That is the cheap half of the
    # negative control -- it cannot say a mutation is still CAUGHT, but it can
    # say the catalogue is still executable, which is the failure that wastes a
    # full round.
    run "mutation-preflight" "$PY" scripts/harness_negative_control.py --build-only
    results+=("SKIP  negative-control (--quick; unit may not advance)")
fi

echo "===GATES==="
printf '%s\n' "${results[@]}"
echo "DISK_FREE_GB: ${DISK_START} at start, $(free_gb) at end (a round needs ~15)"
echo "LOAD: $(uptime | sed 's/.*load averages*: //')"
echo "STEP_LOGS: $GATES_OUT"
if [ $fail -eq 0 ]; then
    echo "VERDICT: GREEN"
    [ $QUICK -eq 1 ] && echo "ADVANCE_ELIGIBLE: NO (quick run)" || echo "ADVANCE_ELIGIBLE: YES"
    exit 0
fi
echo "VERDICT: RED"
echo "ADVANCE_ELIGIBLE: NO"
exit 1
