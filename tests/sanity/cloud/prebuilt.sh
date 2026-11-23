# shellcheck shell=bash
# Verified-prebuilt binary channel shared by sanity/cloud/helpers.sh
# (sanity_bin, sanity_proxy_bin) and scenario/lib.sh (csi_race_prepare).
#
# The csi-sanity binary is a SHARED artifact: one binary for every driver,
# built from the NGX csi-test fork at a pinned commit (CSI_TEST_FORK_SHA).
# The driver-local cmd/csi-sanity source copy is gone; nothing may silently
# fall back to a stale local build. csi_sanity_build_shared() produces the
# binary from a SHA-keyed clean clone of the fork; the prebuilt channel
# (prebuilt.py) verifies an already-produced copy.
#
# Env contract (shared with the run-all dispatcher):
#   CSI_PREBUILT_DIR       directory with csi-sanity/sanity-proxy/csi-race/e2e.test
#   CSI_PREBUILT_MANIFEST  JSON provenance manifest (schema: prebuilt.py)
#   CSI_TEST_FORK_URL      git URL of the shared csi-test fork (test-overridable)
#   CSI_TEST_FORK_SHA      pinned full commit the shared csi-sanity must come from
# With CSI_PREBUILT_DIR unset, callers build the shared binary via
# csi_sanity_build_shared (never a driver-local copy). With it set,
# verification is fail-closed (prebuilt.py): manifest parse, binary presence,
# sha256, host goos/goarch, source_commit for csi-sanity, CURRENT-source
# fingerprint and a smoke probe must ALL pass; a rejection never falls back to
# an unverified binary or to a build. Scratch stays run-unique under the
# caller's CSI_TEST_CACHE ($HOME/.cache/ngxstorage discipline; never /tmp).

CSI_TEST_FORK_URL="${CSI_TEST_FORK_URL:-https://github.com/NGXStorage/csi-test.git}"
CSI_TEST_FORK_SHA="${CSI_TEST_FORK_SHA:-b80ebaa51854c186aac137a9434fd4a05810d6b5}"

csi_prebuilt_enabled() { [ -n "${CSI_PREBUILT_DIR:-}" ]; }

# Build the shared csi-sanity binary into $1 from a SHA-keyed clean clone of
# the fork. Fail-closed: the clone is (re)used only while its HEAD is exactly
# CSI_TEST_FORK_SHA; a stale or wrong-checkout cache is moved aside (never
# rm) and re-cloned. The build uses the fork's own vendor tree and stamps the
# binary's --version with the pinned SHA so prebuilt.py can verify provenance.
# Returns nonzero (and prints the reason) on any failure; callers `fail`.
csi_sanity_build_shared() { # $1=destination binary
    local dest="$1"
    # Resolve the destination to an absolute path and create its directory
    # BEFORE the build subshell cd's into the clone: a relative dest would
    # otherwise land inside the clone cache, not the caller's tree (e.g. the
    # CI job's build/shared-sanity/csi-sanity).
    case "$dest" in
        /*) : ;;
        *) dest="$(pwd)/$dest" ;;
    esac
    mkdir -p "$(dirname "$dest")" 2>/dev/null \
        || { echo "shared csi-sanity: cannot create destination directory for $dest" >&2; return 2; }
    # Cache root is HOME-stable by default; override (CSI_SANITY_CACHE_ROOT)
    # lets callers keep the SHA-keyed clone across privilege changes.
    local cache_root="${CSI_SANITY_CACHE_ROOT:-${HOME}/.cache/ngxstorage}"
    case "$cache_root" in
        /tmp|/tmp/*|/var/tmp|/var/tmp/*) echo "shared csi-sanity: refusing cache under /tmp" >&2; return 2 ;;
    esac
    mkdir -p "$cache_root/csi-test-fork-clone" || { echo "shared csi-sanity: cannot create $cache_root/csi-test-fork-clone" >&2; return 2; }
    local clone_dir="$cache_root/csi-test-fork-clone/$CSI_TEST_FORK_SHA"
    local head=""
    if [ -d "$clone_dir/.git" ]; then
        head="$(git -C "$clone_dir" rev-parse HEAD 2>/dev/null || true)"
        if [ "$head" != "$CSI_TEST_FORK_SHA" ]; then
            local stale="$cache_root/csi-test-fork-clone.stale-$$-$(date +%s)"
            mv "$clone_dir" "$stale" || { echo "shared csi-sanity: cannot set aside stale clone" >&2; return 2; }
            echo "shared csi-sanity: moved stale clone ($head) aside to $stale" >&2
            head=""
        fi
    fi
    if [ -z "$head" ]; then
        local fresh="$cache_root/csi-test-fork-clone.fresh-$$-$(date +%s)"
        git clone --quiet "$CSI_TEST_FORK_URL" "$fresh" \
            || { mv "$fresh" "$cache_root/csi-test-fork-clone.failed-$$-$(date +%s)" 2>/dev/null || true; echo "shared csi-sanity: clone of $CSI_TEST_FORK_URL failed" >&2; return 2; }
        git -C "$fresh" checkout --quiet "$CSI_TEST_FORK_SHA" \
            || { mv "$fresh" "$cache_root/csi-test-fork-clone.failed-$$-$(date +%s)" 2>/dev/null || true; echo "shared csi-sanity: checkout of $CSI_TEST_FORK_SHA failed" >&2; return 2; }
        mv "$fresh" "$clone_dir" || { echo "shared csi-sanity: cannot place clone at $clone_dir" >&2; return 2; }
        head="$(git -C "$clone_dir" rev-parse HEAD 2>/dev/null || true)"
        if [ "$head" != "$CSI_TEST_FORK_SHA" ]; then
            echo "shared csi-sanity: clone is not at pinned SHA ($head != $CSI_TEST_FORK_SHA)" >&2; return 2
        fi
    fi
    # Verify the clone's working tree is CLEAN, not just its HEAD: HEAD alone
    # cannot detect a source file edited after checkout, which would otherwise
    # be built and stamped with the original SHA. A dirty tree is a provenance
    # failure and is refused (the caller rebuilds from a fresh clone).
    if [ -n "$(git -C "$clone_dir" status --porcelain 2>/dev/null)" ]; then
        local dirty="$cache_root/csi-test-fork-clone.dirty-$$-$(date +%s)"
        mv "$clone_dir" "$dirty" 2>/dev/null || true
        echo "shared csi-sanity: clone working tree is not clean (modified after checkout); moved aside to $dirty and refusing to build" >&2
        return 2
    fi
    # -X addresses the MAIN package variable by its compile-time package name
    # ("main.VERSION"); the module import path does not match under this
    # toolchain. This stamps --version with the exact fork commit the binary
    # was built from, which prebuilt.py then verifies.
    ( cd "$clone_dir" && go build -mod=vendor -o "$dest" \
        -ldflags "-w -X main.VERSION=$CSI_TEST_FORK_SHA" \
 ./cmd/csi-sanity ) || { echo "shared csi-sanity: go build from fork clone failed" >&2; return 2; }
}

# Echo "--shared-array" when SANITY_SHARED_ARRAY is set, nothing otherwise
# (the fork binary's default is dedicated-array mode). Every csi-sanity
# invocation site appends this to its argument list, so the flag reaches the
# binary only when explicitly requested.
csi_sanity_shared_array_args() {
    if [ -n "${SANITY_SHARED_ARRAY:-}" ]; then printf '%s\n' --shared-array; fi
}

# Resolve the shared csi-sanity binary into $1 and echo its path. Prefers a
# verified prebuilt when CSI_PREBUILT_DIR is set; otherwise builds from the
# pinned fork. Never yields a stale driver-local copy. Nonzero on any failure.
csi_sanity_resolve() { # $1=destination binary
    local dest="$1"
    mkdir -p "$(dirname "$dest")" 2>/dev/null || return 1
    if csi_prebuilt_enabled; then
        csi_prebuilt_stage csi-sanity "$dest" || return 1
    else
        csi_sanity_build_shared "$dest" || return 1
    fi
    printf '%s\n' "$dest"
}

# Echoes the verified binary path inside CSI_PREBUILT_DIR; nonzero on any
# failed check. The driver root is derived from this helper's own location
# (tests/sanity/cloud -> driver root), so the fingerprint always describes
# the source tree that carries this harness.
csi_prebuilt_verify() { # $1=binary name
    local helper_dir driver_root
    helper_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || return 1
    driver_root="$(cd "$helper_dir/../../.." && pwd)" || return 1
    python3 "$helper_dir/prebuilt.py" verify "$driver_root" "$1"
}

# Verify, then stage through a run-unique temporary next to the destination
# (same filesystem, atomic mv). Nothing unverified ever lands at $2.
csi_prebuilt_stage() { # $1=binary name  $2=destination
    local name="$1" dest="$2" src staged
    src="$(csi_prebuilt_verify "$name")" || return 1
    mkdir -p "$(dirname "$dest")" || return 1
    staged="$(mktemp "${dest}.XXXXXXXX")" || return 1
    cat "$src" > "$staged" && chmod 0700 "$staged" && mv -f "$staged" "$dest"
}