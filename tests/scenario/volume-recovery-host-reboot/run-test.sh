#!/usr/bin/env bash
source "$CSI_TEST_ROOT/lib.sh"
set -euo pipefail
trap 'rc=$?; run_test_cleanup || true; exit $rc' EXIT
DEPLOY="iscsi-volume-recovery-host-reboot"
NS=default
CTRL_NODES=$(oc get pods -A -o json | python3 -c '
import json, sys
pods = json.load(sys.stdin)["items"]
print(" ".join(sorted({p["spec"]["nodeName"] for p in pods
    if "csi-ngxstorage-controller" in p["metadata"]["name"]
    and p["spec"].get("nodeName")})))
') || fail "controller node bilgisi okunamadı"
REBOOT_SAFE_NODE=""
for n in $(oc get nodes --no-headers | awk '$2=="Ready" && /worker/{print $1}'); do
    case " $CTRL_NODES " in *" $n "*) ;; *) REBOOT_SAFE_NODE=$n; break ;; esac
done
[ -n "$REBOOT_SAFE_NODE" ] || fail "controller barındırmayan worker bulunamadı"
NODE0="$REBOOT_SAFE_NODE"
BOOT0=$(oc get "node/$NODE0" -o jsonpath='{.status.nodeInfo.bootID}')
[ -n "$BOOT0" ] || fail "ilk boot ID okunamadı: $NODE0"
mkdir -p "$OUT"
printf '%s\n' "$NODE0" > "$OUT/reboot-node"
oc apply -f "$TEST_DIR/iscsi-reboot-sc.yaml" -f "$TEST_DIR/iscsi-volume-recovery-host-reboot.yaml" -n "$NS" >/dev/null
oc patch "deploy/$DEPLOY" -n "$NS" --type merge \
    -p "{\"spec\":{\"template\":{\"spec\":{\"nodeSelector\":{\"kubernetes.io/hostname\":\"$NODE0\"}}}}}" >/dev/null
oc rollout status "deploy/$DEPLOY" -n "$NS" --timeout=600s
POD0=$(oc get pods -n "$NS" -l "app=$DEPLOY" -o json | python3 -c '
import json, sys
pods = [p for p in json.load(sys.stdin)["items"] if not p["metadata"].get("deletionTimestamp")]
assert len(pods) == 1, "expected exactly one active pod"
print(pods[0]["metadata"]["name"])
')
[ "$(oc get "pod/$POD0" -n "$NS" -o jsonpath='{.spec.nodeName}')" = "$NODE0" ] || fail "ilk pod yanlış node üzerinde"
note_node "$NODE0"
oc exec "pod/$POD0" -n "$NS" -- sha256sum -c /data/marker.sha256
# Cordon keeps the replacement off the rebooting host without forcing detach.
if [ "${KUBE_BIN##*/}" = oc ]; then oc adm cordon "$NODE0"; else oc cordon "$NODE0"; fi
reboot_node "$NODE0"
oc patch "deploy/$DEPLOY" -n "$NS" --type=json \
    -p '[{"op":"remove","path":"/spec/template/spec/nodeSelector"}]' >/dev/null
oc rollout status "deploy/$DEPLOY" -n "$NS" --timeout=1800s
POD1=$(oc get pods -n "$NS" -l "app=$DEPLOY" -o json | python3 -c '
import json, sys
pods = [p for p in json.load(sys.stdin)["items"] if not p["metadata"].get("deletionTimestamp")]
assert len(pods) == 1, "expected exactly one active pod"
print(pods[0]["metadata"]["name"])
')
NODE1=$(oc get "pod/$POD1" -n "$NS" -o jsonpath='{.spec.nodeName}')
[ -n "$NODE1" ] && [ "$NODE1" != "$NODE0" ] || fail "replacement başka node üzerinde değil"
oc exec "pod/$POD1" -n "$NS" -- sha256sum -c /data/marker.sha256
note_node "$NODE1"
boot_deadline=$((SECONDS + 1800))
BOOT1=""
while [ "$SECONDS" -lt "$boot_deadline" ]; do
    BOOT1=$(oc get "node/$NODE0" -o jsonpath='{.status.nodeInfo.bootID}')
    if [ -n "$BOOT1" ] && [ "$BOOT1" != "$BOOT0" ]; then break; fi
    sleep 10
done
[ -n "$BOOT1" ] && [ "$BOOT1" != "$BOOT0" ] || fail "boot ID değişmedi: $NODE0"
oc wait --for=condition=Ready "node/$NODE0" --timeout=1800s
log "HOST RECOVERY OK: $NODE0 -> $NODE1, checksum OK, boot_id $BOOT0 -> $BOOT1"
