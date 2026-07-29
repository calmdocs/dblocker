package dblocker

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestShardIndexStable checks that the same id always maps to the same shard
// for a range of key types.
func TestShardIndexStable(t *testing.T) {
	ids := []interface{}{
		"user-123",
		"",
		int(42),
		int64(42),
		uint64(42),
		int32(-7),
		uint8(255),
		struct{ A, B string }{"a", "b"},
	}
	for _, id := range ids {
		a := shardIndex(id)
		b := shardIndex(id)
		if a != b {
			t.Fatalf("shardIndex not stable for %#v: %d != %d", id, a, b)
		}
	}
}

// TestShardIndexDistribution checks that sequential ids (the common case for
// user ids) spread across shards instead of clustering.
func TestShardIndexDistribution(t *testing.T) {
	seen := make(map[uint32]int)
	n := 10000
	for i := 0; i < n; i++ {
		seen[shardIndex(int64(i))&(shardCount-1)]++
	}
	if len(seen) < shardCount/2 {
		t.Fatalf("sequential int64 ids only reached %d of %d shards", len(seen), shardCount)
	}

	seen = make(map[uint32]int)
	for i := 0; i < n; i++ {
		seen[shardIndex(fmt.Sprintf("user-%d", i))&(shardCount-1)]++
	}
	if len(seen) < shardCount/2 {
		t.Fatalf("sequential string ids only reached %d of %d shards", len(seen), shardCount)
	}
}

// TestManyIDsConcurrent exercises the sharded store with many goroutines
// across many ids at once (run with -race).  Requests for different ids must
// all be served, and per-id RW exclusivity must hold.
func TestManyIDsConcurrent(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	unlockTimeout := 30 * time.Second
	s, err := NewWithUnlockAndStatementTimeouts(parentCtx, "mock", "", &unlockTimeout, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	const (
		numIDs           = 64
		requestsPerID    = 8
		perIDMaxParallel = 1 // RW access must be exclusive per id
	)

	active := make([]int64, numIDs)
	var mu sync.Mutex

	var wg sync.WaitGroup
	errCh := make(chan error, numIDs*requestsPerID)
	for id := 0; id < numIDs; id++ {
		for r := 0; r < requestsPerID; r++ {
			wg.Add(1)
			go func(id int64) {
				defer wg.Done()

				cancel, db, err := s.RWGetDB(id, parentCtx, "many-ids")
				if err != nil {
					errCh <- err
					return
				}
				if db == nil {
					errCh <- fmt.Errorf("nil db for id %d", id)
					return
				}

				mu.Lock()
				active[id]++
				if active[id] > perIDMaxParallel {
					mu.Unlock()
					errCh <- fmt.Errorf("id %d: %d concurrent RW holders", id, active[id])
					cancel()
					return
				}
				mu.Unlock()

				time.Sleep(time.Millisecond)

				mu.Lock()
				active[id]--
				mu.Unlock()

				cancel()
			}(int64(id))
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

// TestReadersConcurrentAcrossIDs checks that read requests for one id and rw
// requests for other ids never block each other.
func TestReadersConcurrentAcrossIDs(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	s, err := New(parentCtx, "mock", "", false)
	if err != nil {
		t.Fatal(err)
	}

	// Hold a long RW lock on id 1
	rwCancel, _, err := s.RWGetDB(int64(1), parentCtx, "holder")
	if err != nil {
		t.Fatal(err)
	}
	defer rwCancel()

	// Requests for other ids must be served promptly regardless
	for id := int64(2); id < 10; id++ {
		ctx, ctxCancel := context.WithTimeout(parentCtx, 5*time.Second)
		cancel, db, err := s.ReadGetDB(id, ctx, "other-id")
		if err != nil {
			ctxCancel()
			t.Fatalf("id %d blocked behind unrelated RW lock: %v", id, err)
		}
		if db == nil {
			ctxCancel()
			t.Fatalf("id %d: nil db", id)
		}
		cancel()
		ctxCancel()
	}
}

// TestMaxOpenConnsPerID checks that the per-id connection limit is applied to
// the shared pool and to separate sessions.
func TestMaxOpenConnsPerID(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	unlockTimeout := 10 * time.Second
	s, err := NewWithOptions(parentCtx, Options{
		DriverName:        "mock",
		DataSourceName:    "",
		UnlockTimeout:     &unlockTimeout,
		MaxOpenConnsPerID: 3,
		MaxIdleConnsPerID: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Shared pool
	cancel, db, err := s.RWGetDB(int64(1), parentCtx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if got := db.Stats().MaxOpenConnections; got != 3 {
		t.Fatalf("shared pool MaxOpenConnections = %d, want 3", got)
	}
	cancel()

	// Separate session (RWGetDBWithTimeout)
	cancel2, db2, err := s.RWGetDBWithTimeout(int64(1), parentCtx, "separate", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := db2.Stats().MaxOpenConnections; got != 3 {
		t.Fatalf("separate session MaxOpenConnections = %d, want 3", got)
	}
	cancel2()
}

// TestNewWithOptionsValidation checks Options validation.
func TestNewWithOptionsValidation(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	if _, err := NewWithOptions(parentCtx, Options{DriverName: "mock", MaxOpenConnsPerID: -1}); err == nil {
		t.Fatal("expected error for negative MaxOpenConnsPerID")
	}
	if _, err := NewWithOptions(parentCtx, Options{DriverName: "mock", MaxIdleConnsPerID: -1}); err == nil {
		t.Fatal("expected error for negative MaxIdleConnsPerID")
	}
	statementTimeout := time.Minute
	if _, err := NewWithOptions(parentCtx, Options{DriverName: "sqlite3", StatementTimeout: &statementTimeout}); err == nil {
		t.Fatal("expected error for statementTimeout on sqlite3")
	}
	if _, err := NewWithOptions(parentCtx, Options{DriverName: "nosuchdb", StatementTimeout: &statementTimeout}); err == nil {
		t.Fatal("expected error for unknown database type")
	}
}
