package perf

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	// DefaultRetention is how long historical performance samples are kept when
	// metrics.performance_retention is not set.
	DefaultRetention = 7 * 24 * time.Hour

	storeModelsBucket = "models"
	storeMetaBucket   = "meta"
	storeVersionKey   = "version"
	storeVersion      = 1
	storeQueueSize    = 4096
	storeBatchLimit   = 200
	storeFlushEvery   = 200 * time.Millisecond
	storePruneEvery   = 50 // batches
	// maxSamplesPerKey bounds a single provider/model/connection even at very
	// high request rates; retention alone is not a memory bound.
	maxSamplesPerKey = 2000
)

// storedSample is the on-disk shape. It embeds Sample so adding a future field
// to Sample is backward-compatible for old rows (zero value on decode).
type storedSample struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Connection string `json:"connection,omitempty"`
	Sample     Sample `json:"sample"`
}

// Store persists performance samples in bbolt. Writes are buffered and batched
// in a background goroutine; the in-memory ring remains the read path, so a
// slow disk never blocks a request.
type Store struct {
	db        *bolt.DB
	retention time.Duration
	ch        chan storedSample
	done      chan struct{}
	dropped   atomic.Uint64

	mu      sync.RWMutex
	closed  bool
	batches int
	errMu   sync.Mutex
	lastErr error
}

// OpenStore opens or creates the database at path. An empty path is an error;
// callers that want persistence disabled should not call OpenStore.
func OpenStore(path string, retention time.Duration) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("performance database path is empty")
	}
	if retention <= 0 {
		retention = DefaultRetention
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create performance database directory: %w", err)
		}
	}

	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("open performance database %s: %w", path, err)
	}
	store := &Store{
		db:        db,
		retention: retention,
		ch:        make(chan storedSample, storeQueueSize),
		done:      make(chan struct{}),
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists([]byte(storeModelsBucket)); err != nil {
			return err
		}
		meta, err := tx.CreateBucketIfNotExists([]byte(storeMetaBucket))
		if err != nil {
			return err
		}
		raw := meta.Get([]byte(storeVersionKey))
		if raw == nil {
			return meta.Put([]byte(storeVersionKey), []byte{storeVersion})
		}
		if len(raw) == 1 && int(raw[0]) > storeVersion {
			return fmt.Errorf("performance database schema %d is newer than supported %d", raw[0], storeVersion)
		}
		return nil
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialise performance database: %w", err)
	}
	if err := store.prune(); err != nil {
		db.Close()
		return nil, fmt.Errorf("prune performance database: %w", err)
	}
	go store.run()
	return store, nil
}

// Enqueue adds one sample to the write-behind queue. It never blocks: when the
// queue is full, the sample is dropped and Dropped reports the loss.
func (s *Store) Enqueue(provider, model, connection string, sample Sample) {
	if s == nil {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return
	}
	record := storedSample{
		Provider:   provider,
		Model:      model,
		Connection: connection,
		Sample:     sample,
	}
	select {
	case s.ch <- record:
	default:
		s.dropped.Add(1)
	}
}

// Dropped reports how many samples were lost because the write queue was full.
func (s *Store) Dropped() uint64 {
	if s == nil {
		return 0
	}
	return s.dropped.Load()
}

// LoadRecent returns the latest limit samples for every key, sorted oldest
// first. It is used once at startup to repopulate the in-memory rings.
func (s *Store) LoadRecent(limit int) ([]storedSample, error) {
	if s == nil || limit <= 0 {
		return nil, nil
	}
	var out []storedSample
	err := s.db.View(func(tx *bolt.Tx) error {
		models := tx.Bucket([]byte(storeModelsBucket))
		if models == nil {
			return nil
		}
		return models.ForEachBucket(func(name []byte) error {
			bucket := models.Bucket(name)
			if bucket == nil {
				return nil
			}
			recent := make([]storedSample, 0, limit)
			cursor := bucket.Cursor()
			for key, raw := cursor.Last(); key != nil && len(recent) < limit; key, raw = cursor.Prev() {
				var record storedSample
				if json.Unmarshal(raw, &record) != nil {
					continue
				}
				recent = append(recent, record)
			}
			for i := len(recent) - 1; i >= 0; i-- {
				out = append(out, recent[i])
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Sample.At.Before(out[j].Sample.At)
	})
	return out, nil
}

// Close drains queued samples, prunes old rows, and closes the database.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.ch)
	s.mu.Unlock()

	<-s.done
	if err := s.prune(); err != nil {
		s.setError(err)
	}
	err := s.db.Close()
	s.errMu.Lock()
	lastErr := s.lastErr
	s.errMu.Unlock()
	return errorsJoin(lastErr, err)
}

func (s *Store) run() {
	defer close(s.done)
	batch := make([]storedSample, 0, storeBatchLimit)
	ticker := time.NewTicker(storeFlushEvery)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.writeBatch(batch); err != nil {
			s.setError(err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case record, ok := <-s.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, record)
			if len(batch) >= storeBatchLimit {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (s *Store) writeBatch(batch []storedSample) error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		models, err := tx.CreateBucketIfNotExists([]byte(storeModelsBucket))
		if err != nil {
			return err
		}
		for _, record := range batch {
			key := keyFor(record.Provider, record.Model, record.Connection)
			bucket, err := models.CreateBucketIfNotExists(key)
			if err != nil {
				return err
			}
			raw, err := json.Marshal(record)
			if err != nil {
				return err
			}
			sequence, err := bucket.NextSequence()
			if err != nil {
				return err
			}
			var sampleKey [8]byte
			binary.BigEndian.PutUint64(sampleKey[:], sequence)
			if err := bucket.Put(sampleKey[:], raw); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.batches++
	if s.batches%storePruneEvery == 0 {
		return s.prune()
	}
	return nil
}

func (s *Store) prune() error {
	if s == nil || s.db == nil || s.retention <= 0 {
		return nil
	}
	cutoff := time.Now().Add(-s.retention)
	return s.db.Update(func(tx *bolt.Tx) error {
		models := tx.Bucket([]byte(storeModelsBucket))
		if models == nil {
			return nil
		}
		var empty [][]byte
		err := models.ForEachBucket(func(name []byte) error {
			bucket := models.Bucket(name)
			if bucket == nil {
				return nil
			}
			var deleteKeys [][]byte
			count := 0
			cursor := bucket.Cursor()
			for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
				count++
				var record storedSample
				expired := json.Unmarshal(raw, &record) != nil
				if !expired && s.retention > 0 && record.Sample.At.Before(cutoff) {
					expired = true
				}
				if expired || count > maxSamplesPerKey {
					deleteKeys = append(deleteKeys, append([]byte(nil), key...))
				}
			}
			for _, key := range deleteKeys {
				if err := bucket.Delete(key); err != nil {
					return err
				}
			}
			if bucket.Stats().KeyN == 0 {
				empty = append(empty, append([]byte(nil), name...))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, name := range empty {
			if err := models.DeleteBucket(name); err != nil && err != bolt.ErrBucketNotFound {
				return err
			}
		}
		return nil
	})
}

func (s *Store) setError(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	if s.lastErr == nil {
		s.lastErr = err
	}
	s.errMu.Unlock()
}

func keyFor(provider, model, connection string) []byte {
	return []byte(provider + "\x00" + model + "\x00" + connection)
}

func errorsJoin(first, second error) error {
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	default:
		return fmt.Errorf("%v; %w", first, second)
	}
}
