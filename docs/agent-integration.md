# Coding Agent Integration

`local-fusion-gateway` currently exposes an OpenAI-compatible **Chat Completions** API:

- `GET /v1/models`
- `POST /v1/chat/completions`

This is enough for basic prompt/response clients and for coding agents that can use an OpenAI-compatible `chat.completions` backend without provider-native tool calling. Full coding-agent use is stricter: most agents need streaming, tool-call passthrough, or a different wire protocol.

## Compatibility Matrix

| Agent | Current status | Native protocol expected by agent | Can point to current gateway? | Notes |
|---|---:|---|---:|---|
| pi (earendil-works/pi) | Supported for OpenAI Chat Completions passthrough | `openai-completions`, `openai-responses`, `anthropic-messages` | Yes, via `openai-completions` | If the request contains `tools`, `tool_choice`, tool messages, `tool_calls`, or `stream=true`, the gateway bypasses Fusion and proxies to `agent_profiles.pi`; if that profile is omitted, it falls back to the synthesizer model so pi can run its own tool loop. |
| omp (Oh My Pi) | Partial | `openai-completions`, `openai-responses`, `anthropic-messages`, proxy discovery | Yes, for `openai-completions` non-stream/simple use | OMP can discover `/v1/models`, but full agent loops need tools/streaming. |
| OpenCode | Partial | AI SDK providers, OpenAI-compatible providers | Likely yes for simple chat-style model calls | Full support needs OpenCode custom provider validation and streaming/tool schema compatibility. |
| Codex CLI | Not yet | OpenAI **Responses API** (`/v1/responses`) | No | Codex custom providers now require `wire_api = "responses"`; this gateway does not implement Responses API yet. |
| Claude Code | Not yet | Anthropic Messages API (`/v1/messages`) | No | Claude Code requires Anthropic-compatible API. Use a bridge/proxy or implement `/v1/messages`. |

## Important Limitation

Do **not** treat the current gateway as a drop-in backend for all coding agents yet.

The current `ChatCompletionRequest` preserves common agent fields such as `tools`, `tool_choice`, assistant `tool_calls`, tool messages, and unknown extra JSON fields. When those fields or `stream=true` are present, the gateway enters agent passthrough mode instead of Fusion mode.

Still not implemented for the Fusion path:

- Native Fusion over multiple models with tool-call voting
- Responses API events
- Anthropic content blocks

If a coding agent sends tool schemas to the gateway today, the gateway does not expose those tools to panel/synthesizer models. The agent may fail, loop, or receive plain text instead of tool calls.

## Recommended Integration Strategy

There are two useful modes.

### Mode A — Advisory / Review Model

Use `local-fusion-gateway` as a high-quality research/review model for coding agents:

- ask it to inspect code via `code_research`
- ask it for architecture plans
- ask it to compare model opinions
- ask it to review proposed diffs

This mode is already viable because the gateway itself provides controlled read-only code tools through `code_research`.

### Mode B — Primary Agent Backend

Use `local-fusion-gateway` as the model backend that drives a coding agent's own read/edit/bash tools.

This requires protocol work:

1. OpenAI Chat Completions tool passthrough
2. Streaming support
3. OpenAI Responses API (`/v1/responses`) for Codex
4. Anthropic Messages API (`/v1/messages`) for Claude Code
5. Provider compatibility flags for strict schemas, developer/system role, max token field, reasoning fields

## pi Configuration

pi can register a custom OpenAI-compatible provider with `api: "openai-completions"`.

Copy `examples/agents/pi-models.json` into `~/.pi/agent/models.json` or merge the provider block into your existing file.

Then run:

```bash
export LOCAL_FUSION_API_KEY="change-me-long-random-token"
pi -p --model local-fusion/local/fusion --api-key "$LOCAL_FUSION_API_KEY" "Summarize this repo architecture."
```

With this setup, pi tool-loop requests (`tools`, `tool_choice`, tool messages, `stream=true`) are proxied directly to the configured `agent_profiles.pi` target, falling back to the synthesizer model. Plain non-tool prompts still use Fusion mode by default.

## omp Configuration

OMP uses `~/.omp/agent/models.yml`.

Copy/merge `examples/agents/omp-models.yml`:

```bash
export LOCAL_FUSION_API_KEY="change-me-long-random-token"
omp -p --model local-fusion/local/fusion --api-key "$LOCAL_FUSION_API_KEY" "Review the current code architecture."
```

For read-only review, prefer sending a `code_research` request directly to the gateway rather than expecting OMP's own tool loop to work through Fusion.

## OpenCode Configuration

OpenCode config lives at one of:

- `~/.config/opencode/opencode.json`
- project-local `opencode.json`
- path from `OPENCODE_CONFIG`

Use `examples/agents/opencode.json` as a starting point. Depending on your OpenCode version, you may also need to add credentials with:

```bash
opencode providers
```

Then select:

```bash
opencode run -m local-fusion/local/fusion "Explain this project."
```

If OpenCode sends tool schemas or expects streaming deltas, current gateway support is incomplete.

## Codex CLI Configuration

Codex custom providers now require the OpenAI Responses API:

```toml
wire_api = "responses"
```

The current gateway does **not** implement `/v1/responses`, so `examples/agents/codex-config.toml` is a future template, not a working config yet.

Required implementation before Codex can use this gateway as primary backend:

- `POST /v1/responses`
- response item format
- streaming events if Codex requires streaming
- function/tool call item passthrough
- reasoning/approval fields as needed

## Claude Code Configuration

Claude Code talks to Anthropic-compatible `/v1/messages`, controlled by:

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8080"
export ANTHROPIC_AUTH_TOKEN="$LOCAL_FUSION_API_KEY"
```

The current gateway does **not** implement `/v1/messages`, so `examples/agents/claude-env.sh` is a future template.

Required implementation before Claude Code can use this gateway as primary backend:

- `POST /v1/messages`
- Anthropic request/response content blocks
- streaming SSE for `message_start`, `content_block_delta`, `tool_use`, `tool_result`, `message_stop`
- tool schema translation
- Claude-specific compatibility behavior

## Suggested Protocol Roadmap

### Phase 1 — Agent-safe OpenAI Chat Completions

Goal: support pi/omp/OpenCode simple tool loops through `/v1/chat/completions`.

Required work:

- Extend request/response structs to preserve unknown/provider fields.
- Add `tools`, `tool_choice`, `parallel_tool_calls` fields.
- If tools are present, support an `agent_mode` policy:
  - `passthrough`: route to one selected upstream model, preserve tool calls.
  - `fusion_review`: do not allow tool calls; use `code_research` and return text.
- Implement streaming `stream: true` for OpenAI chat chunks.
- Add compatibility config per provider/model.

### Phase 2 — Responses API for Codex

Goal: Codex can point directly at `local-fusion-gateway`.

Required endpoints:

- `POST /v1/responses`
- optional `GET /v1/responses/{id}` if needed

Required behavior:

- non-streaming and streaming event formats
- function-call item passthrough
- tool result continuation
- mapping between internal panel/synthesizer and response output items

Recommended first implementation:

- `responses` passthrough mode to one upstream model
- then add fusion/advisory mode later

### Phase 3 — Anthropic Messages API for Claude Code

Goal: Claude Code can point directly at gateway.

Required endpoint:

- `POST /v1/messages`

Required behavior:

- content block conversion
- `tool_use` / `tool_result` support
- SSE streaming
- thinking block handling or explicit rejection

Recommended first implementation:

- Anthropic passthrough adapter to one Anthropic-compatible upstream
- then optional fusion summary for non-tool turns

### Phase 4 — Agent Profiles

Add top-level config presets:

```yaml
agent_profiles:
  pi:
    protocol: openai-completions
    mode: passthrough
    provider: openrouter
    model: openai/gpt-4.1
  codex:
    protocol: openai-responses
    mode: passthrough
    provider: openrouter
    model: openai/gpt-5.1-codex
  claude_code:
    protocol: anthropic-messages
    mode: passthrough
    provider: anthropic
    model: claude-sonnet-4-5
```

## Bottom Line

Current best use:

- `local-fusion-gateway` = multi-model reviewer / code researcher / architecture advisor
- pi/omp/OpenCode = primary editor agents

Future target:

- `local-fusion-gateway` = unified local model backend for pi, omp, OpenCode, Codex, and Claude Code
- protocol adapters route tool loops safely and let Fusion participate in planning/review turns
