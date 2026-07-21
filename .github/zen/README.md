# OpenCode Zen Proxy for Codex

A lightweight Go proxy that provides access to OpenCode Zen's free LLM models
without an API key. Designed to work with Codex (Open Interpreter) as a custom
OpenAI-compatible provider.

## Quick Start

### One-Command Setup (Recommended)

```bash
# Build + auto-setup (generates catalog, updates config)
cd .github/zen && go build -o zen . && ./zen --setup
```

This single command:
1. Builds the proxy binary (`./zen`)
2. Generates the Codex model catalog at `~/.openinterpreter/zen_model_catalog.json`
3. Updates `~/.openinterpreter/config.toml` to reference the catalog
4. Exits — ready to use

No manual steps, no configuration changes needed.

**By default, the catalog contains only free zen models** (no GPT models).
If you also use other providers (OpenAI, Anthropic, etc.) and need bundled
models visible in Codex's UI, use:

```bash
./zen --setup --with-bundled
```

### Start the Proxy

```bash
# Start with auto catalog generation
./zen

# Or with quiet mode for background operation
./zen -quiet &
```

The proxy runs on `127.0.0.1:8787` and auto-refreshes the model catalog every 5 minutes.

### Verify

```bash
# Quick verification of the complete setup
./zen --verify

# Or check manually
codex debug models      # Should show 5 free models (default) or 13+ with --with-bundled
curl http://127.0.0.1:8787/v1/models   # Should show free models
```

## Architecture

```
┌──────────┐    ┌──────────────────┐    ┌──────────────────────┐
│  Codex   │───▶│  Zen Proxy       │───▶│ opencode.ai/zen/v1  │
│ (client) │    │  127.0.0.1:8787  │    │ (upstream API)      │
└──────────┘    └──────────────────┘    └──────────────────────┘
                     │
                     ▼
                ┌──────────┐
                │ models   │
                │ .dev     │
                │ (cost    │
                │  info)   │
                └──────────┘
```

## How It Works

The proxy listens on `127.0.0.1:8787` and forwards requests to OpenCode Zen's
upstream API with two key behaviors:

1. **`GET /v1/models`** — Intercepted. Returns only free models (filtered by
   `cost.input === 0` from models.dev, or by `-free` suffix heuristic).
   Falls back to a verified hardcoded list of 6 free models if upstream is
   unreachable.

2. **`POST /v1/chat/completions`** — Validated, then forwarded upstream.
   - Strips provider prefixes (e.g., `zenfree/hy3-free` → `hy3-free`)
   - Validates the model is in the free list — rejects non-free models with
     HTTP 400
   - Streams responses back to the client

All other requests are forwarded transparently.

## Free Models

| Model ID | Name |
|---|---|
| `big-pickle` | Big Pickle (stealth model) |
| `deepseek-v4-flash-free` | DeepSeek V4 Flash Free |
| `hy3-free` | Hy3 Free |
| `mimo-v2.5-free` | MiMo V2.5 Free |
| `nemotron-3-ultra-free` | Nemotron 3 Ultra Free |
| `north-mini-code-free` | North Mini Code Free |

Note: `deepseek-v4-flash-free` is a reasoning model that puts its output in
`reasoning_content` (OpenAI-compatible field for chain-of-thought).

## Codex Integration

### Provider Configuration

The proxy is configured as a custom provider in `~/.openinterpreter/config.toml`:

```toml
model_provider = "zen"
model = "deepseek-v4-flash-free"

[model_providers.zen]
name = "zen"
base_url = "http://127.0.0.1:8787/v1"
api_key = "sk-zen"
wire_api = "chat"
request_max_retries = 7
stream_max_retries = 9
```

### Model Catalog File (Required Workaround)

Codex v0.0.34 does **not** dynamically discover models from a provider's
`/v1/models` endpoint for the UI model picker. Instead, it uses a hardcoded
bundled catalog (8 models: 7 GPT + 1 codex-auto-review). This affects **all**
generic OpenAI-compatible providers, not just the zen proxy.

The `model_catalog_json` config option tells Codex to use a custom catalog file
that replaces the built-in catalog entirely:

```toml
model_catalog_json = "~/.openinterpreter/zen_model_catalog.json"
```

The `./zen --setup` command generates this file automatically:
1. Fetches the live free model list from the upstream API
2. Creates a catalog with only free zen models at the configured path
3. Updates `config.toml` to reference it

**To include bundled GPT models** (for compatibility with other providers):
```bash
./zen --setup --with-bundled
```

The proxy also auto-generates the catalog on startup and refreshes it every 5
minutes, so the catalog always reflects the current upstream model list.

**Why don't I see GPT models in the model list?** By design — the default
catalog contains only free zen models. If you also use other providers (OpenAI,
Anthropic, etc.) and want bundled models visible in the UI picker, use
`--with-bundled` to merge them into the catalog:

```bash
./zen --setup --with-bundled   # Generates catalog with bundled + free models
```

Note that `model_catalog_json` replaces Codex's built-in catalog entirely.
Without `--with-bundled`, non-zen models won't appear in the UI picker (but
API calls to other providers will still work if you specify the model name
directly).

### About "Normal Provider" Model Discovery

The requirement that "a normal provider should get its model list discovered
via the API" cannot be satisfied with Codex v0.0.34 because:

- The `remote_models` feature flag has been **removed** from Codex
  (confirmed via `codex features list`)
- Codex uses a hardcoded bundled model catalog for the UI model picker
- Generic OpenAI-compatible providers all use `wire_api = "chat"` and share
  the same bundled catalog
- The `model_catalog_json` workaround is the only available mechanism to add
  custom models to Codex's UI

The zen proxy's `/v1/models` endpoint **does** correctly return the 6 free
models — it's Codex that doesn't use this response for UI display. If/when
Codex reimplements the `remote_models` feature, the proxy will work as a
"normal provider" without any workaround.

## Usage Examples

```bash
# List models from the proxy
curl http://127.0.0.1:8787/v1/models

# Chat completion with a free model
curl -X POST http://127.0.0.1:8787/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"big-pickle","messages":[{"role":"user","content":"Hello"}]}'

# Run Codex with a zen model
echo 'Say hello' | codex exec -m deepseek-v4-flash-free \
  --dangerously-bypass-approvals-and-sandbox

# Verify Codex sees all models
codex debug models
```

## Research: Plano (katanemo/plano)

[Plano](https://github.com/katanemo/plano) was investigated as a potential
replacement for the Go proxy to make the proxy "more standard."

### What Plano Provides
- AI-native proxy server built on **Envoy proxy WASM filters** (Rust)
- Smart LLM routing with model aliases and preferences
- Agent orchestration (multi-agent routing)
- Guardrails, moderation, and memory hooks
- OpenTelemetry observability (traces, metrics, logs)
- Standalone CLI (`planoai`) that auto-downloads Envoy on first run

### Why It's Not Suitable for This Use Case

| Factor | Assessment |
|--------|-----------|
| **Complexity** | Downloads Envoy + WASM plugins (~hundreds of MB). 68MB+ source. |
| **Architecture** | WASM filter for Envoy — heavy infrastructure for a simple proxy |
| **Configuration** | YAML-based with agent definitions, routing rules |
| **Model discovery** | Does NOT fix the Codex `remote_models` limitation |
| **Use case fit** | Designed for multi-agent orchestration at scale |

### Verdict

Plano is not appropriate for this use case. The current Go proxy is the right
tool — it's simple (~9MB static binary, no dependencies), focused, and correctly
implements the subset of the OpenAI API needed for chat completions with free
model filtering. Plano would add significant complexity without solving the
Codex model discovery issue (which is a Codex limitation, not a proxy limitation).

Plano would be suitable for: large-scale deployments needing multi-agent
orchestration, smart routing, guardrails, and observability — not for a simple
model proxy.

## Files

| File | Purpose |
|------|---------|
| `main.go` | Zen proxy implementation (Go) — single binary, no external deps |
| `main_test.go` | Tests for the proxy |
| `zen_model_catalog.json` | Auto-generated catalog (5 free models default, 13+ with --with-bundled) |
| `go.mod` | Go module definition |
| `zen` | Compiled binary (not tracked in git) |

### Flags

| Flag | Description |
|------|-------------|
| (default) | Start proxy + auto-generate catalog + refresh every 5 min |
| `--setup` | Generate catalog (zen-only, no GPT models) + update config.toml, then exit |
| `--setup --with-bundled` | Generate catalog with bundled + free models, then exit |
| `--verify` | Check setup: catalog validity, codex model list, proxy status |
| `--configure` | Only update config.toml with `model_catalog_json`, then exit |
| `--catalog-path PATH` | Custom catalog path (default: `~/.openinterpreter/zen_model_catalog.json`) |
| `--listen ADDR` | Bind address (default: `127.0.0.1:8787`) |
| `--upstream URL` | Upstream gateway (default: `https://opencode.ai/zen/v1`) |
| `--quiet` | Reduce logging |
