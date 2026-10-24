"""Bounded, read-only Linux host observations for the existing inspection job.

No daemon or third-party dependency is needed. CPU and interface rates use two
samples in one inspection, not the interval since an earlier stored report.
Unknown values are None, never an invented zero. Interface/link state is not a
claim that a remote device or an OPC application is reachable.
"""
import datetime
import ipaddress
import json
import math
import os
from pathlib import Path
import re
import subprocess
import time


SCHEMA_VERSION = 1
MAX_READ_BYTES = 512 * 1024
MAX_NETWORK_INTERFACES = 128
MAX_INTERFACE_ADDRESSES = 32
MAX_STATUS_BYTES = 96 * 1024  # Leave framing headroom below the Go consumer's 128 KiB limit.
DEFAULT_DISK_PATHS = ('/', '/var/lib/rcd')
IP_EXECUTABLES = ('/usr/sbin/ip', '/sbin/ip', '/usr/bin/ip', '/bin/ip')


def utc_timestamp():
    return datetime.datetime.now(datetime.timezone.utc).isoformat().replace('+00:00', 'Z')


def read_text(path):
    with Path(path).open('rb') as stream:
        value = stream.read(MAX_READ_BYTES + 1)
    if len(value) > MAX_READ_BYTES:
        raise ValueError('observation file exceeds read limit')
    return value.decode('utf-8', errors='strict')


def parse_cpu_stat(text):
    """Return non-overlapping CPU counters; guest time is already in user/nice."""
    counters = None
    logical = 0
    for line in text.splitlines():
        fields = line.split()
        if not fields:
            continue
        if fields[0] == 'cpu':
            if counters is not None or len(fields) < 5:
                raise ValueError('invalid aggregate CPU counters')
            counters = [int(v) for v in fields[1:9]]
            if any(v < 0 for v in counters):
                raise ValueError('negative CPU counters')
            counters.extend([0] * (8 - len(counters)))
        elif re.fullmatch(r'cpu\d+', fields[0]):
            logical += 1
    if counters is None:
        raise ValueError('aggregate CPU counters are missing')
    return {'counters': counters, 'logical_count': logical or None}


def cpu_observation(first, second):
    value = {'available': False, 'error': '', 'utilization_percent': None,
             'logical_count': second.get('logical_count') if second else None}
    if first is None or second is None:
        value['error'] = 'CPU sampling unavailable'
        return value
    delta = [b - a for a, b in zip(first['counters'], second['counters'])]
    if len(delta) != 8 or any(v < 0 for v in delta):
        value['error'] = 'CPU counters reset during sampling'
        return value
    total = sum(delta)
    if total <= 0:
        value['error'] = 'CPU counters did not advance'
        return value
    # Idle and iowait are not reported as actively executing CPU work.
    busy = total - delta[3] - delta[4]
    value.update(available=True, utilization_percent=round(100.0 * busy / total, 3))
    return value


def memory_observation(text):
    value = {'available': False, 'error': '', 'total_bytes': None,
             'available_bytes': None, 'used_bytes': None, 'used_percent': None}
    fields = {}
    for line in text.splitlines():
        if ':' not in line:
            continue
        name, raw = line.split(':', 1)
        if name not in ('MemTotal', 'MemAvailable'):
            continue
        parts = raw.split()
        if name in fields or len(parts) != 2 or parts[1] != 'kB':
            raise ValueError('invalid memory observation')
        fields[name] = int(parts[0]) * 1024
    total, available = fields.get('MemTotal'), fields.get('MemAvailable')
    if total is None or available is None or total <= 0 or not 0 <= available <= total:
        raise ValueError('MemTotal/MemAvailable are missing or invalid')
    value.update(available=True, total_bytes=total, available_bytes=available,
                 used_bytes=total - available,
                 used_percent=round(100.0 * (total - available) / total, 3))
    return value


def parse_network_counters(text):
    counters = {}
    for line in text.splitlines():
        if ':' not in line:
            continue
        # Interface names may contain a colon, so split from the statistics side.
        name, raw = line.rsplit(':', 1)
        name = name.strip()
        if not name or name in ('.', '..') or '/' in name or '\x00' in name or len(name) > 64:
            raise ValueError('invalid interface name')
        fields = raw.split()
        if len(fields) < 16 or name in counters:
            raise ValueError('invalid interface counters')
        values = [int(v) for v in fields[:16]]
        if any(v < 0 for v in values):
            raise ValueError('negative interface counters')
        counters[name] = {'rx_bytes': values[0], 'tx_bytes': values[8]}
    if not counters and not ('Inter-' in text and 'Receive' in text):
        raise ValueError('network counters are missing')
    return counters


def network_rates(first, second, elapsed):
    """Never treat a new interface, reset or invalid clock as zero traffic."""
    if first is None or second is None:
        return None, None, 'Interface appeared or disappeared during sampling'
    if elapsed is None or not math.isfinite(elapsed) or elapsed <= 0:
        return None, None, 'Network sample interval is unavailable'
    rx = second['rx_bytes'] - first['rx_bytes']
    tx = second['tx_bytes'] - first['tx_bytes']
    if rx < 0 or tx < 0:
        return None, None, 'Interface counters reset during sampling'
    return round(rx / elapsed, 3), round(tx / elapsed, 3), ''


def _unescape_mount(value):
    # mountinfo uses octal escapes, including spaces and backslashes.
    return re.sub(r'\\([0-7]{3})', lambda m: chr(int(m.group(1), 8)), value)


def parse_mountinfo(text):
    mounts = []
    for line in text.splitlines():
        fields = line.split()
        if '-' not in fields:
            raise ValueError('invalid mount observation')
        separator = fields.index('-')
        if separator < 6 or len(fields) <= separator + 2:
            raise ValueError('incomplete mount observation')
        mount = _unescape_mount(fields[4])
        if not mount.startswith('/'):
            raise ValueError('invalid mount point')
        mounts.append({'id': fields[0], 'mount_point': mount,
                       'filesystem': fields[separator + 1]})
    if not mounts:
        raise ValueError('mount observations are missing')
    return mounts


def mount_for_path(path, mounts):
    matches = [m for m in mounts if path == m['mount_point']
               or path.startswith(m['mount_point'].rstrip('/') + '/')]
    return max(matches, key=lambda item: len(item['mount_point'])) if matches else None


def ipv4_addresses():
    """Run only a fixed system read command; no shell, path or CLI from a client."""
    binary = next((p for p in IP_EXECUTABLES if os.path.isfile(p) and os.access(p, os.X_OK)), None)
    if binary is None:
        return {}, 'System ip command unavailable; IPv4 addresses not confirmed'
    try:
        result = subprocess.run([binary, '-j', '-4', 'address', 'show'],
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                text=True, timeout=2, check=False)
        if result.returncode != 0 or len(result.stdout) > MAX_READ_BYTES:
            raise ValueError('IPv4 address observation failed')
        entries = json.loads(result.stdout)
        if not isinstance(entries, list):
            raise ValueError('invalid IPv4 address observation')
        addresses = {}
        for entry in entries:
            name = entry.get('ifname')
            if not isinstance(name, str):
                raise ValueError('missing IPv4 interface name')
            found = []
            for address in entry.get('addr_info', []):
                if address.get('family') != 'inet':
                    continue
                local = address.get('local')
                prefix = address.get('prefixlen')
                if not isinstance(prefix, int) or isinstance(prefix, bool) or not 0 <= prefix <= 32:
                    raise ValueError('invalid IPv4 prefix')
                found.append(str(ipaddress.IPv4Address(local)) + '/' + str(prefix))
            addresses[name] = sorted(set(found))
        return addresses, ''
    except (OSError, ValueError, TypeError, AttributeError, subprocess.SubprocessError):
        return {}, 'IPv4 addresses could not be read'


class HostMetricsCollector:
    """Dependencies are explicit so fixtures do not inspect the test host."""
    def __init__(self, proc_root='/proc', sys_root='/sys', reader=read_text,
                 disk_reader=os.statvfs, address_reader=ipv4_addresses,
                 sleeper=time.sleep, clock=time.monotonic, timestamp=utc_timestamp):
        self.proc = Path(proc_root)
        self.sys = Path(sys_root)
        self.read = reader
        self.disk = disk_reader
        self.addresses = address_reader
        self.sleep = sleeper
        self.clock = clock
        self.timestamp = timestamp

    def _read(self, relative, parser):
        try:
            return parser(self.read(self.proc / relative)), ''
        except (OSError, ValueError, TypeError, IndexError, OverflowError, UnicodeError):
            return None, 'Unable to read ' + relative + ' host observation'

    def _network(self, first, second, elapsed, first_error, second_error):
        try:
            addresses, address_error = self.addresses()
        except (OSError, ValueError, TypeError, subprocess.SubprocessError):
            addresses, address_error = {}, 'IPv4 addresses could not be read'
        entries = []
        observation_errors = [address_error] if address_error else []
        for name in sorted(set(first or {}) | set(second or {})):
            if name == 'lo':
                continue
            try:
                flags = int(self.read(self.sys / 'class/net' / name / 'flags').strip(), 0)
                if flags & 0x8:  # IFF_LOOPBACK, including a renamed loopback interface.
                    continue
            except (OSError, ValueError, TypeError, UnicodeError):
                pass
            if len(entries) >= MAX_NETWORK_INTERFACES:
                observation_errors.append('Network interface limit exceeded; showing the first 128 interfaces')
                break
            state, link_up = 'unknown', None
            link_error = ''
            try:
                state = self.read(self.sys / 'class/net' / name / 'operstate').strip()
                if state not in ('up', 'down', 'unknown', 'notpresent', 'lowerlayerdown', 'testing', 'dormant'):
                    raise ValueError('unknown interface operstate')
                if state == 'up':
                    link_up = True
                elif state in ('down', 'notpresent', 'lowerlayerdown'):
                    link_up = False
            except (OSError, ValueError, TypeError, UnicodeError):
                state, link_up = 'unknown', None
                link_error = 'Interface link state could not be read'
            current = (second or {}).get(name)
            rx_rate, tx_rate, rate_error = network_rates((first or {}).get(name), current, elapsed)
            error = '; '.join(v for v in (first_error, second_error, rate_error, link_error) if v)
            interface_addresses = addresses.get(name, [])
            interface_address_error = address_error
            if len(interface_addresses) > MAX_INTERFACE_ADDRESSES:
                warning = 'IPv4 address limit exceeded; showing the first 32 addresses'
                interface_addresses = interface_addresses[:MAX_INTERFACE_ADDRESSES]
                interface_address_error = '; '.join(v for v in (address_error, warning) if v)
                observation_errors.append(warning)
            entries.append({'name': name, 'operstate': state, 'link_up': link_up,
                            'ipv4_addresses': interface_addresses, 'addresses_error': interface_address_error,
                            'available': current is not None, 'error': error,
                            'rx_bytes': current['rx_bytes'] if current else None,
                            'tx_bytes': current['tx_bytes'] if current else None,
                            'rx_bytes_per_second': rx_rate, 'tx_bytes_per_second': tx_rate})
        return entries, '; '.join(dict.fromkeys(observation_errors))

    def _disks(self, paths):
        entries, seen = [], set()
        mounts, mount_error = self._read('self/mountinfo', parse_mountinfo)
        for raw in paths:
            path = os.path.normpath(str(raw))
            mount = mount_for_path(path, mounts or [])
            if mount is not None and mount['id'] in seen:
                continue
            item = {'path': path, 'mount_point': mount['mount_point'] if mount else '',
                    'filesystem': mount['filesystem'] if mount else '', 'available': False,
                    'error': mount_error, 'total_bytes': None, 'available_bytes': None,
                    'used_bytes': None, 'used_percent': None}
            try:
                usage = self.disk(path)
                unit = usage.f_frsize or usage.f_bsize
                total, free, available = (unit * value for value in
                                          (usage.f_blocks, usage.f_bfree, usage.f_bavail))
                if unit <= 0 or total <= 0 or not 0 <= available <= free <= total:
                    raise ValueError('invalid disk capacity')
                used = total - free
                item.update(available=True, total_bytes=total, available_bytes=available,
                            used_bytes=used, used_percent=round(100.0 * used / total, 3))
                if mount is not None:
                    seen.add(mount['id'])
            except (OSError, ValueError, TypeError, AttributeError):
                item['error'] = '; '.join(v for v in (mount_error, 'Disk capacity could not be read') if v)
            entries.append(item)
        return entries

    def collect(self, paths=DEFAULT_DISK_PATHS, sample_seconds=0.2):
        result = {'schema_version': SCHEMA_VERSION, 'observed_at': self.timestamp(),
                  'available': False, 'error': '', 'sample_interval_seconds': None,
                  'uptime_seconds': None,
                  'cpu': {'available': False, 'error': 'CPU sampling unavailable',
                          'utilization_percent': None, 'logical_count': None},
                  'memory': {'available': False, 'error': 'Memory observation unavailable',
                             'total_bytes': None, 'available_bytes': None,
                             'used_bytes': None, 'used_percent': None},
                  'disks': [], 'network': []}
        if (not isinstance(paths, (tuple, list)) or not 1 <= len(paths) <= 16
                or any(not isinstance(v, str) or not os.path.isabs(v) or len(v) > 4096 or '\x00' in v for v in paths)
                or not isinstance(sample_seconds, (int, float)) or isinstance(sample_seconds, bool)
                or not math.isfinite(sample_seconds) or not 0.05 <= sample_seconds <= 1):
            result['error'] = 'Host observation requires fixed absolute paths and a bounded sample interval'
            return result
        errors = []
        try:
            start = self.clock()
            cpu_first, cpu_first_error = self._read('stat', parse_cpu_stat)
            net_first, net_first_error = self._read('net/dev', parse_network_counters)
            self.sleep(sample_seconds)
            cpu_second, cpu_second_error = self._read('stat', parse_cpu_stat)
            net_second, net_second_error = self._read('net/dev', parse_network_counters)
            elapsed = self.clock() - start
            if math.isfinite(elapsed) and 0 < elapsed <= 60:
                result['sample_interval_seconds'] = round(elapsed, 6)
            else:
                errors.append('Host sample interval is unavailable or exceeded 60 seconds')
            result['cpu'] = cpu_observation(cpu_first, cpu_second)
            if cpu_first_error or cpu_second_error:
                result['cpu']['error'] = '; '.join(v for v in (cpu_first_error, cpu_second_error) if v)
            memory, memory_error = self._read('meminfo', memory_observation)
            if memory is not None:
                result['memory'] = memory
            else:
                result['memory']['error'] = memory_error
            result['disks'] = self._disks(paths)
            network, address_error = self._network(net_first, net_second,
                                                  result['sample_interval_seconds'],
                                                  net_first_error, net_second_error)
            result['network'] = network
            def uptime(text):
                value = float(text.split()[0])
                if not math.isfinite(value) or value < 0:
                    raise ValueError('invalid uptime')
                return value
            uptime_value, uptime_error = self._read('uptime', uptime)
            result['uptime_seconds'] = uptime_value
            errors.extend(v for v in (result['cpu']['error'], result['memory']['error'],
                                     net_first_error, net_second_error, address_error, uptime_error) if v)
            errors.extend(item['error'] for item in result['disks'] + network if item['error'])
        except (OSError, ValueError, TypeError, IndexError, OverflowError, subprocess.SubprocessError):
            # Metrics must not turn an otherwise successful version inspection
            # into a failed inventory; retain any observations already obtained.
            errors.append('Host observation was incomplete')
        result['error'] = '; '.join(dict.fromkeys(errors))
        result['observed_at'] = self.timestamp()
        # The array limits can still exceed the wire limit when every interface
        # has many addresses. Keep the gauges and an explicitly partial network
        # list instead of having the consumer discard the whole observation.
        if len(json.dumps(result, allow_nan=False).encode('utf-8')) > MAX_STATUS_BYTES:
            warning = 'Host observation size limit exceeded; network list is partial'
            result['error'] = '; '.join(v for v in (result['error'], warning) if v)
            while result['network'] and len(json.dumps(result, allow_nan=False).encode('utf-8')) > MAX_STATUS_BYTES:
                result['network'].pop()
        result['available'] = bool(result['cpu']['available'] or result['memory']['available']
                                   or any(v['available'] for v in result['disks'] + result['network'])
                                   or result['uptime_seconds'] is not None)
        return result


def collect_host_status(paths=DEFAULT_DISK_PATHS, sample_seconds=0.2):
    """Pass only release-profile-owned disk paths, never browser input."""
    return HostMetricsCollector().collect(paths, sample_seconds)
