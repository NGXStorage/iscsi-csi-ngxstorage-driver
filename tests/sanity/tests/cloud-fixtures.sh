#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cache="$HOME/.cache/ngxstorage"
mkdir -p "$cache"
scratch="$(mktemp -d "$cache/csi-cloud-fixture.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
export WORK="$scratch/work" FAKE_MOUNTS="$scratch/mounts" FINDMNT_RC=0
mkdir -p "$scratch/bin" "$WORK"
: > "$FAKE_MOUNTS"
cat > "$scratch/bin/chroot" <<'EOF'
#!/bin/sh
cat "$FAKE_MOUNTS"
exit "$FINDMNT_RC"
EOF
chmod 700 "$scratch/bin/chroot"
export PATH="$scratch/bin:$PATH"
source "$root/cloud/helpers.sh"
fail() { echo "$*" >&2; exit 1; }
sanity_ns() { echo fixture; }
sanity_container() { echo fixture; }
sanity_workdir() { echo "$WORK"; }
oc() {
    while [ "$1" != -- ]; do shift; done
    shift
    command "$@"
}
printf 'compression: "on"\n' > "$scratch/input.yaml"
sanity_inject pod "$scratch/input.yaml" "$WORK/params.yaml" 0600
cmp "$scratch/input.yaml" "$WORK/params.yaml"
[ "$(stat -c %a "$WORK/params.yaml")" = 600 ]
if (sanity_inject pod "$scratch/missing.yaml" "$WORK/missing.yaml" 0600) 2>/dev/null; then exit 1; fi
[ ! -e "$WORK/missing.yaml" ]
printf '%s\n' "$WORK/mnt/volume" > "$FAKE_MOUNTS"
if sanity_cleanup_workdir pod 2>/dev/null; then exit 1; fi
[ -f "$WORK/params.yaml" ]
: > "$FAKE_MOUNTS"
export FINDMNT_RC=5
if sanity_cleanup_workdir pod 2>/dev/null; then exit 1; fi
[ -f "$WORK/params.yaml" ]
export FINDMNT_RC=0
printf '%s\n' "$WORK-sibling/mnt" > "$FAKE_MOUNTS"
sanity_cleanup_workdir pod
[ ! -e "$WORK" ]
printf '%s\n' 'Cloud fixture upload, missing-input and mounted-cleanup guards PASS'
