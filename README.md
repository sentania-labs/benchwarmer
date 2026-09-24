# Benchwarmer

Benchwarmer runs a local LLM (llama.cpp `llama-server`) on a Windows gaming PC
only while the GPU is otherwise idle, and gets out of the way the moment a
human wants it. The human owns the GPU.

It is a single-machine GPU lifecycle controller: it owns the runtime process,
proxies the OpenAI-compatible API so it can refuse work cleanly, watches games
and GPU contention, drains or kills inference when it must yield, and reloads
after a cooldown. Every decision is recorded with the rule and evidence that
caused it.

**Status:** headless MVP, web UI, tray, and installer built and tested with a
simulated GPU and a fake runtime; target-hardware measurements (Phase 0) are
pending a Defender policy change on the target. See [spec.md](spec.md) for the
product specification and [docs/phase0/plan.md](docs/phase0/plan.md) for the
analysis and decisions.

## Repository layout

| Path | What |
|---|---|
| `cmd/benchwarmer` | The Windows service (`run`, `service install/remove`, `config`) |
| `cmd/bwtray` | Tray app and session agent |
| `cmd/bwprobe` | Phase 0 measurement tool ([runbook](docs/phase0/runbook.md)) |
| `cmd/fakellama` | Test fixture imitating llama-server |
| `internal/controller` | Single-writer lifecycle driver (drain, preempt, cooldown, suppression, recovery) |
| `internal/policy` | Pure policy evaluator and table tests |
| `internal/state`, `internal/config`, `internal/events`, `internal/api` | Shared contracts |
| `internal/proxy` | Inference reverse proxy with the admission gate |
| `internal/observe`, `internal/classify`, `internal/telemetry`, `internal/signals` | Facts from GPU counters, processes, and the session agent |
| `internal/runtime/llamacpp`, `internal/procgroup` | Runtime adapter and Job Object ownership |
| `internal/store`, `internal/metrics`, `internal/webui` | SQLite, Prometheus, dashboard |
| `installer/` | `install.ps1` / `uninstall.ps1` (ADR 0009) |
| `docs/` | ADRs, API schema (`docs/api/openapi.yaml`), Phase 0, deploy, troubleshooting, release |

## Development

Requires Go (version from `go.mod`; the `go` command fetches it automatically).
Development works on Linux; Windows-specific code is cross-compiled and
exercised by the Windows CI job.

```sh
make check     # format, vet (linux+windows), staticcheck, race tests, govulncheck, Windows build
make windows   # dist/bwprobe.exe, dist/fakellama.exe
```

CI runs the same `make check`, plus Windows-native tests and a probe smoke run.
`internal/service` has an end-to-end test that drives the real service with a
fake runtime process and the GPU simulator through load, drain, cooldown,
reload, hard-contention preemption, suppression, and shutdown.

Run locally without a GPU:

```sh
go build -o /tmp/fakellama ./cmd/fakellama
go run ./cmd/benchwarmer config default > data/config.json   # then point runtime.executable at /tmp/fakellama
go run ./cmd/benchwarmer run --data ./data --simulate-gpu --sim-control sim.json
```

See also [troubleshooting](docs/troubleshooting.md), [release process](docs/release.md),
and [policy requirements](docs/deploy/policy-requirements.md).
