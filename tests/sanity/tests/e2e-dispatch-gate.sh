#!/usr/bin/env bash
# Offline dispatcher contract for the E2E prebuilt provenance gate.
#
# Runs the REAL tests/run-all-tests.sh e2e --allow-live inside a synthetic
# git fixture tree (driver repo + one local replace dependency) with stub
# go/kube/network tools and a stub e2e.test that records every invocation.
# No real build, cluster, storage or network access.
#
# Contract (identical in NFS/FC/iSCSI):
#   * CSI_PREBUILT_DIR set => the ONE shared gate (tests/sanity/cloud/
#     prebuilt.py via prebuilt.sh csi_prebuilt_verify) must pass sha256, host
#     goos/goarch, CURRENT-source fingerprint equality and the smoke probe
#     before exec; the verified binary is what executes.
#   * stale driver source, stale replace-dep source, zero/wrong fingerprint,
#     wrong hash, wrong arch/os, failed probe, missing binary, missing
#     manifest => nonzero rc with ZERO test exec, ZERO go build and ZERO
#     kube/network dispatch (the stub call logs stay empty).
#   * valid current-source fixture => exactly ONE test exec of the verified
#     binary (plus the gate's single smoke probe).
#   * CSI_PREBUILT_DIR unset => the regular go test build path is preserved.
# Stale-source cases mutate real tracked files in the scratch fixture so the
# recomputed fingerprint genuinely differs; the verifier is never faked.
# Scratch is run-unique under $HOME/.cache/ngxstorage and left in place
# for inspection (no-rm policy).
set -euo pipefail
sanity_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
driver_root="$(cd "$sanity_root/../.." && pwd)"
driver_name="$(basename "$driver_root")"
cache="$HOME/.cache/ngxstorage"
case "$cache" in /tmp|/tmp/*|/var/tmp|/var/tmp/*) echo "refusing scratch under /tmp or /var/tmp" >&2; exit 2 ;; esac
mkdir -p "$cache"
scratch="$(mktemp -d "$cache/csi-e2e-dispatch-gate.XXXXXX")"

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=contract GIT_AUTHOR_EMAIL=contract@example.com
export GIT_COMMITTER_NAME=contract GIT_COMMITTER_EMAIL=contract@example.com
export CSI_PREBUILT_PROBE_TIMEOUT=15

dispatcher="$driver_root/tests/run-all-tests.sh"

# --- Wiring pins: ONE shared gate on the real exec path, no second inline
# --- verifier in the dispatcher.
grep -q 'source "$driver_root/tests/sanity/cloud/prebuilt.sh"' "$dispatcher"
grep -q 'csi_prebuilt_enabled' "$dispatcher"
grep -q 'csi_prebuilt_verify e2e.test' "$dispatcher"
if grep -q 'prebuilt_e2e_verify' "$dispatcher"; then
    echo "dispatcher still carries the old inline verifier" >&2; exit 1
fi
if grep -q 'hashlib' "$dispatcher"; then
    echo "dispatcher still carries an inline hash verifier" >&2; exit 1
fi

# --- Fixture source tree: driver git repo + one local replace dependency ---
fixture="$scratch/driver"
dep="$scratch/dep"
mkdir -p "$fixture/tests/sanity/cloud" "$fixture/tests/e2e" "$dep" \
         "$scratch/bin" "$scratch/prebuilt"
cp "$dispatcher" "$fixture/tests/run-all-tests.sh"
cp "$sanity_root/cloud/prebuilt.sh" "$sanity_root/cloud/prebuilt.py" \
   "$fixture/tests/sanity/cloud/"
cat > "$fixture/go.mod" <<'EOF'
module fixture.example/driver

go 1.27.0

require example.com/dep v0.0.0

replace example.com/dep => ../dep
EOF
printf 'package e2e // fixture source, tracked\n' > "$fixture/tests/e2e/fixture_test.go"
printf 'fixture driver\n' > "$fixture/README.md"
printf 'module example.com/dep\n\ngo 1.27.0\n' > "$dep/go.mod"
printf 'package dep\n' > "$dep/dep.go"
git -C "$fixture" init -q
git -C "$dep" init -q
git -C "$fixture" add -A
git -C "$dep" add -A
git -C "$fixture" commit -qm fixture
git -C "$dep" commit -qm fixture

PB="$fixture/tests/sanity/cloud/prebuilt.py"
fp() { python3 "$PB" fingerprint "$fixture"; }
fp0="$(fp)"
[ "$fp0" = "$(fp)" ] || { echo "fixture fingerprint not deterministic" >&2; exit 1; }

# --- Stub tools: record every invocation, then fail. python3/git stay real
# --- (the shared gate needs them); no build/kube/network tool may run on a
# --- rejected dispatch, and none is needed on the verified-prebuilt path.
tool_calls="$scratch/tool-calls"
e2e_calls="$scratch/e2e-calls"
marker="$scratch/e2e-exec-marker"
export TOOL_CALL_LOG="$tool_calls" E2E_CALL_LOG="$e2e_calls" E2E_EXEC_MARKER="$marker"
: > "$tool_calls"; : > "$e2e_calls"
for tool in go kubectl oc sudo curl wget nc ssh; do
    cat > "$scratch/bin/$tool" <<EOF
#!/bin/sh
printf '%s %s\n' '$tool' "\$*" >> '$tool_calls'
exit 97
EOF
    chmod 700 "$scratch/bin/$tool"
done

# Stub e2e.test: logs path+args of EVERY invocation (the gate's -h smoke
# probe included); only a real test exec writes the marker. $1 = probe rc.
make_e2e_stub() {
    cat > "$scratch/prebuilt/e2e.test" <<EOF
#!/bin/sh
printf '%s %s\n' "\$0" "\$*" >> '$e2e_calls'
if [ "\${1:-}" = "-h" ]; then exit $1; fi
printf 'E2E_RUN=%s ARGS=%s\n' "\${E2E_RUN:-unset}" "\$*" > '$marker'
exit 0
EOF
    chmod 700 "$scratch/prebuilt/e2e.test"
}
make_e2e_stub 0

# --- Manifest (shared schema: JSON object keyed by binary name) -------------
manifest="$scratch/manifest.json"
write_manifest() { # $1 = source fingerprint to record
    python3 - "$scratch" "$1" <<'PY'
import datetime, hashlib, json, platform, sys
scratch, fp = sys.argv[1], sys.argv[2]
goos = platform.system().lower()
goarch = {"x86_64": "amd64", "amd64": "amd64",
          "aarch64": "arm64", "arm64": "arm64"}[platform.machine().lower()]
with open(f"{scratch}/prebuilt/e2e.test", "rb") as fh:
    sha = hashlib.sha256(fh.read()).hexdigest()
manifest = {"e2e.test": {
    "sha256": sha, "goos": goos, "goarch": goarch,
    "source_fingerprint": fp,
    "go_version": "go1.27.4 (fixture)",
    "build_flags": "-trimpath (fixture)",
    "built_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
}}
with open(f"{scratch}/manifest.json", "w") as fh:
    json.dump(manifest, fh, indent=2)
PY
}
tamper() { # $1 = python expression mutating m
    python3 - "$manifest" "$1" <<'PY'
import json, sys
path, expr = sys.argv[1], sys.argv[2]
with open(path) as fh:
    m = json.load(fh)
exec(expr)
with open(path, "w") as fh:
    json.dump(m, fh, indent=2)
PY
}
write_manifest "$fp0"

# --- Dispatcher runs ---------------------------------------------------------
kubeconfig="$scratch/kubeconfig"; : > "$kubeconfig"
run_dispatcher() { # $@ = extra env assignments; prebuilt env cleared by default
    env -u CSI_PREBUILT_DIR -u CSI_PREBUILT_MANIFEST -u KUBECONFIG -u KUBE_CONTEXT \
        -u BENCH -u E2E_RUN -u E2E_TEST_BINARY \
        -u NGX_CONTROLLER -u NGX_API_KEY -u NGX_POOL_NAME \
        "$@" KUBECONFIG="$kubeconfig" PATH="$scratch/bin:$PATH" \
        bash "$fixture/tests/run-all-tests.sh" e2e --allow-live
}
pb_run() { run_dispatcher CSI_PREBUILT_DIR="$scratch/prebuilt" CSI_PREBUILT_MANIFEST="$manifest"; }
reset_logs() {
    : > "$tool_calls"; : > "$e2e_calls"
    [ ! -e "$marker" ] || mv "$marker" "$scratch/marker.prev"
}
expect_reject() { # $1=name $2=rc $3=msg $4...=needles; zero dispatch asserted
    local name="$1" rc="$2" msg="$3"; shift 3
    [ "$rc" -eq 2 ] || { echo "$name: expected rc=2, got rc=$rc"; echo "$msg"; exit 1; }
    [ ! -s "$tool_calls" ] || { echo "$name: forbidden tool invoked:"; cat "$tool_calls"; exit 1; }
    [ ! -s "$e2e_calls" ] || { echo "$name: e2e.test executed despite rejection:"; cat "$e2e_calls"; exit 1; }
    [ ! -e "$marker" ] || { echo "$name: exec marker written despite rejection"; exit 1; }
    local needle
    for needle in "$@"; do
        grep -Fq -- "$needle" <<< "$msg" || { echo "$name: '$needle' not reported"; echo "$msg"; exit 1; }
    done
}

# 1. Zero fingerprint (64 zeros), otherwise-valid hash/arch: the exact
# reviewer repro must now be rejected at the dispatcher entry.
reset_logs
tamper 'm["e2e.test"]["source_fingerprint"] = "0" * 64'
rc=0 msg=""
msg="$(pb_run 2>&1)" || rc=$?
expect_reject zero-fingerprint "$rc" "$msg" 'source fingerprint mismatch'
write_manifest "$fp0"

# 2. Stale DRIVER source: a real tracked file mutated after the manifest.
reset_logs
printf '// stale driver edit\n' >> "$fixture/tests/e2e/fixture_test.go"
[ "$(fp)" != "$fp0" ] || { echo "driver mutation did not change fingerprint" >&2; exit 1; }
rc=0 msg=""
msg="$(pb_run 2>&1)" || rc=$?
expect_reject stale-driver-source "$rc" "$msg" 'source fingerprint mismatch'
git -C "$fixture" checkout -q -- tests/e2e/fixture_test.go
[ "$(fp)" = "$fp0" ] || { echo "driver restore did not restore fingerprint" >&2; exit 1; }

# 3. Stale local replace dependency source (sibling repo).
reset_logs
printf '// stale dep edit\n' >> "$dep/dep.go"
[ "$(fp)" != "$fp0" ] || { echo "dep mutation did not change fingerprint" >&2; exit 1; }
rc=0 msg=""
msg="$(pb_run 2>&1)" || rc=$?
expect_reject stale-dep-source "$rc" "$msg" 'source fingerprint mismatch'
git -C "$dep" checkout -q -- dep.go
[ "$(fp)" = "$fp0" ] || { echo "dep restore did not restore fingerprint" >&2; exit 1; }

# 4. Wrong hash.
reset_logs
tamper 'm["e2e.test"]["sha256"] = "0" * 64'
rc=0 msg=""
msg="$(pb_run 2>&1)" || rc=$?
expect_reject wrong-hash "$rc" "$msg" 'sha256 mismatch'
write_manifest "$fp0"

# 5. Wrong architecture.
other_arch=arm64; [ "$(uname -m)" = aarch64 ] && other_arch=amd64
reset_logs
tamper "m['e2e.test']['goarch'] = '$other_arch'"
rc=0 msg=""
msg="$(pb_run 2>&1)" || rc=$?
expect_reject wrong-arch "$rc" "$msg" 'execution host is'
write_manifest "$fp0"

# 6. Wrong OS.
reset_logs
tamper 'm["e2e.test"]["goos"] = "windows"'
rc=0 msg=""
msg="$(pb_run 2>&1)" || rc=$?
expect_reject wrong-os "$rc" "$msg" 'execution host is'
write_manifest "$fp0"

# 7. Failed smoke probe: hash-consistent stub whose -h probe exits nonzero.
# The probe itself is the gate's one allowed invocation; no test exec may
# follow it.
reset_logs
make_e2e_stub 3
write_manifest "$fp0"
rc=0 msg=""
msg="$(pb_run 2>&1)" || rc=$?
[ "$rc" -eq 2 ] || { echo "failed-probe: expected rc=2, got rc=$rc"; echo "$msg"; exit 1; }
[ ! -s "$tool_calls" ] || { echo "failed-probe: forbidden tool invoked:"; cat "$tool_calls"; exit 1; }
[ ! -e "$marker" ] || { echo "failed-probe: exec marker written despite rejection"; exit 1; }
[ "$(wc -l < "$e2e_calls")" -eq 1 ] || { echo "failed-probe: expected only the smoke probe invocation"; cat "$e2e_calls"; exit 1; }
grep -Fq -- ' -h' "$e2e_calls" || { echo "failed-probe: probe invocation not recorded"; cat "$e2e_calls"; exit 1; }
grep -Fq -- '-test.timeout' "$e2e_calls" && { echo "failed-probe: test exec after failed probe"; cat "$e2e_calls"; exit 1; }
grep -Fq 'smoke probe exit 3' <<< "$msg" || { echo "failed-probe: probe failure not reported"; echo "$msg"; exit 1; }
make_e2e_stub 0
write_manifest "$fp0"

# 8. Missing binary in EXPLICIT prebuilt mode: hard failure, never a go
# fallback (the old dispatcher fell back to a real build here).
reset_logs
mv "$scratch/prebuilt/e2e.test" "$scratch/e2e.test.aside"
rc=0 msg=""
msg="$(pb_run 2>&1)" || rc=$?
expect_reject missing-binary "$rc" "$msg" 'missing or not a regular file'
mv "$scratch/e2e.test.aside" "$scratch/prebuilt/e2e.test"

# 9. Missing manifest file in explicit prebuilt mode: hard failure.
reset_logs
rc=0 msg=""
msg="$(run_dispatcher CSI_PREBUILT_DIR="$scratch/prebuilt" CSI_PREBUILT_MANIFEST="$scratch/absent.json" 2>&1)" || rc=$?
expect_reject missing-manifest "$rc" "$msg" 'manifest missing'

# 9b. Manifest env unset while the dir is set: hard failure.
reset_logs
rc=0 msg=""
msg="$(run_dispatcher CSI_PREBUILT_DIR="$scratch/prebuilt" 2>&1)" || rc=$?
expect_reject unset-manifest "$rc" "$msg" 'CSI_PREBUILT_MANIFEST is required'

# 10. Valid current-source fixture: exactly ONE test exec of the verified
# binary (plus the gate's single smoke probe); zero build/kube/network.
reset_logs
rc=0 msg=""
msg="$(pb_run 2>&1)" || rc=$?
[ "$rc" -eq 0 ] || { echo "valid-prebuilt: expected rc=0, got rc=$rc"; echo "$msg"; exit 1; }
[ ! -s "$tool_calls" ] || { echo "valid-prebuilt: build/kube/network tool invoked:"; cat "$tool_calls"; exit 1; }
[ "$(wc -l < "$e2e_calls")" -eq 2 ] || { echo "valid-prebuilt: expected probe + one exec"; cat "$e2e_calls"; exit 1; }
[ "$(grep -c -- ' -h$' "$e2e_calls")" -eq 1 ] || { echo "valid-prebuilt: expected exactly one smoke probe"; cat "$e2e_calls"; exit 1; }
pb_real="$(cd "$scratch/prebuilt" && pwd -P)"
grep -Fq "$pb_real/e2e.test -test.timeout=45m -test.count=1 -test.v" "$e2e_calls" \
    || { echo "valid-prebuilt: verified binary path/flags not exec'd"; cat "$e2e_calls"; exit 1; }
[ -f "$marker" ] || { echo "valid-prebuilt: exec marker missing"; echo "$msg"; exit 1; }
grep -Fq 'E2E_RUN=1' "$marker" || { echo "valid-prebuilt: E2E_RUN not passed"; cat "$marker"; exit 1; }

# 11. Prebuilt mode UNSET: the regular go test build path is preserved
# (the go stub records the attempt; no prebuilt binary is exec'd).
reset_logs
rc=0 msg=""
msg="$(run_dispatcher 2>&1)" || rc=$?
[ "$rc" -eq 97 ] || { echo "unset-mode: expected go stub rc=97, got rc=$rc"; echo "$msg"; exit 1; }
[ "$(wc -l < "$tool_calls")" -eq 1 ] || { echo "unset-mode: expected exactly one go call"; cat "$tool_calls"; exit 1; }
grep -Fq 'go test -timeout=45m -count=1 -v ./...' "$tool_calls" \
    || { echo "unset-mode: go test path changed"; cat "$tool_calls"; exit 1; }
[ ! -s "$e2e_calls" ] || { echo "unset-mode: prebuilt binary exec'd"; cat "$e2e_calls"; exit 1; }
[ ! -e "$marker" ] || { echo "unset-mode: exec marker written"; exit 1; }

printf '%s\n' "$driver_name E2E dispatch gate PASS: shared provenance gate on the real exec path, fail-closed rejection matrix with zero dispatch, single verified exec, unset-mode go path (scratch: $scratch)"