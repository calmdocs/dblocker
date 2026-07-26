package dblocker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

func TestDBLocker(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	for i := 0; i <= 10; i++ {
		err := singleTest(parentCtx, i)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func singleTest(parentCtx context.Context, i int) (err error) {
	fmt.Printf("test %d of %d \n\n", i, 10)

	// mock database
	driverName := "mock"
	//driverName := "sqlite3"
	//dbName := ""
	//dataSource := ":memory:"
	dataSource := filepath.Join("testdata", "test.db")
	debug := true
	tag := "test"
	id := int64(0)
	defer os.Remove(dataSource)

	s, err := New(parentCtx, driverName, dataSource, debug)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("not nil")
	}

	// RWGetDB
	cancel, db, err2 := s.RWGetDB(id, parentCtx, tag)
	if err2 != nil {
		return err2
	}
	if db == nil {
		return fmt.Errorf("db nil")
	}

	<-time.After(1 * time.Second)
	cancel()

	// Check that ReadGetDB does not block
	cancel2, db2, err3 := s.ReadGetDB(id, parentCtx, tag)
	if err3 != nil {
		return err3
	}
	if db2 == nil {
		return fmt.Errorf("db nil")
	}

	// Cancel RWGetDB
	cancel2()

	// RWGetDB then immediately cancel
	cancel3, db3, err := s.RWGetDB(id, parentCtx, tag)
	if err != nil {
		return err
	}
	if db3 == nil {
		return fmt.Errorf("not nil")
	}
	cancel3()

	// Check that multiple ReadGetDBs do not block
	for i := 1; i <= 10; i++ {
		cancel, err := testRead(parentCtx, s, id, tag)
		if err != nil {
			return err
		}
		defer cancel()
	}

	return nil
}

func testRead(parentCtx context.Context, s *Store, id int64, tag string) (context.CancelFunc, error) {
	cancel, db, err := s.ReadGetDB(id, parentCtx, tag)
	if err != nil {
		return nil, err
	}
	//defer cancel()
	if db == nil {
		return nil, fmt.Errorf("nil db")
	}
	return cancel, nil
}

// TestConnectFailureSurfacesError checks that a failing database connect
// returns the real connect error to waiters (rather than retrying forever),
// and that the group is deleted afterwards so a later request reconnects.
func TestConnectFailureSurfacesError(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	connectErr := errors.New("unable to open database file")
	var failing atomic.Bool
	failing.Store(true)
	connectDBFunc := func(ctx context.Context, id interface{}, driverName, dataSourceName string, statementTimeout *time.Duration) (*sqlx.DB, error) {
		if failing.Load() {
			return nil, connectErr
		}
		return DefaultConnectDBFunc(ctx, id, driverName, dataSourceName, statementTimeout)
	}

	unlockTimeout := 2 * time.Second
	s, err := NewWithConnectDBFuncAndTimeouts(parentCtx, connectDBFunc, "mock", "", &unlockTimeout, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, _, err = s.RWGetDB(int64(0), parentCtx, "failing")
	if err == nil {
		t.Fatal("expected connect error, got nil")
	}
	if !errors.Is(err, connectErr) {
		t.Fatalf("expected wrapped connect error, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > unlockTimeout+time.Second {
		t.Fatalf("connect failure took too long to surface: %s", elapsed)
	}

	// The failed group must be deleted so the next request reconnects
	failing.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		cancel, db, err := s.RWGetDB(int64(0), parentCtx, "recovered")
		if err == nil && db != nil {
			cancel()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("store did not recover after connect failure cleared: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestConnectFailureDoesNotWedgeStore checks that while one id's database
// connect is failing (and retrying), requests for a different id are served
// normally.  The old code connected while holding the store-wide mutex, so a
// failing connect blocked every waitGetDB call — for any id — indefinitely.
func TestConnectFailureDoesNotWedgeStore(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	badID := int64(1)
	goodID := int64(2)
	connectDBFunc := func(ctx context.Context, id interface{}, driverName, dataSourceName string, statementTimeout *time.Duration) (*sqlx.DB, error) {
		if id == badID {
			return nil, errors.New("unable to open database file")
		}
		return DefaultConnectDBFunc(ctx, id, driverName, dataSourceName, statementTimeout)
	}

	unlockTimeout := 30 * time.Second // long, so the bad id is still mid-retry during the check below
	s, err := NewWithConnectDBFuncAndTimeouts(parentCtx, connectDBFunc, "mock", "", &unlockTimeout, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	// Start the failing request in the background and give startGroup time
	// to enter its connect retry loop
	badErrCh := make(chan error, 1)
	go func() {
		_, _, err := s.RWGetDB(badID, parentCtx, "failing")
		badErrCh <- err
	}()
	time.Sleep(500 * time.Millisecond)

	// A different id must connect promptly while the bad id is retrying
	goodCtx, goodCancel := context.WithTimeout(parentCtx, 5*time.Second)
	defer goodCancel()
	cancel, db, err := s.RWGetDB(goodID, goodCtx, "good")
	if err != nil {
		t.Fatalf("good id blocked or failed while bad id was connecting: %v", err)
	}
	if db == nil {
		t.Fatal("good id returned nil db")
	}
	cancel()

	// The failing request must eventually error out rather than hang
	select {
	case err := <-badErrCh:
		if err == nil {
			t.Fatal("expected connect error for bad id, got nil")
		}
	case <-time.After(unlockTimeout + 5*time.Second):
		t.Fatal("bad id request never returned")
	}
}
