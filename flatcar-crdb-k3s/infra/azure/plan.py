#!/usr/bin/env python3
"""Resolve a fresh Azure deployment to a pinned, reviewable local plan. No provisioning."""
import argparse
from datetime import datetime, timezone
import json
import math
from pathlib import Path
import subprocess
import urllib.parse
import urllib.request


def az(*args):
    return json.loads(subprocess.check_output(['az', *args, '--output', 'json']))


def prices(location):
    query = f"serviceName eq 'Virtual Machines' and armRegionName eq '{location}' and priceType eq 'Consumption'"
    url = 'https://prices.azure.com/api/retail/prices?' + urllib.parse.urlencode({'$filter': query})
    result = []
    while url:
        with urllib.request.urlopen(url, timeout=60) as response:
            page = json.load(response)
        result.extend(page['Items'])
        url = page.get('NextPageLink')
    return result


def available_zones(sku, location):
    info = next((x for x in sku.get('locationInfo', []) if x['location'].lower() == location.lower()), {})
    zones = set(info.get('zones', []))
    for restriction in sku.get('restrictions', []):
        details = restriction.get('restrictionInfo', {})
        regions = details.get('locations', restriction.get('values', []))
        if regions and location.lower() not in [x.lower() for x in regions]:
            continue
        if restriction['type'] == 'Location':
            return []
        if restriction['type'] == 'Zone':
            zones.difference_update(details.get('zones', []))
    return sorted(zones)


def select_candidates(skus, rates, usage, *, location, node_count, min_vcpus=2,
                      min_memory=8, min_disk=64, include_burstable=True):
    # Leave room for filesystem overhead. Runtime checks verify usable capacity too.
    required_raw_mib = math.ceil(min_disk * 1024 / .90)
    quota = {x['name']['value'].lower(): int(x['limit']) - int(x['currentValue']) for x in usage}
    hourly = {}
    for rate in rates:
        description = ' '.join(rate.get(k, '') for k in ('productName', 'skuName', 'meterName')).lower()
        if (rate.get('type') != 'Consumption' or rate.get('unitOfMeasure') != '1 Hour'
                or rate.get('currencyCode') != 'USD' or rate.get('armRegionName', '').lower() != location.lower()
                or any(word in description for word in ('windows', 'spot', 'low priority'))):
            continue
        name, price = rate['armSkuName'], float(rate['retailPrice'])
        if price > 0:
            hourly[name] = min(price, hourly.get(name, float('inf')))
    candidates = []
    for sku in skus:
        name = sku['name']
        caps = {x['name']: x['value'] for x in sku.get('capabilities', [])}
        cores, memory = int(caps.get('vCPUs', 0)), float(caps.get('MemoryGB', 0))
        zones = available_zones(sku, location)
        disk_mib = int(caps.get('MaxResourceVolumeMB', 0))
        # This profile uses Flatcar WAAgent's SCSI resource-disk handling.
        # NVMe-only temporary storage needs its own tested provisioning profile.
        if (name not in hourly or cores < min_vcpus or memory < min_memory
                or disk_mib < required_raw_mib or len(zones) < 3
                or caps.get('CpuArchitectureType') != 'x64'
                or 'V2' not in caps.get('HyperVGenerations', '').split(',')
                or (not include_burstable and name.startswith('Standard_B'))):
            continue
        needed = cores * node_count
        if needed > quota.get('cores', 0) or needed > quota.get(sku['family'].lower(), 0):
            continue
        candidates.append({'vmSize': name, 'family': sku['family'], 'vcpus': cores,
                           'memoryGiB': memory, 'temporaryDiskGiB': disk_mib / 1024,
                           'availableZones': zones, 'hourlyUSDPerNode': hourly[name],
                           'hourlyUSDCluster': hourly[name] * node_count})
    return sorted(candidates, key=lambda x: (x['hourlyUSDCluster'], x['vmSize']))


def resolve(cluster, candidate, *, name, location, api_cidrs, admin_cidrs):
    cluster = json.loads(json.dumps(cluster))
    servers = sorted(n['name'] for n in cluster['nodes'] if n.get('role', 'server') == 'server')
    workers = sorted(n['name'] for n in cluster['nodes'] if n.get('role', 'server') == 'worker')
    if len(servers) != 3:
        raise ValueError('Exactly three servers are required')
    zones = {node: candidate['availableZones'][i % 3] for i, node in enumerate(servers + workers)}
    hostname = f'{name}-api.{location}.cloudapp.azure.com'
    cluster['tls_sans'] = list(dict.fromkeys([*cluster.get('tls_sans', []), hostname]))
    cluster['cockroach_storage'] = 'azure-temporary'
    cluster['cockroach_min_disk_gib'] = 64
    for node in cluster['nodes']:
        node['locality'] = f'cloud=azure,region={location},zone={zones[node["name"]]}'
        node.pop('cluster_mac', None)
    return cluster, {'name': name, 'location': location, 'vmSize': candidate['vmSize'],
                     'nodeZones': zones, 'apiLoadBalancer': True, 'apiDNSLabel': name + '-api',
                     'apiAdminCIDRs': api_cidrs, 'publicNodeIPs': True, 'adminCIDRs': admin_cidrs,
                     'osDiskSizeGB': 32, 'osDiskStorageAccountType': 'StandardSSD_LRS'}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cluster', type=Path, default=Path('../../cluster.json'))
    parser.add_argument('--location', default='westus2')
    parser.add_argument('--name', required=True)
    parser.add_argument('--ssh-public-key', type=Path, required=True)
    parser.add_argument('--admin-cidr', action='append', required=True)
    parser.add_argument('--api-cidr', action='append', default=None)
    parser.add_argument('--min-vcpus', type=int, default=2)
    parser.add_argument('--min-memory-gib', type=float, default=8)
    parser.add_argument('--min-disk-gib', type=int, default=64)
    parser.add_argument('--exclude-burstable', action='store_true')
    parser.add_argument('--output-dir', type=Path, required=True)
    parser.add_argument('--skus-file', type=Path, help='Previously queried full Azure SKU inventory')
    args = parser.parse_args()
    if args.min_vcpus < 1 or args.min_memory_gib <= 0 or args.min_disk_gib < 1:
        raise SystemExit('CPU, memory and disk minimums must be positive')
    if args.output_dir.exists():
        raise SystemExit('Output directory already exists; refusing to change a pinned deployment plan')
    cluster = json.loads(args.cluster.read_text())
    cluster['ssh_authorized_key'] = args.ssh_public_key.read_text().strip()
    account = az('account', 'show')
    skus = json.loads(args.skus_file.read_text()) if args.skus_file else az('vm', 'list-skus', '--location', args.location, '--resource-type', 'virtualMachines', '--all')
    usage = az('vm', 'list-usage', '--location', args.location)
    candidates = select_candidates(skus, prices(args.location), usage, location=args.location,
                                   node_count=len(cluster['nodes']), min_vcpus=args.min_vcpus,
                                   min_memory=args.min_memory_gib, min_disk=args.min_disk_gib,
                                   include_burstable=not args.exclude_burstable)
    if not candidates:
        raise SystemExit('No compatible three-zone SKU has enough quota, memory, CPU, and usable temporary storage')
    cluster, config = resolve(cluster, candidates[0], name=args.name, location=args.location,
                              api_cidrs=args.api_cidr or args.admin_cidr, admin_cidrs=args.admin_cidr)
    cluster['cockroach_min_disk_gib'] = args.min_disk_gib
    args.output_dir.mkdir(mode=0o700, parents=True)
    metadata = {'createdAt': datetime.now(timezone.utc).isoformat(), 'subscriptionId': account['id'],
                'selected': candidates[0], 'alternatives': candidates[1:6],
                'constraints': {'minVCPUs': args.min_vcpus, 'minMemoryGiB': args.min_memory_gib,
                                'minUsableDiskGiB': args.min_disk_gib, 'spot': False,
                                'includeBurstable': not args.exclude_burstable},
                'pulumiConfig': config}
    for filename, data in [('cluster.json', cluster), ('plan.json', metadata)]:
        path = args.output_dir / filename
        with path.open('x') as stream:
            json.dump(data, stream, indent=2)
            stream.write('\n')
        path.chmod(0o600)
    print(json.dumps(metadata['selected'], indent=2))
    print('Pinned plan saved. No cloud resources were created. Prices exclude disks, networking, and tax.')


if __name__ == '__main__':
    main()
