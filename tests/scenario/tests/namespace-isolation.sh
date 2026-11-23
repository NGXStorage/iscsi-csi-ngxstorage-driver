#!/usr/bin/env bash
# Offline proof for namespace isolation + ownership :
# scenario applies/deletes work on a rendered copy under $TMPDIR against a
# recording oc stub that MODELS the namespace API (a store with real UIDs),
# so that
#   (a) every namespaced call stays inside the chosen isolated namespace,
#   (b) the AUTHORIZED namespace (NS, from the runner's NAMESPACE selection)
#       is the single choice — a fixture's own Namespace manifest is rendered
#       to NS and never used to override it,
#   (c) every namespace-carrying reference is rendered consistently:
#       namespaced metadata, a Namespace manifest's own name, and SCC
#       system:serviceaccount references,
#   (d) ownership requires a PROVABLE create: GET errors other than a clean
#       not-found fail closed; a create race (AlreadyExists) adopts NOTHING;
#       cleanup deletes the namespace only when its UID still matches the
#       recorded create UID,
#   (e) the stub inspects the APPLIED/Created MANIFEST CONTENT, not just args,
#   (f) source fixtures stay byte-identical across repeated runs.
# No cluster is contacted.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cache="$HOME/.cache/ngxstorage"
mkdir -p "$cache"
scratch="$(mktemp -d "$cache/csi-ns-isolation.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
source "$root/lib.sh"

# Record the repo fixture tree before/after the whole test.
hash_fixtures() {
    (cd "$root" && find . -name '*.yaml' -print0 | sort -z | xargs -0 sha256sum)
}
before="$(hash_fixtures)"

bin_dir="$scratch/bin"
events="$scratch/events"
content_log="$scratch/content.log"
nsstore="$scratch/nsstore"
mkdir -p "$bin_dir" "$scratch/tmp" "$nsstore"
export TMPDIR="$scratch/tmp" KUBE_BIN=oc PATH="$bin_dir:$PATH"
: > "$content_log"
cat > "$bin_dir/oc" <<EOF
#!/bin/bash
printf 'oc %s\n' "\$*" >> "$events"
case "\$*" in
  'get nodes --no-headers') printf 'worker0 Ready worker\nworker1 Ready worker\n' ;;
  'get pod/'*) printf 'True\n' ;;
  'get pods -n '*) ;;
  'get namespace/'*)
      name=\$(printf '%s\n' "\$*" | sed -n 's/^get namespace\/\([^ ]*\).*/\1/p')
      if [ -f "$nsstore/\$name" ]; then cat "$nsstore/\$name"; exit 0; fi
      if [ "\${NS_GET_ERR:-0}" = "1" ]; then printf 'Forbidden: simulated RBAC denial\n' >&2; exit 1; fi
      case "\$*" in *--ignore-not-found*) exit 0 ;; *) exit 1 ;; esac ;;
  'create -f '*)
      fpath=\$(printf '%s\n' "\$*" | grep -oE -- '-f [^ ]+' | head -1 | cut -d' ' -f2)
      if [ "\${NS_CREATE_RACE:-0}" = "1" ]; then
          # A competing actor created the namespace between our GET and CREATE.
          printf 'race-uid-1\n' > "$nsstore/race-ns"
          printf 'Error from server (AlreadyExists): namespaces.example.com "race-ns" already exists\n' >&2
          exit 1
      fi
      n=\$(( \$(cat "$scratch/uidseq" 2>/dev/null || printf 0) + 1 )); printf '%s' "\$n" > "$scratch/uidseq"
      if [ "\$fpath" = "-" ]; then
          printf '\n=== CREATED stdin ===\n' >> "$content_log"
          body=\$(cat); printf '%s\n' "\$body" >> "$content_log"
      else
          printf '\n=== CREATED %s ===\n' "\$fpath" >> "$content_log"
          body=\$(cat "\$fpath"); printf '%s\n' "\$body" >> "$content_log"
      fi
      cname=\$(printf '%s\n' "\$body" | sed -n 's/^[[:space:]]*name:[[:space:]]*//p' | head -1)
      if [ "\${NS_CREATE_UID_RACE:-0}" = "1" ]; then
          # V1: the API responded with uid create-uid-N, but the stored object
          # is ALREADY a replacement — the run must never own replacement-uid.
          printf 'replacement-uid\n' > "$nsstore/\$cname"
      else
          printf 'create-uid-%s\n' "\$n" > "$nsstore/\$cname"
      fi
      printf 'ns-created %s uid=create-uid-%s\n' "\$cname" "\$n" >> "$events"
      if [ "\${NS_CREATE_NOUID:-0}" = "1" ]; then
          # V1: success rc but a response without a uid — ownership must NOT
          # be claimed.
          printf '{"metadata":{"name":"%s"}}\n' "\$cname"
          exit 0
      fi
      if [ "\${NS_CREATE_BADJSON:-0}" = "1" ]; then
          # W2: success rc with unparseable response — ownership must NOT be
          # claimed.
          printf 'this is { not json\n'
          exit 0
      fi
      # W2: an annotation that itself carries a "uid" key precedes
      # metadata.uid in pretty-printed output — the parser must ignore it.
      printf '{"kind":"Namespace","metadata":{"name":"%s","annotations":{"uid":"annotation-uid-trap"},"uid":"create-uid-%s"}}\n' "\$cname" "\$n"
      exit 0 ;;
  'delete --raw='*)
      # deletion must arrive as `delete --raw=<path> -f -` whose BODY
      # carries the UID precondition; the server enforces it, the stub models
      # enforcement.
      name=\$(printf '%s\n' "\$*" | sed -n 's|^delete --raw=[^ ]*namespaces/\([^ ]*\).*|\1|p')
      dbody=\$(cat)
      printf '\n=== DELETE BODY %s ===\n%s\n' "\$name" "\$dbody" >> "$content_log"
      puid=\$(printf '%s\n' "\$dbody" | sed -n 's/.*"uid":[[:space:]]*"\([^"]*\)".*/\1/p')
      if [ -z "\$puid" ]; then
          printf 'ns-delete-noprecondition %s\n' "\$name" >> "$events"
          printf 'Error from server: preconditions UID is required in this stub\n' >&2
          exit 1
      fi
      if [ -f "$nsstore/\$name" ] && [ "\$(cat "$nsstore/\$name")" = "\$puid" ]; then
          printf 'ns-deleted %s uid=%s\n' "\$name" "\$puid" >> "$events"
          rm -f "$nsstore/\$name"
          exit 0
      fi
      printf 'ns-delete-conflict %s precondition-uid=%s\n' "\$name" "\$puid" >> "$events"
      printf 'Error from server (Conflict): "preconditions" do not apply... uid mismatch\n' >&2
      exit 1 ;;
  'wait --for=jsonpath='*) exit 0 ;;
  'api-resources --no-headers') printf 'securitycontextconstraints scc\n' ;;
  'apply -f '*)
      fpath=\$(printf '%s\n' "\$*" | grep -oE -- '-f [^ ]+' | head -1 | cut -d' ' -f2)
      if [ "\$fpath" = "-" ]; then
        printf '\n=== APPLIED stdin ===\n' >> "$content_log"; cat >> "$content_log"
      else
        printf '\n=== APPLIED %s ===\n' "\$fpath" >> "$content_log"; cat "\$fpath" >> "$content_log"
      fi
      exit 0 ;;
  'delete '*) exit 0 ;;
  'describe '*) exit 0 ;;
  # Snapshot-flow handlers (only fire for snapshot_scenario_flow commands; the
  # existing scenarios never emit these, so they change no other path).
  'get volumesnapshotclass/'*) printf 'vsc\n'; exit 0 ;;
  'get volumesnapshot '*)
      printf '%s' "\$*" | grep -q 'deletionTimestamp' && exit 0
      printf 'true'; exit 0 ;;
  'patch pvc/'*) exit 0 ;;
  'get pvc/'*)
      printf '%s' "\$*" | grep -q 'capacity' && printf '2Gi'
      exit 0 ;;
  'exec '*)
      printf '%s' "\$*" | grep -qE 'cat .*verify\.txt' && printf 'SNAPFIX'
      exit 0 ;;
  *) printf 'unexpected oc call: %s\n' "\$*" >&2; exit 98 ;;
esac
exit 0
EOF
chmod 755 "$bin_dir/oc"

# Begin a fresh scenario run (mirrors run.sh: fresh OUT + minted render identity).
fresh_run() { # $1 = OUT dir
    unset RENDER_DIR RENDER_ID
    rm -rf "$nsstore" && mkdir -p "$nsstore"; rm -f "$scratch/uidseq"
    OUT="$1"
    mkdir -p "$OUT"
    : > "$events"
    : > "$content_log"
}

preseed_ns() { # $1 name $2 uid — model a foreign/pre-existing namespace
    printf '%s\n' "$2" > "$nsstore/$1"
    printf 'ns-preseeded %s uid=%s\n' "$1" "$2" >> "$events"
}

# =========================================================================
# Scenario 1: no namespace manifest; isolated NS chosen by the runner and
# fixtures pinning `metadata.namespace: default`. The isolated NS is
# pre-existing (foreign), adopted, never deleted.
# =========================================================================
test_dir="$scratch/scenario1"
mkdir -p "$test_dir"
cat > "$test_dir/case-sc.yaml" <<'EOF'
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: case-sc
provisioner: ngxstorage
EOF
cat > "$test_dir/case-pvc.yaml" <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: case-pvc
  namespace: default
spec:
  storageClassName: case-sc
  resources:
    requests:
      storage: 1Gi
EOF
cat > "$test_dir/case-pod.yaml" <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: case-pod
  namespace: default
spec:
  nodeSelector:
    kubernetes.io/hostname: REPLACE_WITH_WORKER_NODE
  containers:
  - name: c
    image: nginx
    command: ["sleep", "600"]
EOF
export TEST_NAME=case TEST_DIR="$test_dir" KEEP=0 MARKER="synthetic"
fresh_run "$scratch/out1"
export NS=iso-ns-test
preseed_ns iso-ns-test foreign-uid-1

apply_wait_pod

# The render itself: namespace pinned, worker placeholder substituted (checked
# before cleanup collects the scratch).
[ "$NS" = "iso-ns-test" ] || { echo "NS was overridden" >&2; exit 1; }
grep -q 'namespace: iso-ns-test' "$RENDER_DIR/case-pod.yaml"
grep -q 'kubernetes.io/hostname: worker0' "$RENDER_DIR/case-pod.yaml"
# The source stayed untouched.
grep -q 'namespace: default' "$test_dir/case-pod.yaml"
grep -q 'REPLACE_WITH_WORKER_NODE' "$test_dir/case-pod.yaml"

run_test_cleanup

# Every namespaced operation targets the isolated namespace; none uses default
# or re-derives a foreign one.
! grep -F -- '-n default' "$events"
grep 'apply -f' "$events" | grep -v 'case-sc.yaml' | grep -qF -- '-n iso-ns-test'
grep 'apply -f' "$events" | grep -v 'case-sc.yaml' | grep -qv -- '-n iso-ns-test' && exit 1
grep 'delete -f' "$events" | grep -v 'case-sc.yaml' | grep -qF -- '-n iso-ns-test'
# Adopted foreign namespace: no ownership marker, no delete request at all.
[ ! -f "$OUT/.ns-owned" ]
! grep -q '=== DELETE BODY' "$content_log"
! grep -F -- 'delete namespace/iso-ns-test' "$events"
[ -f "$nsstore/iso-ns-test" ]
# Applies/deletes come only from the rendered copy, never the source dir.
if grep -E 'apply -f|delete -f' "$events" | grep -qF "$test_dir"; then
    echo "apply/delete reached the source fixtures" >&2; exit 1
fi
grep -E 'apply -f|delete -f' "$events" | grep -qF "$RENDER_DIR"
# The scratch was collected (run identity, --keep off) and only its own dir.
[ ! -d "$RENDER_DIR" ]
[ -f "$OUT/.render-collected" ]

# Second run (fresh render identity, fresh OUT): the placeholder is still in the
# source, so re-rendering reproduces a valid manifest. This is the regression
# the old in-place sed -i broke.
fresh_run "$scratch/out1b"
preseed_ns iso-ns-test foreign-uid-1
export NS=iso-ns-test
apply_wait_pod
grep -q 'kubernetes.io/hostname: worker0' "$RENDER_DIR/case-pod.yaml"
grep -q 'REPLACE_WITH_WORKER_NODE' "$test_dir/case-pod.yaml"

# =========================================================================
# Scenario 2: a test that ships its own Namespace manifest named custom-ns, a
# ServiceAccount, an SCC granting that SA, and namespaced PVC. The AUTHORIZED
# NS (iso-ns-test) is the single selection; the manifest is rendered to it and
# never overrides it. The stub inspects the CREATED/APPLIED MANIFEST CONTENT.
# =========================================================================
test_dir="$scratch/scenario2"
mkdir -p "$test_dir"
cat > "$test_dir/custom-namespace.yaml" <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: custom-ns
EOF
cat > "$test_dir/custom-serviceaccount.yaml" <<'EOF'
apiVersion: v1
kind: ServiceAccount
metadata:
  name: custom-sa
  namespace: custom-ns
EOF
cat > "$test_dir/custom-scc.yaml" <<'EOF'
apiVersion: security.openshift.io/v1
kind: SecurityContextConstraints
metadata:
  name: custom-scc
users:
- system:serviceaccount:custom-ns:custom-sa
EOF
cat > "$test_dir/custom-sc.yaml" <<'EOF'
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: custom-sc
provisioner: ngxstorage
EOF
cat > "$test_dir/custom-pod.yaml" <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: custom-pod
  namespace: custom-ns
spec:
  serviceAccountName: custom-sa
  containers:
  - name: c
    image: nginx
    command: ["sleep", "600"]
EOF
cat > "$test_dir/custom-pvc.yaml" <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: custom-pvc
  namespace: custom-ns
spec:
  storageClassName: custom-sc
  resources:
    requests:
      storage: 1Gi
EOF
export TEST_NAME=custom TEST_DIR="$test_dir" KEEP=0 MARKER="synthetic"

# --- 2A: foreign/pre-existing NS is adopted, never claimed or deleted. ---
fresh_run "$scratch/out2a"
export NS=iso-ns-test
preseed_ns iso-ns-test foreign-uid-2
apply_wait_pod
run_test_cleanup
[ "$NS" = "iso-ns-test" ] || { echo "2A: NS overridden by manifest" >&2; exit 1; }
# No namespaced op may target default or the fixture's foreign custom-ns.
! grep -F -- '-n default' "$events"
! grep -F -- '-n custom-ns' "$events"
! grep -F -- 'system:serviceaccount:custom-ns:' "$content_log"
# The fixture Namespace manifest is never created — it is not the runner's to
# manage; only its rendered content may leak into other fixtures' references.
! grep -q '=== CREATED .*custom-namespace.yaml' "$content_log"
[ ! -f "$OUT/.ns-owned" ] || { echo "2A: foreign NS was claimed" >&2; exit 1; }
# Applied content: SCC SA reference and PVC namespace both rendered to NS.
grep -q 'system:serviceaccount:iso-ns-test:custom-sa' "$content_log"
grep -q 'namespace: iso-ns-test' "$content_log"
# A foreign/pre-existing namespace is never deleted.
! grep -F -- 'delete namespace/iso-ns-test' "$events" || { echo "2A: foreign NS deleted" >&2; exit 1; }
[ -f "$nsstore/iso-ns-test" ]

# --- 2B: missing NS is created from the rendered manifest (owned) and the
# exact created UID is deleted at cleanup. ---
fresh_run "$scratch/out2b"
export NS=iso-ns-test
apply_wait_pod
run_test_cleanup
grep -q '=== CREATED .*custom-namespace.yaml' "$content_log"
awk '/=== CREATED .*custom-namespace.yaml/{f=1;next} /^=== /{f=0} f' "$content_log" \
    | grep -q 'name: iso-ns-test'
! grep -q 'name: custom-ns' "$content_log" || { echo "2B: manifest kept foreign name" >&2; exit 1; }
# Ownership marker records the create UID; cleanup deleted that exact UID.
grep -q '^ns-created iso-ns-test uid=create-uid-1$' "$events"
grep -q '^ns-deleted iso-ns-test uid=create-uid-1$' "$events"
[ "$(sed -n 1p "$OUT/.ns-owned" 2>/dev/null || true)" = "create-uid-1" ]
[ ! -f "$nsstore/iso-ns-test" ] || { echo "2B: owned NS not deleted" >&2; exit 1; }
# Nothing namespaced leaked to default or custom-ns.
! grep -F -- '-n default' "$events"
! grep -F -- '-n custom-ns' "$events"

# =========================================================================
# Scenario 3: a test with NO namespace manifest but a missing isolated NS is
# still created (owned) and cleaned up with the SAME UID — the delete is
# asserted on the namespace itself, not on any delete call.
# =========================================================================
test_dir="$scratch/scenario3"
mkdir -p "$test_dir"
cat > "$test_dir/plain-pod.yaml" <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: plain-pod
  namespace: default
spec:
  containers:
  - name: c
    image: nginx
    command: ["sleep", "600"]
EOF
cat > "$test_dir/plain-pvc.yaml" <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: plain-pvc
  namespace: default
spec:
  storageClassName: plain-sc
  resources:
    requests:
      storage: 1Gi
EOF
export TEST_NAME=plain TEST_DIR="$test_dir" KEEP=0
fresh_run "$scratch/out3"
export NS=iso-ns-test
apply_wait_pod
run_test_cleanup
[ "$NS" = "iso-ns-test" ]
# The isolated NS was created via stdin and owned, then deleted under the
# same UID — pod/PVC deletes alone do not prove namespace cleanup.
grep -q '=== CREATED stdin ===' "$content_log"
awk '/=== CREATED stdin ===/{f=1;next} /^=== /{f=0} f' "$content_log" \
    | grep -q 'name: iso-ns-test'
grep -q '^ns-created iso-ns-test uid=create-uid-1$' "$events"
grep -q '^ns-deleted iso-ns-test uid=create-uid-1$' "$events"
# the delete request ITSELF carries the created UID as a precondition —
# asserting the event alone would pass even for a name-only delete.
grep -q '=== DELETE BODY iso-ns-test ===' "$content_log"
awk '/=== DELETE BODY iso-ns-test ===/{f=1;next} /^=== /{f=0} f' "$content_log" \
    | grep -q '"uid":"create-uid-1"'
! grep -F -- '-n default' "$events"

# =========================================================================
# Scenario 4: create race — GET says missing, CREATE returns
# AlreadyExists (a competing actor won). Nothing is claimed; cleanup keeps
# the foreign namespace.
# =========================================================================
fresh_run "$scratch/out4"
export NS=race-ns
export NS_CREATE_RACE=1
ensure_namespace
[ ! -f "$OUT/.ns-owned" ] || { echo "4: raced namespace was claimed" >&2; exit 1; }
[ -f "$nsstore/race-ns" ]
[ "$(cat "$nsstore/race-ns")" = "race-uid-1" ]
unset NS_CREATE_RACE
run_test_cleanup
[ -f "$nsstore/race-ns" ] || { echo "4: foreign namespace was deleted" >&2; exit 1; }
! grep -F -- 'delete namespace/race-ns' "$events"

# =========================================================================
# Scenario 5: GET failure (403/timeout/5xx) is fail-closed — zero
# mutation, no ownership marker.
# =========================================================================
fresh_run "$scratch/out5"
export NS=locked-ns
export NS_GET_ERR=1
if ensure_namespace 2>/dev/null; then
    echo "5: GET error must fail closed" >&2; exit 1
fi
[ ! -f "$OUT/.ns-owned" ] || { echo "5: ownership claimed on GET error" >&2; exit 1; }
! grep -E 'create -f|apply -f' "$events" || { echo "5: mutation happened despite GET error" >&2; exit 1; }
[ ! -f "$nsstore/locked-ns" ]
unset NS_GET_ERR

# =========================================================================
# Scenario 6 (V1): the create RESPONSE says uid create-uid-N but the stored
# object is already a replacement. Ownership must record the response UID —
# never the replacement's — and cleanup must refuse (precondition conflict),
# keeping the replacement and surfacing cleanup-failed.
# =========================================================================
fresh_run "$scratch/out6"
export NS=v1-ns
export NS_CREATE_UID_RACE=1
ensure_namespace
unset NS_CREATE_UID_RACE
[ -f "$OUT/.ns-owned" ] || { echo "6: proven create left no ownership record" >&2; exit 1; }
recorded_uid="$(sed -n 1p "$OUT/.ns-owned")"
[ "$recorded_uid" = "create-uid-1" ] \
    || { echo "6: ownership recorded $recorded_uid, must be the create-response uid" >&2; exit 1; }
[ "$(cat "$nsstore/v1-ns")" = "replacement-uid" ]
cleanup_rc=0
run_test_cleanup || cleanup_rc=$?
[ "$cleanup_rc" -eq 1 ] \
    || { echo "6: precondition conflict must surface as cleanup failure" >&2; exit 1; }
[ "$(cat "$nsstore/v1-ns")" = "replacement-uid" ] \
    || { echo "6: replacement namespace was deleted" >&2; exit 1; }
grep -q '^ns-delete-conflict v1-ns precondition-uid=create-uid-1$' "$events"
[ -f "$OUT/.cleanup-failed" ] || { echo "6: precondition conflict must surface cleanup-failed" >&2; exit 1; }

# =========================================================================
# Scenario 7 (V1): a create that returns success rc but NO uid in the
# response must record NOTHING and fail closed.
# =========================================================================
fresh_run "$scratch/out7"
export NS=v1b-ns
export NS_CREATE_NOUID=1
rc=0
ensure_namespace || rc=$?
unset NS_CREATE_NOUID
[ "$rc" -ne 0 ] || { echo "7: uid-less create response was accepted" >&2; exit 1; }
[ ! -f "$OUT/.ns-owned" ] || { echo "7: ownership claimed from a uid-less response" >&2; exit 1; }
[ -f "$nsstore/v1b-ns" ] || { echo "7: object was not created by the stub" >&2; exit 1; }
run_test_cleanup
[ -f "$nsstore/v1b-ns" ] || { echo "7: unowned namespace was deleted" >&2; exit 1; }

# =========================================================================
# Scenario 8 (W2): a create that succeeds with UNPARSEABLE JSON must record
# nothing and fail closed (the annotation-uid trap in the happy-path response
# is covered by scenarios 2B/3 asserting the recorded uid is the
# metadata.uid, not annotation-uid-trap).
# =========================================================================
fresh_run "$scratch/out8"
export NS=v2-ns
export NS_CREATE_BADJSON=1
rc=0
ensure_namespace || rc=$?
unset NS_CREATE_BADJSON
[ "$rc" -ne 0 ] || { echo "8: bad-JSON create response was accepted" >&2; exit 1; }
[ ! -f "$OUT/.ns-owned" ] || { echo "8: ownership claimed from bad JSON" >&2; exit 1; }
run_test_cleanup
[ -f "$nsstore/v2-ns" ] || { echo "8: unowned namespace was deleted" >&2; exit 1; }

# =========================================================================
# Scenario 9 (snapshot-flow regression): snapshot_scenario_flow must ensure the
# authorized namespace BEFORE applying the first namespaced object. The namespace
# create previously lived only in apply_wait_pod; the snapshot branch rendered
# the ns but never created it, so the first namespaced apply (source PVC) hit
# "namespace not found". Drives the real flow against the stub, which models the
# snapshot/patch/capacity/exec calls so it completes offline.
# =========================================================================
test_dir="$scratch/scenario9"
mkdir -p "$test_dir"
for m in "snapfix-sc.yaml:StorageClass" "snapfix-vsc.yaml:VolumeSnapshotClass" \
         "snapfix-source-pvc.yaml:PersistentVolumeClaim" "snapfix-source-pod.yaml:Pod" \
         "snapfix-vs.yaml:VolumeSnapshot" "snapfix-restore-pvc.yaml:PersistentVolumeClaim" \
         "snapfix-restore-pod.yaml:Pod"; do
    f="${m%%:*}"; k="${m##*:}"
    cat > "$test_dir/$f" <<EOF
apiVersion: v1
kind: $k
metadata:
  name: $f
  namespace: default
spec: {}
EOF
done
export TEST_NAME=snapfix TEST_DIR="$test_dir" KEEP=0 MARKER="SNAPFIX"
fresh_run "$scratch/out9"
export NS=snap-iso-ns
snapshot_scenario_flow
run_test_cleanup
# The namespace was created and OWNED, and the create PRECEDED the first apply.
grep -q '^ns-created snap-iso-ns ' "$events" \
    || { echo "9: snapshot flow never created the authorized namespace" >&2; exit 1; }
first_apply_line=$(grep -n 'oc apply -f' "$events" | head -1 | cut -d: -f1)
ns_created_line=$(grep -n '^ns-created snap-iso-ns ' "$events" | head -1 | cut -d: -f1)
[ "$ns_created_line" -lt "$first_apply_line" ] \
    || { echo "9: namespace create did not precede the first namespaced apply" >&2; exit 1; }
# The owned namespace was cleaned up under the same UID.
grep -q '^ns-deleted snap-iso-ns ' "$events" \
    || { echo "9: owned namespace was not cleaned" >&2; exit 1; }
[ ! -f "$nsstore/snap-iso-ns" ]

# Byte-identity of the whole repo fixture tree after the run.
after="$(hash_fixtures)"
[ "$before" = "$after" ] || { echo "source fixtures changed during the run" >&2; exit 1; }

printf '%s\n' 'Namespace isolation: authorized-NS single selection, applied-content inspection, SCC SA render, foreign-NS never deleted, metadata.uid-only ownership from the create response (annotation-uid trap ignored, replacement never claimed, uid-less/bad-JSON response fail-closed), DELETE-body UID precondition via delete --raw -f - (conflict keeps replacement + cleanup-failed), GET fail-closed, stdin-created NS same-UID cleanup, snapshot-flow ensures namespace before first namespaced apply, source byte-identity PASS'
