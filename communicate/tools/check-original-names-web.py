"""Run the read-only 46 browser acceptance; password is interactive, never argv."""
import argparse
import getpass
import json
from pathlib import Path
import subprocess

parser = argparse.ArgumentParser()
parser.add_argument('--root', type=Path, required=True)
parser.add_argument('--phase', choices=['before', 'catalog-ready', 'after'], required=True)
args = parser.parse_args()
root = args.root.resolve(strict=True)
secret = getpass.getpass('Manager Web password: ')
raise SystemExit(subprocess.run([
    '/home/lenovo/code/rcd_manager_test/tools/node-v24.14.0-linux-x64/bin/node',
    str(root / 'source/communicate/tools/check-original-names-web.cjs'), str(root), args.phase],
    input=json.dumps({'password': secret}), text=True).returncode)
