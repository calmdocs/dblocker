package dblocker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// ConnectDBFunc connects to the database for the given id.  The Store calls
// it whenever a new database session is needed: once per active id for the
// shared session, and once per RWGetDBWithTimeout / RWGetDBxWithTimeout call
// for a separate session.  Custom implementations can route different ids to
// different databases (sharding) or support drivers that
// DefaultConnectDBFunc does not know about.
type ConnectDBFunc func(ctx context.Context, id interface{}, driverName, dataSourceName string, statementTimeout *time.Duration) (db *sqlx.DB, err error)

// Store hands out database sessions where access is locked per id, as if
// each id's database were guarded by its own RWMutex.
//
// Internally the id -> Group map is split across ShardCount lock shards
// (lock striping) so that requests for different ids do not contend on a
// single store-wide mutex.
type Store struct {
	Ctx context.Context

	shards        []*storeShard
	shardMask     uint64
	connectDBFunc ConnectDBFunc

	DriverName       string
	DataSourceName   string
	UnlockTimeout    *time.Duration
	StatementTimeout *time.Duration

	// MaxOpenConnsPerID caps the number of open connections in each
	// database session pool handed out for an id (via
	// (*sql.DB).SetMaxOpenConns).  0 means unlimited.
	MaxOpenConnsPerID int

	debug bool
}

// Options configures a Store created with NewWithOptions.
type Options struct {

	// ConnectDBFunc connects to the database.  nil means
	// DefaultConnectDBFunc (which supports the mock, sqlite3, postgres,
	// and mysql drivers).
	ConnectDBFunc ConnectDBFunc

	// DriverName and DataSourceName are passed to ConnectDBFunc.
	DriverName     string
	DataSourceName string

	// UnlockTimeout is the maximum time a request waits for access to an
	// id's database before giving up.  nil means wait until the request
	// context is cancelled.
	UnlockTimeout *time.Duration

	// StatementTimeout is the per-session statement timeout applied when
	// connecting.  nil means no statement timeout.  When ConnectDBFunc is
	// nil (i.e. DefaultConnectDBFunc is used) a non-nil StatementTimeout
	// returns an error for drivers that do not support statement
	// timeouts (mock and sqlite3).
	StatementTimeout *time.Duration

	// MaxOpenConnsPerID caps the number of open connections in each
	// database session pool handed out for an id (via
	// (*sql.DB).SetMaxOpenConns and SetMaxIdleConns).  This bounds the
	// total connections each individual id can hold against the
	// database.  0 means unlimited.
	//
	// Note that RWGetDBWithTimeout / RWGetDBxWithTimeout open a separate,
	// equally-capped session pool that exists only while its exclusive
	// lock is held, so an id using those calls can briefly hold up to
	// twice this many connections.
	MaxOpenConnsPerID int

	// ShardCount is the number of lock shards for the id -> Group map.
	// It is rounded up to a power of two.  0 means DefaultShardCount.
	ShardCount int

	// Debug prints lock acquisition logs and starts a ticker that
	// reports how long each lock has been held.
	Debug bool
}

// Request is a database access request
type Request struct {
	ctx context.Context
}

// New creates a new dblocker Store
// using the default connectDBFunc;
// with a default unlockTimeout for waiting for access to the database of 2 minutes, and
// with a default statementTimeout for database sessions of 4 minutes (where the database supports statement timeouts)
func New(
	ctx context.Context,
	driverName string,
	dataSourceName string,
	debug bool,
) (s *Store, err error) {

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

	return NewWithConnectDBFuncAndTimeouts(ctx, DefaultConnectDBFunc, driverName, dataSourceName, &unlockTimeout, statementTimeout, debug)
}

// NewWithUnlockAndStatementTimeouts creates a new dblocker Store
// using the default connectDBFunc;
// with an unlockTimeout for waiting for access to the database; and
// with a statementTimeout for database sessions (returns an error if not nil and the database does not support statement timeouts).
func NewWithUnlockAndStatementTimeouts(
	ctx context.Context,
	driverName string,
	dataSourceName string,
	unlockTimeout *time.Duration,
	statementTimeout *time.Duration,
	debug bool,
) (s *Store, err error) {
	return NewWithConnectDBFuncAndTimeouts(ctx, DefaultConnectDBFunc, driverName, dataSourceName, unlockTimeout, statementTimeout, debug)
}

// NewWithConnectDBFuncAndTimeouts creates a new dblocker Store
// with a custom connectDBFunc (which can be used for database types not in the DefaultConnectDBFunc (i.e. sqlite, postgres, and mysql) and/or to shard requests by id for example);
// with an unlockTimeout for waiting for access to the database; and
// with a statementTimeout for database sessions (returns an error if not nil and the database does not support statement timeouts).
func NewWithConnectDBFuncAndTimeouts(
	ctx context.Context,
	connectDBFunc ConnectDBFunc,
	driverName string,
	dataSourceName string,
	unlockTimeout *time.Duration,
	statementTimeout *time.Duration,
	debug bool,
) (s *Store, err error) {

	// Return an error if statementTimeout is not nil and the database does not support statement timeouts
	if statementTimeout != nil {
		switch driverName {
		case "mock":
			return nil, fmt.Errorf("connectDB error: statementTimeout for database type not implemented: %s", driverName)
		case "sqlite3":
			return nil, fmt.Errorf("connectDB error: statementTimeout for database type not implemented: %s", driverName)
		case "postgres":
		case "mysql":
		default:
			return nil, fmt.Errorf("connectDB error: database type not implemented: %s", driverName)
		}
	}

	return newStore(ctx, Options{
		ConnectDBFunc:    connectDBFunc,
		DriverName:       driverName,
		DataSourceName:   dataSourceName,
		UnlockTimeout:    unlockTimeout,
		StatementTimeout: statementTimeout,
		Debug:            debug,
	})
}

// NewWithOptions creates a new dblocker Store from an Options struct.  It is
// the most flexible constructor: it exposes every option, including
// MaxOpenConnsPerID (the total database connection limit for each individual
// id) and ShardCount.
//
// When Options.ConnectDBFunc is nil, DefaultConnectDBFunc is used and the
// DriverName / StatementTimeout combination is validated up front.  When a
// custom ConnectDBFunc is provided, no driver validation is performed - the
// custom function is trusted to handle its own drivers and timeouts.
func NewWithOptions(ctx context.Context, opts Options) (s *Store, err error) {
	if opts.ConnectDBFunc == nil {
		if opts.StatementTimeout != nil {
			switch opts.DriverName {
			case "postgres", "mysql":
			case "mock", "sqlite3":
				return nil, fmt.Errorf("connectDB error: statementTimeout for database type not implemented: %s", opts.DriverName)
			default:
				return nil, fmt.Errorf("connectDB error: database type not implemented: %s", opts.DriverName)
			}
		}
		opts.ConnectDBFunc = DefaultConnectDBFunc
	}
	return newStore(ctx, opts)
}

func newStore(ctx context.Context, opts Options) (s *Store, err error) {
	if opts.MaxOpenConnsPerID < 0 {
		return nil, fmt.Errorf("dblocker error: MaxOpenConnsPerID must not be negative: %d", opts.MaxOpenConnsPerID)
	}

	shardCount := opts.ShardCount
	if shardCount <= 0 {
		shardCount = DefaultShardCount
	}
	shardCount = nextPowerOfTwo(shardCount)

	shards := make([]*storeShard, shardCount)
	for i := range shards {
		shards[i] = &storeShard{m: make(map[interface{}]*Group)}
	}

	return &Store{
		Ctx:               ctx,
		shards:            shards,
		shardMask:         uint64(shardCount - 1),
		connectDBFunc:     opts.ConnectDBFunc,
		DriverName:        opts.DriverName,
		DataSourceName:    opts.DataSourceName,
		UnlockTimeout:     opts.UnlockTimeout,
		StatementTimeout:  opts.StatementTimeout,
		MaxOpenConnsPerID: opts.MaxOpenConnsPerID,
		debug:             opts.Debug,
	}, nil
}

// applyConnLimits applies the per-id connection limit to a database session
// pool before it is handed out.
func (s *Store) applyConnLimits(db *sqlx.DB) {
	if s.MaxOpenConnsPerID <= 0 || db == nil || db.DB == nil {
		return
	}
	db.SetMaxOpenConns(s.MaxOpenConnsPerID)
	db.SetMaxIdleConns(s.MaxOpenConnsPerID)
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

// RWGetDBx returns a shared copy of a database session (*sqlx.DB) for the specified id.
// github.com/jmoiron/sqlx is a library which provides a set of extensions on go's standard database/sql library.
// RWGetDBx acts like Lock() for a RWMutex for the specified id.
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

// ReadGetDB returns a shared copy of a database session (*sql.DB) for the specified id.
// ReadGetDB acts like RLock() for a RWMutex for the specified id.
// Multiple ReadGetDB function calls can access the shared database at the same time.
// All RWGetDB and RWGetDBWithTimeout function calls will wait for access to the database for the specified id until the returned cancel() function is called.
func (s *Store) ReadGetDB(id interface{}, ctx context.Context, tag string) (cancel context.CancelFunc, db *sql.DB, err error) {
	cancel, sqlxDB, err := s.waitGetDB(id, "read", ctx, tag, nil)
	if err != nil {
		return cancel, nil, err
	}
	return cancel, sqlxDB.DB, err
}

// ReadGetDBx returns a shared copy of a database session (*sqlx.DB) for the specified id.
// github.com/jmoiron/sqlx is a library which provides a set of extensions on go's standard database/sql library.
// ReadGetDBx acts like RLock() for a RWMutex for the specified id.
// Multiple ReadGetDBx function calls can access the shared database at the same time.
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

	// Add new Group to the shard map if required.  Only this id's shard is
	// locked, so requests for other ids proceed without contention.
	shard := s.shardFor(id)
	shard.mu.Lock()
	g, ok := shard.m[id]
	if !ok {
		g = &Group{
			requestCount:  0,
			rwRequestCh:   make(chan Request),
			readRequestCh: make(chan Request),
			dbCh:          make(chan *sqlx.DB),
		}
		shard.m[id] = g
		go s.startGroup(shard, id, g)
	}

	// Increment request count
	g.requestCount++
	shard.mu.Unlock()

	// Decrement request count when this function returns
	defer func() {
		shard.mu.Lock()
		g.requestCount--
		shard.mu.Unlock()
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

	// Get database
	switch accessType {
	case "rwseparate":

		// Get new database connection (immediately)
		db, err = s.connectDBFunc(ctx, id, s.DriverName, s.DataSourceName, statementTimeout)
		if err != nil {
			if cancel != nil {
				cancel()
			}
			return nil, nil, err
		}
		s.applyConnLimits(db)

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
		// Reading g.connectErr without the shard lock is safe: it is
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
