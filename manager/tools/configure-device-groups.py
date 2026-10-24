#!/usr/bin/env python3
"""Configure an empty disposable manager for device-groups-fixture.py.

Run only on the remote test host. No production configuration is copied and no
existing unrelated groups/sources are changed. --resume completes a partial
setup with these exact fixture IDs/endpoints; it never deletes configuration.
"""

import argparse
import json
from pathlib import Path
import sys
import time
import urllib.error
import urllib.request


def request(base, method, path, body=None, revision=None):
    headers = {"Origin": base}
    data = None
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    if revision is not None:
        headers["If-Match"] = '"%d"' % revision
    req = urllib.request.Request(base + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=10) as response:
            return json.loads(response.read().decode("utf-8"))
    except urllib.error.HTTPError as error:
        raise RuntimeError("%s %s: HTTP %s %s" % (method, path, error.code, error.read().decode("utf-8")))


def binding_keys(bindings):
    # The Web mapping editor may return devices in a different order after a
    # round trip. Bindings are a set; --resume must not mistake that for a new
    # fixture or silently rewrite otherwise identical configuration.
    return sorted((binding["port_id"], binding["device_id"], binding["group_id"])
                  for binding in (bindings or []))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api", default="http://127.0.0.1:8083")
    parser.add_argument("--fixture", required=True, type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--resume", action="store_true")
    args = parser.parse_args()
    base = args.api.rstrip("/")
    fixture = json.loads(args.fixture.read_text(encoding="utf-8"))
    if fixture["state"] != "ready":
        raise RuntimeError("fixture must be ready before manager configuration")
    groups = request(base, "GET", "/api/v1/groups")["items"]
    sources = request(base, "GET", "/api/v1/sources")["items"]
    expected_groups = {"shared": "共享计算与未分配", "device-a": "设备 A", "device-b": "设备 B"}
    expected_sources = {"lower-a", "lower-b"}
    if (groups or sources) and not args.resume:
        raise RuntimeError("manager is not empty; only use --resume for this same fixture")
    if any(group["id"] not in expected_groups for group in groups) or any(source["id"] not in expected_sources for source in sources):
        raise RuntimeError("unrelated configuration exists; refusing to modify this manager")
    for identifier, name in expected_groups.items():
        if not any(group["id"] == identifier for group in groups):
            request(base, "POST", "/api/v1/groups", {"id": identifier, "name": name})
    for label in ("a", "b"):
        lower = fixture["sources"][label]
        identifier = "lower-" + label
        mutation = {
            "id": identifier, "name": "采集源 " + label.upper(), "group_id": "shared",
            "endpoint": lower["endpoint"], "enabled": True, "security_policy": "None", "security_mode": "None",
            "group_bindings": [
                {"group_id": "device-a", "port_id": lower["port_id"], "device_id": "1"},
                {"group_id": "device-b", "port_id": lower["port_id"], "device_id": "2"},
            ],
        }
        existing = next((source for source in sources if source["id"] == identifier), None)
        if existing is None:
            request(base, "POST", "/api/v1/sources", mutation)
        elif (any(existing.get(key) != mutation[key] for key in ("endpoint", "group_id"))
              or binding_keys(existing.get("group_bindings")) != binding_keys(mutation["group_bindings"])):
            raise RuntimeError("existing source %s does not match this fixture; no overwrite performed" % identifier)
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        sources = request(base, "GET", "/api/v1/sources")["items"]
        if len(sources) == 2 and all(source["catalog_state"] == "Ready" and source["subscription_state"] == "Active" for source in sources):
            break
        time.sleep(0.5)
    else:
        raise RuntimeError("lower discovery failed: " + json.dumps(sources, ensure_ascii=False))
    for identifier in expected_groups:
        group = request(base, "GET", "/api/v1/groups/" + identifier)
        if group.get("default_source_id") != "lower-a":
            request(base, "PUT", "/api/v1/groups/" + identifier + "/default-source",
                    {"source_id": "lower-a"}, group["revision"])
    topologies = {source: request(base, "GET", "/api/v1/sources/" + source + "/topology") for source in sorted(expected_sources)}
    if any(not topology["supported"] or len(topology["ports"]) != 1 or len(topology["ports"][0]["devices"]) != 2 for topology in topologies.values()):
        raise RuntimeError("manager did not preserve the two-port/two-device topology")
    result = {"api": base, "overview": request(base, "GET", "/api/v1/overview"),
              "groups": request(base, "GET", "/api/v1/groups")["items"], "topologies": topologies}
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as error:
        print("Configuration failed: %s" % error, file=sys.stderr)
        sys.exit(1)
