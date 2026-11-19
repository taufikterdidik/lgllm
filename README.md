# lgllm

<img src="icon.png" alt="lgllm" width="88" align="right">

Lightweight Go reimplementation of the core of [9Router](https://github.com/decolua/9router):
one [OI]-compatible endpoint in front of many LLM providers, with fallback,
format translation and a token saver.

**Status: phases 1–31 implemented.** Streaming proxy, declarative provider
registry, aliases/combos with prefix inference, format translation
(OpenAI Chat/Anthropic/Gemini/Responses), combo fallback with credential rotation
and cooldowns, token saver, Prometheus metrics + usage log, encrypted secrets and
per-key rate limits, an admin REST API with an embedded web UI, packaging
benchmarks, a per-model TTFT/latency/TPS dashboard with p50, client-abort
classification, a model-detail endpoint with combo context windows, a form-based
admin UI with generated and named client keys, bbolt-backed performance history,
per-connection concurrency caps, and app/favicon assets.
9Router parity is not the goal; see
[`docs/PLAN.md`](docs/PLAN.md) for the phase-by-phase scope and what each phase
deliberately leaves out.

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

Other entry points:

```bash
make check                 # gofmt + vet + test
make snapshot              # cross-platform archives via goreleaser
make image-size            # scratch image, prints its size
make bench                 # local size/start/RSS/latency/throughput numbers
```

## Releases

A pushed annotated tag such as `v0.1.0` starts
[`.github/workflows/release.yml`](.github/workflows/release.yml). The workflow
runs the test gate, builds archives and deb/rpm/apk packages with GoReleaser,
builds a signed multi-arch `ghcr.io/taufikterdidik/lgllm` image, and publishes
the GitHub release only after both artifact sets succeed. Pre-release tags such
as `v0.2.0-rc.1` are marked as prereleases and do not move `latest`.

Release artifacts include the static binaries, checksums, `LICENSE`, the example
config, and the systemd unit. The container image runs as UID 65532 and uses
`/lgllm -healthcheck` internally. The operator checklist is in
[`docs/RELEASE.md`](docs/RELEASE.md).

The Docker image is `scratch` + the static binary, CA certificates and tzdata:
about 9 MB in the release check. It listens on `0.0.0.0:20128` inside the container;
publish it only on loopback or behind your own TLS/auth boundary, and set
`auth.mode: required` before exposing it beyond localhost. A systemd unit and
package post-install script live in [`deploy/`](deploy/), and release archives are
produced by [`.goreleaser.yaml`](.goreleaser.yaml) (deb/rpm/apk plus
linux/darwin/windows amd64+arm64). Measured numbers and methodology:
[`docs/BENCH.md`](docs/BENCH.md).

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
| `-master-key KEY` | decrypt `enc:v1:` secrets (prefer `LGLLM_MASTER_KEY`) |
| `-encrypt` | read a secret from stdin and print its `enc:v1:` form |

Reload the config without restarting with `SIGHUP` (or by editing the file, when
polling is on). A failed reload keeps the previous config serving. Changing
`server.listen` requires a restart; everything else applies live.

## Config CLI

`lgllm config` edits the YAML file directly; it does not need a running server.
The edits are applied to the YAML node tree, so comments and `${ENV}`
placeholders survive, and the result is validated with the server's parser
before the file is replaced atomically. Run it with no command on a terminal for
an interactive menu.

```bash
lgllm config init                                  # starter config.yaml

# A catalog provider needs no declaration: a connection is enough.
lgllm config connection add --provider openai --id work --api-key '${OPENAI_API_KEY}'

# Custom / self-hosted provider
lgllm config provider add --id lab \
  --base-url https://gateway.internal/v1 --format openai --auth bearer --model qwen3

# Refresh the advertised model list from the provider's GET /models
lgllm config models sync --provider lab --dry-run
lgllm config models sync --provider lab --replace
lgllm config connection test --provider lab

lgllm config alias set fast ds/deepseek-chat
lgllm config combo add cheap --member ds/deepseek-chat --member or/google/gemini-2.5-pro
lgllm config show
lgllm config validate
```

`models sync` merges by default, preserving the name, context and capabilities
of models that are still advertised; `--replace` drops the ones the provider no
longer lists. The runtime view used for discovery includes `admin.yaml` and
decrypted `enc:v1:` secrets (`LGLLM_MASTER_KEY` or `--master-key`), but writes go
to the main file only and are refused for resources managed through the admin
API. The running server picks the change up on its next config poll, or send
`SIGHUP`.

| Command | Purpose |
|---|---|
| `provider list\|add\|set\|remove` | catalog and custom providers, including headers, session header and models |
| `connection list\|add\|set\|remove\|test` | credentials, base URL overrides, live `/models` probe |
| `models list\|sync` | configured models; `--upstream` previews the provider list |
| `alias list\|set\|remove` | client-facing model aliases |
| `combo list\|add\|set\|remove` | fallback groups |
| `show`, `validate`, `init`, `wizard` | inspect, check, bootstrap, interactive menu |

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
| `GET /v1/models/{model}` | OpenAI model object for a routable id, alias, or combo; `{model}` may contain slashes |
| `GET /api/v1/models/{model}` | identical to `GET /v1/models/{model}` |
| `GET /v1/model/{model}` | OpenRouter-shaped `{"data":...}` single-model detail, including context and combo members |
| `GET /api/v1/model/{model}` | identical to `GET /v1/model/{model}` |
| `GET /api/tags` | Ollama `ListResponse`: `{"models":[{name, model, modified_at, size, digest, details, capabilities}]}` |
| `GET,POST /api/show` | Ollama `ShowResponse`; body `{"model":"..."}` or `?model=` |
| `GET /v1/props` | llama.cpp `/props`: `model_alias`, `n_ctx`, `modalities`, `build_info` |

`/props` is registered as well, so a llama.cpp client that hard-codes the native
path works without a rewrite.

The model-detail wildcard echoes the id that was requested and accepts the same
names as generation: provider-prefixed ids, aliases, combos, and bare names
routed by `default_provider`. Model lists remain hints rather than an allow-list,
so an unlisted `provider/model` still resolves.

A combo reports the smallest context among its members, but only when every
member has context metadata. If any member's context is unknown, the response
sets `context_known: false` and omits the numeric window instead of advertising
an unsafe limit. `/api/show` and `/v1/props` use the same effective value, so a
client that compacts around `n_ctx` sees the combo minimum rather than the first
member's size.

They describe models; they never run one. Generation goes through
`/v1/chat/completions`, `/v1/messages`, or the provider's native generation
endpoint reached through the translator. Fields that
describe a local weight file (`size`, `parameter_size`, `quantization_level`,
`modelfile`, `template`) are empty rather than invented, and `capabilities`
reports `completion` plus whatever `models[].capabilities` declares in the
config. Ollama-shaped endpoints answer errors as `{"error":"..."}` (a flat
string), not the `[OI]` envelope.

### Admin API

Set `admin.enabled: true` to manage providers, connections, combos and client
keys over HTTP. It is off by default, uses its own credential (`admin.token`,
`Authorization: Bearer ...` or `X-Admin-Token`), and never accepts inference
keys. Without a token it is loopback-only and a public bind is refused.

| Endpoint | Methods |
|---|---|
| `/api/providers` | `GET` (`?all=1` includes the catalog), `POST`; `PUT/DELETE /api/providers/{id}` |
| `/api/catalog` | `GET` built-in provider definitions |
| `/api/connections` | `GET`, `POST`; `PUT/DELETE /api/connections/{id}` |
| `/api/combos` | `GET`, `POST`; `PUT/DELETE /api/combos/{name}` |
| `/api/keys` | `GET`, `POST`; `PUT/DELETE /api/keys/{id}` |
| `/api/keys/generate` | `POST` with optional `{ "name": "..." }`; returns a new `sk-lgllm-...` key once |
| `/api/usage?from=&to=&group=provider\|model\|day` | `GET` |
| `/api/performance?provider=` | `GET` (recent TTFT/latency/TPS per model) |
| `/api/logs?limit=N` | `GET` |
| `/api/logs/stream` | `GET` (SSE, live entries) |
| `/api/test-connection` | `POST {"provider":"...","model":"...","connection":"..."}` |

The main `config.yaml` is never rewritten (it may contain `${ENV}` references and
comments). API-created resources live in `admin.yaml` next to it, merged at
startup, written atomically with mode 0600, and secrets are encrypted when a
master key is configured. Entries from `config.yaml` are listed with
`"managed":"config"` and are read-only through the API; built-in catalog entries
use `"managed":"catalog"` and can be customized by creating an admin override. A
mutation is validated
against the full config and the provider catalog before it is published; a
rejected change rolls the file back. `GET` redacts keys (`***`); sending `***`
back keeps the stored value. When `metrics.usage_log_size` is 0 the usage and log
endpoints are empty and the live stream is unavailable.

### Admin UI

With `admin.enabled: true`, open `http://127.0.0.1:20128/ui/` and paste the admin
token. The page is plain HTML/CSS/JS embedded in the binary (no Node, no asset
directory) and has tabs for providers, connections, combos, keys, usage,
performance, live logs, and settings. It calls the admin API with `X-Admin-Token`;
the token is kept in `localStorage` for that browser only. Static assets carry a
strict `Content-Security-Policy` and are served without an inference key, while
every API call still needs the admin token. When the admin API is off, `/ui/`
answers 404.

Providers, connections, and combos are configured with forms, not raw JSON:
choose a built-in catalog entry (or a custom endpoint), edit headers and models,
pick a provider from a dropdown for a connection, and build combo members from
the model list. Keys has a **Generate key** action that creates a random
`sk-lgllm-...` value with an optional user-facing name, shows it once, and copies
it to the clipboard; only the name and hash prefix are listed afterwards, and
admin keys can be renamed without changing the secret. Config-managed entries
remain read-only.

### Observability

| Endpoint | Shape |
|---|---|
| `GET /metrics` | Prometheus text: requests by ingress/provider/model/status, duration histogram, tokens, token-saver bytes, failed attempts |
| `GET /usage?limit=N` | recent requests (newest first): request id, provider/model, status, duration, tokens, saved bytes, estimated cost, error |
| `GET /api/performance` | last N attempts per advertised model: TTFT, latency, TPS, success rate, split by stream/non-stream |
| `metrics.pprof_listen` | optional separate loopback listener exposing `/debug/pprof/` |

`/metrics` and `/usage` sit behind the same auth as the API (only health checks
are public), so a scraper in `auth.mode: required` needs a key. `metrics.enabled:
false` turns both into 404s while counters keep running in memory. Costs come
from a small manual table; an unknown model is reported as `cost_known: false`,
not as a misleading zero.

### Client integration

Both ingress dialects are available. Use a `provider/model`, an alias, or a combo
name from `GET /v1/models` as the model.

**Claude Code** (Anthropic ingress, translated to any provider):

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:20128
export ANTHROPIC_AUTH_TOKEN=sk-your-lgllm-key   # optional in auth.mode: localhost
claude --model cc/claude-sonnet-4-5             # or an alias such as "fast"
```

**Cline / Continue** (OpenAI-compatible): base URL
`http://127.0.0.1:20128/v1`, API key from `auth.keys` (any string in loopback
mode), model e.g. `ds/deepseek-chat`, `oa/gpt-4.1`, or a combo name.

**OpenCode** (`opencode.json`):

```json
{
  "provider": {
    "lgllm": {
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": "http://127.0.0.1:20128/v1", "apiKey": "sk-your-lgllm-key" },
      "models": { "fast": { "name": "fast" } }
    }
  }
}
```

**Codex CLI** (`~/.codex/config.toml`). The `/v1/responses` ingress is not
implemented, so use the chat wire API:

```toml
model = "oa/gpt-4.1"
model_provider = "lgllm"

[model_providers.lgllm]
name = "lgllm"
base_url = "http://127.0.0.1:20128/v1"
env_key = "LGLLM_API_KEY"
wire_api = "chat"
```

Providers are declared in `providers`/`connections`. Everything in the built-in
catalog (anthropic, commandcode, commandcode-claude, deepseek, gemini, groq,
mistral, ollama, openai, opencode-go, opencode-go-anthropic, openrouter) only
needs a connection entry;
a custom gateway spells out `base_url`, `format` and `auth`. A provider becomes
usable only when at least one enabled connection references it and has a key
(except `auth: none`, e.g. local Ollama).

```yaml
connections:
  - id: ollama-local
    provider: ollama          # no key needed
  - id: deepseek-main
    provider: ds
    api_key: ${DEEPSEEK_API_KEY}
  - id: go-main
    provider: ocg             # OpenCode Go
    api_key: ${OPENCODE_GO_API_KEY}
  - id: commandcode-main
    provider: cmd
    api_key: ${COMMAND_CODE_API_KEY}
  - id: commandcode-claude
    provider: cmdc            # Claude models need the Messages surface
    api_key: ${COMMAND_CODE_API_KEY}
```

Each connection can also cap how many upstream streams it serves at once:

```yaml
connections:
  - id: deepseek-main
    provider: ds
    api_key: ${DEEPSEEK_API_KEY}
    max_concurrent: 4         # 0 or absent = unlimited
```

The slot covers the whole attempt, including the response body, so a long stream
holds it until the client finishes reading. When every credential of a provider
is full, the request moves to the next combo member; if nothing accepts it,
lgllm returns `429` with `Retry-After` instead of queueing indefinitely. A
connection at its cap is not marked unhealthy and does not enter cooldown.
`lgllm_connection_concurrency_limited_total{provider,connection}` counts the
skipped attempts. The CLI exposes the same knob:

```bash
lgllm config connection set --id deepseek-main --max-concurrent 4
```

OpenCode Go is OpenAI-compatible but requires a stable per-conversation id in
`x-opencode-session`; without it the gateway answers `400 MissingSessionID`.
lgllm fills the header from a client session header (`X-Lgllm-Session`,
`X-Opencode-Session`, `X-Session-Id`, …) or, when none is present, from a
deterministic fingerprint of the first user message, so every turn of a
conversation reuses the same id while unrelated chats get different ones. The
capability is declarative, so a custom gateway can opt in:

```yaml
providers:
  - id: my-gateway
    base_url: https://gateway.example/v1
    session_header: x-opencode-session
```

The same gateway is also available on the Anthropic Messages surface as
`opencode-go-anthropic` (`ocga`). It uses the same model ids and credential but
serves `https://opencode.ai/inference/go/anthropic/v1`, so Claude Code and
Anthropic SDK clients can call models such as `ocga/qwen4-max` natively. Add a
separate connection because one connection belongs to one provider:

```yaml
connections:
  - id: ocg-humas
    provider: opencode-go
    api_key: ${OPENCODE_GO_API_KEY}
  - id: ocga-humas
    provider: opencode-go-anthropic
    api_key: ${OPENCODE_GO_API_KEY}
```

Command Code is also built in. It serves OpenAI chat models and Claude models
from the same base URL, but Claude only answers on the Anthropic Messages
endpoint, so the catalog has two entries: `commandcode` (`cmd`) for the chat
models and `commandcode-claude` (`cmdc`) for Claude. Both read
`COMMAND_CODE_API_KEY`; add a connection to each if you want both groups. The
model hints come from `GET /provider/v1/models`; a model id is still forwarded
unchanged, so vendor-prefixed ids such as `deepseek/deepseek-v4-flash` work even
when they are not listed.

## Layout

```
cmd/lgllm/            entrypoint: flags, logging, signal handling, wiring
internal/config/      schema, defaults, validation, ${ENV} expansion, watcher
internal/configcmd/   `lgllm config` editor (node-preserving YAML, model sync)
internal/auth/        API key verifier (constant time, hash-only option)
internal/registry/    provider catalog, config merge, validation, model routing
internal/upstream/    outbound HTTP client, streaming relay, stall detection
internal/translate/   pivot IR and OpenAI/Anthropic/Gemini/Responses translators
internal/balancer/    credential rotation, sticky sessions, cooldown/backoff
internal/sessionid/   stable opaque session ids for providers that require one
internal/tokensaver/  tool-result compression (diff, grep, JSON, logs)
internal/metrics/     Prometheus text exposition
internal/perf/        per-model attempt window + read-only SSE usage tap
internal/usagelog/    in-memory ring of recent requests
internal/pricing/     manual per-model cost table
internal/secret/      AES-GCM encryption for credentials at rest
internal/ratelimit/   per-key request buckets and daily token quota
internal/admin/       management API state, overlay persistence, rollback
internal/server/ui/   embedded admin UI (plain HTML/CSS/JS, no Node build)
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

**Streaming is byte-for-byte.** The relay flushes on every chunk and never rewrites
the payload; a read-only tap observes usage and first-token timing without
buffering it. The SSE response headers are flushed as soon as the upstream answers,
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

**Large tool results are compressed, never user text.** The token saver only
rewrites `role: tool` content. It skips failed results, keeps the head and tail of
every payload, and appends a marker so a retry cannot compress twice. `git diff`,
grep/status output, JSON and long logs get shape-aware filters. `X-Lgllm-Token-Saver:
off` disables it for one request, `combos[].token_saver: false` for one combo, and
the response header reports `saved=N/M` bytes.

**The admin API edits an overlay, not your config.** Writing back the main
config would resolve `${ENV}` placeholders and drop comments, so API-created
providers, connections, combos and keys go to `admin.yaml`. Each mutation is
serialized, persisted atomically, validated end-to-end (including
`registry.Validate`) and then swapped into the running server; a failure restores
the previous file. Config-managed entries stay read-only, which keeps deletion
unambiguous.

**Secrets can stay encrypted on disk.** A connection key (or client key, or
provider header) written as `enc:v1:<base64>` is AES-256-GCM ciphertext. The
master key comes from `LGLLM_MASTER_KEY` or `--master-key`, never from the YAML
file. Generate a value with `printf '%s' "$KEY" | lgllm -encrypt`. A config with
ciphertext but no key refuses to start rather than sending the ciphertext
upstream.

**Limits are per client key.** `auth.rate_limit.requests_per_minute` is a token
bucket (with `burst`), and `tokens_per_day` is a quota charged from the usage of
each response. The bucket key is a hash of the presented API key, or a shared
`local` bucket for trusted loopback clients. Rejections are 429 with
`Retry-After`, and `/metrics`/`/usage` are exempt so monitoring still works when
a key is over budget. Limits live in memory and reset on restart; the daily quota
resets at 00:00 UTC.

**Observability is local and cheap.** `/metrics` is rendered by hand (no client
library), the usage log is a bounded in-memory ring (500 entries by default), and
pprof gets its own loopback listener rather than sharing the API port. Request
bodies are logged only at debug level, truncated and with keys/tokens redacted.

**Recent performance is measured, not inferred.** Each upstream attempt records
TTFT (first content event), latency (last byte) and TPS in a small per-model ring
(`metrics.performance_window`, default 10). Stream TPS uses the provider's usage
between the first and last content event; a JSON response has no first-token
signal, so its TPS is `tokens/latency` and the UI labels it `est.`. Failures
count toward the success rate but not toward the averages or p50, and a provider
that reports no usage yields `n/a` rather than a guessed token count. The tap is
read-only, so a passthrough stream stays byte-identical.

The Performance tab shows the **p50 (median)** of the last window. The API keeps
`avg_*` fields for compatibility and the Prometheus gauges remain averages for
aggregate monitoring; `p50_*` fields are returned alongside them. p50 is more
stable than a mean over a small window; p95/p99 are intentionally deferred until
the window or persisted history is large enough to make them meaningful.

The expanded time-series charts have a **Linear / Log** toggle. Linear is the
default; the choice is stored in `localStorage`. Log mode compresses a wide
latency spread so one slow outlier no longer flattens every fast sample, and
axis labels switch to seconds once they pass 1000 ms. A range smaller than 2x
stays linear because log would not add information there; the table sparkline
also stays linear. Failed attempts remain dashed red markers in both modes.

The table also has an **`i/o/c`** column: USD per one million tokens for input,
output and cache read. It shows the base list price, so time-of-day multipliers
and long-context tiers beyond the first band are not applied. Free models show
`free`; a model with no published price shows `—`. OpenCode Go prices come from
its `/models` response; Command Code has no price field in that endpoint, so its
table is curated from the published pricing page. `GET /api/performance` returns
the same numbers under `pricing` (`input`, `output`, `cache_read`,
`cache_write`, `known`).

The ring is the fast read path. Set `metrics.performance_db` to a bbolt file to
also persist samples: writes go through a bounded background queue, the latest
window is loaded at startup, samples older than `metrics.performance_retention`
(default 168h) are pruned, and each provider/model/connection is capped at 2000
rows. Leaving `performance_db` empty keeps the old in-memory-only behavior. Under
systemd, use a writable path such as `/var/lib/lgllm/performance.db`.

The Performance tab also keeps a ring per connection, so one slow credential can
be separated from a healthy one on the same provider. Click a row for the
TTFT/latency and TPS time-series charts plus the per-connection table; a mode
with fewer than three successes is marked `low`. `GET /api/performance` returns
the same data (add `?series=1` for the chart points), and `/metrics` exposes
`lgllm_model_ttft_seconds`, `lgllm_model_tps` and
`lgllm_model_requests_window` with `provider`, `model` and `mode` labels.

**A client abort is not a model failure.** Some providers, notably Command Code,
send `finish_reason` and the final `usage` chunk separately. A client that stops
at `finish_reason` closes the connection while lgllm is still reading that
trailing chunk, which used to surface as `context canceled` and lower the
model's success rate. The stream tap now remembers the finish signal: a close
after finish is recorded as a completed success (usage may stay `n/a`), a close
before finish is left out of the performance window, and both are counted in
`lgllm_client_cancelled_total{provider,after_finish}`.

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
