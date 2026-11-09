# Rencana: dashboard performa per model upstream

Status: **rencana** (belum diimplementasikan)

## Tujuan

Menampilkan, untuk setiap model upstream yang tersedia, rata-rata **TPS**
(token/detik), **TTFT** (time to first token), dan **latency** berdasarkan
**10 request terakhir**. Data muncul di UI web (`/ui/`) dan tersedia lewat API
admin agar bisa dipakai klien lain.

Batasan yang dipegang:

- Tidak menambah dependensi runtime atau langkah build Node.
- Tidak menyimpan prompt, isi respons, atau credential — hanya angka dan
  metadata model.
- Data in-memory (ring buffer), konsisten dengan usage log; hilang saat restart.
- Model yang belum pernah dipakai tetap muncul dengan nilai `n/a`, karena
  pertanyaannya "masing-masing model yang tersedia".

## Definisi metrik

| Metrik | Definisi | Catatan |
|---|---|---|
| **TTFT** | waktu dari request dikirim ke upstream sampai **token/konten pertama** tiba | Bukan `message_start` (Anthropic) atau chunk role-only (OpenAI); event pertama yang membawa `text`/`tool_calls`/`functionCall`. |
| **Latency** | waktu dari request dikirim sampai byte terakhir respons diterima | Untuk stream: sampai stream selesai/DONE. Untuk non-stream: sampai body selesai dibaca. |
| **TPS** | `completion_tokens / (last_content − first_content)` | Untuk non-stream tidak ada sinyal first-token; dipakai `completion_tokens / latency` dan ditandai **est.** |
| **Attempts** | jumlah percobaan terakhir yang masuk jendela (maks 10) | Termasuk kegagalan; rata-rata hanya dihitung dari yang sukses. |
| **Success rate** | `sukses / attempts` dalam jendela | Kegagalan tidak menggeser TTFT/latency/TPS. |

Aturan sampling:

- Satu sampel = satu **attempt** ke satu `provider/model`. Combo/fallback
  menghasilkan beberapa sampel; yang tampil adalah model yang benar-benar
  dicoba.
- Attempt non-2xx/transport gagal tetap dicatat (`ok=false`, latency, tanpa
  token) agar success rate jujur; rata-rata performa mengabaikannya.
- Stream dan non-stream dipisah dalam agregat karena TPS/TTFT tidak sebanding.
  UI menampilkan keduanya (mis. `stream 7 · non-stream 2`).
- Token tidak diketahui (provider tidak mengirim usage) → TPS `n/a`, TTFT dan
  latency tetap tampil. Untuk OpenAI pass-through, usage hanya ada bila klien
  mengirim `stream_options.include_usage`; request hasil translasi
  (`/v1/messages` → OpenAI) sudah menambahkannya.

## Arsitektur

### 1. `internal/perf` (baru)

```go
type Window struct {            // satu provider/model
    Samples []Sample            // ring, maks N (default 10)
}

type Sample struct {
    At        time.Time
    OK        bool
    Stream    bool
    TTFT      time.Duration
    Latency   time.Duration
    Tokens    int
    TokensKnown bool
    TPS       float64           // estimasi bila non-stream
}

type Registry struct {          // map[key]*Window + mutex
    Record(provider, model string, s Sample)
    Snapshot() []Aggregate
}

type Aggregate struct {
    Provider, Model string
    Attempts, Successes, Failures int
    Stream, NonStream ModeStats
    LastSeen time.Time
}
```

- Key: `providerID + "/" + modelID` (id upstream, bukan alias).
- Ukuran jendela dari config (`metrics.performance_window`, default 10);
  perubahan butuh restart (sama seperti usage log).
- Saat serve, gabungkan dengan `registry.Providers()` supaya semua model yang
  diiklankan muncul; key yang sudah tidak ada di registry tidak ditampilkan.

### 2. Pengukuran

Titik ukur di `internal/server/retry.go` (`tryConnection`):

```
start := time.Now()            // sebelum s.Do
resp, err := s.Upstream.Do(...)
// header diterima
...
firstContent := tap.FirstContentAt()   // dari SSE tap (stream)
end := time.Now()                       // setelah body selesai dibaca/relay
```

- **Stream:** bungkus `resp.Body` dengan `perf.SSETap` sebelum masuk ke
  passthrough/konverter. Tap:
  - mencatat waktu `data:` pertama yang membawa konten (bukan komentar/`ping`),
  - membaca `usage` di event akhir sesuai format,
  - tidak mengubah byte (passthrough tetap byte-for-byte).
- **Non-stream:** setelah body dibaca, token diambil dari decoder format yang
  sudah dipakai untuk metrics. TTFT = latency; TPS = token/latency (est.).
- **Gagal:** catat `ok=false` + latency; TTFT/TPS nol.
- **Commit:** sampel dicatat saat respons selesai (bukan saat header), sehingga
  stream yang putus di tengah tetap punya latency parsial dan ditandai gagal.

Ekstraksi usage per format (di tap atau konverter):

| Format | Sumber token |
|---|---|
| OpenAI stream | chunk dengan `usage` (bila `stream_options.include_usage`) |
| OpenAI non-stream | `usage.prompt_tokens/completion_tokens` |
| Anthropic | `message_delta.usage.output_tokens` + `message_start.usage.input_tokens` |
| Gemini | `usageMetadata.candidatesTokenCount` |
| Responses | `response.completed.response.usage` |

### 3. API

`GET /api/performance` (token admin, sama seperti endpoint admin lain):

```json
{
  "object": "list",
  "window": 10,
  "data": [
    {
      "provider": "deepseek",
      "model": "deepseek-chat",
      "available": true,
      "attempts": 10,
      "successes": 9,
      "failures": 1,
      "last_seen": "2026-11-09T10:15:00Z",
      "stream": {
        "samples": 7,
        "avg_ttft_ms": 310,
        "avg_latency_ms": 2600,
        "avg_tps": 41.2,
        "tokens_known": true
      },
      "non_stream": {
        "samples": 2,
        "avg_ttft_ms": 900,
        "avg_latency_ms": 900,
        "avg_tps": 33.1,
        "tokens_known": true,
        "tps_estimated": true
      }
    }
  ]
}
```

- `avg_*` dibulatkan; `null`/tidak ada bila `samples=0`.
- Model tanpa sampel tetap dikembalikan dengan `attempts: 0`.
- Filter opsional `?provider=` untuk halaman provider tertentu.
- Tidak ada data prompt/credential.

### 4. UI

Tab baru **Performance** di `internal/server/ui/index.html` + `app.js`:

```
Performance                     window: last 10 attempts · refresh 5s [x]
Provider/model          Req  OK   TTFT     Latency   TPS      Mode
ds/deepseek-chat        10   9    310 ms   2.6 s     41.2     stream 7 · direct 2
oa/gpt-4.1               6   6    480 ms   1.9 s     38.9     stream 6
cc/claude-sonnet-4-5      0   0    n/a      n/a       n/a      —
```

- Kolom: model, attempts (`n/10`), success, TTFT, latency, TPS, mode, last seen.
- Badge `est.` untuk TPS non-stream; `n/a` bila token tidak diketahui.
- Sort default: TPS menurun, model tanpa data di bawah; klik header untuk sort.
- Auto-refresh 5 detik, berhenti saat tab browser tidak aktif.
- Empty state: "Belum ada sampel — kirim satu request ke model ini."
- Tanpa grafik di iterasi pertama; angka tabel dulu supaya cepat dan mudah diuji.

### 5. Observability (opsional, murah)

Tambahkan gauge Prometheus di `internal/metrics`:

- `lgllm_model_ttft_seconds{provider,model}` (avg jendela)
- `lgllm_model_tps{provider,model}` (avg jendela)
- `lgllm_model_requests_window{provider,model}`

Ini memungkinkan scrape tanpa membuka UI. Tidak wajib untuk iterasi pertama.

## Langkah implementasi

1. **`internal/perf`** — ring buffer per key, agregasi, unit test (window wrap,
   kegagalan tidak menggeser rata-rata, model tanpa sampel, konkurensi).
2. **`perf.SSETap`** — reader yang mencatat first-content dan usage; unit test
   dengan potongan SSE per format (OpenAI, Anthropic, Gemini, Responses),
   termasuk event yang terpecah antar-`Read`.
3. **Instrumentasi server** — pasang tap/waktu di `tryConnection`; catat sampel
   sukses dan gagal. Pastikan passthrough tetap byte-identik (tap hanya membaca
   salinan).
4. **API admin** — `GET /api/performance`, gabungkan dengan daftar model
   registry, tambahkan test server.
5. **UI** — tab + tabel + polling; test statis bahwa aset memuat endpoint dan
   label kolom.
6. **Config** — `metrics.performance_window` (default 10), validasi > 0.
7. **Dokumentasi** — README (tab baru), `docs/PLAN.md` (fase 12), catatan
   definisi TPS/TTFT.
8. **Verifikasi live** — fake upstream dengan delay terkontrol; cek angka
   mendekati expected (toleransi ±15%), lalu cek tampilan `/ui/`.

## Kriteria selesai

- Setelah ≥10 request ke satu model, API/UI menampilkan rata-rata dari maksimal
  10 attempt terakhir; request ke-11 menggeser yang tertua.
- TTFT diukur sampai konten pertama; latency sampai akhir respons.
- TPS stream memakai token dari usage provider; non-stream ditandai `est.`.
- Kegagalan tercatat di success rate tetapi tidak menggeser rata-rata performa.
- Model yang tersedia tetapi belum dipakai tampil dengan `n/a`.
- Tidak ada prompt/credential di data maupun log baru.
- UI memperbarui otomatis dan tetap berfungsi saat tab lain dipakai.
- `make check` dan `go test -race ./...` bersih.

## Risiko & mitigasi

| Risiko | Mitigasi |
|---|---|
| Provider tidak mengirim usage pada stream | Tampilkan `n/a`; untuk request hasil translasi, pastikan `include_usage` diset. Jangan mengarang token. |
| TPS stream vs non-stream tidak sebanding | Pisahkan agregat `stream`/`non_stream`; tandai `est.`. |
| Sampel 10 terlalu kecil/noisy | Tampilkan jumlah sampel; jendela bisa dikonfigurasi; jangan tampilkan angka bila `samples < 3` (opsional: beri label "low sample"). |
| Tap mengganggu passthrough | Tap hanya `io.Reader` yang menyalin byte; ada test byte-identik untuk stream. |
| Model dihapus/diubah saat reload | Filter snapshot API dengan registry aktif; key lama tidak ditampilkan. |
| Memori tumbuh karena banyak model | Map dibatasi jumlah model × N sampel; bersihkan key yang tidak ada saat reload (opsional). |
| Ketidakcocokan attempt vs request | Dokumentasikan bahwa satu baris = attempt provider/model, bukan satu request klien. |

## Estimasi

- `internal/perf` + tap + test: ±0,5 hari
- instrumentasi + API + test: ±0,5 hari
- UI + test + dokumentasi: ±0,5 hari
- Verifikasi live: ±0,25 hari

Total ±1,5–2 hari kerja fokus. Tidak memerlukan dependensi baru.

## Setelah fitur ini

1. **Token stream + SQLite**: angka TPS untuk pass-through OpenAI dan riwayat
   yang tahan restart; sekaligus menutup item fase 7/9.
2. **Ingress `/v1/responses`**: Codex CLI tanpa `wire_api = "chat"`.
3. **Publikasi rilis**: tag + GitHub Actions + goreleaser + image multi-arch.
4. **Soak/load test** dan CI macOS/ARM.
5. OAuth/web-login tetap tidak disarankan (di luar cakupan v1).