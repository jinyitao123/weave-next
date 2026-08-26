# Weave

<p align="center">
  <a href="README.md">English</a> | <a href="README.zh-CN.md">中文</a>
</p>

**Give an always-on agent an address, and a leash. Then you can hand it real work.**

Weave is the platform layer for [Loom](https://github.com/jinyitao123/loom). Loom builds a mind that can act. Weave decides which of its acts are allowed to touch the real world — and keeps every one of them on the record.

## The one idea

Everything you hand to someone else rides on a single coupling: **whoever holds the authority to act also carries the accountability for the outcome.** A colleague you delegate to shares your downside — their name, their job. That shared stake is what makes delegation safe.

An AI agent breaks that coupling. It will act — send the email, move the stock, call the tool — but it carries none of your risk. It can't be incentivized, can't be punished, has no stake in the outcome. So the two classic ways to make delegation safe — *align its interests* and *trust its judgment* — are both gone. You hand off the **labor** and keep **100% of the risk**.

When you can't transfer the risk, the only lever left is structural: **bound the agent's authority at the exact points where an action becomes irreversible, and keep every crossing legible.** That boundary — where reversible computation becomes irreversible consequence — is the whole reason Weave exists.

So Weave optimizes for one property most agent platforms don't even name: **delegatability**. Not how autonomous an agent can be, but how safely you can hand it work while you are not watching.

Two things make an agent delegatable:

- **Determinism** — its actions are predictable and replayable. You cannot govern what you cannot foresee or reconstruct. *Loom's graph gives this: an agent is a JSON graph of steps, not a loose prompt loop. The graph fixes what happens in what order; a skill only describes what "good" looks like — the* what*, never the* how.
- **A fail-closed write boundary** — irreversible external writes are not delegated to business teams by default, and the whole run is traced and replayable. *Weave gives this: a narrow execution boundary and the trace ledger.*

**Determinism + the gate = something you can actually delegate to.** Loom makes the hand steady; Weave decides which acts of that steady hand may reach the world. A steady hand you cannot govern is still a liability; a gate on an unpredictable hand has nothing solid to hold. You need both.

## Loom and Weave

|  | Loom | Weave |
|---|---|---|
| **is** | the kernel — a mind that can act | the platform — the governed surface where that mind touches the world |
| **its object** | the *interior* of an agent (how it thinks and executes) | the *boundary* of an agent (which actions cross, who approves, what is on record) |
| **gives you** | deterministic execution: agent-as-data, a graph you can read | runnable teams: the write boundary, durable outputs, an addressable runtime |
| **metaphor** | the loom that weaves | 经纬 — the warp (people, who bear the risk), the weft (agents, who bear none), and the *heddle* that decides which thread may cross |

An agent on its own is just computation; it has no boundary. Weave is what gives it a **skin**: tools are its hands, a channel (chat / email) is its mouth, and the gate is the wrist a human can hold.

## What Weave is, concretely

Three properties the whole design is built to guarantee:

- **Always-on** — agents run as persistent runtimes (a server, a user's own machine, or an edge box), not one-shot calls. Work can arrive while you sleep.
- **Addressable** — you reach an agent the way you reach a colleague: a chat, an inbox. It remembers you.
- **Accountable** — external write actions fail closed unless explicitly implemented as product flows; every run is traced and replayable. This is load-bearing, not a setting.

```
┌─────────────────────────────────────────────┐
│  Weave Console (React 19 + Vite)            │  <- Browser UI
├─────────────────────────────────────────────┤
│  Weave API (Echo)                           │  <- REST + SSE
│  ┌──────────────┬──────────┬─────────────┐  │
│  │ Write Gate   │ Registry │ Memory      │  │  <- the leash: fail-closed writes, trace
│  │ Audit / Trace│ Compiler │ LLM Router  │  │
│  └──────┬───────┴────┬─────┴──────┬──────┘  │
├─────────┼────────────┼────────────┼──────────┤
│   Loom Kernel      Store (PGStore)  MCP Host │  <- deterministic execution
└─────────────────────────────────────────────┘
   PostgreSQL 16+          MCP Servers (HTTP)
```

## What's in the box

**The kernel — what makes an agent delegatable**

| Capability | How |
|---|---|
| **Write-action boundary** | Irreversible tool calls are rejected unless they are implemented as explicit product flows. Fail-closed: no product path, no write. |
| **Audit & Trace** | Per-step trace recording, run history, `runtime_info` in SSE done events. Every run is replayable. |
| **Deterministic execution** | Agents compile to a Loom Graph. Declarative JSON graphs express multi-step business flows (6 step types: chat, llm_call, llm_check, yield, transform, builtin). |
| **Guardrails & Budget** | Per-agent blocked-term checks; per-agent USD / token / step / output limits with fallback models. |
| **Handoff vs. Consult** | Two collaboration verbs, human-in-the-loop by design: *handoff* delegates a task (the delegate's result is final); *consult* asks sub-agents and aggregates their results back for the lead to decide. |

**Always-on & addressable**

| Capability | How |
|---|---|
| **Persistent & multi-runtime** | Agents run on the server, on a user's own machine, or at the edge — each claiming work over the API. |
| **Chat & Resume** | `/v1/chat` runs a Loom Graph; `/v1/resume` continues from a yield. |
| **SSE Streaming** | Real-time token streaming with structured block interception (chart / mermaid / SVG). |

**Platform services**

| Capability | How |
|---|---|
| **LLM Router** | Runtime-configurable providers (OpenAI, DeepSeek, Gemini) — hot-swap without restart. |
| **Vector Memory** | pgvector-backed semantic memory with auto-remember, deduplication, and recall. |
| **Skill System** | Reusable prompt modules with progressive disclosure (SemanticMatcher + KeywordMatcher). |
| **MCP Integration** | Composite HTTP MCP host with per-tool filtering and per-server headers. |
| **Multi-tenancy** | HS256 JWT token-based tenant isolation. |
| **Console** | Embedded React 19 UI for workbench/chat, agent config (including memory records and skill bodies), teams, runs/latest checkpoints, runtimes, and settings. |

## Quickstart

### Docker Compose (recommended)

```bash
cd weave

# Configure env
cp deploy/.env.example deploy/.env
# Edit deploy/.env:
#   JWT_SECRET=$(openssl rand -hex 32)

# Build & start (the standard platform refresh entry point)
scripts/refresh-weave.sh

# Check
docker compose ps
docker compose logs -f weave
```

After startup:

| Service | URL | Description |
|---|---|---|
| **Weave + Console** | http://localhost:8080 | Same-origin management UI, REST, and SSE |
| **PostgreSQL** | localhost:5432 | `weave/weave` |

Default login: `admin` / `admin123`

Run `make smoke` to verify the real Weave process end-to-end with PostgreSQL and a local fake LLM.

### Configure an LLM Provider

```bash
# Get auth token
TOKEN=$(curl -s http://localhost:8080/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"admin123"}' | jq -r .token)

# Add OpenAI-compatible provider
curl -X POST http://localhost:8080/v1/providers \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "id": "openai",
    "name": "OpenAI",
    "base_url": "https://api.openai.com",
    "api_key": "sk-...",
    "models": ["gpt-4o", "gpt-4o-mini"]
  }'
```

### Create an Agent

```bash
curl -X POST http://localhost:8080/v1/agents \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-assistant",
    "model": "gpt-4o-mini",
    "spec": {
      "identity": { "core": "You are a helpful assistant." },
      "skills": [{ "name": "polite-tone" }],
      "profiles": {
        "formal": { "system_addition": "Use formal language.", "greeting": "Good day." },
        "casual": { "system_addition": "Be casual.", "greeting": "Hey!" }
      }
    },
    "guard": { "enabled": true, "blocked_terms": ["password", "secret"] },
    "max_tokens": 4000
  }'
```

### Chat

```bash
curl -X POST http://localhost:8080/v1/chat \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "agent": "my-assistant",
    "message": "Hello!",
    "profile": "formal",
    "stream": true
  }'
```

### CLI and MCP clients

The `weave` binary can call an already-running Weave service without loading
server configuration or opening PostgreSQL:

```bash
export WEAVE_API_URL=http://127.0.0.1:8080 # default
export WEAVE_API_KEY=wv_sk_xxx

weave team samples
weave team up -f templates/code-review.yaml \
  --idempotency-key 2bab1728-9dc0-4b61-a1f5-115fcaaf8e08
weave team dispatch --team <team-id> --task "Review this change" \
  --client-request-id 676506ec-1cf5-4c78-b324-29057591d3b5 --wait
weave status build <build-id>
weave status dispatch <client-request-id>
weave status team-run <run-snapshot-id>
weave deliverable list
weave deliverable get <deliverable-id>
```

Use an API key with role `admin` for team creation. The required scopes are
`org` for templates, teams, and build status; `chat` for dispatch, resume, and
deliverables; and `runs` for team-run status. `team_create` never invents an
idempotency key. Keep the same API key throughout one dispatch because request
ownership is key-specific. Leads backed by CLI runtimes reject asynchronous
dispatch. Deliverables appear only when an agent explicitly calls
`save_deliverable`; v1 has no team or run filter for deliverable listing.

Codex reads local stdio MCP servers from `~/.codex/config.toml` or a trusted
project's `.codex/config.toml`:

```toml
[mcp_servers.weave]
command = "/absolute/path/to/weave"
args = ["mcp", "serve"]
env = { WEAVE_API_URL = "http://127.0.0.1:8080" }
env_vars = ["WEAVE_API_KEY"]
```

Export `WEAVE_API_KEY` before starting Codex. For Claude Code, register the
same process with:

```bash
claude mcp add --transport stdio \
  --env WEAVE_API_URL=http://127.0.0.1:8080 \
  --env WEAVE_API_KEY="$WEAVE_API_KEY" \
  weave -- /absolute/path/to/weave mcp serve
```

## API Endpoints

| Method | Path | Description |
|---|---|---|
| `POST` | `/v1/auth/login` | Login with username/password |
| `POST` | `/v1/auth/token` | Issue JWT token (dev mode) |
| `GET` | `/v1/health` | Health check |
| `GET/POST/PUT/DELETE` | `/v1/agents` | Agent CRUD |
| `GET` | `/v1/agents/:name/topology` | Get compiled graph topology |
| `POST` | `/v1/agents/:name/preview-prompt` | Preview assembled system prompt |
| `POST` | `/v1/chat` | Chat (supports `stream: true`, `profile`, `context`) |
| `POST` | `/v1/resume` | Resume yielded graph |
| `GET/DELETE` | `/v1/sessions` | Session management |
| `GET` | `/v1/runs` | Run history & traces |
| `GET` | `/v1/usage` | Token & cost usage |
| `GET/POST/PUT/DELETE` | `/v1/providers` | LLM provider config |
| `GET/PUT` | `/v1/settings/embedder` | Embedding provider config |
| `GET/POST/PUT/DELETE` | `/v1/skills` | Skill CRUD |
| `GET/POST/DELETE` | `/v1/agents/:name/memories` | Agent memory CRUD |
| `POST` | `/v1/agents/:name/memories/search` | Semantic memory search |

## Configuration

| Env Var | Required | Default | Description |
|---|---|---|---|
| `DATABASE_URL` | Yes | -- | PostgreSQL connection string |
| `JWT_SECRET` | Yes | -- | HS256 signing key |
| `PORT` | No | `8080` | HTTP listen port |
| `WEAVE_DEV_MODE` | No | `false` | Enable dev endpoints (/v1/auth/token) |

LLM providers and embedder are configured at runtime via the API, not env vars.

## Project Structure

```
weave/
├── cmd/weave/main.go           Entry point
├── internal/
│   ├── api/                    REST API (Echo): chat, sse, agents, teams
│   ├── mcphost/                MCP HTTP host + write gate (fail-closed)
│   ├── compiler/               AgentRecord -> Loom Graph
│   ├── declarative/            Declarative graph factory (JSON -> Loom Graph)
│   ├── registry/               Agent / Skill / team storage & types
│   ├── llmrouter/              Multi-provider LLM routing
│   ├── memory/                 Vector memory (pgvector)
│   ├── embedder/               Embedding HTTP client
│   ├── storeext/               PGStore platform extensions
│   └── config/                 Env-based configuration
├── console/                    Legacy UI source (kept for reference; excluded from builds)
├── console-v2/                 Active React 19 + Vite UI embedded into Weave
├── deploy/                     Docker deployment files
└── docker-compose.yml          Full ecosystem compose
```

## License

Apache-2.0
