# local-fusion-gateway

A local OpenAI-compatible Chat Completions gateway that fans out a single `/v1/chat/completions` request to multiple upstream models in parallel, then calls a synthesizer model to produce a unified response. Designed as a **local Fusion** prototype.

## Features

- OpenAI-compatible HTTP API (`/v1/chat/completions`, `/v1/models`, `/health`)
- Parallel fan-out to configurable panel models
- Synthesizer model merges panel results with a structured prompt (consensus, conflicts, omissions, final answer)
- Environment-variable-based upstream API key management (`api_key_env`)
- Optional local bearer-token auth for gateway clients (`auth_token_env`)
- Request body limits for chat completions (`max_body_bytes`, default 2 MiB)
- Sanitized upstream error responses and request IDs (`X-Request-ID`)
- Optional sanitized debug artifacts with hashes/lengths instead of full prompts by default
- Optional `code_research` mode that lets panel models use controlled read-only local-code tools before answering
- YAML configuration
- Minimal dependencies (stdlib-first, only `gopkg.in/yaml.v3`)

## Configuration

Copy `config.example.yaml` to `config.yaml` and edit it:

```bash
cp config.example.yaml config.yaml
```

### Key Fields

| Field | Description |
|-------|-------------|
| `listen` | Address to listen on (default: `:8080`) |
| `virtual_model` | Model ID reported to clients (default: `local/fusion`) |
| `timeout_seconds` | Timeout per upstream request (default: `120`) |
| `auth_token_env` | Optional environment variable that holds the local gateway bearer token. Empty disables local auth for compatibility. |
| `max_body_bytes` | Maximum `/v1/chat/completions` request body size in bytes (default: `2097152`, 2 MiB). |
| `debug.enabled` | Enables sanitized per-request JSON debug files (default: `false`). |
| `debug.dir` | Directory for debug files (default: `debug`). |
| `debug.capture_content` | If `true`, debug files include prompt/response text. Keep `false` for production; hashes and lengths are recorded by default. |
| `providers` | List of upstream LLM providers with name, `base_url`, and optional `api_key_env` |
| `panel` | Models to query in parallel (each references a provider) |
| `synthesizer` | Model used to synthesize panel outputs |

### API Keys via Environment Variables

Set `api_key_env` to the name of an environment variable that holds the API key:

```yaml
providers:
  - name: "openrouter"
    base_url: "https://openrouter.ai/api/v1"
    api_key_env: "OPENROUTER_API_KEY"
```

Then run with:

```bash
export OPENROUTER_API_KEY="your-upstream-provider-token"
export LOCAL_FUSION_API_KEY="a-long-random-local-gateway-token"
make run
```

### Local Gateway Authentication

Set `auth_token_env` to require client authentication on `/v1/models` and
`/v1/chat/completions`:

```yaml
auth_token_env: "LOCAL_FUSION_API_KEY"
```

If `auth_token_env` is empty, the gateway remains unauthenticated for backward
compatibility. `/health` is always unauthenticated so local supervisors can probe
it safely.

Authenticated requests must include:

```bash
-H "Authorization: Bearer $LOCAL_FUSION_API_KEY"
```

### Debug Artifacts

Debug artifacts are disabled by default:

```yaml
debug:
  enabled: false
  dir: "debug"
  capture_content: false
```

When enabled, each chat request writes one JSON file containing the `run_id`,
timestamp, model, panel success/failure summaries, synthesizer summary, elapsed
time, final status, and content lengths/hashes. API keys are never written.
Prompt and response text are only written if `debug.capture_content: true`.

When `code_research` is enabled for a request, debug artifacts also record whether code research was enabled and, per panel, which tools were used, which files were read, and which searches were requested. Tool result content is stored only when `debug.capture_content: true`; otherwise only length/hash metadata is stored.

### Code Research Mode

`code_research` is an optional per-request mode. When enabled, each panel model runs an independent research loop before producing its panel answer. The gateway exposes only controlled read-only tools implemented in Go:

- `list_files(pattern, limit)` under `workdir`
- `search_files(query, file_glob, limit)` for text substring search
- `read_file(path, offset, limit)` with line numbers and strict `workdir` containment
- `git_diff()` with fixed `git diff --stat` and bounded `git diff` output when `include_git_diff` is true

No arbitrary shell, write access, or network access is provided by this mode. Sensitive files such as `.env`, `*.pem`, `*.key`, `auth.json`, credentials/secrets files, `*.sqlite`, and `*.db` are refused.

Example request:

```bash
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $LOCAL_FUSION_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "local/fusion",
    "messages": [
      {"role": "user", "content": "Where is request authentication handled? Cite files and lines."}
    ],
    "code_research": {
      "enabled": true,
      "workdir": "/absolute/path/to/your/repo",
      "max_rounds": 4,
      "max_file_bytes": 20000,
      "max_total_bytes": 80000,
      "include_git_diff": true
    }
  }' | jq .
```

If `code_research` is omitted or `enabled` is `false`, the gateway keeps the original plain panel fan-out behavior.

## Running

```bash
# Install dependencies
go mod tidy

# Run with default config.yaml
make run

# Run with a custom config
go run . -config /path/to/config.yaml

# Or build and run
go build -o local-fusion-gateway .
./local-fusion-gateway -config config.yaml
```

## Testing

```bash
make test
```

Tests use `httptest` fake upstream servers — no real API keys or network access required.

## Usage with curl

### Health Check

```bash
curl http://localhost:8080/health
# {"ok":true}
```

### List Models

Without local auth (`auth_token_env: ""`):

```bash
curl http://localhost:8080/v1/models
# {"object":"list","data":[{"id":"local/fusion",...},...]}
```

With local auth enabled:

```bash
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer $LOCAL_FUSION_API_KEY"
```

### Chat Completion

```bash
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $LOCAL_FUSION_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "local/fusion",
    "messages": [{"role": "user", "content": "What is the capital of France?"}]
  }' | jq .
```


## Coding Agent Integration

This project can already be used as an advisory/review model for coding agents through its OpenAI-compatible `/v1/chat/completions` endpoint and `code_research` mode.

Full primary-backend support for agents such as Codex CLI and Claude Code requires additional wire protocols. See:

- `docs/agent-integration.md`
- `examples/agents/`

Current practical status:

| Agent | Status |
|---|---|
| pi | Partial: OpenAI Chat Completions advisory/review use |
| omp | Partial: OpenAI Chat Completions advisory/review use |
| OpenCode | Partial: simple OpenAI-compatible use, version-dependent |
| Codex CLI | Future: needs `/v1/responses` |
| Claude Code | Future: needs Anthropic `/v1/messages` |

## Production Running Suggestions

- Set `auth_token_env` and export a long random token before exposing the
  gateway to any non-loopback client.
- Keep upstream provider keys in environment variables only; do not commit real
  tokens to YAML files.
- Bind to `127.0.0.1` or protect the listener with a local firewall/reverse
  proxy if you do not need LAN access.
- Keep `debug.enabled: false` unless actively diagnosing a run. If enabled,
  keep `debug.capture_content: false` unless you explicitly accept
  prompt/response storage on disk.
- Review filesystem permissions for the debug directory; artifacts are written
  with owner-only file permissions.
- Size `max_body_bytes` to your expected local workload. The default is 2 MiB.
- Use `X-Request-ID` / `error.request_id` to correlate client errors with local
  logs and optional debug files.

## Limitations

- **No streaming support**: `stream: true` returns HTTP 400. This is a prototype focused on non-streaming completions.
- **Single-turn only**: No context/history management beyond forwarding the message array.
- **No OpenAI tool/function-calling protocol**: `code_research` uses a gateway-owned JSON text protocol for controlled read-only tools instead of provider-native `tool_calls`.
- **Token estimation**: Usage statistics use a rough heuristic (4 characters ≈ 1 token).
- **No retries or circuit breaking**: Failed panel models are simply logged and omitted.

## Architecture

```text
Client
  │
  ▼
POST /v1/chat/completions
  │
  ├─► Panel Model A ──┐
  ├─► Panel Model B ──┤ (parallel)
  ├─► Panel Model C ──┘
  │
  ├─ Collect successes / failures
  │    (at least 1 success required)
  │
  ├─ Build synthesizer prompt
  │    (consensus, conflicts, omissions, final answer)
  │
  ├─► Synthesizer Model
  │
  ▼
Synthesized Response
```

## Requirements

- Go 1.22+
- Access to upstream LLM APIs (e.g., OpenRouter, Ollama, or any
  OpenAI-compatible endpoint)
