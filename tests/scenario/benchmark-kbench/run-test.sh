#!/usr/bin/env bash
source "$CSI_TEST_ROOT/lib.sh"
set -euo pipefail
trap 'rc=$?; run_test_cleanup || true; exit $rc' EXIT
NS=$(grep -h "^  name:" "$TEST_DIR"/*namespace*.yaml | head -1 | awk "{print \$2}")
export NS
# kbench runs to completion as a plain Pod (worker-pinned, root) and the test
# waits for its Succeeded phase.
oc apply -f "$TEST_DIR"/*namespace*.yaml >/dev/null
oc apply -f "$TEST_DIR"/*serviceaccount*.yaml -n "$NS" >/dev/null
for f in $(find "$TEST_DIR" -name '*.yaml' ! -name '*scc*.yaml' \
    ! -name '*namespace*.yaml' ! -name '*serviceaccount*.yaml'); do
    oc apply -f "$f" -n "$NS" >/dev/null
done
# OpenShift: a dedicated SCC because the namespace UID range moves on every
# recreation and cannot be pinned in the pod spec; the grant needs a minute
# or two to propagate to admission, so sleep before waiting.
if oc api-resources --no-headers 2>/dev/null | grep -qi securitycontextconstraints; then
    oc apply -f "$TEST_DIR"/*scc*.yaml >/dev/null
    sleep 90
fi
oc wait --for=jsonpath='{.status.phase}'=Bound "pvc/benchmark-kbench-iscsi-pvc" -n "$NS" --timeout=600s \
    || { oc describe "pvc/benchmark-kbench-iscsi-pvc" -n "$NS" | tail -15; fail "kbench PVC Bound olmadı"; }
oc wait --for=jsonpath='{.status.phase}'=Succeeded "pod/benchmark-kbench-iscsi" -n "$NS" --timeout="${KBENCH_TIMEOUT:-1200}s" \
    || { oc describe "pod/benchmark-kbench-iscsi" -n "$NS" | tail -40; fail "kbench pod tamamlanmadı"; }
oc logs "pod/benchmark-kbench-iscsi" -n "$NS" --tail=30
