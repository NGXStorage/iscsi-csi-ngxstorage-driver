#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_root="${TMPDIR:-$HOME/.cache/ngxstorage}"
case "$tmp_root" in /tmp|/tmp/*|/var/tmp|/var/tmp/*) echo "refusing scratch under /tmp or /var/tmp" >&2; exit 2 ;; esac
mkdir -p "$tmp_root"
tmp="$(mktemp -d "$tmp_root/iscsi-race-cleanup-test.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

export CSI_TEST_ROOT="$root"
export PROTOCOL=iscsi
export OUT="$tmp/out"
mkdir -p "$OUT"
source "$root/lib.sh"

calls="$tmp/calls"
csi_race_exec() { printf '%s\n' "$*" >> "$calls"; }
csi_race_cleanup() { :; }
log() { :; }
CSI_RACE_CONTROLLER_POD=fixture-controller

cat > "$OUT/csi-race-create-snapshot.json" <<'JSON'
{"operation":"create-snapshot","results":[
 {"index":0,"code":"OK","snapshot_id":"snap-1"},
 {"index":1,"code":"OK","snapshot_id":"snap-1"},
 {"index":2,"code":"DeadlineExceeded"},
 {"index":3,"code":"OK","snapshot_id":"snap-1"}]}
JSON
cat > "$OUT/csi-race-snapshot-source.json" <<'JSON'
{"operation":"create-volume","results":[{"index":0,"code":"OK","volume_id":"lun-1"}]}
JSON

# Partial RPC failure must still clean every unique successful snapshot result,
# then the source LUN; marker and JSON duplicates must not issue duplicate deletes.
printf 'snap-1' > "$OUT/.csi-race-snapshot-id"
printf 'lun-1' > "$OUT/.csi-race-volume-id"
direct_controller_race_cleanup
expected="$tmp/expected"
printf '%s\n' \
  'delete-snapshot --snapshot-id snap-1 --timeout 120s' \
  'delete-volume --volume-id lun-1 --timeout 120s' > "$expected"
diff -u "$expected" "$calls"
[[ ! -e "$OUT/.cleanup-failed" ]]

# Divergent IDs are all reclaimed, with snapshots always preceding the source LUN.
: > "$calls"
cat > "$OUT/csi-race-create-snapshot.json" <<'JSON'
{"results":[{"code":"OK","snapshot_id":"snap-a"},{"code":"OK","snapshot_id":"snap-b"},{"code":"OK","snapshot_id":"snap-a"}]}
JSON
direct_controller_race_cleanup
printf '%s\n' \
  'delete-snapshot --snapshot-id snap-a --timeout 120s' \
  'delete-snapshot --snapshot-id snap-b --timeout 120s' \
  'delete-volume --volume-id lun-1 --timeout 120s' > "$expected"
diff -u "$expected" "$calls"

# Corrupt evidence is surfaced as a cleanup failure even if a marker allows a
# known resource to be deleted; never claim cleanup completeness silently.
: > "$calls"
printf '{not-json' > "$OUT/csi-race-create-snapshot.json"
printf 'snap-known' > "$OUT/.csi-race-snapshot-id"
if direct_controller_race_cleanup 2>"$tmp/cleanup.err"; then
  echo "malformed race evidence unexpectedly passed cleanup" >&2
  exit 1
fi
[[ -e "$OUT/.cleanup-failed" ]]
grep -q 'cannot read race result' "$tmp/cleanup.err"
grep -q -- '--snapshot-id snap-known' "$calls"

echo "iSCSI partial/divergent snapshot race cleanup contract passed"