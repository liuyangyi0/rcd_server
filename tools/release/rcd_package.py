#!/usr/bin/env python3
"""Build an immutable, single-component .rcd.tar.gz for Semaphore uploads.

Only a verified binary and allowlisted public metadata enter the package.
Does not execute binaries, contact hosts, or deploy anything.
"""
import argparse
from datetime import datetime
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import tarfile
import tempfile
from application_profile import BUILTINS, component_slug, application_slug

COMPONENTS = ('lower', 'manager', 'agent', 'communicate')
FIELDS = ('component', 'version', 'release_id', 'commit', 'source_sha256', 'dirty', 'built_at', 'target')
ID = re.compile(r'[A-Za-z0-9][A-Za-z0-9._+\-]{0,127}\Z')
HASH = re.compile(r'[a-f0-9]{64}\Z')
VERSION = re.compile(r'(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\Z')
FORMATS = {'lower': {'command_journal', 'maintenance'}, 'manager': {'manager_db'},
           'agent': {'agent_state'}, 'communicate': set()}


def digest(path):
    h = hashlib.sha256()
    with path.open('rb') as stream:
        for data in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(data)
    return h.hexdigest()


def load(path):
    if path.is_symlink() or not path.is_file() or path.stat().st_size > 8 << 20:
        raise ValueError('regular bounded JSON file required')
    def unique(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ValueError('duplicate JSON field: ' + key)
            result[key] = value
        return result
    result = json.loads(path.read_text(encoding='utf-8'), object_pairs_hook=unique)
    if not isinstance(result, dict):
        raise ValueError('JSON object required')
    return result


def manifest(build, expected_sha256, compatibility, notes):
    if not build.is_absolute() or build.is_symlink() or not build.is_dir():
        raise ValueError('absolute regular build directory required')
    private = load(build / 'manifest.json')
    if not HASH.fullmatch(expected_sha256) or digest(build / 'manifest.json') != expected_sha256:
        raise ValueError('build manifest SHA-256 mismatch')
    info = private.get('build_info', {})
    if not isinstance(info, dict) or info.get('schema_version') != 1 or any(k not in info for k in FIELDS):
        raise ValueError('complete build identity required')
    component = info['component']
    if not component_slug(component) or private.get('component') != component or type(info['dirty']) is not bool:
        raise ValueError('build component or dirty identity mismatch')
    if any(not isinstance(info[k], str) or not ID.fullmatch(info[k]) for k in ('version', 'release_id')):
        raise ValueError('invalid version or release id')
    if not isinstance(info['commit'], str) or not re.fullmatch('[a-f0-9]{40}', info['commit']):
        raise ValueError('full commit required')
    if not isinstance(info['source_sha256'], str) or not HASH.fullmatch(info['source_sha256']) or private.get('source', {}).get('sha256') != info['source_sha256']:
        raise ValueError('source identity mismatch')
    timestamp = info['built_at']
    if not isinstance(timestamp, str) or not re.fullmatch(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?(?:Z|[+-]\d\d:\d\d)', timestamp) or datetime.fromisoformat(timestamp.replace('Z', '+00:00')).tzinfo is None:
        raise ValueError('build timestamp must be RFC3339')
    binary = build / 'binary'
    if binary.is_symlink() or not binary.is_file() or not 64 <= binary.stat().st_size <= 512 << 20:
        raise ValueError('regular bounded binary required')
    machine = {'linux/amd64': 62, 'linux/x86_64': 62,
               'linux/arm64': 183, 'linux/aarch64': 183}.get(info['target'])
    with binary.open('rb') as stream:
        header = stream.read(64)
    if (not machine or header[:7] != b'\x7fELF\x02\x01\x01' or
            int.from_bytes(header[16:18], 'little') not in (2, 3) or
            int.from_bytes(header[18:20], 'little') != machine):
        raise ValueError('ELF platform mismatch')
    sha = digest(binary)
    if sha != private.get('binary_sha256'):
        raise ValueError('binary SHA-256 mismatch')
    if not isinstance(compatibility, dict) or set(compatibility) != {'adapter_min_version', 'state_formats', 'component_min_versions'}:
        raise ValueError('explicit compatibility fields required')
    adapter = compatibility['adapter_min_version']
    if not isinstance(adapter, str) or not VERSION.fullmatch(adapter):
        raise ValueError('invalid adapter version')
    formats = compatibility['state_formats']
    if not isinstance(formats, dict) or set(formats) != FORMATS.get(component, set()):
        raise ValueError('component state format coverage required')
    for bounds in formats.values():
        if (not isinstance(bounds, dict) or set(bounds) != {'min', 'max'} or
                any(type(bounds[k]) is not int for k in ('min', 'max')) or
                not 0 <= bounds['min'] <= bounds['max'] <= 2147483647):
            raise ValueError('invalid schema range')
    deps = compatibility['component_min_versions']
    if not isinstance(deps, dict) or any(not component_slug(k) or k == component or not isinstance(v, str) or not VERSION.fullmatch(v) for k, v in deps.items()):
        raise ValueError('invalid component dependencies')
    if not isinstance(notes, str) or len(notes.encode('utf-8')) > 16 << 10 or '\0' in notes:
        raise ValueError('invalid release notes')
    return dict({k: info[k] for k in FIELDS}, schema_version=1, binary_sha256=sha,
                binary_size=binary.stat().st_size, compatibility=compatibility, release_notes=notes)


def export(build, expected_sha256, compatibility, notes, output):
    if not output.is_absolute() or not output.name.endswith('.rcd.tar.gz') or not output.parent.is_dir() or output.exists() or output.is_symlink():
        raise ValueError('new absolute .rcd.tar.gz output required')
    public = manifest(build, expected_sha256, compatibility, notes)
    raw = json.dumps(public, ensure_ascii=False, sort_keys=True, separators=(',', ':')).encode()
    if len(raw) > 64 << 10:
        raise ValueError('manifest too large')
    with tempfile.TemporaryDirectory(prefix='.rcd-package-', dir=output.parent) as directory:
        stage = Path(directory)
        # Copy once, then hash that exact copy before building the archive.
        shutil.copyfile(build / 'binary', stage / 'binary')
        if digest(stage / 'binary') != public['binary_sha256']:
            raise ValueError('binary changed during export')
        archive = stage / 'package.rcd.tar.gz'
        with archive.open('xb') as target:
            with gzip.GzipFile(fileobj=target, mode='wb', mtime=0, filename='') as gz:
                with tarfile.open(fileobj=gz, mode='w', format=tarfile.USTAR_FORMAT) as tar:
                    for name, size, stream in [('manifest.json', len(raw), io.BytesIO(raw)),
                                               ('binary', public['binary_size'], (stage / 'binary').open('rb'))]:
                        with stream:
                            entry = tarfile.TarInfo(name)
                            entry.size, entry.mode, entry.mtime = size, 0o644, 0
                            tar.addfile(entry, stream)
            target.flush()
            os.fsync(target.fileno())
        if archive.stat().st_size > 256 << 20:
            raise ValueError('compressed archive too large')
        # No-clobber publication also handles concurrent builders.
        os.link(archive, output)
        descriptor = os.open(output.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    return {'path': str(output), 'sha256': digest(output), 'component': public['component'], 'release_id': public['release_id']}


def export_binary(binary, metadata, notes, output):
    """Package third-party Linux ELF without running it or accepting scripts.

    Metadata is public provenance supplied by the release builder. The server
    still requires a project registration before accepting this component.
    """
    if (not binary.is_absolute() or binary.is_symlink() or not binary.is_file()
            or not isinstance(metadata, dict) or set(metadata) != set(FIELDS)
            or not application_slug(metadata.get('component'))):
        raise ValueError('registered application slug, regular ELF and exact public build metadata required')
    with tempfile.TemporaryDirectory(prefix='rcd-application-build-') as directory:
        build = Path(directory)
        shutil.copyfile(binary, build / 'binary')
        private = {'component': metadata['component'], 'build_info': dict(metadata, schema_version=1),
                   'binary_sha256': digest(build / 'binary'), 'source': {'sha256': metadata['source_sha256']}}
        (build / 'manifest.json').write_text(json.dumps(private), encoding='utf-8')
        return export(build, digest(build / 'manifest.json'), {
            'adapter_min_version': '1.1.0', 'state_formats': {}, 'component_min_versions': {}}, notes, output)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument('--build', type=Path)
    source.add_argument('--binary', type=Path)
    parser.add_argument('--metadata', type=Path)
    parser.add_argument('--manifest-sha256')
    parser.add_argument('--compatibility', type=Path)
    parser.add_argument('--notes', default='')
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    if args.binary:
        if args.metadata is None or args.manifest_sha256 or args.compatibility:
            parser.error('--binary requires --metadata; it cannot take private build/compatibility overrides')
        result = export_binary(args.binary, load(args.metadata), args.notes, args.output)
    else:
        if not args.manifest_sha256 or args.compatibility is None or args.metadata:
            parser.error('--build requires --manifest-sha256 and --compatibility')
        result = export(args.build, args.manifest_sha256, load(args.compatibility), args.notes, args.output)
    print(json.dumps(result))
