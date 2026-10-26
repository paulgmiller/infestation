import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('azure_plan', Path(__file__).with_name('plan.py'))
plan = importlib.util.module_from_spec(spec)
spec.loader.exec_module(plan)


def sku(name, *, disk=76800, memory=8, cpu=2, zones=None, family='family', arch='x64'):
    return {'name': name, 'family': family, 'locationInfo': [{'location': 'westus2', 'zones': zones or ['1','2','3']}],
            'restrictions': [], 'capabilities': [{'name': k, 'value': str(v)} for k,v in
                {'MaxResourceVolumeMB': disk, 'MemoryGB': memory, 'vCPUs': cpu,
                 'HyperVGenerations': 'V1,V2', 'CpuArchitectureType': arch}.items()]}


def rate(name, price, description='Linux', sku_name='regular'):
    return {'armSkuName': name, 'retailPrice': price, 'productName': description, 'skuName': sku_name,
            'meterName': '', 'type': 'Consumption', 'unitOfMeasure': '1 Hour',
            'currencyCode': 'USD', 'armRegionName': 'westus2'}


class PlanTests(unittest.TestCase):
    def test_filters_prices_capacity_zones_and_quota(self):
        skus = [sku('good'), sku('expensive'), sku('small', disk=65536), sku('noquota', family='blocked'),
                sku('arm', arch='Arm64'), sku('twozones', zones=['1','2'])]
        rates = [rate(x['name'], .01) for x in skus if x['name'] not in ('good','expensive')]
        rates += [rate('good', .113), rate('good', .001, sku_name='Spot'),
                  rate('good', .002, description='Windows'), rate('expensive', .2)]
        usage = [{'name': {'value': k}, 'limit': v, 'currentValue': 0}
                 for k,v in [('cores',10),('family',10),('blocked',0)]]
        result = plan.select_candidates(skus, rates, usage, location='westus2', node_count=3)
        self.assertEqual([x['vmSize'] for x in result], ['good','expensive'])
        self.assertAlmostEqual(result[0]['hourlyUSDCluster'], .339)
        usage[0]['currentValue'] = 5
        self.assertEqual(plan.select_candidates(skus,rates,usage,location='westus2',node_count=3), [])

    def test_restricted_zone_and_stable_server_placement(self):
        item = sku('good')
        item['restrictions'] = [{'type':'Zone','restrictionInfo':{'locations':['westus2'],'zones':['2']}}]
        self.assertEqual(plan.available_zones(item,'westus2'), ['1','3'])
        candidate = {'vmSize':'good','availableZones':['1','2','3']}
        nodes = [{'name': n, 'ip': f'192.168.1.{i+101}'} for i,n in enumerate(['kube3','kube1','kube2'])]
        cluster, cfg = plan.resolve({'nodes':nodes},candidate,name='test',location='westus2',api_cidrs=['0.0.0.0/0'],admin_cidrs=['192.0.2.1/32'])
        self.assertEqual(cfg['nodeZones'], {'kube1':'1','kube2':'2','kube3':'3'})
        self.assertEqual(cluster['nodes'][0]['locality'], 'cloud=azure,region=westus2,zone=3')
        self.assertEqual(cluster['cockroach_storage'], 'azure-temporary')
        self.assertIn('test-api.westus2.cloudapp.azure.com', cluster['tls_sans'])


if __name__ == '__main__':
    unittest.main()
