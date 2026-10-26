import base64
import json
import os
from pathlib import Path
import tempfile
import subprocess
import unittest
from unittest.mock import patch

import generate


class GeneratorTests(unittest.TestCase):
    def test_tls_san_validation(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(generate, 'CONFIG', Path(tmp) / 'cluster.json'):
            data = self.data()
            for invalid in ('api.example.test', [None], ['https://api.example.test'], ['host:6443'], ['bad..name']):
                data['tls_sans'] = invalid
                generate.CONFIG.write_text(json.dumps(data))
                with self.assertRaises(ValueError):
                    generate.config()
            data['tls_sans'] = ['api.example.test', '192.0.2.10', '2001:db8::1']
            generate.CONFIG.write_text(json.dumps(data))
            self.assertEqual(generate.config()['tls_sans'], data['tls_sans'])

    def data(self):
        data = json.loads((generate.HERE / 'cluster.json').read_text())
        data['ssh_authorized_key'] = 'ssh-ed25519 AAAATEST'
        return data

    def test_worker_config_validation(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(generate, 'CONFIG', Path(tmp) / 'cluster.json'):
            data = self.data()
            worker = {'name': 'worker1', 'ip': '192.168.1.104', 'role': 'worker'}
            data['nodes'].append(worker)
            generate.CONFIG.write_text(json.dumps(data))
            self.assertEqual(len(generate.config()['nodes']), 4)
            data['nodes'] = [worker]
            generate.CONFIG.write_text(json.dumps(data))
            with self.assertRaisesRegex(ValueError, 'require server_url'):
                generate.config()
            for url in ('http://192.168.1.101:6443', 'https://', 'https://host:bad',
                        'https://user:password@host:6443', 'https://host/path'):
                worker['server_url'] = url
                generate.CONFIG.write_text(json.dumps(data))
                with self.assertRaises(ValueError):
                    generate.config()
            worker['server_url'] = 'https://192.168.1.101:6443'
            generate.CONFIG.write_text(json.dumps(data))
            self.assertEqual(generate.config()['nodes'], [worker])
            worker['role'] = 'workre'
            data['nodes'] = self.data()['nodes'] + [worker]
            generate.CONFIG.write_text(json.dumps(data))
            with self.assertRaises(ValueError):
                generate.config()

    def test_worker_cockroach_default_and_opt_out(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            pki = root / 'pki'
            pki.mkdir()
            data = self.data()
            data['tls_sans'] = ['api.example.test']
            for node in data['nodes']:
                dst = pki / node['name']
                dst.mkdir()
                for name in ('ca.crt', 'node.crt', 'node.key', 'client.kine.crt', 'client.kine.key'):
                    (dst / name).write_text(name)
            worker = {'name': 'worker1', 'ip': '192.168.1.104', 'role': 'worker'}
            data['nodes'].insert(0, worker)
            worker_pki = pki / worker['name']
            worker_pki.mkdir()
            for name in ('ca.crt', 'node.crt', 'node.key'):
                (worker_pki / name).write_text(name)
            with patch.object(generate, 'PKI', pki), patch.object(generate, 'OUT', root / 'output'):
                generate.ignition(data)
                doc = json.loads((root / 'output/worker1.ign').read_text())
                files = {f['path']: f for f in doc['storage']['files']}
                conf_file = files['/etc/rancher/k3s/config.yaml']
                conf = base64.b64decode(conf_file['contents']['source'].split(',')[1]).decode()
                self.assertIn('server: "https://192.168.1.101:6443"', conf)
                self.assertIn(f'token: {json.dumps((pki / "k3s-token").read_text().strip())}', conf)
                self.assertEqual(conf_file['mode'], 0o600)
                for server_option in ('datastore-', 'advertise-address:', 'tls-san:',
                                      'flannel-backend:', 'disable-network-policy:', 'disable:'):
                    self.assertNotIn(server_option, conf)
                self.assertIn('/etc/cockroach/certs/node.crt', files)
                self.assertEqual(files['/etc/cockroach/certs/node.key']['mode'], 0o400)
                self.assertEqual(files['/etc/cockroach/certs/node.key']['user']['id'], 1000)
                self.assertNotIn('/etc/cockroach/certs/client.kine.key', files)
                self.assertNotIn('/etc/cockroach/certs/client.kine.crt', files)
                self.assertEqual([u['name'] for u in doc['systemd']['units']],
                                 ['cockroachdb.service', 'k3s-agent.service'])
                db = doc['systemd']['units'][0]
                self.assertTrue(db['enabled'])
                self.assertIn(' start --certs-dir', db['contents'])
                self.assertIn('192.168.1.101:26257', db['contents'])
                self.assertEqual(doc['storage']['links'], [{
                    'path': '/etc/systemd/system/multi-user.target.wants/k3s-agent.service',
                    'target': '/usr/local/lib/systemd/system/k3s-agent.service', 'overwrite': True,
                }])
                server = json.loads((root / 'output/kube1.ign').read_text())
                server_conf = next(f for f in server['storage']['files'] if f['path'] == '/etc/rancher/k3s/config.yaml')
                self.assertIn('  - "api.example.test"\n', base64.b64decode(server_conf['contents']['source'].split(',')[1]).decode())
                db_unit = server['systemd']['units'][0]['contents']
                self.assertIn('192.168.1.104:26257', db_unit)
                self.assertIn('192.168.1.101:26257', db_unit)
                worker['cockroachdb'] = False
                generate.ignition(data)
                doc = json.loads((root / 'output/worker1.ign').read_text())
                self.assertFalse(any(f['path'].startswith('/etc/cockroach/') for f in doc['storage']['files']))
                self.assertEqual([u['name'] for u in doc['systemd']['units']], ['k3s-agent.service'])
                self.assertNotIn('cockroachdb.service', json.dumps(doc['systemd']))
                server = json.loads((root / 'output/kube1.ign').read_text())
                self.assertNotIn('192.168.1.104:26257', server['systemd']['units'][0]['contents'])
                worker['server_url'] = 'https://api.example.test:6443'
                data['nodes'] = [worker]
                generate.ignition(data)
                doc = json.loads((root / 'output/worker1.ign').read_text())
                conf = next(f for f in doc['storage']['files'] if f['path'].endswith('config.yaml'))
                self.assertIn('https://api.example.test:6443', base64.b64decode(
                    conf['contents']['source'].split(',')[1]).decode())
                (pki / 'k3s-token').unlink()
                with self.assertRaisesRegex(ValueError, 'existing cluster token'):
                    generate.ignition(data)
                self.assertFalse((pki / 'k3s-token').exists())

    def test_workers_receive_node_pki_by_default_with_opt_out(self):
        with tempfile.TemporaryDirectory() as tmp:
            pki = Path(tmp) / 'pki'
            data = self.data()
            data['nodes'].append({'name': 'worker1', 'ip': '192.168.1.104', 'role': 'worker'})
            data['nodes'].append({'name': 'worker2', 'ip': '192.168.1.105',
                                  'role': 'worker', 'cockroachdb': False})

            def fake_docker(command, **kwargs):
                if 'create-ca' in command:
                    (pki / 'ca.crt').write_text('test-ca')

            with patch.object(generate, 'PKI', pki), patch.object(generate.subprocess, 'run', side_effect=fake_docker) as run:
                generate.make_pki(data)
                self.assertEqual((pki / 'worker1/ca.crt').read_text(), 'test-ca')
                worker_commands = [call.args[0] for call in run.call_args_list
                                   if '--certs-dir=/pki/worker1' in call.args[0]]
                self.assertEqual(len(worker_commands), 1)
                self.assertIn('create-node', worker_commands[0])
                self.assertFalse((pki / 'worker2').exists())
                self.assertFalse(any('/pki/worker2' in str(call) for call in run.call_args_list))

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
                data['cockroach_storage'] = 'azure-temporary'
                data['nodes'][0]['locality'] = 'cloud=azure,region=westus2,zone=1'
                generate.ignition(data)
                temp = json.loads((root / 'output/kube1.ign').read_text())
                unit = temp['systemd']['units'][0]['contents']
                self.assertIn('src=/mnt/resource/cockroach', unit)
                self.assertNotIn('src=/var/lib/cockroach', unit)
                self.assertIn('Requires=docker.service waagent.service', unit)
                self.assertIn('--locality=cloud=azure,region=westus2,zone=1', unit)
                checker = next(f for f in temp['storage']['files'] if f['path'].endswith('/check-cockroach-storage'))
                script = base64.b64decode(checker['contents']['source'].split(',')[1]).decode()
                self.assertIn('findmnt -rn -M /mnt/resource -o FSTYPE', script)
                self.assertIn(str(64 * 1024 ** 3), script)
                self.assertNotIn('mkfs', script)
                # Exercise fail-closed mount checks without mounting or formatting disks.
                checker_path = root / 'check-storage'
                checker_path.write_text(script)
                finder = root / 'findmnt'
                finder.write_text('#!/bin/sh\ncase "$*" in *FSTYPE*) echo "$TEST_FS";; *) echo "$TEST_SIZE";; esac\n')
                finder.chmod(0o755)
                for filesystem, size, expected in [('ext4', 75 * 1024**3, 0),
                                                    ('ntfs', 75 * 1024**3, 1),
                                                    ('ext4', 32 * 1024**3, 1),
                                                    ('', '', 1)]:
                    env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'],
                               TEST_FS=filesystem, TEST_SIZE=str(size))
                    checked = subprocess.run(['/bin/sh', str(checker_path)], env=env, capture_output=True)
                    self.assertEqual(checked.returncode, expected)
                data.pop('cockroach_storage')
                data.update(single_node_lab=True, nodes=data['nodes'][:1])
                generate.ignition(data)
                unit = json.loads((root / 'output/kube1.ign').read_text())['systemd']['units'][0]['contents']
                self.assertIn(' start-single-node --certs-dir', unit)
                self.assertNotIn('--join=', unit)
                data['nodes'][0].update(cockroachdb=False, datastore_host='192.168.100.11',
                                        cluster_mac='52:54:00:ca:00:02', ip='192.168.100.12')
                generate.ignition(data)
                doc = json.loads((root / 'output/kube1.ign').read_text())
                files = {f['path']: f for f in doc['storage']['files']}
                self.assertNotIn('/etc/cockroach/certs/node.key', files)
                self.assertIn('/etc/cockroach/certs/client.kine.key', files)
                self.assertNotIn('cockroachdb.service', json.dumps(doc['systemd']))
                decode = lambda path: base64.b64decode(files[path]['contents']['source'].split(',')[1]).decode()
                self.assertIn('postgres://kine@192.168.100.11:26257/', decode('/etc/rancher/k3s/config.yaml'))
                self.assertIn('Address=192.168.100.12/24', decode('/etc/systemd/network/10-lab-cluster.network'))


if __name__ == '__main__':
    unittest.main()
