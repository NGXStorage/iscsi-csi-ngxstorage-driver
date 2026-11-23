#!/usr/bin/env python3
"""Verified-prebuilt test-binary channel for the CSI sanity/scenario harness.

The execution helper may have no Go toolchain. The approved path is: build
locally, record hash + build provenance, transfer, and verify fail-closed on
the execution host BEFORE any use. This module is the shared verification
implementation used by tests/sanity/cloud/helpers.sh (sanity_bin,
sanity_proxy_bin) and tests/scenario/lib.sh (csi_race_prepare) through
prebuilt.sh. It never falls back to an unverified binary: any failed check
is a hard error with a nonzero exit.

Environment contract (shared with the run-all dispatcher):
  CSI_PREBUILT_DIR       directory holding the prebuilt binaries
                         (csi-sanity, sanity-proxy, csi-race, e2e.test)
  CSI_PREBUILT_MANIFEST  path to the JSON provenance manifest
  CSI_PREBUILT_PROBE_TIMEOUT  optional smoke-probe seconds (default 15)

When CSI_PREBUILT_DIR is unset the callers keep their original go-build
path; this module is not consulted at all.

Manifest schema: a JSON object keyed by binary name; each entry carries
string fields (all required, all non-empty):
  {"csi-sanity": {
     "sha256": "<64 lowercase hex of the binary file>",
     "goos": "linux", "goarch": "amd64",
     "source_fingerprint": "<see algorithm below>",
     "source_commit": "<full 40-hex fork commit the binary was built from>",
     "go_version": "<toolchain that built it>",
     "build_flags": "<flags used>",
     "built_at": "<UTC timestamp>"}, ...}
go_version/build_flags/built_at are recorded provenance: they must be
present and non-empty, but only sha256/goos/goarch/source_fingerprint and
the smoke probe are machine-checkable on a Go-less execution host.
source_commit is REQUIRED only for csi-sanity (the shared fork-built binary);
the csi-sanity smoke probe additionally asserts --version identifies that
exact fork commit, so a binary built from the wrong commit is rejected.

Verification order (ALL must pass before the path is returned):
  1. env set, manifest exists and parses as a JSON object;
  2. entry for the requested binary exists with all required fields;
  3. binary exists as a regular file (no symlink) and is executable;
  4. sha256(binary) == entry.sha256;
  5. entry.goos/goarch == execution host (uname semantics);
  6. entry.source_fingerprint == fingerprint recomputed from the CURRENT
     driver source at runtime (stale or wrong-source builds fail);
  7. smoke probe executes the binary on the host:
       csi-sanity   --version  -> exit 0
       sanity-proxy -h         -> exit 0 (flag help, before any listener)
       csi-race     (no args)  -> exit 2 AND "usage: csi-race" on output
                                  (exit 2 alone could be a Go panic)
       e2e.test     -h         -> exit 0 (test-binary help, no tests run)
     Wrong-arch/corrupt binaries fail to exec or probe and are rejected.

source_fingerprint — exact algorithm (reproduce at build time by running
`python3 tests/sanity/cloud/prebuilt.py fingerprint <driver-root>` on the
build host AFTER the build, from the identical source tree):

  1. Module inventory: start at the driver root; parse go.mod `replace`
     directives (single-line and block form) and follow every LOCAL-path
     target (./, ../ or absolute; versioned replacements are not local),
     recursively through each target's own go.mod. This mirrors
     build/image.py module_dirs()/sources() local-replace discovery without
     needing Go on the execution host; it is a superset (all local replaces
     in the module graph, not only ./cmd/csi-driver deps), which can only
     over-invalidate, never under-invalidate. Pinned module dependencies
     are covered indirectly through go.mod/go.sum content hashes.
  2. Map every module directory to its git toplevel
     (`git rev-parse --show-toplevel`); a directory without Git provenance
     is a hard error (same rule as build/image.py sources()). Deduplicate
     by toplevel and sort toplevels bytewise by absolute path.
  3. Per repository, build the file inventory:
       `git ls-files -z` (tracked) + `git ls-files --others
       --exclude-standard -z` (untracked, non-ignored; this includes
       vendor/ where it is untracked build input).
     Drop any path with a path component named "logs" or "artifacts"
     (generated test logs / build artifacts, e.g. tests/logs/**,
     build/artifacts/**), even if force-added to the index. Git's
     exclude-standard already keeps ignored logs/evidence/credentials out.
     Deduplicate and sort the remaining paths bytewise.
  4. Per repository hash:
       head = `git rev-parse HEAD`
       repo_hash = sha256 over, for each sorted path P:
           P (raw bytes) + NUL +
           "symlink" + NUL + readlink target bytes   (if P is a symlink)
           "file" + NUL + sha256(file content) hex   (if P is a regular file)
           "deleted"                               (tracked P missing from
                                                    the worktree)
           + LF
       Any other entry kind (directory/gitlink) is a hard error.
  5. Feed each repository record into the outer hash in sorted order:
       absolute repo path + NUL + head + NUL + repo_hash hex + LF
  6. source_fingerprint = sha256 of the concatenated records, lowercase hex.

The fingerprint therefore covers HEAD, every tracked/untracked source edit,
additions and deletions in the driver repo AND its local replace deps, and
changes when the build tree differs from the execution tree in any of those.
"""
from __future__ import annotations

import hashlib
import json
import os
import platform
import re
import subprocess
import sys
from pathlib import Path

KNOWN_BINARIES = ("csi-sanity", "sanity-proxy", "csi-race", "e2e.test")
REQUIRED_FIELDS = ("sha256", "goos", "goarch", "source_fingerprint",
                   "go_version", "build_flags", "built_at")
EXCLUDED_COMPONENTS = {"logs", "artifacts"}


class PrebuiltError(RuntimeError):
    pass


def _run(args, cwd=None):
    return subprocess.run(args, cwd=cwd, check=True, capture_output=True).stdout


def _git(repo, *args):
    return _run(["git", "-C", str(repo), *args])


def _local_replace_dirs(module_dir):
    """Local-path replace targets of one go.mod (no Go toolchain needed)."""
    gomod = module_dir / "go.mod"
    if not gomod.is_file():
        raise PrebuiltError(f"local replace dependency has no go.mod: {module_dir}")
    dirs = []
    in_block = False
    for raw in gomod.read_text(encoding="utf-8").splitlines():
        line = raw.split("//", 1)[0].strip()
        if not line:
            continue
        if line == "replace (":
            in_block = True
            continue
        if in_block:
            if line == ")":
                in_block = False
                continue
            entry = line
        elif line.startswith("replace "):
            entry = line[len("replace "):]
        else:
            continue
        if "=>" not in entry:
            raise PrebuiltError(f"malformed replace directive in {gomod}: {line}")
        target = entry.split("=>", 1)[1].strip().split()
        if len(target) != 1:
            continue  # versioned replacement: not a local directory
        path = target[0]
        if path.startswith(("./", "../", "/")):
            dirs.append((module_dir / path).resolve())
    return dirs


def _repo_toplevels(driver_root):
    seen, queue = set(), [driver_root]
    while queue:
        module_dir = queue.pop(0)
        if module_dir in seen:
            continue
        seen.add(module_dir)
        queue.extend(d for d in _local_replace_dirs(module_dir) if d not in seen)
    tops = set()
    for module_dir in sorted(seen):
        try:
            top = _git(module_dir, "rev-parse", "--show-toplevel").decode().strip()
        except subprocess.CalledProcessError:
            raise PrebuiltError(
                f"local build dependency has no Git provenance: {module_dir}") from None
        tops.add(Path(top))
    return sorted(tops)


def _excluded(rel):
    return any(part in EXCLUDED_COMPONENTS for part in rel.split("/"))


def _repo_record(repo):
    head = _git(repo, "rev-parse", "HEAD").decode().strip()
    tracked = _git(repo, "ls-files", "-z").split(b"\0")
    untracked = _git(repo, "ls-files", "--others", "--exclude-standard", "-z").split(b"\0")
    paths = sorted({p for p in tracked + untracked
                    if p and not _excluded(os.fsdecode(p))})
    digest = hashlib.sha256()
    for raw in paths:
        full = repo / os.fsdecode(raw)
        digest.update(raw + b"\0")
        if full.is_symlink():
            digest.update(b"symlink\0" + os.fsencode(os.readlink(full)))
        elif full.is_file():
            digest.update(b"file\0" + hashlib.sha256(full.read_bytes()).hexdigest().encode())
        elif not full.exists():
            digest.update(b"deleted")  # tracked file removed from the worktree
        else:
            raise PrebuiltError(f"unsupported source inventory entry: {full}")
        digest.update(b"\n")
    return (str(repo).encode() + b"\0" + head.encode() + b"\0"
            + digest.hexdigest().encode() + b"\n")


def source_fingerprint(driver_root):
    digest = hashlib.sha256()
    for repo in _repo_toplevels(driver_root):
        digest.update(_repo_record(repo))
    return digest.hexdigest()


def _host_platform():
    goos = platform.system().lower()
    goarch = {"x86_64": "amd64", "amd64": "amd64",
              "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine().lower())
    if goos not in ("linux", "darwin") or goarch is None:
        raise PrebuiltError(
            f"unsupported execution host platform: {platform.system()}/{platform.machine()}")
    return goos, goarch


def _probe_timeout():
    raw = os.environ.get("CSI_PREBUILT_PROBE_TIMEOUT", "15")
    try:
        value = float(raw)
    except ValueError:
        raise PrebuiltError(f"CSI_PREBUILT_PROBE_TIMEOUT is not a number: {raw!r}") from None
    if value <= 0:
        raise PrebuiltError("CSI_PREBUILT_PROBE_TIMEOUT must be positive")
    return value


def _smoke_probe(name, binary, expect_commit=None):
    probes = {
        "csi-sanity": (["--version"], (0,), None),
        "sanity-proxy": (["-h"], (0,), None),
        "csi-race": ([], (2,), b"usage: csi-race"),
        "e2e.test": (["-h"], (0,), None),
    }
    args, ok_codes, needle = probes[name]
    try:
        proc = subprocess.run([str(binary), *args], capture_output=True,
                              timeout=_probe_timeout())
    except subprocess.TimeoutExpired:
        raise PrebuiltError(f"{name}: smoke probe timed out") from None
    except OSError as exc:
        raise PrebuiltError(f"{name}: smoke probe cannot execute binary: {exc}") from None
    if proc.returncode not in ok_codes:
        raise PrebuiltError(
            f"{name}: smoke probe exit {proc.returncode}, expected {ok_codes}")
    if needle is not None and needle not in proc.stdout + proc.stderr:
        raise PrebuiltError(f"{name}: smoke probe output missing {needle.decode()!r}")
    if expect_commit is not None:
        # The shared csi-sanity binary is stamped at build time with the fork
        # commit it was built from; --version must identify that exact commit.
        if expect_commit not in proc.stdout.decode(errors="replace"):
            raise PrebuiltError(
                f"{name}: --version does not identify expected fork commit "
                f"{expect_commit}")


def verify(driver_root, name):
    if name not in KNOWN_BINARIES:
        raise PrebuiltError(f"unknown prebuilt binary name: {name!r}")
    prebuilt_dir = os.environ.get("CSI_PREBUILT_DIR", "")
    manifest_path = os.environ.get("CSI_PREBUILT_MANIFEST", "")
    if not prebuilt_dir:
        raise PrebuiltError("CSI_PREBUILT_DIR is not set")
    if not manifest_path:
        raise PrebuiltError("CSI_PREBUILT_MANIFEST is required when CSI_PREBUILT_DIR is set")
    manifest_file = Path(manifest_path)
    if not manifest_file.is_file():
        raise PrebuiltError(f"prebuilt manifest missing: {manifest_file}")
    try:
        manifest = json.loads(manifest_file.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, UnicodeDecodeError, OSError) as exc:
        raise PrebuiltError(f"prebuilt manifest unreadable/invalid JSON: {exc}") from None
    if not isinstance(manifest, dict):
        raise PrebuiltError("prebuilt manifest must be a JSON object keyed by binary name")
    entry = manifest.get(name)
    if not isinstance(entry, dict):
        raise PrebuiltError(f"prebuilt manifest has no entry for {name}")
    for field in REQUIRED_FIELDS:
        value = entry.get(field)
        if not isinstance(value, str) or not value.strip():
            raise PrebuiltError(f"manifest entry {name}.{field} missing or empty")
    if not re.fullmatch(r"[0-9a-f]{64}", entry["sha256"]):
        raise PrebuiltError(f"manifest entry {name}.sha256 is not 64 lowercase hex chars")
    expect_commit = None
    if name == "csi-sanity":
        expect_commit = entry.get("source_commit")
        if not isinstance(expect_commit, str) or not re.fullmatch(r"[0-9a-f]{40}", expect_commit):
            raise PrebuiltError(
                "manifest entry csi-sanity.source_commit missing or not a full "
                "40-hex fork commit")
    binary = Path(prebuilt_dir).resolve() / name
    if binary.is_symlink() or not binary.is_file():
        raise PrebuiltError(f"prebuilt binary missing or not a regular file: {binary}")
    if not os.access(binary, os.X_OK):
        raise PrebuiltError(f"prebuilt binary not executable: {binary}")
    actual = hashlib.sha256(binary.read_bytes()).hexdigest()
    if actual != entry["sha256"]:
        raise PrebuiltError(
            f"{name}: sha256 mismatch (manifest={entry['sha256']} actual={actual})")
    goos, goarch = _host_platform()
    if entry["goos"] != goos or entry["goarch"] != goarch:
        raise PrebuiltError(
            f"{name}: manifest targets {entry['goos']}/{entry['goarch']}, "
            f"execution host is {goos}/{goarch}")
    current = source_fingerprint(driver_root)
    if entry["source_fingerprint"] != current:
        raise PrebuiltError(
            f"{name}: source fingerprint mismatch — stale or wrong-source build "
            f"(manifest={entry['source_fingerprint']} current={current})")
    _smoke_probe(name, binary, expect_commit)
    return binary


def main(argv):
    if len(argv) == 2 and argv[0] == "fingerprint":
        print(source_fingerprint(Path(argv[1]).resolve()))
        return 0
    if len(argv) == 3 and argv[0] == "verify":
        print(verify(Path(argv[1]).resolve(), argv[2]))
        return 0
    print("usage: prebuilt.py fingerprint <driver-root>\n"
          "       prebuilt.py verify <driver-root> <binary-name>", file=sys.stderr)
    return 2


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv[1:]))
    except PrebuiltError as exc:
        print(f"prebuilt: ERROR: {exc}", file=sys.stderr)
        sys.exit(1)
    except subprocess.CalledProcessError as exc:
        detail = (exc.stderr or b"").decode(errors="replace").strip().splitlines()
        hint = detail[0] if detail else "no detail"
        print(f"prebuilt: ERROR: command failed ({' '.join(exc.cmd[:3])}...): {hint}",
              file=sys.stderr)
        sys.exit(1)
    except OSError as exc:
        print(f"prebuilt: ERROR: {exc}", file=sys.stderr)
        sys.exit(1)