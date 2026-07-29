package dblocker_test

import (
	"context"
	"fmt"
	"time"

	"github.com/calmdocs/dblocker"
)

// ExampleNew shows the simplest way to create a Store and take the
// exclusive (RW) lock for an id.
func ExampleNew() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The "mock" driver is used here so the example runs without a real
	// database.  Use "sqlite3", "postgres", or "mysql" in real code (and
	// import the matching driver package).
	dbStore, err := dblocker.New(ctx, "mock", "", false)
	if err != nil {
		panic(err)
	}

	// Exclusive access for id "user-1" until cancelDB() is called
	cancelDB, db, err := dbStore.RWGetDB("user-1", ctx, "example rw")
	if err != nil {
		panic(err)
	}
	defer cancelDB()

	fmt.Println(db != nil)
	// Output: true
}

// ExampleStore_ReadGetDB shows concurrent read access: multiple ReadGetDB
// calls for the same id hold the read lock at the same time.
func ExampleStore_ReadGetDB() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbStore, err := dblocker.New(ctx, "mock", "", false)
	if err != nil {
		panic(err)
	}

	cancel1, db1, err := dbStore.ReadGetDB("user-1", ctx, "reader 1")
	if err != nil {
		panic(err)
	}
	defer cancel1()

	// A second reader does not block while the first read lock is held
	cancel2, db2, err := dbStore.ReadGetDB("user-1", ctx, "reader 2")
	if err != nil {
		panic(err)
	}
	defer cancel2()

	fmt.Println(db1 != nil, db2 != nil)
	// Output: true true
}

// ExampleNewWithOptions shows the Options constructor, including the total
// database connection limit for each individual id.
func ExampleNewWithOptions() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	unlockTimeout := 2 * time.Minute
	dbStore, err := dblocker.NewWithOptions(ctx, dblocker.Options{
		DriverName:        "mock",
		DataSourceName:    "",
		UnlockTimeout:     &unlockTimeout,
		MaxOpenConnsPerID: 5, // each id may hold at most 5 open connections
	})
	if err != nil {
		panic(err)
	}

	cancelDB, db, err := dbStore.RWGetDB("user-1", ctx, "limited")
	if err != nil {
		panic(err)
	}
	defer cancelDB()

	fmt.Println(db.Stats().MaxOpenConnections)
	// Output: 5
}
