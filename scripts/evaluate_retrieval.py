#!/usr/bin/env python3
"""Evaluate candidate and displayed governing-evidence recall without model calls.

Cases are JSONL objects: task_id, query, governing_ids, split (dev or test).
Task groups must not cross splits. Labels must come from independently recorded
source evidence, never from the retriever's own output. Output rows are durable
and resumable, keyed by task/engine/budget and bound to journal/cases hashes.
"""
from __future__ import annotations
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import re
import signal
import statistics
import subprocess
import tempfile
import time


def measure(command: list[str]) -> tuple[str, float, int | None]:
    """Run an actual retrieval command with bounded output, time and resources."""
    with tempfile.TemporaryDirectory(prefix="curator-eval-") as tmp:
        timing = Path(tmp) / "time"
        timed = Path("/usr/bin/time").is_file()
        wrapped = ["/usr/bin/time", "-f", "%M", "-o", str(timing), *command] if timed else command
        started = time.monotonic()
        process = subprocess.Popen(wrapped, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
        try:
            stdout, stderr = process.communicate(timeout=30)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.communicate()
            raise RuntimeError(f"retrieval timed out: {command[1]}") from None
        elapsed = time.monotonic() - started
        if process.returncode:
            raise RuntimeError(f"retrieval failed ({process.returncode}): {stderr.decode(errors='replace')[:2000]}")
        if len(stdout) > 1_048_576:
            raise RuntimeError("retrieval output exceeded 1 MiB")
        peak = int(timing.read_text().strip()) if timed else None
        return stdout.decode(), elapsed, peak


def evaluate(args: argparse.Namespace) -> None:
    source = args.repo / ".curator/journal/events.jsonl"
    snapshot = source.stat()
    binary_hash = hashlib.sha256(args.binary.read_bytes()).hexdigest()
    evaluator_hash = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    with source.open("rb") as stream:
        source_hash = hashlib.file_digest(stream, "sha256").hexdigest()
    with args.cases.open("rb") as stream:
        cases_hash = hashlib.file_digest(stream, "sha256").hexdigest()
    if args.cases.stat().st_size > 16_777_216:
        raise ValueError("cases exceed 16 MiB")
    cases = [json.loads(line) for line in args.cases.read_text().splitlines() if line.strip()]
    if not cases or len(cases)>10000:
        raise ValueError("need 1..10000 independently labeled cases")
    tasks: dict[str, str] = {}
    for case in cases:
        task, split = case["task_id"], case["split"]
        if split not in ("dev", "test") or task in tasks:
            raise ValueError("task groups must be unique and use dev/test splits")
        tasks[task] = split
        if not case["governing_ids"] or not case["query"]:
            raise ValueError(f"missing labels/query for {task}")
        if len(case["governing_ids"])>50 or len(case["query"].encode())>4096:
            raise ValueError("case exceeds 50 labels or 4096 query bytes")
        if "governing_texts" in case and set(case["governing_texts"]) != set(case["governing_ids"]):
            raise ValueError("governing text labels must cover exactly the governing IDs")
        if any(not isinstance(text,str) or not text.strip() for text in case.get("governing_texts",{}).values()):
            raise ValueError("governing text labels must be nonempty strings")
    args.out.mkdir(parents=True, exist_ok=True)
    rows_path = args.out / "retrieval.jsonl"
    if rows_path.exists() and rows_path.stat().st_size>67_108_864:
        raise ValueError("resume rows exceed 64 MiB")
    rows = [json.loads(line) for line in rows_path.read_text().splitlines()] if rows_path.exists() else []
    if any(row["source_sha256"] != source_hash or row["cases_sha256"] != cases_hash or row.get("binary_sha256")!=binary_hash or row.get("evaluator_sha256")!=evaluator_hash for row in rows):
        raise ValueError("resume inputs/retriever changed; use a new output directory")
    done = {(row["task_id"], row["engine"], row["budget"]) for row in rows}
    base = [str(args.binary), "memory-search", "--cwd", str(args.repo)]
    for case in cases:
        gold = set(case["governing_ids"])
        for engine in args.engines.split(","):
            for budget in args.budgets:
                key = (case["task_id"], engine, budget)
                if key in done:
                    continue
                common = [*base, "--query", case["query"], "--engine", engine, "--budget", str(budget)]
                raw, candidate_s, candidate_peak = measure([*common, "--format", "json", "--limit", "50"])
                candidates = [event_id for session in json.loads(raw)["sessions"] for hit in session["hits"] for event_id in (hit.get("evidence_event_ids") or [hit["event_id"]])]
                display, display_s, display_peak = measure(common)
                ids = re.findall(r"^ID ([^ |]+) ", display, flags=re.MULTILINE)
                if engine=="episodes":
                    ids.extend(re.findall(r"event:([A-Za-z0-9][A-Za-z0-9._:@/-]{0,127})",display))
                current = source.stat()
                if (current.st_ino,current.st_size,current.st_mtime_ns)!=(snapshot.st_ino,snapshot.st_size,snapshot.st_mtime_ns):
                    raise RuntimeError("journal changed during evaluation; use an immutable fixture")
                evidence = case.get("governing_texts", {})
                normalized = " ".join(display.split())
                full_recall = sum(" ".join(text.split()) in normalized for text in evidence.values()) / len(evidence) if evidence else None
                row = {"task_id": key[0], "split": case["split"], "engine": engine, "budget": budget,
                       "source_sha256": source_hash, "cases_sha256": cases_hash,
                       "binary_sha256": binary_hash, "evaluator_sha256": evaluator_hash,
                       "candidate_ids": candidates, "display_ids": ids, "governing_ids": sorted(gold),
                       "candidate_recall": len(gold.intersection(candidates)) / len(gold),
                       "display_recall": len(gold.intersection(ids)) / len(gold),
                       "full_evidence_recall": full_recall,
                       "memory_output_estimated_tokens": math.ceil(len(display.encode()) / 4),
                       "candidate_wall_s": candidate_s, "display_wall_s": display_s,
                       "candidate_peak_kib": candidate_peak, "display_peak_kib": display_peak}
                with rows_path.open("a") as output:
                    output.write(json.dumps(row, sort_keys=True) + "\n")
                    output.flush()
                    os.fsync(output.fileno())
                rows.append(row)
                done.add(key)
    report = {"accounting": "memory output uses ceil(UTF-8 bytes/4), not model tokenization",
              "scope": "offline recall/performance only; downstream success and causal usefulness require paired agent runs",
              "groups": []}
    for split, engine, budget in sorted({(r["split"], r["engine"], r["budget"]) for r in rows}):
        group = [r for r in rows if (r["split"], r["engine"], r["budget"]) == (split, engine, budget)]
        full = [r["full_evidence_recall"] for r in group if r.get("full_evidence_recall") is not None]
        report["groups"].append({"split": split, "engine": engine, "budget": budget, "tasks": len(group),
                                 "full_evidence_recall": statistics.mean(full) if full else None,
                                 **{field: statistics.mean(r[field] for r in group) for field in
                                    ("candidate_recall", "display_recall", "memory_output_estimated_tokens", "candidate_wall_s", "display_wall_s")},
                                 "max_process_peak_kib": max((r["candidate_peak_kib"] or 0 for r in group), default=0)})
    (args.out / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", required=True, type=Path)
    parser.add_argument("--cases", required=True, type=Path)
    parser.add_argument("--out", required=True, type=Path)
    parser.add_argument("--binary", type=Path, default=Path(__file__).resolve().parents[1] / "curator/bin/curator")
    parser.add_argument("--engines", default="legacy,fts,hybrid")
    parser.add_argument("--budgets", type=int, nargs="+", default=[100, 250])
    args = parser.parse_args()
    if any(engine not in ("legacy", "fts", "hybrid", "episodes") for engine in args.engines.split(",")):
        parser.error("engines must be legacy,fts,hybrid,episodes")
    if not 1 <= len(args.budgets) <= 8 or any(budget<64 or budget>8192 for budget in args.budgets):
        parser.error("need 1..8 budgets in 64..8192")
    evaluate(args)


if __name__ == "__main__":
    main()
