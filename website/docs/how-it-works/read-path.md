---
sidebar_position: 1
title: How Knowledge is Retrieved
---

# How Knowledge is Retrieved

Team members access the shared knowledge base through memoryd's **MCP server**. When an AI tool calls `memory_search`, memoryd finds the most relevant knowledge and returns it directly to the tool.

## The flow

```
AI tool calls memory_search → memoryd finds relevant knowledge → results returned to the tool
```

### 1. Understand the query

When an AI tool calls `memory_search` with a query, memoryd converts it into a mathematical representation (an embedding) that captures its meaning — not just keywords, but concepts.

This happens locally on every team member's machine using a lightweight model. No data leaves the machine for this step.

### 2. Search the shared store

memoryd searches the team's shared knowledge base for relevant items. The search strategy depends on your setup:

| Setup | How search works |
|---|---|
| **Atlas cluster** (recommended) | **Hybrid search** — combines meaning-based similarity with keyword matching, then diversifies results. Best for teams. |
| **Local MongoDB** (solo dev) | **Vector search** — meaning-based similarity only. Good for individual use. |

With Atlas, the search also filters out low-quality items automatically — noise that was captured but never proved useful doesn't clutter the results.

See [Hybrid Search](hybrid-search) for how the Atlas search pipeline works.

### 3. Return results

The most relevant knowledge items are returned to the AI tool via MCP. The tool receives the actual content — past debugging sessions, architecture decisions, deployment procedures — and can use it as context for its current task.

A result limit (default: 5 items, configurable via `retrieval_top_k`) controls how many items are returned per search.

### 4. Quality feedback

When knowledge items are retrieved, memoryd records that they were useful. Over time, items that are retrieved frequently earn higher quality scores. Items that are never retrieved eventually decay and are cleaned up by the [quality maintenance](quality-loop) process.

This creates a virtuous cycle: the more your team uses memoryd, the better the knowledge quality becomes.
