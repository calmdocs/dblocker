package dblocker

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// Store is the dblocker store
type Store struct {
	sync.Mutex

	Ctx context.Context

	m             map[interface{}]*Group
	connectDBFunc func(ctx context.Context, id interface{}, driverName, dataSourceName string, statementTimeout *time.Duration) (db *sqlx.DB, err error)

	DriverName       string
	DataSourceName   string
	UnlockTimeout    *time.Duration
	StatementTimeout *time.Duration
	debug            bool
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
	connectDBFunc func(ctx context.Context, id interface{}, driverName, dataSourceName string, statementTimeout *time.Duration) (db *sqlx.DB, err error),
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

	return &Store{
		Ctx:              ctx,
		m:                make(map[interface{}]*Group),
		connectDBFunc:    connectDBFunc,
		DriverName:       driverName,
		DataSourceName:   dataSourceName,
		UnlockTimeout:    unlockTimeout,
		StatementTimeout: statementTimeout,
		debug:            debug,
	}, nil
}

// RWGetDB returns a shared copy of a database session (*sql.DB) for the specified id.
// RWGetDB acts like Lock() for a RWMutex for the specified id.
// All other RWGetDB, RWGetDBWithTimeout, and ReadDB function calls will wait for access to the database for the specified id until the returned cancel() function is called.
func (s *Store) RWGetDB(id interface{}, ctx context.Context, tag string) (cancel context.CancelFunc, db *sql.DB, err error) {
	cancel, sqlxDB, err := s.waitGetDB(id, "rw", ctx, tag, nil)
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
// All other RWGetDB, RWGetDBWithTimeout, and ReadDB function calls will wait for access to the database for the specified id until the returned cancel() function is called.
func (s *Store) RWGetDBWithTimeout(id interface{}, ctx context.Context, tag string, statementTimeout *time.Duration) (cancel context.CancelFunc, db *sql.DB, err error) {
	cancel, sqlxDB, err := s.waitGetDB(id, "rwseparate", ctx, tag, statementTimeout)
	return cancel, sqlxDB.DB, err
}

// RWGetDBWithTimeout returns a new database session (*sqlx.DB) for the specified id with a custom session timeout.
// github.com/jmoiron/sqlx is a library which provides a set of extensions on go's standard database/sql library.
// RWGetDBWithTimeout acts like Lock() for a RWMutex for the specified id.
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

	// Create context
	var ctx context.Context
	if s.UnlockTimeout == nil {
		ctx, cancel = context.WithCancel(parentCtx)
	} else {
		ctx, cancel = context.WithTimeout(parentCtx, *s.UnlockTimeout)
	}

	// Check accessType
	switch accessType {
	case "rw":
	case "rwseparate":
	case "read":
	default:
		if cancel != nil {
			cancel()
		}
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
			if cancel != nil {
				cancel()
			}
		case <-ctx.Done():
			if cancel != nil {
				cancel()
			}
		}
	}()

	// Add new Group to the Store map if required
	s.Lock()
	g, ok := s.m[id]
	if !ok {
		s.m[id] = &Group{
			requestCount: 0,
			//DB:		nil,
			rwRequestCh:   make(chan Request),
			readRequestCh: make(chan Request),
			dbCh:          make(chan *sqlx.DB),
		}
		g = s.m[id]
		go s.startGroup(id, g)
	}

	// Increment request count
	s.m[id].requestCount++
	s.Unlock()

	// Decrement request count when this function returns
	defer func() {
		s.Lock()
		s.m[id].requestCount--
		s.Unlock()
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
	default:
		return nil, nil, fmt.Errorf("unknown access type error: %s", accessType)
	}

	// Return cancelFunc and database
	return cancel, db, nil
}
