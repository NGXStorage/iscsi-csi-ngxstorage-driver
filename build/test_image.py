#!/usr/bin/env python3
"""Registry/provenance regression tests; no registry writes or daemon required."""
import importlib.util
import contextlib
import io
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('driver_image', Path(__file__).with_name('image.py'))
image = importlib.util.module_from_spec(spec)
spec.loader.exec_module(image)


class ImageContract(unittest.TestCase):
    def response(self, stdout='', stderr='', code=0):
        return subprocess.CompletedProcess([], code, stdout, stderr)

    def test_only_explicit_missing_is_absent(self):
        for missing in ['manifest unknown', 'ERROR: registry/repo:sha: not found']:
            with patch.object(image.subprocess, 'run', return_value=self.response(stderr=missing, code=1)):
                self.assertIsNone(image.registry_manifest('registry/repo:sha'))
        for error in ['401 Unauthorized', '403 denied', 'authentication required',
                      'dial tcp: connection refused', 'TLS handshake timeout', 'name not found',
                      'unexpected redirect', '404 not found: authentication required']:
            with self.subTest(error=error), patch.object(image.subprocess, 'run', return_value=self.response(stderr=error, code=1)):
                with self.assertRaises(image.BuildError):
                    image.registry_manifest('registry/repo:sha')

    def test_timeout_and_invalid_response_are_errors(self):
        with patch.object(image.subprocess, 'run', side_effect=subprocess.TimeoutExpired('docker', 45)):
            with self.assertRaises(image.BuildError):
                image.registry_manifest('registry/repo:sha')
        for response in ['not json', '[]']:
            with patch.object(image.subprocess, 'run', return_value=self.response(stdout=response)):
                with self.assertRaises(image.BuildError):
                    image.registry_manifest('registry/repo:sha')

    def test_existing_tag_cannot_change_content_or_replace_index(self):
        for manifest in [{'config': {'digest': 'sha256:other'}}, {'manifests': [{'digest': 'sha256:same'}]}]:
            with patch.object(image, 'registry_manifest', return_value=manifest):
                with self.assertRaises(image.BuildError):
                    image.immutable_tag('registry/repo:sha', 'sha256:same')
        with patch.object(image, 'registry_manifest', return_value={'config': {'digest': 'sha256:same'}}):
            self.assertTrue(image.immutable_tag('registry/repo:sha', 'sha256:same'))
        with patch.object(image, 'registry_manifest', return_value=None):
            self.assertFalse(image.immutable_tag('registry/repo:sha', 'sha256:same'))

    def test_local_replacements_and_workspace_modules_are_in_provenance(self):
        packages = [
            {'Module': {'Main': True, 'Dir': '/source/driver'}},
            {'Module': {'Path': 'sdk', 'Replace': {'Dir': '/source/sdk'}}},
            {'Module': {'Path': 'sdk', 'Replace': {'Dir': '/source/sdk'}}},
            {'Module': {'Path': 'remote', 'Dir': '/cache/remote@v1'}},
            {'Module': {'Replace': {'Dir': '/cache/fork@v1', 'Version': 'v1'}}},
        ]
        self.assertEqual(image.module_dirs('\n'.join(json.dumps(p) for p in packages)), ['/source/driver', '/source/sdk'])

    def test_wrong_version_or_revision_is_rejected(self):
        good = {'org.opencontainers.image.revision': 'abcdef', 'vcs-ref': 'abcdef',
                'version': '1.0.0', 'org.opencontainers.image.version': '1.0.0'}
        image.verify_labels(good, 'abcdef', '1.0.0')
        for key in good:
            with self.subTest(key=key), self.assertRaises(image.BuildError):
                image.verify_labels({**good, key: 'unknown'}, 'abcdef', '1.0.0')

    def test_dirty_dependencies_stop_push_before_docker(self):
        with patch.object(image, 'capture', return_value='a' * 40), \
             patch.object(image, 'sources', return_value=[{'dirty': True}]), \
             patch.object(image.subprocess, 'run') as run, \
             patch.dict(image.os.environ, {}, clear=True):
            with self.assertRaisesRegex(image.BuildError, 'clean driver and local dependency'):
                image.main(['quay'])
            run.assert_not_called()


    def test_publish_builds_once_and_reuses_candidate_for_release(self):
        import tempfile
        home = Path.home()
        scratch = home / '.cache/ngxstorage'
        scratch.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(dir=scratch) as directory:
            root = Path(directory) / 'arbitrary-checkout-name'
            (root / 'pkg/driver').mkdir(parents=True)
            (root / 'build').mkdir()
            (root / 'pkg/driver/driver.go').write_text('DriverName = "fc.csi.ngxstorage.com"\nDriverVersion = "1.0.0"')
            revision, image_id = 'a' * 40, 'sha256:' + 'b' * 64
            labels = {'org.opencontainers.image.revision': revision, 'vcs-ref': revision,
                      'version': '1.0.0', 'org.opencontainers.image.version': '1.0.0'}
            published, calls = set(), []

            def command(args, **kwargs):
                calls.append(args)
                if args[:2] == ['docker', 'build']:
                    Path(args[args.index('--iidfile') + 1]).write_text(image_id)
                if args[:2] == ['docker', 'push']:
                    published.add(args[2])
                return self.response()

            def captured(args, **kwargs):
                if args[:2] == ['git', 'rev-parse']:
                    return revision[:12] if '--short=12' in args else revision
                if args[:3] == ['docker', 'image', 'inspect']:
                    return json.dumps(labels)
                if args[:3] == ['docker', 'buildx', 'imagetools']:
                    return 'sha256:' + 'c' * 64
                raise AssertionError(args)

            with patch.object(image, '__file__', str(root / 'build/image.py')), \
                 patch.object(image, 'sources', return_value=[{'path': str(root), 'revision': revision, 'dirty': False}]), \
                 patch.object(image, 'capture', side_effect=captured), \
                 patch.object(image.subprocess, 'run', side_effect=command), \
                 patch.object(image, 'registry_manifest', return_value=None), \
                 patch.object(image, 'immutable_tag', side_effect=lambda ref, iid: ref in published), \
                 patch.dict(image.os.environ, {'HOME': str(home), 'DRIVER_RELEASE': '1', 'RELEASE_TAG': '1.0.0-1'}, clear=True), \
                 contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(image.main(['quay']), 0)
            self.assertEqual(sum(c[:2] == ['docker', 'build'] for c in calls), 1)
            self.assertEqual(len(published), 2)
            self.assertTrue(all(c[2] == image_id for c in calls if c[:2] == ['docker', 'tag']))
            evidence = json.loads((root / 'build/artifacts/image.json').read_text())
            self.assertEqual(set(evidence['published']), published)
            self.assertTrue(all(':dev' not in r and ':latest' not in r for r in published))


if __name__ == '__main__':
    unittest.main()
