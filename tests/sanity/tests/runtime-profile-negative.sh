#!/usr/bin/env bash
# Offline dispatcher-level negatives for the runtime profile contract (B12, iSCSI).
#
# Runs the REAL tests/run-all-tests.sh inside a synthetic integration tree and
# proves, without any live access:
#   * unset/empty required runtime env fails fast (rc=2) NAMING the variable;
#   * the internal Tests/infrastructure markdown is never read (grep contract
#     pins + a tripwire file at the old scrape path + python3/cat tool stubs
#     that record any read/build attempt);
#   * no network/build/elevation tool is invoked when the profile is missing
#     (curl/wget/nc/oc/kubectl/ssh/go/sudo stubs must stay untouched);
#   * file mode stays authoritative through the dispatcher (partial file +
#     full stale env is rejected);
#   * the --allow-live gate and the scenario credential contract still hold;
#   * the prebuilt e2e.test dispatch goes through the ONE shared fail-closed
#     gate (sha256/arch/CURRENT-source fingerprint/smoke probe); explicit
#     prebuilt mode with a missing binary is a hard failure — the regular go
#     test path is used only when CSI_PREBUILT_DIR is unset.
# Synthetic values only — no real credential is ever read or attempted.
set -euo pipefail
sanity_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tests_root="$(cd "$sanity_root/.." && pwd)"
driver_root="$(cd "$tests_root/.." && pwd)"
driver_name="$(basename "$driver_root")"
cache="$HOME/.cache/ngxstorage"
mkdir -p "$cache"
scratch="$(mktemp -d "$cache/csi-runtime-negative.XXXXXX")"
trash="$HOME/.cache/opencode/trash"
mkdir -p "$trash"
trap 'mv "$scratch" "$trash/" 2>/dev/null || true' EXIT

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=contract GIT_AUTHOR_EMAIL=contract@example.com
export GIT_COMMITTER_NAME=contract GIT_COMMITTER_EMAIL=contract@example.com

KEYS=(STORAGE_API_KEY STORAGE_IPS STORAGE_POOL_NAME STORAGE_PORTAL_GROUP)
SYNTH=(synthetic-not-a-real-key 192.0.2.1 synthetic-pool synthetic-portal)

# --- Synthetic integration tree: same depth as the real layout so the old
# --- scrape path (driver_root/../../../..) resolves inside the scratch.
fake_int="$scratch/int"
fake_driver="$fake_int/ContainerStorage/CSI/Driver/$driver_name"
mkdir -p "$fake_driver/tests/sanity/local" "$fake_driver/tests/sanity/cloud" \
         "$fake_driver/tests/e2e" \
         "$fake_driver/tests/scenario/volume-existing-static" \
         "$fake_int/Tests/infrastructure/cluster-infrastructure"
cp "$driver_root/tests/run-all-tests.sh" "$fake_driver/tests/run-all-tests.sh"
cp "$sanity_root/local/runtime-profile.sh" "$fake_driver/tests/sanity/local/runtime-profile.sh"
cp "$sanity_root/cloud/prebuilt.sh" "$sanity_root/cloud/prebuilt.py" \
   "$fake_driver/tests/sanity/cloud/"
: > "$fake_driver/tests/scenario/run.sh"
: > "$fake_driver/tests/scenario/volume-existing-static/run-test.sh"
# The synthetic driver tree is a real git repo with a go.mod so the shared
# prebuilt gate can recompute the CURRENT-source fingerprint at dispatch time.
cat > "$fake_driver/go.mod" <<'EOF'
module fixture.example/driver

go 1.27.0
EOF
git -C "$fake_driver" init -q
git -C "$fake_driver" add -A
git -C "$fake_driver" commit -qm fixture
fake_config="$fake_driver/tests/sanity/local/config.env"

# Tripwire at the exact path the removed doc-scrape used to read. Any attempt
# to open it via python3/cat is recorded by the stubs; its sentinel must never
# reach the dispatcher output.
sentinel="LAB-SENTINEL-DO-NOT-READ-$$-$RANDOM"
cat > "$fake_int/Tests/infrastructure/cluster-infrastructure/test-infrastructure.md" <<EOF
### V6BGWYPA
| V6BGWYPA_A | \`$sentinel\` | 192.0.2.1 |
| V6BGWYPA_B | \`$sentinel-b\` | 192.0.2.2 |
- synthetic-pool - A node
- ISCSI
  - synthetic-portal - A node
EOF

# --- Tool stubs: record every invocation, then fail. The dispatcher must
# --- reach its profile verdict without touching any of them.
stub_calls="$scratch/stub-calls"
: > "$stub_calls"
e2e_calls="$scratch/e2e-calls"
: > "$e2e_calls"
export E2E_CALL_LOG="$e2e_calls"
mkdir -p "$scratch/bin" "$scratch/bin-prebuilt"
make_stub() { # $1=dir $2=tool
    cat > "$1/$2" <<EOF
#!/bin/sh
printf '%s %s\n' '$2' "\$*" >> '$stub_calls'
exit 97
EOF
    chmod 700 "$1/$2"
}
for tool in python3 go sudo curl wget nc oc kubectl ssh cat; do make_stub "$scratch/bin" "$tool"; done
# Prebuilt verification needs REAL python3 and git (the shared gate); only
# build/network tools stay stubbed on that path.
for tool in go sudo curl wget nc oc kubectl ssh; do make_stub "$scratch/bin-prebuilt" "$tool"; done

# Everything the operator environment might carry is cleared for each run.
CLEAR=(env -u STORAGE_API_KEY -u STORAGE_IPS -u STORAGE_POOL_NAME -u STORAGE_PORTAL_GROUP
       -u NGX_CONTROLLER -u NGX_API_KEY -u NGX_POOL_NAME -u NGX_DATA_IP -u NGX_SDK_CLI
       -u KUBECONFIG -u KUBE_CONTEXT -u BENCH -u KUBE_BIN
       -u CSI_PREBUILT_DIR -u CSI_PREBUILT_MANIFEST
       -u SANITY_MOCK_COORDINATION -u SANITY_FOCUS -u SANITY_FOCUS_FILE -u SANITY_SKIP_TESTS
       -u E2E_RUN -u E2E_TEST_BINARY)

check_clean() { # $1=name $2=msg $3=rc — rc=2, no tool call, no e2e exec, no sentinel leak
    local name="$1" msg="$2" rc="$3"
    [ "$rc" -eq 2 ] || { echo "$name: expected rc=2, got rc=$rc"; echo "$msg"; exit 1; }
    if [ -s "$stub_calls" ]; then echo "$name: forbidden tool invoked:"; cat "$stub_calls"; exit 1; fi
    if [ -s "$e2e_calls" ]; then echo "$name: e2e.test executed despite rejection:"; cat "$e2e_calls"; exit 1; fi
    if grep -Fq "$sentinel" <<< "$msg"; then echo "$name: lab sentinel leaked into output"; echo "$msg"; exit 1; fi
}
expect_named() { # $1=name $2=msg $3...=needles
    local name="$1" msg="$2"; shift 2
    local needle
    for needle in "$@"; do
        grep -Fq -- "$needle" <<< "$msg" || { echo "$name: '$needle' not reported"; echo "$msg"; exit 1; }
    done
}

# 1. ENV mode, no profile at all: every required key must be named.
rc=0 msg=""
msg="$("${CLEAR[@]}" PATH="$scratch/bin:$PATH" bash "$fake_driver/tests/run-all-tests.sh" sanity local --allow-live 2>&1)" || rc=$?
check_clean "absent-profile" "$msg" "$rc"
expect_named "absent-profile" "$msg" "${KEYS[@]}"

# 2. ENV mode: each key unset in turn (others synthetic) must be named.
# CLEAR already unsets all four keys; only the present ones are assigned
# (env stops option parsing at the first assignment, so no -u goes here).
for ((i = 0; i < 4; i++)); do
    envargs=()
    for ((j = 0; j < 4; j++)); do
        if ((j != i)); then envargs+=("${KEYS[$j]}=${SYNTH[$j]}"); fi
    done
    rc=0 msg=""
    msg="$("${CLEAR[@]}" "${envargs[@]}" PATH="$scratch/bin:$PATH" bash "$fake_driver/tests/run-all-tests.sh" sanity local --allow-live 2>&1)" || rc=$?
    check_clean "unset-${KEYS[$i]}" "$msg" "$rc"
    expect_named "unset-${KEYS[$i]}" "$msg" "${KEYS[$i]}"
done

# 3. ENV mode: an EMPTY value counts as missing (each key in turn).
for ((i = 0; i < 4; i++)); do
    envargs=()
    for ((j = 0; j < 4; j++)); do
        if ((j == i)); then envargs+=("${KEYS[$j]}="); else envargs+=("${KEYS[$j]}=${SYNTH[$j]}"); fi
    done
    rc=0 msg=""
    msg="$("${CLEAR[@]}" "${envargs[@]}" PATH="$scratch/bin:$PATH" bash "$fake_driver/tests/run-all-tests.sh" sanity local --allow-live 2>&1)" || rc=$?
    check_clean "empty-${KEYS[$i]}" "$msg" "$rc"
    expect_named "empty-${KEYS[$i]}" "$msg" "${KEYS[$i]}"
done

# 4. FILE mode through the dispatcher: a partial config.env masked by a full
# stale environment is rejected; the missing keys are named, values never.
printf 'STORAGE_API_KEY="synthetic-only-key"\n' > "$fake_config"
rc=0 msg=""
msg="$("${CLEAR[@]}" STORAGE_API_KEY=stale-key STORAGE_IPS=192.0.2.9 STORAGE_POOL_NAME=stale-pool "${KEYS[3]}=stale-extra" PATH="$scratch/bin:$PATH" bash "$fake_driver/tests/run-all-tests.sh" sanity local --allow-live 2>&1)" || rc=$?
check_clean "partial-file-full-env" "$msg" "$rc"
expect_named "partial-file-full-env" "$msg" STORAGE_IPS STORAGE_POOL_NAME "${KEYS[3]}"
if grep -Fq 'stale-key' <<< "$msg"; then echo "partial-file-full-env: credential value leaked"; echo "$msg"; exit 1; fi

# 5. FILE mode: a complete config.env passes the profile and the dispatcher
# proceeds to the binary preflight (which fails in the synthetic tree) —
# proving acceptance happens without any live/build access.
cat > "$fake_config" <<EOF
STORAGE_API_KEY="${SYNTH[0]}"
STORAGE_IPS="${SYNTH[1]}"
STORAGE_POOL_NAME="${SYNTH[2]}"
${KEYS[3]}="${SYNTH[3]}"
EOF
rc=0 msg=""
msg="$("${CLEAR[@]}" PATH="$scratch/bin:$PATH" bash "$fake_driver/tests/run-all-tests.sh" sanity local --allow-live 2>&1)" || rc=$?
check_clean "complete-file" "$msg" "$rc"
expect_named "complete-file" "$msg" binary
mv "$fake_config" "$scratch/config.env.complete.aside"

# 6. Scenario runtime profile: unset NGX_CONTROLLER/credential/NGX_POOL_NAME
# must each be named; no tool may run.
rc=0 msg=""
msg="$("${CLEAR[@]}" BENCH=k8s PATH="$scratch/bin:$PATH" bash "$fake_driver/tests/run-all-tests.sh" scenario volume-existing-static --allow-live 2>&1)" || rc=$?
check_clean "scenario-absent-profile" "$msg" "$rc"
expect_named "scenario-absent-profile" "$msg" NGX_CONTROLLER NGX_POOL_NAME STORAGE_API_KEY/NGX_API_KEY

# 7. Scenario credential alternative: NGX_API_KEY alone satisfies the runtime
# check (the runner's precedence); dispatch then stops at the missing SDK CLI.
rc=0 msg=""
msg="$("${CLEAR[@]}" BENCH=k8s NGX_CONTROLLER=192.0.2.1 NGX_POOL_NAME=synthetic-pool NGX_API_KEY=synthetic-cred PATH="$scratch/bin:$PATH" bash "$fake_driver/tests/run-all-tests.sh" scenario volume-existing-static --allow-live 2>&1)" || rc=$?
check_clean "scenario-ngx-api-key" "$msg" "$rc"
expect_named "scenario-ngx-api-key" "$msg" ngxstorage-cli
if grep -Fq 'runtime profile incomplete' <<< "$msg"; then echo "scenario-ngx-api-key: NGX_API_KEY alternative rejected"; echo "$msg"; exit 1; fi

# 8. The --allow-live gate still precedes everything.
rc=0 msg=""
msg="$("${CLEAR[@]}" PATH="$scratch/bin:$PATH" bash "$fake_driver/tests/run-all-tests.sh" sanity local 2>&1)" || rc=$?
check_clean "allow-live-gate" "$msg" "$rc"
expect_named "allow-live-gate" "$msg" --allow-live

# 9. Grep contract pins on the REAL dispatcher: the doc-scrape channel stays
# gone and the runtime-profile preflight stays wired.
! grep -q 'test-infrastructure.md' "$driver_root/tests/run-all-tests.sh"
! grep -q 'V6BGWYPA' "$driver_root/tests/run-all-tests.sh"
! grep -q 'infra_file' "$driver_root/tests/run-all-tests.sh"
! grep -q 'lab_defaults' "$driver_root/tests/run-all-tests.sh"
grep -q 'sanity_profile_preflight' "$driver_root/tests/run-all-tests.sh"
grep -q 'csi_prebuilt_verify e2e.test' "$driver_root/tests/run-all-tests.sh"
if grep -q 'prebuilt_e2e_verify' "$driver_root/tests/run-all-tests.sh"; then
    echo "dispatcher still carries the old inline verifier" >&2; exit 1
fi

# --- Prebuilt e2e.test dispatch: the ONE shared fail-closed gate
# --- (prebuilt.py via prebuilt.sh csi_prebuilt_verify) before exec.
kubeconfig="$scratch/kubeconfig"; : > "$kubeconfig"
pb_dir="$scratch/prebuilt"; mkdir -p "$pb_dir"
marker="$scratch/e2e-exec-marker"
export E2E_EXEC_MARKER="$marker"
# The stub records EVERY invocation (the gate's -h smoke probe included);
# only a real test exec writes the marker.
cat > "$pb_dir/e2e.test" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$E2E_CALL_LOG"
if [ "${1:-}" = "-h" ]; then exit 0; fi
printf 'E2E_RUN=%s ARGS=%s\n' "${E2E_RUN:-unset}" "$*" > "$E2E_EXEC_MARKER"
exit 0
EOF
chmod 700 "$pb_dir/e2e.test"
host_os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
    x86_64) host_arch=amd64 ;;
    aarch64|arm64) host_arch=arm64 ;;
    *) host_arch="$(uname -m)" ;;
esac
other_arch=arm64; [[ "$host_arch" == arm64 ]] && other_arch=amd64
sha="$(sha256sum "$pb_dir/e2e.test" | cut -d' ' -f1)"
manifest="$pb_dir/manifest.json"
# CURRENT-source fingerprint of the synthetic git driver tree (real algorithm).
tree_fp="$(python3 "$fake_driver/tests/sanity/cloud/prebuilt.py" fingerprint "$fake_driver")"
write_manifest() { # $1=sha $2=goos $3=goarch $4=source fingerprint
    cat > "$manifest" <<EOF
{"e2e.test": {"sha256": "$1", "goos": "$2", "goarch": "$3", "source_fingerprint": "$4", "go_version": "go1.25.0", "build_flags": "-trimpath", "built_at": "2026-11-16T00:00:00Z"}}
EOF
}
pb_run() { # $@ = extra env assignments; runs the e2e suite with stubs (real python3/git)
    : > "$e2e_calls"
    "${CLEAR[@]}" "$@" KUBECONFIG="$kubeconfig" PATH="$scratch/bin-prebuilt:$PATH" \
        bash "$fake_driver/tests/run-all-tests.sh" e2e --allow-live
}

# 10. Binary present but no manifest: refuse (fail-closed).
rc=0 msg=""
msg="$(pb_run CSI_PREBUILT_DIR="$pb_dir" 2>&1)" || rc=$?
check_clean "prebuilt-no-manifest" "$msg" "$rc"
expect_named "prebuilt-no-manifest" "$msg" CSI_PREBUILT_MANIFEST

# 11. sha256 mismatch: refuse.
write_manifest 0000000000000000000000000000000000000000000000000000000000000000 "$host_os" "$host_arch" "$tree_fp"
rc=0 msg=""
msg="$(pb_run CSI_PREBUILT_DIR="$pb_dir" CSI_PREBUILT_MANIFEST="$manifest" 2>&1)" || rc=$?
check_clean "prebuilt-bad-sha" "$msg" "$rc"
expect_named "prebuilt-bad-sha" "$msg" 'sha256 mismatch'

# 12. Wrong architecture: refuse.
write_manifest "$sha" "$host_os" "$other_arch" "$tree_fp"
rc=0 msg=""
msg="$(pb_run CSI_PREBUILT_DIR="$pb_dir" CSI_PREBUILT_MANIFEST="$manifest" 2>&1)" || rc=$?
check_clean "prebuilt-wrong-arch" "$msg" "$rc"
expect_named "prebuilt-wrong-arch" "$msg" 'execution host is'

# 13. Manifest without an e2e.test entry: refuse.
printf '{"csi-sanity": {"sha256": "%s"}}\n' "$sha" > "$manifest"
rc=0 msg=""
msg="$(pb_run CSI_PREBUILT_DIR="$pb_dir" CSI_PREBUILT_MANIFEST="$manifest" 2>&1)" || rc=$?
check_clean "prebuilt-no-entry" "$msg" "$rc"
expect_named "prebuilt-no-entry" "$msg" 'no entry for e2e.test'

# 13a. Zero source_fingerprint (64 zeros) with valid hash/arch: refuse —
# hash+arch alone are not provenance.
write_manifest "$sha" "$host_os" "$host_arch" 0000000000000000000000000000000000000000000000000000000000000000
rc=0 msg=""
msg="$(pb_run CSI_PREBUILT_DIR="$pb_dir" CSI_PREBUILT_MANIFEST="$manifest" 2>&1)" || rc=$?
check_clean "prebuilt-zero-fingerprint" "$msg" "$rc"
expect_named "prebuilt-zero-fingerprint" "$msg" 'source fingerprint mismatch'

# 13b. Stale tracked source in the synthetic driver tree: the recomputed
# CURRENT fingerprint differs, so the same manifest is refused.
write_manifest "$sha" "$host_os" "$host_arch" "$tree_fp"
printf '# stale edit\n' >> "$fake_driver/tests/scenario/volume-existing-static/run-test.sh"
rc=0 msg=""
msg="$(pb_run CSI_PREBUILT_DIR="$pb_dir" CSI_PREBUILT_MANIFEST="$manifest" 2>&1)" || rc=$?
check_clean "prebuilt-stale-source" "$msg" "$rc"
expect_named "prebuilt-stale-source" "$msg" 'source fingerprint mismatch'
git -C "$fake_driver" checkout -q -- tests/scenario/volume-existing-static/run-test.sh

# 14. Valid current-source manifest: the verified prebuilt binary is exec'd
# exactly once with E2E_RUN=1 and the go-test flag mapping (plus the gate's
# single -h smoke probe); no build tool runs.
write_manifest "$sha" "$host_os" "$host_arch" "$tree_fp"
: > "$marker"; : > "$e2e_calls"
rc=0 msg=""
msg="$(pb_run CSI_PREBUILT_DIR="$pb_dir" CSI_PREBUILT_MANIFEST="$manifest" 2>&1)" || rc=$?
[ "$rc" -eq 0 ] || { echo "prebuilt-valid: expected rc=0, got rc=$rc"; echo "$msg"; exit 1; }
[ -s "$stub_calls" ] && { echo "prebuilt-valid: build tool invoked:"; cat "$stub_calls"; exit 1; }
[ -f "$marker" ] || { echo "prebuilt-valid: prebuilt binary was not exec'd"; echo "$msg"; exit 1; }
grep -Fq 'E2E_RUN=1' "$marker" || { echo "prebuilt-valid: E2E_RUN not passed"; cat "$marker"; exit 1; }
grep -Fq -- '-test.timeout=45m' "$marker" || { echo "prebuilt-valid: timeout flag not mapped"; cat "$marker"; exit 1; }
[ "$(wc -l < "$e2e_calls")" -eq 2 ] || { echo "prebuilt-valid: expected probe + one exec"; cat "$e2e_calls"; exit 1; }
[ "$(grep -c -- '^-h$' "$e2e_calls")" -eq 1 ] || { echo "prebuilt-valid: expected exactly one smoke probe"; cat "$e2e_calls"; exit 1; }
[ "$(grep -c -- '-test.timeout=45m' "$e2e_calls")" -eq 1 ] || { echo "prebuilt-valid: expected exactly one test exec"; cat "$e2e_calls"; exit 1; }

# 15. CSI_PREBUILT_DIR set but no e2e.test inside: HARD failure. Explicit
# prebuilt mode never falls back to a host go build (the old dispatcher did).
empty_dir="$scratch/prebuilt-empty"; mkdir -p "$empty_dir"
: > "$stub_calls"; : > "$e2e_calls"
rc=0 msg=""
msg="$(pb_run CSI_PREBUILT_DIR="$empty_dir" CSI_PREBUILT_MANIFEST="$manifest" 2>&1)" || rc=$?
check_clean "prebuilt-absent-binary" "$msg" "$rc"
expect_named "prebuilt-absent-binary" "$msg" 'missing or not a regular file'

# 16. Prebuilt env passthrough: the scenario dispatch forwards
# CSI_PREBUILT_DIR / CSI_PREBUILT_MANIFEST to the runner UNCHANGED (the
# consumers verify; the dispatcher never re-validates them on this path).
cat > "$fake_driver/tests/scenario/run.sh" <<'EOF'
#!/usr/bin/env bash
printf 'DIR=%s MANIFEST=%s ALLOW_LIVE=%s\n' "${CSI_PREBUILT_DIR:-unset}" "${CSI_PREBUILT_MANIFEST:-unset}" "${ALLOW_LIVE:-unset}" > "$PREBUILT_PASSTHROUGH_MARKER"
EOF
mkdir -p "$fake_driver/tests/tools/bin"
printf '#!/bin/sh\nexit 0\n' > "$fake_driver/tests/tools/bin/ngxstorage-cli"
chmod 700 "$fake_driver/tests/tools/bin/ngxstorage-cli"
export PREBUILT_PASSTHROUGH_MARKER="$scratch/passthrough-marker"
: > "$stub_calls"
rc=0 msg=""
msg="$("${CLEAR[@]}" BENCH=k8s NGX_CONTROLLER=192.0.2.1 NGX_POOL_NAME=synthetic-pool STORAGE_API_KEY=synthetic-cred CSI_PREBUILT_DIR="$pb_dir" CSI_PREBUILT_MANIFEST="$manifest" PATH="$scratch/bin:$PATH" bash "$fake_driver/tests/run-all-tests.sh" scenario volume-existing-static --allow-live 2>&1)" || rc=$?
[ "$rc" -eq 0 ] || { echo "prebuilt-passthrough: expected rc=0, got rc=$rc"; echo "$msg"; exit 1; }
[ -s "$stub_calls" ] && { echo "prebuilt-passthrough: forbidden tool invoked:"; cat "$stub_calls"; exit 1; }
grep -Fq "DIR=$pb_dir MANIFEST=$manifest ALLOW_LIVE=1" "$PREBUILT_PASSTHROUGH_MARKER" \
    || { echo "prebuilt-passthrough: env not forwarded unchanged"; cat "$PREBUILT_PASSTHROUGH_MARKER"; exit 1; }
rc=0 msg=""
msg="$("${CLEAR[@]}" BENCH=k8s NGX_CONTROLLER=192.0.2.1 NGX_POOL_NAME=synthetic-pool STORAGE_API_KEY=synthetic-cred PATH="$scratch/bin:$PATH" bash "$fake_driver/tests/run-all-tests.sh" scenario volume-existing-static --allow-live 2>&1)" || rc=$?
[ "$rc" -eq 0 ] || { echo "prebuilt-passthrough-unset: expected rc=0, got rc=$rc"; echo "$msg"; exit 1; }
grep -Fq 'DIR=unset MANIFEST=unset ALLOW_LIVE=1' "$PREBUILT_PASSTHROUGH_MARKER" \
    || { echo "prebuilt-passthrough-unset: unset env not preserved"; cat "$PREBUILT_PASSTHROUGH_MARKER"; exit 1; }

printf '%s\n' 'iSCSI dispatcher runtime-profile negatives (fail-fast naming, no doc read, no network/build on missing env, allow-live gate, prebuilt e2e fail-closed, prebuilt env passthrough) PASS'