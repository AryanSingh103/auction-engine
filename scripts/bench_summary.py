#!/usr/bin/env python3
"""Summarize a scripts/bench.sh output directory as Markdown tables.

Usage: scripts/bench_summary.py docs/benchmarks/<run-dir>

For each (strategy, config) it reports the MEDIAN across repetitions and
the min-max range, so a single lucky or unlucky run cannot set the number.
Runs whose verification checks failed (*.FAILED.json) are listed and
excluded; their numbers must not be used.
"""
import glob
import json
import os
import re
import statistics
import sys


def load(run_dir):
    runs = {}
    for path in sorted(glob.glob(os.path.join(run_dir, "*.json"))):
        name = os.path.basename(path)
        if name.endswith(".server.json") or name.endswith(".FAILED.json"):
            continue
        label = name[: -len(".json")]
        m = re.match(r"(.+)-r(\d+)$", label)
        if not m:
            continue
        with open(path) as f:
            rep = json.load(f)
        server = {}
        server_path = os.path.join(run_dir, label + ".server.json")
        if os.path.exists(server_path):
            with open(server_path) as f:
                server = json.load(f)
        runs.setdefault(m.group(1), []).append((rep, server))
    failed = sorted(os.path.basename(p) for p in glob.glob(os.path.join(run_dir, "*.FAILED.json")))
    return runs, failed


def med_range(values, fmt):
    values = [v for v in values if v is not None]
    if not values:
        return "n/a"
    med = statistics.median(values)
    if len(values) == 1:
        return fmt.format(med)
    return (fmt + " ({}–{})").format(med, fmt.format(min(values)), fmt.format(max(values)))


def num(v):
    try:
        return float(v)
    except (TypeError, ValueError):
        return None


def main():
    run_dir = sys.argv[1]
    runs, failed = load(run_dir)

    print("| run | reps | throughput req/s | accepted bids/s | p50 ms | p99 ms | p99.9 ms | too-low share | GET share of all requests | lock wait p99 ms (server) | optimistic conflicts | contention 503s |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|")

    def order(key):
        m = re.match(r"(control|pessimistic|optimistic)-(hot|spread|healthz)-w(\d+)", key)
        if not m:
            return (9, key, 0, 0)
        kind, scen, w = m.groups()
        return ({"control": 0, "hot": 1, "spread": 2, "healthz": 0}[scen] if kind != "control" else 0,
                scen, int(w), 0 if kind == "pessimistic" else 1)

    for key in sorted(runs, key=order):
        reps = runs[key]
        rs = [r for r, _ in reps]
        ss = [s for _, s in reps]

        def too_low_share(r):
            res = r["results"]
            bids = sum(v for k, v in res.items() if not k.startswith("get_"))
            return 100 * res.get("409_bid_too_low", 0) / bids if bids else None

        def get_share(r):
            res = r["results"]
            total = sum(res.values())
            return 100 * sum(v for k, v in res.items() if k.startswith("get_")) / total if total else None

        print("| {} | {} | {} | {} | {} | {} | {} | {} | {} | {} | {} | {} |".format(
            key, len(reps),
            med_range([r["throughput_rps"] for r in rs], "{:.0f}"),
            med_range([r["accepted_bids_per_second"] for r in rs], "{:.0f}") if not key.startswith("control") else "–",
            med_range([r["latency"]["p50_ms"] for r in rs], "{:.2f}"),
            med_range([r["latency"]["p99_ms"] for r in rs], "{:.2f}"),
            med_range([r["latency"]["p99_9_ms"] for r in rs], "{:.1f}"),
            med_range([too_low_share(r) for r in rs], "{:.0f}%") if not key.startswith("control") else "–",
            med_range([get_share(r) for r in rs], "{:.0f}%") if not key.startswith("control") else "–",
            med_range([None if num(s.get("lock_wait_p99_s")) is None else 1000 * num(s.get("lock_wait_p99_s")) for s in ss], "{:.1f}") if not key.startswith("control") else "–",
            med_range([num(s.get("optimistic_conflicts")) for s in ss], "{:.0f}") if key.startswith("optimistic") else "–",
            med_range([num(s.get("contention_failures")) for s in ss], "{:.0f}") if key.startswith("optimistic") else "–",
        ))

    print()
    print("Server-side snapshot medians (direction only; see the caveats):")
    print()
    print("| run | api CPU-seconds | pool empty-acquire waits |")
    print("|---|---|---|")
    for key in sorted(runs, key=order):
        ss = [s for _, s in runs[key]]
        print("| {} | {} | {} |".format(key,
            med_range([num(s.get("api_cpu_seconds")) for s in ss], "{:.1f}"),
            med_range([num(s.get("pool_empty_acquires")) for s in ss], "{:.0f}")))
    print()
    if failed:
        print("Runs that FAILED their verification checks (excluded above): " + ", ".join(failed))
    else:
        print("Every run passed all verification checks.")


if __name__ == "__main__":
    main()
