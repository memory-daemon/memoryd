#!/usr/bin/env python3
"""Clean cross-run analysis excluding errors."""
import json

dataset = []
with open("data/eval-large.jsonl") as f:
    for line in f:
        dataset.append(json.loads(line))

for run_idx, fname in enumerate(["benchmark-1k-r1.jsonl", "benchmark-1k-r2.jsonl", "benchmark-1k-r3.jsonl"], 1):
    results = []
    with open(fname) as fh:
        for line in fh:
            results.append(json.loads(line))

    valid = [r for r in results if r["stage"] != "error"]
    total_valid = len(valid)

    stages = {}
    for r in valid:
        stages[r["stage"]] = stages.get(r["stage"], 0) + 1

    pre_haiku = stages.get("pre_filter", 0) + stages.get("length_filter", 0) + stages.get("content_score_filter", 0)
    haiku_calls = stages.get("synthesizer_skip", 0) + stages.get("stored", 0)

    mixed_sub_total = 0
    mixed_sub_stored = 0
    for r in valid:
        idx = r["index"]
        lbl = dataset[idx].get("label", "")
        resp = dataset[idx].get("assistant_response", "")
        prompt = dataset[idx].get("user_prompt", "")
        is_mixed = "hyperswitch" not in resp.lower() and "hyperswitch" not in prompt.lower()
        if lbl == "substantive" and is_mixed:
            mixed_sub_total += 1
            if r["stage"] == "stored":
                mixed_sub_stored += 1

    print(f"Run {run_idx} (excl {len(results)-len(valid)} errors):")
    print(f"  Valid entries:    {total_valid}")
    print(f"  Pre-Haiku:       {pre_haiku} ({pre_haiku/total_valid*100:.0f}%)")
    print(f"    length_filter:   {stages.get('length_filter', 0)}")
    print(f"    content_score:   {stages.get('content_score_filter', 0)}")
    print(f"  Haiku calls:     {haiku_calls} ({haiku_calls/total_valid*100:.0f}%)")
    print(f"    stored:          {stages.get('stored', 0)}")
    print(f"    synth_skip:      {stages.get('synthesizer_skip', 0)}")
    print(f"  Hand-crafted substantive recall: {mixed_sub_stored}/{mixed_sub_total} ({mixed_sub_stored/max(mixed_sub_total,1)*100:.0f}%)")
    print()
