---
sidebar_position: 2
title: How Knowledge is Captured
---

# How Knowledge is Captured

Every AI interaction across your team is automatically processed into reusable knowledge — with zero impact on response speed.

## The flow

```
AI response → Pre-filter → Length check → Noise score → LLM quality gate → Break into pieces → Filter → Scrub secrets → Deduplicate → Store
```

### 1. Pre-filter cheap noise

Before any expensive processing, a fast string-matching filter checks if the exchange is purely procedural — a user saying "ok" and an assistant responding "I'll update that for you." These are rejected instantly with no LLM cost.

Rejected exchanges are logged and used to **teach the system what noise looks like** (see adaptive learning below).

### 2. Check response length

Very short responses (under 80 characters by default) almost never contain durable knowledge. These are skipped before making any LLM call, saving cost and reducing noise.

### 3. Score against noise prototypes

The raw response text is embedded and compared against learned noise prototypes — patterns of content that previous rejections have established as low-value. If the score falls below the threshold (0.35 by default), the response is skipped.

This gate is **adaptive**: as the system sees more noise, it gets better at recognizing it. The prototypes are rebuilt every 25 rejections from a ring buffer of the 500 most recent rejected exchanges.

### 4. LLM quality gate

Responses that pass the automated filters go to an LLM (Claude Haiku) for final judgment. The model either distills the exchange into a concise knowledge entry or returns "SKIP" if there's no durable value. This catches nuanced noise that pattern matching misses.

### 5. Break into meaningful pieces

Raw conversation text is split into knowledge-sized chunks at natural boundaries — paragraph breaks, logical sections. A function explanation stays together. A list of deployment steps stays together. Only oversized blocks get split further.

### 6. Filter noise

Not everything in a conversation is worth keeping. memoryd automatically filters:

- **Too short** — fragments under 20 characters (code snippets, acknowledgments)
- **Not natural language** — binary data, ASCII art, raw output with less than 40% readable text

### 7. Scrub secrets

**Before anything is stored**, memoryd scrubs sensitive content using 13 detection patterns:

| What's detected | Examples |
|---|---|
| Cloud credentials | AWS access keys, AWS secret keys |
| API tokens | GitHub tokens, Slack tokens, Stripe keys |
| Authentication secrets | JWTs, Bearer tokens, private keys, SSH keys |
| Connection strings | Database URIs with embedded passwords |
| Generic secrets | Key-value pairs containing `password`, `secret`, `token`, `api_key` |

Detected values are replaced with safe placeholders (e.g., `[REDACTED:AWS_KEY]`). **Secrets never enter the shared knowledge store.** This is critical for team deployments where multiple people contribute to the same database.

### 8. Deduplicate

Each new piece of knowledge is compared against what's already in the store:

| Similarity | What happens |
|---|---|
| **Very high** (≥ 92%) | Already known — skip |
| **Moderate** (≥ 75%, from a source) | Related to existing reference material — stored with a link back to the original |
| **Low** | Novel knowledge — stored normally |

This is especially valuable for teams: when three engineers independently learn the same thing about a service, it's stored once — not three times.

### 9. Store

Each surviving piece becomes a knowledge item in the shared MongoDB store, available to every team member's AI tools immediately.

## Why async matters

The entire capture pipeline runs in the background. The AI response streams back to the developer in real-time — memoryd processes it after delivery. Team members never experience any slowdown from the knowledge capture process.

## Adaptive noise learning

The pre-LLM gates aren't static — they improve over time. Here's how:

1. **Rejected exchanges accumulate** — Every exchange rejected by the pre-filter or the LLM quality gate is logged to a ring buffer (500 most recent).
2. **Noise prototypes are rebuilt** — Every 25 rejections, the assistant texts are re-embedded as noise prototypes.
3. **The content scorer improves** — New prototypes are hot-swapped into the scoring system, so the noise detection adapts to your team's specific patterns.
4. **Persistence across restarts** — The rejection log is saved to disk, so the system doesn't lose its learned noise patterns when the daemon restarts.

The content score gate does **not** feed rejected exchanges back into the rejection store — this prevents a positive feedback loop where the scorer would amplify its own signal.

## What builds over time

After a team has been using memoryd for a few weeks, the shared store typically contains:

- **Architecture decisions** — why the team chose specific patterns, from the conversations where those decisions were made
- **Debugging playbooks** — how to diagnose common issues, from actual debugging sessions
- **Deployment knowledge** — environment config, migration procedures, rollback steps
- **Codebase conventions** — naming patterns, error handling approaches, testing strategies
- **Integration details** — how services connect, what APIs expect, edge cases discovered in practice

All of it captured organically, with secrets scrubbed and duplicates merged.
