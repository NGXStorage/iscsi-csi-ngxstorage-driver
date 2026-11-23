#!/usr/bin/env bash
# Offline contract for the verified-prebuilt binary channel (prebuilt.py /
# prebuilt.sh, sanity_bin/sanity_proxy_bin in cloud/helpers.sh and
# csi_race_prepare in scenario/lib.sh) AND the shared csi-sanity builder
# (csi_sanity_build_shared: clean clone of the pinned fork -> verify HEAD ->
# go build -mod=vendor, stamped with the fork commit). Self-contained
# synthetic fixtures: a driver repo, a local-replace dep, and a "fork" repo
# holding the shared csi-sanity source, plus stub binaries and a stubbed go
# toolchain. No real go build, no cluster, no storage, no network.
# Scratch is run-unique under $HOME/.cache/ngxstorage and left in place
# for inspection (no-rm policy).
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"          # tests/sanity
driver_root="$(cd "$root/../.." && pwd)"
cache="$HOME/.cache/ngxstorage"
case "$cache" in /tmp|/tmp/*|/var/tmp|/var/tmp/*) echo "refusing scratch under /tmp or /var/tmp" >&2; exit 2 ;; esac
mkdir -p "$cache"
scratch="$(mktemp -d "$cache/csi-prebuilt-contract.XXXXXX")"

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=contract GIT_AUTHOR_EMAIL=contract@example.com
export GIT_COMMITTER_NAME=contract GIT_COMMITTER_EMAIL=contract@example.com
export CSI_PREBUILT_PROBE_TIMEOUT=15

# --- fixture source tree: driver repo + one local replace dependency ------
mkdir -p "$scratch/driver/cmd/csi-sanity" \
         "$scratch/driver/tests/sanity/cloud" \
         "$scratch/driver/tests/scenario" \
         "$scratch/driver/tests/tools/sanity-proxy" \
         "$scratch/driver/tests/tools/csi-race" \
         "$scratch/dep" "$scratch/bin" "$scratch/prebuilt"
cp "$root/cloud/helpers.sh" "$root/cloud/prebuilt.sh" "$root/cloud/prebuilt.py" \
   "$scratch/driver/tests/sanity/cloud/"
cp "$driver_root/tests/scenario/lib.sh" "$scratch/driver/tests/scenario/lib.sh"
cat > "$scratch/driver/go.mod" <<'EOF'
module fixture.example/driver

go 1.27.0

require example.com/dep v0.0.0

replace example.com/dep => ../dep
EOF
printf 'package main\n' > "$scratch/driver/cmd/csi-sanity/main.go"
printf 'fixture driver\n' > "$scratch/driver/README.md"
printf 'module example.com/dep\n\ngo 1.27.0\n' > "$scratch/dep/go.mod"
printf 'package dep\n' > "$scratch/dep/dep.go"
git -C "$scratch/driver" init -q
git -C "$scratch/dep" init -q
git -C "$scratch/driver" add -A
git -C "$scratch/dep" add -A
git -C "$scratch/driver" commit -qm fixture
git -C "$scratch/dep" commit -qm fixture

# --- fixture "fork": the shared csi-sanity source at a known commit --------
# build-shared-sanity clones CSI_TEST_FORK_URL and checks out CSI_TEST_FORK_SHA;
# here both point at this local git repo so the clone works offline.
mkdir -p "$scratch/fork/cmd/csi-sanity" "$scratch/fork/vendor"
printf 'module github.com/kubernetes-csi/csi-test/v5\n\ngo 1.27.0\n' > "$scratch/fork/go.mod"
printf 'package main\n' > "$scratch/fork/cmd/csi-sanity/main.go"
printf '# github.com/kubernetes-csi/csi-test/v5 v0.0.0\n' > "$scratch/fork/vendor/modules.txt"
git -C "$scratch/fork" init -q
git -C "$scratch/fork" add -A
git -C "$scratch/fork" commit -qm "shared csi-sanity"
FIXTURE_SHA="$(git -C "$scratch/fork" rev-parse HEAD)"
BOGUS_SHA="0000000000000000000000000000000000000000"
export CSI_TEST_FORK_URL="$scratch/fork" CSI_TEST_FORK_SHA="$FIXTURE_SHA"
# Keep the SHA-keyed clone cache inside the run-unique scratch (isolated, no-rm).
export CSI_SANITY_CACHE_ROOT="$scratch/forkcache"

PB="$scratch/driver/tests/sanity/cloud/prebuilt.py"
fp() { python3 "$PB" fingerprint "$scratch/driver"; }

# --- fingerprint algorithm checks -----------------------------------------
fp0="$(fp)"
[ "$fp0" = "$(fp)" ] || { echo "fingerprint not deterministic" >&2; exit 1; }

printf '// edit\n' >> "$scratch/driver/cmd/csi-sanity/main.go"
[ "$(fp)" != "$fp0" ] || { echo "tracked edit not detected" >&2; exit 1; }
git -C "$scratch/driver" checkout -q -- cmd/csi-sanity/main.go
[ "$(fp)" = "$fp0" ] || { echo "restore did not restore fingerprint" >&2; exit 1; }

printf 'package extra\n' > "$scratch/driver/extra.go"
[ "$(fp)" != "$fp0" ] || { echo "untracked source addition not detected" >&2; exit 1; }
git -C "$scratch/driver" add extra.go
git -C "$scratch/driver" commit -qm extra

# Generated logs/artifacts are excluded even when untracked or force-added.
fp1="$(fp)"
mkdir -p "$scratch/driver/tests/logs" "$scratch/driver/build/artifacts"
printf 'noise\n' > "$scratch/driver/tests/logs/run.log"
printf '{}\n' > "$scratch/driver/build/artifacts/image.json"
[ "$(fp)" = "$fp1" ] || { echo "untracked logs/artifacts changed fingerprint" >&2; exit 1; }
printf 'tracked noise\n' > "$scratch/driver/tests/logs/tracked.log"
git -C "$scratch/driver" add -f tests/logs/tracked.log
[ "$(fp)" = "$fp1" ] || { echo "tracked logs changed fingerprint" >&2; exit 1; }
git -C "$scratch/driver" commit -qm logs

# Local replace dependency content is covered.
fp2="$(fp)"
printf '// edit\n' >> "$scratch/dep/dep.go"
[ "$(fp)" != "$fp2" ] || { echo "local replace dep edit not detected" >&2; exit 1; }
git -C "$scratch/dep" checkout -q -- dep.go
[ "$(fp)" = "$fp2" ] || { echo "dep restore did not restore fingerprint" >&2; exit 1; }

# Tracked deletion is covered.
mv "$scratch/driver/README.md" "$scratch/README.aside"
[ "$(fp)" != "$fp2" ] || { echo "tracked deletion not detected" >&2; exit 1; }
mv "$scratch/README.aside" "$scratch/driver/README.md"
[ "$(fp)" = "$fp2" ] || { echo "deletion restore did not restore fingerprint" >&2; exit 1; }

# --- shared csi-sanity builder (point 1 + 4): build-from-clean-clone --------
export PATH="$scratch/bin:$PATH"
export GO_CALL_LOG="$scratch/go-calls"
# go stub: GO_STUB_MODE=build emulates `go build ... -o <out> -ldflags "-w -X
# main.VERSION=<sha>"` by writing a binary that --version-prints the stamped
# sha; GO_STUB_MODE=fail exits 1 (used for fail-closed assertions). Always
# records its argv for command-shape assertions.
cat > "$scratch/bin/go" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$GO_CALL_LOG"
[ "${GO_STUB_MODE:-build}" = fail ] && exit 1
out=""; ver=""; prev=""
for a in "$@"; do
  case "$prev" in
    -o) out="$a" ;;
    -ldflags) case "$a" in *-X\ main.VERSION=*) ver="${a#*-X main.VERSION=}"; ver="${ver%% *}";; esac ;;
  esac
  prev="$a"
done
if [ -n "$out" ]; then
  printf '#!/bin/sh\nif [ "${1:-}" = "--version" ]; then echo "Version = %s"; exit 0; fi\nexit 0\n' "${ver:-unknown}" > "$out"
  chmod 0700 "$out"
fi
exit 0
EOF
chmod 700 "$scratch/bin/go"

# The builder/arg functions live in the harness (prebuilt.sh); source the
# fixture copy so they are defined before the builder tests below.
# shellcheck disable=SC1091
source "$scratch/driver/tests/sanity/cloud/prebuilt.sh"

# Happy path: builds into dest, --version carries the pinned fork commit.
: > "$GO_CALL_LOG"
GO_STUB_MODE=build csi_sanity_build_shared "$scratch/bin/shared-csi-sanity" \
    || { echo "csi_sanity_build_shared happy path failed" >&2; exit 1; }
[ "$( "$scratch/bin/shared-csi-sanity" --version )" = "Version = $FIXTURE_SHA" ] \
    || { echo "shared binary --version does not carry the pinned commit" >&2; exit 1; }
grep -q "build -mod=vendor .*-X main.VERSION=$FIXTURE_SHA.* ./cmd/csi-sanity" "$GO_CALL_LOG" \
    || { echo "go build not invoked with -mod=vendor + stamped commit" >&2; cat "$GO_CALL_LOG" >&2; exit 1; }

# Wrong pinned SHA: checkout cannot land on it -> fail-closed, no binary.
: > "$GO_CALL_LOG"
if ( CSI_TEST_FORK_SHA="$BOGUS_SHA" GO_STUB_MODE=build csi_sanity_build_shared "$scratch/bin/should-not-exist" ) 2> "$scratch/wrongsha.err"; then
    echo "wrong pinned SHA accepted" >&2; exit 1
fi
[ ! -e "$scratch/bin/should-not-exist" ]
grep -qE "checkout of $BOGUS_SHA failed|not at pinned SHA" "$scratch/wrongsha.err" \
    || { echo "wrong-SHA rejection did not name the cause" >&2; cat "$scratch/wrongsha.err" >&2; exit 1; }

# Relative dest resolves against the CALLER's cwd, not the clone cache.
# (Fix 1: the build subshell cd's into the clone, so a relative dest must be
# absolutized before that or it lands in the clone, not build/shared-sanity/.)
rel_dir="$scratch/relout"; mkdir -p "$rel_dir"
: > "$GO_CALL_LOG"
( cd "$rel_dir" && GO_STUB_MODE=build csi_sanity_build_shared "sub/csi-sanity" ) \
    || { echo "relative-path shared build failed" >&2; exit 1; }
[ -x "$rel_dir/sub/csi-sanity" ] \
    || { echo "relative dest did not land in the caller's directory" >&2; exit 1; }
[ ! -e "$scratch/forkcache/csi-test-fork-clone/$FIXTURE_SHA/sub/csi-sanity" ] \
    || { echo "relative dest leaked into the clone cache" >&2; exit 1; }
[ "$( "$rel_dir/sub/csi-sanity" --version )" = "Version = $FIXTURE_SHA" ]

# Clone at the right HEAD but with source modified after checkout is refused.
# (Fix 2: HEAD alone would stamp the modified build with the original SHA.)
dirty_clone="$scratch/forkcache/csi-test-fork-clone/$FIXTURE_SHA"
printf '// tampered\n' >> "$dirty_clone/cmd/csi-sanity/main.go"
: > "$GO_CALL_LOG"
if ( GO_STUB_MODE=build csi_sanity_build_shared "$scratch/bin/dirty-should-not-exist" ) 2> "$scratch/dirty.err"; then
    echo "modified-source clone accepted" >&2; exit 1
fi
[ ! -e "$scratch/bin/dirty-should-not-exist" ]
grep -q "not clean" "$scratch/dirty.err" \
    || { echo "dirty-tree rejection did not name the cause" >&2; cat "$scratch/dirty.err" >&2; exit 1; }

# --- stub binaries + manifest ----------------------------------------------
make_stub() { # $1=name
    case "$1" in
        csi-sanity)   printf '#!/bin/sh\nif [ "${1:-}" = "--version" ]; then echo "Version = %s"; exit 0; fi\nexit 0\n' "$CSI_TEST_FORK_SHA" ;;
        sanity-proxy) printf '#!/bin/sh\nexit 0\n' ;;
        csi-race)     printf '#!/bin/sh\necho "usage: csi-race <create-volume|delete-volume>" >&2\nexit 2\n' ;;
        e2e.test)     printf '#!/bin/sh\nexit 0\n' ;;
    esac > "$scratch/prebuilt/$1"
    chmod 0700 "$scratch/prebuilt/$1"
}
for name in csi-sanity sanity-proxy csi-race e2e.test; do make_stub "$name"; done

write_manifest() { # $1 = source fingerprint to record
    python3 - "$scratch" "$1" "$CSI_TEST_FORK_SHA" <<'PY'
import datetime, hashlib, json, platform, sys
scratch, fp, commit = sys.argv[1], sys.argv[2], sys.argv[3]
goos = platform.system().lower()
goarch = {"x86_64": "amd64", "amd64": "amd64",
          "aarch64": "arm64", "arm64": "arm64"}[platform.machine().lower()]
manifest = {}
for name in ("csi-sanity", "sanity-proxy", "csi-race", "e2e.test"):
    with open(f"{scratch}/prebuilt/{name}", "rb") as fh:
        data = fh.read()
    entry = {
        "sha256": hashlib.sha256(data).hexdigest(),
        "goos": goos, "goarch": goarch,
        "source_fingerprint": fp,
        "go_version": "go1.27.4 (fixture)",
        "build_flags": "-trimpath (fixture)",
        "built_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    }
    if name == "csi-sanity":
        entry["source_commit"] = commit
    manifest[name] = entry
with open(f"{scratch}/manifest.json", "w") as fh:
    json.dump(manifest, fh, indent=2)
PY
}
tamper() { # $1 = python expression mutating m
    python3 - "$scratch/manifest.json" "$1" <<'PY'
import json, sys
path, expr = sys.argv[1], sys.argv[2]
with open(path) as fh:
    m = json.load(fh)
exec(expr)
with open(path, "w") as fh:
    json.dump(m, fh, indent=2)
PY
}
fp_final="$(fp)"
write_manifest "$fp_final"
export CSI_PREBUILT_DIR="$scratch/prebuilt" CSI_PREBUILT_MANIFEST="$scratch/manifest.json"

verify() { python3 "$PB" verify "$scratch/driver" "$1" > "$scratch/v.out" 2> "$scratch/v.err"; }
expect_fail() { # $1=name $2=expected error detail
    if verify "$1"; then echo "expected rejection: $1 ($2)" >&2; exit 1; fi
    grep -q "$2" "$scratch/v.err" || { cat "$scratch/v.err" >&2; echo "missing error detail: $2" >&2; exit 1; }
}

# --- verification matrix ----------------------------------------------------
for name in csi-sanity sanity-proxy csi-race e2e.test; do
    verify "$name" || { cat "$scratch/v.err" >&2; echo "valid prebuilt rejected: $name" >&2; exit 1; }
    [ "$(cat "$scratch/v.out")" = "$scratch/prebuilt/$name" ]
done

# csi-sanity source_commit: a binary that does not identify the pinned commit
# is rejected (wrong pin / stale prebuilt from another fork commit).
tamper 'm["csi-sanity"]["source_commit"] = "f" * 40'
expect_fail csi-sanity "does not identify expected fork commit"
write_manifest "$fp_final"
tamper 'm["csi-sanity"]["source_commit"] = "not-a-sha"'
expect_fail csi-sanity "not a full 40-hex fork commit"
write_manifest "$fp_final"

tamper 'm["csi-sanity"]["sha256"] = "0" * 64'
expect_fail csi-sanity "sha256 mismatch"
write_manifest "$fp_final"

other_arch=arm64; [ "$(uname -m)" = aarch64 ] && other_arch=amd64
tamper "m['csi-sanity']['goarch'] = '$other_arch'"
expect_fail csi-sanity "execution host is"
write_manifest "$fp_final"
tamper 'm["csi-sanity"]["goos"] = "windows"'
expect_fail csi-sanity "execution host is"
write_manifest "$fp_final"

printf '// stale\n' >> "$scratch/driver/cmd/csi-sanity/main.go"
expect_fail csi-sanity "source fingerprint mismatch"
git -C "$scratch/driver" checkout -q -- cmd/csi-sanity/main.go

# Wrong source tree (the dep repo is not the driver tree).
if python3 "$PB" verify "$scratch/dep" csi-sanity > "$scratch/v.out" 2> "$scratch/v.err"; then
    echo "wrong-source tree accepted" >&2; exit 1
fi
grep -q "source fingerprint mismatch" "$scratch/v.err"

CSI_PREBUILT_MANIFEST="$scratch/absent.json" expect_fail csi-sanity "manifest missing"
( unset CSI_PREBUILT_MANIFEST; expect_fail csi-sanity "CSI_PREBUILT_MANIFEST is required" )
( unset CSI_PREBUILT_DIR; expect_fail csi-sanity "CSI_PREBUILT_DIR is not set" )

cp "$scratch/manifest.json" "$scratch/manifest.good"
printf '{not json\n' > "$scratch/manifest.json"
expect_fail csi-sanity "invalid JSON"
cp "$scratch/manifest.good" "$scratch/manifest.json"

tamper 'del m["csi-race"]'
expect_fail csi-race "no entry"
write_manifest "$fp_final"
tamper 'del m["csi-sanity"]["built_at"]'
expect_fail csi-sanity "built_at"
write_manifest "$fp_final"
tamper 'del m["csi-sanity"]["source_commit"]'
expect_fail csi-sanity "source_commit missing"
write_manifest "$fp_final"
tamper 'm["csi-sanity"]["sha256"] = "xyz"'
expect_fail csi-sanity "64 lowercase hex"
write_manifest "$fp_final"

mv "$scratch/prebuilt/csi-sanity" "$scratch/csi-sanity.aside"
expect_fail csi-sanity "missing or not a regular file"
mv "$scratch/csi-sanity.aside" "$scratch/prebuilt/csi-sanity"

chmod 0600 "$scratch/prebuilt/csi-sanity"
expect_fail csi-sanity "not executable"
chmod 0700 "$scratch/prebuilt/csi-sanity"

# Hash-consistent but corrupt: the smoke probe must still reject it.
mv "$scratch/prebuilt/csi-sanity" "$scratch/csi-sanity.good"
printf 'this is not an executable\n' > "$scratch/prebuilt/csi-sanity"
chmod 0700 "$scratch/prebuilt/csi-sanity"
write_manifest "$fp_final"
expect_fail csi-sanity "smoke probe cannot execute"
mv "$scratch/csi-sanity.good" "$scratch/prebuilt/csi-sanity"
write_manifest "$fp_final"

# Probe hang is bounded.
printf '#!/bin/sh\nsleep 5\n' > "$scratch/prebuilt/csi-sanity"
chmod 0700 "$scratch/prebuilt/csi-sanity"
write_manifest "$fp_final"
CSI_PREBUILT_PROBE_TIMEOUT=1 expect_fail csi-sanity "timed out"
make_stub csi-sanity
write_manifest "$fp_final"

expect_fail definitely-not-a-binary "unknown prebuilt binary"

# --- --shared-array plumbing (point 4): runner arg translation --------------
[ -z "$(unset SANITY_SHARED_ARRAY; csi_sanity_shared_array_args)" ] \
    || { echo "shared-array emitted without SANITY_SHARED_ARRAY" >&2; exit 1; }
[ "$(SANITY_SHARED_ARRAY=1 csi_sanity_shared_array_args)" = "--shared-array" ] \
    || { echo "SANITY_SHARED_ARRAY=1 did not translate to --shared-array" >&2; exit 1; }

# --- harness integration: helpers.sh + lib.sh csi_race_prepare -------------
export CSI_DRIVER_TESTS="$scratch/driver/tests"
PROTOCOL="$(basename "$driver_root" | tr '[:upper:]' '[:lower:]')"
export PROTOCOL
cat > "$scratch/bin/kube" <<'EOF'
#!/bin/sh
case "$1" in
    get) printf 'fixture-controller-pod\n' ;;
    exec) cat > "$CSI_RACE_CAPTURE" ;;
    *) echo "kube stub: unexpected $*" >&2; exit 1 ;;
esac
EOF
chmod 700 "$scratch/bin/kube"
export KUBE_BIN="$scratch/bin/kube" CSI_RACE_CAPTURE="$scratch/race-capture"
fail() { echo "[ERROR] $*" >&2; exit 1; }
# shellcheck disable=SC1091
source "$scratch/driver/tests/sanity/cloud/helpers.sh"
# shellcheck disable=SC1091
source "$scratch/driver/tests/scenario/lib.sh"

# Valid prebuilt: installed without any go invocation.
: > "$GO_CALL_LOG"
cache1="$scratch/cache1"; mkdir -p "$cache1"
out="$(CSI_TEST_CACHE="$cache1" sanity_bin)"
[ "$out" = "$cache1/csi-sanity" ]
cmp "$cache1/csi-sanity" "$scratch/prebuilt/csi-sanity"
out="$(CSI_TEST_CACHE="$cache1" sanity_proxy_bin)"
[ "$out" = "$cache1/sanity-proxy" ]
cmp "$cache1/sanity-proxy" "$scratch/prebuilt/sanity-proxy"
[ ! -s "$GO_CALL_LOG" ] || { echo "go invoked in prebuilt mode" >&2; exit 1; }

# csi_race_prepare prebuilt: staged + injected without any go invocation.
cache_race="$scratch/cache-race"; mkdir -p "$cache_race"
(
    CSI_TEST_CACHE="$cache_race"
    csi_race_prepare
    [ "$CSI_RACE_CONTROLLER_POD" = fixture-controller-pod ]
)
cmp "$CSI_RACE_CAPTURE" "$scratch/prebuilt/csi-race"
cmp "$cache_race/csi-race" "$scratch/prebuilt/csi-race"
[ ! -s "$GO_CALL_LOG" ] || { echo "go invoked in prebuilt mode (race)" >&2; exit 1; }

# Fail-closed: a rejected prebuilt never falls back to a build or a cache.
tamper 'm["csi-sanity"]["sha256"] = "0" * 64; m["sanity-proxy"]["sha256"] = "0" * 64; m["csi-race"]["sha256"] = "0" * 64'
cache2="$scratch/cache2"; mkdir -p "$cache2"
if ( CSI_TEST_CACHE="$cache2" sanity_bin ) > "$scratch/i.out" 2> "$scratch/i.err"; then
    echo "tampered prebuilt accepted by sanity_bin" >&2; exit 1
fi
grep -q "sha256 mismatch" "$scratch/i.err"
[ ! -e "$cache2/csi-sanity" ]
if ( CSI_TEST_CACHE="$cache1" sanity_proxy_bin ) > "$scratch/i.out" 2> "$scratch/i.err"; then
    echo "tampered prebuilt accepted by sanity_proxy_bin (cached copy bypass)" >&2; exit 1
fi
if ( CSI_TEST_CACHE="$cache_race"; csi_race_prepare ) > "$scratch/i.out" 2> "$scratch/i.err"; then
    echo "tampered prebuilt accepted by csi_race_prepare" >&2; exit 1
fi
[ ! -s "$GO_CALL_LOG" ] || { echo "fallback build after prebuilt rejection" >&2; exit 1; }
write_manifest "$fp_final"

# Env set but manifest unset: still fail-closed, no build fallback.
cache3="$scratch/cache3"; mkdir -p "$cache3"
if ( unset CSI_PREBUILT_MANIFEST; CSI_TEST_CACHE="$cache3" sanity_bin ) 2> "$scratch/i.err"; then
    echo "missing manifest accepted" >&2; exit 1
fi
grep -q "CSI_PREBUILT_MANIFEST is required" "$scratch/i.err"
[ ! -s "$GO_CALL_LOG" ]

# Env unset: the shared build-from-fork path is attempted; a failed build
# fail-closes (no fallback to a cache or an unverified binary).
: > "$GO_CALL_LOG"
cache4="$scratch/cache4"; mkdir -p "$cache4"
if ( unset CSI_PREBUILT_DIR CSI_PREBUILT_MANIFEST; GO_STUB_MODE=fail; export GO_STUB_MODE; CSI_TEST_CACHE="$cache4" sanity_bin ) 2> "$scratch/i.err"; then
    echo "failed shared build unexpectedly succeeded" >&2; exit 1
fi
grep -q "build -mod=vendor .* ./cmd/csi-sanity" "$GO_CALL_LOG"
grep -q "shared csi-sanity build from fork" "$scratch/i.err"
[ ! -e "$cache4/csi-sanity" ]
# proxy/race still build driver-local in env-unset mode (stub records it).
if ( unset CSI_PREBUILT_DIR CSI_PREBUILT_MANIFEST; GO_STUB_MODE=fail; export GO_STUB_MODE; CSI_TEST_CACHE="$cache4" sanity_proxy_bin ) 2> "$scratch/i.err"; then
    echo "stubbed go build unexpectedly succeeded (proxy)" >&2; exit 1
fi
grep -q "build -o .* \." "$GO_CALL_LOG"
if ( unset CSI_PREBUILT_DIR CSI_PREBUILT_MANIFEST; GO_STUB_MODE=fail; export GO_STUB_MODE; CSI_TEST_CACHE="$cache4" csi_race_prepare ) 2> "$scratch/i.err"; then
    echo "stubbed go build unexpectedly succeeded (race)" >&2; exit 1
fi
grep -q "csi-race build failed" "$scratch/i.err"

printf '%s\n' 'Prebuilt + shared-builder contract PASS: fingerprint algorithm, build-from-clean-clone (pinned SHA), source_commit fail-closed, --shared-array plumbing, harness integration (scratch: '"$scratch"')'
