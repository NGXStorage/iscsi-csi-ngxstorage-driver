# shellcheck shell=bash
# Connects csi-sanity to deployed node and controller sockets.
# Temporary binaries use the existing /csi mounts; no rollout is required.

# Verified-prebuilt channel (CSI_PREBUILT_DIR/CSI_PREBUILT_MANIFEST); with the
# env unset every function below keeps its original go-build path unchanged.
# shellcheck source=tests/sanity/cloud/prebuilt.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/prebuilt.sh"

sanity_bin() {
    local bin="${CSI_TEST_CACHE:?}/csi-sanity"
    local build_bin
    mkdir -p "$CSI_TEST_CACHE"
    if csi_prebuilt_enabled; then
        # Explicit verified-prebuilt entry: fail-closed verification, never a
        # silent fallback to an unverified binary or a host build.
        csi_prebuilt_stage csi-sanity "$bin" \
            || fail "csi-sanity verified-prebuilt rejected or staging failed"
        printf '%s\n' "$bin"
        return 0
    fi
    build_bin=$(mktemp "$CSI_TEST_CACHE/csi-sanity.XXXXXXXX") || fail "csi-sanity temporary binary failed"
    # Shared binary from the pinned fork commit (never a stale driver-local copy).
    if ! (cd "$CSI_DRIVER_TESTS/.." && csi_sanity_build_shared "$build_bin"); then
        rm -f "$build_bin"
        fail "shared csi-sanity build from fork @${CSI_TEST_FORK_SHA} failed"
    fi
    mv -f "$build_bin" "$bin" || fail "csi-sanity binary install failed"
    printf '%s\n' "$bin"
}

sanity_proxy_bin() {
    local bin="${CSI_TEST_CACHE:?}/sanity-proxy"
    if csi_prebuilt_enabled; then
        # Re-verified on every call: a cached copy must never outlive the
        # provenance of the CURRENT source.
        csi_prebuilt_stage sanity-proxy "$bin" \
            || fail "sanity-proxy verified-prebuilt rejected or staging failed"
    elif [ ! -x "$bin" ]; then
        mkdir -p "$CSI_TEST_CACHE"
        (cd "$CSI_DRIVER_TESTS/tools/sanity-proxy" && go build -o "$bin" .) \
            || fail "driver-local sanity-proxy build failed"
    fi
    printf '%s\n' "$bin"
}

sanity_ns()      { printf '%s\n' "${CSI_DRIVER_NAMESPACE:-ngxstorage-${PROTOCOL}}"; }
sanity_container() { printf '%s-csi-ngxstorage\n' "$PROTOCOL"; }
sanity_plugin_dir() { printf '%s.csi.ngxstorage.com\n' "$PROTOCOL"; }

# The chroot mount wrapper needs the same absolute kubelet path on the host.
# Bidirectional propagation makes the mounts visible through both bind paths.
sanity_workdir() { printf '/var/lib/kubelet/plugins/%s/sanity-out-%s-%s\n' "$(sanity_plugin_dir)" "${STAMP:?}" "$$"; }

# Pick the running controller/node pod for this protocol. Empty output means
# "not found"; callers decide how to report it (the pipeline must not kill the
# script under set -e).
sanity_pick_pod() { # $1=component
    oc get pods -n "$(sanity_ns)" \
        -l "app.kubernetes.io/name=${PROTOCOL}-csi-ngxstorage,app.kubernetes.io/component=$1" \
        -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name}{"\n"}{end}' 2>/dev/null | head -1 || true
}

# Copy a local binary into a running pod through stdin; the minimal runtime
# image needs neither tar nor cp. Removed again by sanity_withdraw.
sanity_inject() { # $1=pod  $2=local-binary  $3=remote-path
    local pod="$1" binary="$2" remote="$3" mode="${4:-0700}"
    [ -f "$binary" ] && [ -r "$binary" ] || fail "inject: file unreadable: $binary"
    case "$mode" in 0700|0600) ;; *) fail "inject: unsupported mode: $mode" ;; esac
    oc exec -i -n "$(sanity_ns)" "$pod" -c "$(sanity_container)" -- \
        sh -c 'umask 077; tmp="${1}.tmp.$$"; cat > "$tmp" && chmod "$2" "$tmp" && mv -f -- "$tmp" "$1" || { rm -f -- "$tmp"; exit 1; }' sh "$remote" "$mode" < "$binary" \
        || fail "inject: $remote -> $pod aktarılamadı"
}

sanity_cleanup_workdir() {
    local pod="$1" base
    [ -n "$pod" ] || return 0
    base="$(sanity_workdir)"
    oc exec -i -n "$(sanity_ns)" "$pod" -c "$(sanity_container)" -- \
        env SANITY_BASE="$base" sh -s <<'CLEANUP'
set -e
 targets=$(chroot /host findmnt -rn -o TARGET)
 if printf '%s\n' "$targets" | awk -v p="$SANITY_BASE" '$0==p || index($0,p"/")==1 {found=1} END {exit !found}'; then
    echo "sanity cleanup: mounted target remains under $SANITY_BASE" >&2
    exit 1
 fi
 rm -rf -- "$SANITY_BASE"
CLEANUP
}

sanity_withdraw() { # $1=pod  $2..=remote paths
    local pod="$1"; shift
    [ -n "$pod" ] || return 0
    oc exec -n "$(sanity_ns)" "$pod" -c "$(sanity_container)" -- rm -rf -- "$@" >/dev/null 2>&1 || true
}

# Test-only TCP<->unix bridge inside the running controller pod so the node
# test client can reach the controller without exposing its socket. Binds a
# pod-network port and is stopped in teardown. csi-sanity's utils.Connect
# passes the controller address straight to grpc.NewClient, so it must be a
# scheme-less host:port (a "tcp://" prefix never resolves and times out).
sanity_kill_proxies() { # $1=pod
    local pod="$1"
    oc exec -n "$(sanity_ns)" "$pod" -c "$(sanity_container)" -- sh -c '
        for p in /proc/[0-9]*; do
            target=$(readlink "$p/exe" 2>/dev/null || true)
            case "$target" in
                /csi/sanity-proxy|/csi/sanity-proxy\ \(deleted\))
                    kill "${p#/proc/}" 2>/dev/null || true ;;
            esac
        done
        rm -f /csi/sanity-proxy /csi/sanity-proxy.log
        true' >/dev/null 2>&1 || true
}

sanity_bridge_start() { # $1=controller pod -> echoes "ip:port"
    local pod="$1" port="${SANITY_BRIDGE_PORT:-9977}"
    sanity_kill_proxies "$pod"
    sanity_inject "$pod" "$(sanity_proxy_bin)" /csi/sanity-proxy
    # The proxy's startup line goes to stderr so stdout carries only the
    # endpoint for the caller.
    oc exec -n "$(sanity_ns)" "$pod" -c "$(sanity_container)" -- \
        sh -c "nohup /csi/sanity-proxy -listen 0.0.0.0:$port -unix /csi/csi.sock >/csi/sanity-proxy.log 2>&1 & sleep 1; cat /csi/sanity-proxy.log >&2" >/dev/null \
        || fail "sanity-proxy başlatılamadı"
    SANITY_BRIDGE_POD="$pod"
    export SANITY_BRIDGE_POD
    local ip; ip="$(oc get pod -n "$(sanity_ns)" "$pod" -o jsonpath='{.status.podIP}')"
    [ -n "$ip" ] || fail "controller pod IP çözülemedi"
    printf '%s:%s\n' "$ip" "$port"
}

sanity_bridge_stop() {
    local pod="${SANITY_BRIDGE_POD:-}"
    [ -n "$pod" ] || return 0
    sanity_kill_proxies "$pod"
    unset SANITY_BRIDGE_POD
}

# Run csi-sanity inside the running node pod against the live node socket and
# the bridged controller. Evidence lands under $OUT.
sanity_run() { # $1=node pod  $2=controller-endpoint
    local node_pod="$1" controller_ep="$2"
    sanity_withdraw "$node_pod" /csi/csi-sanity
    sanity_inject "$node_pod" "$(sanity_bin)" /csi/csi-sanity

    local base
    base="$(sanity_workdir)"
    oc exec -n "$(sanity_ns)" "$node_pod" -c "$(sanity_container)" -- \
        sh -ec 'umask 077; mkdir "$1"; printf "{}\n" > "$1/secrets.yaml"' sh "$base" \
        || fail "sanity workdir could not be created: $base"
    sanity_inject "$node_pod" "$TEST_DIR/params.yaml" "$base/params.yaml" 0600
    sanity_inject "$node_pod" "$CSI_DRIVER_TESTS/sanity/cloud/mutableparams.yaml" "$base/mutableparams.yaml" 0600
    log "csi-sanity: node=unix:///csi/csi.sock (running pod $node_pod) controller=$controller_ep"
    # tee preserves the suite output locally; the exit status through oc exec
    # is the suite's own status.
    if oc exec -i -n "$(sanity_ns)" "$node_pod" -c "$(sanity_container)" -- env \
        CONTROLLER_EP="$controller_ep" \
        SANITY_BASE="$(sanity_workdir)" \
        SANITY_SKIP="${SANITY_SKIP:-GroupController}" \
        SANITY_ACCESS_TYPE="${SANITY_ACCESS_TYPE:-mount}" \
        SANITY_FOCUS="${SANITY_FOCUS:-}" \
        SANITY_FOCUS_FILE="${SANITY_FOCUS_FILE:-}" \
        SANITY_SHARED_ARRAY="${SANITY_SHARED_ARRAY:-}" \
        sh -s <<'RUN' 2>&1 | tee "$OUT/sanity-suite.log"
set -e
BASE="$SANITY_BASE"
test -r "$BASE/params.yaml"
test -s "$BASE/mutableparams.yaml"
set --
if [ -n "$SANITY_FOCUS" ]; then set -- "$@" "--ginkgo.focus=$SANITY_FOCUS"; fi
if [ -n "$SANITY_FOCUS_FILE" ]; then set -- "$@" "--ginkgo.focus-file=$SANITY_FOCUS_FILE"; fi
if [ -n "$SANITY_SHARED_ARRAY" ]; then set -- "$@" "--shared-array"; fi
if [ "$#" -gt 0 ]; then set -- "$@" --ginkgo.fail-on-empty; fi
/csi/csi-sanity "$@" --ginkgo.vv --ginkgo.no-color --ginkgo.trace \
  --ginkgo.junit-report="$BASE/junit.xml" \
  --csi.endpoint=unix:///csi/csi.sock \
  --csi.controllerendpoint="$CONTROLLER_EP" \
  --csi.testvolumesize=1073741824 --csi.testvolumeexpandsize=2147483648 \
  --csi.testvolumeaccesstype="$SANITY_ACCESS_TYPE" \
  --csi.mountdir="$BASE/mnt" --csi.stagingdir="$BASE/staging" \
  --csi.secrets="$BASE/secrets.yaml" \
  --csi.testvolumeparameters="$BASE/params.yaml" \
  --csi.testvolumemutableparameters="$BASE/mutableparams.yaml" \
  --ginkgo.skip="$SANITY_SKIP"
RUN
    then
        oc exec -n "$(sanity_ns)" "$node_pod" -c "$(sanity_container)" -- cat "$base/junit.xml" > "$OUT/sanity-junit.xml" \
            || fail "sanity JUnit evidence could not be saved"
        log "csi-sanity PASS: $(grep -aE 'Passed \|' "$OUT/sanity-suite.log" | tail -1)"
    else
        oc exec -n "$(sanity_ns)" "$node_pod" -c "$(sanity_container)" -- cat "$base/junit.xml" > "$OUT/sanity-junit.xml" 2>/dev/null || true
        fail "csi-sanity FAIL; evidence=$OUT/sanity-suite.log"
    fi
}
