package dblocker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDBLocker(t *testing.T) {
	parentCtx, parentCancel := context.WithCancel(context.Background())
	defer parentCancel()

	for i := 0; i <= 10; i++ {
		err := singleTest(parentCtx, i)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func singleTest(parentCtx context.Context, i int) (err error) {
	fmt.Printf("test %d of %d \n\n", i, 10)

	// mock database
	driverName := "mock"
	//driverName := "sqlite3"
	//dbName := ""
	//dataSource := ":memory:"
	dataSource := filepath.Join("testdata", "test.db")
	debug := true
	tag := "test"
	id := int64(0)
	defer os.Remove(dataSource)

	s, err := New(parentCtx, driverName, dataSource, debug)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("not nil")
	}

	// RWGetDB
	cancel, db, err2 := s.RWGetDB(id, parentCtx, tag)
	if err2 != nil {
		return err2
	}
	if db == nil {
		return fmt.Errorf("db nil")
	}

	<-time.After(1 * time.Second)
	cancel()

	// Check that ReadGetDB does not block
	cancel2, db2, err3 := s.ReadGetDB(id, parentCtx, tag)
	if err3 != nil {
		return err3
	}
	if db2 == nil {
		return fmt.Errorf("db nil")
	}

	// Cancel RWGetDB
	cancel2()

	// RWGetDB then immediately cancel
	cancel3, db3, err := s.RWGetDB(id, parentCtx, tag)
	if err != nil {
		return err
	}
	if db3 == nil {
		return fmt.Errorf("not nil")
	}
	cancel3()

	// Check that multiple ReadGetDBs do not block
	for i := 1; i <= 10; i++ {
		cancel, err := testRead(parentCtx, s, id, tag)
		if err != nil {
			return err
		}
		defer cancel()
	}

	return nil
}

func testRead(parentCtx context.Context, s *Store, id int64, tag string) (context.CancelFunc, error) {
	cancel, db, err := s.ReadGetDB(id, parentCtx, tag)
	if err != nil {
		return nil, err
	}
	//defer cancel()
	if db == nil {
		return nil, fmt.Errorf("nil db")
	}
	return cancel, nil
}
