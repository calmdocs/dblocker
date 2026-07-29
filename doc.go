// Package dblocker locks a shared database session behind what is
// effectively a RWMutex for each "user" or "id".
//
// # Why
//
// dblocker allows:
//
//   - simple access to sqlite without crashes caused by concurrent reads and
//     writes from multiple database sessions;
//   - a simple guarantee that only one "user" or "id" writes to the database
//     at any time, as if access for that id were locked behind a RWMutex;
//   - multiple concurrent read requests per id without tools such as
//     pgbouncer;
//   - running multiple sql commands (and other go code) for an id without
//     worrying about concurrent access for that id, and without wrapping
//     everything in one database transaction.
//
// # Locking model
//
// Each id maps to one shared database session ([sqlx.DB]).  The Get
// functions hand out that session together with a cancel function, and hold
// the id's lock until cancel is called (or the request context ends):
//
//   - [Store.RWGetDB] / [Store.RWGetDBx] act like Lock(): exclusive access.
//   - [Store.ReadGetDB] / [Store.ReadGetDBx] act like RLock(): any number of
//     concurrent readers.
//   - [Store.RWGetDBWithTimeout] / [Store.RWGetDBxWithTimeout] act like
//     Lock() but return a separate, newly opened session with its own
//     statement timeout.  The separate session is closed automatically when
//     cancel is called.
//
// Requests wait at most Options.UnlockTimeout for the lock (2 minutes when
// using [New]); the shared session is opened on first use for an id and
// closed once the id goes idle.
//
// # Lock sharding
//
// The internal id -> session map is lock-striped across
// [DefaultShardCount] shards (configurable via Options.ShardCount), each
// guarded by its own mutex, with ids hashed to shards.  Requests for
// different ids therefore do not contend on a single store-wide mutex.
//
// # Per-id connection limits
//
// Options.MaxOpenConnsPerID bounds the total number of open database
// connections each individual id can hold: every session pool handed out
// for an id is capped with (*sql.DB).SetMaxOpenConns.  Use this to stop a
// single id from exhausting the database server's connection limit.
//
// # Custom connections and database sharding
//
// A custom [ConnectDBFunc] (via [NewWithOptions] or
// [NewWithConnectDBFuncAndTimeouts]) can add drivers that
// [DefaultConnectDBFunc] does not support, or route different ids to
// different databases (sharding), since the id is passed to every connect
// call.
package dblocker
