package dblocker

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
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

// TestMaxConnsPerID checks that concurrent sessions for one id are capped, a
// session beyond the cap waits (and fails when its context ends), sessions
// for other ids are unaffected, and a freed slot admits a waiting session.
func TestMaxConnsPerID(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	unlockTimeout := 10 * time.Second
	s, err := NewWithOptions(parentCtx, Options{
		DriverName:    "mock",
		UnlockTimeout: &unlockTimeout,
		MaxConnsPerID: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Fill both slots for id 1 with concurrent read sessions
	cancel1, _, err := s.ReadGetDB(int64(1), parentCtx, "reader 1")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel1()
	cancel2, _, err := s.ReadGetDB(int64(1), parentCtx, "reader 2")
	if err != nil {
		t.Fatal(err)
	}

	// A third session for id 1 must wait and fail once its context ends
	waitCtx, waitCancel := context.WithTimeout(parentCtx, 500*time.Millisecond)
	_, _, err = s.ReadGetDB(int64(1), waitCtx, "over the cap")
	waitCancel()
	if err == nil {
		t.Fatal("third session for the id was admitted over MaxConnsPerID")
	}

	// Other ids are unaffected
	cancelOther, _, err := s.ReadGetDB(int64(2), parentCtx, "other id")
	if err != nil {
		t.Fatalf("session for another id blocked by MaxConnsPerID: %v", err)
	}
	cancelOther()

	// Releasing a slot admits a new session for id 1
	cancel2()
	deadline := time.Now().Add(5 * time.Second)
	for {
		waitCtx, waitCancel := context.WithTimeout(parentCtx, 500*time.Millisecond)
		cancel3, _, err := s.ReadGetDB(int64(1), waitCtx, "after release")
		waitCancel()
		if err == nil {
			cancel3()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session not admitted after slot release: %v", err)
		}
	}
}

// TestPoolsLeftUntouched checks that dblocker never configures the pools it
// hands out — it only opens, provides, and closes them.  A pool it has not
// touched reports MaxOpenConnections == 0 (unlimited, the database/sql
// default), even when both connection limits are set.
func TestPoolsLeftUntouched(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	unlockTimeout := 10 * time.Second
	s, err := NewWithOptions(parentCtx, Options{
		DriverName:    "mock",
		UnlockTimeout: &unlockTimeout,
		MaxConns:      100,
		MaxConnsPerID: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.MaxConns != 100 {
		t.Fatalf("MaxConns = %d, want 100", s.MaxConns)
	}
	if s.MaxConnsPerID != 3 {
		t.Fatalf("MaxConnsPerID = %d, want 3", s.MaxConnsPerID)
	}
	if cap(s.connSem) != 100 {
		t.Fatalf("connSem cap = %d, want 100", cap(s.connSem))
	}

	// Shared pool
	cancel, db, err := s.RWGetDB(int64(1), parentCtx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if got := db.Stats().MaxOpenConnections; got != 0 {
		t.Fatalf("shared pool was configured: MaxOpenConnections = %d, want 0", got)
	}
	cancel()

	// Separate session (RWGetDBWithTimeout)
	cancel2, db2, err := s.RWGetDBWithTimeout(int64(1), parentCtx, "separate", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := db2.Stats().MaxOpenConnections; got != 0 {
		t.Fatalf("separate session was configured: MaxOpenConnections = %d, want 0", got)
	}
	cancel2()

	// Constructors without MaxConns apply no limit at all
	s2, err := New(parentCtx, "mock", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if s2.MaxConns != 0 || s2.MaxConnsPerID != 0 || s2.connSem != nil {
		t.Fatalf("New applied connection limits: MaxConns=%d MaxConnsPerID=%d", s2.MaxConns, s2.MaxConnsPerID)
	}
}

// TestMaxConns checks that concurrent database sessions across all ids are
// capped, that a session beyond the cap waits, and that it proceeds once a
// slot is released.
func TestMaxConns(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	unlockTimeout := 10 * time.Second
	s, err := NewWithOptions(parentCtx, Options{
		DriverName:    "mock",
		MaxConns:      2,
		UnlockTimeout: &unlockTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Fill both slots with sessions on different ids
	cancel1, _, err := s.RWGetDB(int64(1), parentCtx, "slot 1")
	if err != nil {
		t.Fatal(err)
	}
	cancel2, _, err := s.ReadGetDB(int64(2), parentCtx, "slot 2")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel2()

	// A third session must wait for a slot and fail once its context ends
	waitCtx, waitCancel := context.WithTimeout(parentCtx, 500*time.Millisecond)
	_, _, err = s.RWGetDB(int64(3), waitCtx, "over the cap")
	waitCancel()
	if err == nil {
		t.Fatal("third session was admitted over MaxConns")
	}

	// Releasing a slot must admit a new session
	cancel1()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, ctxCancel := context.WithTimeout(parentCtx, 500*time.Millisecond)
		cancel3, db, err := s.RWGetDB(int64(3), ctx, "after release")
		ctxCancel()
		if err == nil && db != nil {
			cancel3()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session not admitted after slot release: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestNewWithOptionsValidation checks Options validation.
func TestNewWithOptionsValidation(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	if _, err := NewWithOptions(parentCtx, Options{DriverName: "mock", MaxConnsPerID: -1}); err == nil {
		t.Fatal("expected error for negative MaxConnsPerID")
	}
	if _, err := NewWithOptions(parentCtx, Options{DriverName: "mock", MaxConns: -1}); err == nil {
		t.Fatal("expected error for negative MaxConns")
	}
	customConnect := func(ctx context.Context, id interface{}, driverName, dataSourceName string, statementTimeout *time.Duration) (*sqlx.DB, error) {
		return DefaultConnectDBFunc(ctx, id, driverName, dataSourceName, statementTimeout)
	}
	if _, err := NewWithOptions(parentCtx, Options{DriverName: "mock", ConnectDBFunc: customConnect, MaxConns: 1}); err != nil {
		t.Fatalf("MaxConns with a custom ConnectDBFunc must be allowed: %v", err)
	}
	statementTimeout := time.Minute
	if _, err := NewWithOptions(parentCtx, Options{DriverName: "sqlite3", StatementTimeout: &statementTimeout}); err == nil {
		t.Fatal("expected error for statementTimeout on sqlite3")
	}
	if _, err := NewWithOptions(parentCtx, Options{DriverName: "nosuchdb", StatementTimeout: &statementTimeout}); err == nil {
		t.Fatal("expected error for unknown database type")
	}
}

// TestSeparateSessionClosedOnCancel checks that the separate session pool
// opened by RWGetDBWithTimeout is closed once its cancel function is called,
// so separate sessions do not leak connections.
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
