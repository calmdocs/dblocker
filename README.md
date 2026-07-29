# dblocker

[![godoc](https://godoc.org/github.com/calmdocs/dblocker?status.svg)](https://godoc.org/github.com/calmdocs/dblocker)

Golang database locker.  A simple library to lock a shared database session for each "user" or "id" behind what is effectively a RWMutex.

Works with [sqlite](https://github.com/mattn/go-sqlite3), [postgres](https://github.com/lib/pq), and [mysql](https://github.com/go-sql-driver/mysql) by default.  Other databases can be added with a custom [ConnectDBFunc](https://godoc.org/github.com/calmdocs/dblocker).

The `ReadGetDB` and `RWGetDB` functions return a shared [database/sql](https://pkg.go.dev/database/sql) database.  The `ReadGetDBx` and `RWGetDBx` functions return a shared [sqlx](https://github.com/jmoiron/sqlx) database.  [sqlx](https://github.com/jmoiron/sqlx) is a library which provides a set of extensions on go's standard database/sql library.

## Why?

Allows:
- simple access to sqlite without worrying about crashes due to concurrent reads and writes from multiple database sessions.
- a simple mechanism to ensure that only one "user" or "id" writes to the database at any time, as if access for that "user" or "id" is locked behind a RWMutex.
- database access such as multiple concurrent database select requests without also requiring the use of [pgbouncer](https://www.pgbouncer.org) for postgres or similar session access caching tools.
- multiple sql commands (and other go code) to be run for a "user" or "id" without worrying about concurrent access for that "user" or "id", and without needing to run all of the database commands in one database transaction.
- a total database connection limit for each individual "user" or "id" (see [Per-id connection limits](#per-id-connection-limits)).

If you use a custom [ConnectDBFunc](https://godoc.org/github.com/calmdocs/dblocker), you can also implement simple database sharding based on the "user" or "id" that you provide.

## How it works

Each id maps to one shared database session (a `*sql.DB` / `*sqlx.DB` pool).  The Get functions hand out that session together with a `cancel` function, and hold the id's lock until `cancel` is called (or the request context ends):

| Function | Lock behaviour | Session |
| --- | --- | --- |
| `RWGetDB` / `RWGetDBx` | `Lock()` — exclusive | shared session for the id |
| `ReadGetDB` / `ReadGetDBx` | `RLock()` — concurrent readers | shared session for the id |
| `RWGetDBWithTimeout` / `RWGetDBxWithTimeout` | `Lock()` — exclusive | separate new session with a custom statement timeout, closed automatically on `cancel` |

Requests wait at most `UnlockTimeout` for the lock (2 minutes when using `New`).  The shared session for an id is opened on first use and closed automatically once the id goes idle.

### Lock sharding

Locks and sessions for different ids are independent.  Internally the id → session map is [lock-striped](https://strebkov.dev/posts/shard-your-locks/) across 256 shards (configurable via `Options.ShardCount`), each guarded by its own mutex, with ids hashed to shards.  Requests for different ids therefore never contend on a single store-wide mutex, so throughput scales with cores instead of serialising on one hot lock.

## Quick start

```go
package main

import (
    "context"

    "github.com/calmdocs/dblocker"
    _ "github.com/mattn/go-sqlite3" // bring your own driver
)

func main() {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    // Create a dbStore
    // with a default unlockTimeout for waiting for access to the database of 2 minutes, and
    // with a default statementTimeout for database sessions of 4 minutes (where the database supports statement timeouts).
    dbStore, err := dblocker.New(ctx, "sqlite3", "/path/to/sql.db", false)
    if err != nil {
        panic(err)
    }

    // Exclusive access for id "123" until cancelDB() is called
    cancelDB, db, err := dbStore.RWGetDBx("123", ctx, "create table")
    if err != nil {
        panic(err)
    }
    defer cancelDB()

    _, err = db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS files (id TEXT, user_id TEXT, name TEXT);")
    if err != nil {
        panic(err)
    }
}
```

## Per-id connection limits

`NewWithOptions` exposes `MaxOpenConnsPerID`, a total database connection limit for each individual id.  Every session pool handed out for an id is capped with [`(*sql.DB).SetMaxOpenConns`](https://pkg.go.dev/database/sql#DB.SetMaxOpenConns), so a single id cannot exhaust the database server's connection limit:

```go
unlockTimeout := 2 * time.Minute
dbStore, err := dblocker.NewWithOptions(ctx, dblocker.Options{
    DriverName:        "postgres",
    DataSourceName:    "postgres://user:pass@localhost/db?sslmode=disable",
    UnlockTimeout:     &unlockTimeout,
    MaxOpenConnsPerID: 5, // each id may hold at most 5 open connections
})
```

Note: `RWGetDBWithTimeout` / `RWGetDBxWithTimeout` open a separate, equally-capped session pool that exists only while its exclusive lock is held, so an id using those calls can briefly hold up to twice the limit.

## Full example

```go
package main

import (
    "context"
    "fmt"

    "github.com/calmdocs/dblocker"
    "github.com/google/uuid"
    _ "github.com/mattn/go-sqlite3"
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
    // sqlite due to concurrent reads and writes and
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

## Custom connect functions and database sharding

`ConnectDBFunc` receives the id for every connect call, so a custom function can route different ids to different databases (sharding), or add drivers that `DefaultConnectDBFunc` does not support:

```go
connectDBFunc := func(ctx context.Context, id interface{}, driverName, dataSourceName string, statementTimeout *time.Duration) (*sqlx.DB, error) {
    // Route each id to a database shard
    dsn := dsnForShard(id)
    return sqlx.ConnectContext(ctx, driverName, dsn)
}

dbStore, err := dblocker.NewWithOptions(ctx, dblocker.Options{
    ConnectDBFunc:  connectDBFunc,
    DriverName:     "postgres",
    DataSourceName: "", // unused by the custom func above
})
```

## Constructors

| Constructor | Use when |
| --- | --- |
| `New` | You want sensible defaults (2 minute unlock timeout; 4 minute statement timeout where supported). |
| `NewWithUnlockAndStatementTimeouts` | You want custom timeouts with the default connect function. |
| `NewWithConnectDBFuncAndTimeouts` | You want a custom connect function (extra drivers, database sharding). |
| `NewWithOptions` | You want everything above plus `MaxOpenConnsPerID` (a total database connection limit for each individual id) and/or a custom `ShardCount`. |

## Timeouts

- **UnlockTimeout** — how long a request waits for access to an id's database before giving up (`nil` waits until the request context is cancelled).
- **StatementTimeout** — a per-session statement timeout applied at connect time.  Supported for postgres (`SET statement_timeout`) and mysql (`SET SESSION MAX_EXECUTION_TIME`); sqlite and the mock driver return an error if a statement timeout is requested.

## Debugging

Pass `Debug: true` (or `debug := true` with the other constructors) to print lock acquisitions and a ticker showing how long each tagged lock has been held:

```
dblocker: rw update file name
dblocker ticker count (0) duration (2s): update file name
```

The `tag` argument on every Get function names the caller in this output, which makes it easy to find the code path holding a lock for too long.
