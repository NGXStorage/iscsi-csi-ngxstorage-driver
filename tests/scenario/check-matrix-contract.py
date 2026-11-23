#!/usr/bin/env python3
"""Static contract for the Plan 11 CSI live-acceptance scenario matrix."""

from __future__ import annotations

import pathlib
import sys


SCENARIO_ROOT = pathlib.Path(__file__).resolve().parent
DRIVER_NAME = {"fc": "FC", "iscsi": "iSCSI", "nfs": "NFS"}

COMMON = {
    "controller-restart-leader-election",
    "volume-access-multivolume",
    "volume-access-readonly",
    "volume-access-readonly-multipod",
    "volume-access-rwop",
    "volume-attributes-mutable",
    "volume-data-integrity-marker",
    "volume-delete-attached",
    "volume-existing-static",
    "volume-lifecycle-rwo",
    "volume-race-idempotent-create",
    "volume-race-snapshot-create",
    "volume-recovery-host-reboot",
    "volume-resize-attached",
    "volume-resize-online-filesystem",
    "volume-snapshot-expand-restore",
    "volume-snapshot-restore",
    "workload-statefulset",
    "sanity",
}

BLOCK = {
    "filesystem-xfs",
    "volume-access-multiwriter",
    "volume-attach-cross-node",
    "volume-attach-multipath",
    "volume-race-controller-expand",
    "volume-race-first-stage-format",
    "volume-raw-block-device",
    "volume-resize-raw-block",
}

REQUIRED = {
    "fc": COMMON | BLOCK,
    "iscsi": COMMON | BLOCK | {"volume-race-publish-unpublish"},
    "nfs": COMMON
    | {
        "mount-nfs-v3",
        "volume-access-readwrite-multipod",
        "volume-access-rwx-multinode",
        "volume-existing-readonly-static",
    },
}


def fail(message: str) -> None:
    raise SystemExit(message)


def validate_protocol(protocol: str) -> None:
    root = SCENARIO_ROOT
    if not root.is_dir():
        fail(f"{protocol}: protocol test directory is missing")

    actual = {path.name for path in root.iterdir() if path.is_dir() and (path / "run-test.sh").is_file()}
    missing = sorted(REQUIRED[protocol] - actual)
    if missing:
        fail(f"{protocol}: required Plan 11 scenarios are missing: {missing}")

    for scenario in sorted(REQUIRED[protocol]):
        runner = root / scenario / "run-test.sh"
        # run.sh deliberately invokes every scenario as `bash run-test.sh`, so
        # an executable bit is not part of the public runner contract.
        text = runner.read_text(encoding="utf-8")
        if "lib.sh" not in text:
            fail(f"{protocol}/{scenario}: runner does not source the shared CSI test library")

    print(f"{protocol}: required Plan 11 matrix contract passed ({len(REQUIRED[protocol])} scenarios)")


def main() -> int:
    protocol = pathlib.Path(__file__).resolve().parents[2].name.lower().replace("-consumer", "")
    if protocol not in REQUIRED:
        fail(f"unsupported driver test root: {protocol}")
    validate_protocol(protocol)
    print(f"CSI {protocol} functional matrix contract passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
