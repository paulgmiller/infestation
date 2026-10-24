#!/usr/bin/env python3
"""Generate per-node Flatcar Ignition v3.3 configs for host CRDB + K3s.

Run: python3 generate.py pki       # requires local Docker
     python3 generate.py ignition  # needs certificates in ./pki/
"""

import argparse
import base64
import ipaddress
import json
import os
import re
from pathlib import Path
import secrets
import subprocess

HERE = Path(__file__).resolve().parent
CONFIG = HERE / 'cluster.json'
PKI = HERE / 'pki'
OUT = HERE / 'output'


def config():
    data = json.loads(CONFIG.read_text())
    nodes = data['nodes']
    count = 1 if data.get('single_node_lab', False) else 3
    if len(nodes) != count or len({x['name'] for x in nodes}) != count or len({x['ip'] for x in nodes}) != count:
        raise ValueError(f'Exactly {count} distinct nodes are expected')
    for n in nodes:
        ipaddress.ip_address(n['ip'])
        if 'datastore_host' in n:
            ipaddress.ip_address(n['datastore_host'])
        if not n.get('cockroachdb', True) and 'datastore_host' not in n:
            raise ValueError('Nodes without CockroachDB require datastore_host')
        if 'cluster_mac' in n and not re.fullmatch(r'(?:[0-9a-f]{2}:){5}[0-9a-f]{2}', n['cluster_mac']):
            raise ValueError('Invalid cluster_mac')
        if not re.fullmatch(r'[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?', n['name']):
            raise ValueError('Node names must be DNS-label-compatible')
    if not data['ssh_authorized_key'].startswith(('ssh-ed25519 AAAA', 'ssh-rsa AAAA', 'ecdsa-sha2-nistp256 AAAA')):
        raise ValueError('Replace ssh_authorized_key with your real SSH public key')
    if data['architecture'] not in ('x86-64', 'aarch64'):
        raise ValueError('architecture must be x86-64 or aarch64')
    return data


def docker_cmd(data, *args):
    # Run Cockroach CLI as the invoking host UID to keep private PKI files user-owned.
    return ['docker', 'run', '--rm', '--user', f'{os.getuid()}:{os.getgid()}',
            '-v', f'{PKI}:/pki', '--entrypoint=/cockroach/cockroach',
            data['cockroach_image'], *args]


def make_pki(data):
    if PKI.exists():
        raise RuntimeError('pki/ already exists. Refusing to overwrite any certificates or CA keys.')
    PKI.mkdir(mode=0o700)
    os.chmod(PKI, 0o700)
    (PKI / 'admin').mkdir(mode=0o700)
    for n in data['nodes']:
        (PKI / n['name']).mkdir(mode=0o700)

    subprocess.run(docker_cmd(data, 'cert', 'create-ca', '--certs-dir=/pki',
                              '--ca-key=/pki/ca.key'), check=True)
    ca_cert = (PKI / 'ca.crt').read_bytes()
    for n in data['nodes']:
        dst = PKI / n['name']
        (dst / 'ca.crt').write_bytes(ca_cert)
        subprocess.run(docker_cmd(data, 'cert', 'create-node',
                                  n['ip'], n['name'], 'localhost', '127.0.0.1',
                                  f'--certs-dir=/pki/{n["name"]}',
                                  '--ca-key=/pki/ca.key'), check=True)
        subprocess.run(docker_cmd(data, 'cert', 'create-client', 'kine',
                                  f'--certs-dir=/pki/{n["name"]}',
                                  '--ca-key=/pki/ca.key'), check=True)
    (PKI / 'admin' / 'ca.crt').write_bytes(ca_cert)
    subprocess.run(docker_cmd(data, 'cert', 'create-client', 'root',
                              '--certs-dir=/pki/admin',
                              '--ca-key=/pki/ca.key'), check=True)
    for path in PKI.rglob('*.key'):
        path.chmod(0o600)
    print('Created private CA, per-node node/kine certificates and admin root certificate in pki/.')
    print('Keep ca.key and pki/admin/ offline/backed up; do not upload them to the nodes.')


def contents(value):
    if isinstance(value, str):
        value = value.encode('utf-8')
    return {'source': 'data:;base64,' + base64.b64encode(value).decode('ascii')}


def ignite_file(path, content=None, mode=0o644, *, url=None):
    assert (content is None) != (url is None)
    return {'path': path, 'mode': mode, 'overwrite': True,
            'contents': {'source': url} if url else contents(content)}


def cockroach_unit(node, seeds, image, single_node=False, cache='512MiB', sql_memory='512MiB'):
    ip = node['ip']
    command = 'start-single-node' if single_node else 'start'
    join = '' if single_node else f' --join={seeds}'
    return f'''[Unit]
Description=CockroachDB host datastore
Wants=network-online.target
After=network-online.target docker.service
Requires=docker.service
StartLimitIntervalSec=0

[Service]
Type=simple
TimeoutStartSec=0
TimeoutStopSec=120
ExecStartPre=/usr/bin/install -d -o 1000 -g 1000 -m 0700 /var/lib/cockroach
ExecStartPre=-/usr/bin/docker rm -f cockroachdb
ExecStart=/usr/bin/docker run --rm --name cockroachdb --network host --user 1000:1000 --pull=missing --log-driver=journald --mount type=bind,src=/var/lib/cockroach,dst=/cockroach/cockroach-data --mount type=bind,src=/etc/cockroach/certs,dst=/cockroach/certs,readonly --entrypoint=/cockroach/cockroach {image} {command} --certs-dir=/cockroach/certs --store=/cockroach/cockroach-data --listen-addr=0.0.0.0:26257 --advertise-addr={ip}:26257 --http-addr=127.0.0.1:8080{join} --cache={cache} --max-sql-memory={sql_memory} --logtostderr=INFO
ExecStop=/usr/bin/docker stop --time 90 cockroachdb
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
'''


def ignition(data):
    OUT.mkdir(exist_ok=True)
    OUT.chmod(0o700)
    token_file = PKI / 'k3s-token'
    if not token_file.exists():
        token_file.write_text(secrets.token_hex(32) + '\n')
        token_file.chmod(0o600)
    token = token_file.read_text().strip()
    if len(token) < 16:
        raise ValueError('K3s token looks too short')
    seeds = ','.join(f"{n['ip']}:26257" for n in data['nodes'])
    ext_version = data['k3s_extension_version']
    ext_path = f'https://extensions.flatcar.org/extensions/k3s-{ext_version}-{data["architecture"]}.raw'
    for node in data['nodes']:
        name, ip = node['name'], node['ip']
        nodepki = PKI / name
        certs = {
            'ca.crt': (0o644, 0),
            'node.crt': (0o644, 0),
            'node.key': (0o400, 1000),
            'client.kine.crt': (0o600, 0),
            'client.kine.key': (0o600, 0),
        }
        if not node.get('cockroachdb', True):
            certs.pop('node.crt')
            certs.pop('node.key')
        files = [
            ignite_file('/etc/hostname', f'{name}\n'),
            ignite_file('/etc/flatcar/update.conf', 'REBOOT_STRATEGY=off\n'),
            ignite_file('/etc/extensions/k3s.raw', mode=0o644, url=ext_path),
        ]
        if 'cluster_mac' in node:
            files.append(ignite_file('/etc/systemd/network/10-lab-cluster.network',
                                     cluster_network(node)))
        for certname, (mode, uid) in certs.items():
            f = ignite_file(f'/etc/cockroach/certs/{certname}', (nodepki / certname).read_bytes(), mode)
            if uid:
                f['user'] = {'id': uid}
            files.append(f)
        # Using certificates instead of SQL passwords for Kine's PostgreSQL client.
        k3s_conf = f'''token: "{token}"
node-name: "{name}"
node-ip: "{ip}"
advertise-address: "{ip}"
tls-san:
  - "{ip}"
datastore-endpoint: "postgres://kine@{node.get('datastore_host', '127.0.0.1')}:26257/kubernetes?sslmode=verify-full"
datastore-cafile: /etc/cockroach/certs/ca.crt
datastore-certfile: /etc/cockroach/certs/client.kine.crt
datastore-keyfile: /etc/cockroach/certs/client.kine.key
flannel-backend: none
disable-network-policy: true
disable:
  - traefik
  - servicelb
'''
        files.append(ignite_file('/etc/rancher/k3s/config.yaml', k3s_conf, 0o600))
        doc = {
            'ignition': {'version': '3.3.0'},
            'passwd': {'users': [{'name': 'core', 'sshAuthorizedKeys': [data['ssh_authorized_key']]}]},
            'storage': {
                'files': files,
                'links': [
                    {'path': '/etc/systemd/system/multi-user.target.wants/k3s.service',
                     'target': '/usr/local/lib/systemd/system/k3s.service', 'overwrite': True},
                ],
            },
            'systemd': {'units': [
                {'name': 'cockroachdb.service', 'enabled': True,
                 'contents': cockroach_unit(node, seeds, data['cockroach_image'], data.get('single_node_lab', False),
                                            data.get('cockroach_cache', '512MiB'), data.get('cockroach_sql_memory', '512MiB'))},
                {'name': 'k3s.service', 'dropins': [
                    {'name': '10-cockroach-dependency.conf', 'contents':
                     '[Unit]\nAfter=cockroachdb.service\nWants=cockroachdb.service\n'
                     '[Service]\nRestart=always\nRestartSec=15s\n'},
                ]},
            ]},
        }
        if not node.get('cockroachdb', True):
            doc['systemd']['units'] = [{'name': 'k3s.service', 'dropins': [{
                'name': '10-restart.conf',
                'contents': '[Service]\nRestart=always\nRestartSec=15s\n',
            }]}]
        outpath = OUT / f'{name}.ign'
        outpath.write_text(json.dumps(doc, indent=2) + '\n')
        outpath.chmod(0o600)
        assert json.loads(outpath.read_text())['ignition']['version'] == '3.3.0'
        print(f'Wrote {outpath}')
    print('These Ignition files contain sensitive private keys + K3s token. Keep private.')


def cluster_network(node):
    return (f"[Match]\nMACAddress={node['cluster_mac']}\n\n[Network]\n"
            f"Address={node['ip']}/24\nLinkLocalAddressing=no\nIPv6AcceptRA=no\n")


def main():
    global CONFIG, PKI, OUT
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=('pki', 'ignition'))
    parser.add_argument('--config', type=Path, default=CONFIG)
    parser.add_argument('--state-dir', type=Path, default=HERE)
    args = parser.parse_args()
    CONFIG = args.config.resolve()
    PKI = args.state_dir.resolve() / 'pki'
    OUT = args.state_dir.resolve() / 'output'
    data = config()
    {'pki': make_pki, 'ignition': ignition}[args.command](data)

if __name__ == '__main__':
    main()
