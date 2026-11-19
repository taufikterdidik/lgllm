package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lgllm/internal/config"
	"lgllm/internal/perf"
	"lgllm/internal/registry"
	"lgllm/internal/tokensaver"
)

func TestAdminPerformanceShowsWindowAndSplitsModes(t *testing.T) {
	s, _, _ := newAdminTestServer(t, false)

	s.Perf.Record("testprov", "test-model", perf.Sample{
		OK: true, Stream: true,
		TTFT: 100 * time.Millisecond, Latency: 2 * time.Second,
		Tokens: 80, TokensKnown: true, TPS: 40,
	})
	s.Perf.Record("testprov", "test-model", perf.Sample{
		OK: false, Stream: true, Latency: 3 * time.Second,
	})

	res := do(s, adminRequest(http.MethodGet, "/api/performance", adminTestToken, ""))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	var body struct {
		Object string         `json:"object"`
		Window int            `json:"window"`
		Data   []perfModelDTO `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, res.Body)
	}
	if body.Object != "list" || body.Window != perf.DefaultWindow {
		t.Errorf("object/window = %q/%d", body.Object, body.Window)
	}
	if len(body.Data) != 1 {
		t.Fatalf("data = %d rows, want 1", len(body.Data))
	}
	m := body.Data[0]
	if m.Provider != "testprov" || m.Model != "test-model" {
		t.Errorf("model = %s/%s", m.Provider, m.Model)
	}
	if m.Attempts != 2 || m.Successes != 1 || m.Failures != 1 {
		t.Errorf("attempts/successes/failures = %d/%d/%d", m.Attempts, m.Successes, m.Failures)
	}
	if m.LastSeen == nil {
		t.Error("last_seen is null")
	}
	if m.Stream.Samples != 1 || m.Stream.AvgTTFTMS == nil || *m.Stream.AvgTTFTMS != 100 {
		t.Errorf("stream = %+v", m.Stream)
	}
	if m.Stream.AvgLatencyMS == nil || *m.Stream.AvgLatencyMS != 2000 {
		t.Errorf("stream latency = %v", m.Stream.AvgLatencyMS)
	}
	if m.Stream.AvgTPS == nil || *m.Stream.AvgTPS != 40 {
		t.Errorf("stream tps = %v", m.Stream.AvgTPS)
	}
	if m.Stream.P50TTFTMS == nil || *m.Stream.P50TTFTMS != 100 ||
		m.Stream.P50LatencyMS == nil || *m.Stream.P50LatencyMS != 2000 ||
		m.Stream.P50TPS == nil || *m.Stream.P50TPS != 40 {
		t.Errorf("stream p50 = %+v", m.Stream)
	}
	if m.Pricing.Known {
		t.Error("a custom provider reported a published price")
	}
	if m.NonStream.Samples != 0 || m.NonStream.AvgTPS != nil {
		t.Errorf("non_stream = %+v, want empty with null averages", m.NonStream)
	}
}

func TestAdminPerformanceIncludesPricing(t *testing.T) {
	s, _, _ := newAdminTestServerWith(t, false, func(c *config.Config) {
		c.Providers = []config.Provider{{ID: "opencode-go"}}
		c.Connections = []config.Connection{
			{ID: "go", Provider: "ocg", APIKey: "sk"},
			{ID: "go-ant", Provider: "ocga", APIKey: "sk"},
		}
		c.DefaultProvider = "opencode-go"
	}, nil)

	res := do(s, adminRequest(http.MethodGet, "/api/performance", adminTestToken, ""))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	var body struct {
		Data []perfModelDTO `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var chat, anth *perfModelDTO
	for i := range body.Data {
		m := &body.Data[i]
		if m.Model != "qwen4-max" {
			continue
		}
		switch m.Provider {
		case "opencode-go":
			chat = m
		case "opencode-go-anthropic":
			anth = m
		}
	}
	if chat == nil || anth == nil {
		t.Fatalf("qwen4-max missing from one surface: chat=%v ant=%v", chat, anth)
	}
	for _, m := range []*perfModelDTO{chat, anth} {
		if !m.Pricing.Known {
			t.Errorf("%s: pricing unknown", m.Provider)
			continue
		}
		if m.Pricing.Input != 1.5 || m.Pricing.Output != 4.5 ||
			m.Pricing.CacheRead != 0.075 || m.Pricing.CacheWrite != 1.875 {
			t.Errorf("%s: pricing = %+v", m.Provider, m.Pricing)
		}
	}
}

func TestAdminPerformanceConnectionBreakdownAndLowSample(t *testing.T) {
	s, _, _ := newAdminTestServer(t, false)
	s.Perf.RecordConnection("testprov", "test-model", "conn-a", perf.Sample{
		OK: true, Stream: true, TTFT: 100 * time.Millisecond,
		Latency: time.Second, Tokens: 20, TokensKnown: true, TPS: 20,
	})
	s.Perf.RecordConnection("testprov", "test-model", "conn-b", perf.Sample{
		OK: false, Stream: true, Latency: 2 * time.Second,
	})

	res := do(s, adminRequest(http.MethodGet, "/api/performance", adminTestToken, ""))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	var body struct {
		Data []perfModelDTO `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, res.Body)
	}
	if len(body.Data) != 1 {
		t.Fatalf("data = %d rows, want 1", len(body.Data))
	}
	m := body.Data[0]
	if m.Attempts != 2 || m.Successes != 1 || m.Failures != 1 {
		t.Errorf("model totals = %d/%d/%d", m.Attempts, m.Successes, m.Failures)
	}
	if len(m.Connections) != 2 {
		t.Fatalf("connections = %+v, want 2 rows", m.Connections)
	}
	byID := make(map[string]perfConnectionDTO, len(m.Connections))
	for _, c := range m.Connections {
		byID[c.Connection] = c
	}
	a, ok := byID["conn-a"]
	if !ok {
		t.Fatalf("conn-a missing from %+v", m.Connections)
	}
	if a.Attempts != 1 || a.Successes != 1 || a.Failures != 0 {
		t.Errorf("conn-a totals = %d/%d/%d", a.Attempts, a.Successes, a.Failures)
	}
	if !a.Stream.LowSample || a.Stream.Samples != 1 {
		t.Errorf("conn-a stream = %+v, want one low-sample success", a.Stream)
	}
	if b := byID["conn-b"]; b.Attempts != 1 || b.Failures != 1 {
		t.Errorf("conn-b = %+v", b)
	}
	// A single successful sample also marks the model-level average as low.
	if !m.Stream.LowSample {
		t.Errorf("model stream = %+v, want low_sample", m.Stream)
	}
	if m.Stream.AvgTPS == nil || *m.Stream.AvgTPS != 20 {
		t.Errorf("model stream tps = %v", m.Stream.AvgTPS)
	}
}

func TestAdminPerformanceSeriesIsOptIn(t *testing.T) {
	s, _, _ := newAdminTestServer(t, false)
	base := time.Unix(4000, 0)
	s.Perf.Record("testprov", "test-model", perf.Sample{
		At: base, OK: true, Stream: true,
		TTFT: 100 * time.Millisecond, Latency: time.Second,
		Tokens: 20, TokensKnown: true, TPS: 20,
	})
	s.Perf.Record("testprov", "test-model", perf.Sample{
		At: base.Add(time.Second), OK: false, Stream: true, Latency: 2 * time.Second,
	})

	plain := do(s, adminRequest(http.MethodGet, "/api/performance", adminTestToken, ""))
	var plainBody struct {
		Data []perfModelDTO `json:"data"`
	}
	if err := json.Unmarshal(plain.Body.Bytes(), &plainBody); err != nil {
		t.Fatal(err)
	}
	if len(plainBody.Data) != 1 || len(plainBody.Data[0].Series) != 0 {
		t.Errorf("plain series = %+v, want omitted", plainBody.Data)
	}

	withSeries := do(s, adminRequest(http.MethodGet, "/api/performance?series=1", adminTestToken, ""))
	if withSeries.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", withSeries.Code, withSeries.Body)
	}
	var body struct {
		Data []perfModelDTO `json:"data"`
	}
	if err := json.Unmarshal(withSeries.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, withSeries.Body)
	}
	if len(body.Data) != 1 {
		t.Fatalf("data = %d rows, want 1", len(body.Data))
	}
	series := body.Data[0].Series
	if len(series) != 2 {
		t.Fatalf("series = %+v, want 2 samples", series)
	}
	if !series[0].At.Equal(base) || !series[0].OK || series[0].TPS == nil || *series[0].TPS != 20 {
		t.Errorf("first sample = %+v", series[0])
	}
	if series[1].OK || series[1].TPS != nil || series[1].TTFTMS != nil {
		t.Errorf("failed sample = %+v, want null timings", series[1])
	}
}

func TestMetricsExposePerformanceGauges(t *testing.T) {
	s, _, _ := newAdminTestServer(t, false)
	s.Perf.RecordConnection("testprov", "test-model", "conn-a", perf.Sample{
		OK: true, Stream: true, TTFT: 250 * time.Millisecond,
		Latency: 2 * time.Second, Tokens: 50, TokensKnown: true, TPS: 40,
	})

	res := do(s, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	out := res.Body.String()
	for _, want := range []string{
		`lgllm_model_requests_window{provider="testprov",model="test-model"} 1`,
		`lgllm_model_ttft_seconds{provider="testprov",model="test-model",mode="stream"} 0.25`,
		`lgllm_model_tps{provider="testprov",model="test-model",mode="stream"} 40`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics missing %q in:\n%s", want, out)
		}
	}
}

func TestAdminPerformanceListsAdvertisedModelsWithoutSamples(t *testing.T) {
	s, _, _ := newAdminTestServer(t, false)
	// A sample for a model that is not advertised must not leak into the list.
	s.Perf.Record("testprov", "other-model", perf.Sample{OK: true, Stream: false})

	res := do(s, adminRequest(http.MethodGet, "/api/performance", adminTestToken, ""))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	var body struct {
		Data []perfModelDTO `json:"data"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 1 {
		t.Fatalf("data = %d rows, want only the advertised model", len(body.Data))
	}
	if m := body.Data[0]; m.Attempts != 0 || m.Stream.AvgTTFTMS != nil || m.LastSeen != nil {
		t.Errorf("unused model = %+v", m)
	}

	// The optional filter accepts both the provider id and its alias.
	filtered := do(s, adminRequest(http.MethodGet, "/api/performance?provider=nope", adminTestToken, ""))
	var empty struct {
		Data []perfModelDTO `json:"data"`
	}
	if err := json.Unmarshal(filtered.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if len(empty.Data) != 0 {
		t.Errorf("filtered data = %d rows, want 0", len(empty.Data))
	}
}

func TestPerformanceRecordsNonStreamAttempt(t *testing.T) {
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"testprov/test-model","messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}

	agg := findAggregate(t, s.Perf.Snapshot(), "testprov", "test-model")
	if agg.Attempts != 1 || agg.Successes != 1 || agg.Failures != 0 {
		t.Errorf("attempts/successes/failures = %d/%d/%d", agg.Attempts, agg.Successes, agg.Failures)
	}
	if agg.NonStream.Samples != 1 || agg.Stream.Samples != 0 {
		t.Errorf("non-stream/stream samples = %d/%d", agg.NonStream.Samples, agg.Stream.Samples)
	}
	if !agg.NonStream.TokensKnown || !agg.NonStream.TPSKnown || !agg.NonStream.TPSEstimated {
		t.Errorf("non-stream stats = %+v", agg.NonStream)
	}
	if agg.NonStream.AvgTTFT <= 0 || agg.NonStream.AvgLatency <= 0 {
		t.Errorf("non-stream timings = %v/%v", agg.NonStream.AvgTTFT, agg.NonStream.AvgLatency)
	}
	conns := s.Perf.SnapshotConnections()
	if len(conns) != 1 || conns[0].Connection != "test-conn" || conns[0].Attempts != 1 {
		t.Errorf("connection breakdown = %+v, want test-conn", conns)
	}
}

func TestPerformanceRecordsStreamAttempt(t *testing.T) {
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"role":"assistant"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"testprov/test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), `"content":"hi"`) {
		t.Errorf("stream body = %s", res.Body)
	}

	agg := findAggregate(t, s.Perf.Snapshot(), "testprov", "test-model")
	if agg.Attempts != 1 || agg.Successes != 1 {
		t.Errorf("attempts/successes = %d/%d", agg.Attempts, agg.Successes)
	}
	if agg.Stream.Samples != 1 || agg.NonStream.Samples != 0 {
		t.Errorf("stream/non-stream samples = %d/%d", agg.Stream.Samples, agg.NonStream.Samples)
	}
	if !agg.Stream.TokensKnown || agg.Stream.AvgTPS <= 0 {
		t.Errorf("stream stats = %+v, want token-backed TPS", agg.Stream)
	}
	if agg.Stream.AvgTTFT <= 0 || agg.Stream.AvgTTFT > agg.Stream.AvgLatency {
		t.Errorf("TTFT %v vs latency %v", agg.Stream.AvgTTFT, agg.Stream.AvgLatency)
	}
}

func TestPerformanceRecordsFailedAttempt(t *testing.T) {
	_, cfg := mockUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	})
	s := newTestServer(t, cfg, nil)

	res := do(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"testprov/test-model","messages":[{"role":"user","content":"hi"}]}`)))
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the provider error", res.Code)
	}

	agg := findAggregate(t, s.Perf.Snapshot(), "testprov", "test-model")
	if agg.Attempts != 1 || agg.Successes != 0 || agg.Failures != 1 {
		t.Errorf("attempts/successes/failures = %d/%d/%d", agg.Attempts, agg.Successes, agg.Failures)
	}
	if agg.Stream.Samples != 0 || agg.NonStream.Samples != 0 {
		t.Errorf("a failure must not enter the averages: %+v", agg)
	}
}

func TestClientCancelAfterFinishIsSuccess(t *testing.T) {
	s, _, _ := newAdminTestServer(t, false)
	tap := readFinishTap(t,
		`data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n"+
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	provider := &registry.Provider{ID: "testprov"}
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))

	s.finishStream(req, log,
		provider, "test-model", "test-conn", time.Now().Add(-time.Second), 0, tap,
		context.Canceled, tokensaver.Stats{})

	agg := findAggregate(t, s.Perf.Snapshot(), "testprov", "test-model")
	if agg.Attempts != 1 || agg.Successes != 1 || agg.Failures != 0 {
		t.Errorf("attempts/successes/failures = %d/%d/%d, want a completed success",
			agg.Attempts, agg.Successes, agg.Failures)
	}
	if agg.Stream.TokensKnown {
		t.Error("usage arrived after the client left, so tokens must stay unknown")
	}
	if !strings.Contains(logBuf.String(), "client closed after finish") ||
		strings.Contains(logBuf.String(), "stream ended with an error") {
		t.Errorf("unexpected log: %s", logBuf.String())
	}

	var metricsOut strings.Builder
	if err := s.Metrics.Render(&metricsOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(metricsOut.String(),
		`lgllm_client_cancelled_total{provider="testprov",after_finish="true"} 1`) {
		t.Errorf("metrics missing after-finish cancel:\n%s", metricsOut.String())
	}
}

func TestClientCancelBeforeFinishIsNotFailure(t *testing.T) {
	s, _, _ := newAdminTestServer(t, false)
	tap := readFinishTap(t, `data: {"choices":[{"delta":{"content":"hi"}}]}`+"\n\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	provider := &registry.Provider{ID: "testprov"}
	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))

	s.finishStream(req, log,
		provider, "test-model", "test-conn", time.Now().Add(-time.Second), 0, tap,
		context.Canceled, tokensaver.Stats{})

	if agg := s.Perf.Snapshot(); len(agg) != 0 {
		t.Errorf("aborted attempt entered the performance window: %+v", agg)
	}
	if !strings.Contains(logBuf.String(), "client canceled stream") ||
		strings.Contains(logBuf.String(), "stream ended with an error") {
		t.Errorf("unexpected log: %s", logBuf.String())
	}

	var metricsOut strings.Builder
	if err := s.Metrics.Render(&metricsOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(metricsOut.String(),
		`lgllm_client_cancelled_total{provider="testprov",after_finish="false"} 1`) {
		t.Errorf("metrics missing early cancel:\n%s", metricsOut.String())
	}
}

func readFinishTap(t *testing.T, payload string) *perf.SSETap {
	t.Helper()
	tap := perf.NewSSETap(io.NopCloser(strings.NewReader(payload)), registry.FormatChat)
	if _, err := io.ReadAll(tap); err != nil {
		t.Fatalf("read tap: %v", err)
	}
	if err := tap.Close(); err != nil {
		t.Fatalf("close tap: %v", err)
	}
	return tap
}

func findAggregate(t *testing.T, all []perf.Aggregate, provider, model string) perf.Aggregate {
	t.Helper()
	for _, a := range all {
		if a.Provider == provider && a.Model == model {
			return a
		}
	}
	t.Fatalf("no aggregate for %s/%s in %+v", provider, model, all)
	return perf.Aggregate{}
}
