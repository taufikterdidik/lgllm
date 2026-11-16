package perf

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStorePersistsRecentSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "performance.db")
	store, err := OpenStore(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewWithStore(3, store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-5 * time.Second)
	for i := 0; i < 5; i++ {
		registry.RecordConnection("p", "m", "conn-a", Sample{
			At:          base.Add(time.Duration(i) * time.Second),
			OK:          true,
			Stream:      true,
			TTFT:        time.Duration(10+i) * time.Millisecond,
			Latency:     time.Duration(100+i) * time.Millisecond,
			Tokens:      10 + i,
			TokensKnown: true,
			TPS:         float64(20 + i),
		})
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenStore(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	registry, err = NewWithStore(3, store)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()

	series := registry.Series("p", "m")
	if len(series) != 3 {
		t.Fatalf("series = %d samples, want the ring size 3", len(series))
	}
	if got, want := series[0].At, base.Add(2*time.Second); !got.Equal(want) {
		t.Errorf("oldest loaded sample = %v, want %v", got, want)
	}
	if got, want := series[2].TPS, float64(24); got != want {
		t.Errorf("newest TPS = %v, want %v", got, want)
	}

	agg := registry.Snapshot()
	if len(agg) != 1 || agg[0].Attempts != 3 || agg[0].Successes != 3 {
		t.Errorf("model aggregate after reload = %+v", agg)
	}
	conns := registry.SnapshotConnections()
	if len(conns) != 1 || conns[0].Connection != "conn-a" || conns[0].Attempts != 3 {
		t.Errorf("connection aggregate after reload = %+v", conns)
	}
}

func TestStoreDisabledWithoutPath(t *testing.T) {
	var store *Store
	if _, err := OpenStore("", time.Hour); err == nil {
		t.Fatal("OpenStore accepted an empty path")
	}
	if store.Dropped() != 0 {
		t.Error("nil store dropped count is not zero")
	}
}
