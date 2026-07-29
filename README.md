# dblocker

[![godoc](https://godoc.org/github.com/calmdocs/dblocker?status.svg)](https://godoc.org/github.com/calmdocs/dblocker)

Golang database locker.  A simple library to lock a shared database session for each "user" or "id" behind what is effectively a per-id RWMutex.

Works with [sqlite](https://github.com/mattn/go-sqlite3), [postgres](https://github.com/lib/pq), and [mysql](https://github.com/go-sql-driver/mysql) by default.  Other databases can be added by using a custom [ConnectDBFunc](https://godoc.org/github.com/calmdocs/dblocker).

The `ReadGetDB` and `RWGetDB` functions return a shared [database/sql](https://pkg.go.dev/database/sql) database.  The `ReadGetDBx` and `RWGetDBx` functions return a shared [sqlx](https://github.com/jmoiron/sqlx) database.  [sqlx](https://github.com/jmoiron/sqlx) is a library which provides a set of extensions on go's standard database/sql library.

## Why?

Allows:
- simple access to sqlite without worrying about crashes due to concurrent reads and writes from multiple database sessions.
- a simple mechanism to ensure that only one "user" or "id" writes to the database at any time, as if access for that "user" or "id" is locked behind a RWMutex.
- database access such as multiple concurrent database select requests without also requiring the use of [pgbouncer](https://www.pgbouncer.org) for postgres or similar session access caching tools.
- multiple sql commands (and other go code) to be run for a "user" or "id", while not worrying about concurrent access for that "user" or "id", and without needing to run all of the database commands in one database transaction.
- caps on the number of concurrent database connections for each individual "user" or "id" (`MaxConnsPerID`) and in total across all ids (`MaxConns`).

If you use a custom [ConnectDBFunc](https://godoc.org/github.com/calmdocs/dblocker), you can also implement simple database sharding based on the "user" or "id" that you provide.

## Access model

Each id is served by its own shared database session (a connection pool).  Three kinds of access are available:

| Function | Returns | Behaviour |
| --- | --- | --- |
| `RWGetDB` / `RWGetDBx` | `*sql.DB` / `*sqlx.DB` | Exclusive access for the id, like `Lock()` on a RWMutex.  All other requests for the same id wait until the returned `cancel()` is called. |
| `ReadGetDB` / `ReadGetDBx` | `*sql.DB` / `*sqlx.DB` | Shared access for the id, like `RLock()` on a RWMutex.  Any number of read requests run concurrently; writers wait. |
| `RWGetDBWithTimeout` / `RWGetDBxWithTimeout` | `*sql.DB` / `*sqlx.DB` | Exclusive access on a brand new session with a custom statement timeout, for one-off long-running statements. |

Every accessor returns a `cancel` function.  Calling it releases the lock for that id — defer it immediately after a successful call:

```go
cancelDB, db, err := dbStore.RWGetDBx(userID, ctx, "update files")
if err != nil {
    return err
}
defer cancelDB()
```

Requests for **different** ids never block each other (and, since the internal
store is lock-sharded, barely even contend — see
[Design](#design) below).

## Constructors

```go
// Defaults: 2 minute unlockTimeout, and a 4 minute statementTimeout
// where the database supports statement timeouts (postgres and mysql).
// No connection limits.
dbStore, err := dblocker.New(ctx, driverName, dataSourceName, debug)

// Like New, but additionally caps concurrent database connections at
// 100 (DefaultMaxConns) in total across all ids, and at
// 20 (DefaultMaxConnsPerID) for each individual id.
dbStore, err := dblocker.NewWithConnLimits(ctx, driverName, dataSourceName, debug)

// As above with custom limits and timeouts (0 disables a limit, nil disables a timeout).
dbStore, err := dblocker.NewWithConnLimitsAndTimeouts(
    ctx, driverName, dataSourceName, maxConns, maxConnsPerID, &unlockTimeout, &statementTimeout, debug)

// Custom timeouts (nil disables the timeout).
dbStore, err := dblocker.NewWithUnlockAndStatementTimeouts(
    ctx, driverName, dataSourceName, &unlockTimeout, &statementTimeout, debug)

// Full control, including per-id connection limits and a custom connect function.
dbStore, err := dblocker.NewWithOptions(ctx, dblocker.Options{
    ConnectDBFunc:     nil, // nil uses dblocker.DefaultConnectDBFunc
    DriverName:        "postgres",
    DataSourceName:    dsn,
    UnlockTimeout:     &unlockTimeout,
    StatementTimeout:  &statementTimeout,
    MaxConns:          50, // total concurrent db connection limit across all ids (0 = no limit)
    MaxConnsPerID:     5,  // concurrent db connection limit for each individual id (0 = no limit)
    Debug:             false,
})
```

Existing code using `New`, `NewWithUnlockAndStatementTimeouts`, or
`NewWithConnectDBFuncAndTimeouts` is unchanged — those constructors apply no
connection limits.

- **UnlockTimeout** — the maximum time a request waits for access to an id's database.  `nil` means wait until the request context is done.
- **StatementTimeout** — a per-session statement timeout, applied where the database supports it (postgres and mysql).  Constructors return an error if you set it for a database that does not support it.
- **MaxConnsPerID** — caps the number of concurrent database connections for each individual id (applied to the shared session and to separate sessions created by `RWGetDBWithTimeout`), so one busy or misbehaving id cannot exhaust the database server's connection limit.
- **MaxConns** — caps the number of concurrent database sessions in total across all ids.  dblocker assumes all database access goes through it, and that each session runs one query at a time, so capping concurrent sessions caps concurrent database connections in use.  A session holds its slot from when access is granted (after any wait for the id's lock — sessions queued behind a busy id do not consume budget) until its `cancel()` is called.  Requests beyond the cap wait, subject to the request context and the `UnlockTimeout`.

Within the caps, connections are reused rather than churned: a connection freed by one query goes directly to any waiting request, and is otherwise kept open for the id's next request; the whole pool is closed when the id's last request finishes, so an inactive id holds no connections.

## Example

```go
package main

import (
    "context"
    "fmt"

    "github.com/calmdocs/dblocker"
    "github.com/google/uuid"
    _ "github.com/mattn/go-sqlite3" // bring your own driver
)

type File struct {
    ID     string `db:"id"`
    UserID string `db:"user_id"`
    Name   string `db:"name"`
}

func main() {
    debug := false
    driverName := "sqlite3"
    dataSourceName := "/path/to/sql.db"
    userID := "123"
    oldFileName := "file 27"
    newFileName := "newFile.txt"

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    // Create a dbStore
    // with a default unlockTimeout for waiting for access to the database of 2 minutes, and
    // with a default statementTimeout for database sessions of 4 minutes (where the database supports statement timeouts).
    dbStore, err := dblocker.New(ctx, driverName, dataSourceName, debug)
    if err != nil {
        panic(err)
    }

    // Create database table using RWGetDBx and insert 50 rows.
    err = createTableAndInsertRows(ctx, dbStore, userID)
    if err != nil {
        panic(err)
    }

    // Allow user 123 to get a list of files from the database using ReadGetDBx.
    // Concurrent read access to the database for user 123 is permitted.
    files, err := getFiles(ctx, dbStore, userID)
    if err != nil {
        panic(err)
    }
    fmt.Println(files)

    // Allow user 123 to update a database entry using RWGetDBx.
    // No concurrent access to the database for that user is permitted.
    err = updateFileName(ctx, dbStore, userID, oldFileName, newFileName)
    if err != nil {
        panic(err)
    }

    // Run 20 concurrent goroutines.
    // Using new database connections instead of dblocker here would break
    // sqlite due to concurrent reads and writes, and
    // postgres due to too many concurrent database connections.
    for i := 1; i <= 20; i++ {
        go func() {
            files, err := getFiles(ctx, dbStore, userID)
            if err != nil {
                panic(err)
            }
            fmt.Println(files)

            err = updateFileName(ctx, dbStore, userID, oldFileName, newFileName)
            if err != nil {
                panic(err)
            }
        }()
    }

    select {}
}

func createTableAndInsertRows(ctx context.Context, dbStore *dblocker.Store, userID string) (err error) {

    // Get exclusive access to the shared database session
    cancelDB, db, err := dbStore.RWGetDBx(userID, ctx, "create database")
    if err != nil {
        return err
    }
    defer cancelDB()

    // Create table if it does not exist
    _, err = db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS files (id TEXT, user_id TEXT, name TEXT);")
    if err != nil {
        return err
    }

    // Insert 50 rows into the database
    for i := 1; i <= 50; i++ {

        // Create new id using uuidv7
        newUUID, err := uuid.NewV7()
        if err != nil {
            return err
        }

        // Insert
        _, err = db.ExecContext(
            ctx,
            db.Rebind("insert into files(id, user_id, name) values(?, ?, ?)"),
            newUUID.String(),
            userID,
            fmt.Sprintf("file %d", i),
        )
        if err != nil {
            return err
        }
    }
    return nil
}

func getFiles(ctx context.Context, dbStore *dblocker.Store, userID string) (files []File, err error) {

    // Multiple ReadGetDBx calls can access the shared database session concurrently
    cancelDB, db, err := dbStore.ReadGetDBx(userID, ctx, "get files")
    if err != nil {
        return nil, err
    }
    defer cancelDB()

    err = db.SelectContext(
        ctx,
        &files,
        db.Rebind("SELECT * from files WHERE user_id = ?"),
        userID,
    )
    if err != nil {
        return nil, err
    }
    return files, nil
}

func updateFileName(ctx context.Context, dbStore *dblocker.Store, userID string, oldFileName string, newFileName string) (err error) {

    // Get exclusive access to the shared database session
    cancelDB, db, err := dbStore.RWGetDBx(userID, ctx, "update file name")
    if err != nil {
        return err
    }
    defer cancelDB()

    _, err = db.ExecContext(
        ctx,
        db.Rebind("update files set name = ? WHERE user_id = ? AND name = ?"),
        newFileName,
        userID,
        oldFileName,
    )
    return err
}
```

More runnable examples (including per-id connection limits and a custom
`ConnectDBFunc`) are in
[example_test.go](example_test.go) and on
[godoc](https://godoc.org/github.com/calmdocs/dblocker).

## Design

- **One goroutine per active id.**  The first request for an id creates a `Group`, which connects to the database and then serves requests over channels, implementing the RWMutex semantics.  When the last request for an id finishes, the session is closed and the group is deleted, so idle ids cost nothing.
- **Sharded store lock.**  The store's `id -> group` map is split across 256 independently locked shards (selected by hashing the id), so bookkeeping for different ids rarely touches the same mutex.  A single store-wide mutex flatlines as cores are added, while sharded locks scale near-linearly — see [Shard your locks: benchmarking 6 Go cache designs](https://strebkov.dev/posts/shard-your-locks/), which measured a 9× throughput jump moving from one lock to 256 shards.  Shards are padded to separate cache lines to avoid false sharing.  This is transparent to callers: locking semantics per id are unchanged.
- **Connects never hold locks.**  Database connects (including retries while a database is temporarily unreachable) happen outside the shard mutex, so a slow or failing connect for one id never wedges requests for other ids, and connect errors are surfaced to waiters instead of timing out silently.
- **Everything is context-aware.**  Waiting for the lock respects the request context, the store context, and the `UnlockTimeout`.

## Debugging

Pass `debug: true` (or `Options.Debug`) to log each lock acquisition with its
`tag`, and to print a ticker for any lock held longer than 2 seconds — which
makes it easy to find the request that is holding an id's lock:

```
dblocker: rw update file name
dblocker ticker count (0) duration (2.001s): update file name
```

The `tag` argument on every accessor exists for exactly this purpose — use a
short description of the call site.
