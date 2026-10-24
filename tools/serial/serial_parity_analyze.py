"""Aggregate completed probe logs without interpreting silence as hardware failure."""
import argparse
import collections
import json
from pathlib import Path


def analyze(path):
    result = {"file": path.name, "counts": collections.Counter(), "devices": {},
              "marker_positions": collections.Counter(), "pending_queries": 0,
              "bad_examples": [], "bad_chunks": collections.Counter()}
    elapsed = []
    for line in path.read_text().splitlines():
        row = json.loads(line)
        kind = row.get("type")
        if kind in ("start", "options", "end"):
            result[kind] = row
        elif kind == "query":
            outcome = row["result"]
            result["counts"][outcome] += 1
            device = str(row["id"])
            result["devices"].setdefault(device, collections.Counter())[outcome] += 1
            result["marker_positions"].update(row.get("error_positions", []))
            result["pending_queries"] += row.get("pending_before", 0) > 0
            if outcome != "cancelled":
                elapsed.append(row["elapsed_us"])
            if outcome not in ("valid", "timeout", "cancelled"):
                result["bad_chunks"][row.get("chunks", "unknown")] += 1
                if len(result["bad_examples"]) < 3:
                    result["bad_examples"].append(row)
    elapsed.sort()
    if elapsed:
        result["query_ms"] = {"min": elapsed[0] / 1000, "p50": elapsed[len(elapsed) // 2] / 1000,
                              "p95": elapsed[min(len(elapsed)-1, int(len(elapsed)*.95))] / 1000,
                              "max": elapsed[-1] / 1000}
    end = result.get("end")
    result["complete"] = bool(end and not end["failed"] and not end["interrupted"])
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("folder", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    results = []
    for path in sorted(args.folder.glob("*-tty*.jsonl")):
        result = analyze(path)
        results.append(result)
        counts = result["counts"]
        bad = sum(v for k, v in counts.items() if k not in ("valid", "timeout", "cancelled"))
        print(path.name, "complete", result["complete"], "valid", counts["valid"],
              "bad", bad, "timeouts", counts["timeout"], "all_zero", counts["all_zero"],
              "pending", result["pending_queries"], "markers", dict(result["marker_positions"]))
    with args.output.open("w", encoding="utf-8") as stream:
        json.dump(results, stream, indent=2, ensure_ascii=False)


if __name__ == "__main__":
    main()
