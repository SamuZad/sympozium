"""Reports this node's Celln cells and parents to the fleet's cells ConfigMap.

Usage: fleet-cells.py snapshot ROOT NODE   (prints one node's report as JSON)

The report is what `celln ps -a` shows for the node's authority root, trimmed
to every live cell and the newest finished ones, plus each recent parent from
the parent journal with the worker cells its turns ran in. It carries hashes,
timings and outcomes only: never a task, an answer or a credential. The
fleet-cells.sh loop publishes it under data.<node>; the API server joins it
to AgentRuns.
"""
import json
import os
import subprocess
import sys
import time

KEEP_FINISHED = 50
KEEP_PARENTS = 50
CELL_FIELDS = ("id", "description", "status", "backend", "started_ms", "finished_ms", "duration_ms", "error", "tools")


def cells(root):
    out = subprocess.run(
        ["/usr/local/bin/celln", "ps", "-a", "--json", "--root", root],
        check=True, capture_output=True, text=True, timeout=20,
    ).stdout
    records = []
    for line in out.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if event.get("event") == "cell":
            records.append({k: event.get(k) for k in CELL_FIELDS})
    live = [c for c in records if c["status"] == "running"]
    finished = [c for c in records if c["status"] != "running"][:KEEP_FINISHED]
    return live + finished


def read_json(path):
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, ValueError):
        return None


def parents(root):
    journal = os.path.join(root, "parent-journal")
    try:
        entries = [os.path.join(journal, name) for name in os.listdir(journal)]
    except OSError:
        return []
    entries = sorted((e for e in entries if os.path.isdir(e)), key=os.path.getmtime, reverse=True)
    out = []
    for entry in entries[:KEEP_PARENTS]:
        parent = read_json(os.path.join(entry, "parent.json"))
        if not parent or not parent.get("parent"):
            continue
        turns = {}
        for name in os.listdir(entry):
            parts = name.split(".")
            if len(parts) != 3 or parts[2] != "json" or parts[1] not in ("reserved", "destroyed", "committed"):
                continue
            record = read_json(os.path.join(entry, name)) or {}
            turn = turns.setdefault(parts[0], {"stage": "reserved"})
            if parts[1] == "reserved":
                turn["turnId"] = (record.get("request") or {}).get("turnId", "")
                turn["timeoutMs"] = (record.get("timeout_nanos") or 0) // 1_000_000
            elif parts[1] == "destroyed":
                turn["succeeded"] = bool(record.get("succeeded"))
                if turn["stage"] != "committed":
                    turn["stage"] = "child-destroyed"
            else:
                turn["stage"] = "committed"
            if record.get("child"):
                turn["child"] = record["child"]
        out.append({
            "incarnation": parent["parent"],
            "updatedMs": int(os.path.getmtime(entry) * 1000),
            "turns": sorted(turns.values(), key=lambda t: t.get("turnId", "")),
        })
    return out


def snapshot(root, node):
    return {
        "apiVersion": "sympozium.ai/celln-node-cells-v1",
        "node": node,
        "cells": cells(root),
        "parents": parents(root),
    }


def main():
    if len(sys.argv) != 4 or sys.argv[1] != "snapshot":
        sys.exit("usage: fleet-cells.py snapshot ROOT NODE")
    report = snapshot(sys.argv[2], sys.argv[3])
    report["reportedMs"] = int(time.time() * 1000)
    print(json.dumps(report, separators=(",", ":")))


if __name__ == "__main__":
    main()
