package dblocker

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"
)

// DefaultConnectDBFunc is the default function used to connecct to the database
// This default function has an unused id variable.  This function could be customised, for example, to send requests to different database shards based on the provided id.
//
// statementTimeout, when not nil, is applied by adding it to the data source
// name, so that the database server enforces it on every connection the pool
// dials — not just the first (a plain "SET statement_timeout" Exec would
// configure only the single pooled connection it happened to run on).  For
// per-call timeouts, use context.WithTimeout on the individual query instead.
func DefaultConnectDBFunc(ctx context.Context, id interface{}, driverName, dataSourceName string, statementTimeout *time.Duration) (db *sqlx.DB, err error) {
	switch driverName {
	case "mock":
		if statementTimeout != nil {
			return nil, fmt.Errorf("connectDB error: statementTimeout for database type not implemented: %s", driverName)
		}
		mockDB, _, err := sqlmock.New()
		if err != nil {
			return nil, err
		}
		db = sqlx.NewDb(mockDB, "sqlmock")
	case "sqlite3":
		if statementTimeout != nil {
			return nil, fmt.Errorf("connectDB error: statementTimeout for database type not implemented: %s", driverName)
		}
		db, err = sqlx.ConnectContext(ctx, driverName, dataSourceName)
		if err != nil {
			return nil, err
		}
	case "postgres", "mysql":
		dsn, err := statementTimeoutDSN(driverName, dataSourceName, statementTimeout)
		if err != nil {
			return nil, err
		}
		db, err = sqlx.ConnectContext(ctx, driverName, dsn)
		if err != nil {
			return nil, err
		}
		return db, nil
	default:
		return nil, fmt.Errorf("connectDB error: database type not implemented: %s", driverName)
	}
	return db, nil
}

// statementTimeoutDSN returns dataSourceName adjusted so that the database
// server applies statementTimeout to every connection dialled with it.  A
// nil statementTimeout returns dataSourceName unchanged.
//
// For postgres the timeout is added as a server option
// (-c statement_timeout=<ms>), supporting both URL and keyword/value data
// source names.  For mysql it is added as the max_execution_time session
// variable, which mysql applies to SELECT statements.
func statementTimeoutDSN(driverName, dataSourceName string, statementTimeout *time.Duration) (string, error) {
	if statementTimeout == nil {
		return dataSourceName, nil
	}
	ms := statementTimeout.Milliseconds()

	switch driverName {
	case "postgres":
		if strings.HasPrefix(dataSourceName, "postgres://") || strings.HasPrefix(dataSourceName, "postgresql://") {
			u, err := url.Parse(dataSourceName)
			if err != nil {
				return "", err
			}
			q := u.Query()
			opts := q.Get("options")
			if opts != "" {
				opts += " "
			}
			opts += fmt.Sprintf("-c statement_timeout=%d", ms)
			q.Set("options", opts)
			u.RawQuery = q.Encode()
			return u.String(), nil
		}

		// Keyword/value data source name
		return fmt.Sprintf("%s options='-c statement_timeout=%d'", dataSourceName, ms), nil
	case "mysql":
		sep := "?"
		if strings.Contains(dataSourceName, "?") {
			sep = "&"
		}
		return fmt.Sprintf("%s%smax_execution_time=%d", dataSourceName, sep, ms), nil
	default:
		return "", fmt.Errorf("connectDB error: statementTimeout for database type not implemented: %s", driverName)
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
