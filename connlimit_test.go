package dblocker

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// stubDriver is a minimal database/sql driver that counts its live physical
// connections, so tests can verify exactly how many connections the limiter
// allowed at once.
type stubDriver struct {
	mu      sync.Mutex
	open    int
	maxOpen int
	fail    bool
}

func (d *stubDriver) Open(name string) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fail {
		return nil, errors.New("stub connect failure")
	}
	d.open++
	if d.open > d.maxOpen {
		d.maxOpen = d.open
	}
	return &stubConn{d: d}, nil
}

func (d *stubDriver) stats() (open, maxOpen int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.open, d.maxOpen
}

type stubConn struct{ d *stubDriver }

func (c *stubConn) Prepare(query string) (driver.Stmt, error) { return &stubStmt{}, nil }
func (c *stubConn) Begin() (driver.Tx, error)                 { return nil, errors.New("not implemented") }
func (c *stubConn) Close() error {
	c.d.mu.Lock()
	c.d.open--
	c.d.mu.Unlock()
	return nil
}

type stubStmt struct{}

func (s *stubStmt) Close() error  { return nil }
func (s *stubStmt) NumInput() int { return 0 }
func (s *stubStmt) Exec(args []driver.Value) (driver.Result, error) {
	return driver.RowsAffected(0), nil
}
func (s *stubStmt) Query(args []driver.Value) (driver.Rows, error) { return &stubRows{}, nil }

type stubRows struct{}

func (r *stubRows) Columns() []string              { return []string{} }
func (r *stubRows) Close() error                   { return nil }
func (r *stubRows) Next(dest []driver.Value) error { return io.EOF }

var (
	stub         = &stubDriver{}
	stubFailing  = &stubDriver{fail: true}
	registerStub sync.Once
)

func registerStubDrivers() {
	registerStub.Do(func() {
		sql.Register("dblocker_stub", stub)
		sql.Register("dblocker_stub_failing", stubFailing)
	})
}

// TestConnLimiterCapsTotalConnections checks that a shared connLimiter caps
// the number of physical connections, hands freed capacity to waiters, and
// counts across pools.
func TestConnLimiterCapsTotalConnections(t *testing.T) {
	registerStubDrivers()
	ctx := context.Background()

	limiter := newConnLimiter(2)
	db, err := openLimitedDB("dblocker_stub", "", limiter)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(0) // close (and release) connections as soon as they are returned

	// Check out both slots
	c1, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// A third physical connection must block until a slot frees
	shortCtx, shortCancel := context.WithTimeout(ctx, 300*time.Millisecond)
	_, err = db.Conn(shortCtx)
	shortCancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded over the cap, got: %v", err)
	}

	// Closing one connection must admit the next
	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}
	c3, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("connection not admitted after a close: %v", err)
	}

	if err := c2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c3.Close(); err != nil {
		t.Fatal(err)
	}

	// A second pool sharing the limiter counts against the same budget
	db2, err := openLimitedDB("dblocker_stub", "", limiter)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	db2.SetMaxIdleConns(0)

	d1, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := db2.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	shortCtx2, shortCancel2 := context.WithTimeout(ctx, 300*time.Millisecond)
	_, err = db2.Conn(shortCtx2)
	shortCancel2()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded across pools, got: %v", err)
	}
	d1.Close()
	d2.Close()

	if _, maxOpen := stub.stats(); maxOpen > 2 {
		t.Fatalf("driver saw %d concurrent connections, cap is 2", maxOpen)
	}
}

// TestConnLimiterReleasesSlotOnConnectFailure checks that a failed dial does
// not leak a limiter slot.
func TestConnLimiterReleasesSlotOnConnectFailure(t *testing.T) {
	registerStubDrivers()
	ctx := context.Background()

	limiter := newConnLimiter(1)
	db, err := openLimitedDB("dblocker_stub_failing", "", limiter)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i := 0; i < 3; i++ {
		if err := db.PingContext(ctx); err == nil {
			t.Fatal("expected connect failure from failing stub driver")
		}
	}
	if len(limiter.sem) != 0 {
		t.Fatalf("limiter leaked %d slot(s) after failed connects", len(limiter.sem))
	}
}

// TestConnLimiterReuseKeepsBudget checks that idle connections are reused
// (no re-dial) rather than consuming additional budget.
func TestConnLimiterReuseKeepsBudget(t *testing.T) {
	registerStubDrivers()
	ctx := context.Background()

	limiter := newConnLimiter(1)
	db, err := openLimitedDB("dblocker_stub", "", limiter)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	before, _ := stub.stats()
	for i := 0; i < 5; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Close(); err != nil { // returns the conn to the idle pool
			t.Fatal(err)
		}
	}
	after, _ := stub.stats()
	if after-before > 1 {
		t.Fatalf("expected a single reused connection, saw %d live", after-before)
	}
}
