package dblocker_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/calmdocs/dblocker"
	"github.com/jmoiron/sqlx"
)

// The examples below use the "mock" driver so that they run without a real
// database.  In your own code use "sqlite3", "postgres", or "mysql" (and
// import the matching driver), or provide a custom ConnectDBFunc.

// ExampleNew demonstrates exclusive (RW) and shared (read) access to the
// database session for one id.
func ExampleNew() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := dblocker.New(ctx, "mock", "", false)
	if err != nil {
		panic(err)
	}

	// Exclusive access, like Lock() on a RWMutex for this id.
	cancelDB, db, err := store.RWGetDB("user-123", ctx, "update files")
	if err != nil {
		panic(err)
	}
	fmt.Println("exclusive access acquired:", db != nil)

	// Releasing the lock lets the next request for this id proceed.
	cancelDB()

	// Shared access, like RLock() on a RWMutex: any number of readers for
	// an id can hold the session at the same time.
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cancelDB, _, err := store.ReadGetDB("user-123", ctx, "list files")
			if err != nil {
				panic(err)
			}
			defer cancelDB()
			time.Sleep(100 * time.Millisecond) // all three overlap here
		}()
	}
	wg.Wait()
	fmt.Println("three concurrent readers finished")

	// Output:
	// exclusive access acquired: true
	// three concurrent readers finished
}

// ExampleNewWithOptions demonstrates capping concurrent database sessions
// for each individual id (MaxConnsPerID) and in total (MaxConns).  The caps
// gate the handing out of sessions; the database pool itself is never
// configured or touched by dblocker.
func ExampleNewWithOptions() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := dblocker.NewWithOptions(ctx, dblocker.Options{
		DriverName:     "mock",
		DataSourceName: "",
		MaxConns:       50,
		MaxConnsPerID:  3,
	})
	if err != nil {
		panic(err)
	}

	// Take the three read sessions id 7 is allowed concurrently
	for i := 0; i < 3; i++ {
		cancelDB, _, err := store.ReadGetDB(int64(7), ctx, "reader")
		if err != nil {
			panic(err)
		}
		defer cancelDB()
	}

	// A fourth concurrent session for id 7 waits; here it times out
	waitCtx, waitCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer waitCancel()
	_, _, err = store.ReadGetDB(int64(7), waitCtx, "over the cap")
	fmt.Println("fourth concurrent session admitted:", err == nil)

	// Output:
	// fourth concurrent session admitted: false
}

// ExampleNewWithOptions_customConnectDBFunc demonstrates a custom
// ConnectDBFunc, which can route different ids to different database shards
// or add support for other database types.
func ExampleNewWithOptions_customConnectDBFunc() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connectDBFunc := func(ctx context.Context, id interface{}, driverName, dataSourceName string, statementTimeout *time.Duration) (*sqlx.DB, error) {
		// For example: pick a dataSourceName based on id here.
		return dblocker.DefaultConnectDBFunc(ctx, id, driverName, dataSourceName, statementTimeout)
	}

	store, err := dblocker.NewWithOptions(ctx, dblocker.Options{
		ConnectDBFunc:  connectDBFunc,
		DriverName:     "mock",
		DataSourceName: "",
	})
	if err != nil {
		panic(err)
	}

	cancelDB, db, err := store.RWGetDBx("tenant-42", ctx, "custom connect")
	if err != nil {
		panic(err)
	}
	defer cancelDB()

	fmt.Println("connected:", db != nil)

	// Output:
	// connected: true
}
