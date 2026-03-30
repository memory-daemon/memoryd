#!/usr/bin/env python3
"""Detailed cross-run analysis for the 1000-row benchmark."""
import json, sys, collections

dataset_path = "data/eval-large.jsonl"
run_files = ["benchmark-large1.jsonl", "benchmark-large2.jsonl", "benchmark-large3.jsonl"]

# Load ground-truth labels and origin
labels = {}
origins = {}  # "mixed" or "hf"
with open(dataset_path) as f:
    for i, line in enumerate(f):
        d = json.loads(line)
        labels[i] = d.get("label", "unknown")
        # Hand-crafted entries have shorter responses (< 2000 chars typically)
        # and specific labels, while HF responses can be very long
        # We can identify by checking if user_prompt is from eval-mixed patterns
        resp = d.get("assistant_response", "")
        # Simple heuristic: eval-mixed entries don't contain hyperswitch references
        if "hyperswitch" in resp.lower() or "hyperswitch" in d.get("user_prompt", "").lower():
            origins[i] = "hf"
        elif len(resp) < 3000 and labels[i] in ("noise", "low", "substantive"):
            origins[i] = "mixed"  # likely hand-crafted
        else:
            origins[i] = "hf"

for run_idx, run_file in enumerate(run_files, 1):
    results = []
    with open(run_file) as f:
        for line in f:
            results.append(json.loads(line))

    print(f"{'='*60}")
    print(f"RUN {run_idx}: {run_file}")
    print(f"{'='*60}")

    # Stage counts
    stages = collections.Counter(r["stage"] for r in results)
    print(f"\nStage distribution:")
    for stage in ["pre_filter", "length_filter", "content_score_filter", "synthesizer_skip", "stored", "error"]:
        print(f"  {stage:<24} {stages.get(stage, 0):>4}")

    # Count Haiku calls = total - pre_filter - length_filter - content_score_filter
    pre_haiku = stages.get("pre_filter", 0) + stages.get("length_filter", 0) + stages.get("content_score_filter", 0)
    haiku_calls = len(results) - pre_haiku - stages.get("error", 0)
    print(f"\n  Pre-Haiku filtered:      {pre_haiku}")
    print(f"  Haiku calls:             {haiku_calls}")

    # Substantive recall split by origin
    print(f"\nSubstantive recall by origin:")
    for origin in ["mixed", "hf"]:
        stored = 0
        total = 0
        for r in results:
            idx = r["index"]
            if labels.get(idx) == "substantive" and origins.get(idx) == origin:
                total += 1
                if r["stage"] == "stored":
                    stored += 1
        if total > 0:
            print(f"  {origin:>5}: {stored}/{total} stored ({stored/total*100:.0f}%)")

    # Filtered substantive by stage, split by origin
    print(f"\nFiltered substantive by stage+origin:")
    for origin in ["mixed", "hf"]:
        stage_counts = collections.Counter()
        for r in results:
            idx = r["index"]
            if labels.get(idx) == "substantive" and origins.get(idx) == origin and r["stage"] != "stored":
                stage_counts[r["stage"]] += 1
        if stage_counts:
            print(f"  {origin}: {dict(stage_counts)}")

    print()
