# arr-reconciler

[![CI](https://github.com/ramseymcgrath/arr-reconciler/actions/workflows/ci.yml/badge.svg)](https://github.com/ramseymcgrath/arr-reconciler/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/ramseymcgrath/arr-reconciler)](https://goreportcard.com/report/github.com/ramseymcgrath/arr-reconciler)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

An autonomous reconciliation agent for a Sonarr/Radarr stack. On a schedule it:

1. **Stuck queue items** — pulls the download queue from each instance, flags items that look stalled/failed, asks Claude to triage each one, and removes (+ blocklists) the ones Claude judges dead so a different release gets grabbed.
2. **DB vs disk** — for each instance:
   - **Missing files**: DB references a file that's gone from disk → triggers a `RescanSeries`/`RescanMovie` so the instance corrects itself (and re-searches monitored items).
   - **Orphan files**: files present on disk under a library root that no DB references → asks Claude which are safe junk, then relocates them to a ZFS trash dataset (never `rm`).

Claude makes the per-item decisions. **Hard safety rails sit above Claude and cannot be overridden by its output.**

## Safety model

Orphan handling is the only destructive path, and it is gated by rails enforced in code, before and after the model is consulted:

- **No unlink.** Files are *moved* to `safety.trash_dataset` (intended to be its own ZFS dataset, so snapshots/quotas apply) under a date-stamped path, then purged after `trash_ttl`. Recovery is `mv` back.
- **Path allowlist.** A file is only eligible if it lives under `safety.allowed_roots`. Library roots reported by the arr instances that fall outside the allowlist are skipped entirely.
- **Protected extensions.** Anything matching `safety.protected_extensions` (subs, nfo, artwork, etc.) is never touched.
- **Minimum age.** A file must be older than `safety.min_orphan_age` to avoid racing in-flight imports.
- **Per-run caps.** `max_deletes_per_run` and `max_bytes_per_run` bound blast radius.
- **`delete_enabled: false`** disables orphan relocation completely; missing-file rescans still run.

Queue removal is reversible by nature (re-search) and capped by `queue.max_removals_per_run`.

`dry_run: true` reports every action it *would* take without performing any. **Start here.**

## Build

```bash
go build -o arr-reconciler ./cmd/reconciler
```

No third-party dependencies — the standard library only, so the build is hermetic and `go.sum` is empty.

## Develop

```bash
go test ./...   # unit tests for the safety rails, trash bin, decision parsing, notifier
go vet ./...
```

The safety-critical logic (path allowlist, protected extensions, trash relocation/purge, LLM-output parsing) is covered by unit tests; run them before changing anything that can touch disk.

## Configure

Copy `config.example.json`, fill in API keys (Sonarr/Radarr `Settings → General → API Key`, plus an Anthropic key). Keep `dry_run: true` and `delete_enabled: false` for the first runs.

API keys in a config file should be `chmod 600` and owned by the service user. You can also point `claude.api_key` etc. at values injected by your secrets manager — the binary just reads the file.

## Run

One-shot (recommended; pair with the systemd timer):

```bash
./arr-reconciler -config /etc/arr-reconciler/config.json
```

Force dry-run regardless of config:

```bash
./arr-reconciler -config ./config.json -dry-run
```

Built-in daemon mode (alternative to the timer):

```bash
./arr-reconciler -config ./config.json -interval 4h -jitter 10m
```

## Deploy (systemd)

```bash
sudo install -m 0755 arr-reconciler /usr/local/bin/
sudo install -d /etc/arr-reconciler /var/lib/arr-reconciler
sudo install -m 0600 config.json /etc/arr-reconciler/config.json
sudo cp deploy/arr-reconciler.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now arr-reconciler.timer
# manual trigger:
sudo systemctl start arr-reconciler.service
journalctl -u arr-reconciler.service -f
```

The unit runs as user `mediabot` with `ProtectSystem=strict`; it can read `/tank/media` and write only `/tank/trash` and its state dir. Adjust paths/user to your pool.

## ZFS trash dataset

```bash
zfs create -o mountpoint=/tank/trash tank/trash
# optional: cap it so trash can never eat the pool
zfs set quota=1T tank/trash
```

Because relocation is a same-pool `rename(2)` it's atomic and instant when trash lives on the same pool as the media; cross-dataset moves within one pool still rename without copying. If trash is on a *different* pool, the tool falls back to copy-then-remove.

## Output

Each run prints (and webhook-posts, if configured) a summary, and writes the full structured report to `state_file` as JSON for later inspection.

## Observability (Datadog LLM Observability)

Every Claude call is traced into [Datadog LLM Observability](https://docs.datadoghq.com/llm_observability/). There is no Go SDK, so the tool posts spans and evaluations straight to a local Datadog agent's EVP proxy — **no API key needed in agent mode**, the agent injects it. Enable it by setting `llmobs.endpoint`; leave it empty to disable all instrumentation (it becomes a complete no-op).

```json
"llmobs": {
  "endpoint": "http://datadog-agent:8126",
  "ml_app": "arr-reconciler",
  "tags": ["env:home", "stack:media"]
}
```

Each run emits one trace shaped like:

```
agent:  reconcile_run
 ├─ llm:  queue_triage:sonarr       input/output, model, input_tokens/output_tokens/total_tokens
 │   ├─ task: decision:q-12         eval model_confidence=0.92, rail_outcome=executed
 │   └─ task: decision:q-15         eval model_confidence=0.40, rail_outcome=kept
 └─ llm:  orphan_triage:radarr
     └─ task: decision:o-3          eval model_confidence=0.95, rail_outcome=blocked_recheck
```

Two evaluations are submitted per decision:

- **`model_confidence`** (score) — the model's self-reported confidence.
- **`rail_outcome`** (categorical) — what the hard safety rails did with the model's action: `executed`, `kept`, `dry_run`, or a veto (`blocked_recheck`, `capped_deletes`, `capped_bytes`, `capped_removals`, `error`). This is the signal that shows, in Datadog, how often the rails overrode the model — exactly the cases worth auditing.

Token usage feeds the standard LLM Obs cost/latency views. The agent must have `apm_config.enabled: true` (it forwards LLM Obs through the trace agent on `:8126`). Spans must be under 24h old and under 5 MB per flush — both always true for this workload.

## Models

Each task uses a model matched to its risk and complexity (all overridable in `claude` config):

- **`queue_model`** (default `claude-haiku-4-5`) — queue triage is reversible (removal just re-searches) with simple signals, so a small fast model suffices.
- **`orphan_model`** (default `claude-sonnet-4-6`) — orphan triage is the destructive path and needs nuanced judgement (junk vs. hand-managed sidecar) on a large pool, so it uses a stronger model.
- **`model`** (default `claude-sonnet-4-6`) — fallback when a per-task model is unset.

Candidate payloads are kept minimal and continuous values are bucketed (age, size), so an unchanged set of candidates serializes identically across runs — letting the AI Gateway response cache hit instead of drifting every run.

## Notes

- Sonarr/Radarr v3 API only (current stable). Lidarr/Readarr use v1 and aren't wired up.
- The model is asked for strict JSON; parsing tolerates stray fences/prose. If a decision can't be parsed, that instance's pass errors out rather than guessing.
- Token cost scales with how many *candidates* exist, not library size — a healthy library sends almost nothing to the API.
- Calls route through the Cloudflare AI Gateway (caching, logging, rate limiting) when `claude.base_url` points at the gateway and `gateway_token` is set; otherwise they go direct to Anthropic.
