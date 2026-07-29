package dblocker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestShardForConsistency checks that equal ids always hash to the same
// shard and that different ids spread across more than one shard.
func TestShardForConsistency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := NewWithOptions(ctx, Options{DriverName: "mock"})
	if err != nil {
		t.Fatal(err)
	}

	ids := []interface{}{
		"user-1", "user-2", "",
		int(42), int64(42), int32(7), uint64(7),
		struct{ A, B string }{"a", "b"},
	}
	for _, id := range ids {
		a := s.shardFor(id)
		b := s.shardFor(id)
		if a != b {
			t.Fatalf("id %v hashed to two different shards", id)
		}
	}

	used := make(map[*storeShard]bool)
	for i := 0; i < 1000; i++ {
		used[s.shardFor(fmt.Sprintf("user-%d", i))] = true
	}
	if len(used) < 2 {
		t.Fatalf("1000 ids all hashed to %d shard(s)", len(used))
	}
}

// TestShardCountRounding checks that ShardCount is rounded up to a power of
// two so masking is valid.
func TestShardCountRounding(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, tc := range []struct{ in, want int }{
		{0, DefaultShardCount},
		{1, 1},
		{3, 4},
		{64, 64},
		{100, 128},
	} {
		s, err := NewWithOptions(ctx, Options{DriverName: "mock", ShardCount: tc.in})
		if err != nil {
			t.Fatal(err)
		}
		if len(s.shards) != tc.want {
			t.Fatalf("ShardCount %d: got %d shards, want %d", tc.in, len(s.shards), tc.want)
		}
	}
}

// TestConcurrentIDs exercises many ids concurrently so the race detector can
// check the sharded map, request counting, and group lifecycle.
func TestConcurrentIDs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	unlockTimeout := 30 * time.Second
	s, err := NewWithOptions(ctx, Options{
		DriverName:    "mock",
		UnlockTimeout: &unlockTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("user-%d", i%10)
			for j := 0; j < 5; j++ {
				cancelDB, db, err := s.RWGetDB(id, ctx, "concurrent rw")
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				if db == nil {
					select {
					case errCh <- fmt.Errorf("nil db for id %s", id):
					default:
					}
					return
				}
				cancelDB()

				cancelDB, db, err = s.ReadGetDB(id, ctx, "concurrent read")
				if err != nil {
					select {
					case errCh <- err:
					default:
					}
					return
				}
				if db == nil {
					select {
					case errCh <- fmt.Errorf("nil db for id %s", id):
					default:
					}
					return
				}
				cancelDB()
			}
		}(i)
	}
	wg.Wait()

	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
}

// TestMaxOpenConnsPerID checks that the per-id connection limit is applied
// to both the shared session pool and separate RWGetDBWithTimeout pools.
func TestMaxOpenConnsPerID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	limit := 3
	s, err := NewWithOptions(ctx, Options{
		DriverName:        "mock",
		MaxOpenConnsPerID: limit,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Shared session pool
	cancelDB, db, err := s.RWGetDB("user-1", ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if got := db.Stats().MaxOpenConnections; got != limit {
		t.Fatalf("shared pool MaxOpenConnections = %d, want %d", got, limit)
	}
	cancelDB()

	// Separate session pool
	cancelDB, db, err = s.RWGetDBWithTimeout("user-1", ctx, "separate", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := db.Stats().MaxOpenConnections; got != limit {
		t.Fatalf("separate pool MaxOpenConnections = %d, want %d", got, limit)
	}
	cancelDB()
}

// TestMaxOpenConnsPerIDValidation checks that a negative limit is rejected.
func TestMaxOpenConnsPerIDValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := NewWithOptions(ctx, Options{DriverName: "mock", MaxOpenConnsPerID: -1})
	if err == nil {
		t.Fatal("expected error for negative MaxOpenConnsPerID, got nil")
	}
}

// TestSeparateSessionClosedOnCancel checks that the session pool opened by
// RWGetDBWithTimeout is closed when its cancel function is called, so
// repeated calls do not leak connections.
func TestSeparateSessionClosedOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := NewWithOptions(ctx, Options{DriverName: "mock"})
	if err != nil {
		t.Fatal(err)
	}

	cancelDB, db, err := s.RWGetDBWithTimeout("user-1", ctx, "separate", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("separate session unusable before cancel: %v", err)
	}
	cancelDB()

	// The close runs in a goroutine triggered by the cancelled context, so
	// poll briefly for the pool to report closed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := db.Ping()
		if err != nil && strings.Contains(err.Error(), "closed") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("separate session still open after cancel (ping err: %v)", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestCustomConnectDBFuncSkipsDriverValidation checks that NewWithOptions
// does not reject unknown drivers when a custom ConnectDBFunc is provided.
func TestCustomConnectDBFuncSkipsDriverValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	statementTimeout := time.Minute
	_, err := NewWithOptions(ctx, Options{
		ConnectDBFunc:    DefaultConnectDBFunc,
		DriverName:       "customdriver",
		StatementTimeout: &statementTimeout,
	})
	if err != nil {
		t.Fatalf("custom ConnectDBFunc should skip driver validation: %v", err)
	}
}
