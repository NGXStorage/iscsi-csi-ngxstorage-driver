#!/usr/bin/env bash
# Offline proof for render run identity : the render dir is per-run,
# identified by RENDER_ID and shared by every pre/run/post/cleanup process of
# that run. A second run of the same TEST_NAME gets a DIFFERENT directory and
# never inherits the first run's stale manifests (a YAML removed from the
# source must not survive into the second render). Cleanup deletes only its own
# run's manifests and collects only its own scratch, at most once; --keep
# retains it. Parallel runs with distinct identities never collide. The scratch
# lives under $TMPDIR (not a RAM tmpfs) and is reclaimed. No cluster is touched.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cache="$HOME/.cache/ngxstorage"
mkdir -p "$cache"
scratch="$(mktemp -d "$cache/csi-render-identity.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
source "$root/lib.sh"

# The scratch must live under $TMPDIR, never a RAM tmpfs (/tmp, /var/tmp).
case "$TMPDIR" in /tmp|/tmp/*|/var/tmp|/var/tmp/*) echo "TMPDIR is a RAM tmpfs" >&2; exit 1 ;; esac
export TMPDIR="$scratch/tmp" KUBE_BIN=oc
mkdir -p "$TMPDIR"

# Recording oc stub: every cleanup delete / apply is recorded to $EVENTS.
events="$scratch/events"
export EVENTS="$events"
bin_dir="$scratch/bin"
mkdir -p "$bin_dir"
export PATH="$bin_dir:$PATH"
cat > "$bin_dir/oc" <<EOF
#!/bin/bash
printf 'oc %s\n' "\$*" >> "\${EVENTS:?}"
exit 0
EOF
chmod 755 "$bin_dir/oc"

# Source fixtures: two manifests. stale-pod.yaml is moved out of the source
# tree between run 1 and run 2 to model a source change (a YAML dropped from
# the fixtures). current-pod.yaml stays.
test_dir="$scratch/source"
mkdir -p "$test_dir"
cat > "$test_dir/current-pod.yaml" <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: current-pod
  namespace: default
spec:
  containers: [{name: c, image: nginx}]
EOF
cat > "$test_dir/stale-pod.yaml" <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: stale-pod
  namespace: default
spec:
  containers: [{name: c, image: nginx}]
EOF
export TEST_NAME=same TEST_DIR="$test_dir" KEEP=0 MARKER="synthetic"

# Mint a fresh render identity exactly as run.sh run_one does.
render_identity() {
    RENDER_DIR="$(mktemp -d "$TMPDIR/csi-scenario-render.XXXXXX")"
    RENDER_ID="$(basename "$RENDER_DIR")"
    export RENDER_DIR RENDER_ID
}

# ---------------------------------------------------------------------------
# RUN 1
# ---------------------------------------------------------------------------
render_identity
run1_dir="$RENDER_DIR"; run1_id="$RENDER_ID"
OUT1="$scratch/out1"; mkdir -p "$OUT1"; export OUT="$OUT1" NS=ns-one
: > "$events"
render_test_dir
# Run 1 captured both manifests at render time.
[ -f "$run1_dir/current-pod.yaml" ]
[ -f "$run1_dir/stale-pod.yaml" ]
# The scratch lives under TMPDIR and carries a run identity.
[ -n "$run1_id" ]
case "$run1_dir" in "$TMPDIR"/*) ;; *) echo "scratch not under TMPDIR: $run1_dir" >&2; exit 1 ;; esac
# The namespace pin rendered to this run's selected NS.
grep -q 'namespace: ns-one' "$run1_dir/current-pod.yaml"

# ---------------------------------------------------------------------------
# Model a source change: stale-pod.yaml leaves the source tree (moved, not rm).
# ---------------------------------------------------------------------------
mkdir -p "$test_dir/removed"
mv "$test_dir/stale-pod.yaml" "$test_dir/removed/"

# ---------------------------------------------------------------------------
# RUN 2 — same TEST_NAME, a DIFFERENT render identity.
# ---------------------------------------------------------------------------
render_identity
run2_dir="$RENDER_DIR"; run2_id="$RENDER_ID"
[ "$run2_dir" != "$run1_dir" ] || { echo "two runs of the same TEST_NAME shared a render dir" >&2; exit 1; }
[ "$run2_id" != "$run1_id" ]
OUT2="$scratch/out2"; mkdir -p "$OUT2"; export OUT="$OUT2" NS=ns-two
: > "$events"
render_test_dir
# The YAML removed from source must NOT survive into the second render, and the
# surviving one must be present. Render 1 (taken before the removal) still holds
# the stale file — proving the dirs are genuinely independent.
[ -f "$run2_dir/current-pod.yaml" ]
[ ! -f "$run2_dir/stale-pod.yaml" ]
[ -f "$run1_dir/stale-pod.yaml" ]
grep -q 'namespace: ns-two' "$run2_dir/current-pod.yaml"

# Idempotence: re-rendering within the same run keeps the same dir and any
# substitutions, and does not re-copy a second time.
render_test_dir
[ "$RENDER_DIR" = "$run2_dir" ]

# ---------------------------------------------------------------------------
# Cleanup uses ONLY its own run's manifests; runs never cross-contaminate.
# ---------------------------------------------------------------------------
export OUT="$OUT1" NS=ns-one RENDER_DIR="$run1_dir" RENDER_ID="$run1_id"
: > "$events"
run_test_cleanup
grep -qF "$run1_dir" "$events" || { echo "run1 cleanup did not use its own manifests" >&2; exit 1; }
! grep -qF "$run2_dir" "$events" || { echo "run1 cleanup leaked into run2 dir" >&2; exit 1; }
# Run 1 scratch collected; run 2 dir untouched by run 1 cleanup.
[ ! -d "$run1_dir" ]
[ -f "$OUT1/.render-collected" ]
[ -d "$run2_dir" ]

export OUT="$OUT2" NS=ns-two RENDER_DIR="$run2_dir" RENDER_ID="$run2_id"
: > "$events"
run_test_cleanup
grep -qF "$run2_dir" "$events"
! grep -qF "$run1_dir" "$events" || { echo "run2 cleanup leaked into run1 dir" >&2; exit 1; }
[ ! -d "$run2_dir" ]
[ -f "$OUT2/.render-collected" ]

# ---------------------------------------------------------------------------
# After collection, render_test_dir never repopulates the collected dir.
# ---------------------------------------------------------------------------
export OUT="$OUT2" NS=ns-two RENDER_DIR="$run2_dir" RENDER_ID="$run2_id"
render_test_dir
[ ! -d "$run2_dir" ] || { echo "collected scratch was repopulated" >&2; exit 1; }

# ---------------------------------------------------------------------------
# --keep retains the rendered evidence (scratch not collected).
# ---------------------------------------------------------------------------
export KEEP=1 OUT="$scratch/out3" NS=ns-keep RENDER_DIR="" RENDER_ID=""
mkdir -p "$scratch/out3"
: > "$events"
render_identity
keep_dir="$RENDER_DIR"
render_test_dir
[ -f "$keep_dir/current-pod.yaml" ]
run_test_cleanup
[ -d "$keep_dir" ] || { echo "--keep scratch was collected" >&2; exit 1; }
[ ! -f "$scratch/out3/.render-collected" ] || { echo "--keep set a collection marker" >&2; exit 1; }
# --keep returns early: no delete was issued for the retained evidence.
[ ! -s "$events" ] || { echo "--keep issued deletes" >&2; exit 1; }
export KEEP=0

# ---------------------------------------------------------------------------
# Parallel selection: two distinct render identities for different NS/worker
# choices never collide — each renders into its own dir, independently.
# ---------------------------------------------------------------------------
export OUT="$scratch/out4" NS=ns-par-a
mkdir -p "$scratch/out4"
render_identity
par_a="$RENDER_DIR"; id_a="$RENDER_ID"
: > "$events"; render_test_dir
export OUT="$scratch/out5" NS=ns-par-b
mkdir -p "$scratch/out5"
render_identity
par_b="$RENDER_DIR"; id_b="$RENDER_ID"
: > "$events"; render_test_dir
[ "$par_a" != "$par_b" ] && [ "$id_a" != "$id_b" ]
grep -q 'namespace: ns-par-a' "$par_a/current-pod.yaml"
grep -q 'namespace: ns-par-b' "$par_b/current-pod.yaml"

# ---------------------------------------------------------------------------
# a pre/run/post/cleanup CHILD process that sources lib.sh must KEEP the
# runner-minted render identity (sourcing lib.sh must not reset it), and it
# must render into the SAME directory — seeing the same substitutions.
# ---------------------------------------------------------------------------
export OUT="$scratch/out6" NS=ns-child
mkdir -p "$scratch/out6"
render_identity
child_dir="$RENDER_DIR"
child_id="$RENDER_ID"
render_test_dir
grep -q 'namespace: ns-child' "$child_dir/current-pod.yaml"
child_seen="$(
    source "$root/lib.sh"
    [ -n "${RENDER_DIR:-}" ] || exit 3
    [ "$RENDER_DIR" = "$child_dir" ] || exit 4
    [ "$RENDER_ID" = "$child_id" ] || exit 5
    render_test_dir
    grep -q 'namespace: ns-child' "$RENDER_DIR/current-pod.yaml" || exit 6
    printf 'same'
)"
[ "$child_seen" = "same" ] || { echo "child process lost or re-minted the render identity (rc=$child_seen)" >&2; exit 1; }

# The runner's OUT is run-unique (second-resolution stamps alone would share
# sentinels between two runs of the same test in one second). Live two-run
# behavior is covered by the scenario suite; here the assignment itself is pinned.
grep -qF 'OUT="$SRC_DIR/logs/$BENCH-$STAMP-$$"' "$root/run.sh" \
    || { echo "run.sh OUT is not run-unique" >&2; exit 1; }

printf '%s\n' 'Render run identity: per-run dir, stale-source-YAML dropped, cleanup owns only its own manifests, scratch collected once (kept on --keep), parallel isolation, child-process identity inheritance, run-unique OUT PASS'
