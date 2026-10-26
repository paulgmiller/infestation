from contextlib import contextmanager
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('azure_bootstrap', Path(__file__).with_name('bootstrap.py'))
bootstrap = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bootstrap)
ID = '96c1d420-f94b-43e3-845d-e946b2fcce1b'


def result(code=0, stdout='', stderr=''):
    return subprocess.CompletedProcess([], code, stdout, stderr)


class FakeBootstrap(bootstrap.Bootstrap):
    def __init__(self, state, initialized=False, interrupt=False):
        nodes = [{'name':f'kube{i}', 'ip':f'192.0.2.{i}'} for i in range(1,4)]
        super().__init__(state, {'nodes':nodes}, {}, state/'key', 'cilium')
        self.initialized, self.interrupt, self.inits = initialized, interrupt, 0

    @contextmanager
    def tunnel(self, *args):
        yield 12345

    def run(self, *args, **kwargs):
        return result()

    def sql_identity(self, port):
        return result(stdout='cluster_id\n'+ID+'\n') if self.initialized else result(1)

    def db(self, port, *args, **kwargs):
        if args[0] == 'init':
            marker = json.loads((self.state/'bootstrap.json').read_text())
            assert marker['initializationAttempted']
            self.inits += 1
            if self.interrupt:
                raise RuntimeError('Interrupted after init intent')
            self.initialized = True
            return result()
        if args[0] == 'node':
            return result(stdout='is_live,ranges_unavailable,ranges_underreplicated\ntrue,0,0\ntrue,0,0\ntrue,0,0\n')
        return result()


class BootstrapTests(unittest.TestCase):
    def test_fresh_cluster_initializes_once_and_repeated_bootstrap_skips_init(self):
        with tempfile.TemporaryDirectory() as tmp:
            worker = FakeBootstrap(Path(tmp))
            worker.database(True)
            worker.database(True)
            self.assertEqual(worker.inits, 1)
            self.assertEqual(json.loads((Path(tmp)/'bootstrap.json').read_text())['clusterID'], ID)
            self.assertEqual((Path(tmp)/'bootstrap.json').stat().st_mode & 0o777, 0o600)

    def test_interrupted_init_is_not_automatically_repeated(self):
        with tempfile.TemporaryDirectory() as tmp:
            worker = FakeBootstrap(Path(tmp), interrupt=True)
            with self.assertRaisesRegex(RuntimeError, 'Interrupted'):
                worker.database(True)
            with self.assertRaisesRegex(RuntimeError, 'never repeated'):
                worker.database(True)
            self.assertEqual(worker.inits, 1)

    def test_lost_quorum_does_not_initialize_a_new_database(self):
        with tempfile.TemporaryDirectory() as tmp:
            bootstrap.private_json(Path(tmp)/'bootstrap.json', {'clusterID': ID})
            worker = FakeBootstrap(Path(tmp))
            def once(action, description, **kwargs):
                response = action()
                if response.returncode:
                    raise RuntimeError('Existing cluster unavailable')
                return response
            with patch.object(bootstrap, 'wait_for', side_effect=once):
                with self.assertRaisesRegex(RuntimeError, 'Existing cluster unavailable'):
                    worker.database(True)
            self.assertEqual(worker.inits, 0)

    def test_changed_or_malformed_identity_is_rejected(self):
        with self.assertRaisesRegex(RuntimeError, 'identity changed'):
            bootstrap.record_identity({'clusterID':ID}, 'e9c1d420-f94b-43e3-845d-e946b2fcce1b')
        with self.assertRaises((ValueError, RuntimeError)):
            bootstrap.cluster_id(result(stdout='cluster_id\nnot-a-uuid\n'))


if __name__ == '__main__':
    unittest.main()
