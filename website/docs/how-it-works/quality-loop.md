---
sidebar_position: 3
title: Quality Maintenance
---

# Quality Maintenance

memoryd doesn't just accumulate knowledge — it curates it. A background process called the **steward** continuously scores, cleans, and deduplicates the shared knowledge base.

## Why this matters for teams

Without curation, a shared knowledge store degrades quickly. Outdated patterns, one-off debugging conversations, and redundant explanations crowd out the signal. The problem is worse with teams — more contributors means more noise.

The steward solves this by treating quality as a feedback loop: knowledge that helps people answer questions survives and rises to the top. Knowledge that doesn't, fades away. No one needs to manually curate anything.

## Learning period

For the first 50 retrieval events, the steward stays hands-off. Everything is kept while the system learns what your team actually finds useful. Once enough usage data accumulates, quality-aware filtering activates automatically.

## The maintenance cycle

Every 60 minutes (configurable), the steward runs a three-phase sweep:

### Phase 1: Score

Every knowledge item gets a quality score based on two factors:

- **Usage** — How often has this been retrieved? Items that are frequently surfaced in team members' sessions score higher.
- **Recency** — How recently was this last useful? Unused knowledge decays over time (90-day half-life by default).

New items start with a neutral score and aren't penalized until they've had a chance to prove their value.

### Phase 2: Clean up

A knowledge item is removed only when **all three** conditions are met:

1. **Old enough** — exists for more than 24 hours (grace period)
2. **Low quality** — score below the pruning threshold
3. **Never retrieved** — zero evidence anyone found it useful

This is deliberately conservative. Even a single retrieval saves an item from cleanup. The system only removes knowledge that has genuinely zero evidence of value.

### Phase 3: Deduplicate

When multiple team members learn the same thing independently (which happens constantly), near-duplicate items accumulate. The steward identifies pairs that are semantically very similar (≥ 88% similarity) and keeps the one with more usage signal.

This is especially valuable for teams: three engineers debug the same service issue in the same week — the steward merges the redundant entries into one high-quality item.

## How this helps at scale

| Team size | Steward impact |
|---|---|
| **Small team (3-5)** | Mostly dedup and noise removal. Keeps the store clean as people ramp up. |
| **Medium team (5-15)** | Cross-contributor dedup becomes significant. Quality scoring surfaces the team's most valuable knowledge. |
| **Large team (15+)** | Essential. Without curation, the store would become too noisy for effective retrieval. The steward keeps signal-to-noise high. |

## Adaptive noise filtering

Beyond the steward's post-storage maintenance, memoryd also filters noise **before** knowledge enters the store. This happens through a multi-stage pipeline:

1. **Pre-filter** — Fast string matching rejects obviously procedural exchanges (no LLM cost)
2. **Length gate** — Very short responses (< 80 chars) are skipped
3. **Content score gate** — Responses are scored against learned noise prototypes. Below the threshold, they're skipped before any LLM call
4. **LLM quality gate** — A lightweight model (Claude Haiku) judges whether the exchange has durable value

The content scorer **adapts over time**: rejected exchanges are accumulated in a ring buffer, and every 25 rejections the noise prototypes are rebuilt from actual rejected content. This means the system learns your team's specific noise patterns — the kind of procedural back-and-forth that happens in your codebase's conversations.

## Configuration

All steward settings are tunable in `config.yaml`. See [Configuration](../configuration) for the full reference and team-specific tuning recommendations.
