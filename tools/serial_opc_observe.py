"""Export node names or observe OPC values/status/timestamps; never writes OPC values."""
import argparse
import asyncio
import collections
import csv
import datetime
import json
import logging
from pathlib import Path
import time


def export_manifest(folder, output):
    nodes = []
    for path in sorted(folder.glob("*.csv")):
        with path.open(encoding="utf-8-sig", newline="") as stream:
            rows = list(csv.reader(stream))
        header = {r[0].strip().lower(): r[1].strip() for r in rows[:7] if len(r) >= 2}
        for row in rows[8:11]:
            if len(row) < 2 or not row[0]:
                continue
            nodes.append({"port": header["com"], "device_id": int(header["deviceid"]),
                          "kks": header["pre"] + row[0], "type": row[1]})
    with output.open("x", encoding="utf-8") as stream:
        json.dump(nodes, stream, ensure_ascii=False, indent=2)
    print("EXPORTED", len(nodes), "nodes")


async def observe(args):
    from asyncua import Client, ua

    manifest = json.loads(args.manifest.read_text(encoding="utf-8"))
    if not manifest:
        raise ValueError("empty manifest")
    timestamps = collections.defaultdict(set)
    signatures = collections.defaultdict(set)
    statuses = collections.defaultdict(collections.Counter)
    notifications = collections.Counter()
    failures = []
    with args.output.open("x", encoding="utf-8") as stream:
        def emit(kind, **fields):
            stream.write(json.dumps({"time": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                                     "event": kind, **fields}, ensure_ascii=False, default=str) + "\n")
            stream.flush()

        class Handler:
            def datachange_notification(self, node, value, data):
                notifications[node.nodeid.to_string()] += 1
                emit("notification", node=node.nodeid.to_string(), value=value,
                     data_value=data.monitored_item.Value)

            def status_change_notification(self, status):
                emit("subscription_status", status=status)

        async with Client(args.url, timeout=10) as client:
            namespace = await client.get_namespace_index("urn:SerialServer")
            nodes = [client.get_node(ua.NodeId(x["kks"], namespace)) for x in manifest]
            endpoints = await client.get_endpoints()
            emit("connected", namespace=namespace, count=len(nodes), url=args.url,
                 advertised_endpoints=sorted({endpoint.EndpointUrl for endpoint in endpoints}))
            parameters = ua.CreateSubscriptionParameters()
            parameters.RequestedPublishingInterval = 250
            parameters.RequestedLifetimeCount = 10000
            parameters.RequestedMaxKeepAliveCount = (
                args.keepalive_count if args.keepalive_count is not None
                else client.get_keepalive_count(250)
            )
            parameters.MaxNotificationsPerPublish = 10000
            parameters.PublishingEnabled = True
            subscription = await client.create_subscription(parameters, Handler())
            try:
                handles = await subscription.subscribe_data_change(
                    nodes, queuesize=10, sampling_interval=args.sampling_ms)
                emit("subscribed", handles=handles, sampling_ms=args.sampling_ms,
                     requested_keepalive_count=parameters.RequestedMaxKeepAliveCount,
                     requested_publishing_ms=parameters.RequestedPublishingInterval)
                if any(isinstance(handle, ua.StatusCode) and handle.is_bad() for handle in handles):
                    raise RuntimeError("one or more monitored items could not be created")
                parameters = ua.ReadParameters()
                parameters.MaxAge = 0
                parameters.TimestampsToReturn = ua.TimestampsToReturn.Both
                parameters.NodesToRead = [ua.ReadValueId(NodeId=n.nodeid, AttributeId=ua.AttributeIds.Value)
                                          for n in nodes]
                started = time.monotonic()
                last_progress = started
                while time.monotonic() - started < args.seconds:
                    try:
                        values = await client.uaclient.read(parameters)
                        if len(values) != len(nodes):
                            raise RuntimeError("read result count mismatch")
                        for item, value in zip(manifest, values):
                            name = item["kks"]
                            raw_value = value.Value.Value if value.Value is not None else None
                            statuses[name][value.StatusCode.name] += 1
                            if value.ServerTimestamp:
                                timestamps[name].add(value.ServerTimestamp.isoformat())
                            signatures[name].add((value.StatusCode.name, repr(raw_value)))
                            emit("read", **item, value=raw_value, status=value.StatusCode.name,
                                 server_timestamp=value.ServerTimestamp, source_timestamp=value.SourceTimestamp)
                    except Exception as error:
                        failures.append(str(error))
                        emit("read_error", reason=str(error))
                    if time.monotonic() - last_progress >= 30:
                        print("OPC_PROGRESS", round(time.monotonic() - started),
                              "timestamp_advancing_nodes", sum(len(x) > 1 for x in timestamps.values()),
                              "read_errors", len(failures), flush=True)
                        last_progress = time.monotonic()
                    await asyncio.sleep(1)
            finally:
                await subscription.delete()
        summary = [{**item, "statuses": dict(statuses[item["kks"]]),
                    "distinct_server_timestamps": len(timestamps[item["kks"]]),
                    "distinct_value_status_pairs": len(signatures[item["kks"]]),
                    "notifications": notifications[ua.NodeId(item["kks"], namespace).to_string()]}
                   for item in manifest]
        emit("summary", nodes=summary, read_errors=failures)
        print("OPC_DONE", "nodes", len(nodes), "read_errors", len(failures), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--export-config", type=Path)
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--url", default="opc.tcp://192.168.10.46:4840/")
    parser.add_argument("--seconds", type=int, default=120)
    parser.add_argument("--keepalive-count", type=int)
    parser.add_argument("--sampling-ms", type=float, default=0)
    args = parser.parse_args()
    if args.export_config:
        export_manifest(args.export_config, args.manifest)
    else:
        if not args.output or not 1 <= args.seconds <= 3600:
            parser.error("output and duration 1..3600 required")
        if args.keepalive_count is not None and not 1 <= args.keepalive_count <= 1000:
            parser.error("keepalive count must be 1..1000")
        if args.sampling_ms not in (-1, 0, 250, 1000):
            parser.error("sampling must be -1, 0, 250, or 1000ms")
        logging.basicConfig(level=logging.WARNING, filename=str(args.output) + ".client.log",
                            encoding="utf-8", format="%(asctime)s %(name)s %(levelname)s %(message)s")
        asyncio.run(observe(args))


if __name__ == "__main__":
    main()
