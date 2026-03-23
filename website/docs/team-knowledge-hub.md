---
sidebar_position: 5
title: Team Knowledge Hub
---

# Team Knowledge Hub

memoryd isn't just a tool for individual developers. It's a **shared knowledge layer for your engineering organization** — one that builds itself from the work your team is already doing.

## The problem memoryd solves

Every engineering team has hard-won institutional knowledge:

- How the deploy pipeline actually works (not what the stale wiki says)
- Why that config flag exists and when to change it
- What the payment service expects in edge cases
- How to diagnose that intermittent CI failure

This knowledge lives in people's heads, buried Slack threads, outdated Confluence pages, and tribal memory that walks out the door when someone leaves or changes teams. No one writes documentation — or when they do, it's outdated by the time it's published.

**memoryd captures this knowledge automatically, keeps it current, and makes it available to everyone's AI tools — without anyone stopping to write docs.**

## How it works

Every team member runs memoryd locally. All instances connect to a **shared MongoDB Atlas cluster**. When anyone uses their AI coding tools (Claude Code, Cursor, Windsurf, etc.), the knowledge from those sessions flows into the shared store.

```
Engineer A (Claude Code)  ──→                         ←── Engineer B (Cursor)
                               Shared Atlas Cluster
Engineer C (read-only)    ←──                         ←── Engineer D (Claude Code)
                                      ↕
                              Quality Maintenance
                          (dedup, scoring, pruning)
```

### Setup is minimal

Each team member adds one line to their config:

```yaml
mongodb_atlas_uri: "mongodb+srv://team-cluster.mongodb.net/?retryWrites=true"
atlas_mode: true
```

That's it. The knowledge store, quality signals, and cross-team deduplication all flow through the same database. Atlas handles the infrastructure.

### Different tools, same knowledge

The store is tool-agnostic. Team members can use whatever they prefer:

| Team member | Their tool | Integration | What happens |
|---|---|---|---|
| Alice | Claude Code | Proxy + MCP | Every session automatically captured; MCP tools for retrieval |
| Bob | Cursor | MCP server | Agent searches and stores via tool calls |
| Carol | Windsurf | MCP (read-only) | Consumes team knowledge, doesn't contribute |
| Dave | Custom pipeline | MCP server | Integrates with internal tooling |

All four benefit from the same knowledge pool. Alice's debugging session about the auth service helps Bob when he encounters the same issue next week. Carol's AI tool in Windsurf already knows the deployment procedure that Dave figured out last month.

### Quality at scale

The [quality maintenance system](how-it-works/quality-loop) becomes *more* valuable with a shared store:

- **Cross-contributor dedup** — when three engineers independently learn the same thing about a service, the system consolidates to a single knowledge item
- **Collective signal** — knowledge that gets retrieved across multiple team members' sessions earns a higher quality score faster
- **Natural pruning** — one-off debugging artifacts that are never useful to anyone else decay and disappear automatically

## Seeding your knowledge base

Teams can accelerate the ramp-up by ingesting existing documentation:

```bash
memoryd ingest "team-wiki" https://wiki.yourcompany.com/engineering
memoryd ingest "api-docs" https://docs.internal.yourcompany.com
```

Or upload files directly through the dashboard or CLI. Ingested sources live alongside organically captured knowledge and go through the same quality process — scored by actual usefulness, not by when they were written.

Over time, a search might return:

```
[1] (source: claude-code, relevance: 0.87)
The auth service rejects tokens older than 24h — need to refresh before calling...

[2] (source: source:internal-wiki, relevance: 0.82)
Production deployments require approval in #releases before merging to main...

[3] (source: mcp, relevance: 0.79)
The payment webhook validates signatures using HMAC-SHA256 with the secret from...
```

Reference documentation and real-world experience, side by side.

## What builds over time

After a team has been using memoryd for a few weeks:

| Knowledge type | How it's captured |
|---|---|
| **Architecture decisions** | From the conversations where they were made — including the "why" |
| **Debugging playbooks** | From actual debugging sessions, not theoretical runbooks |
| **Deployment procedures** | From real deploy sessions — current, not last year's wiki page |
| **Codebase conventions** | From code review discussions and implementation patterns |
| **Integration details** | From sessions working with APIs and services — edge cases included |
| **Onboarding context** | Accumulated from everyone — new hires inherit months of team knowledge on day one |

The knowledge is always current because it's built from current work. There's no documentation lag, no stale wiki, no "ask Sarah, she knows."

## Team-scoped knowledge (roadmap)

Today, all team members sharing an Atlas cluster contribute to and read from a single knowledge pool. This works well for teams and small organizations.

**Coming next: overlapping knowledge scopes aligned to teams and business units.**

The idea is simple. Different teams work in different domains — the payments team, the platform team, the mobile team. Each generates domain-specific knowledge. But teams also overlap — the payments team shares context with the platform team around deployments, and with the mobile team around API contracts.

```
┌─────────────────────────────────────────────────┐
│                  Organization                    │
│                                                  │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐      │
│  │ Payments │  │ Platform │  │  Mobile  │      │
│  │   Team   │  │   Team   │  │   Team   │      │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘      │
│       │              │              │            │
│       └──────┬───────┘              │            │
│              │                      │            │
│     Deploy knowledge         API contracts       │
│      (shared scope)          (shared scope)      │
│                                                  │
│            Org-wide knowledge                    │
│     (coding standards, CI/CD, security)          │
└─────────────────────────────────────────────────┘
```

With scoped knowledge:

- **Team scope** — each team's AI tools prioritize knowledge from their own domain
- **Shared scopes** — overlapping areas surface knowledge from all contributing teams
- **Org scope** — universal knowledge (coding standards, security practices, CI/CD) is available everywhere

A payment engineer's AI tool would see: payments-specific knowledge first, shared deployment knowledge second, and org-wide standards third. A new hire on the mobile team would inherit both mobile-specific and org-wide knowledge from day one.

This maps naturally to how organizations actually work — overlapping circles of context, not rigid silos.

## Participation is opt-in

memoryd respects individual choice:

| Level | How | Best for |
|---|---|---|
| **Full participation** | Proxy or MCP with writes | Engineers who want maximum value — contribute and benefit |
| **Read-only** | MCP search only | New hires, evaluators, PMs, security-sensitive contexts |
| **Isolated** | Separate database | Teams that need a private store |

There's no forced contribution. The value proposition of the shared store speaks for itself — the more people participate, the more everyone benefits. Most teams find that adoption is organic once a few people start and others see the results.

## Getting started with your team

1. **Start small** — pick 3-5 engineers for a pilot. Set up a shared Atlas cluster, install memoryd.
2. **Work normally for a sprint** — no behavior changes needed. Knowledge accumulates from regular AI tool usage.
3. **Show the results** — search the knowledge base, browse the dashboard. The value is visible within days.
4. **Expand gradually** — add more team members. Connect them read-only first if preferred.
5. **Seed with sources** — ingest team wikis, API docs, runbooks to accelerate the knowledge base.

→ [Getting Started](getting-started) has the full setup guide.
