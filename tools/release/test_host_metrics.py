"""Host observation fixtures; run only on the authorized remote test host."""
import json
import ipaddress
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from host_metrics import (HostMetricsCollector, MAX_READ_BYTES, MAX_NETWORK_INTERFACES,
                          MAX_INTERFACE_ADDRESSES, MAX_STATUS_BYTES, cpu_observation,
                          ipv4_addresses, memory_observation, mount_for_path,
                          network_rates, parse_cpu_stat, parse_mountinfo,
                          parse_network_counters, read_text)


def cpu(user=100, system=100, idle=800, guest=100):
    return 'cpu  %d 0 %d %d 0 0 0 0 %d 0\ncpu0 1 0 0 1\ncpu1 1 0 0 1\n' % (user, system, idle, guest)


def network(rx=1000, tx=2000, name='eth0'):
    return ('Inter-| Receive | Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\n'
            + '%s: %d 1 0 0 0 0 0 0 %d 1 0 0 0 0 0 0\n' % (name, rx, tx))


MOUNTS = ('21 1 8:1 / / rw,relatime - ext4 /dev/sda1 rw\n'
          '22 21 8:2 / /var/lib/rcd rw,relatime - ext4 /dev/sda2 rw\n')


def golden_host_status():
    """Same producer/consumer wire fixture; never replace CIDR with bare IP."""
    file = Path(__file__).resolve().parents[1] / 'ansible/semaphore/custom/files/services/rcd/testdata/host_status_v1.json'
    return json.loads(file.read_text(encoding='utf-8'))


class ParserTests(unittest.TestCase):
    def test_cpu_guest_time_is_not_counted_twice_and_logical_count_is_explicit(self):
        first = parse_cpu_stat(cpu())
        second = parse_cpu_stat(cpu(200, 200, 1000, 200))
        result = cpu_observation(first, second)
        self.assertTrue(result['available'])
        self.assertEqual(result['utilization_percent'], 50)
        self.assertEqual(result['logical_count'], 2)

    def test_cpu_reset_or_no_progress_is_unknown_not_idle_zero(self):
        sample = parse_cpu_stat(cpu())
        for result in (cpu_observation(sample, sample),
                       cpu_observation(sample, parse_cpu_stat(cpu(0, 0, 0))),
                       cpu_observation(None, sample)):
            self.assertFalse(result['available'])
            self.assertIsNone(result['utilization_percent'])
            self.assertTrue(result['error'])

    def test_cpu_old_short_record_can_be_read_but_missing_negative_and_duplicate_rejected(self):
        self.assertEqual(parse_cpu_stat('cpu 1 2 3 4\n')['counters'], [1, 2, 3, 4, 0, 0, 0, 0])
        for text in ('intr 1\n', 'cpu 1 2 3\n', 'cpu -1 0 0 1\n', 'cpu 1 2 3 4\ncpu 1 2 3 4\n'):
            with self.subTest(text=text), self.assertRaises(ValueError):
                parse_cpu_stat(text)

    def test_memory_uses_available_not_just_free_memory(self):
        result = memory_observation('MemTotal: 1000 kB\nMemFree: 10 kB\nMemAvailable: 400 kB\n')
        self.assertEqual(result['total_bytes'], 1024000)
        self.assertEqual(result['available_bytes'], 409600)
        self.assertEqual(result['used_bytes'], 614400)
        self.assertEqual(result['used_percent'], 60)

    def test_memory_missing_available_invalid_units_or_inconsistent_capacity_are_rejected(self):
        for text in ('MemTotal: 1 kB\n', 'MemTotal: 1 B\nMemAvailable: 1 kB\n',
                     'MemTotal: 0 kB\nMemAvailable: 0 kB\n',
                     'MemTotal: 1 kB\nMemAvailable: 2 kB\n',
                     'MemTotal: 1 kB\nMemAvailable: -1 kB\n'):
            with self.subTest(text=text), self.assertRaises(ValueError):
                memory_observation(text)

    def test_network_counts_and_rates_use_the_sampling_interval(self):
        a = parse_network_counters(network())['eth0']
        b = parse_network_counters(network(1100, 2200))['eth0']
        self.assertEqual(network_rates(a, b, 0.2), (500.0, 1000.0, ''))
        self.assertEqual(network_rates(a, a, 0.2), (0.0, 0.0, ''))

    def test_network_missing_interface_reset_and_invalid_clock_never_report_zero_rate(self):
        a = {'rx_bytes': 100, 'tx_bytes': 200}
        b = {'rx_bytes': 1, 'tx_bytes': 300}
        for first, second, elapsed in ((None, a, 0.2), (a, None, 0.2),
                                       (a, b, 0.2), (a, a, 0),
                                       (a, a, float('nan')), (a, a, float('inf'))):
            rx, tx, error = network_rates(first, second, elapsed)
            self.assertIsNone(rx)
            self.assertIsNone(tx)
            self.assertTrue(error)

    def test_network_malformed_negative_duplicate_or_path_names_are_rejected(self):
        for text in ('', network(-1), network() + network(), network(name='../bad')):
            with self.subTest(text=text), self.assertRaises(ValueError):
                parse_network_counters(text)

    def test_mount_info_uses_path_boundaries_and_unescapes_mount_points(self):
        mounts = parse_mountinfo(MOUNTS + '23 21 8:3 / /data\\040disk rw - ext4 /dev/sda3 rw\n')
        self.assertEqual(mount_for_path('/var/lib/rcd/data', mounts)['id'], '22')
        self.assertEqual(mount_for_path('/var/lib/rcd-other', mounts)['id'], '21')
        self.assertEqual(mount_for_path('/data disk/config', mounts)['mount_point'], '/data disk')
        for text in ('', 'broken line\n', '23 21 8:3 / relative rw - ext4 /dev/sda3 rw\n'):
            with self.subTest(text=text), self.assertRaises(ValueError):
                parse_mountinfo(text)

    def test_file_read_is_bounded(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / 'stat'
            path.write_bytes(b'x' * (MAX_READ_BYTES + 1))
            with self.assertRaises(ValueError):
                read_text(path)


class AddressTests(unittest.TestCase):
    def test_ip_command_output_matches_shared_cidr_wire_fixture(self):
        golden = golden_host_status()
        raw = []
        expected = {}
        for interface in golden['network']:
            addresses = []
            for cidr in interface['ipv4_addresses']:
                parsed = ipaddress.IPv4Interface(cidr)
                addresses.append({'family': 'inet', 'local': str(parsed.ip), 'prefixlen': parsed.network.prefixlen})
            raw.append({'ifname': interface['name'], 'addr_info': addresses})
            expected[interface['name']] = interface['ipv4_addresses']
        with patch('host_metrics.os.path.isfile', return_value=True), patch('host_metrics.os.access', return_value=True), \
                patch('host_metrics.subprocess.run', return_value=SimpleNamespace(returncode=0, stdout=json.dumps(raw))):
            actual, error = ipv4_addresses()
        self.assertEqual(error, '')
        self.assertEqual(actual, expected)
        self.assertTrue(all('/' in address for values in actual.values() for address in values))

    def test_ipv4_uses_only_a_fixed_read_only_command_without_shell(self):
        raw = [{'ifname': 'eth0', 'addr_info': [{'family': 'inet', 'local': '192.168.10.46', 'prefixlen': 24},
                                               {'family': 'inet6', 'local': '::1', 'prefixlen': 128}]}]
        with patch('host_metrics.os.path.isfile', return_value=True), patch('host_metrics.os.access', return_value=True), \
                patch('host_metrics.subprocess.run', return_value=SimpleNamespace(returncode=0, stdout=json.dumps(raw))) as run:
            self.assertEqual(ipv4_addresses(), ({'eth0': ['192.168.10.46/24']}, ''))
            args, options = run.call_args
            self.assertEqual(args[0], ['/usr/sbin/ip', '-j', '-4', 'address', 'show'])
            self.assertNotIn('shell', options)
            self.assertEqual(options['timeout'], 2)

    def test_ipv4_missing_command_timeout_and_malformed_output_are_explicitly_unknown(self):
        with patch('host_metrics.os.path.isfile', return_value=False):
            addresses, error = ipv4_addresses()
            self.assertEqual(addresses, {})
            self.assertTrue(error)
        with patch('host_metrics.os.path.isfile', return_value=True), patch('host_metrics.os.access', return_value=True):
            for side_effect, output in ((subprocess.TimeoutExpired('ip', 2), None),
                                         (None, 'broken'), (None, '{}'),
                                         (None, '[{"ifname":"eth0","addr_info":[{"family":"inet","local":"1.2.3.4","prefixlen":true}]}]')):
                with self.subTest(output=output), patch('host_metrics.subprocess.run', side_effect=side_effect,
                                                      return_value=SimpleNamespace(returncode=0, stdout=output)):
                    addresses, error = ipv4_addresses()
                    self.assertEqual(addresses, {})
                    self.assertTrue(error)


class CollectorTests(unittest.TestCase):
    def fixture(self, before=None, after=None, clock=(100.0, 100.2), disk=None):
        first = {'stat': cpu(), 'net/dev': network() + network(name='lo'),
                 'meminfo': 'MemTotal: 1000 kB\nMemAvailable: 400 kB\n',
                 'uptime': '1234.5 500.0\n', 'self/mountinfo': MOUNTS,
                 'sys/class/net/eth0/operstate': 'up\n', 'sys/class/net/eth0/flags': '0x1003\n'}
        first.update(before or {})
        second = dict(first)
        second.update({'stat': cpu(200, 200, 1000), 'net/dev': network(1100, 2200) + network(name='lo')})
        second.update(after or {})
        current = [first]
        times = iter(clock)
        def reader(path):
            key = str(path)
            value = current[0].get(key)
            if value is None:
                raise PermissionError('fixture unavailable')
            return value
        def sleep(_seconds):
            current[0] = second
        default_disk = SimpleNamespace(f_frsize=4096, f_bsize=4096, f_blocks=1000, f_bfree=400, f_bavail=350)
        collector = HostMetricsCollector(proc_root='.', sys_root='sys', reader=reader,
                                        disk_reader=disk or (lambda _path: default_disk),
                                        address_reader=lambda: ({'eth0': ['192.168.10.46/24']}, ''),
                                        sleeper=sleep, clock=lambda: next(times),
                                        timestamp=lambda: '2026-10-21T08:00:00Z')
        return collector

    def test_complete_snapshot_is_json_safe_read_only_and_deduplicates_same_mount(self):
        result = self.fixture().collect(('/', '/var/lib', '/var/lib/rcd', '/var/lib/rcd/data'))
        self.assertTrue(result['available'])
        self.assertEqual(result['error'], '')
        self.assertEqual(result['schema_version'], 1)
        self.assertAlmostEqual(result['sample_interval_seconds'], 0.2)
        self.assertEqual(result['cpu']['utilization_percent'], 50)
        self.assertEqual(result['memory']['used_percent'], 60)
        self.assertEqual(result['uptime_seconds'], 1234.5)
        self.assertEqual([v['mount_point'] for v in result['disks']], ['/', '/var/lib/rcd'])
        self.assertEqual(result['disks'][0]['total_bytes'], 4096000)
        self.assertEqual(result['disks'][0]['used_percent'], 60)
        self.assertEqual([v['name'] for v in result['network']], ['eth0'])
        nic = result['network'][0]
        self.assertTrue(nic['available'])
        self.assertTrue(nic['link_up'])
        self.assertEqual(nic['ipv4_addresses'], ['192.168.10.46/24'])
        self.assertAlmostEqual(nic['rx_bytes_per_second'], 500, places=2)
        json.dumps(result, allow_nan=False)

    def test_full_snapshot_matches_shared_go_consumer_wire_fixture(self):
        golden = golden_host_status()
        collector = self.fixture()
        collector.timestamp = lambda: golden['observed_at']
        addresses = {interface['name']: interface['ipv4_addresses'] for interface in golden['network']}
        collector.addresses = lambda: (addresses, '')
        result = collector.collect(tuple(disk['path'] for disk in golden['disks']))
        # Full structural equality covers embedded disk/memory fields, boolean
        # availability, timestamps, counters/rates and IPv4 CIDR representation.
        self.assertEqual(result, golden)
        json.dumps(result, allow_nan=False)

    def test_many_interfaces_are_bounded_without_discarding_cpu_or_memory(self):
        first = ''.join(network(name='eth%03d' % i) for i in range(MAX_NETWORK_INTERFACES + 1))
        second = ''.join(network(1100, 2200, 'eth%03d' % i) for i in range(MAX_NETWORK_INTERFACES + 1))
        result = self.fixture(before={'net/dev': first}, after={'net/dev': second}).collect(('/',))
        self.assertEqual(len(result['network']), MAX_NETWORK_INTERFACES)
        self.assertIn('interface limit exceeded', result['error'])
        self.assertTrue(result['available'])
        self.assertTrue(result['cpu']['available'])
        self.assertTrue(result['memory']['available'])
        json.dumps(result, allow_nan=False)

    def test_many_ipv4_addresses_keep_a_bounded_explicit_partial_observation(self):
        collector = self.fixture()
        addresses = ['192.0.2.%d/32' % i for i in range(1, MAX_INTERFACE_ADDRESSES + 2)]
        collector.addresses = lambda: ({'eth0': addresses}, '')
        result = collector.collect(('/',))
        self.assertEqual(result['network'][0]['ipv4_addresses'], addresses[:MAX_INTERFACE_ADDRESSES])
        self.assertIn('address limit exceeded', result['network'][0]['addresses_error'])
        self.assertIn('address limit exceeded', result['error'])
        self.assertTrue(result['cpu']['available'])
        self.assertTrue(result['memory']['available'])

    def test_combined_maximum_interfaces_and_addresses_fit_the_consumer_wire_limit(self):
        names = ['eth%03d' % i for i in range(MAX_NETWORK_INTERFACES)]
        first = ''.join(network(name=name) for name in names)
        second = ''.join(network(1100, 2200, name) for name in names)
        collector = self.fixture(before={'net/dev': first}, after={'net/dev': second})
        collector.addresses = lambda: ({name: ['198.251.255.%d/32' % i for i in range(1, MAX_INTERFACE_ADDRESSES + 1)] for name in names}, '')
        result = collector.collect(('/',))
        self.assertLessEqual(len(json.dumps(result, allow_nan=False).encode('utf-8')), MAX_STATUS_BYTES)
        self.assertIn('size limit exceeded', result['error'])
        self.assertLess(len(result['network']), MAX_NETWORK_INTERFACES)
        self.assertTrue(result['memory']['available'])
        self.assertTrue(result['cpu']['available'])

    def test_suspended_sample_keeps_gauges_but_does_not_report_invalid_network_rates(self):
        result = self.fixture(clock=(100.0, 161.0)).collect(('/',))
        self.assertIsNone(result['sample_interval_seconds'])
        self.assertIsNone(result['network'][0]['rx_bytes_per_second'])
        self.assertIsNone(result['network'][0]['tx_bytes_per_second'])
        self.assertIn('exceeded 60 seconds', result['error'])
        self.assertTrue(result['memory']['available'])
        self.assertTrue(result['disks'][0]['available'])

    def test_interface_removal_preserves_name_but_never_reuses_old_counters_or_rates(self):
        result = self.fixture(after={'net/dev': network(name='lo')}).collect(('/',))
        nic = result['network'][0]
        self.assertEqual(nic['name'], 'eth0')
        self.assertFalse(nic['available'])
        self.assertIsNone(nic['rx_bytes'])
        self.assertIsNone(nic['tx_bytes'])
        self.assertIsNone(nic['rx_bytes_per_second'])
        self.assertIsNone(nic['tx_bytes_per_second'])
        self.assertTrue(nic['error'])

    def test_new_interface_has_current_counters_but_unknown_rate(self):
        result = self.fixture(before={'net/dev': network(name='lo')}).collect(('/',))
        nic = result['network'][0]
        self.assertTrue(nic['available'])
        self.assertEqual(nic['rx_bytes'], 1100)
        self.assertIsNone(nic['rx_bytes_per_second'])

    def test_network_reset_and_invalid_clock_do_not_fabricate_idle_traffic(self):
        for collector in (self.fixture(after={'net/dev': network(1, 2)}),
                          self.fixture(clock=(100.0, 100.0))):
            result = collector.collect(('/',))
            self.assertIsNone(result['network'][0]['rx_bytes_per_second'])
            self.assertIsNone(result['network'][0]['tx_bytes_per_second'])
            self.assertTrue(result['network'][0]['error'])
            json.dumps(result, allow_nan=False)

    def test_missing_cpu_does_not_discard_memory_disk_and_network(self):
        result = self.fixture(before={'stat': None}, after={'stat': None}).collect(('/',))
        self.assertTrue(result['available'])
        self.assertFalse(result['cpu']['available'])
        self.assertIsNone(result['cpu']['utilization_percent'])
        self.assertTrue(result['memory']['available'])
        self.assertTrue(result['disks'][0]['available'])
        self.assertTrue(result['network'][0]['available'])
        self.assertTrue(result['error'])

    def test_missing_link_state_is_unknown_not_down_or_up(self):
        result = self.fixture(before={'sys/class/net/eth0/operstate': None}).collect(('/',))
        self.assertEqual(result['network'][0]['operstate'], 'unknown')
        self.assertIsNone(result['network'][0]['link_up'])
        self.assertTrue(result['network'][0]['available'])

    def test_all_data_unavailable_is_explicit_and_does_not_raise(self):
        def unavailable(_path):
            raise PermissionError('fixture denied')
        c = self.fixture(disk=unavailable)
        c.read = unavailable
        c.addresses = lambda: ({}, 'addresses unavailable')
        result = c.collect(('/',))
        self.assertFalse(result['available'])
        self.assertTrue(result['error'])
        self.assertIsNone(result['cpu']['utilization_percent'])
        self.assertIsNone(result['memory']['total_bytes'])
        self.assertIsNone(result['disks'][0]['total_bytes'])
        self.assertIsNone(result['uptime_seconds'])
        json.dumps(result, allow_nan=False)

    def test_bad_paths_and_unbounded_or_nan_intervals_return_unknown_without_sampling(self):
        c = self.fixture()
        c.sleep = lambda _value: self.fail('invalid input must not sample')
        for paths, seconds in ((('relative',), 0.2), (('/',), 10), (('/',), float('nan')),
                                (('/',), 0), ('/', 0.2), ((), 0.2), (('/',), True)):
            with self.subTest(paths=paths, seconds=seconds):
                result = c.collect(paths, seconds)
                self.assertFalse(result['available'])
                self.assertTrue(result['error'])
                self.assertIsNone(result['sample_interval_seconds'])

    def test_renamed_loopback_is_filtered_from_interface_list(self):
        c = self.fixture(before={'sys/class/net/eth0/flags': '0x9\n'})
        self.assertEqual(c.collect(('/',))['network'], [])


if __name__ == '__main__':
    unittest.main()
