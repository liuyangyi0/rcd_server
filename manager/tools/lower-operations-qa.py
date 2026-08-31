#!/usr/bin/env python3
"""Remote-only lower operations regression against the disposable PTY fixture.

No builds or production settings are touched. Explicit mutation opt-in, exact
fixture endpoint/port matching and a loopback HTTP URL are required. Evidence,
automatic config backups and synthetic log files are retained after the run.
"""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import socket
import sys
import time
import traceback
import urllib.error
import urllib.parse
import urllib.request


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def digest(content):
    if isinstance(content, str):
        content = content.encode("utf-8")
    return hashlib.sha256(content).hexdigest()


def atomic_json(path, value):
    temporary = path.with_name(path.name + ".operations-qa-%d.tmp" % os.getpid())
    with temporary.open("x", encoding="utf-8") as output:
        json.dump(value, output, ensure_ascii=False, indent=2)
        output.flush()
        os.fsync(output.fileno())
    os.replace(str(temporary), str(path))


class Regression:
    def __init__(self, args):
        assert sys.platform.startswith("linux"), "Execute only on the remote Linux host"
        assert args.allow_mutations, "--allow-mutations is required"
        parsed = urllib.parse.urlparse(args.api)
        assert parsed.hostname in ("127.0.0.1", "localhost") and parsed.scheme == "http"
        assert parsed.port not in (None, 80, 8080), "Use an explicit isolated test manager"
        self.base = args.api.rstrip("/")
        self.manifest_path = args.fixture.resolve(strict=True)
        self.fixture = self.read_manifest()
        self.root = self.manifest_path.parent
        assert self.fixture["state"] == "ready", "Start with a ready disposable fixture"
        assert self.root == Path(self.fixture["root"]).resolve()
        assert str(self.root).startswith("/home/lenovo/code/rcd_device_groups_20260828/runs/"), "Not the isolated test workspace"
        self.output = args.output.resolve()
        assert self.output != self.root and self.output.parent != Path("/")
        self.output.mkdir(parents=True, exist_ok=False)
        self.report = {"started_at": now(), "api": self.base, "fixture": str(self.manifest_path),
                       "host": socket.gethostname(), "checks": [], "requests": [], "restoration": [], "ok": False}
        self.source_revisions = {}
        self.initial_configs = {}
        self.changed_config = False
        self.changed_acquisition = False
        self.original_control = json.loads((self.root / "control.json").read_text(encoding="utf-8"))
        self.original_process = Path("/proc/398713/stat").read_text().split()[21]
        assert os.readlink("/proc/398713/exe") == "/home/lenovo/code/rcd/rcd_server"

    def read_manifest(self):
        return json.loads(self.manifest_path.read_text(encoding="utf-8"))

    def http(self, method, path, body=None, revision=None, expected=200):
        headers = {"Origin": self.base, "X-RCD-Operator": "remote-operations-regression"}
        data = None
        if body is not None:
            data = json.dumps(body, ensure_ascii=False).encode("utf-8")
            headers["Content-Type"] = "application/json"
        if revision is not None:
            headers["If-Match"] = json.dumps(str(revision))
        request = urllib.request.Request(self.base + "/api/v1" + path, data=data, method=method, headers=headers)
        started = time.monotonic()
        try:
            response = urllib.request.urlopen(request, timeout=20)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            status = response.status
            result = json.loads(response.read().decode("utf-8"))
        self.report["requests"].append({"method": method, "path": path, "status": status,
                                        "duration_ms": round((time.monotonic() - started) * 1000)})
        assert status == expected, "%s %s returned %d, expected %d: %s" % (method, path, status, expected, str(result)[:1000])
        return result

    def get(self, source, suffix, query=None):
        path = "/sources/lower-%s/operations/%s" % (source, suffix)
        if query:
            path += "?" + urllib.parse.urlencode(query)
        return self.http("GET", path)

    def wait(self, label, predicate, timeout=40):
        deadline = time.monotonic() + timeout
        last = None
        while time.monotonic() < deadline:
            try:
                value = predicate()
                if value:
                    return value
            except (AssertionError, OSError, ValueError) as error:
                last = error
            time.sleep(0.2)
        raise AssertionError("timeout %s: %s" % (label, last))

    def check(self, name, **details):
        self.report["checks"].append({"name": name, "at": now(), **details})
        print("PASS " + name, flush=True)

    def status(self, source="a"):
        result = self.get(source, "status")
        assert result["source_id"] == "lower-" + source and len(result["ports"]) == 1
        assert result["source_revision"] == self.source_revisions[source]
        assert result["ports"][0]["port_id"] == self.fixture["sources"][source]["port_id"]
        return result

    def control(self, enabled, device=None):
        snapshot = self.status()
        body = {"source_revision": self.source_revisions["a"], "port_id": self.fixture["sources"]["a"]["port_id"], "enabled": enabled}
        suffix = "port"
        if device is not None:
            suffix = "device-read"
            body["device_id"] = str(device)
        self.changed_acquisition = True
        result = self.http("PUT", "/sources/lower-a/operations/" + suffix, body, snapshot["revision"])
        assert result["status"] in ("Applied", "Pending")
        def applied():
            status = self.status()
            port = status["ports"][0]
            if port.get("control_pending"):
                return False
            if device is None:
                return status if port["enabled"] == enabled and port["applied_enabled"] == enabled and port["open"] == enabled else False
            station = next(item for item in port["devices"] if item["device_id"] == str(device))
            return status if station["read_enabled"] == enabled and station["applied_read_enabled"] == enabled else False
        return self.wait("acquisition applied", applied)

    def sample(self, source, identifier):
        page = self.http("GET", "/points?" + urllib.parse.urlencode({"route_source_id": "lower-"+source, "q": identifier, "limit": 100}))
        matches = [item for item in page["items"] if item["node"]["key"]["identifier"] == identifier]
        assert len(matches) == 1 and matches[0]["selected_source_id"] == "lower-"+source
        return matches[0]["sample"]

    def sample_is(self, source, identifier, good, value=None):
        sample = self.sample(source, identifier)
        return (sample["status_code"] & 0xC0000000 == 0) == good and (value is None or sample.get("value") == value)

    def has_serial_fd(self, source="a"):
        lower = self.read_manifest()["sources"][source]
        assert lower["pid"], "source is not running"
        target = lower["pty_slave"]
        for entry in Path("/proc/%s/fd" % lower["pid"]).iterdir():
            try:
                if os.readlink(str(entry)) == target:
                    return True
            except FileNotFoundError:
                pass
        return False

    def restart_a(self):
        old_pid = self.read_manifest()["sources"]["a"]["pid"]
        control_path = self.root / "control.json"
        current = json.loads(control_path.read_text())
        current.setdefault("a", {})["server_mode"] = "stopped"
        atomic_json(control_path, current)
        self.wait("owned source stopped", lambda: self.read_manifest()["sources"]["a"]["process_state"] == "stopped", 15)
        current["a"]["server_mode"] = "running"
        atomic_json(control_path, current)
        self.wait("new owned source PID", lambda: self.read_manifest()["sources"]["a"]["pid"] not in (None, old_pid), 15)
        # The fixture's normal readiness requires both devices to poll. During
        # the persistence test we intentionally keep them disabled, so prove OPC
        # availability and actual closed handles before reenabling acquisition.
        status = self.wait("OPC operations after restart", lambda: self.status(), 40)
        new_pid = self.read_manifest()["sources"]["a"]["pid"]
        assert new_pid != old_pid
        return status, old_pid, new_pid

    def config_body(self, content):
        return {"source_revision": self.source_revisions["a"], "name": "calc.toml", "content": content}

    def config_write(self, content, revision):
        self.changed_config = True
        return self.http("PUT", "/sources/lower-a/operations/config", self.config_body(content), revision)

    def run(self):
        for source in ("a", "b"):
            configured = self.http("GET", "/sources/lower-" + source)
            fixture_source = self.fixture["sources"][source]
            assert configured["endpoint"] == fixture_source["endpoint"]
            assert fixture_source["opc_port"] != 4840 and Path(fixture_source["run_dir"]).resolve().parent == self.root
            self.source_revisions[source] = configured["revision"]
            capability = self.get(source, "capabilities")
            assert capability["supported"] and capability["schema_version"] == 1
            assert set(capability["features"]) == {"acquisition", "configuration", "logs"}
            assert capability["source_revision"] == configured["revision"]
            state = self.status(source)["ports"][0]
            assert state["enabled"] and state["applied_enabled"] and state["open"]
            assert len(state["devices"]) == 2 and all(device["read_enabled"] and device["applied_read_enabled"] for device in state["devices"])
        self.check("physical source capability and runtime snapshots")

        for file in self.get("a", "configs")["items"]:
            document = self.get("a", "config", {"name": file["name"]})
            assert document["active_revision"] == document["revision"] == digest(document["content"])
            validation = self.http("POST", "/sources/lower-a/operations/config/validate", {"name": file["name"], "content": document["content"]})
            assert validation["valid"], validation
            self.initial_configs[file["name"]] = document
        assert set(self.initial_configs) == {"calc.toml", "config.toml", "rcd_device_1.csv", "rcd_device_2.csv"}
        self.check("all four existing configuration files round-trip and validate")

        state = self.status()
        body = {"source_revision": self.source_revisions["a"], "port_id": state["ports"][0]["port_id"], "enabled": False}
        self.http("PUT", "/sources/lower-a/operations/port", body, expected=428)
        self.http("PUT", "/sources/lower-a/operations/port", {**body, "source_revision": 999999}, state["revision"], 409)
        self.http("PUT", "/sources/lower-a/operations/port", body, "999999", 409)
        assert self.status()["revision"] == state["revision"]
        self.http("GET", "/sources/lower-a/operations/config?name=..%2Fconfig.toml", expected=422)
        self.check("double revision conflicts and invalid resource names cause no write")

        original = self.initial_configs["calc.toml"]
        invalid = original["content"] + "\n[[rules]]\nkks_calc = 'A_VALUE'\ndata_type = 'Double'\nexpression = '('\n"
        validation = self.http("POST", "/sources/lower-a/operations/config/validate", {"name": "calc.toml", "content": invalid})
        assert not validation["valid"] and validation["errors"]
        self.http("PUT", "/sources/lower-a/operations/config", self.config_body(invalid), original["revision"], 422)
        assert self.get("a", "config", {"name": "calc.toml"})["revision"] == original["revision"]
        csv = self.initial_configs["rcd_device_1.csv"]
        invalid_csv = self.http("POST", "/sources/lower-a/operations/config/validate", {"name": csv["name"], "content": csv["content"].replace("A_VALUE", "B_VALUE")})
        assert not invalid_csv["valid"]
        self.check("invalid formula and duplicate KKS rejected without changing disk")

        # Exercise the actual JSON -> UA String -> Rust parser path at the
        # advertised limit, not merely the HTTP decoder or a fake lower client.
        max_bytes = 2 * 1024 * 1024
        prefix = original["content"] + "\n#"
        large = prefix + "\t" * (max_bytes - len(prefix.encode("utf-8")))
        large_validation = self.http("POST", "/sources/lower-a/operations/config/validate", {"name": "calc.toml", "content": large})
        assert large_validation["valid"], large_validation
        # Invalid TOML still needs to cross the 12 MiB escaped String intact,
        # returning a bounded diagnostic rather than killing the OPC Session.
        escaped_invalid = "#" + "\x01" * (max_bytes - 1)
        invalid_large = self.http("POST", "/sources/lower-a/operations/config/validate", {"name": "calc.toml", "content": escaped_invalid})
        assert not invalid_large["valid"] and invalid_large["errors"]
        assert len(json.dumps(invalid_large).encode("utf-8")) < 100000
        self.http("POST", "/sources/lower-a/operations/config/validate", {"name": "calc.toml", "content": large + "x"}, expected=413)
        assert self.get("a", "config", {"name": "calc.toml"})["revision"] == original["revision"]
        self.check("2 MiB configuration and worst-case escaped UA String validate without writes or disconnects")

        modified = original["content"] + "\n# remote operations regression " + now() + "\n"
        saved = self.config_write(modified, original["revision"])
        assert saved["revision"] == digest(modified) and saved["active_revision"] == original["revision"] and saved["restart_required"]
        assert saved["backups"], "automatic backup missing"
        backup_id = saved["backups"][0]["id"]
        backup = self.get("a", "config", {"name": "calc.toml", "backup_id": backup_id})
        assert backup["content"] == original["content"] and backup["revision"] == saved["revision"] and backup["backup_revision"] == original["revision"]
        self.http("PUT", "/sources/lower-a/operations/config", self.config_body(original["content"]), original["revision"], 409)
        self.check("atomic save, automatic backup, restore CAS and restart-required marker")

        self.control(False, 1)
        self.wait("A quality becomes bad", lambda: self.sample_is("a", "A_VALUE", False))
        self.wait("B still good", lambda: self.sample_is("a", "B_VALUE", True, 22))
        assert self.sample_is("b", "A_VALUE", True, 101) and self.has_serial_fd()
        self.check("station 1 stops reading, station 2 and other source remain live")
        self.control(False)
        assert not self.has_serial_fd(), "worker reported closed but retained the PTY handle"
        self.wait("both local station qualities bad", lambda: self.sample_is("a", "A_VALUE", False) and self.sample_is("a", "B_VALUE", False))
        assert self.sample_is("b", "B_VALUE", True, 202)
        source_view = self.http("GET", "/sources/lower-a")
        assert source_view["connection_state"] == "Connected"
        self.check("port close releases actual fd without stopping OPC or failing over")

        status, old_pid, new_pid = self.restart_a()
        port = status["ports"][0]
        assert not port["enabled"] and not port["applied_enabled"] and not port["open"] and not self.has_serial_fd()
        station = next(device for device in port["devices"] if device["device_id"] == "1")
        assert not station["read_enabled"] and not station["applied_read_enabled"]
        active = self.get("a", "config", {"name": "calc.toml"})
        assert active["active_revision"] == active["revision"] == digest(modified) and not active["restart_required"]
        self.check("closed port and stopped station survive process restart; config becomes active", old_pid=old_pid, new_pid=new_pid)
        self.control(True)
        assert self.has_serial_fd()
        self.wait("reopened B fresh data", lambda: self.sample_is("a", "B_VALUE", True, 22))
        assert self.sample_is("a", "A_VALUE", False)
        self.control(True, 1)
        self.wait("A fresh data restored", lambda: self.sample_is("a", "A_VALUE", True, 11))
        self.check("port reopens and station read can be restored independently")
        restored = self.config_write(original["content"], active["revision"])
        assert restored["revision"] == original["revision"] and restored["restart_required"]
        self.restart_a()
        self.wait("all fixture points restored", lambda: self.sample_is("a", "A_VALUE", True, 11) and self.sample_is("a", "B_VALUE", True, 22))
        self.changed_config = False
        self.changed_acquisition = False
        self.check("configuration restoration and second restart complete")

        self.test_logs()
        self.test_filtered_long_log_lines()
        audit = self.http("GET", "/management-audit?limit=100")
        operations = [item for item in audit["items"] if item["kind"].startswith("source.operation.")]
        assert operations and any(item["details"].get("content_sha256") == digest(modified) for item in operations)
        encoded = json.dumps(operations)
        assert modified not in encoded and "[[rules]]" not in encoded
        self.check("management audit retains target, revisions and hashes, not file bodies", entries=len(operations))

    def test_logs(self):
        logs = self.get("a", "logs")
        assert logs["items"], "fixture should have real Rust logs"
        actual = logs["items"][0]
        tail = self.get("a", "log", {"name": actual["name"], "generation": actual["generation"], "tail": "true", "limit": 2048})
        assert len(tail["text"].encode("utf-8")) <= 2048 and tail["generation"] == actual["generation"]
        source = self.fixture["sources"]["a"]
        directory = Path(source["log_dir"]).resolve(strict=True)
        assert directory.parent == Path(source["run_dir"]).resolve()
        name = "rcd_server_fixture_operations-qa-%d-%d.log" % (os.getpid(), time.time_ns())
        path = directory / name
        content = "".join("时间 [%-5s] 测试中文😀 line %d %s\n" % ("WARN" if index % 17 == 0 else "INFO", index, "x"*45) for index in range(6000)).encode("utf-8")
        with path.open("xb") as output:
            output.write(content)
        listed = next(item for item in self.get("a", "logs")["items"] if item["name"] == name)
        generation = listed["generation"]
        offset, parts, pages = 0, [], 0
        while True:
            chunk = self.get("a", "log", {"name": name, "generation": generation, "offset": offset, "limit": 32768})
            assert chunk["generation"] == generation and len(chunk["text"].encode("utf-8")) <= 32768
            assert chunk["next_offset"] > offset or chunk["eof"]
            parts.append(chunk["text"]); pages += 1; offset = chunk["next_offset"]
            assert pages < 100
            if chunk["eof"]:
                break
        assert "".join(parts) == content.decode("utf-8") and offset == len(content)
        filtered = self.get("a", "log", {"name": name, "generation": generation, "offset": 0, "limit": 4096, "level": "WARN", "q": "测试中文"})
        assert filtered["text"] and all("[WARN ]" in line for line in filtered["text"].splitlines())
        with path.open("ab") as output:
            output.write("[INFO ] appended 中文😀\n".encode("utf-8"))
        appended = self.get("a", "log", {"name": name, "generation": generation, "offset": offset, "limit": 4096})
        assert appended["generation"] == generation and appended["text"] == "[INFO ] appended 中文😀\n"
        # Retain both generations as evidence. Never rotate the application's
        # real log: only the uniquely named synthetic file created above.
        path.rename(self.output / (name + ".first-generation"))
        with path.open("xb") as output:
            output.write(b"[WARN ] replacement generation\n")
        query = urllib.parse.urlencode({"name": name, "generation": generation, "offset": 0, "limit": 4096})
        self.http("GET", "/sources/lower-a/operations/log?"+query, expected=409)
        replacement = self.get("a", "log", {"name": name, "offset": 0, "limit": 4096})
        assert replacement["generation"] != generation and "replacement generation" in replacement["text"]
        with path.open("wb") as output:
            output.write(b"")
        query = urllib.parse.urlencode({"name": name, "generation": replacement["generation"], "offset": 0})
        self.http("GET", "/sources/lower-a/operations/log?"+query, expected=409)
        empty = self.get("a", "log", {"name": name, "offset": 0, "limit": 4096})
        assert empty["eof"] and empty["text"] == "" and empty["next_offset"] == 0
        self.check("real log tail, bounded UTF8 pagination, filter, append, rotation, truncation and empty state", pages=pages, synthetic_file=str(path))

    def test_filtered_long_log_lines(self):
        source = self.fixture["sources"]["a"]
        directory = Path(source["log_dir"]).resolve(strict=True)
        assert directory.parent == Path(source["run_dir"]).resolve()
        name = "rcd_server_fixture_operations-long-%d-%d.log" % (os.getpid(), time.time_ns())
        matched = "时间 [WARN ] needle-prefix " + "设备😀" * 35 + "\n"
        content = matched + "时间 [INFO ] other line must not inherit WARN or the query\n"
        with (directory / name).open("xb") as output:
            output.write(content.encode("utf-8"))
        for filters in ({"level": "WARN"}, {"q": "needle-prefix"}):
            offset, pages, pieces, generation = 0, 0, [], ""
            while True:
                chunk = self.get("a", "log", {"name": name, "generation": generation, "offset": offset, "limit": 32, **filters})
                assert len(chunk["text"].encode("utf-8")) <= 32
                assert chunk["next_offset"] > offset or chunk["eof"]
                generation = chunk["generation"]
                pieces.append(chunk["text"])
                offset = chunk["next_offset"]
                pages += 1
                assert pages < 100
                if chunk["eof"]:
                    break
            assert "".join(pieces) == matched, "filtered continuation lost a matched long line or leaked context to the next line"
            assert offset == len(content.encode("utf-8")) and pages > 2
        self.check("filtered long UTF8 log lines retain every continuation and isolate following lines", synthetic_file=str(directory / name))

        huge_name = "rcd_server_fixture_operations-long-bound-%d-%d.log" % (os.getpid(), time.time_ns())
        with (directory / huge_name).open("xb") as output:
            output.write(b"time [WARN ] " + b"x" * 300000 + b"\n")
        first = self.get("a", "log", {"name": huge_name, "offset": 0, "limit": 32})
        query = {"name": huge_name, "generation": first["generation"], "offset": 280000, "limit": 32, "level": "WARN"}
        failure = self.http("GET", "/sources/lower-a/operations/log?" + urllib.parse.urlencode(query), expected=422)
        assert failure["error"]["code"] == "invalid_request"
        del query["level"]
        unfiltered = self.get("a", "log", query)
        assert unfiltered["text"] == "x" * 32 and unfiltered["next_offset"] == 280032
        self.check("filtered line lookback is bounded with explicit recovery to unfiltered pagination", synthetic_file=str(directory / huge_name))

    def restore(self):
        if self.changed_acquisition or self.changed_config:
            control = json.loads((self.root / "control.json").read_text())
            control.setdefault("a", {})["server_mode"] = "running"
            atomic_json(self.root / "control.json", control)
            self.wait("lower available for restoration", lambda: self.status())
        if self.changed_acquisition:
            self.control(True)
            self.control(True, 1)
            self.control(True, 2)
        if self.changed_config and "calc.toml" in self.initial_configs:
            current = self.get("a", "config", {"name": "calc.toml"})
            original = self.initial_configs["calc.toml"]
            if current["content"] != original["content"]:
                self.config_write(original["content"], current["revision"])
            self.restart_a()
        atomic_json(self.root / "control.json", self.original_control)
        self.wait("fixture ready after restoration", lambda: all(source["process_state"] == "running" for source in self.read_manifest()["sources"].values()))
        for source in ("a", "b"):
            port = self.status(source)["ports"][0]
            assert port["enabled"] and port["applied_enabled"] and port["open"] and not port["control_pending"] and not port["pending_commands"]
            assert all(device["read_enabled"] and device["applied_read_enabled"] for device in port["devices"])
        for name, original in self.initial_configs.items():
            current = self.get("a", "config", {"name": name})
            assert current["content"] == original["content"] and not current["restart_required"]
        assert Path("/proc/398713/stat").read_text().split()[21] == self.original_process
        self.report["restoration"].append({"at": now(), "acquisition": "all enabled and applied", "configuration": "original content active", "original_pid": 398713, "original_untouched": True})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--api", required=True)
    parser.add_argument("--fixture", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--allow-mutations", action="store_true")
    run = Regression(parser.parse_args())
    try:
        run.run()
        run.report["ok"] = True
    except Exception:
        run.report["error"] = traceback.format_exc()
        print(run.report["error"], flush=True)
    finally:
        try:
            run.restore()
        except Exception:
            run.report["ok"] = False
            run.report["restoration_error"] = traceback.format_exc()
            print(run.report["restoration_error"], flush=True)
        run.report["finished_at"] = now()
        atomic_json(run.output / "report.json", run.report)
    return 0 if run.report["ok"] else 1


if __name__ == "__main__":
    sys.exit(main())
