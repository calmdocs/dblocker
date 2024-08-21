# dblocker

[![godoc](https://godoc.org/github.com/calmdocs/dblocker?status.svg)](https://godoc.org/github.com/calmdocs/dblocker)

Golang database locker.  Locks a shared database session for each "user" behind a RWMutex. 

Works with [sqlite](github.com/mattn/go-sqlite3), [postgres](github.com/lib/pq), and [mysql](github.com/go-sql-driver/mysql) by default.  Other databases can be easily added by using a custom [connectDBFunc](https://godoc.org/github.com/calmdocs/dblocker).

The ReadGetDB and RWGetDB functions return a shared [db/sql](https://pkg.go.dev/database/sql) database.  

The ReadGetDBx and RWGetDBx functions return a [sqlx](github.com/jmoiron/sqlx) databse.  [sqlx](github.com/jmoiron/sqlx) is a library which provides a set of extensions on go's standard database/sql library.

## Why?

Allows:
- simple access to sqlite without worrying about crashes due to concurrently reads and writes from multiple databse sessions.
- a simple mechanism to ensure that only one "user" accesses the database at any time, as if access for that "user" is locked behind a RWMutex.
- database access such as multiple cuncurrent datbase select requests without also requiring the use of [pgbouncer](https://www.pgbouncer.org) or similar session access caching tools.
- multiple sql commands (and other go code) to be run for a "user", while not worrying about concurrent access for that "user", and without needing to run all of the database commands in one database transaction.

If you use a custom [connectDBFunc](https://godoc.org/github.com/calmdocs/dblocker), you can also implement simple database sharding based on the id that you provide.

## Example
```
func main() {
    debug := false
    driverName := sqlite3
    dataSourceName := "/path/to/sql.db"
    userID := "123"
    fileID := 27
    fileName := "newFile.txt"

    ctx, cancel := context.WithCancel(context.Backgound())
    defer cancel()

    dbStore, err := dblocker.New(ctx, driverName, dataSourceName, debug)
    if err != nil {
        panic(err)
    }

    // Allow user 123 to get a list of files from the database using ReadGetDB.
    // Concurrent read access to the database for user 123 is permitted.
    files, err := getFiles(ctx, dbStore, userID)
    if err != nil {
        panic(err)
    }
    fmt.Println(files)
    
    // Allow user 123 to update a database entry using RWGetDB.
    // No concurrent access to the database for that user is permitted.
    err = updateFileName(ctx, userID, fileID, fileName)
    if err != nil {
        panic(err)
    }

    // Run 20 concurrent goroutines
    // Using new database connections instead of dblocker here would break
    // sqlite due to concurrent reads and writes and
    // postgres due to too many concurrent database connections
    for i := 1; i <= 20; i++ {
        go func() {
            files, err := getFiles(ctx, dbStore, userID)
            if err != nil {
                panic(err)
            }
            fmt.Println(files)

            err = updateFileName(ctx, userID, fileID, fileName)
            if err != nil {
                panic(err)
            }
        }
    }
}

func getFiles(ctx context.Context, dbStore *dblocker.Store, userID string) (files []File, err error) {

    // Multiple ReadGetDBx calls can access the shared database session concurrently
    cancelDB, db, err := dbStore.ReadGetDBx(userID, ctx, "get files")
    if err != nil {
        return err
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

func updateFileName(ctx context.Context, userID string, fileID int64, fileName string) (err error) {
    
    // Get exclusive access the shared database session
    cancelDB, db, err := dbStore.RWGetDBx(userID, ctx, "update file name")
    if err != nil {
        return err
    }
    defer cancelDB()

    _, err = db.ExecContext(
        ctx,
        db.Rebind("update files set name = ? WHERE file_id = ?"),
        fileName,
        fileID,
    )
    return err
}
```