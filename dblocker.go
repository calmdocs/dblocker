package dblocker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// ConnectDBFunc connects to the database identified by driverName and
// dataSourceName and returns a ready-to-use *sqlx.DB.  The id of the
// requesting Group is provided so that implementations can, for example,
// route different ids to different database shards.  statementTimeout, when
// not nil, should be applied to the session where the database supports it.
type ConnectDBFunc func(ctx context.Context, id interface{}, driverName, dataSourceName string, statementTimeout *time.Duration) (db *sqlx.DB, err error)

// Store is the dblocker store.
//
// The id -> *Group map is split across shardCount independently locked
// shards so that requests for different ids do not contend on a single
// store-wide mutex (see shard.go).
type Store struct {
	Ctx context.Context

	shards        [shardCount]shard
	connectDBFunc ConnectDBFunc

	// connSem bounds the total number of concurrent database sessions
	// across all ids when MaxConns > 0 (nil means no limit).  A slot is
	// held from when access for a session is granted until the session's
	// cancel function is called (or its context ends).
	connSem chan struct{}

	DriverName       string
	DataSourceName   string
	UnlockTimeout    *time.Duration
	StatementTimeout *time.Duration

	// MaxConnsPerID caps the number of concurrent database connections in
	// each id's pool (both the shared pool and any separate session opened
	// with RWGetDBWithTimeout / RWGetDBxWithTimeout).  0 means no limit.
	//
	// Within the cap, connections are reused rather than churned: a freed
	// connection is handed directly to any waiting request (database/sql
	// semantics), and otherwise kept for the next request for the same id.
	// The whole pool is closed when the id's last request finishes.
	MaxConnsPerID int

	// MaxConns caps the total number of concurrent database sessions
	// across all ids.  A session is a single sequential unit of database
	// work, as if it were one transaction (an RW session acts like a
	// single exclusive transaction for its id; a read session is one
	// concurrent reader), so with all database access going through
	// dblocker, capping concurrent sessions caps concurrent database
	// connections in use.  A session holds its slot from when access is
	// granted (after any wait for the id's lock) until its cancel
	// function is called.  Requests beyond the cap wait, subject to the
	// request context and the UnlockTimeout.  0 means no limit.
	MaxConns int

	debug bool
}

// Options configures a Store created with NewWithOptions.
type Options struct {
	// ConnectDBFunc connects to the database.  nil uses DefaultConnectDBFunc.
	ConnectDBFunc ConnectDBFunc

	// DriverName is the database/sql driver name (e.g. "sqlite3",
	// "postgres", "mysql", or "mock").
	DriverName string

	// DataSourceName is the driver-specific data source name.
	DataSourceName string

	// UnlockTimeout is the maximum time a request waits for access to an
	// id's database.  nil means wait until the request context is done.
	//
	// The timeout also auto-releases a session's lock when it expires
	// (the escape hatch for a forgotten cancel), but it does not stop a
	// query that is already running — so queries in a session must
	// complete within the UnlockTimeout.  For longer-running work, set
	// UnlockTimeout to nil or bound each query with a per-call context
	// timeout below the UnlockTimeout.
	UnlockTimeout *time.Duration

	// StatementTimeout is the per-session statement timeout, applied where
	// the database supports it (postgres and mysql with the default
	// ConnectDBFunc).  nil disables it.
	StatementTimeout *time.Duration

	// MaxConnsPerID caps the number of concurrent database connections in
	// each id's pool.  Within the cap, connections are reused: freed
	// connections go directly to waiting requests, or are kept for the
	// next request for the same id until the id's last request finishes
	// (which closes the whole pool).  0 means no limit.
	MaxConnsPerID int

	// MaxConns caps the total number of concurrent database sessions
	// across all ids, assuming all database access goes through dblocker
	// (see Store.MaxConns).  0 means no limit.
	MaxConns int

	// Debug enables logging of lock acquisition and a ticker for
	// long-held locks.
	Debug bool
}

// Request is a database access request
type Request struct {
	ctx context.Context
}

// New creates a new dblocker Store
// using the default connectDBFunc;
// with a default unlockTimeout for waiting for access to the database of 2 minutes, and
// with a default statemenTimeout for database sessions of 4 minutes (where the database supports statement timeouts)
func New(
	ctx context.Context,
	driverName string,
	dataSourceName string,
	debug bool,
) (s *Store, err error) {

	// Use default connectDBFunc
	connectDBFunc := DefaultConnectDBFunc

	// Default timeout for waiting for access to the database
	unlockTimeout := 2 * time.Minute
	defaultStatementTimeout := 4 * time.Minute

	// Default statement timeout for database sessions
	var statementTimeout *time.Duration
	switch driverName {
	case "postgres":
		statementTimeout = &defaultStatementTimeout
	case "mysql":
		statementTimeout = &defaultStatementTimeout
	default:
	}

	return NewWithConnectDBFuncAndTimeouts(ctx, connectDBFunc, driverName, dataSourceName, &unlockTimeout, statementTimeout, debug)
}

// NewWithUnlockAndStatementTimeouts creates a new dblocker Store
// using the default connectDBFunc;
// with an unlockTimeout for waiting for access to the database; and
// with a statemenTimeout for database sessions (returns an error if not nil and the database does not support statement timeouts).
func NewWithUnlockAndStatementTimeouts(
	ctx context.Context,
	driverName string,
	dataSourceName string,
	unlockTimeout *time.Duration,
	statementTimeout *time.Duration,
	debug bool,
) (s *Store, err error) {

	// Use default connectDBFunc
	connectDBFunc := DefaultConnectDBFunc

	return NewWithConnectDBFuncAndTimeouts(ctx, connectDBFunc, driverName, dataSourceName, unlockTimeout, statementTimeout, debug)
}

// NewWithConnectDBFuncAndTimeouts creates a new dblocker Store
// with a custom connectDBFunc (which can be used for database types not in the DefaultConnectDBFunc (i.e. sqlite, postgres, and mysql) and/or to shard requests by id for example);
// with an unlockTimeout for waiting for access to the database; and
// with a statemenTimeout for database sessions (returns an error if not nil and the database does not support statement timeouts).
func NewWithConnectDBFuncAndTimeouts(
	ctx context.Context,
	connectDBFunc ConnectDBFunc,
	driverName string,
	dataSourceName string,
	unlockTimeout *time.Duration,
	statementTimeout *time.Duration,
	debug bool,
) (s *Store, err error) {
	return NewWithOptions(ctx, Options{
		ConnectDBFunc:    connectDBFunc,
		DriverName:       driverName,
		DataSourceName:   dataSourceName,
		UnlockTimeout:    unlockTimeout,
		StatementTimeout: statementTimeout,
		Debug:            debug,
	})
}

// NewWithOptions creates a new dblocker Store from Options.
// It is the constructor to use for connection limits: set Options.MaxConns
// (total across all ids) and/or Options.MaxConnsPerID (per id).
// It returns an error if Options.StatementTimeout is not nil and the database
// does not support statement timeouts.
func NewWithOptions(ctx context.Context, opts Options) (s *Store, err error) {

	// Use default connectDBFunc unless a custom one is provided
	connectDBFunc := opts.ConnectDBFunc
	if connectDBFunc == nil {
		connectDBFunc = DefaultConnectDBFunc
	}

	// Return an error if statementTimeout is not nil and the database does not support statement timeouts
	if opts.StatementTimeout != nil {
		switch opts.DriverName {
		case "mock":
			return nil, fmt.Errorf("connectDB error: statementTimeout for database type not implemented: %s", opts.DriverName)
		case "sqlite3":
			return nil, fmt.Errorf("connectDB error: statementTimeout for database type not implemented: %s", opts.DriverName)
		case "postgres":
		case "mysql":
		default:
			return nil, fmt.Errorf("connectDB error: database type not implemented: %s", opts.DriverName)
		}
	}

	if opts.MaxConnsPerID < 0 {
		return nil, fmt.Errorf("dblocker error: MaxConnsPerID must not be negative: %d", opts.MaxConnsPerID)
	}
	if opts.MaxConns < 0 {
		return nil, fmt.Errorf("dblocker error: MaxConns must not be negative: %d", opts.MaxConns)
	}

	s = &Store{
		Ctx:              ctx,
		connectDBFunc:    connectDBFunc,
		DriverName:       opts.DriverName,
		DataSourceName:   opts.DataSourceName,
		UnlockTimeout:    opts.UnlockTimeout,
		StatementTimeout: opts.StatementTimeout,
		MaxConnsPerID:    opts.MaxConnsPerID,
		MaxConns:         opts.MaxConns,
		debug:            opts.Debug,
	}
	if opts.MaxConns > 0 {
		s.connSem = make(chan struct{}, opts.MaxConns)
	}
	for i := range s.shards {
		s.shards[i].m = make(map[interface{}]*Group)
	}
	return s, nil
}

// connectDB dials the database for id using the configured connectDBFunc.
func (s *Store) connectDB(ctx context.Context, id interface{}, statementTimeout *time.Duration) (*sqlx.DB, error) {
	return s.connectDBFunc(ctx, id, s.DriverName, s.DataSourceName, statementTimeout)
}

// applyConnLimitsPerID applies the per-id connection limit (if any) to a
// freshly connected database pool.  The idle limit is set to match the open
// limit so that, within the cap, connections are reused for the id's next
// requests rather than closed and re-dialled between bursts; the whole pool
// is closed when the id's last request finishes.
func (s *Store) applyConnLimitsPerID(db *sqlx.DB) {
	if db == nil {
		return
	}
	if s.MaxConnsPerID > 0 {
		db.SetMaxOpenConns(s.MaxConnsPerID)
		db.SetMaxIdleConns(s.MaxConnsPerID)
	}
}

// RWGetDB returns a shared copy of a database session (*sql.DB) for the specified id.
// RWGetDB acts like Lock() for a RWMutex for the specified id.
// All other RWGetDB, RWGetDBWithTimeout, and ReadDB function calls will wait for access to the database for the specified id until the returned cancel() function is called.
func (s *Store) RWGetDB(id interface{}, ctx context.Context, tag string) (cancel context.CancelFunc, db *sql.DB, err error) {
	cancel, sqlxDB, err := s.waitGetDB(id, "rw", ctx, tag, nil)
	if err != nil {
		return cancel, nil, err
	}
	return cancel, sqlxDB.DB, err
}

// RWGetDB returns a shared copy of a database session (*sqlx.DB) for the specified id.
// github.com/jmoiron/sqlx is a library which provides a set of extensions on go's standard database/sql library.
// RWGetDB acts like Lock() for a RWMutex for the specified id.
// All other RWGetDB, RWGetDBWithTimeout, and ReadDB function calls will wait for access to the database for the specified id until the returned cancel() function is called.
func (s *Store) RWGetDBx(id interface{}, ctx context.Context, tag string) (cancel context.CancelFunc, db *sqlx.DB, err error) {
	return s.waitGetDB(id, "rw", ctx, tag, nil)
}

// RWGetDBWithTimeout returns a new database session (*sql.DB) for the specified id with a custom session timeout.
// RWGetDBWithTimeout acts like Lock() for a RWMutex for the specified id.
// The separate session is closed automatically when the returned cancel() function is called (or when the request context is cancelled).
// All other RWGetDB, RWGetDBWithTimeout, and ReadDB function calls will wait for access to the database for the specified id until the returned cancel() function is called.
func (s *Store) RWGetDBWithTimeout(id interface{}, ctx context.Context, tag string, statementTimeout *time.Duration) (cancel context.CancelFunc, db *sql.DB, err error) {
	cancel, sqlxDB, err := s.waitGetDB(id, "rwseparate", ctx, tag, statementTimeout)
	if err != nil {
		return cancel, nil, err
	}
	return cancel, sqlxDB.DB, err
}

// RWGetDBxWithTimeout returns a new database session (*sqlx.DB) for the specified id with a custom session timeout.
// github.com/jmoiron/sqlx is a library which provides a set of extensions on go's standard database/sql library.
// RWGetDBxWithTimeout acts like Lock() for a RWMutex for the specified id.
// The separate session is closed automatically when the returned cancel() function is called (or when the request context is cancelled).
// All other RWGetDB, RWGetDBWithTimeout, and ReadDB function calls will wait for access to the database for the specified id until the returned cancel() function is called.
func (s *Store) RWGetDBxWithTimeout(id interface{}, ctx context.Context, tag string, statementTimeout *time.Duration) (cancel context.CancelFunc, db *sqlx.DB, err error) {
	return s.waitGetDB(id, "rwseparate", ctx, tag, statementTimeout)
}

// ReadDB returns a shared copy of a database session (*sql.DB) for the specified id.
// ReadDB acts like RLock() for a RWMutex for the specified id.
// Multiple ReadDB function calls can access the shared database at the same time.
// All RWGetDB and RWGetDBWithTimeout function calls will wait for access to the database for the specified id until the returned cancel() function is called.
func (s *Store) ReadGetDB(id interface{}, ctx context.Context, tag string) (cancel context.CancelFunc, db *sql.DB, err error) {
	cancel, sqlxDB, err := s.waitGetDB(id, "read", ctx, tag, nil)
	if err != nil {
		return cancel, nil, err
	}
	return cancel, sqlxDB.DB, err
}

// ReadDB returns a shared copy of a database session (*sqlx.DB) for the specified id.
// github.com/jmoiron/sqlx is a library which provides a set of extensions on go's standard database/sql library.
// ReadDB acts like RLock() for a RWMutex for the specified id.
// Multiple ReadDB function calls can access the shared database at the same time.
// All RWGetDB and RWGetDBWithTimeout function calls will wait for access to the database for the specified id until the returned cancel() function is called.
func (s *Store) ReadGetDBx(id interface{}, ctx context.Context, tag string) (cancel context.CancelFunc, db *sqlx.DB, err error) {
	return s.waitGetDB(id, "read", ctx, tag, nil)
}

func (s *Store) waitGetDB(id interface{}, accessType string, parentCtx context.Context, tag string, statementTimeout *time.Duration) (cancel context.CancelFunc, db *sqlx.DB, err error) {

	// Create context.
	// ctxCancel is a local copy for the goroutine below: the named return
	// value cancel is written by every return statement, which would race
	// with the goroutine reading it.
	var ctx context.Context
	var ctxCancel context.CancelFunc
	if s.UnlockTimeout == nil {
		ctx, ctxCancel = context.WithCancel(parentCtx)
	} else {
		ctx, ctxCancel = context.WithTimeout(parentCtx, *s.UnlockTimeout)
	}
	cancel = ctxCancel

	// Check accessType
	switch accessType {
	case "rw":
	case "rwseparate":
	case "read":
	default:
		ctxCancel()
		return nil, nil, fmt.Errorf("unknown access type error: %s", accessType)
	}

	// Cancel context when done
	go func() {
		if s.debug {
			fmt.Println(fmt.Sprintf("dblocker: %s", accessType), tag)
			tickerCancel := s.ticker(ctx, tag)
			defer tickerCancel()
		}

		select {
		case <-s.Ctx.Done():
			ctxCancel()
		case <-ctx.Done():
			ctxCancel()
		}
	}()

	// Add new Group to the shard map if required.  Only the shard for this
	// id is locked, so requests for ids on other shards proceed in parallel.
	sh := s.shardFor(id)
	sh.Lock()
	g, ok := sh.m[id]
	if !ok {
		g = &Group{
			requestCount:  0,
			rwRequestCh:   make(chan Request),
			readRequestCh: make(chan Request),
			dbCh:          make(chan *sqlx.DB),
		}
		sh.m[id] = g
		go s.startGroup(id, g)
	}

	// Increment request count
	g.requestCount++
	sh.Unlock()

	// Decrement request count when this function returns
	defer func() {
		sh.Lock()
		g.requestCount--
		sh.Unlock()
	}()

	// Send request and wait
	switch accessType {
	case "rw", "rwseparate":
		select {
		case g.rwRequestCh <- Request{ctx: ctx}:
		case <-s.Ctx.Done():
			if cancel != nil {
				cancel()
			}
			return nil, nil, s.Ctx.Err()
		case <-ctx.Done():
			if cancel != nil {
				cancel()
			}
			return nil, nil, ctx.Err()
		}
	case "read":
		select {
		case g.readRequestCh <- Request{ctx: ctx}:
		case <-s.Ctx.Done():
			if cancel != nil {
				cancel()
			}
			return nil, nil, s.Ctx.Err()
		case <-ctx.Done():
			if cancel != nil {
				cancel()
			}
			return nil, nil, ctx.Err()
		}
	default:
		if cancel != nil {
			cancel()
		}
		return nil, nil, fmt.Errorf("unknown access type error: %s", accessType)
	}

	// Reserve a session slot if MaxConns is set.  The slot is acquired
	// only after access for the id is granted (so sessions queued behind
	// a busy id do not consume budget while they wait) and is released
	// when the session's context is done — i.e. when the caller calls
	// cancel, or the UnlockTimeout expires.  Every return path below
	// either calls cancel() or hands cancel to the caller, so the release
	// goroutine always fires.
	if s.connSem != nil {
		select {
		case s.connSem <- struct{}{}:
			go func() {
				<-ctx.Done()
				<-s.connSem
			}()
		case <-s.Ctx.Done():
			if cancel != nil {
				cancel()
			}
			return nil, nil, s.Ctx.Err()
		case <-ctx.Done():
			if cancel != nil {
				cancel()
			}
			return nil, nil, ctx.Err()
		}
	}

	// Get database
	switch accessType {
	case "rwseparate":

		// Get new database connection (immediately)
		db, err = s.connectDB(ctx, id, statementTimeout)
		if err != nil {
			if cancel != nil {
				cancel()
			}
			return nil, nil, err
		}
		s.applyConnLimitsPerID(db)

		// Close the separate session when the request is done.  ctx is
		// also cancelled when s.Ctx is done (see the goroutine above),
		// so this covers store shutdown too.  Without this every
		// RWGetDBWithTimeout call would leak a session pool.
		sepDB := db
		go func() {
			<-ctx.Done()
			sepDB.Close()
		}()
	case "rw", "read":

		// Get shared database connection (wait)
		select {
		case db = <-g.dbCh:
		case <-s.Ctx.Done():
			if cancel != nil {
				cancel()
			}
			return nil, nil, s.Ctx.Err()
		case <-ctx.Done():
			if cancel != nil {
				cancel()
			}
			return nil, nil, ctx.Err()
		}

		// A nil db means the group's database connect failed (see
		// drainFailedGroup) — return the real connect error.
		// Reading g.connectErr without the store lock is safe: it is
		// written once before any nil db is sent on g.dbCh, so the
		// channel receive above orders the read after the write.
		if db == nil {
			if cancel != nil {
				cancel()
			}
			return nil, nil, fmt.Errorf("dblocker connect error: %w", g.connectErr)
		}
	default:
		return nil, nil, fmt.Errorf("unknown access type error: %s", accessType)
	}

	// Return cancelFunc and database
	return cancel, db, nil
}
