# lgllm

Lightweight Go reimplementation of the core of [9Router](https://github.com/decolua/9router):
one [OI]-compatible endpoint in front of many LLM providers, with fallback,
format translation and a token saver.

**Status: phase 2 complete** — a working streaming proxy in front of any
chat-completions provider, model routing by provider prefix, and provider
credentials from one YAML file. Format translation (Anthropic/Gemini/Responses)
lands in phase 4. See [`docs/PLAN.md`](docs/PLAN.md) for the roadmap.

Progress: phase 1 (skeleton, config, auth, server) and phase 2 (single-provider
streaming passthrough, provider registry, routing) are done and covered by tests,
plus the discovery compatibility endpoints (`/api/v1/models`, `/api/tags`,
`/api/show`, `/v1/props`).

## Why

9Router is excellent but heavy: Next.js 16 + React 19, ~100k lines of JS, 147
provider files, browser automation for web-login providers, SQLite via cgo. Most
of that is not needed to route a coding agent's traffic. lgllm keeps the useful
20% and aims for:

- one static binary, `CGO_ENABLED=0`
- no Node/npm at runtime
- one YAML config file
- streaming passthrough with no buffering

## Build and run

```bash
make build                 # -> bin/lgllm
cp config.example.yaml config.yaml
./bin/lgllm -config config.yaml
```

```bash
curl localhost:20128/healthz
curl localhost:20128/readyz
```

Flags:

| Flag | Purpose |
|---|---|
| `-config PATH` | config file (default `config.yaml`) |
| `-listen ADDR` | override `server.listen` |
| `-log-level LEVEL` | override `log.level` |
| `-log-format FORMAT` | override `log.format` |
| `-config-poll DURATION` | poll interval for file changes, `0` disables |
| `-check` | validate the config (including provider cross-references) and exit |
| `-version` | print build information |
| `-healthcheck` | probe a running instance and exit 0/1 |

Reload the config without restarting with `SIGHUP` (or by editing the file, when
polling is on). A failed reload keeps the previous config serving. Changing
`server.listen` requires a restart; everything else applies live.

## Using the proxy

Point any [OI]-compatible client at `http://127.0.0.1:20128/v1` and pick a model
as `provider/model` or `alias/model`:

```bash
curl http://127.0.0.1:20128/v1/models

curl http://127.0.0.1:20128/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"ds/deepseek-chat","messages":[{"role":"user","content":"hi"}]}'

# streaming, exactly as the provider emits it
curl -N http://127.0.0.1:20128/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"ds/deepseek-chat","stream":true,"messages":[{"role":"user","content":"hi"}]}'

# Anthropic clients: same registry, translated responses
curl http://127.0.0.1:20128/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{"model":"oa/gpt-4.1","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}'
```

Routing rules:

- `openai/gpt-4o`, `oa/gpt-4o` → the `openai` provider (id or alias).
- `gpt-4o` → `default_provider`, or the only usable provider.
- `deepseek-ai/DeepSeek-V3` → the slash is not a known provider, so it is part of
  the model id and goes to the default provider (OpenRouter-style ids work).
- Responses carry `X-Lgllm-Provider` and `X-Lgllm-Model` so a request can be
  traced to the hop that served it.

Status codes: `400` for an unroutable bare model or a bad body, `404` for an
unknown provider prefix, `502`/`504` for a failed/slow upstream, `501` for a
provider format with no translator yet (native Ollama `/api/chat`), and `503` when
no provider has a usable connection at all (an operator problem rather than a
client mistake).

### Compatibility endpoints

Client tools sometimes probe a different server's dialect before they will talk
to a proxy. These read-only endpoints answer in that dialect, sourced from the
same registry, so a model name taken from them works unchanged in
`/v1/chat/completions`:

| Endpoint | Shape |
|---|---|
| `GET /api/v1/models` | identical to `GET /v1/models` (pure alias) |
| `GET /api/tags` | Ollama `ListResponse`: `{"models":[{name, model, modified_at, size, digest, details, capabilities}]}` |
| `GET,POST /api/show` | Ollama `ShowResponse`; body `{"model":"..."}` or `?model=` |
| `GET /v1/props` | llama.cpp `/props`: `model_alias`, `n_ctx`, `modalities`, `build_info` |

`/props` is registered as well, so a llama.cpp client that hard-codes the native
path works without a rewrite.

They describe models; they never run one. Generation goes through
`/v1/chat/completions`, `/v1/messages`, or the provider's native generation
endpoint reached through the translator. Fields that
describe a local weight file (`size`, `parameter_size`, `quantization_level`,
`modelfile`, `template`) are empty rather than invented, and `capabilities`
reports `completion` plus whatever `models[].capabilities` declares in the
config. Ollama-shaped endpoints answer errors as `{"error":"..."}` (a flat
string), not the `[OI]` envelope.

Providers are declared in `providers`/`connections`. Everything in the built-in
catalog (anthropic, deepseek, gemini, groq, mistral, ollama, openai, openrouter)
only needs a connection entry; a custom gateway spells out `base_url`, `format`
and `auth`. A provider becomes usable only when at least one enabled connection
references it and has a key (except `auth: none`, e.g. local Ollama).

```yaml
connections:
  - id: ollama-local
    provider: ollama          # no key needed
  - id: deepseek-main
    provider: ds
    api_key: ${DEEPSEEK_API_KEY}
```

## Layout

```
cmd/lgllm/            entrypoint: flags, logging, signal handling, wiring
internal/config/      schema, defaults, validation, ${ENV} expansion, watcher
internal/auth/        API key verifier (constant time, hash-only option)
internal/registry/    provider catalog, config merge, validation, model routing
internal/upstream/    outbound HTTP client, streaming relay, stall detection
internal/translate/   pivot IR and OpenAI/Anthropic/Gemini/Responses translators
internal/balancer/    credential rotation, sticky sessions, cooldown/backoff
internal/server/      listener, middleware chain, routes, /v1 handlers
internal/version/     build metadata
docs/PLAN.md          phase-by-phase roadmap
```

## Design decisions worth knowing

**No global read/write timeout.** A chat completion can stream for minutes, so a
blanket timeout would cut it off. Only `ReadHeaderTimeout` and `IdleTimeout` are
applied at the server. Upstream timeouts are per phase: connect, response headers,
then an idle watchdog (`upstream.*` in the config). A client that stops reading is
also cut off, so it cannot pin a goroutine and an upstream connection forever.

**Streaming is byte-for-byte.** The relay flushes on every chunk and never parses
the payload. The SSE response headers are flushed as soon as the upstream answers,
so a client can distinguish "stream open, no token yet" from a stalled connect.
Buffering proxies (nginx, Cloudflare) are told to back off with
`X-Accel-Buffering: no` and `Cache-Control: no-cache`.

**Provider errors are relayed verbatim.** A 400 from a provider explains what to
fix ("context length exceeded", a bad tool-call id). Only a curated header set
(rate-limit hints, `Retry-After`) is copied; hop-by-hop headers are recomputed.

**Format translation uses OpenAI Chat as the pivot.** `internal/translate`
decodes the client's dialect into one request shape and encodes it for the
provider, so adding a dialect costs one decoder plus one encoder rather than a
pair per provider. `openai`, `anthropic`, native `gemini`, and
`openai-responses` are supported both ways, including streaming and tool calls.
When the client and provider speak the same dialect the body is forwarded
byte-for-byte; only the `model` member is rewritten when routing changes it.
`/v1/messages` speaks Anthropic to the client and can route to any provider.

**Failover is per attempt and per credential.** A request walks an ordered plan:
combo members first, and within each provider its credentials. A credential that
answers 401/402/429/5xx cools down (exponential, capped) and the next one serves;
a bad request (400/404/413/422) is request-scoped and never cools the account. A
`X-Lgllm-Session`/`X-Session-Id` header, Anthropic `metadata.user_id`, or
`prompt_cache_key` makes the choice sticky so the upstream prompt cache stays
warm. Streams are peeked before the first byte reaches the client when a fallback
exists, so a provider that answers 200 and then errors does not consume the
request. When every attempt fails, a single attempt is relayed verbatim; multiple
attempts produce a 502 that lists each provider, model, status and error.

**Auth defaults to `localhost`.** Loopback clients are trusted so a locally
installed CLI works with no setup; anything else must present a key. `auth.mode:
off` is refused when the listener is not on loopback, because that would turn the
process into an open relay billing the owner's provider accounts.

**Keys are compared in constant time and never logged.** `sha256:<hex>` entries
let a shared config store digests instead of plaintext. The client's own
`Authorization` header is never forwarded upstream; the provider key is the only
credential that leaves the process.

**Config parsing is strict.** Unknown keys, multiple YAML documents and duration
typos fail at startup instead of silently disabling a limit. Provider
cross-references (unknown connection target, missing key, bad `format`) are
checked by the registry before a config is accepted.

**`${VAR}` expansion happens before parsing, on scalar values only.** Keys are
never expanded, so a variable cannot inject YAML structure. An unresolved
variable without a `:-default` is a hard error rather than an empty string.

**Middleware order** is recover → request ID → access log → CORS → auth → body
limit. Recovery is outermost so a panic anywhere becomes a 500 instead of killing
in-flight streams.

## Development

```bash
make check     # gofmt + vet + test
make race      # tests under the race detector
make size      # binary size baseline
```
