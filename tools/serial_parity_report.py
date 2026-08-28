"""Summarize JSONL from the standalone serial parity probe (standard library only)."""
import argparse
import collections
import json
from pathlib import Path


def summarize(path):
    counts = collections.Counter()
    devices = collections.defaultdict(collections.Counter)
    elapsed = []
    marks = 0
    mark_queries = 0
    latest = None
    start = None
    icount = []
    samples = []
    malformed = 0
    positions = collections.Counter()
    pending = 0
    for line in path.read_text().splitlines():
        try:
            row = json.loads(line)
        except json.JSONDecodeError:
            malformed += 1
            continue
        if row["type"] == "start":
            start = row
        elif row["type"] == "end":
            latest = row
        elif row["type"] == "icount":
            icount.append(row)
        elif row["type"] == "query":
            result = row["result"]
            counts[result] += 1
            devices[(row["id"], row["requested_len"])][result] += 1
            elapsed.append(row["elapsed_us"])
            marks += row["error_marks"]
            mark_queries += bool(row["error_marks"])
            positions.update(row.get("error_positions", []))
            pending += row.get("pending_before", 0) > 0
            if (result not in ("valid", "timeout") or row["error_marks"]) and len(samples) < 3:
                samples.append(row)
    total = sum(counts.values())
    elapsed.sort()
    print(path.name, "DONE" if latest else "RUNNING", dict(counts))
    if start:
        print("  port", start["port"], "mode", start["mode"], start["rx_mode"],
              "iflag", start["iflag"], "baud", start["ospeed"])
    print("  queries", total, "error_marks", marks, "marked_queries", mark_queries,
          "malformed_lines", malformed)
    print("  marked_byte_positions", dict(positions), "queries_with_pending_input", pending)
    if latest:
        print("  seconds", round(latest["duration_us"] / 1e6, 2),
              "failed", latest["failed"], "interrupted", latest["interrupted"])
    if elapsed:
        print("  query_ms min/p50/p95/max", *(round(elapsed[i] / 1000, 2)
              for i in (0, len(elapsed) // 2, min(len(elapsed) - 1, int(len(elapsed) * .95)), -1)))
    responding = []
    for (device, length), results in sorted(devices.items()):
        if results["valid"]:
            responding.append(f"{device}:{length}={results['valid']}/{sum(results.values())}")
    print("  responders", " ".join(responding) or "none")
    if len(icount) == 2 and all(x["supported"] for x in icount):
        print("  icount_delta", {k: icount[1][k] - icount[0][k]
              for k in ("rx", "tx", "parity", "frame", "overrun", "brk", "buf_overrun")})
    elif icount:
        print("  icount_UNSUPPORTED", [x["errno"] for x in icount])
    for sample in samples:
        print("  anomaly", json.dumps(sample))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("paths", nargs="+")
    args = parser.parse_args()
    for name in args.paths:
        summarize(Path(name))


if __name__ == "__main__":
    main()
