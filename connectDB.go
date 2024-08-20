package dblocker

import (
	"context"
	"fmt"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
)

// DefaultConnectDBFunc is the default function used to connecct to the database
// This default function has an unused id variable.  This function could be customised, for example, to send requests to different database shards based on the provided id.
func DefaultConnectDBFunc(ctx context.Context, id interface{}, driverName, dataSourceName string, statementTimeout *time.Duration) (db *sqlx.DB, err error) {
	switch driverName {
	case "mock":
		mockDB, _, err := sqlmock.New()
		if err == nil && statementTimeout != nil {
			return nil, fmt.Errorf("connectDB error: statementTimeout for database type not implemented: %s", driverName)
		}
		db = sqlx.NewDb(mockDB, "sqlmock")
	case "sqlite3":
		db, err = sqlx.ConnectContext(ctx, driverName, dataSourceName)
		if err == nil && statementTimeout != nil {
			return nil, fmt.Errorf("connectDB error: statementTimeout for database type not implemented: %s", driverName)
		}
	case "postgres":
		db, err = sqlx.ConnectContext(ctx, driverName, dataSourceName)
		if err == nil && statementTimeout != nil {
			_, err := db.ExecContext(ctx, fmt.Sprintf("SET statement_timeout = %d;", statementTimeout.Milliseconds()))
			if err != nil {
				return nil, err
			}
		}
	case "mysql":
		db, err = sqlx.ConnectContext(ctx, driverName, dataSourceName)
		if err == nil && statementTimeout != nil {
			_, err := db.ExecContext(ctx, fmt.Sprintf("SET SESSION MAX_EXECUTION_TIME=%d;", statementTimeout.Milliseconds()))
			if err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("connectDB error: database type not implemented: %s", driverName)
	}
	return db, err
}

func connectDBAndWait(
	ctx context.Context, 
	id interface{}, 
	connectDBFunc func(ctx context.Context, id interface{}, driverName, dataSourceName string, statementTimeout *time.Duration) (db *sqlx.DB, err error),
	driverName string, 
	dataSourceName string, 
	statementTimeout *time.Duration,
) (db *sqlx.DB) {

	idleDuration := 2 * time.Second
	idleDelay := time.NewTimer(idleDuration)
	defer idleDelay.Stop()

	var err error
	done := false
	for !done {
		done = true

		db, err = connectDBFunc(ctx, id, driverName, dataSourceName, statementTimeout)
		if err != nil {
			done = false

			fmt.Println("dbLocker connect error:", err.Error())

			idleDelay.Reset(idleDuration)
			select {
			case <-ctx.Done():
				return
			case <-idleDelay.C:
			}
		}
	}
	return db
}
