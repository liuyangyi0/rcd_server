#!/usr/bin/env python3
"""Build/package the manager through the existing release implementation, on 46.

No deployment. Inputs must already be staged and tested in the isolated root.
"""
import argparse
import json
from pathlib import Path
import subprocess
import sys


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--release-tools', type=Path, required=True)
    parser.add_argument('--commit', required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--release-notes', default='同步输入免ACK预读；支持64KiB整包原子提交。保持原采集、选源、配置及端口。')
    args = parser.parse_args()
    assert '192.168.10.46' in subprocess.check_output(['hostname', '-I'], text=True).split()
    root = args.root.resolve(strict=True)
    source = root / 'source'
    stage = root / 'release-manager'
    stage.mkdir(mode=0o700, exist_ok=False)
    sys.path.insert(0, str(args.release_tools.resolve(strict=True)))
    import release
    from rcd_package import export
    original = release.run

    def logged(*command, **kwargs):
        print('RUN', command, flush=True)
        try:
            result = original(*command, **kwargs)
        except subprocess.CalledProcessError as error:
            print(error.output, flush=True)
            raise
        print(result, flush=True)
        return result

    release.run = logged
    profile = {'host': '192.168.10.46', 'component': 'manager', 'source_dir': str(source),
               'release_dir': str(stage / 'artifacts'), 'runtime_dir': str(stage / 'unused'),
               'executable': str(stage / 'unused/binary'), 'databases': [str(stage / 'unused/state.sqlite')]}
    built = release.Release(profile, '192.168.10.46', 'manager').build(
        args.commit, True, args.version, args.version)
    directory = Path(built['release'])
    package = export(directory, release.sha(directory / 'manifest.json'),
                     {'adapter_min_version': '1.1.0', 'state_formats': {'manager_db': {'min': 16, 'max': 16}},
                      'component_min_versions': {}},
                     args.release_notes,
                     stage / ('manager-' + args.version + '.rcd.tar.gz'))
    (stage / 'result.json').write_text(json.dumps({'build': built, 'package': package}, indent=2))


if __name__ == '__main__':
    main()
