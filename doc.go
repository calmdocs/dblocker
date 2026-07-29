// Package dblocker locks a shared database session for each "user" or "id"
// behind what is effectively a per-id RWMutex.
//
// # Why
//
// dblocker allows:
//
//   - simple access to sqlite without worrying about crashes due to
//     concurrent reads and writes from multiple database sessions;
//   - a simple mechanism to ensure that only one "user" or "id" writes to
//     the database at any time, as if access for that id is locked behind a
//     RWMutex;
//   - multiple concurrent database reads (e.g. SELECT requests) per id
//     without also requiring a session pooler such as pgbouncer;
//   - multiple sql commands (and other go code) to run for an id without
//     worrying about concurrent access for that id, and without needing to
//     run every command inside one database transaction.
//
// # Access model
//
// Each id is served by its own shared database session (a connection pool).
// Three kinds of access are available:
//
//   - RWGetDB / RWGetDBx: exclusive access, like Lock() on a RWMutex.  All
//     other requests for the same id wait until the returned cancel function
//     is called.
//   - ReadGetDB / ReadGetDBx: shared access, like RLock() on a RWMutex.
//     Any number of read requests for an id run concurrently; writers wait.
//   - RWGetDBWithTimeout / RWGetDBxWithTimeout: exclusive access on a brand
//     new session with a custom statement timeout, for one-off long-running
//     statements.
//
// Every accessor returns a cancel function.  Calling it releases the lock
// for that id; deferring it immediately after a successful call is the
// expected pattern:
//
//	cancelDB, db, err := store.RWGetDBx(userID, ctx, "update files")
//	if err != nil {
//		return err
//	}
//	defer cancelDB()
//
// The x-suffixed variants return a *sqlx.DB
// (github.com/jmoiron/sqlx, a set of extensions on database/sql);
// the others return a standard *sql.DB from the same underlying session.
//
// # Lock sharding
//
// Internally the Store keeps its id -> group map in 256 independently
// locked shards, so bookkeeping for different ids rarely contends on the
// same mutex and throughput scales with cores rather than serialising on a
// single store-wide lock (see
// https://strebkov.dev/posts/shard-your-locks/).  This is transparent to
// callers: locking semantics per id are unchanged.
//
// # Connection limits
//
// NewWithConnLimits behaves exactly like New but additionally caps
// concurrent database connections at DefaultMaxConns (100) in total across
// all ids, and at DefaultMaxConnsPerID (20) for each individual id:
//
//	store, err := dblocker.NewWithConnLimits(ctx, "postgres", dsn, false)
//
// The existing constructors (New, NewWithUnlockAndStatementTimeouts, and
// NewWithConnectDBFuncAndTimeouts) are unchanged and apply no limits.
// Custom limits are available via NewWithConnLimitsAndTimeouts, or via
// Options.MaxConns and Options.MaxConnsPerID:
//
//	store, err := dblocker.NewWithOptions(ctx, dblocker.Options{
//		DriverName:     "postgres",
//		DataSourceName: dsn,
//		MaxConns:       50,
//		MaxConnsPerID:  5,
//	})
//
// MaxConnsPerID caps each id's connection pool via database/sql's
// SetMaxOpenConns.  MaxConns caps concurrent sessions store-wide.
//
// dblocker is a locker: a session is a single sequential unit of database
// work, exactly as if it were one transaction.  An RW session acts like a
// single exclusive transaction for its id, and each read session is one
// concurrent reader.  Run a session's commands one after another; to do
// work concurrently, take concurrent sessions.  Since all database access
// goes through dblocker and each session runs one command at a time,
// capping concurrent sessions (MaxConns) caps concurrent database
// connections in use.  A session holds its MaxConns slot from when access
// is granted (after any wait for the id's lock) until its cancel function
// is called.  A request beyond either cap waits, subject to the request
// context and the UnlockTimeout.
//
// Within the caps, connections are reused rather than churned: a connection
// freed by one query is handed directly to any waiting request, and
// otherwise kept open for the id's next request.  The whole pool is closed
// when the id's last request finishes, so an inactive id holds no
// connections at all.
//
// # Timeouts
//
// There are three layers of query timeout, from finest to coarsest:
//
// Per call: to give an individual query a timeout (and leave other queries
// without one), wrap that call's context — this works on every driver,
// including sqlite:
//
//	qctx, qcancel := context.WithTimeout(ctx, 5*time.Second)
//	defer qcancel()
//	_, err = db.ExecContext(qctx, "UPDATE ...")   // this call: 5s limit
//	_, err = db.ExecContext(ctx, "SELECT ...")    // this call: no limit
//
// Per session: RWGetDBWithTimeout / RWGetDBxWithTimeout open a separate
// session whose statement timeout overrides the store's (nil disables it),
// for one-off long-running work.
//
// Per store: StatementTimeout is a server-side backstop applied to every
// session (postgres and mysql).  It is added to the data source name, so
// the server enforces it on every connection the pool dials; note that
// mysql's max_execution_time applies to SELECT statements only.
//
// One contract follows from the UnlockTimeout: a session's lock is
// automatically released when the UnlockTimeout expires (the escape hatch
// for a forgotten cancel), but a query already running is not stopped by
// that release — so a query slower than the UnlockTimeout can overlap the
// next session for the same id.  Queries in a session must therefore
// complete within the UnlockTimeout; for longer-running work, set
// UnlockTimeout to nil (the lock is then held until cancel is called) or
// bound each query with a per-call context timeout below the UnlockTimeout.
//
// # Drivers
//
// sqlite (github.com/mattn/go-sqlite3), postgres (github.com/lib/pq), and
// mysql (github.com/go-sql-driver/mysql) are supported by
// DefaultConnectDBFunc; import the driver you use.  Other databases — or
// custom behaviour such as routing ids to different database shards — can
// be added with a custom ConnectDBFunc.
package dblocker
