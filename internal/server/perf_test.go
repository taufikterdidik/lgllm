package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lgllm/internal/perf"
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
	if m.NonStream.Samples != 0 || m.NonStream.AvgTPS != nil {
		t.Errorf("non_stream = %+v, want empty with null averages", m.NonStream)
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
