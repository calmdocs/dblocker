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
// # Per-id connection limits
//
// Options.MaxOpenConnsPerID and Options.MaxIdleConnsPerID cap the size of
// each id's database connection pool, bounding the total number of database
// connections any single id can hold:
//
//	maxConns := 5
//	store, err := dblocker.NewWithOptions(ctx, dblocker.Options{
//		DriverName:        "postgres",
//		DataSourceName:    dsn,
//		MaxOpenConnsPerID: maxConns,
//		MaxIdleConnsPerID: maxConns,
//	})
//
// # Drivers
//
// sqlite (github.com/mattn/go-sqlite3), postgres (github.com/lib/pq), and
// mysql (github.com/go-sql-driver/mysql) are supported by
// DefaultConnectDBFunc; import the driver you use.  Other databases — or
// custom behaviour such as routing ids to different database shards — can
// be added with a custom ConnectDBFunc.
package dblocker
