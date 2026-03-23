---
sidebar_position: 1
title: MCP Server
---

# MCP Server

The MCP (Model Context Protocol) server is how memoryd connects to **any AI coding tool** — Cursor, Windsurf, Cline, Claude Code, custom pipelines, or anything that speaks MCP. It's the universal integration point.

## Why MCP matters for teams

Different team members use different tools. That's fine. MCP is a standard protocol that lets any compatible tool search and store knowledge in the same shared database. The tool doesn't matter — the knowledge store is the product.

Alice uses Claude Code (via proxy), Bob uses Cursor (via MCP), Carol has a custom pipeline (via MCP). They all contribute to and benefit from the same team knowledge.

## Setup

Add memoryd to your tool's MCP configuration:

```json
{
  "mcpServers": {
    "memoryd": {
      "command": "memoryd",
      "args": ["mcp"]
    }
  }
}
```

This works with Claude Code, Cursor, Windsurf, Cline, and any tool that supports MCP servers. The memoryd daemon must be running (`memoryd start`) for MCP tools to work.

## Available tools

The MCP server gives AI tools access to 8 capabilities:

### Core knowledge operations

| Tool | What it does |
|---|---|
| `memory_search` | Search the shared knowledge base with a natural language query |
| `memory_store` | Store new knowledge (auto-deduplicated, noise-filtered, secrets scrubbed) |
| `memory_list` | Browse stored knowledge, optionally filtered by text |
| `memory_delete` | Remove a specific knowledge item |

### Source ingestion

| Tool | What it does |
|---|---|
| `source_ingest` | Crawl a URL (wiki, docs site) and add its content to the knowledge base |
| `source_list` | List all ingested sources and their status |
| `source_remove` | Remove a source and all its associated knowledge |

### Quality monitoring

| Tool | What it does |
|---|---|
| `quality_stats` | Check the knowledge base health — retrieval counts, learning status |

## How it fits together

```
Any AI tool ←→ MCP (stdin/stdout) ←→ memoryd ←→ Shared Atlas store
```

The MCP server is the **full knowledge management interface** — read, write, update, and delete. It's how AI tools interact with the shared knowledge base:

- **Search** (`memory_search`) — runs the [retrieval pipeline](../how-it-works/read-path) with hybrid search, quality filtering, and diversity optimization
- **Store** (`memory_store`) — adds new knowledge through the [capture pipeline](../how-it-works/write-path) with noise filtering, secret scrubbing, and deduplication
- **Delete** (`memory_delete`) — removes outdated or incorrect knowledge items
- **Browse** (`memory_list`) — lists stored knowledge, optionally filtered

The proxy handles **automatic capture** from conversations. MCP handles **everything else** — retrieval, explicit storage, and knowledge maintenance.

## The good citizen pattern

AI tools that connect via MCP should actively maintain knowledge quality — not just consume it. When an AI tool retrieves knowledge via `memory_search` and discovers that the information is **outdated, inaccurate, or no longer relevant**, it should correct the record:

1. **Delete** the outdated item with `memory_delete`
2. **Store** the corrected version with `memory_store`

This matters at team scale. A deployment procedure changes, an API gets deprecated, an architecture decision gets reversed — without active maintenance, stale knowledge poisons the store for everyone. When every team member's AI tool acts as a good citizen, the knowledge base stays current organically.

For example: Copilot retrieves a memory about the deploy process that references an old CI pipeline. The developer corrects it during the session. Copilot should delete the outdated memory and store the updated version — so the next person who asks about deployments gets the right answer.

**This is the expected behavior.** AI tools using memoryd should treat knowledge maintenance as part of their workflow, not an afterthought.

## Read-only usage

Any tool can connect via MCP with `--read-only` and **only use `memory_search`** — consuming team knowledge without contributing or modifying. This is useful for:

- Tools that should read but not write (evaluation, auditing)
- Team members in security-sensitive contexts
- Trial periods before committing to full integration

Note: read-only tools can't participate in the [good citizen pattern](#the-good-citizen-pattern) above. For full knowledge health, at least some team members should run in full mode.

See [Read-Only Mode](read-only-mode) for more on this pattern.

## For teams using multiple tools

A common team setup:

| Team member | Tool | Connection | Contribution |
|---|---|---|---|
| Alice | Claude Code | Proxy + MCP | Automatic capture via proxy, retrieval + knowledge maintenance via MCP |
| Bob | Cursor | MCP | Agent searches, stores, and maintains knowledge |
| Carol | Custom pipeline | MCP (read-only) | Reads team knowledge, doesn't write or modify |
| Dave | Claude Code + Cursor | Proxy + MCP | Full capture + full knowledge management |

All four draw from and contribute to the same knowledge pool.
