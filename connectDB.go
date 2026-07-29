package dblocker

import (
	"context"
	"fmt"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"
)

// DefaultConnectDBFunc is the default function used to connecct to the database
// This default function has an unused id variable.  This function could be customised, for example, to send requests to different database shards based on the provided id.
func DefaultConnectDBFunc(ctx context.Context, id interface{}, driverName, dataSourceName string, statementTimeout *time.Duration) (db *sqlx.DB, err error) {
	switch driverName {
	case "mock":
		mockDB, _, err := sqlmock.New()
		if err != nil {
			return nil, err
		}
		db = sqlx.NewDb(mockDB, "sqlmock")
	case "sqlite3", "postgres", "mysql":
		db, err = sqlx.ConnectContext(ctx, driverName, dataSourceName)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("connectDB error: database type not implemented: %s", driverName)
	}
	if err := setStatementTimeout(ctx, db, driverName, statementTimeout); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// limitedConnectDB connects like DefaultConnectDBFunc, but every physical
// database connection in the returned pool counts against limiter (the
// store-wide MaxConns budget).  The "mock" driver's pool is created by
// sqlmock itself and cannot be wrapped, so mock connections are not counted.
func limitedConnectDB(ctx context.Context, driverName, dataSourceName string, statementTimeout *time.Duration, limiter *connLimiter) (db *sqlx.DB, err error) {
	switch driverName {
	case "mock":
		return DefaultConnectDBFunc(ctx, nil, driverName, dataSourceName, statementTimeout)
	case "sqlite3", "postgres", "mysql":
		sqlDB, err := openLimitedDB(driverName, dataSourceName, limiter)
		if err != nil {
			return nil, err
		}
		db = sqlx.NewDb(sqlDB, driverName)
		if err := db.PingContext(ctx); err != nil {
			db.Close()
			return nil, err
		}
	default:
		return nil, fmt.Errorf("connectDB error: database type not implemented: %s", driverName)
	}
	if err := setStatementTimeout(ctx, db, driverName, statementTimeout); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// setStatementTimeout applies statementTimeout to the database session for
// databases that support it.  A nil statementTimeout is a no-op; a non-nil
// statementTimeout for a database without statement timeout support is an
// error.
func setStatementTimeout(ctx context.Context, db *sqlx.DB, driverName string, statementTimeout *time.Duration) error {
	if statementTimeout == nil {
		return nil
	}
	switch driverName {
	case "postgres":
		_, err := db.ExecContext(ctx, fmt.Sprintf("SET statement_timeout = %d;", statementTimeout.Milliseconds()))
		return err
	case "mysql":
		_, err := db.ExecContext(ctx, fmt.Sprintf("SET SESSION MAX_EXECUTION_TIME=%d;", statementTimeout.Milliseconds()))
		return err
	default:
		return fmt.Errorf("connectDB error: statementTimeout for database type not implemented: %s", driverName)
	}
}

// connectDBAndWait retries connect every 2 seconds until it succeeds,
// maxWait elapses, or ctx is cancelled.  On failure the last connect error is
// returned so callers can surface the real cause (e.g. "unable to open
// database file") instead of retrying forever and never reporting anything.
func connectDBAndWait(
	ctx context.Context,
	connect func(ctx context.Context) (db *sqlx.DB, err error),
	maxWait time.Duration,
) (db *sqlx.DB, err error) {

	idleDuration := 2 * time.Second
	idleDelay := time.NewTimer(idleDuration)
	defer idleDelay.Stop()

	deadline := time.Now().Add(maxWait)
	for {
		db, err = connect(ctx)
		if err == nil {
			return db, nil
		}

		fmt.Println("dbLocker connect error:", err.Error())

		// Give up if the next retry would land past the deadline
		if time.Now().Add(idleDuration).After(deadline) {
			return nil, err
		}

		idleDelay.Reset(idleDuration)
		select {
		case <-ctx.Done():
			return nil, err
		case <-idleDelay.C:
		}
	}
}
