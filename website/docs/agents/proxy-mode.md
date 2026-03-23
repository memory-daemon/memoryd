---
sidebar_position: 2
title: Proxy Mode
---

# Proxy Mode

Proxy mode is the zero-effort way to connect Claude Code (or any Anthropic-based tool) to your team's shared knowledge. Set one environment variable and work normally — knowledge capture happens automatically in the background.

## How it works

memoryd runs a local proxy on each team member's machine. When they point their AI tool at it, the proxy:

1. **On the way out** — forwards the request to the LLM provider exactly as-is, with no modifications
2. **On the way back** — streams the response to the developer in real-time, then asynchronously processes the conversation through the [knowledge capture pipeline](../how-it-works/write-path) and stores it

The proxy is a true passthrough — it never modifies prompts or responses. Knowledge capture happens entirely in the background after the response is delivered. The developer doesn't change their workflow. Knowledge just accumulates.

To **retrieve** stored knowledge, team members use the [MCP server](mcp-server). The MCP tools (`memory_search`, `memory_list`, etc.) are how AI tools access the shared knowledge base.

```
AI tool → memoryd proxy (local) → Anthropic API
               ↓                       ↓
        passthrough (no changes)  stream response back
                                       ↓
                              capture async (background)
                                       ↓
                              shared Atlas store
```

## Setup

```bash
memoryd start
export ANTHROPIC_BASE_URL=http://127.0.0.1:7432
```

That's it. Launch Claude Code and work normally.

## What gets captured

Both sides of every conversation feed the knowledge store:

| Source | What's captured |
|---|---|
| **Questions** | What the developer asked — signals what topics matter |
| **Responses** | The AI's answers — architecture decisions, debugging steps, explanations |

Everything goes through the same pipeline — [noise filtering, secret scrubbing, deduplication](../how-it-works/write-path). There's no risk of secrets or garbage entering the shared store.

## Real-time streaming

memoryd handles streaming responses (SSE) natively. The response streams to the developer in real-time; memoryd buffers a copy in the background for processing. Developers never experience any slowdown.

## Built-in dashboard

The proxy also serves a dashboard at `http://localhost:7432` where team members can:

- Browse stored knowledge
- Search the knowledge base
- View quality statistics
- Monitor ingested sources

## Proxy + MCP: the recommended setup

Proxy mode handles **automatic capture** — it passively records knowledge from every session. The MCP server handles **everything else** — retrieval, explicit storage, and knowledge maintenance.

Most teams use both together:

| Component | Role |
|---|---|
| **Proxy** | Automatic knowledge capture from every conversation |
| **MCP server** | Knowledge retrieval, search, explicit store, and maintenance (updating outdated items) |

For Claude Code, both run simultaneously — proxy captures in the background while MCP tools give the agent full access to the knowledge base, including the ability to correct outdated information. For Cursor, Windsurf, and other tools, MCP handles both capture and management.

They all feed and read from the same shared store.
