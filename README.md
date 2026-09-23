# Benchwarmer

Benchwarmer runs a local LLM (llama.cpp `llama-server`) on a Windows gaming PC
only while the GPU is otherwise idle, and gets out of the way the moment a
human wants it. The human owns the GPU.

It is a single-machine GPU lifecycle controller: it owns the runtime process,
proxies the OpenAI-compatible API so it can refuse work cleanly, watches games
and GPU contention, drains or kills inference when it must yield, and reloads
after a cooldown. Every decision is recorded with the rule and evidence that
caused it.

**Status:** Phase 0 (measurement spike). See [spec.md](spec.md) for the product
specification and [docs/phase0/plan.md](docs/phase0/plan.md) for where things
stand.

## Repository layout

| Path | What |
|---|---|
| `cmd/bwprobe` | Phase 0 measurement tool for the target PC ([runbook](docs/phase0/runbook.md)) |
| `cmd/fakellama` | Test fixture imitating llama-server |
| `internal/procgroup` | Runtime process-tree ownership (Windows Job Objects) |
| `internal/telemetry` | GPU telemetry collection and own/external demand split |
| `internal/signals` | Processes, foreground/fullscreen/idle, sessions |
| `docs/adr` | Architecture decision records |
| `docs/phase0` | Phase 0 plan, runbook, findings, provisional thresholds |

## Development

Requires Go (version from `go.mod`; the `go` command fetches it automatically).
Development works on Linux; Windows-specific code is cross-compiled and
exercised by the Windows CI job.

```sh
make check     # format, vet (linux+windows), staticcheck, race tests, govulncheck, Windows build
make windows   # dist/bwprobe.exe, dist/fakellama.exe
```

CI runs the same `make check`, plus Windows-native tests and a probe smoke run.
