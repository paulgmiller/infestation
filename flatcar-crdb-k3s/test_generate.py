import base64
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import generate


class GeneratorTests(unittest.TestCase):
    def data(self):
        data = json.loads((generate.HERE / 'cluster.json').read_text())
        data['ssh_authorized_key'] = 'ssh-ed25519 AAAATEST'
        return data

    def test_single_node_requires_explicit_lab_mode(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'cluster.json'
            data = self.data()
            data['nodes'] = data['nodes'][:1]
            with patch.object(generate, 'CONFIG', path):
                path.write_text(json.dumps(data))
                with self.assertRaises(ValueError):
                    generate.config()
                data['single_node_lab'] = True
                path.write_text(json.dumps(data))
                self.assertEqual(len(generate.config()['nodes']), 1)

    def test_generated_credentials_and_service_mode(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            pki = root / 'pki'
            for node in self.data()['nodes']:
                dst = pki / node['name']
                dst.mkdir(parents=True)
                for name in ('ca.crt', 'node.crt', 'node.key', 'client.kine.crt', 'client.kine.key'):
                    (dst / name).write_text(name)
            # CA private key and root credentials must never enter Ignition.
            (pki / 'ca.key').write_text('PRIVATE-CA-KEY')
            data = self.data()
            with patch.object(generate, 'PKI', pki), patch.object(generate, 'OUT', root / 'output'):
                generate.ignition(data)
                doc = json.loads((root / 'output/kube1.ign').read_text())
                files = {f['path']: f for f in doc['storage']['files']}
                self.assertTrue(files['/etc/flatcar/update.conf']['overwrite'])
                self.assertEqual(files['/etc/cockroach/certs/node.key']['user']['id'], 1000)
                self.assertEqual(files['/etc/cockroach/certs/node.key']['mode'], 0o400)
                self.assertFalse(any('root' in p or 'ca.key' in p for p in files))
                conf = base64.b64decode(files['/etc/rancher/k3s/config.yaml']['contents']['source'].split(',')[1]).decode()
                self.assertIn('sslmode=verify-full', conf)
                self.assertIn('flannel-backend: none', conf)
                unit = doc['systemd']['units'][0]['contents']
                self.assertIn(' start --certs-dir', unit)
                self.assertIn('--join=', unit)
                self.assertNotIn('--insecure', unit)
                data.update(single_node_lab=True, nodes=data['nodes'][:1])
                generate.ignition(data)
                unit = json.loads((root / 'output/kube1.ign').read_text())['systemd']['units'][0]['contents']
                self.assertIn(' start-single-node --certs-dir', unit)
                self.assertNotIn('--join=', unit)


if __name__ == '__main__':
    unittest.main()
