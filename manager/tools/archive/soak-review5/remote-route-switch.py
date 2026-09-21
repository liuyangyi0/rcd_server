#!/usr/bin/env python3
import json
import os
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime

BASE = "/home/lenovo/code/rcd_manager_test/soak-review5"
API = "http://127.0.0.1:8081/api/v1"
GROUP = "remote-line"
SOURCE_A = "rcd-46-none"
SOURCE_B = "rcd-46-secure-anon"
INTERVAL_SECONDS = 15 * 60


def stamp():
    return datetime.now().astimezone().isoformat(timespec="seconds")


def soak_running():
    try:
        with open(os.path.join(BASE, "soak-24h.pid"), encoding="ascii") as handle:
            os.kill(int(handle.read().strip()), 0)
        return True
    except (FileNotFoundError, ProcessLookupError, PermissionError, ValueError):
        return False


def get_json(url):
    with urllib.request.urlopen(url, timeout=15) as response:
        return json.load(response)


def switch_once():
    sessions = get_json(f"{API}/sessions").get("items", [])
    sessions = [item for item in sessions if item.get("client_name") == "rcd-manager-soak-01"]
    if not sessions:
        raise RuntimeError("target soak Session is not online")
    session = sessions[0]
    route = session.get("routes", {}).get(GROUP, {})
    current = route.get("source_id")
    target = SOURCE_B if current == SOURCE_A else SOURCE_A
    revision = int(route.get("revision", 0))
    path = urllib.parse.quote(session["id"], safe="")
    body = json.dumps({"source_id": target}).encode("utf-8")
    request = urllib.request.Request(
        f"{API}/sessions/{path}/groups/{GROUP}/selection",
        data=body,
        method="PUT",
        headers={"Content-Type": "application/json", "If-Match": f'"{revision}"'},
    )
    started = time.monotonic()
    with urllib.request.urlopen(request, timeout=60) as response:
        result = json.load(response)
    elapsed_ms = round((time.monotonic() - started) * 1000, 1)
    print(
        f"{stamp()} SWITCH client=rcd-manager-soak-01 from={current} to={target} "
        f"revision={result.get('revision')} http_ms={elapsed_ms}",
        flush=True,
    )


while soak_running():
    try:
        switch_once()
    except (urllib.error.URLError, urllib.error.HTTPError, RuntimeError, ValueError) as error:
        print(f"{stamp()} SWITCH_ERROR {error}", flush=True)
    for _ in range(INTERVAL_SECONDS):
        if not soak_running():
            raise SystemExit(0)
        time.sleep(1)
