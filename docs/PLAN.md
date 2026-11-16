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

> Fase 19 menutup publikasi rilis, image multi-arch, SBOM/provenance, dan
> signing image. Tap Homebrew, signing paket, dan uji distro bersih masih
> terbuka.

---

## 12. Dashboard performa per model — ✅ SELESAI

> Status: setiap model yang diiklankan menampilkan rata-rata **TTFT**,
> **latency**, dan **TPS** dari **10 attempt terakhir** (dapat diubah lewat
> `metrics.performance_window`). Stream dan non-stream dipisah. Data muncul di
> `GET /api/performance` dan tab **Performance** di UI, diperbarui tiap 5 detik.
> Rencana awal dan definisi metrik ada di
> [`PLAN-PERFORMANCE.md`](PLAN-PERFORMANCE.md).

Ringkasnya: sampel diambil dari instrumentasi `tryConnection` + pembacaan SSE
usage; disimpan di ring buffer in-memory `internal/perf`; kegagalan masuk success
rate tetapi tidak menggeser rata-rata performa.

### 12a. Keputusan implementasi — ✅ SELESAI

- **`internal/perf`.** `Registry` menyimpan ring `Sample` per `provider/model`
  (default 10). `Aggregate` memisahkan `stream` dan `non_stream`; `Snapshot`
  diurutkan agar output stabil. Hanya angka dan waktu yang disimpan — tidak ada
  prompt, respons, atau credential.
- **`perf.SSETap`.** Membungkus body SSE dan membaca baris `data:` tanpa
  mengubah satu byte; ada test yang membandingkan output tap dengan input
  byte-per-byte. Tap mengenali konten pertama dan usage untuk OpenAI, Anthropic,
  Gemini, dan Responses, termasuk event yang terpecah antar-`Read`.
- **Titik ukur.** `tryConnection` membuat tap sebelum stream di-peek/direlay,
  jadi event pertama yang sudah dibaca saat peek tetap terhitung. Sampel dicatat
  setelah respons selesai (bukan saat header); stream yang putus di tengah
  dicatat gagal dengan latency parsial. Transport error, non-2xx, body kosong,
  dan kegagalan decode juga tercatat gagal.
- **Definisi angka.** TTFT = waktu dari request dikirim ke konten pertama
  (`content`, `reasoning_content`, tool call, atau delta ekuivalen). Latency =
  sampai byte terakhir. TPS stream = `completion_tokens / (konten terakhir −
  konten pertama)`; bila hanya ada satu event konten, dipakai latency dan
  ditandai `est.`. Non-stream selalu `tokens/latency` dan ditandai `est.`.
- **Usage provider.** TPS hanya dihitung bila provider mengirim usage. Request
  yang ditranslasi ke OpenAI menambahkan `stream_options.include_usage`; pada
  pass-through OpenAI, klien yang mengatur opsi itu. Tanpa usage, TPS `n/a`
  (tidak dikarang dari jumlah chunk).
- **API.** `GET /api/performance` memakai autentikasi admin yang sama, menerima
  filter `?provider=` (id atau alias), dan mengembalikan **semua model yang
  diiklankan** — termasuk yang belum pernah dipakai (`attempts: 0`, nilai
  `null`). Sampel untuk model yang tidak lagi ada di registry tidak ditampilkan.
- **UI.** Tab **Performance** menampilkan model, jumlah attempt/sukses, TTFT,
  latency, TPS, mode (`stream n · non-stream n`), dan last seen. Urutan default
  TPS menurun; auto-refresh 5 detik berhenti saat tab browser tidak aktif.
- **Konfigurasi.** `metrics.performance_window` (default 10) divalidasi harus > 0.
  Seperti usage log, mengubah ukurannya butuh restart dan `apply` mencatat
  peringatan.
- **Verifikasi.** Unit test ring buffer/agregasi/tap untuk empat format, test
  server untuk API dan pencatatan attempt sukses/gagal (stream dan non-stream),
  serta test UI statis. `make check` dan `go test -race ./...` bersih.

### 12b. Belum masuk fase 12

- **Persistensi.** Sampel hilang saat restart (in-memory, sama seperti usage log);
  belum ada SQLite/riwayat jangka panjang.
- **Gauge Prometheus.** `lgllm_model_ttft_seconds`, `lgllm_model_tps`, dan
  `lgllm_model_requests_window` belum diekspos; angka hanya lewat API/UI.
- **TPS non-stream yang eksak.** Belum ada sinyal first-token untuk respons JSON,
  jadi TPS non-stream tetap estimasi `tokens/latency`.
- **Token stream di `/usage` dan `/metrics`.** Tap sudah membaca usage, tetapi
  pencatatan usage request stream masih 0; integrasi itu menyusul bersama
  penyimpanan.
- **Per-connection.** Agregat masih per `provider/model`, belum per credential;
  satu akun lambat tidak bisa dibedakan dari akun lain di provider yang sama.
- **Grafik dan ambang sampel.** Belum ada grafik time-series atau label
  "low sample"; UI hanya menampilkan jumlah sampel.
- **Alias dan combo.** Baris mengikuti model upstream yang diiklankan; alias dan
  combo tidak mendapat baris agregat sendiri.
- **Sampling overhead.** Tap membaca dan mem-parse setiap event SSE; belum diukur
  dampaknya pada throughput stream yang sangat besar.

> Fase 16 menutup tiga item di atas: gauge Prometheus, grafik/ambang sampel, dan
> breakdown per-connection. Persistensi, token stream, alias/combo, dan
> pengukuran overhead masih terbuka.

### 13. Config CLI (`lgllm config`)

Tujuan: mengatur `config.yaml` tanpa server berjalan dan tanpa editor teks,
secara interaktif maupun non-interaktif.

- **Model edit.** Perubahan dilakukan pada pohon node YAML (`yaml.Node`), bukan
  marshal ulang struct. Komentar, urutan key, placeholder `${ENV}`, dan nilai
  `enc:v1:` yang tidak disentuh tetap utuh.
- **Aman.** Setiap perubahan divalidasi dengan parser yang sama (`config.Parse`
  + `registry.New`) sebelum file diganti secara atomik (tulis temp, `fsync`,
  `rename`, mode file dipertahankan). File tidak pernah ditulis jika hasilnya
  invalid.
- **Non-interaktif.** `init`, `show`, `validate`, `provider list|add|set|remove`,
  `connection list|add|set|remove|test`, `models list|sync`,
  `alias list|set|remove`, dan `combo list|add|set|remove`. Flag bisa sebelum
  atau sesudah posisi id (`combo add cheap --member ...` tetap bekerja).
- **Interaktif.** `lgllm config wizard` (atau `lgllm config` di terminal tanpa
  argumen) menampilkan menu: tambah provider katalog/custom, tambah koneksi,
  sync model, show, alias, remove, validate. Wizard memanggil jalur
  non-interaktif yang sama, jadi validasi dan penulisan tetap satu implementasi.
- **Provider.** `provider add openai` cukup menulis `id: openai`; field lain
  mewarisi katalog. Provider custom wajib `--base-url`. `provider set`
  mendukung `--base-url`, `--format`, `--auth`, `--auth-param`, `--header`,
  `--timeout`, `--model`, dan enable/disable. `show` hanya menampilkan status
  secret (placeholder/ciphertext/`set, N chars`), tidak pernah plaintext.
- **Koneksi.** `connection add --provider oa` menerima alias katalog dan
  menulis id kanonik. Tanpa `--api-key`, provider katalog memakai default
  `${KEY_ENV}` (mis. `${OPENAI_API_KEY}`); provider `auth: none` tidak butuh key.
  `connection test` memanggil `GET /models` dengan kredensial tersebut.
- **Sync model.** `models sync` membaca `GET <base_url>/models` memakai
  koneksi terpilih (`--connection`, default koneksi enabled pertama), mendukung
  auth `bearer`, `x-api-key`, `query`, header provider (mis.
  `anthropic-version`), timeout, dan bentuk respons OpenAI (`data`), Ollama
  (`models`), maupun array. Default **merge**: metadata `name`, `context`,
  `capabilities` model lama dipertahankan dan id baru ditambahkan. `--replace`
  membuang id yang tidak lagi diiklankan. `--dry-run` mencetak hasil tanpa
  menulis. URL di error di-redact (termasuk query auth).
- **Runtime vs file.** Discovery memakai `config.LoadWithOverrides`, jadi
  `admin.yaml` dan secret `enc:v1:` ikut terpakai (`LGLLM_MASTER_KEY` /
  `--master-key`), tetapi penulisan hanya ke file utama. Perubahan yang akan
  menduplikasi id milik `admin.yaml` ditolak dengan pesan agar memakai admin
  API. Perubahan provider yang dikelola admin juga ditolak.
- **Verifikasi.** Test untuk init/validate/show, siklus provider+koneksi,
  aktivasi catalog lewat koneksi saja, placeholder `${ENV}` dan masking,
  sync merge/replace/dry-run serta preservasi metadata, tiga skema auth,
  kegagalan upstream yang tidak mengubah file, `connection test`, alias/combo,
  wizard, dan perintah tidak dikenal. `go vet`, `go test ./...` bersih.

### 13b. Belum masuk fase 13

- **Section selain routing.** Belum ada perintah untuk `server`, `auth`,
  `metrics`, `upstream`, `log`, atau `admin`; file masih harus diedit manual.
  Wizard hanya menyentuh providers, connections, models, aliases, combos.
- **TUI penuh.** Wizard berbasis baris, bukan layar TUI dengan navigasi panah.
- **Output JSON.** Belum ada `--json` untuk scripting; saat ini output teks.
- **Diff dan undo.** Hanya `models sync --dry-run` yang punya pratinjau; belum
  ada diff untuk semua perintah, backup otomatis, atau undo.
- **Metadata model dari upstream.** Sync hanya mengambil id; `context` dan
  `capabilities` baru ditangani kalau sudah ada di config atau diisi manual.
- **Key yang belum ter-resolve.** `models sync`/`connection test` gagal bila
  `${VAR}` belum diexport di shell yang menjalankan CLI; belum ada prompt
  interaktif untuk mengisi key sementara.
- **Input secret.** Wizard belum menyembunyikan ketikan API key (tanpa
  `golang.org/x/term`); secret juga bisa terlihat di shell history bila
  `--api-key` diisi literal. Gunakan `${VAR}` atau `lgllm -encrypt`.
- **Katalog remote.** Daftar provider bawaan masih hard-coded; belum ada
  pembaruan katalog dari jaringan.
- **Test koneksi saat sync.** `models sync` tidak otomatis menjalankan
  `connection test`; keduanya perintah terpisah.

---

## 14. Builtin provider OpenCode Go + session header — ✅ SELESAI

> Status: `opencode-go` (alias `ocg`) tersedia di katalog bawaan. Provider ini
> memakai permukaan **OpenAI Chat Completions** di
> `https://opencode.ai/inference/go/openai/v1`, auth bearer
> `OPENCODE_GO_API_KEY`, dan **wajib** mengirim header
> `x-opencode-session` berisi id percakapan yang stabil. Tanpa header itu
> gateway menjawab `400 MissingSessionID`.

Kemampuan ini bukan hard-code per provider: registry menyimpan field deklaratif
`session_header`, jadi provider lain yang punya aturan serupa bisa memakainya
lewat config tanpa mengubah kode proxy.

### 14a. Keputusan implementasi — ✅ SELESAI

- **Satu entri katalog.** Go menerjemahkan setiap model teks ke permukaan
  OpenAI, jadi cukup satu entri `FormatChat`. Daftar model di katalog adalah
  hint `GET /v1/models`, bukan allow-list; model baru tetap diteruskan.
- **Model berbayar saja.** `beam-free`, `iceman-contributor-free`, dan
  `longcat-2.5-free` tidak diiklankan karena dokumentasi menyebut model gratis
  bisa `403` di luar klien OpenCode. Model berbayar tetap bisa dipakai lewat
  id apa pun.
- **Field `session_header`.** `config.Provider.SessionHeader`
  (`yaml:"session_header"`) diteruskan ke `registry.Provider.SessionHeader`.
  Nama header divalidasi sebagai HTTP token dan tidak boleh menimpa header yang
  dikelola lgllm (`authorization`, `content-type`, `host`, dan sejenisnya).
- **Sumber id.** Urutan prioritas: header eksplisit klien
  (`X-Lgllm-Session`, `X-Opencode-Session`, `X-Opencode-Session-Id`,
  `X-Session-Id`, `X-Session-Affinity`, `Session-Id`, `Conversation-Id`,
  `X-Claude-Code-Session-Id`) → `prompt_cache_key` → fingerprint deterministik
  dari pesan user pertama (atau `system`/`instructions`/`prompt`) →
  `metadata.user_id`/`user` → fingerprint request id. Nilai disanitasi dan
  dibatasi 256 byte.
- **Stabil per percakapan.** `internal/sessionid.Fingerprint` menghasilkan
  `ses_` + 12 hex + 14 alfanumerik dari SHA-256 seed. Turn kedua dari
  percakapan yang sama mengirim id yang sama; percakapan lain mendapat id
  berbeda. Seed memakai pesan user pertama karena system prompt sering berubah
  (tanggal, working directory).
- **Terpisah dari sticky balancer.** `generationRequest.conversation` menyimpan
  id keluar, sedangkan `session` tetap dipakai balancer dan hanya diisi id
  eksplisit. Combo round-robin tanpa header klien tetap berotasi; provider yang
  butuh header tetap mendapat fingerprint.
- **Header keluar.** `applySessionHeader` hanya menulis bila provider punya
  `session_header`. User-Agent klien diteruskan; bila kosong dipakai
  `lgllm/<versi>`, karena gateway menolak `Go-http-client/1.1`.
- **Admin dan CLI.** DTO provider admin memuat `session_header`, dan
  `/api/test-connection` mengirim id (dari body `session` atau fingerprint
  probe). `config provider add|set --session-header` menulis field YAML;
  `models sync` mengirim header yang sama saat discovery.
- **CORS.** Header sesi masuk allow-list preflight agar klien browser bisa
  mengirim id percakapan.
- **Verifikasi.** Test fingerprint/sanitasi, prioritas header klien, ekstraksi
  teks empat dialek, integrasi dua turn + percakapan berbeda, provider tanpa
  kapabilitas, test-connection admin, preflight CORS, flag CLI, dan discovery
  `models sync`. `go test ./...` bersih.

### 14b. Belum masuk fase 14

- **Harga model Go.** Tabel harga belum diisi karena Go memakai skema
  peak/off-peak; dashboard biaya akan menampilkan harga tidak diketahui.
- **Model gratis.** Tiga model free-tier sengaja tidak diiklankan; belum ada
  flag untuk membukanya bagi pengguna yang memang punya akses.
- **Session id dari body OpenCode.** Belum ada pembacaan metadata sesi khusus
  OpenCode di body; id diambil dari header, `prompt_cache_key`, atau
  fingerprint pesan pertama.
- **Rotasi id.** Id tidak pernah kedaluwarsa atau dirotasi otomatis; percakapan
  yang sangat panjang tetap memakai satu id sampai klien mengirim id baru.
- **Provider lain.** Baru OpenCode Go yang memakai `session_header` di katalog;
  provider lain bisa menambahkannya lewat config, tetapi belum ada preset.

---

## 15. Builtin provider Command Code — ✅ SELESAI

> Status: `commandcode` (alias `cmd`) dan `commandcode-claude` (alias `cmdc`)
> tersedia di katalog bawaan. Keduanya memakai base URL
> `https://api.commandcode.ai/provider/v1` dengan auth bearer
> `COMMAND_CODE_API_KEY`. `cmd` melayani model OpenAI/chat lewat
> `/chat/completions`; `cmdc` melayani model Claude lewat `/messages`.

Dokumentasi Command Code menyebut satu base URL, tetapi tidak satu format:
Claude hanya menjawab di `/v1/messages`, sedangkan model lain di
`/v1/chat/completions` (dan sebagian di `/v1/responses`). lgllm memilih format
per provider, bukan per model, jadi dua entri katalog adalah representasi yang
jujur tanpa menambah lapisan routing baru.

### 15a. Keputusan implementasi — ✅ SELESAI

- **Dua entri, satu key.** `commandcode` memakai `FormatChat`, sedangkan
  `commandcode-claude` memakai `FormatAnthropic`. Keduanya berbagi base URL,
  auth bearer, dan `KeyEnv: COMMAND_CODE_API_KEY`. Pengguna yang ingin kedua
  kelompok model menambahkan satu koneksi untuk masing-masing provider.
- **Daftar model dari API live.** Hint `cmd` berisi 86 model yang mendukung
  `/chat/completions`; `cmdc` berisi 12 model Claude yang hanya mendukung
  `/messages`. Model dengan prefix vendor (`deepseek/...`, `Qwen/...`,
  `moonshotai/...`) disimpan apa adanya; saat dipanggil lewat lgllm gunakan
  prefix provider, mis. `cmd/deepseek/deepseek-v4-flash`.
- **Model gratis ikut diiklankan.** `inkling-2-small:free` dan
  `glyph-cluster:free` adalah model resmi Provider API dengan batas harian,
  berbeda dari model gratis OpenCode Go yang bisa ditolak di luar kliennya.
- **Tanpa session header.** Command Code tidak mewajibkan header sesi; fitur
  `session_header` tetap tersedia bila provider lain membutuhkannya.
- **Tanpa ZDR default.** Header `x-cmd-zdr: 1` tidak dikirim otomatis karena
  dapat menggagalkan model tanpa upstream ZDR (`422 cmd_zdr_no_providers`).
- **Responses endpoint.** Format `openai-responses` didukung registry, tetapi
  lgllm belum membuka ingress `/v1/responses`; karena itu entri chat memakai
  Chat Completions yang sudah menjadi pivot. Model yang hanya punya
  `/responses` belum bisa dipakai.
- **Anthropic version.** Proxy menambahkan `anthropic-version: 2023-06-01`
  untuk provider `FormatAnthropic` bila belum diisi, sehingga `cmdc` langsung
  bekerja lewat `/v1/messages`.
- **Verifikasi.** Test katalog memastikan base URL, format, key env, pemisahan
  model Claude/non-Claude, dan materialisasi kedua alias menjadi provider
  terpisah. `go test ./...` bersih.

### 15b. Belum masuk fase 15

- **Routing per model.** Belum ada satu provider yang otomatis memilih
  `/messages` untuk Claude dan `/chat/completions` untuk model lain; pengguna
  memilih entri `cmd` atau `cmdc`.
- **Ingress Responses.** `/v1/responses` belum dilayani, jadi klien yang hanya
  bisa memakai Responses API belum bisa memakai model yang hanya mendukung
  endpoint itu.
- **Endpoint `systemone`.** Model keputusan `typesafe/jev` memakai bentuk
  request non-chat dan tidak diiklankan sebagai model chat.
- **ZDR opt-in.** Belum ada opsi config/CLI untuk selalu mengirim
  `x-cmd-zdr: 1`; header bisa ditambahkan manual lewat `headers`.
- **Harga.** Tarif per model Command Code belum masuk tabel `internal/pricing`;
  dashboard biaya akan menampilkan biaya tidak diketahui.
- **Metadata model.** `context_length` dan `supported_endpoints` dari
  `/provider/v1/models` belum diisi otomatis ke config oleh `models sync`.

---

## 16. Dashboard performa lanjutan — ✅ SELESAI

> Status: fase 12 diperluas tanpa dependensi baru. `GET /api/performance`
> sekarang mengembalikan `connections`, `low_sample`, dan (dengan `?series=1`)
> titik time-series. UI menampilkan sparkline TPS, grafik TTFT/latency/TPS saat
> baris diklik, serta tabel per connection. `/metrics` mengekspos gauge
> `lgllm_model_ttft_seconds`, `lgllm_model_tps`, dan
> `lgllm_model_requests_window`.

### 16a. Keputusan implementasi — ✅ SELESAI

- **Dua ring, satu attempt.** `perf.Registry` tetap menyimpan ring model
  (jendela “N attempt terakhir” untuk tabel utama) dan menambahkan ring per
  `provider/model/connection`. `RecordConnection` menulis ke keduanya;
  `Record` lama tetap ada untuk pemanggil yang hanya butuh agregat model.
  Ring connection juga dibatasi N, jadi biaya memori tetap
  `model × N + connection × N`.
- **Breakdown per connection bersifat opsional.** `SnapshotConnections`
  mengembalikan `Aggregate.Connection`. API menempelkannya ke model sebagai
  `connections`; model yang hanya punya sampel tanpa label connection tidak
  mendapat baris palsu. Connection utama tetap menampilkan statistik model
  gabungan, bukan salah satu akun saja.
- **Series opt-in.** `GET /api/performance?series=1` menambahkan array `series`
  berisi sampel ring (lama → baru), termasuk kegagalan. Payload default tetap
  ringkas karena halaman lain dan klien API tidak butuh titik grafik.
  Setiap titik membawa `at`, `ok`, `stream`, `ttft_ms`, `latency_ms`, `tps`,
  `tps_estimated`, `tokens`, dan `tokens_known`; nilai timing `null` untuk
  attempt yang gagal.
- **Low sample.** Ambangnya konstanta `perf.LowSampleThreshold = 3`. Mode
  dengan 1–2 success ditandai `low_sample: true`; API juga mengirim
  `low_sample_at` agar UI tidak menebak ambang. Rata-rata tetap ditampilkan,
  hanya diberi badge `low`, bukan disembunyikan.
- **Grafik tanpa library.** UI memakai SVG inline: sparkline TPS di tabel dan
  dua grafik waktu (TTFT + latency, lalu TPS) di baris detail. Garis putus-putus
  merah menandai attempt gagal. Tidak ada canvas, chart library, atau langkah
  build; CSP tetap `default-src 'self'`.
- **Tabel per connection.** Baris detail memuat connection, attempts, success,
  TTFT, latency, TPS, mode, dan last seen. Klik baris membuka/menutup detail;
  auto-refresh 5 detik tetap mempertahankan baris yang sedang terbuka.
- **Gauge Prometheus.** `internal/metrics.WritePerformanceGauges` merender dari
  snapshot ring saat scrape, bukan menyimpan state kedua. Gauge TTFT/TPS memakai
  label `provider`, `model`, `mode` (`stream`/`non_stream`); requests hanya
  `provider`, `model`. Mode tanpa success tidak mengeluarkan gauge TTFT/TPS.
- **Verifikasi.** Test baru mencakup pemisahan ring connection, urutan series,
  `low_sample`, `connections` di API, `?series=1` opt-in, dan gauge di
  `/metrics`; test UI statis memastikan endpoint series dan label baru dipakai.
  `make check` dan `go test -race ./...` bersih.

### 16b. Belum masuk fase 16

- **Persentil.** Grafik dan tabel masih memakai rata-rata; belum ada p50/p95/p99
  atau histogram per model di dashboard.
- **Riwayat panjang.** Series hanya berisi maksimal N attempt terakhir di memori;
  belum ada SQLite/rollup per jam atau per hari.
- **Ambang low sample yang dapat dikonfigurasi.** Nilai 3 masih konstanta kode,
  belum field `metrics.*`.
- **Baris per connection di tabel utama.** Breakdown hanya muncul di detail;
  belum ada tampilan datar `provider/model/connection` atau filter per akun.
- **Agregat alias dan combo.** Baris tetap mengikuti model upstream; alias/combo
  belum mendapat ring sendiri.
- **Alerting.** Belum ada ambang error rate/TTFT yang memicu peringatan; gauge
  hanya untuk scrape eksternal.
- **TPS non-stream eksak.** Tetap `tokens/latency` dan ditandai `est.` karena
  respons JSON tidak punya sinyal token pertama.
- **Token stream di usage log.** Tap sudah membaca usage untuk performa, tetapi
  entri usage request stream masih 0.
- **Overhead sampling.** Pembacaan/parsing SSE per event belum diukur pada
  throughput sangat besar.

---

## 17. Stream cancel setelah finish (Command Code) — ✅ SELESAI

> Status: `context canceled` pada provider Command Code tidak lagi dicatat
> sebagai kegagalan model. Penyebabnya adalah Command Code mengirim
> `finish_reason` dan chunk `usage` secara terpisah, sehingga client yang
> berhenti di `finish_reason` menutup koneksi sebelum chunk usage dan `[DONE]`
> terbaca. Tap sekarang mencatat sinyal finish; abort setelah finish dihitung
> sukses, abort sebelum finish tidak masuk success rate, dan keduanya terlihat
> di `lgllm_client_cancelled_total`.

### 17a. Keputusan implementasi — ✅ SELESAI

- **Akar masalah.** Reproduksi dengan client yang berhenti tepat di
  `finish_reason` menghasilkan `context canceled` hanya untuk Command Code.
  Command Code mengirim chunk finish tanpa `usage`, lalu chunk usage terpisah,
  lalu `[DONE]`; OpenCode Go menggabungkan `finish_reason` dan `usage` dalam
  satu chunk. Client yang berhenti di finish membatalkan `r.Context()` saat
  lgllm masih menunggu sisa stream.
- **Sinyal finish di tap.** `perf.TapStats` mendapat `FinishedAt` dan
  `FinishReason`, dengan helper `SawFinish()`. Deteksi mencakup OpenAI
  (`choices[].finish_reason`), Anthropic (`message_delta.stop_reason` dan
  `message_stop`), Gemini (`candidates[].finishReason`), dan Responses
  (`response.completed`). Tap tetap read-only dan tidak mengubah byte.
- **Klasifikasi abort.** `finishStream` membedakan tiga hasil: selesai normal,
  client pergi setelah finish, dan client pergi sebelum finish. Abort terdeteksi
  dari `context.Canceled` yang disertai `r.Context().Err() != nil`, atau dari
  `upstream.ErrWriteTimeout` (client berhenti membaca).
- **Dampak ke performa.** Abort setelah finish dicatat sebagai attempt sukses;
  usage tetap `n/a` karena chunk usage mungkin belum terbaca. Abort sebelum
  finish tidak dicatat sebagai sukses maupun gagal, sehingga success rate model
  tidak turun karena client yang menutup koneksi.
- **Log.** Pembatalan klien dicatat pada level INFO dengan pesan
  `client closed after finish` atau `client canceled stream`. WARN tetap
  dipakai untuk error upstream nyata dan idle timeout.
- **Metrik.** Counter `lgllm_client_cancelled_total{provider,after_finish}`
  membedakan abort setelah finish dari abort dini. Tidak ada state tambahan;
  counter hidup di `internal/metrics` seperti counter lain.
- **Tanpa buffering finish.** Chunk `finish_reason` tidak ditahan sampai usage
  datang, karena itu menambah latensi dan melanggar passthrough byte-identik.
- **Verifikasi.** Test tap untuk empat format, test server untuk abort
  setelah/sebelum finish (perf, log, dan counter), serta `make check` dan
  `go test -race ./...` bersih.

### 17b. Belum masuk fase 17

- **Usage setelah client pergi.** Kalau client menutup sebelum chunk usage,
  token completion tetap tidak diketahui; TPS model tersebut `n/a`.
- **Client yang berhenti di finish.** Perilaku client tetap di luar kendali
  lgllm; client yang ingin usage lengkap harus membaca sampai `[DONE]`.
- **Counter per model.** `lgllm_client_cancelled_total` hanya berlabel provider,
  belum model atau connection.
- **Ambang abort.** Belum ada klasifikasi abort karena timeout client tertentu
  versus tombol stop; keduanya saat ini terlihat sama.
- **Provider lain dengan chunk pasca-finish.** Mekanisme tap generik sudah
  menangani finish, tetapi belum ada test live untuk setiap provider.
- **Persistensi.** Counter dan sinyal finish tetap in-memory seperti metrik lain.

---

## 18. Endpoint detail model — ✅ SELESAI

> Status: `GET /v1/models/{model...}` dan alias `GET /api/v1/models/{model...}`
> mengembalikan satu objek model OpenAI. Path memakai trailing wildcard karena
> id provider-prefixed dan id gaya OpenRouter mengandung slash.

### 18a. Keputusan implementasi — ✅ SELESAI

- **Wildcard, bukan satu segmen.** Route `GET /v1/models/{model...}` menangkap
  seluruh sisa path, jadi `cmd/deepseek/deepseek-v4.1-flash` dan
  `ocg/deepseek-v4.1-flash` sampai utuh. `/v1/models` tetap menjadi daftar.
- **Resolusi lewat registry.** Handler memakai `Registry.Lookup`, jadi detail
  bekerja untuk id provider/model, alias, combo, dan bare model yang dirutekan
  `default_provider`. Id yang dikembalikan adalah id yang diminta, bukan hasil
  normalisasi, agar client yang menyimpan nilai dari path tidak kaget.
- **`owned_by`.** Provider asli untuk model/alias, dan `"combo"` untuk combo.
  Field lain tetap bentuk OpenAI: `object: "model"`, `created: 0`.
- **404 jujur.** Nama yang tidak bisa dirutekan mendapat JSON 404
  `model "..." not found`; path kosong juga 404. Model di luar daftar hint tetap
  boleh selama provider prefix/default-nya valid, konsisten dengan prinsip
  "model list adalah hint, bukan allow-list".
- **Alias path.** `/api/v1/models/{model...}` memakai handler yang sama, jadi
  payload tidak bisa berbeda dari `/v1/models/{model...}`.
- **Root discovery.** `GET /` mencantumkan kedua endpoint detail.
- **Verifikasi.** Test mencakup model dengan slash, alias, alias path API,
  unknown provider, dan bentuk JSON. `make check` dan `go test -race ./...`
  bersih.

### 18b. Belum masuk fase 18

- **Metadata detail.** Belum ada `context_window`, `capabilities`, atau harga di
  objek model; field tersebut masih hanya di config dan endpoint kompatibilitas
  lain.
- **Detail dari upstream.** Endpoint dijawab dari registry lokal; belum
  memanggil `GET /models/{id}` upstream untuk metadata live.
- **Cache dan ETag.** Belum ada cache atau `ETag`; setiap request membaca
  snapshot registry in-memory.
- **Model yang tidak advertised.** Detail tetap mengembalikan model yang tidak
  ada di daftar hint selama provider prefix/default valid.
- **Bentuk Ollama/llama.cpp.** Detail model tetap memakai bentuk OpenAI; jalur
  Ollama tetap lewat `/api/show`.

---

## 19. Proses rilis — ✅ SELESAI

> Status: rilis pertama disiapkan end-to-end: `LICENSE`, metadata GoReleaser yang
> benar, paket memasang binary di `/usr/bin/lgllm`, image multi-arch di GHCR,
> workflow tag `v*`, draft release sebelum publish, checksum, SBOM/provenance,
> dan signing image dengan cosign.

### 19a. Keputusan implementasi — ✅ SELESAI

- **Lisensi.** `LICENSE` MIT ditambahkan dan ikut masuk arsip rilis serta image.
- **GoReleaser.** `before.hooks` memakai `go mod verify` (tidak menulis ulang
  `go.mod`/`go.sum`), `bindir` eksplisit `/usr/bin`, homepage diperbaiki ke
  `taufikterdidik/lgllm`, dan arsip memuat LICENSE, README, contoh config,
  systemd unit, PLAN, serta BENCH.
- **Paket Linux.** Binary deb/rpm/apk terpasang di `/usr/bin/lgllm`; unit
  systemd dan instruksi post-install memakai path yang sama.
- **Container.** Dockerfile multi-arch memakai `TARGETOS`/`TARGETARCH`, label OCI,
  `HEALTHCHECK` lewat binary `-healthcheck`, dan CMD `-listen 0.0.0.0:20128`.
  `.dockerignore` mencegah config lokal, admin overlay, `.git`, dan artefak build
  masuk context.
- **CI.** `ci.yml` menjalankan gofmt/vet/race/build, `goreleaser check`,
  `goreleaser release --snapshot --clean`, dan `docker build`.
- **Workflow rilis.** Tag `v*` menjalankan test gate, membuat arsip/paket sebagai
  draft, membangun image amd64+arm64, menandatangani digest image, lalu
  mempublikasikan release. Tag dengan `-` menjadi prerelease dan tidak memindahkan
  `latest`.
- **Dependency hygiene.** Dependabot mingguan untuk Go module dan GitHub Actions.
- **Verifikasi.** `goreleaser check` dan snapshot lintas platform berhasil;
  paket deb berisi `/usr/bin/lgllm` dan unit systemd; image 9.0 MB berjalan dan
  `-version` bekerja; YAML workflow valid.

### 19b. Belum masuk fase 19

- **Tanda tangan checksum dan paket.** Image ditandatangani cosign; checksum dan
  paket deb/rpm/apk belum ditandatangani.
- **Homebrew tap.** Blok `brews` masih dikomentari sampai repository tap dibuat.
- **Build macOS/Windows asli.** Cross-build dibuat, tetapi runtime macOS, Windows,
  dan ARM belum diuji di CI.
- **Uji instalasi distro bersih.** Paket belum diuji di Debian/Fedora/Alpine
  bersih; unit systemd juga belum diuji di semua distro.
- **Notarization/signing Windows.** Belum ada Authenticode atau notarization
  Apple.
- **Auto-update.** Belum ada pemeriksaan versi terbaru atau mekanisme upgrade.
- **Soak/load test.** Rilis belum di-gate oleh uji beban jangka panjang.
- **Rilis pertama.** `v0.1.0` sudah dipublikasikan pada 2026-11-15; rilis
  berikutnya memakai workflow yang sama.

---

## 20. Konfigurasi UI berbasis form + generate key — ✅ SELESAI

> Status: tab Providers, Connections, Combos, dan Keys sekarang memakai form.
> Katalog provider bawaan tersedia lewat API, provider list menampilkan status
> koneksi, combo punya member picker, dan client key baru bisa digenerate dengan
> tombol **Generate key**.

### 20a. Keputusan implementasi — ✅ SELESAI

- **Katalog untuk UI.** `GET /api/catalog` mengembalikan provider bawaan
  (base URL, format, auth, headers, model hints, `key_env`, situs) sehingga form
  tidak perlu menyalin daftar di JavaScript.
- **Provider list efektif.** `GET /api/providers?all=1` menggabungkan provider
  yang dikonfigurasi dengan katalog yang belum dipakai, memakai field hasil
  materialisasi, dan menambahkan `connected`, `connections`, `key_env`, `site`.
  Sumbernya dibedakan `catalog`, `config`, atau `admin`. Tanpa `?all=1`, respons
  lama tetap berisi provider efektif yang sudah ada.
- **Generate key.** `POST /api/keys/generate` membuat
  `sk-lgllm-<32 byte random base64url>` dengan `crypto/rand`, menyimpannya di
  overlay, dan mengembalikan plaintext sekali. `GET /api/keys` tetap hanya
  menampilkan hash prefix. `POST /api/keys` tetap tersedia untuk key manual.
- **Form provider.** Sumber `catalog`/`custom`, prefill otomatis, edit base URL,
  format, auth, timeout, session header, header key/value, dan model rows
  (id/name/context/capabilities). Memilih catalog lalu menyimpan membuat override
  admin tanpa mengubah katalog; provider config tetap read-only.
- **Form connection.** Provider dipilih dari dropdown (termasuk katalog yang
  belum terhubung), ID disarankan otomatis, API key password, override base URL,
  dan status disabled. Key `***` tidak dikirim balik; field kosong mempertahankan
  nilai lama.
- **Form combo.** Strategy, token saver tri-state (inherit/on/off), dan member
  list dengan picker model, input bebas untuk alias, serta tombol urut naik/turun.
- **Keys UI.** Tombol utama Generate key menampilkan modal copy-once; add existing
  key tetap tersedia sebagai aksi sekunder.
- **Smoke test.** Server sementara + Playwright menjalankan alur tambah provider
  katalog, tambah connection, tambah combo, dan generate key tanpa console error.

### 20b. Belum masuk fase 20

- **Alias UI.** Alias routing masih lewat config/CLI; belum ada tab alias.
- **Sync model dari upstream.** Form provider masih manual; tombol “fetch models”
  yang memakai `GET /models` provider belum ada.
- **Test sebelum simpan.** Test connection memakai connection tersimpan; belum
  ada test inline dengan API key yang belum disimpan.
- **Metadata key.** Belum ada label, tanggal dibuat, expiry, atau usage per key.
- **Section config lain.** `server`, `auth`, `metrics`, `upstream`, dan `admin`
  tetap diedit lewat file config.
- **Advanced JSON.** Editor JSON mentah sudah dihapus dari jalur utama; pengguna
  yang butuh field di luar form harus mengedit `admin.yaml`/`config.yaml`.
- **Drag-and-drop dan import/export.** Reorder combo masih tombol, belum drag;
  belum ada backup/restore konfigurasi dari UI.
- **Mobile polish.** Tabel dan form responsif dasar, tetapi belum dioptimalkan
  untuk layar kecil.

---

## 21. Ikon aplikasi dan favicon dashboard — ✅ SELESAI

> Status: ikon sumber 1254×1254 dipakai sebagai ikon proyek, favicon dashboard,
> apple-touch-icon, ikon PWA, ikon paket Linux, dan logo kecil di header UI.

### 21a. Keputusan implementasi — ✅ SELESAI

- **Satu sumber, beberapa ukuran.** `icon.png` 512×512 menjadi ikon aplikasi di
  root repo; turunannya `favicon-32.png`, `apple-touch-icon.png`,
  `icon-192.png`, dan `icon-512.png` masuk ke aset UI yang di-embed.
- **Dashboard.** `index.html` memuat favicon, apple-touch-icon, manifest PWA,
  `theme-color`, dan logo di header. Manifest menyediakan nama, warna tema, dan
  ikon 192/512.
- **Distribusi.** Arsip GoReleaser memuat `icon.png`; paket deb/rpm/apk memasang
  ikon ke `/usr/share/icons/hicolor/512x512/apps/lgllm.png`.
- **CSP tetap ketat.** `img-src 'self' data:` sudah mengizinkan aset lokal tanpa
  membuka sumber eksternal.
- **Verifikasi.** Test UI memeriksa link favicon/manifest/logo dan endpoint
  `/ui/favicon-32.png` serta `/ui/manifest.json`; aset tetap tersaji dari embed.

### 21b. Belum masuk fase 21

- **Ikon executable.** Binary Linux tidak membawa ikon embedded; ikon hanya
  tersedia di UI, arsip, dan paket.
- **Desktop entry.** Belum ada file `.desktop` yang memasangkan ikon dengan
  aplikasi desktop.
- **Logo gelap/terang.** Belum ada varian warna terpisah untuk tema terang.
- **Maskable icon.** Ikon PWA belum diuji dengan safe area maskable.

---

## 22. Context window combo + endpoint model OpenRouter — ✅ SELESAI

> Status: `GET /v1/model/{model...}` dan alias `/api/v1/model/{model...}`
> tersedia, model detail membawa `context_length`, dan combo memakai context
> terkecil dari membernya. `/api/show` dan `/v1/props` juga memakai nilai combo
> yang sama.

### 22a. Keputusan implementasi — ✅ SELESAI

- **Penyebab 404.** Client meminta `/v1/model/dsf` (singular, gaya OpenRouter),
  sedangkan route yang ada hanya `/v1/models/{model...}`. Route singular
  ditambahkan sebagai compatibility alias dengan envelope `{"data": ...}`.
- **Context combo.** `modelContextInfoFor` me-resolve model/alias/combo, lalu
  menghitung nilai positif terkecil dari seluruh member. Combo dengan member
  yang context-nya belum diketahui menghasilkan `context_known: false` dan tidak
  mengiklankan angka, supaya client tidak memakai batas yang terlalu besar.
- **Endpoint yang diperbaiki.** `/v1/models` dan `/v1/models/{model}` membawa
  `context_length`/`context_window`; `/api/show` mengisi
  `lgllm.context_length`; `/v1/props` mengisi `n_ctx` dengan context combo.
- **Sumber context.** `config.models[].context` tetap otoritatif. Katalog
  bawaan mendapat hint context untuk model yang diketahui
  (`commandcode/deepseek/deepseek-v4.1-flash` dan
  `opencode-go/deepseek-v4.1-flash` = 1.000.000), sehingga combo `dsf` langsung
  terisi.
- **UI.** Tab Combos menampilkan kolom `ctx min`, atau `unknown` bila salah satu
  member belum punya context.
- **Verifikasi.** Test server untuk combo 128k vs 1M, alias, member unknown,
  `/v1/model`, `/v1/models`, `/api/show`, dan `/v1/props`; live probe combo
  `dsf` menghasilkan 1.000.000.

### 22b. Belum masuk fase 22

- **Sync metadata context.** `models sync` masih mengambil id; belum menyimpan
  `context_length` dari `/models` upstream.
- **Context katalog lengkap.** Hanya model yang diketahui yang diberi hint;
  model lain tetap `unknown` sampai diisi operator.
- **Context per member di UI.** Kolom combo menampilkan min; detail per member
  baru ada di respons API, belum di tabel UI.
- **Model tanpa advertised metadata.** Model yang diteruskan tetapi tidak ada di
  daftar hint tetap unknown.
- **Nested combo.** Combo di dalam combo belum didukung resolver routing.

---

## 23. Persistensi performance dengan bbolt + ring — ✅ SELESAI

> Status: sampel performa tetap dibaca dari ring in-memory, tetapi sekarang bisa
> ditulis ke bbolt lewat queue write-behind. Saat restart, ring diisi kembali
> dari sampel terbaru di database.

### 23a. Keputusan implementasi — ✅ SELESAI

- **Ring tetap jalur baca.** API/UI tidak menunggu disk. `Registry` menyimpan
  ring per model dan per connection seperti sebelumnya; bbolt hanya menerima
  salinan sampel.
- **Write-behind queue.** `perf.Store` memakai channel berkapasitas 4096,
  batches hingga 200 sampel, dan flush setiap 200ms. Queue penuh men-drop sampel
  (best-effort metrics), bukan memblokir request.
- **Skema bbolt.** Bucket `models` berisi sub-bucket
  `provider\x00model\x00connection`; setiap sampel diberi sequence bbolt.
  `LoadRecent` mengambil N sampel terbaru per key, mengurutkannya berdasarkan
  waktu, lalu mengisi ring model/connection tanpa menulis ulang ke DB.
- **Retensi dan batas.** `metrics.performance_retention` default 168 jam; setiap
  key dibatasi 2000 baris. Prune dilakukan saat open, berkala saat flush, dan
  saat shutdown.
- **Konfigurasi.** `metrics.performance_db` kosong berarti persistence mati.
  Path non-empty membuka database mode 0600; parent directory dibuat 0750.
  Perubahan path/retensi membutuhkan restart dan menghasilkan warning reload.
- **Operasional.** Store ditutup setelah HTTP server shutdown, sehingga sampel
  terakhir di queue sempat ditulis. bbolt lock timeout 1 detik dan schema
  version dicek saat open.
- **Verifikasi.** Test menulis 5 sampel, menutup registry, membuka database
  lagi, dan memastikan ring/connection berisi 3 sampel terbaru. Test config
  mencakup default retention dan nilai nol; `make check` dan race test bersih.

### 23b. Belum masuk fase 23

- **API history panjang.** API masih membaca ring; belum ada endpoint untuk
  query deret waktu dari bbolt.
- **Kapasitas per key configurable.** Batas 2000 masih konstanta kode.
- **Kompresi/rotasi file.** Belum ada compaction manual, enkripsi database, atau
  rotasi file.
- **Multi-proses.** Satu file bbolt hanya boleh dibuka satu proses; belum ada
  dukungan multi-instance berbagi database.
- **Metrik drop.** Jumlah sampel yang di-drop tersedia di `Store.Dropped()`,
  tetapi belum diekspos ke `/metrics`/UI.
- **Backup/restore.** Belum ada perintah backup database performa.

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
| 12 | Dashboard TPS/TTFT/latency per model | 1,5–2 hari | ✅ /api/performance + tab Performance |
| 13 | Config CLI (provider, connection, sync model) | 2 hari | ✅ non-interaktif + wizard |
| 14 | Builtin OpenCode Go + session header | 1 hari | ✅ `opencode-go` + `session_header` |
| 15 | Builtin Command Code (chat + Claude) | 1 hari | ✅ `commandcode` + `commandcode-claude` |
| 16 | Dashboard performa lanjutan | 1 hari | ✅ grafik, per-connection, gauge Prometheus |
| 17 | Stream cancel setelah finish | 0,5 hari | ✅ abort client tidak dihitung gagal + counter |
| 18 | Endpoint detail model | 0,5 hari | ✅ `GET /v1/models/{model...}` |
| 19 | Proses rilis | 1 hari | ✅ workflow tag → artifacts + image + release |
| 20 | UI form provider/connection/combo + generate key | 2 hari | ✅ katalog, form, copy-once key |
| 21 | Ikon aplikasi + favicon | 0,5 hari | ✅ favicon, apple-touch-icon, manifest |
| 22 | Context combo + endpoint model OpenRouter | 1 hari | ✅ `/v1/model/{model}` + ctx minimum |
| 23 | Persistensi performance bbolt + ring | 1 hari | ✅ write-behind queue + reload saat restart |

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
