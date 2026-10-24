#!/usr/bin/env python3
"""Disposable Flatcar lab. All state and secrets live in ./lab/."""
import argparse
import bz2
import hashlib
import json
import os
import shutil
from pathlib import Path
import socket
import subprocess
import sys
import time

HERE = Path(__file__).resolve().parent
LAB = HERE / 'lab'
FLATCAR = '4757.2.2'
BASE = 'flatcar_production_qemu_image.img'


def run(*args, **kwargs):
    return subprocess.run([str(a) for a in args], check=True, **kwargs)


def ssh_args(node='kube1'):
    return ['ssh', '-i', str(LAB / 'id_ed25519'), '-p', str(2221 + int(node[-1])),
            '-o', 'IdentitiesOnly=yes', '-o', 'StrictHostKeyChecking=accept-new',
            '-o', f'UserKnownHostsFile={LAB / "known_hosts"}',
            '-o', 'ConnectTimeout=5', 'core@127.0.0.1']


def prepare():
    LAB.mkdir(mode=0o700, exist_ok=True)
    LAB.chmod(0o700)
    if not (LAB / 'id_ed25519').exists():
        run('ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', LAB / 'id_ed25519')
    if (LAB / 'peer-network.json').exists():
        config = json.loads((LAB / 'cluster.json').read_text())
    else:
        config = json.loads((HERE / 'cluster.json').read_text())
        config.update(single_node_lab=True,
                      ssh_authorized_key=(LAB / 'id_ed25519.pub').read_text().strip(),
                      nodes=[{'name': 'kube1', 'ip': '10.0.2.15'}])
    (LAB / 'cluster.json').write_text(json.dumps(config, indent=2) + '\n')
    if not (LAB / 'pki').exists():
        run(sys.executable, HERE / 'generate.py', 'pki', '--config', LAB / 'cluster.json', '--state-dir', LAB)
    run(sys.executable, HERE / 'generate.py', 'ignition', '--config', LAB / 'cluster.json', '--state-dir', LAB)
    archive = LAB / (BASE + '.bz2')
    digests = LAB / (BASE + '.bz2.DIGESTS')
    url = f'https://stable.release.flatcar-linux.net/amd64-usr/{FLATCAR}/'
    for path in (archive, digests):
        if not path.exists():
            run('curl', '-fL', '--retry', '3', '-o', str(path) + '.part', url + path.name)
            Path(str(path) + '.part').rename(path)
    with archive.open('rb') as src:
        digest = hashlib.file_digest(src, 'sha512').hexdigest()
    if f'{digest}  {archive.name}' not in digests.read_text():
        raise RuntimeError('Flatcar image SHA512 mismatch')
    image = LAB / BASE
    if not image.exists():
        with bz2.open(archive, 'rb') as src, Path(str(image) + '.part').open('wb') as dst:
            shutil.copyfileobj(src, dst)
        Path(str(image) + '.part').rename(image)
    print('Prepared lab; Flatcar SHA512 verified.')


def start(node='kube1'):
    state = LAB if node == 'kube1' else LAB / node
    if (state / 'qemu.pid').exists():
        raise RuntimeError('qemu.pid exists; inspect the running VM before starting another')
    disk = state / 'node.qcow2'
    if not disk.exists():
        run('qemu-img', 'create', '-f', 'qcow2', '-F', 'qcow2', '-b', LAB / BASE, disk, '40G')
    index = int(node[-1])
    forwards = f'hostfwd=tcp:127.0.0.1:{2221 + index}-:22,hostfwd=tcp:127.0.0.1:{6442 + index}-:6443'
    if node == 'kube1':
        forwards += ',hostfwd=tcp:127.0.0.1:26257-:26257'
    network = []
    if (LAB / 'peer-network.json').exists():
        settings = json.loads((LAB / 'peer-network.json').read_text())
        if settings.get('backend') == 'multicast':
            backend = f'socket,id=cluster,mcast={settings["group"]}:{settings["port"]},localaddr=127.0.0.1'
        else:
            mode = 'listen' if node == 'kube1' else 'connect'
            backend = f'socket,id=cluster,{mode}=127.0.0.1:{settings["port"]}'
        network = ['-netdev', backend, '-device',
                   f'virtio-net-pci,id=cluster-nic,netdev=cluster,mac={settings[node]["cluster_mac"]}']
    run('qemu-system-x86_64', '-name', f'infestation-{node}', '-enable-kvm', '-cpu', 'host',
        '-smp', '4' if node == 'kube1' else '2', '-m', '6144' if node == 'kube1' else '3072',
        '-drive', f'if=virtio,format=qcow2,file={disk}',
        '-fw_cfg', f'name=opt/org.flatcar-linux/config,file={state / "output" / (node + ".ign")}',
        '-netdev', f'user,id=eth0,{forwards}',
        '-device', f'virtio-net-pci,netdev=eth0,mac=52:54:00:12:34:{0x55 + index:02x}',
        *network, '-display', 'none', '-serial', f'file:{state / "console.log"}',
        '-monitor', f'unix:{state / "monitor.sock"},server=on,wait=off',
        '-pidfile', state / 'qemu.pid', '-daemonize')


def monitor(command, node='kube1'):
    state = LAB if node == 'kube1' else LAB / node
    with socket.socket(socket.AF_UNIX) as conn:
        conn.settimeout(10)
        conn.connect(str(state / 'monitor.sock'))
        def response():
            data = b''
            while b'(qemu) ' not in data:
                block = conn.recv(4096)
                if not block:
                    break
                data += block
            return data.decode(errors='replace')
        response()
        conn.sendall(command.encode() + b'\n')
        result = response()
        if 'Error' in result or 'failed' in result:
            raise RuntimeError(result)
        return result


def add_server():
    """Migrate kube1 onto a private link; provision kube2 against its existing DB."""
    import base64
    import generate
    if (LAB / 'kube2/qemu.pid').exists():
        raise RuntimeError('kube2 is already running; use ssh2 to inspect it')
    settings = {'port': 19540,
                'kube1': {'ip': '192.168.100.11', 'cluster_mac': '52:54:00:ca:00:01'},
                'kube2': {'ip': '192.168.100.12', 'cluster_mac': '52:54:00:ca:00:02'}}
    config = json.loads((LAB / 'cluster.json').read_text())
    config['nodes'][0].update(settings['kube1'])
    generate.PKI = LAB / 'pki'
    # Reissue kube1's node certificate with the private-link IP, preserving the old pair.
    stage = generate.PKI / 'kube1-lan'
    stage.mkdir(mode=0o700, exist_ok=True)
    shutil.copyfile(generate.PKI / 'ca.crt', stage / 'ca.crt')
    if not (stage / 'node.key').exists():
        run(*generate.docker_cmd(config, 'cert', 'create-node', '192.168.100.11',
                                '10.0.2.15', 'kube1', 'localhost', '127.0.0.1',
                                '--certs-dir=/pki/kube1-lan', '--ca-key=/pki/ca.key'))
    original = generate.PKI / 'kube1'
    for name in ('node.crt', 'node.key'):
        backup = original / (name + '.before-private-link')
        if not backup.exists():
            shutil.copyfile(original / name, backup)
            backup.chmod(0o600)
        shutil.copyfile(stage / name, original / name)
        (original / name).chmod(0o600)
    peer = LAB / 'kube2'
    peer.mkdir(mode=0o700, exist_ok=True)
    certs = generate.PKI / 'kube2'
    certs.mkdir(mode=0o700, exist_ok=True)
    shutil.copyfile(generate.PKI / 'ca.crt', certs / 'ca.crt')
    if not (certs / 'client.kine.key').exists():
        run(*generate.docker_cmd(config, 'cert', 'create-client', 'kine',
                                '--certs-dir=/pki/kube2', '--ca-key=/pki/ca.key'))
    second = dict(config, nodes=[dict(name='kube2', cockroachdb=False,
                                     datastore_host=settings['kube1']['ip'], **settings['kube2'])])
    (peer / 'cluster.json').write_text(json.dumps(second, indent=2) + '\n')
    generate.OUT = peer / 'output'
    generate.ignition(second)
    generate.OUT = LAB / 'output'
    generate.ignition(config)
    doc = json.loads((LAB / 'output/kube1.ign').read_text())
    selected = {'/etc/systemd/network/10-lab-cluster.network',
                '/etc/cockroach/certs/node.crt', '/etc/cockroach/certs/node.key',
                '/etc/rancher/k3s/config.yaml'}
    for file in doc['storage']['files']:
        if file['path'] not in selected:
            continue
        content = base64.b64decode(file['contents']['source'].split(',')[1])
        uid = file.get('user', {}).get('id', 0)
        command = (f"sudo sh -c 'umask 077; cat > {file['path']}; "
                   f"chown {uid}:0 {file['path']}; chmod {file['mode']:o} {file['path']}'")
        run(*ssh_args(), command, input=content)
    unit = doc['systemd']['units'][0]['contents']
    run(*ssh_args(), 'sudo tee /etc/systemd/system/cockroachdb.service >/dev/null', input=unit.encode())
    run(*ssh_args(), 'sudo networkctl reload')
    (LAB / 'peer-network.json').write_text(json.dumps(settings, indent=2) + '\n')
    if 'cluster-nic' not in monitor('info network'):
        monitor(f'netdev_add socket,id=cluster,listen=127.0.0.1:{settings["port"]}')
        monitor(f'device_add virtio-net-pci,id=cluster-nic,netdev=cluster,mac={settings["kube1"]["cluster_mac"]}')
    (LAB / 'cluster.json').write_text(json.dumps(config, indent=2) + '\n')
    run(*ssh_args(), 'sudo systemctl daemon-reload; sudo systemctl restart cockroachdb; sudo systemctl restart k3s')
    start('kube2')
    run(*ssh_args(), 'sudo k3s kubectl rollout restart -n kube-system daemonset/cilium daemonset/cilium-envoy deployment/cilium-operator')
    print('kube2 booting; SSH is on localhost:2223, API on localhost:6444.')


def sql(statement, capture=False):
    return db_cli('sql', '--format=csv' if capture else '--format=tsv',
                  '--execute', statement, capture=capture)


def db_cli(*args, capture=False):
    config = json.loads((LAB / 'cluster.json').read_text())
    return run('docker', 'run', '--rm', '--network', 'host', '--user',
        f'{os.getuid()}:{os.getgid()}',
        '-v', f'{LAB / "pki/admin"}:/certs:ro', '--entrypoint=/cockroach/cockroach',
        config['cockroach_image'], *args, '--certs-dir=/certs', '--host=127.0.0.1:26257',
        capture_output=capture)


def apply_guest(node, doc):
    """Apply only the generated configuration needed for an in-place migration."""
    import base64
    for file in doc['storage']['files']:
        if not file['path'].startswith(('/etc/cockroach/', '/etc/rancher/k3s/config', '/etc/systemd/network/')):
            continue
        uid = file.get('user', {}).get('id', 0)
        parent = str(Path(file['path']).parent)
        command = (f"sudo sh -c 'install -d {parent}; umask 077; cat > {file['path']}; "
                   f"chown {uid}:0 {file['path']}; chmod {file['mode']:o} {file['path']}'")
        run(*ssh_args(node), command, input=base64.b64decode(file['contents']['source'].split(',')[1]))
    for unit in doc['systemd']['units']:
        if unit.get('contents'):
            run(*ssh_args(node), f'sudo tee /etc/systemd/system/{unit["name"]} >/dev/null',
                input=unit['contents'].encode())
        for dropin in unit.get('dropins', []):
            directory = f'/etc/systemd/system/{unit["name"]}.d'
            run(*ssh_args(node), f'sudo mkdir -p {directory}; sudo tee {directory}/{dropin["name"]} >/dev/null',
                input=dropin['contents'].encode())
    run(*ssh_args(node), 'sudo systemctl daemon-reload; sudo systemctl enable cockroachdb; sudo networkctl reload')


def expand_three():
    import generate
    if (LAB / 'kube3/qemu.pid').exists():
        raise RuntimeError('kube3 is already running; inspect it with ssh3')
    config = json.loads((LAB / 'cluster.json').read_text())
    config.update(single_node_lab=False, cockroach_cache='128MiB', cockroach_sql_memory='128MiB')
    config['nodes'] = [dict(name=f'kube{i}', ip=f'192.168.100.{10+i}',
                            cluster_mac=f'52:54:00:ca:00:{i:02x}') for i in range(1, 4)]
    generate.PKI = LAB / 'pki'
    generate.OUT = LAB / 'output'
    for node in config['nodes']:
        certs = generate.PKI / node['name']
        certs.mkdir(mode=0o700, exist_ok=True)
        shutil.copyfile(generate.PKI / 'ca.crt', certs / 'ca.crt')
        if not (certs / 'node.key').exists():
            run(*generate.docker_cmd(config, 'cert', 'create-node', node['ip'], node['name'],
                                    'localhost', '127.0.0.1', f'--certs-dir=/pki/{node["name"]}', '--ca-key=/pki/ca.key'))
        if not (certs / 'client.kine.key').exists():
            run(*generate.docker_cmd(config, 'cert', 'create-client', 'kine',
                                    f'--certs-dir=/pki/{node["name"]}', '--ca-key=/pki/ca.key'))
    generate.ignition(config)
    for node in config['nodes']:
        state = LAB if node['name'] == 'kube1' else LAB / node['name']
        state.mkdir(mode=0o700, exist_ok=True)
        (state / 'output').mkdir(mode=0o700, exist_ok=True)
        if state != LAB:
            shutil.copyfile(LAB / 'output' / (node['name'] + '.ign'), state / 'output' / (node['name'] + '.ign'))
            (state / 'output' / (node['name'] + '.ign')).chmod(0o600)
        (state / 'cluster.json').write_text(json.dumps(config, indent=2) + '\n')
    settings = dict(backend='multicast', group='239.192.100.1', port=19541,
                    **{n['name']: {'ip': n['ip'], 'cluster_mac': n['cluster_mac']} for n in config['nodes']})
    print('Moving the private NICs onto the loopback multicast bus...', flush=True)
    for node in config['nodes'][:2]:
        name = node['name']
        if 'cluster-bus' not in monitor('info network', name):
            monitor('netdev_add socket,id=cluster-bus,mcast=239.192.100.1:19541,localaddr=127.0.0.1', name)
            monitor('device_del cluster-nic', name)
            for _ in range(50):
                if 'cluster-nic' not in monitor('info network', name):
                    break
                time.sleep(.2)
            else:
                raise RuntimeError(f'{name}: private NIC did not unplug')
            monitor('netdev_del cluster', name)
            monitor(f'device_add virtio-net-pci,id=cluster-nic,netdev=cluster-bus,mac={node["cluster_mac"]}', name)
        doc = json.loads((LAB / 'output' / (name + '.ign')).read_text())
        apply_guest(name, doc)
    (LAB / 'peer-network.json').write_text(json.dumps(settings, indent=2) + '\n')
    print('Restarting the existing database with multi-node start, preserving its store...', flush=True)
    run(*ssh_args(), 'sudo systemctl restart cockroachdb')
    run(*ssh_args('kube2'), 'sudo systemctl start cockroachdb')
    start('kube3')
    print('kube3 booting; restarting kube1/kube2 K3s against their local SQL endpoints...', flush=True)
    for node in ('kube1', 'kube2'):
        run(*ssh_args(node), 'sudo systemctl restart k3s')
    run(*ssh_args(), 'sudo k3s kubectl rollout restart -n kube-system daemonset/cilium daemonset/cilium-envoy deployment/cilium-operator')
    print('Wait for all three DB nodes, then run restore-replication and cross-smoke.', flush=True)


def restore_replication():
    import csv
    import io
    import re
    status = db_cli('node', 'status', '--format=csv', capture=True)
    nodes = list(csv.DictReader(io.StringIO(status.stdout.decode())))
    if len(nodes) != 3 or any(n.get('is_live') != 'true' for n in nodes):
        raise RuntimeError('Refusing to restore replication until exactly three database nodes are live')
    before = sql('SHOW ALL ZONE CONFIGURATIONS;', capture=True).stdout
    backup = LAB / 'replication-before.csv'
    if not backup.exists():
        backup.write_bytes(before)
        backup.chmod(0o600)
    statements = []
    for row in csv.DictReader(io.StringIO(before.decode())):
        raw = row['raw_config_sql']
        if re.search(r'\bnum_replicas = 1\b', raw):
            statements.append(re.sub(r'\bnum_replicas = 1\b', 'num_replicas = 3', raw).rstrip(';') + ';')
    if statements:
        sql('\n'.join(statements))
    sql('SHOW ALL ZONE CONFIGURATIONS;')


def quorum_smoke():
    """Verify Kine writes survive loss of kube1's SQL process, then restore it."""
    import csv
    import io
    status = db_cli('node', 'status', '--ranges', '--format=csv', capture=True)
    nodes = list(csv.DictReader(io.StringIO(status.stdout.decode())))
    if len(nodes) != 3 or any(n.get('is_live') != 'true' or
                             n.get('ranges_underreplicated') != '0' or
                             n.get('ranges_unavailable') != '0' for n in nodes):
        raise RuntimeError('Quorum test requires three live, fully replicated database nodes')
    try:
        run(*ssh_args(), 'sudo systemctl stop cockroachdb')
        print('kube1 CockroachDB stopped; testing API writes through kube2...', flush=True)
        run(*ssh_args('kube2'), 'sudo k3s kubectl --request-timeout=45s create configmap lab-quorum-smoke '
            '--from-literal=value=three-node-majority --dry-run=client -o yaml | '
            'sudo k3s kubectl --request-timeout=45s apply -f -')
        result = run(*ssh_args('kube3'), 'sudo k3s kubectl --request-timeout=45s get configmap lab-quorum-smoke '
                     '-o jsonpath="{.data.value}"', capture_output=True)
        if result.stdout != b'three-node-majority':
            raise RuntimeError('Quorum test read did not match the write')
        print('PASS: API write on kube2 and read on kube3 while kube1 SQL was stopped.', flush=True)
    finally:
        run(*ssh_args(), 'sudo systemctl start cockroachdb')
        print('kube1 CockroachDB started again.', flush=True)


def kubeconfig(node='kube1'):
    result = run(*ssh_args(node), 'sudo cat /etc/rancher/k3s/k3s.yaml', capture_output=True)
    content = result.stdout
    path = LAB / ('kubeconfig' if node == 'kube1' else f'kubeconfig-{node}')
    if node != 'kube1':
        content = content.replace(b'127.0.0.1:6443', f'127.0.0.1:{6442 + int(node[-1])}'.encode())
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, 'wb') as dst:
        dst.write(content)
    path.chmod(0o600)
    print(f'Kubeconfig: {path}')


def cross_smoke():
    from itertools import permutations
    namespace = 'lab-cross'
    settings = json.loads((LAB / 'peer-network.json').read_text())
    nodes = sorted(key for key in settings if key.startswith('kube'))
    docs = [{'apiVersion': 'v1', 'kind': 'Namespace', 'metadata': {'name': namespace}}]
    for node in nodes:
        labels = {'app': 'lab-cross-web', 'server': node}
        metadata = {'name': 'web-' + node, 'namespace': namespace, 'labels': labels}
        docs.append({'apiVersion': 'v1', 'kind': 'Pod', 'metadata': metadata,
                     'spec': {'nodeName': node, 'containers': [{
                         'name': 'web', 'image': 'busybox:1.37.0',
                         'command': ['sh', '-ec', f'mkdir -p /www; echo {node} > /www/index.html; exec httpd -f -p 8080 -h /www'],
                         'readinessProbe': {'httpGet': {'path': '/', 'port': 8080}},
                         'resources': {'requests': {'cpu': '10m', 'memory': '16Mi'}, 'limits': {'memory': '64Mi'}}}]}})
        docs.append({'apiVersion': 'v1', 'kind': 'Service', 'metadata': metadata,
                     'spec': {'selector': labels, 'ports': [{'port': 8080, 'targetPort': 8080}]}})
    run(*ssh_args(), 'sudo k3s kubectl apply -f -', input=json.dumps({'apiVersion': 'v1', 'kind': 'List', 'items': docs}).encode())
    run(*ssh_args(), f'sudo k3s kubectl wait -n {namespace} --for=condition=Ready pod -l app=lab-cross-web --timeout=45s')
    jobs = []
    for node, other in permutations(nodes, 2):
        jobs.append({'apiVersion': 'batch/v1', 'kind': 'Job',
                     'metadata': {'name': f'from-{node}-to-{other}', 'namespace': namespace},
                     'spec': {'backoffLimit': 3, 'template': {'spec': {
                         'nodeName': node, 'restartPolicy': 'Never', 'containers': [{
                             'name': 'check', 'image': 'busybox:1.37.0',
                             'command': ['sh', '-ec', 'nslookup kubernetes.default.svc.cluster.local; '
                                         f'test "$(wget -qO- -T 15 http://web-{other}.{namespace}.svc.cluster.local:8080)" = {other}; '
                                         f'echo "PASS: {node} to {other}, DNS and cross-node service HTTP"'],
                             'resources': {'requests': {'cpu': '10m', 'memory': '16Mi'}, 'limits': {'memory': '64Mi'}}}]}}}})
    run(*ssh_args(), 'sudo k3s kubectl apply -f -', input=json.dumps({'apiVersion': 'v1', 'kind': 'List', 'items': jobs}).encode())
    for node in nodes:
        run(*ssh_args(), f'sudo k3s kubectl wait --for=condition=Ready node/{node} --timeout=45s')
    for node, other in permutations(nodes, 2):
        job = f'from-{node}-to-{other}'
        run(*ssh_args(), f'sudo k3s kubectl wait -n {namespace} --for=condition=complete job/{job} --timeout=180s')
        run(*ssh_args(), f'sudo k3s kubectl logs -n {namespace} -l job-name={job} --all-containers --prefix')


def cilium(args):
    import tarfile
    version = 'v0.20.1'
    archive = LAB / 'cilium-linux-amd64.tar.gz'
    sums = LAB / 'cilium-linux-amd64.tar.gz.sha256sum'
    for path in (archive, sums):
        if not path.exists():
            run('curl', '-fL', '--retry', '3', '-o', str(path) + '.part',
                f'https://github.com/cilium/cilium-cli/releases/download/{version}/{path.name}')
            Path(str(path) + '.part').rename(path)
    with archive.open('rb') as src:
        digest = hashlib.file_digest(src, 'sha256').hexdigest()
    if digest != sums.read_text().split()[0]:
        raise RuntimeError('Cilium CLI SHA256 mismatch')
    with tarfile.open(archive) as src:
        src.extract('cilium', LAB, filter='data')
    run(LAB / 'cilium', '--kubeconfig', LAB / 'kubeconfig', *args)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['prepare', 'start', 'start2', 'start3', 'ssh', 'ssh2', 'ssh3', 'sql', 'db-status', 'bootstrap-db', 'kubeconfig', 'kubeconfig2', 'kubeconfig3', 'cilium', 'smoke', 'cross-smoke', 'quorum-smoke', 'stop', 'stop2', 'stop3', 'add-server', 'expand-three', 'restore-replication'])
    parser.add_argument('args', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.command == 'prepare':
        prepare()
    elif args.command == 'start':
        start()
    elif args.command in ('start2', 'start3'):
        start('kube' + args.command[-1])
    elif args.command == 'ssh':
        run(*ssh_args(), *args.args)
    elif args.command in ('ssh2', 'ssh3'):
        run(*ssh_args('kube' + args.command[-1]), *args.args)
    elif args.command == 'add-server':
        add_server()
    elif args.command == 'expand-three':
        expand_three()
    elif args.command == 'restore-replication':
        restore_replication()
    elif args.command == 'quorum-smoke':
        quorum_smoke()
    elif args.command == 'db-status':
        db_cli('node', 'status', '--ranges')
    elif args.command == 'sql':
        sql(' '.join(args.args))
    elif args.command == 'bootstrap-db':
        # start-single-node initializes itself. Never run cockroach init here.
        sql((HERE / 'bootstrap.sql').read_text())
    elif args.command == 'kubeconfig':
        kubeconfig()
    elif args.command in ('kubeconfig2', 'kubeconfig3'):
        kubeconfig('kube' + args.command[-1])
    elif args.command == 'cross-smoke':
        cross_smoke()
    elif args.command == 'cilium':
        cilium(args.args)
    elif args.command == 'smoke':
        run(*ssh_args(), 'sudo k3s kubectl apply -f -', input=(HERE / 'smoke.yaml').read_bytes())
        run(*ssh_args(), 'sudo k3s kubectl wait --for=condition=Ready node/kube1 --timeout=45s')
        run(*ssh_args(), 'sudo k3s kubectl rollout status -n lab-smoke deployment/web --timeout=180s')
        run(*ssh_args(), 'sudo k3s kubectl wait -n lab-smoke --for=condition=complete job/dns-http --timeout=180s')
        run(*ssh_args(), 'sudo k3s kubectl logs -n lab-smoke -l job-name=dns-http --all-containers --prefix')
    elif args.command in ('stop', 'stop2', 'stop3'):
        # Ask this VM to shut down gracefully; never signal an unchecked PID.
        monitor('system_powerdown', 'kube1' if args.command == 'stop' else 'kube' + args.command[-1])


if __name__ == '__main__':
    main()
