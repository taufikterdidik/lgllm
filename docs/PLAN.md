# lgllm — Padanan 9Router versi lightweight (Go)

Dokumen ini adalah rencana pembangunan (roadmap) bertahap: dari core proxy minimal
sampai UI opsional. Diturunkan dari pembacaan source `decolua/9router` (v terbaru):

```
src/app/api/v1/{chat/completions, messages, responses, models, embeddings, audio, images, videos, search}
src/sse/{handlers,services,utils}      -> lapisan Next.js (auth key, quota, logging)
open-sse/providers/registry/*.js (147) -> deklarasi provider (baseUrl, format, model list)
open-sse/translator/{request,response} -> terjemahan format antar-provider
open-sse/executors/*.js (47)           -> eksekutor khusus (OAuth/web/browser)
open-sse/services/{combo,accountFallback,tokenRefresh}.js
open-sse/rtk/*                         -> kompresi tool_result
src/lib/db/* (SQLite)                  -> config, usage, request log
```

---

## 0. Prinsip & batas lingkup

9Router itu "everything router": 40+ provider, OAuth web login, browser pool, MITM,
multi-bahasa, cloud sync, Next.js 16 + React 19. Yang kita ambil hanya inti yang
memberi 90% nilai, sisanya dipotong.

| Kapabilitas | 9Router | lgllm v1 | Alasan |
|---|---|---|---|
| Ingress OpenAI-compatible (`/v1/chat/completions`) | ✅ | ✅ wajib | ini yang dipakai semua CLI |
| Ingress Anthropic (`/v1/messages`) | ✅ | ✅ selesai fase 4 | Claude Code butuh ini |
| Ingress Gemini/responses | ✅ | opsional | OpenAI Chat + Anthropic sudah cukup |
| Egress provider API-key (OpenAI/Anthropic/Gemini/Groq/DeepSeek/OpenRouter/GLM/Ollama) | ✅ | ✅ | inti |
| Provider OAuth / web-login / browser automation (Kiro, Cursor, Codex-web) | ✅ | ❌ | berat + rawan ToS, bukan "lightweight" |
| Auto-fallback + multi-account round-robin | ✅ | ✅ fase 5 | pembeda utama |
| Kompresi tool_result (RTK) | ✅ | ✅ versi lite fase 6 | hemat token nyata |
| Caveman/Ponytail prompt injection | ✅ | opsional fase 6 | murah, tinggal prompt |
| Dashboard UI | Next.js | fase 10, statis + `go:embed` | opsional |
| SQLite | ✅ | fase 3 (awalnya YAML) | hindari cgo |
| Cloud sync, i18n, updater, tunnel, media (STT/TTS/image/video) | ✅ | ❌ | out of scope v1 |

Target teknis "lightweight":
- 1 binary statis (CGO off), ~12–20 MB, RSS idle < 40 MB, start < 100 ms.
- Runtime tanpa Node/npm. Binary + 1 file config.
- Target ~8–12 ribu baris Go, bukan ~100 ribu baris JS.

Catatan legal: 9Router MIT, boleh jadi acuan arsitektur, tetap cantumkan atribusi.
Jangan port jalur OAuth/web-login provider gratisan — itu bagian paling rapuh dan
paling berkaitan dengan ToS provider.

---

## 1. Fondasi repo (hari 1) — ✅ SELESAI

> Implementasi: `cmd/lgllm`, `internal/{config,auth,server,version}`,
> `config.example.yaml`, `Makefile`, `Dockerfile`, CI. Binary 7.2 MB (CGO off),
> `make check` hijau termasuk `-race`.

**Yang sudah ada:**

- `Config` + default + validasi strict (unknown key, multi-document, tipe durasi).
- Ekspansi `${VAR}` / `${VAR:-default}` sebelum parsing; gagal keras bila var hilang.
- Watcher config: SIGHUP + polling mtime, swap atomik, reload gagal tidak menurunkan layanan.
- `auth.Verifier` constant-time, dukung `sha256:<hex>`, rotasi key live, fail-closed.
- Server: recover → request ID → access log → CORS → auth → body limit.
  `ReadHeaderTimeout`/`IdleTimeout` saja (tanpa timeout total, supaya stream aman).
- `/healthz`, `/readyz`, `/version`, `/`, dan `/v1/*` (501 informatif).
- `-check`, `-version`, `-healthcheck` (untuk distroless), `-listen`, `-log-*`, `-config-poll`.
- Graceful shutdown SIGINT/SIGTERM, binary statis, image Docker scratch.

**Temuan saat implementasi (penting untuk fase berikutnya):**

1. `http.Server` tanpa `ReadTimeout`/`WriteTimeout` — jangan pernah menambahkannya
   global; fase 2 harus pakai deadline per-fase + stall detector.
2. `statusRecorder` wajib meneruskan `Flush()` dan `Unwrap()`, kalau tidak SSE mati diam-diam.
3. Middleware auth memakai `s.Config()` per request, bukan snapshot, agar reload berlaku.
4. Port `20128` sering sudah dipakai 9Router; default tetap sama supaya drop-in,
   tapi sediakan `-listen` untuk menjalankan keduanya berdampingan.

Struktur:

```
lgllm/
  cmd/lgllm/main.go          # wiring, flags, signal handling
  internal/
    config/                  # load + validasi + reload YAML/env
    server/                  # HTTP server, middleware, routing
    auth/                    # API key lokal, hash, middleware
    registry/                # deklarasi provider (Go literal, bukan file per provider)
    provider/                # adapter: openai, anthropic, gemini, ollama, custom
    router/                  # resolve model -> target, combo, fallback, cooldown
    translate/               # IR + request/response translator + stream state machine
    tokensaver/              # filter git-diff/grep/ls/json + guard
    usage/                   # pencatatan token, biaya, request log
    store/                   # interface + implementasi YAML dulu, SQLite nanti
    api/                     # handler /v1/* dan /api/*
  internal/translate/testdata # golden files
  web/                       # (fase 10) SPA, di-embed
  docs/PLAN.md
  Makefile, Dockerfile, goreleaser.yaml
```

Dependensi awal sengaja sedikit:
`net/http` (Go 1.22 pattern routing), `gopkg.in/yaml.v3`, `log/slog`.
Belum pakai framework. Tambah `modernc.org/sqlite` (pure Go) hanya di fase 3.

Definisi selesai fase 1:
- `lgllm -config config.yaml` jalan, `/healthz` OK, config tervalidasi dengan pesan error jelas,
  `SIGHUP` reload config.

### 1b. Belum masuk fase 1 (saat fase itu selesai)

- **Store/CRUD.** Belum ada paket `store`: YAML adalah satu-satunya sumber config,
  dan belum ada cara menulis balik dari API. SQLite + CRUD masuk fase 9.
- **Auth lanjutan.** Baru API key statis (`auth.keys`); belum ada enkripsi secret,
  refresh token, atau OAuth — fase 8, dan OAuth web-login tetap out of scope.
- **Observability.** Belum ada `/metrics`, usage log, harga, atau pprof — fase 7.
- **UI / admin API.** Belum ada; fase 9–10.
- **Provider adapter terpisah.** Registry masih satu paket Go dengan katalog;
  adapter per format menyusul bersama translasi (fase 4).

---

## 2. Core proxy satu provider (hari 2–3) — ✅ SELESAI

Tujuan: menyalurkan request ke satu upstream tanpa mengubah apa pun (passthrough).
Ini fondasi untuk semua fase berikutnya.

- `POST /v1/chat/completions` dan `GET /v1/models`.
- Streaming: **jangan buffer**. Baca upstream dengan `bufio.Scanner`/`io.Copy` ke
  `http.ResponseWriter` + `Flush()` per event. Set header `text/event-stream`,
  `Cache-Control: no-cache`, matikan kompresi.
- Propagasi `r.Context()` ke `http.NewRequestWithContext` supaya client disconnect
  langsung membatalkan request upstream (hemat token!).
- Timeout berlapis: dial 10s, response header 120s, **tanpa** total timeout untuk stream,
  tapi idle/stall timeout (mis. 90s tanpa byte → abort).
- Error handling: upstream 4xx/5xx diteruskan apa adanya, plus `X-Lgllm-Provider`.
- Middleware: request ID, access log (slog), recover panic, body size limit,
  CORS untuk browser client.
- API key lokal: `Authorization: Bearer <key>`; mode `localhost-only` (tanpa key kalau
  bind ke 127.0.0.1) meniru perilaku 9Router.

Definisi selesai: `curl` stream ke OpenAI/DeepSeek lewat lgllm, throughput sama
dengan langsung (uji dengan `hey`/`vegeta`), latency overhead < 5 ms.

**Temuan saat implementasi (penting untuk fase berikutnya):**

1. **Tidak ada `http.Client.Timeout`.** Timeout header diterapkan lewat
   `Transport.ResponseHeaderTimeout` di client terpisah per nilai timeout
   (`byTimeout`), bukan context deadline — context deadline juga mematikan body,
   sehingga stream sehat ikut terpotong.
2. **Watchdog stall menutup `resp.Body`, bukan membatalkan context.** Menutup body
   langsung membangunkan `Read` yang macet.
3. **`SetWriteDeadline` per chunk** (`http.ResponseController`) mencegah client yang
   berhenti membaca menahan goroutine selamanya; deadline dibersihkan di akhir agar
   koneksi keep-alive tidak keracunan.
4. **Registry dibangun sebelum swap config.** `Watcher.onChange` kini
   `func(*Config) error`: registry baru divalidasi lebih dulu, dan error menolak
   seluruh reload sehingga snapshot lama tetap melayani.
5. **Katalog menyimpan API root**, bukan endpoint penuh; proxy menambahkan
   `/chat/completions`. Gemini diarahkan ke root kompatibel `[OI]`
   (`/v1beta/openai`) supaya bisa lewat passthrough fase 2 tanpa translator.
6. **Prefix model yang bukan provider dianggap bagian dari id model**, sehingga
   id ala OpenRouter (`vendor/model`) tetap bekerja. Tanpa prefix, provider default
   dipakai; bila tidak bisa disimpulkan, jawabannya 400 dengan contoh format.
7. Provider non-`openai` dijawab 501 eksplisit sampai translator fase 4 ada.
8. **Header SSE di-flush sebelum byte pertama.** Tanpa flush eksplisit setelah
   `WriteHeader`, `net/http` menahan status dan header sampai ada 32 KiB body
   atau handler selesai. Akibatnya client tidak tahu stream sudah terbuka, dan
   `stream_write_timeout` baru bisa mendeteksi client yang pergi jauh lebih
   lambat. Satu `ResponseController.Flush()` tepat setelah `WriteHeader` untuk
   respons SSE memperbaikinya (terlihat langsung sebagai hang di test
   client-disconnect).

Temuan lain saat stabilisasi test fase 2:

9. **`httptest.Server.Close` menunggu handler yang masih berjalan**, jadi mock
   upstream yang "tidak pernah menjawab" membuat seluruh suite menggantung, bukan
   gagal. Handler mock harus berhenti pada `r.Context().Done()` atau setelah
   batas waktu pendek.
10. **Non-stream response memakai `Content-Length` upstream.** Body-nya disalin
    utuh, jadi panjang asli masih valid; ini menghindari chunked encoding untuk
    respons JSON biasa.
11. **Tanpa provider yang bisa dipakai, jawabannya 503, bukan 404.** 404 ambigu
    dengan "model tidak dikenal", padahal penyebabnya config kosong.

### 2b. Belum masuk fase 2 (saat fase itu selesai)

- **Translasi format.** Provider non-OpenAI dijawab 501; implementasinya fase 4.
- **Fallback & multi-akun.** Connection pertama selalu dipakai, belum ada retry,
  rotasi, atau cooldown — fase 5.
- **Alias & combo.** Baru `provider/model`; nama pendek dan grup menyusul fase 3.
- **Token saver.** Body diteruskan apa adanya; kompresi tool result fase 6.
- **Observability.** Belum ada metrik, usage log, atau estimasi biaya — fase 7.
- **Ingress Ollama native.** `/api/chat` dan `/completion` belum ada; yang tersedia
  baru endpoint discovery (`/api/tags`, `/api/show`, `/v1/props`).
- **Endpoint media/embedding.** Hanya chat completions; embeddings, image, audio
  tetap out of scope v1.

---

## 3. Registry provider & routing model (hari 4–6) — ✅ SELESAI

> Status: registry deklaratif, materialisasi katalog, validasi lintas-referensi,
> dan resolusi `provider/model` sudah jalan sejak fase 2. Fase 3 melengkapinya
> dengan alias model (`fast` → `ds/deepseek-chat`), inferensi prefix (`gpt-*` →
> openai, `claude-*` → anthropic), `combos[]`, dan pencantuman keduanya di
> `/v1/models`. Yang belum: rotasi multi-akun, retry/fallback antar-member combo,
> dan penyimpanan SQLite/CRUD — itu fase 5/9.


Model deklaratif, bukan kode per provider:

```go
type Provider struct {
    ID       string
    Alias    string
    Category string   // apikey | oauth | free
    BaseURL  string
    Format   Format   // openai | anthropic | gemini | openai-responses | ollama
    Auth     AuthSpec // header + scheme + source
    Headers  map[string]string
    Models   []Model
    Timeout  time.Duration
}

type Model struct {
    ID, Name string
    Kind     string // llm | embedding | image...
    Context  int
    Capabilities map[string]bool // vision, tools, pdf
}
```

- Registry awal cukup 6–8 provider: `openai`, `anthropic`, `gemini`, `deepseek`,
  `groq`, `openrouter`, `ollama`, `custom*` (OpenAI-compatible generik).
- Resolusi nama model: `provider/model`, alias provider (`cc/` → anthropic),
  alias model (`fast` → `groq/llama-...`), dan inferensi prefix (`gpt-*` → openai,
  `claude-*` → anthropic) dengan fallback ke provider default.
- Simpan daftar koneksi (akun) + API key di store. Mulai YAML; migrasi ke SQLite
  begitu butuh usage log dan CRUD dari UI.
- Format config: satu file `config.yaml` berisi `server`, `providers[]`,
  `connections[]`, `combos[]`, `keys[]`, `tokensaver`.

Definisi selesai: `GET /v1/models` mengembalikan gabungan model + combo + alias
dalam format OpenAI (`{"object":"list","data":[...]}`), dan routing `provider/model`
bekerja untuk 3 provider berbeda.

### 3a. Endpoint kompatibilitas discovery — ✅ SELESAI

Client lokal sering menanyakan dulu "server ini punya model apa" dalam dialek
masing-masing. Empat endpoint read-only ditambahkan, semuanya membaca registry
yang sama sehingga nama dari daftar itu bisa langsung dipakai di
`/v1/chat/completions`:

| Endpoint | Dialek | Isi |
|---|---|---|
| `GET /api/v1/models` | 9Router/[OI] | handler yang sama dengan `/v1/models` (alias murni) |
| `GET /api/tags` | Ollama `ListResponse` | `name`, `model`, `modified_at`, `size`, `digest`, `details`, `capabilities` |
| `GET,POST /api/show` | Ollama `ShowResponse` | body `{"model":"..."}` atau `?model=` |
| `GET /v1/props` | llama.cpp `/props` | `model_alias`, `n_ctx`, `modalities`, `build_info` |

Keputusan yang diambil:

- Nama model selalu ber-prefix provider (`ak/...`), karena itulah bentuk yang
  diterima router; daftar dan request memakai id yang sama. Suffix Ollama
  `:latest` ditoleransi saat lookup, karena klien Ollama sering menambahkannya.
- `details.format` = `api`, bukan `gguf`: modelnya remote, tidak ada file bobot.
  `parameter_size`/`quantization_level` dibiarkan kosong daripada ditebak dari
  nama model. `details.parent_model` diisi id provider sebagai provenance.
- `digest` = sha256 dari `provider/model`, bukan hash bobot (yang tidak bisa
  dibaca). Klien memakai field ini sebagai kunci cache/dedupe, jadi nilai kosong
  akan membuat semua model tampak sama; nilai ini stabil antar-restart dan
  berubah hanya kalau target routing berubah.
- `capabilities` = `completion` + apa pun yang dideklarasikan
  `models[].capabilities` di config. Proxy tidak bisa mengintrospeksi model
  remote, jadi menebak akan menyesatkan.
- Error endpoint Ollama memakai bentuk datar `{"error":"..."}`, bukan envelope
  [OI]; client Ollama akan gagal membaca yang kedua.
- Registry kosong: `/api/tags` menjawab `{"models":[]}` dan `/api/show` menjawab
  404 `model "x" not found`, persis Ollama yang tidak punya model itu.
  `/v1/props` tetap 200 dengan deskripsi kosong: probe klien tidak boleh
  menyimpulkan prosesnya mati hanya karena belum ada koneksi.
- `/v1/props` melaporkan default, bukan state slot hidup: `n_predict` -1 (tanpa
  batas dari server), `n_ctx` dari config (0 = tidak diketahui, provider yang
  menentukan), `is_sleeping` selalu false karena tidak ada KV cache lokal.
- Semua handler read-only dan tidak pernah menghubungi provider, jadi tidak ada
  biaya token atau risiko membocorkan credential.
- Tidak ada translasi generasi (`/api/chat`, `/completion`): itu bagian fase 4.

### 3b. Alias, combo, dan inferensi prefix — ✅ SELESAI

Tiga cara memberi nama pada target routing, semuanya dideklarasikan di
`config.yaml` dan dicantumkan di `/v1/models`, `/api/tags`, dan `/api/show`:

```yaml
aliases:
  fast: ds/deepseek-chat
combos:
  - name: coding
    strategy: priority
    members: [cc/, oa/gpt-4.1]
```

- Bentuk target yang diterima: `provider/model`, `provider/` (model pertama yang
  diiklankan), atau alias lain. Model telanjang **ditolak** saat validasi: ia
  akan diam-diam bergantung pada `default_provider`, dan kesalahan seperti itu
  baru terasa ketika default berubah.
- Alias dan combo disimpan apa adanya lalu diresolusi per request, sehingga
  "provider belum punya credential" tetap bisa dibedakan dari "model tidak
  dikenal". Combo melewati member yang providernya tidak punya credential.
- Urutan lookup: `provider/model` → alias → combo → inferensi prefix → default.
  Nama yang mengandung `/` tidak pernah lewat inferensi, karena bentuk itu juga
  dipakai id gaya OpenRouter (`deepseek-ai/DeepSeek-V3`).
- Inferensi sengaja dibatasi ke prefix yang tidak ambigu: `gpt-*`/`o1-*`/`o3-*`/
  `o4-*` → openai, `claude-*` → anthropic, `gemini-*` → gemini, `deepseek-*` →
  deepseek. Prefix yang dipakai beberapa provider (llama, qwen, mistral) tidak
  ditebak, karena menebak akan mengirim model lokal ke cloud.
- Nama alias/combo tidak boleh bertabrakan dengan id provider, alias provider,
  atau satu sama lain. Config memeriksa yang dideklarasikan; `registry.Validate`
  mengulangnya untuk alias katalog yang tidak disebut config (`cc`), sekaligus
  mendeteksi siklus dan target yang tidak dikenal.
- Dua strategi berjalan: `priority` memakai member pertama yang usable,
  `round-robin` merotasi titik mulai per request (kursor hidup di snapshot, jadi
  reload mengulang dari member nol). Combo fase 3 hanya **memilih** member;
  retry ke member berikutnya setelah upstream menolak baru ada di fase 5.
- Anggota combo boleh menunjuk alias, tetapi tidak boleh menunjuk combo lain
  (nesting ditolak) supaya urutan percobaan tetap datar dan mudah dibaca di log.

### 3c. Belum masuk fase 3 (saat fase itu selesai)

- **Retry/fallback setelah upstream menolak.** Fase 3 hanya memilih target di
  depan request; percobaan berikutnya, cooldown, dan rotasi multi-akun fase 5.
- **Alias yang menunjuk combo.** Target alias hanya `provider/model` atau alias
  lain; combo tidak bisa dirujuk dari `aliases:`.
- **Strategi `capability`.** Baru `priority` dan `round-robin`; pemilihan
  berdasarkan tools/vision/context belum ada.
- **Penyimpanan & CRUD shortcut.** Alias/combo hanya dari YAML, belum bisa
  diubah lewat API atau disimpan di SQLite (fase 9).
- **Model allow-list per provider.** Daftar model tetap hint untuk discovery;
  id apa pun tetap diteruskan.

---

## 4. Translasi format — bagian tersulit (minggu 2) — ✅ SELESAI

> Status: `internal/translate` memakai OpenAI Chat Completions sebagai pivot.
> Yang sudah jalan: `openai ↔ anthropic` (request, response, streaming, tool call),
> ingress `POST /v1/messages` untuk klien Anthropic, `openai ↔ gemini` (native
> `generateContent`/`streamGenerateContent`), dan `openai ↔ openai-responses`.
> Provider yang formatnya sama dengan klien tetap diteruskan byte-per-byte.
> Yang belum: `/api/chat` & `/completion` gaya Ollama native, audio/file part, dan
> signature blok thinking Anthropic (reasoning tidak dikirim balik ke provider
> lain).

Ini yang membuat "satu endpoint untuk semua CLI". Pakai **OpenAI Chat sebagai pivot**,
persis seperti 9Router (`source -> openai -> target`), supaya jumlah translator
linear bukan kuadratik.

IR minimal:

```go
type Message struct {
    Role       string        // system|user|assistant|tool
    Blocks     []Block       // text, image, tool_use, tool_result, thinking
    ToolCalls  []ToolCall
    ToolCallID string
}
type Request struct {
    Model, System string
    Messages []Message
    Tools []Tool
    ToolChoice any
    MaxTokens int
    Temperature *float64
    Stream bool
    Stop []string
}
```

Translator yang wajib, berurutan prioritas:
1. `openai` ↔ `anthropic` (Claude Code, Cline, Continue) — paling penting.
2. `anthropic` → `openai` untuk ingress `/v1/messages`.
3. `openai` ↔ `gemini`.
4. `openai` ↔ `openai-responses` (Codex CLI).

Detail yang sering bikin bug, harus masuk test:
- `tool_calls` id ↔ `tool_use.id` + `tool_result.tool_use_id`; tool result paralel.
- `stop_reason`: `tool_use`/`end_turn`/`max_tokens` ↔ `tool_calls`/`stop`/`length`.
- `usage`: `prompt_tokens`/`completion_tokens` ↔ `input_tokens`/`output_tokens`
  (+ `cache_read_input_tokens`).
- Thinking/reasoning: `reasoning_content`, `thinking` blocks, `reasoning_effort`,
  signature Anthropic (jangan dikirim ke provider lain).
- Multi-part content: text + image (base64 & URL), audio dibuang bila tak didukung.
- System prompt: array Anthropic vs string OpenAI.
- `max_tokens` wajib di Anthropic, default-kan kalau klien tidak kirim.

Streaming: translator harus **stateful per request**:
- Anthropic `message_start` → `content_block_start` → `content_block_delta` →
  `message_delta`/`message_stop` dipetakan ke potongan `chat.completion.chunk`.
- Kirim `[DONE]` hanya untuk format OpenAI.
- Tangani tool-call delta (argumen JSON terpotong) dan flush buffer bila stream putus.

Strategi uji: golden files (request/response JSON in → out) + test round-trip
`openai → claude → openai` idempoten pada fixture nyata. Ini yang membuat refactor aman.

Definisi selesai: Claude Code CLI dan satu tool OpenAI-compatible bisa memakai
provider yang formatnya berbeda, streaming dan tool calling utuh.

### 4a. Keputusan implementasi — ✅ SELESAI

- Pintu masuk menentukan dialek respons dan envelope error: `/v1/chat/completions`
  memakai bentuk OpenAI, `/v1/messages` memakai `{"type":"error","error":{...}}`.
  Middleware auth ikut membedakan keduanya supaya klien Anthropic tidak menerima
  envelope yang tidak bisa diparse.
- Jika dialek klien dan provider sama, body diteruskan apa adanya (hanya `model`
  yang ditulis ulang bila routing mengubahnya). Translasi hanya terjadi ketika
  dialeknya berbeda, sehingga field vendor yang tidak dikenal tidak hilang.
- Streaming diterjemahkan per event lewat `io.Pipe`: konverter menulis SSE ke
  pipe, lalu `upstream.Relay` tetap memegang idle timeout dan write deadline yang
  sama dengan passthrough. Rantai dua tahap dipakai untuk ingress Anthropic ke
  provider non-OpenAI (`provider → OpenAI → Anthropic`).
- `max_tokens` diisi `DefaultMaxTokens` (4096) saat klien tidak mengirimnya,
  karena Anthropic mewajibkannya.
- Tool call dipetakan dua arah: `tool_calls[].id` ↔ `tool_use.id`,
  `role:"tool"` ↔ `tool_result`, dan argumen JSON yang terpotong saat streaming
  di-flush per delta. `tool_choice` `none` diterjemahkan dengan menghapus daftar
  tool, karena Anthropic tidak punya mode itu.
- `reasoning_content`/blok `thinking` dibaca dari provider dan dikembalikan ke
  klien OpenAI sebagai `reasoning_content`; signature Anthropic tidak pernah
  dikirim ke provider lain.
- Gambar `data:` dan URL dipetakan ke `image_url` (OpenAI), blok `image`
  (Anthropic), `inlineData`/`fileData` (Gemini). Audio dan file dibuang.
- Endpoint native per format: `chat/completions`, `messages`,
  `models/{model}:generateContent`, dan `responses`. Gemini native memakai
  `base_url` yang berakhir di `/v1beta`.

### 4b. Belum masuk fase 4 (sengaja ditunda)

Dicatat supaya tidak dianggap bug dan tidak hilang dari roadmap:

- **Ingress Gemini dan OpenAI Responses.** Yang ada baru egress: klien berbicara
  OpenAI Chat atau Anthropic, lalu diterjemahkan ke Gemini/Responses. Endpoint
  `POST /v1/responses` dan `:generateContent` sebagai pintu masuk belum ada.
- **Ollama native.** `POST /api/chat` dan `/api/completion` belum diterjemahkan,
  dan provider dengan `format: ollama` masih dijawab 501.
- **Audio dan file.** `input_audio`, `input_file`, serta part dokumen dibuang saat
  translasi; hanya teks dan gambar yang bertahan.
- **Signature thinking Anthropic.** Blok `thinking` dibaca sebagai
  `reasoning_content`, tetapi `signature`-nya tidak disimpan, jadi tidak bisa
  dikirim balik ke Anthropic pada turn berikutnya. Untuk provider lain, reasoning
  tetap tidak dikirim.
- **Reasoning ke klien Anthropic.** Ingress `/v1/messages` belum memancarkan
  `thinking_delta`; `reasoning_content` dari upstream OpenAI/Gemini hanya memicu
  blok teks, isinya tidak diteruskan.
- **Prompt caching.** `cache_control` Anthropic tidak dibuat otomatis dan tidak
  diterjemahkan; `cache_read_input_tokens` hanya dilaporkan kalau provider
  mengirimnya.
- **Parameter generasi lanjutan.** `response_format`/JSON schema, `n`, `logprobs`,
  `presence_penalty`, `frequency_penalty`, `seed`, dan `user` belum dipetakan ke
  Gemini (`responseMimeType`/`responseSchema`) atau Anthropic.
- **Fitur server-side provider.** Tool bawaan (web search, computer use, code
  interpreter), citations, dan multimodal output dibuang karena tidak punya
  padanan lintas provider.
- **`/v1/messages/count_tokens`.** Belum ada; Claude Code memakai endpoint ini
  untuk estimasi, dan saat ini akan mendapat 404.
- **Envelope error lintas dialek untuk egress chat.** Error provider tetap
  direlai verbatim; provider Anthropic yang error bisa mengirim envelope
  Anthropic ke klien `/v1/chat/completions`. Ingress `/v1/messages` sudah
  menerjemahkan envelope-nya.

---

## 5. Router: combo, fallback, multi-akun (minggu 3) — ✅ SELESAI

> Status: request sekarang berjalan lewat daftar percobaan. Combo mencoba member
> berikutnya setelah gagal, provider dengan beberapa connection merotasi
> kredensialnya, kredensial yang gagal masuk cooldown dengan backoff, dan stream
> di-peek dulu sebelum dianggap commit. Semua state ada di memori proses.

```go
type Combo struct {
    Name    string
    Members []string // "anthropic/claude-sonnet", "deepseek/deepseek-chat", "groq/..."
    Strategy string  // priority | round-robin | capability
}
```

- Loop anggota combo; request dianggap commit setelah byte pertama terkirim.
  Untuk stream, **peek event pertama**: kalau upstream balas 2xx tapi stream kosong /
  langsung error, masih boleh pindah anggota. Setelah ada konten terkirim, tidak bisa.
- Klasifikasi error (contoh aturan, buat tabel bukan if-else berserak):
  - 401/403 → akun invalid (cooldown panjang), fallback.
  - 402/quota/insufficient → cooldown sampai reset bila diketahui, fallback.
  - 429 → exponential backoff 1s→2s→4s… maks 4 menit per akun, fallback.
  - 400/413/422 (context overflow, param salah) → **request-scoped**: jangan cooldown
    akun, tapi coba anggota berikutnya (pelajaran penting dari `accountFallback.js`).
  - 5xx/timeout/koneksi putus → cooldown pendek, fallback.
- Round-robin multi-akun per provider + sticky per session (hash `session_id`) supaya
  prompt cache upstream tetap kena.
- Circuit breaker per koneksi (opsional, fase 8).
- Balas 502 dengan ringkasan semua percobaan (`lastError`) supaya debugging gampang,
  formatnya jangan menutupi error asli.

Definisi selesai: matikan satu API key → request otomatis pindah, tanpa 5xx ke klien.

### 5a. Keputusan implementasi — ✅ SELESAI

- `registry.Plan` mengembalikan daftar percobaan berurutan, bukan satu target.
  Combo berisi semua member yang providernya punya credential; `Lookup` menjadi
  pembungkus yang mengambil percobaan pertama. Combo `round-robin` merotasi
  urutan itu per request.
- `internal/balancer` memilih urutan kredensial untuk satu provider:
  - kredensial yang sedang cooldown dilewati;
  - jika semua sedang cooldown dan masih ada member combo lain, provider itu
    dilewati sama sekali; kalau ini percobaan terakhir, semuanya tetap dicoba
    sebagai jalan terakhir;
  - `X-Lgllm-Session`, `X-Session-Id`, `metadata.user_id`, atau
    `prompt_cache_key` membuat pilihan sticky per sesi supaya prompt cache
    upstream tetap kena; tanpa session, kursor berputar per request.
- Klasifikasi kegagalan (`classifyStatus`):

  | Status | Cooldown | Fallback |
  |---|---|---|
  | 400/404/413/422 | tidak ada (request-scoped) | ya |
  | 401/403 | 30 mnt, dobel s/d 2 jam | ya |
  | 402/quota | 15 mnt, dobel s/d 24 jam | ya |
  | 429 | `Retry-After`, atau 1 dtk dobel s/d 4 mnt | ya |
  | 408 | 15 dtk s/d 5 mnt | ya |
  | 5xx | 30 dtk s/d 10 mnt | ya |
  | timeout/koneksi putus | 30 dtk s/d 10 mnt | ya |

  Backoff dihitung per connection id dan di-reset saat sukses. Cooldown hanya
  memengaruhi request berikutnya: request yang sedang berjalan langsung pindah
  ke kredensial/member berikutnya tanpa menunggu.
- Stream di-peek hanya bila masih ada fallback (member atau connection lain).
  Kalau upstream membalas 200 lalu mengirim event error atau langsung EOF,
  request belum dianggap commit dan member berikutnya dicoba. Kalau tidak ada
  fallback, header langsung dikirim seperti sebelumnya supaya klien bisa
  menutup body dan membatalkan request upstream.
- Semua percobaan gagal: satu percobaan direlai verbatim (status, header
  rate-limit, body asli) seperti sebelum fase 5; lebih dari satu menghasilkan
  502 dengan `attempts[]` yang menyebut provider, model, connection, status, dan
  error masing-masing. Ingress `/v1/messages` memakai envelope Anthropic.

### 5b. Belum masuk fase 5 (saat fase itu selesai)

- **Circuit breaker per koneksi.** Cooldown sederhana sudah ada; breaker dengan
  ambang error dan half-open probing masuk fase 8 (opsional).
- **Strategi `capability`.** Pemilihan member masih `priority`/`round-robin`,
  belum berdasarkan tools/vision/context.
- **Cooldown yang bertahan setelah restart.** State hanya di memori proses;
  restart (atau instance lain) memulai tanpa riwayat kegagalan.
- **Pemilihan berbasis latensi/biaya.** Belum ada pengukuran per kredensial
  sebagai dasar urutan; sementara hanya rotasi + kesehatan.
- **Batas percobaan yang bisa dikonfigurasi.** Semua member/koneksi dicoba;
  belum ada `max_attempts` atau timeout total per request.

---

## 6. Token saver lite (minggu 3–4) — ✅ SELESAI

> Status: `internal/tokensaver` memampatkan hasil tool sebelum dikirim ke
> provider. Yang sudah jalan: filter diff/grep/log/JSON, guard ukuran + error +
> idempoten + head/tail, bypass per request, opt-out per combo, prompt opsional,
> serta angka `saved` di log dan response header. Agregasi "saved X%" untuk UI
> menunggu fase 7.

Port inti RTK saja, tanpa semua filter eksotis:
- Hook sebelum translasi, hanya menyentuh blok `tool_result` / `role:"tool"`.
- Guard: hanya bila panjang > N byte (mis. 2 KB), lewati `is_error`, idempoten,
  dan selalu pertahankan awal + akhir output (head/tail) agar model tetap paham.
- Filter awal: `git diff`, `git status`, `grep`/`rg`, `ls`/`find`, `tree`,
  JSON besar (minify + buang field verbose), log panjang (dedup baris).
- Hitung `bytesBefore/bytesAfter`, catat ke usage log → tampilkan "saved X%" di UI.
- Bypass per request: header `X-Lgllm-Token-Saver: off`.
- Opsional murah: injeksi prompt "caveman"/"ponytail" (cukup system message
  tambahan, bisa dimatikan per combo).

Definisi selesai: pada sesi agent nyata, input token turun ≥ 15% dengan output
tetap benar (uji regresi manual pada 5 task).

### 6a. Keputusan implementasi — ✅ SELESAI

- Hook berjalan pada pivot, sebelum encoding ke format provider. Untuk request
  yang dialeknya sama dengan provider, hanya `messages`/`system` yang diganti
  saat memang ada perubahan; field top-level yang tidak dikenal (mis.
  `response_format`, `parallel_tool_calls`) tetap diteruskan. Tanpa perubahan,
  body asli diteruskan byte-per-byte.
- Guard: hanya `role:"tool"`, hanya bila panjang ≥ `min_bytes`, tidak pernah
  untuk tool result error (`is_error` Anthropic), dan hasil yang sudah mengandung
  marker `[lgllm:token-saver]` dilewati sehingga request ulang tidak memampatkan
  dua kali.
- Filter dipilih dari bentuk konten: unified diff (buang metadata + context,
  simpan file/hunk header dan baris perubahan), JSON valid (minify + buang field
  verbose seperti `node_id`, `avatar_url`, `created_at`), grep/status/log
  (gabungkan baris berulang dengan penanda `repeated N times`), dan fallback
  head/tail.
- Semua hasil selalu mempertahankan awal dan akhir: 2/3 head, 1/3 tail dari
  target `max_bytes` (default 8 KB), dengan penanda jumlah byte yang dibuang.
- Bypass per request: `X-Lgllm-Token-Saver: off` (juga `false`/`0`/`no`).
  Opt-out per combo: `combos[].token_saver: false`.
- `tokensaver.prompt` opsional ditambahkan ke system prompt sekali saja; ini
  pengganti murah untuk gaya "caveman/ponytail", bukan filter terpisah.
- Statistik (`before`, `after`, `saved`, filter yang aktif) dicatat di log
  request dan header respons `X-Lgllm-Token-Saver: on; saved=N/M`, sehingga
  klien/UI bisa menampilkannya tanpa membuka log. Agregasi per sesi/biaya
  menyusul di fase 7.

### 6b. Belum masuk fase 6 (saat fase itu selesai)

- **Filter yang bisa dikonfigurasi.** Set filter tetap; belum ada pilihan
  menyalakan/mematikan per jenis konten.
- **Metrik per filter.** Baru total byte yang dihemat per request; belum ada
  hitungan per filter atau per sesi (agregasi fase 7).
- **Dedupe lintas tool result.** Setiap hasil dikompres sendiri; potongan yang
  sama di beberapa tool result belum digabung.
- **Regresi otomatis 5 task agent.** Definisi "output tetap benar" masih diuji
  manual; belum ada harness regresi.

---

## 7. Observability & operasional (minggu 4) — ✅ SELESAI

> Status: `GET /metrics` menyajikan counter/histogram Prometheus tanpa dependensi
> eksternal, `GET /usage` membaca ring buffer request terakhir, dan pprof punya
> listener loopback terpisah. Token/biaya dihitung dari usage provider; stream
> masih 0 token karena belum di-parse per chunk.

- `GET /healthz` (liveness), `GET /readyz` (cek store), `GET /metrics` (Prometheus:
  request count/latency/token per provider), pprof di port terpisah.
- Request log: id, waktu, provider, model, status, durasi, token in/out, biaya estimasi,
  bytes saved, error. Simpan ring buffer di memori + opsional SQLite.
- Tabel harga per model untuk estimasi biaya (jangan over-engineer, update manual).
- Log terstruktur slog, level via config; log body hanya bila `debug` dan dengan redaksi
  header otorisasi.

### 7a. Keputusan implementasi — ✅ SELESAI

- **Metrik tanpa dependensi.** `internal/metrics` menulis format teks Prometheus
  sendiri (counter, gauge, histogram) supaya binary tetap hanya bergantung pada
  `yaml.v3`. Metrik: request per ingress/provider/model/status, histogram durasi,
  token prompt/completion, byte yang dihemat token saver, dan kegagalan attempt
  upstream (termasuk yang berhasil di-retry).
- **Usage ring buffer.** `internal/usagelog` menyimpan N request terakhir
  (`metrics.usage_log_size`, default 500). Entry berisi request id, ingress,
  provider/model, status, stream, durasi, token, byte hemat, biaya, dan error.
  `GET /usage?limit=N` mengembalikan yang terbaru lebih dulu. Ukuran buffer
  diterapkan saat startup; mengubahnya butuh restart.
- **Estimasi biaya.** `internal/pricing` berisi tabel manual USD per 1 juta token
  dengan longest-prefix match (mis. `gpt-4o-mini-*` tidak memakai tarif `gpt-4o`).
  Model yang tidak dikenal dilaporkan `cost_known: false`, bukan biaya 0 yang
  menyesatkan.
- **pprof terpisah.** `metrics.pprof_listen` menyalakan listener sendiri; validator
  menolak alamat non-loopback karena pprof tidak punya autentikasi. Perubahan
  alamat butuh restart (dicatat di log reload).
- **Log body hanya saat debug.** Body dipotong 2 KB dan diredaksi (`sk-*`,
  `Bearer ...`, field `api_key`/`authorization`/`token`/`secret`/`password`)
  sebelum masuk log. Level tetap dari `log.level`.
- **Metrik tetap jalan saat `/metrics` dimatikan.** `metrics.enabled: false`
  membuat kedua endpoint 404, tetapi counter tetap dihitung supaya menyalakannya
  kembali tidak memerlukan restart.
- Token diambil dari body respons non-stream lewat decoder format yang sama
  dengan translasi. Untuk stream, angka token masih 0 karena belum ada parser
  usage per event.

### 7b. Belum masuk fase 7 (saat fase itu selesai)

- **Token untuk stream.** Usage dari event terakhir (`message_delta`,
  `response.completed`, Gemini `usageMetadata`) belum diekstrak; stream tercatat
  0 token.
- **Penyimpanan persisten.** Ring buffer hilang saat restart; SQLite/usage log
  durable belum ada (fase 9).
- **Harga yang bisa dikonfigurasi.** Tabel harga masih di kode; belum ada
  override lewat YAML atau API.
- **Agregasi & dashboard.** Belum ada rollup per sesi/hari, persentase hemat
  token, atau grafik; baru endpoint mentah.
- **Metrik tambahan.** Belum ada lag queue, ukuran config reload, error parsing,
  atau metrik per connection (hanya per provider).
- **Retensi & sampling.** Semua request dicatat di ring; belum ada filter
  sampling atau batas ukuran body respons untuk usage.
- **Trace/span.** Belum ada OpenTelemetry; hanya request id di log dan usage.

---

## 8. Kredensial lanjutan (opsional, minggu 5) — ✅ SELESAI (tanpa OAuth/web-login)

> Status: secret bisa disimpan sebagai ciphertext AES-GCM, dan `auth.rate_limit`
> membatasi request per API key plus kuota token harian. OAuth PKCE, refresh
> token, dan provider free-tier berbasis web login tetap **tidak** dikerjakan:
> itu bertentangan dengan batas lingkup v1 (tanpa browser automation) dan paling
> rawan ToS.

- Enkripsi secret at-rest (AES-GCM, key dari env/`--master-key`), bukan plaintext YAML.
- OAuth PKCE untuk provider yang memang mengizinkannya + refresh token background
  (single-flight, jitter, `refresh_lead_ms`).
- Rate limit per API key + kuota harian (mirip `keyAccess`).
- Kalau tetap mau provider "free tier" berbasis web login: isolasi di build tag
  terpisah dan dokumentasikan risikonya. Jangan jadi dependency core.

### 8a. Keputusan implementasi — ✅ SELESAI

- **Enkripsi at-rest.** `internal/secret` memakai AES-256-GCM dengan nonce acak;
  nilai ditulis `enc:v1:<base64(nonce||ciphertext)>`. Master key diambil dari
  `LGLLM_MASTER_KEY` atau `--master-key`; 32 byte base64/hex dipakai langsung,
  string ≥16 karakter di-hash SHA-256 agar passphrase bisa dipakai. Key tidak
  pernah masuk file config.
- **Dekripsi saat load.** `config.DecryptSecrets` mengganti `connections[].api_key`,
  `auth.keys[]`, dan `providers[].headers` yang berprefix `enc:v1:` **di memori**
  saja; file tidak pernah ditulis ulang. Jika ada ciphertext tetapi tidak ada
  master key, startup ditolak dengan lokasi field yang jelas. Reload memakai
  master key yang sama karena ikut di `Overrides`.
- **Alat bantu.** `printf '%s' "$KEY" | lgllm -encrypt` mencetak ciphertext,
  sehingga plaintext tidak masuk shell history atau process list.
- **Rate limit per key.** `auth.rate_limit.requests_per_minute` + `burst` memakai
  token bucket; `tokens_per_day` adalah kuota token (prompt+completion) yang
  diisi dari usage respons. Bucket per identitas: hash 8 byte dari API key yang
  dikirim, atau `local` untuk klien loopback yang dipercaya tanpa key.
- **Penolakan.** 429 + `Retry-After`; ingress `/v1/messages` memakai envelope
  Anthropic. `/metrics` dan `/usage` dikecualikan supaya monitoring tidak ikut
  mati saat kuota habis. Penolakan dihitung di
  `lgllm_rate_limited_total{reason}`.
- **Batas dan reset.** State di memori; kuota harian direset pada 00:00 UTC.
  Perubahan limit berlaku langsung pada reload karena limiter membaca config per
  request.
- **OAuth tidak diimplementasikan.** Tidak ada PKCE, refresh token, atau
  build-tag web-login; provider yang butuh itu tetap di luar cakupan.

### 8b. Belum masuk fase 8 (saat fase itu selesai)

- **OAuth PKCE & refresh token.** Tidak ada alur otorisasi, single-flight refresh,
  jitter, atau `refresh_lead_ms`.
- **Provider free-tier berbasis web login.** Tetap out of scope; tidak ada build
  tag terpisah karena tidak ada kode yang perlu diisolasi.
- **Manajemen key lewat API/UI.** Key dan limit hanya dari YAML; belum ada CRUD
  atau rotasi dari admin API (fase 9).
- **Kuota per key yang berbeda-beda.** Semua key berbagi satu kebijakan global;
  belum ada tabel limit per key.
- **Persistensi & distribusi limit.** State hilang saat restart dan tidak dibagi
  antar-instance (butuh Redis/DB bila multi-node).
- **Rotasi master key.** Belum ada re-enkripsi massal atau dukungan dua key
  sekaligus untuk migrasi.
- **Audit & alarm.** Belum ada log audit khusus perubahan kredensial atau
  notifikasi saat kuota hampir habis.

---

## 9. Admin REST API (minggu 5) — ✅ SELESAI

> Status: provider, connection, combo, dan client key bisa dikelola lewat HTTP;
> usage/logs diekspos (termasuk SSE), dan `POST /api/test-connection` menguji satu
> kredensial. Resource dari `config.yaml` bersifat read-only; resource buatan API
> disimpan di overlay `admin.yaml`. Endpoint berjalan dengan token admin terpisah.

UI nanti hanya konsumen API ini:
```
GET/POST/PUT/DELETE /api/providers
GET/POST/PUT/DELETE /api/connections
GET/POST/PUT/DELETE /api/combos
GET/POST/DELETE     /api/keys
GET                 /api/usage?from=&to=&group=provider
GET                 /api/logs?limit=
GET                 /api/logs/stream        (SSE live)
POST                /api/test-connection
```
Autentikasi admin terpisah dari key inference (session cookie/token), default hanya
bind localhost.

### 9a. Keputusan implementasi — ✅ SELESAI

- **Overlay, bukan menulis ulang config.** `config.yaml` bisa berisi `${ENV}` dan
  komentar yang akan hancur bila di-round-trip. Resource buatan API disimpan di
  `admin.yaml` (default di samping config, bisa diatur `admin.state_file`), lalu
  digabung saat load. File ditulis atomik dengan mode 0600; secret dienkripsi bila
  master key tersedia.
- **Read-only untuk resource config.** Entri dari `config.yaml` tampil dengan
  `"managed":"config"` dan PUT/DELETE-nya 409. Ini membuat arti "delete" tidak
  ambigu dan mencegah API menimpa file utama.
- **Validasi transaksional.** Setiap mutasi: clone state → tulis file → load
  ulang config + `registry.Validate` → publish ke server. Jika gagal, file
  dikembalikan ke isi sebelumnya dan state memory tidak berubah.
- **Auth admin terpisah.** `admin.enabled` + `admin.token` (atau loopback-only bila
  token kosong). Middleware inference melewati path admin, dan token inference
  tidak diterima sebagai token admin. Bind publik tanpa token ditolak config.
- **Redaksi.** GET connection mengembalikan `"api_key":"***"`; mengirim `***`
  kembali mempertahankan nilai lama. Header provider yang namanya mengandung
  key/token/secret/auth/cookie juga diredaksi; `***` pada PUT mempertahankan
  nilai lama.
- **Endpoint.** CRUD providers/connections/combos/keys, agregasi usage
  (`group=provider|model|day`, filter `from`/`to`), logs mentah, SSE
  `/api/logs/stream` (subscriber non-blocking; event lambat dibuang, bukan
  memblokir request), dan `test-connection` yang mengirim satu request minimal
  lalu melaporkan status/latensi.
- **Store tetap YAML.** Belum ada SQLite; overlay adalah satu file kecil. Mutasi
  ditulis penuh setiap kali (aman untuk ukuran v1) dan tahan restart.

### 9b. Belum masuk fase 9 (saat fase itu selesai)

- **SQLite / query store.** Overlay YAML ditulis ulang penuh per mutasi; belum ada
  index, transaksi multi-tabel, atau riwayat perubahan.
- **Session cookie / UI login.** Auth admin masih token statis; belum ada sesi,
  CSRF, atau halaman login (fase 10).
- **Edit resource config lewat API.** Provider/connection/combo dari `config.yaml`
  tetap read-only; memindahkannya ke overlay harus manual.
- **Rotasi kredensial otomatis.** Tidak ada endpoint untuk regenerate key atau
  mengenkripsi ulang semua secret.
- **Paginasi & filter lanjutan.** `/api/logs` hanya `limit`; usage hanya rentang
  waktu + group, tanpa pagination atau export.
- **Audit trail.** Perubahan admin belum dicatat ke log audit khusus selain log
  "applied new config".
- **Multi-tenant.** Satu token admin, satu overlay; belum ada role atau isolasi
  per tenant.

---

## 10. UI opsional (minggu 6, hanya setelah core stabil) — ✅ SELESAI (opsi A, tanpa build Node)

> Status: dashboard web minimal di-`go:embed`, ditulis dengan HTML/CSS/JS biasa
> tanpa langkah build. Tab Providers/Connections/Combos/Keys/Usage/Logs
> (SSE)/Settings memakai admin API yang sudah ada. TUI (opsi B) tidak dikerjakan.

Dua opsi, pilih salah satu:

A. **Web UI minimal** — SPA (Svelte/Vite, ~100 KB) di-`go:embed`, halaman:
   Providers, Connections, Combos, Keys, Usage, Logs (SSE), Settings.
   Tanpa SSR, tanpa Node saat runtime. Ini yang menyerupai dashboard 9Router.

B. **TUI** (bubbletea) — untuk yang benar-benar ingin "lightweight": kelola koneksi,
   lihat log live, lihat usage dari terminal. Binary tetap satu, tanpa aset web.

Rekomendasi: B dulu kalau targetnya personal/VPS, A kalau mau dipakai orang lain.

### 10a. Keputusan implementasi — ✅ SELESAI

- **Opsi A tanpa toolchain Node.** Alih-alih Svelte/Vite, UI ditulis sebagai tiga
  file (`index.html`, `style.css`, `app.js`) dan di-`embed` ke binary. Tidak ada
  langkah build, `node_modules`, atau aset runtime; `go build` tetap satu-satunya
  yang dibutuhkan.
- **Konsumsi admin API yang sama.** Token admin dimasukkan di halaman dan disimpan
  di `localStorage` browser. Resource config tampil read-only; resource admin bisa
  diedit/dihapus. Key selalu tampil `***`.
- **Fitur:** daftar + tambah/edit/hapus provider, connection, combo; tambah/hapus
  client key; usage dengan group provider/model/day; log live lewat SSE
  (`fetch` streaming, karena `EventSource` tidak bisa mengirim header token);
  tombol "Test" per provider; halaman Settings berisi versi/readiness.
- **Keamanan.** Halaman statis dilayani tanpa key inference, tetapi setiap panggilan
  API tetap butuh token admin. CSP ketat (`default-src 'self'`), `nosniff`,
  `Referrer-Policy: no-referrer`. UI hanya aktif saat `admin.enabled`.
- **Tanpa framework.** Semua rendering memakai `textContent`/DOM API, bukan
  `innerHTML` dengan data server, sehingga nama provider/model tidak bisa menjadi
  HTML.

### 10b. Belum masuk fase 10 (saat fase itu selesai)

- **TUI (opsi B).** Tidak dibuat; belum ada bubbletea atau klien terminal.
- **Build pipeline SPA.** Tidak ada Svelte/Vite/bundler; UI sengaja ditulis tangan
  agar binary tidak bergantung pada Node.
- **Edit settings dari UI.** Settings hanya menampilkan versi/readiness; config,
  limit, token, dan harga tetap diedit di file.
- **Login/sesi UI.** Token disimpan di `localStorage`; belum ada cookie sesi,
  CSRF, atau logout server-side.
- **Pagination/filter tabel.** Daftar memuat semua entri; usage hanya rentang
  waktu + group.
- **Streaming log yang tahan reconnect.** Tidak ada resume dari event terakhir;
  reconnect memulai dari event baru.
- **i18n dan tema.** Teks Inggris, tema gelap saja.
- **Aksesibilitas lanjutan.** Belum diuji dengan screen reader atau keyboard-only.

---

## 11. Packaging & benchmark (minggu 6) — ✅ SELESAI

> Status: `.goreleaser.yaml` menargetkan linux/darwin/windows amd64+arm64 plus
> paket deb/rpm/apk, image `scratch` 8.7 MB, unit systemd + post-install, dan
> `scripts/bench.py` yang mengukur ukuran/cold start/RSS/latensi/throughput.
> Hasil dan metodologi ada di `docs/BENCH.md`. Publikasi rilis dan tap Homebrew
> belum dijalankan (tidak ada remote/tap).

- `goreleaser`: linux/darwin/windows amd64+arm64, CGO_ENABLED=0, `-trimpath -ldflags "-s -w"`.
- Docker multi-stage → `gcr.io/distroless/static` atau `scratch`; image < 30 MB.
- Systemd unit + Homebrew tap (opsional).
- Benchmark pembanding vs 9Router: ukuran image, RSS idle & saat streaming,
  cold start, overhead latency p50/p99, request/detik.
- Dokumentasi: README, contoh `config.yaml`, panduan integrasi
  Claude Code / Cline / Continue / OpenCode / Codex.

### 11a. Keputusan implementasi — ✅ SELESAI

- **Rilis lintas platform.** `.goreleaser.yaml` (v2) membangun linux/darwin/windows
  amd64+arm64 dengan `CGO_ENABLED=0`, `-trimpath`, `-s -w`, menulis checksum, dan
  membuat paket deb/rpm/apk berisi unit systemd + contoh config. Snapshot lokal:
  `make snapshot` (`goreleaser release --snapshot --clean`).
- **Image kecil.** Dockerfile multi-stage → `scratch`; CA bundle dan tzdata
  diambil dari stage build (`apk add ca-certificates tzdata`). Image terukur
  **8.7 MB**; container diuji dengan config read-only dan `/healthz` menjawab 200.
- **Operasional.** `deploy/lgllm.service` menjalankan binary sebagai user
  `lgllm`, dengan hardening (`NoNewPrivileges`, `ProtectSystem=strict`,
  `PrivateTmp`, dst.) dan `StateDirectory=lgllm` untuk `admin.state_file`.
  `deploy/package-postinstall.sh` membuat user/direktori dan mencetak langkah
  berikutnya tanpa menyalakan service diam-diam.
- **Benchmark yang bisa diulang.** `scripts/bench.py` (stdlib Python) membuat mock
  upstream, config sementara, menjalankan binary, lalu mengukur ukuran binary,
  cold start, RSS idle/streaming, latensi p50/p95/p99, overhead vs upstream
  langsung, throughput keep-alive, dan rata-rata streaming. `make bench` dan
  `make image-size` adalah pintu masuknya.
- **Hasil (i3-4330, loopback, 2026-11-07):** binary 8.97 MB, image 8.7 MB, cold
  start 5.1 ms, RSS idle 13.9 MB / streaming 20.0 MB, p50 0.39 ms, p99 0.78 ms,
  overhead p50 0.16 ms, 2398 req/s (32 klien). Detail di `docs/BENCH.md`.
- **Dokumentasi integrasi.** README memuat contoh Claude Code (ingress
  `/v1/messages`), Cline/Continue, OpenCode, dan Codex CLI dengan
  `wire_api = "chat"` karena ingress `/v1/responses` belum ada.
- **Perbandingan 9Router.** Tidak diotomasi (butuh instance/kredensial sendiri);
  metodologi membandingkan di mesin yang sama didokumentasikan, bukan klaim angka.

### 11b. Belum masuk fase 11 (saat fase itu selesai)

- **Publikasi rilis.** Belum ada tag/remote, upload GitHub Releases, atau tap
  Homebrew; `brews` masih dikomentari.
- **Tanda tangan & supply chain.** Belum ada cosign/Sigstore, SBOM (Syft),
  SLSA provenance, atau signing paket.
- **Multi-arch image & registry.** Image belum dipublikasikan; manifest
  amd64+arm64, digest pinning, dan pemindaian kerentanan belum dijalankan.
- **Benchmark vs 9Router.** Belum dijalankan otomatis; hanya metodologi.
- **Soak/load test.** Belum ada k6/wrk jangka panjang, uji memori bocor, atau
  uji jaringan lambat/putus.
- **Hardware/OS lain.** Build lintas platform dibuat, tetapi runtime macOS,
  Windows, dan ARM (Raspberry Pi) belum diuji di CI.
- **Auto-update & notifikasi versi.** Tidak ada pemeriksaan rilis terbaru.
- **Homebrew/systemd di distro nyata.** Unit dan post-install belum diuji di
  Debian/Fedora/Alpine bersih.

---

## Urutan eksekusi ringkas

| Fase | Isi | Estimasi | Demo |
|---|---|---|---|
| 1 | Skeleton, config, health | 1 hari | binary jalan |
| 2 | Proxy streaming + auth key | 2 hari | ✅ selesai |
| 3 | Registry + routing model | 3 hari | ✅ selesai (alias, combo, inferensi) |
| 4 | Translasi format + stream state machine | 5–7 hari | ✅ openai ↔ anthropic/gemini/responses |
| 5 | Combo, fallback, multi-akun | 3–4 hari | ✅ failover + cooldown otomatis |
| 6 | Token saver | 3 hari | ✅ hemat byte per request (agregasi di fase 7) |
| 7 | Metrics, usage, log | 2 hari | ✅ /metrics, /usage, pprof |
| 8 | Secret encryption, OAuth (opsional) | 3 hari | ✅ enkripsi + rate limit (OAuth dilewati) |
| 9 | Admin API | 2 hari | ✅ CRUD + usage/logs + test-connection |
| 10 | UI (web/TUI) | 4–6 hari | ✅ web minimal embedded (tanpa Node) |
| 11 | Release & benchmark | 2 hari | ✅ image 8.7 MB + BENCH.md |

Total core (fase 1–7) realistis ~3 minggu kerja fokus. Fase 8–10 bisa berbulan-bulan
kalau dikejar paritas dengan 9Router — jangan.

---

## Risiko utama

1. **Translasi format** adalah 60% effort. Jangan tulis translator ad-hoc; bangun IR +
   golden test sejak awal, kalau tidak akan jadi utang teknis permanen.
2. **Fallback saat streaming** tidak bisa setelah byte pertama. Desain "peek first event"
   dari awal, jangan ditambal belakangan.
3. **Bocor konteks/token**: context cancellation dan idle timeout wajib benar sejak fase 2,
   karena agent CLI sering membatalkan request di tengah jalan.
4. **Godaan menambah provider**. Registry deklaratif membuat tambah provider murah,
   tapi setiap format baru menambah matriks test. Batasi 4 format dulu.
5. **Provider OAuth/web-login** menggoda (gratis) tapi rapuh dan berisiko ToS —
   perlakukan sebagai plugin eksperimental, bukan fitur inti.
