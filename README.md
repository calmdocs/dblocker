# dblocker

[![godoc](https://godoc.org/github.com/calmdocs/dblocker?status.svg)](https://godoc.org/github.com/calmdocs/dblocker)

Golang database locker.  Locks a shared database session for each "user" behind a RWMutex. 

Works with sqlite, postgres, and mysql by default.  Other databases can be easily added by using a custom connectDBFunc.

The shared database returned by dblocker is a [sqlx databse](github.com/jmoiron/sqlx).  [sqlx](github.com/jmoiron/sqlx) is a library which provides a set of extensions on go's standard database/sql library.

## Why?

Allows:
- simple access to sqlite without worrying about crashes due to concurrent reads and writes.
- a simple mechanism to ensure that only one "user" accesses the database at any time.  For example, many large databases have an id column that should not be accessed concurrently.
- code that does not need [pgbouncer](https://www.pgbouncer.org) or similar caching mechanisms.
- multiple sql commands (and other go code) to be run for a "user", while not worrying about concurrent access for that "user", without needing to do so in one database transaction.

If you use a custom connectDBFunc, you can also implement simple database sharding.

## Example
```
func main() {
    debug := false
    driverName := sqlite3
    dataSourceName := "/path/to/sql.db"
    userID := "123"
    fileID := 27
    fileName :+ "newFile.txt"

    ctx, cancel := context.WithCancel(context.Backgound())
    defer cancel()

    dbStore, err := dblocker.New(ctx, driverName, dataSourceName, debug)
    if err != nil {
        panic(err)
    }

    // Allow user 123 to get the files using ReadGetDB.
    // Concurrent read access for user 123 is permitted.
    files, err := getFiles(ctx, dbStore, userID)
    if err != nil {
        panic(err)
    }
    
    // Allow user 123 to update the file using RWGetDB
    // No concurrent access for that user is permitted.
    err = updateFileName(ctx, userID, fileID, fileName)
    if err != nil {
        panic(err)
    }
}

func getFiles(ctx context.Context, dbStore *dblocker.Store, userID string) (files []File, err error) {

    // Multiple ReadGetDB calls can access the shared database session concurrently
    cancelDB, db, err := dbStore.ReadGetDB(userID, ctx, "get files")
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
    cancelDB, db, err := dbStore.RWGetDB(userID, ctx, "update file name")
    if err != nil {
        return err
    }
    defer cancelDB()

	defer db.Close()
	_, err = db.ExecContext(
		ctx,
		db.Rebind("update files set name = ? WHERE file_id = ?"),
		fileName,
		fileID,
	)
	return err
}
```