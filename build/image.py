#!/usr/bin/env python3
"""Build one image and publish immutable source tags."""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


class BuildError(RuntimeError):
    pass


def capture(args, *, cwd=None, input=None):
    return subprocess.run(args, cwd=cwd, input=input, text=True, capture_output=True, check=True).stdout.strip()


def registry_manifest(reference):
    """Docker handles credentials/tokens; only an explicit missing manifest is absent."""
    try:
        result = subprocess.run(
            ['docker', 'buildx', 'imagetools', 'inspect', '--raw', reference],
            text=True, capture_output=True, timeout=45,
        )
    except subprocess.TimeoutExpired:
        raise BuildError('Registry lookup timed out; publication stopped') from None
    if result.returncode:
        error = result.stderr.lower()
        if any(s in error for s in ('unauthorized', 'denied', '401', '403', 'authentication')):
            raise BuildError('Registry authentication/authorization failed; check docker login')
        if (f'{reference.lower()}: not found' in error
                or any(s in error for s in ('manifest unknown', 'manifest_unknown', 'no such manifest', '404 not found'))):
            return None
        raise BuildError('Registry lookup failed; check registry name, network and TLS (publication stopped)')
    try:
        manifest = json.loads(result.stdout)
    except json.JSONDecodeError:
        raise BuildError('Registry returned an invalid manifest') from None
    if not isinstance(manifest, dict):
        raise BuildError('Registry returned an invalid manifest')
    return manifest


def immutable_tag(reference, image_id):
    manifest = registry_manifest(reference)
    if manifest is None:
        return False
    # Single-platform builds must not replace a multi-platform index either.
    if manifest.get('config', {}).get('digest') != image_id:
        raise BuildError(f'Immutable tag already identifies different content: {reference}')
    return True


def module_dirs(stream):
    decoder = json.JSONDecoder()
    directories = set()
    while stream.strip():
        item, offset = decoder.raw_decode(stream.lstrip())
        stream = stream.lstrip()[offset:]
        module = item.get('Module') or {}
        replacement = module.get('Replace') or {}
        if module.get('Main'):
            directories.add(module['Dir'])
        elif replacement.get('Dir') and not replacement.get('Version'):
            directories.add(replacement['Dir'])
    return sorted(directories)


def sources(root, *, vendored=False):
    dependencies = capture(['go', 'list', '-deps', '-json', './cmd/csi-driver'], cwd=root)
    result, seen = [], set()
    # Vendored dependencies belong to the driver's own source commit.
    directories = [str(root)] if vendored else [str(root), *module_dirs(dependencies)]
    for directory in directories:
        try:
            repo = capture(['git', '-C', directory, 'rev-parse', '--show-toplevel'])
        except subprocess.CalledProcessError:
            raise BuildError(f'Local build dependency has no Git provenance: {directory}') from None
        if repo in seen:
            continue
        seen.add(repo)
        revision = capture(['git', '-C', repo, 'rev-parse', 'HEAD'])
        status = capture(['git', '-C', repo, 'status', '--porcelain', '--untracked-files=all'])
        diff = subprocess.check_output(['git', '-C', repo, 'diff', '--binary', 'HEAD'])
        fingerprint = hashlib.sha256(status.encode() + diff)
        # Detect edits to untracked source too; ignored credentials/logs are excluded.
        names = subprocess.check_output(['git', '-C', repo, 'ls-files', '--others', '--exclude-standard', '-z'])
        for name in names.split(b'\0'):
            if name:
                path = Path(repo) / os.fsdecode(name)
                fingerprint.update(name)
                if path.is_symlink():
                    fingerprint.update(os.readlink(path).encode())
                elif path.is_file():
                    fingerprint.update(path.read_bytes())
        result.append({'path': repo, 'revision': revision, 'dirty': bool(status), 'fingerprint': fingerprint.hexdigest()})
    return result


def verify_labels(labels, revision, version):
    if labels.get('org.opencontainers.image.revision') != revision or labels.get('vcs-ref') != revision:
        raise BuildError('Built image revision does not match its source')
    if labels.get('version') != version or labels.get('org.opencontainers.image.version') != version:
        raise BuildError('Built image version does not match the driver')


def main(argv):
    if argv in (['--help'], ['-h']):
        print('Usage: make docker-driver | make docker-push REGISTRY_TARGET=quay|prod|test\n'
              'local builds only; quay/prod/test build and push commit tags.\n'
              'Optional: IMAGE=<repository>, DRIVER_RELEASE=dev, RELEASE_TAG=<version-release>.\n'
              'Push requires clean driver/local dependency sources and docker login.\n'
              'Dirty local builds use <commit>-dirty; no latest/dev/branch tags are created.')
        return 0
    if len(argv) != 1 or argv[0] not in {'local', 'quay', 'prod', 'test'}:
        raise BuildError('Expected local|quay|prod|test; see --help')
    mode = argv[0]
    root = Path(__file__).resolve().parents[1]
    source = (root / 'pkg/driver/driver.go').read_text()
    identity = re.search(r'DriverName\s*=\s*"(fc|iscsi|nfs)\.csi\.ngxstorage\.com"', source)
    if not identity:
        raise BuildError('Cannot resolve driver identity')
    protocol = identity[1]
    name = f'{protocol}-csi-ngxstorage'
    temporary = Path.home() / '.cache/ngxstorage'
    temporary.mkdir(parents=True, exist_ok=True)
    os.environ['TMPDIR'] = str(temporary)
    module_mode = '-mod=vendor' if (root / 'vendor/modules.txt').is_file() else '-mod=readonly'
    os.environ['GOFLAGS'] = (os.environ.get('GOFLAGS', '') + ' ' + module_mode).strip()
    os.environ.setdefault('CGO_ENABLED', '0')
    os.environ['GOOS'], os.environ['GOARCH'] = 'linux', 'amd64'
    version_match = re.search(r'DriverVersion\s*=\s*"([^"]+)"', source)
    if not version_match:
        raise BuildError('Cannot resolve driver version')
    version = version_match[1]
    if os.environ.get('DRIVER_VERSION', version) != version:
        raise BuildError('DRIVER_VERSION must match pkg/driver/driver.go')
    revision = capture(['git', 'rev-parse', 'HEAD'], cwd=root)
    if os.environ.get('VCS_REF', revision) != revision:
        raise BuildError('VCS_REF must be the full current source commit')
    short = capture(['git', 'rev-parse', '--short=12', 'HEAD'], cwd=root)
    release = os.environ.get('DRIVER_RELEASE', 'dev')
    release_tag = os.environ.get('RELEASE_TAG', '')
    if release_tag and (release == 'dev' or release_tag != f'{version}-{release}'):
        raise BuildError('RELEASE_TAG must equal <driver-version>-<DRIVER_RELEASE>; set a non-dev release')
    if not re.fullmatch(r'[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}', release):
        raise BuildError('Invalid DRIVER_RELEASE')
    before = sources(root, vendored=module_mode == '-mod=vendor')
    dirty = any(item['dirty'] for item in before)
    if dirty and mode != 'local':
        raise BuildError('Push requires clean driver and local dependency repositories; commit changes first')
    repositories = {'local': name + '-driver', 'quay': 'quay.io/ngxstorage/' + name,
                    'prod': 'docker.io/ngxstorage/' + name, 'test': 'docker.io/babico/' + name}
    repository = os.environ.get('IMAGE', repositories[mode])
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9./_-]*', repository):
        raise BuildError('IMAGE must be a repository without a tag, digest or URL scheme')
    tag = short + ('-dirty' if dirty else '')
    reference = repository + ':' + tag
    if dirty and release_tag:
        raise BuildError('A dirty local build cannot carry a release tag')
    public_sources = [{'repository': Path(s['path']).name, 'revision': s['revision'], 'dirty': s['dirty']} for s in before]
    references = [reference, *([repository + ':' + release_tag] if release_tag else [])]
    if mode != 'local':
        password_name, host, user = {
            'quay': ('QUAY_LOGIN_NGX', 'quay.io', 'ngxstorage'),
            'prod': ('DCKR_LOGIN_NGX', 'docker.io', 'ngxstorage'),
            'test': ('DCKR_LOGIN_BABICO', 'docker.io', 'babico'),
        }[mode]
        password = os.environ.get(password_name)
        if password:
            capture(['docker', 'login', host, '--username', user, '--password-stdin'], input=password + '\n')
        # Reject auth/network failures before compilation. Check collisions after building.
        for requested in references:
            registry_manifest(requested)
    binary = root / 'cmd/csi-driver' / (name + '-driver')
    print(f'Building {reference} (driver {version}, source {revision}, dirty={dirty})', flush=True)
    subprocess.run(['go', 'build', '-trimpath', '-o', str(binary), './cmd/csi-driver'], cwd=root, check=True)
    # Build privately: an immutable tag collision must not replace a local SHA tag.
    with tempfile.TemporaryDirectory(prefix='csi-image-', dir=temporary) as scratch:
        iid = Path(scratch) / 'image-id'
        subprocess.run(['docker', 'build', '--platform=linux/amd64', '--iidfile', str(iid),
                        '--build-arg', 'VERSION=' + version, '--build-arg', 'RELEASE=' + release,
                        '--build-arg', 'VCS_REF=' + revision,
                        '--label', 'io.ngxstorage.source.dirty=' + str(dirty).lower(),
                        '--label', 'io.ngxstorage.source.modules=' + json.dumps(public_sources, separators=(',', ':')),
                        '--label', 'org.opencontainers.image.licenses=Apache-2.0',
                        '.'], cwd=root, check=True)
        image_id = iid.read_text().strip()
    if sources(root, vendored=module_mode == '-mod=vendor') != before:
        raise BuildError('Source changed during build; candidate left untagged, nothing pushed')
    labels = json.loads(capture(['docker', 'image', 'inspect', image_id, '--format', '{{json .Config.Labels}}']))
    verify_labels(labels, revision, version)
    existing = {}
    # Inspect every destination before publishing any tag.
    if mode != 'local':
        existing = {ref: immutable_tag(ref, image_id) for ref in references}
    for ref in references:
        subprocess.run(['docker', 'tag', image_id, ref], check=True)
    evidence = {'imageID': image_id, 'revision': revision, 'version': version, 'release': release,
                'dirty': dirty, 'sources': public_sources, 'references': references, 'published': {}}
    artifact = root / 'build/artifacts/image.json'
    artifact.parent.mkdir(parents=True, exist_ok=True)
    artifact.write_text(json.dumps(evidence, indent=2) + '\n')
    if mode != 'local':
        for ref in references:
            if not existing[ref] and not immutable_tag(ref, image_id):
                subprocess.run(['docker', 'push', ref], check=True)
            if not immutable_tag(ref, image_id):
                raise BuildError('Pushed image could not be read back')
            digest = capture(['docker', 'buildx', 'imagetools', 'inspect', ref, '--format', '{{.Manifest.Digest}}'])
            if not re.fullmatch(r'sha256:[0-9a-f]{64}', digest):
                raise BuildError('Registry did not return a valid image digest')
            evidence['published'][ref] = repository + '@' + digest
            artifact.write_text(json.dumps(evidence, indent=2) + '\n')
            print(f'Verified {ref} -> {repository}@{digest}', flush=True)
    print(f'Image: {image_id}\nEvidence: {artifact}')
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main(sys.argv[1:]))
    except (BuildError, subprocess.CalledProcessError, OSError) as error:
        # Captured auth/environment output is deliberately not dumped.
        message = str(error) if isinstance(error, BuildError) else 'Build command failed; see preceding output or check tooling'
        print('ERROR: ' + message, file=sys.stderr)
        sys.exit(1)
