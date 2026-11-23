#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cache="$HOME/.cache/ngxstorage"
mkdir -p "$cache"
scratch="$(mktemp -d "$cache/csi-reboot-contract.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
export OUT="$scratch/out" CSI_TEST_ROOT="$scratch" TEST_DIR="$scratch" KUBE_BIN=oc
mkdir -p "$OUT"
cat > "$scratch/lib.sh" <<'EOF'
log() { :; }
fail() { echo "$*" >&2; exit 1; }
note_node() { :; }
run_test_cleanup() { :; }
reboot_node() { echo "reboot:$1" >> "$OUT/events"; }
oc() {
    echo "$*" >> "$OUT/events"
    case "$*" in
        'get pods -A -o json') echo '{"items":[{"metadata":{"name":"fc-csi-ngxstorage-controller-1"},"spec":{"nodeName":"worker0"}}]}' ;;
        'get nodes --no-headers') printf 'worker0 Ready worker\nworker2 Ready worker\n' ;;
        'get node/worker2 -o jsonpath={.status.nodeInfo.bootID}')
            [ "${EMPTY_BOOT:-0}" != 1 ] || return 0
            if [ -e "$OUT/boot-read" ]; then echo new-boot; else touch "$OUT/boot-read"; echo old-boot; fi ;;
        'get pods -n default -l '*'-o json')
            if [ -e "$OUT/pod-read" ]; then pod=replacement; else touch "$OUT/pod-read"; pod=original; fi
            printf '{"items":[{"metadata":{"name":"%s"}}]}\n' "$pod" ;;
        'get pod/original -n default -o jsonpath={.spec.nodeName}') echo worker2 ;;
        'get pod/replacement -n default -o jsonpath={.spec.nodeName}') echo worker1 ;;
        'wait --for=condition=Ready node/worker2 --timeout=1800s') return "${READY_RC:-0}" ;;
        'rollout status '*) return "${ROLLOUT_RC:-0}" ;;
        *out-of-service*|*finalizers*) echo 'unsafe operation' >&2; return 99 ;;
        apply\ *|patch\ *|exec\ *|adm\ cordon\ *|adm\ uncordon\ *) : ;;
        *) echo "unexpected call: $*" >&2; return 98 ;;
    esac
}
EOF
for mode in success missing-boot failed-rollout; do
    rm -f "$OUT/boot-read" "$OUT/pod-read" "$OUT/events" "$OUT/reboot-node"
    export EMPTY_BOOT=0 ROLLOUT_RC=0
    [ "$mode" != missing-boot ] || export EMPTY_BOOT=1
    [ "$mode" != failed-rollout ] || export ROLLOUT_RC=7
    rc=0
    bash "$root/volume-recovery-host-reboot/run-test.sh" > "$scratch/log" 2>&1 || rc=$?
    if [ "$mode" = success ]; then
        [ "$rc" -eq 0 ] || { cat "$scratch/log"; exit 1; }
        grep -qx 'reboot:worker2' "$OUT/events"
        grep -q '^adm cordon worker2$' "$OUT/events"
    else
        [ "$rc" -ne 0 ]
        ! grep -q '^reboot:' "$OUT/events"
    fi
done
printf '%s\n' worker2 > "$OUT/reboot-node"
: > "$OUT/events"
export READY_RC=1
if bash "$root/volume-recovery-host-reboot/post-test.sh" > "$scratch/log" 2>&1; then exit 1; fi
! grep -q '^adm uncordon' "$OUT/events"
[ -f "$OUT/.cleanup-failed" ]
export READY_RC=0
bash "$root/volume-recovery-host-reboot/post-test.sh" > "$scratch/log" 2>&1
grep -q '^adm uncordon worker2$' "$OUT/events"
printf '%s\n' 'Graceful reboot placement, boot proof and cleanup contract PASS'
