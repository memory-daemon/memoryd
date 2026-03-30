---
slug: /
sidebar_position: 1
title: What is memoryd?
---

# memoryd

**A shared knowledge layer for engineering teams — built automatically from the work you're already doing.**

Every AI coding session starts cold. The agent doesn't remember the architecture decisions from last sprint, the deployment procedure your team refined over months, or the workaround for that edge case someone debugged last Tuesday. Teams compensate by re-explaining context, maintaining wikis no one updates, and hoping institutional knowledge doesn't walk out the door.

memoryd changes this. It's a lightweight service that sits alongside your team's AI coding tools, transparently capturing knowledge from every session and making it available to everyone. Over time, your team builds an always-current knowledge base — without anyone stopping to write documentation.

```
Your Team's Agents → memoryd → LLM Provider
                        ↕
               MongoDB Atlas (shared)
```

## How it fits into your team

Every team member runs memoryd locally. All instances connect to a **shared MongoDB Atlas cluster** — the same database your org already knows how to manage. When one engineer debugs an infrastructure issue, the resolution is available to everyone's AI tools. Architecture decisions, API patterns, onboarding context — it all accumulates organically from daily work.

**No one writes documentation. No one maintains a wiki. Knowledge builds itself.**

| Role | How they benefit |
|------|-----------------|
| **Engineers** | Their AI tools have context from past sessions — theirs and their teammates'. Less re-explaining, fewer repeated mistakes. |
| **Engineering Managers** | Institutional knowledge is retained even through team turnover. Onboarding accelerates as new hires inherit the team's accumulated context. |
| **PMs & TPMs** | Cross-team knowledge flows naturally. One team's learnings surface for others working in adjacent areas. |
| **Platform / DevOps** | Operational knowledge — deployment procedures, incident resolutions, infrastructure quirks — persists and spreads across the org. |

→ [Team Knowledge Hub](team-knowledge-hub) explores the team vision in depth, including how knowledge can be scoped to teams and business units.

## Works with any AI coding tool

memoryd is **tool-agnostic**. It connects to your team's tools through two interfaces:

| Interface | Best for | How it works |
|-----------|----------|-------------|
| **[Proxy mode](agents/proxy-mode)** | Claude Code, any Anthropic-based tool | Passthrough — set one environment variable, work normally. Conversations captured automatically in the background. |
| **[MCP server](agents/mcp-server)** | Cursor, Windsurf, Cline, custom tools | Standard protocol — agent searches, stores, and maintains knowledge via tool calls. |

Teams don't need to standardize on one tool. Alice uses Claude Code, Bob uses Cursor, Carol has a custom pipeline — they all feed and draw from the same knowledge store. There's also a **[read-only mode](agents/read-only-mode)** for teams or tools that should consume knowledge without contributing.

## What memoryd handles automatically

You don't need to understand the internals to use it, but here's what's happening under the hood:

1. **[Knowledge capture](how-it-works/write-path)** — Every AI interaction routed through the proxy is automatically filtered through multi-stage noise detection (adaptive content scoring, LLM quality gates), scrubbed of secrets (API keys, tokens, passwords), deduplicated against what's already known, and stored. The system learns what noise looks like from your team's specific patterns.

2. **[Knowledge retrieval & maintenance](how-it-works/read-path)** — AI tools access and manage the shared knowledge base through MCP tools — searching, storing, and updating knowledge. When a tool receives outdated information, it corrects the record so the whole team benefits.

3. **[Quality maintenance](how-it-works/quality-loop)** — A background process continuously scores knowledge by how useful it's been, removes noise, and merges near-duplicates. The store stays clean without manual curation.

4. **[Intelligent search](how-it-works/hybrid-search)** — On Atlas, retrieval combines meaning-based and keyword-based search with diversity optimization. It finds both semantically related and exact-match results.

## Getting started

A team lead or platform engineer typically handles the one-time setup:

1. **Provision** a MongoDB Atlas cluster (free tier works for evaluation)
2. **Install** memoryd on each team member's machine
3. **Configure** everyone to point at the shared cluster

That's it. Team members work normally with their preferred AI tools, and the knowledge base builds in the background.

→ [Getting Started](getting-started) has the full setup guide.
