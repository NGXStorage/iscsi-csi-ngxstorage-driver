#!/usr/bin/env bash
# Offline proof for the disruptive/stress gate : the real executor
# (tests/scenario/run.sh) must refuse reboot/stress scenarios without
# --include-disruptive/--include-stress (or the matching env), and `all` must
# skip them. Refusals happen before any scenario body runs, so no side effect
# (run-test.sh invocation, manifest apply) is recorded. No cluster is touched.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cache="$HOME/.cache/ngxstorage"
mkdir -p "$cache"
scratch="$(mktemp -d "$cache/csi-disruptive-gate.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT

fixture="$scratch/scenario"
mkdir -p "$fixture"
cp "$root/run.sh" "$fixture/run.sh"
cat > "$fixture/lib.sh" <<'EOF'
backend_snapshots() { :; }
collect_logs() { :; }
collect_render_scratch() { :; }
EOF
for name in volume-lifecycle-rwo volume-recovery-host-reboot benchmark-stress-parameters; do
    mkdir -p "$fixture/$name"
    cat > "$fixture/$name/run-test.sh" <<EOF
echo "ran:$name" >> "\$EVENTS"
EOF
done

export EVENTS="$scratch/events" BENCH=ocp2 KUBE_BIN=bash ALLOW_LIVE=1 NAMESPACE=gate-test-ns
export NGX_CONTROLLER=fixture STORAGE_API_KEY=fixture NGX_POOL_NAME=fixture
export NGX_SDK_CLI="$(type -P true)"

run() {
    # Runs the fixture executor and returns its rc without tripping set -e.
    local rc=0
    "$@" > "$scratch/output" 2>&1 || rc=$?
    return "$rc"
}

# 1. Named reboot scenario is refused with rc=2 and zero side effects.
: > "$EVENTS"
rc=0
run bash "$fixture/run.sh" volume-recovery-host-reboot || rc=$?
[ "$rc" = 2 ]
grep -Fq -- '--include-disruptive' "$scratch/output"
[ ! -s "$EVENTS" ] || { echo "side effect leaked past the gate" >&2; exit 1; }

# 2. The explicit flag allows it.
: > "$EVENTS"
run bash "$fixture/run.sh" volume-recovery-host-reboot --include-disruptive
grep -qF "ran:volume-recovery-host-reboot" "$EVENTS"

# 3. The env form allowed by tests/run-all-tests.sh agrees.
: > "$EVENTS"
INCLUDE_DISRUPTIVE=1 run bash "$fixture/run.sh" volume-recovery-host-reboot
grep -qF "ran:volume-recovery-host-reboot" "$EVENTS"

# 4. Named stress scenario is refused without --include-stress.
: > "$EVENTS"
rc=0
run bash "$fixture/run.sh" benchmark-stress-parameters || rc=$?
[ "$rc" = 2 ]
grep -Fq -- '--include-stress' "$scratch/output"
[ ! -s "$EVENTS" ] || { echo "side effect leaked past the stress gate" >&2; exit 1; }

# 5. --include-stress allows it.
: > "$EVENTS"
run bash "$fixture/run.sh" benchmark-stress-parameters --include-stress
grep -qF "ran:benchmark-stress-parameters" "$EVENTS"

# 6. `all` skips both gated scenarios unless explicitly included.
: > "$EVENTS"
run bash "$fixture/run.sh" all
grep -qF "ran:volume-lifecycle-rwo" "$EVENTS"
! grep -q "ran:volume-recovery-host-reboot" "$EVENTS"
! grep -q "ran:benchmark-stress-parameters" "$EVENTS"
grep -qF '[skip] volume-recovery-host-reboot (use --include-disruptive)' "$scratch/output"
grep -qF '[skip] benchmark-stress-parameters (use --include-stress)' "$scratch/output"

# 7. `all` with both flags runs everything.
: > "$EVENTS"
run bash "$fixture/run.sh" all --include-disruptive --include-stress
for name in volume-lifecycle-rwo volume-recovery-host-reboot benchmark-stress-parameters; do
    grep -qF "ran:$name" "$EVENTS"
done

# 8. A plain scenario never trips the gate.
: > "$EVENTS"
run bash "$fixture/run.sh" volume-lifecycle-rwo
grep -qF "ran:volume-lifecycle-rwo" "$EVENTS"

printf '%s\n' 'Disruptive/stress gate in the real executor (refuse/skip/allow, no side effects) PASS'
