#!/usr/bin/env python3
"""Analyze the HF dataset for diversity and create a larger benchmark dataset."""
import json
import random
import collections
import sys

def analyze():
    rows = []
    with open("data/dataset-hf.jsonl") as f:
        for line in f:
            rows.append(json.loads(line))

    print(f"Total rows: {len(rows)}")

    # Check diversity by looking at user_prompt first 50 chars
    prefixes = collections.Counter()
    for r in rows:
        p = r.get("user_prompt", "")[:50]
        prefixes[p] += 1

    print(f"Unique prompt prefixes (50ch): {len(prefixes)}")
    print("Top 10:")
    for prefix, count in prefixes.most_common(10):
        print(f"  {count:>5}x  {repr(prefix[:60])}")

    # Check response length distribution
    lens = [len(r.get("assistant_response", "")) for r in rows]
    lens.sort()
    print(f"\nResponse length: min={lens[0]}, p25={lens[len(lens)//4]}, "
          f"median={lens[len(lens)//2]}, p75={lens[3*len(lens)//4]}, max={lens[-1]}")

    # Check for content type diversity in random 1000
    random.seed(42)
    sample = random.sample(rows, 1000)
    short = sum(1 for r in sample if len(r.get("assistant_response", "")) < 80)
    medium = sum(1 for r in sample if 80 <= len(r.get("assistant_response", "")) < 500)
    long_resp = sum(1 for r in sample if len(r.get("assistant_response", "")) >= 500)
    print(f"\nRandom 1000 sample: short(<80)={short}, medium(80-500)={medium}, long(500+)={long_resp}")

    # Check for actual content patterns in the sample
    ack_patterns = ["Sure", "I'll", "Let me", "Here", "OK", "Done", "Got it", "Understood"]
    ack_count = 0
    code_count = 0
    for r in sample:
        resp = r.get("assistant_response", "")
        if any(resp.strip().startswith(p) for p in ack_patterns) and len(resp) < 200:
            ack_count += 1
        if "```" in resp or "func " in resp or "def " in resp or "class " in resp:
            code_count += 1

    print(f"Acknowledgments (short + starts with ack pattern): {ack_count}")
    print(f"Contains code blocks/definitions: {code_count}")

    # Show sample of short responses
    print("\nSample short responses:")
    short_samples = [r for r in sample if len(r.get("assistant_response", "")) < 100]
    random.shuffle(short_samples)
    for r in short_samples[:10]:
        resp = r.get("assistant_response", "").strip()[:100]
        print(f"  [{len(r['assistant_response']):>4}ch] {repr(resp)}")

    # Show some medium responses
    print("\nSample medium responses:")
    med_samples = [r for r in sample if 200 <= len(r.get("assistant_response", "")) < 600]
    random.shuffle(med_samples)
    for r in med_samples[:5]:
        resp = r.get("assistant_response", "").strip()[:120]
        print(f"  [{len(r['assistant_response']):>4}ch] {repr(resp)}")

if __name__ == "__main__":
    analyze()
