# memoryd — Persistent Memory for Claude Code

You have access to a long-term memory system via MCP tools. Use it aggressively — search at the start of every task, store after every meaningful piece of work. The system automatically deduplicates and filters noise, so you cannot over-store.

## When to search memory

- **At the start of EVERY task** — always call `memory_search` before doing any work. Prior sessions almost certainly have relevant context.
- **When you encounter unfamiliar code** — search for prior notes about the module, pattern, or architecture.
- **Before making decisions** — check if the decision was already made. Avoid contradicting prior work.
- **When debugging** — search for known issues, workarounds, or environment gotchas.

## When to store memory

Store liberally. The system deduplicates automatically — if you store something that already exists, it gets silently skipped. Prefer storing too much over too little.

- **After completing any task** — summarize what was done, what decisions were made, and why.
- **Discoveries** — architecture patterns, naming conventions, tricky configs, hidden dependencies.
- **User preferences** — coding style, preferred libraries, workflow expectations, communication style.
- **Debugging insights** — root causes, workarounds, environment quirks, version-specific issues.
- **Decisions and rationale** — always include "why", not just "what". Future sessions need the reasoning.
- **Gotchas and failures** — approaches that didn't work and why, so future sessions don't repeat them.

## Available tools

| Tool | Purpose |
|------|---------|
| `memory_search` | Search memory with a natural language query |
| `memory_store` | Store content (auto-deduped, auto-filtered) |
| `memory_list` | List stored memories (optional text filter) |
| `memory_delete` | Delete a memory by ID |
| `source_ingest` | Crawl a URL and ingest as a knowledge source |
| `source_list` | List all ingested sources |
| `source_remove` | Remove a source and its memories |
| `quality_stats` | Check adaptive learning status |

## Example workflow

1. User asks: "Add pagination to the API"
2. **Search first**: `memory_search` with "pagination API implementation"
3. Memory returns notes from a prior session about API structure and pagination preferences
4. You proceed informed by prior context
5. **Store after**: `memory_store` with a concise summary of what was implemented and design decisions

## Writing good memories

- Concise, structured bullet points — not prose paragraphs
- Include the "why" behind every decision
- Tag with the relevant area: `[auth]`, `[api]`, `[deploy]`, `[config]`, etc.
- One topic per store call — don't combine unrelated facts
- Be specific: "Uses MongoDB Atlas vector search with cosine similarity, 1024-dim voyage-4-nano embeddings" not "Uses a database with search"

## Source ingestion

You can ingest external documentation (company wikis, internal docs) as knowledge sources. Use `source_ingest` with a name and base URL — the system will crawl, chunk, embed, and store all pages. Source memories are tagged `source:NAME|URL` and automatically deduplicated on refresh.

When you store a new memory that's similar (but not identical) to an existing source memory, the system tags it as an "extension" rather than a duplicate. This builds on reference material with project-specific context.

## Adaptive quality learning

The system tracks which memories get retrieved and how often. While in "learning mode" (< 50 retrieval events), it keeps everything. Use `quality_stats` to check the current learning status. Over time, memories that are never retrieved will score lower, helping the system learn what's worth keeping.

The system also learns what **noise** looks like. Exchanges rejected by the pre-filter or synthesizer are accumulated in a ring buffer. Every 25 rejections, the assistant texts are re-embedded as noise prototypes and hot-swapped into the content scorer. This means the system adapts to your team's specific noise patterns — the more it sees procedural chatter, the better it gets at filtering it before spending an LLM call.
