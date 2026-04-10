# memoryd — Codebase Reference for Claude Code

> Shared memory/IP/project context is in the parent `CLAUDE.md` and `PROJECT_CONTEXT.md`. This file covers memoryd-specific architecture.

Module: `github.com/memory-daemon/memoryd`
Go version: 1.26+
Config: `~/.memoryd/config.yaml`

## CLI Commands

```
memoryd start      Start daemon (foreground). Creates config on first run.
memoryd mcp        Start as MCP stdio server (for Claude Code MCP integration)
memoryd status     Ping health endpoint
memoryd search     Regex search on memory content
memoryd forget     Delete one memory by hex ID
memoryd wipe       Delete all memories (confirmation required)
memoryd env        Print ANTHROPIC_BASE_URL export
memoryd version    Print version
memoryd ingest     Crawl a URL and store as source
memoryd sources    List ingested sources
memoryd export     Export memories to markdown
```

## Build & Test

```bash
make build              # → bin/memoryd
go test ./...           # all unit tests (no external deps needed)
go vet ./...            # static analysis
```

## Conventions

- Standard Go: `gofmt`, `go vet`, no external linters
- Interfaces defined in the package that uses them
- Functional options pattern for configuration (e.g., `proxy.WithStore()`)
- Errors logged at the boundary, not propagated through async paths
- Unit tests use in-memory mocks, test files live next to their code
- Write pipeline runs in goroutines — errors logged, never returned to caller
- `redact.Clean()` strips secrets BEFORE embedding — secrets never enter the vector store
- Daemon binds to 127.0.0.1 only

## Gotchas

1. **Embedding dim is 1024, not 512.** voyage-4-nano produces 1024-dim vectors. The vector index must match.
2. **Atlas Local doesn't support `$search` or `$vectorSearch` filters.** Those are Atlas-proper features.
3. **New memories have quality_score 0.** AtlasStore uses `$or` to avoid filtering out unscored memories.
4. **SSE streaming.** The proxy buffers the full response for the write path while streaming to the client. Don't break the streaming path for write-path changes.
5. **Config path expansion.** `~` in `model_path` is expanded by the config loader. Use the config package's path handling.
6. **Content score pre-gate does NOT feed rejection store.** Only QuickFilter and synthesizer rejections feed back. Prevents positive feedback loop.
