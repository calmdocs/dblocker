package dblocker

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
)

// connLimiter bounds the total number of open database connections across
// every pool it is attached to (the store-wide MaxConns budget).  A slot is
// acquired for each physical connection when it is dialled and released when
// that connection is closed.
type connLimiter struct {
	sem chan struct{}
}

func newConnLimiter(n int) *connLimiter {
	return &connLimiter{sem: make(chan struct{}, n)}
}

func (l *connLimiter) acquire(ctx context.Context) error {
	select {
	case l.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *connLimiter) release() {
	<-l.sem
}

// openLimitedDB opens a database/sql pool for driverName and dataSourceName
// in which every physical connection counts against limiter.  Opening a
// connection beyond the limit blocks until another connection (in any pool
// sharing the limiter) closes, or the connect context is done.
func openLimitedDB(driverName, dataSourceName string, limiter *connLimiter) (*sql.DB, error) {

	// Resolve the registered driver for driverName
	base, err := sql.Open(driverName, dataSourceName)
	if err != nil {
		return nil, err
	}
	drv := base.Driver()
	if err := base.Close(); err != nil {
		return nil, err
	}

	// Build a connector, mirroring what database/sql does internally for
	// drivers that predate driver.DriverContext
	var connector driver.Connector
	if dc, ok := drv.(driver.DriverContext); ok {
		connector, err = dc.OpenConnector(dataSourceName)
		if err != nil {
			return nil, err
		}
	} else {
		connector = dsnConnector{dsn: dataSourceName, driver: drv}
	}

	return sql.OpenDB(&limitedConnector{connector: connector, limiter: limiter}), nil
}

// dsnConnector adapts a legacy driver.Driver to driver.Connector, as
// database/sql does internally in sql.Open.
type dsnConnector struct {
	dsn    string
	driver driver.Driver
}

func (t dsnConnector) Connect(_ context.Context) (driver.Conn, error) {
	return t.driver.Open(t.dsn)
}

func (t dsnConnector) Driver() driver.Driver {
	return t.driver
}

// limitedConnector wraps a driver.Connector so that each physical connection
// holds a connLimiter slot from dial to close.
type limitedConnector struct {
	connector driver.Connector
	limiter   *connLimiter
}

func (c *limitedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := c.limiter.acquire(ctx); err != nil {
		return nil, err
	}
	conn, err := c.connector.Connect(ctx)
	if err != nil {
		c.limiter.release()
		return nil, err
	}
	return &limitedConn{conn: conn, limiter: c.limiter}, nil
}

func (c *limitedConnector) Driver() driver.Driver {
	return c.connector.Driver()
}

// limitedConn wraps a driver.Conn to release its connLimiter slot exactly
// once on Close.  The optional driver interfaces are forwarded to the
// underlying connection where it implements them, and otherwise fall back to
// the same behaviour database/sql applies to drivers without them
// (driver.ErrSkip, or the legacy method).
type limitedConn struct {
	conn    driver.Conn
	limiter *connLimiter
	once    sync.Once
}

func (c *limitedConn) Prepare(query string) (driver.Stmt, error) {
	return c.conn.Prepare(query)
}

func (c *limitedConn) Close() error {
	err := c.conn.Close()
	c.once.Do(c.limiter.release)
	return err
}

func (c *limitedConn) Begin() (driver.Tx, error) {
	//lint:ignore SA1019 driver.Conn requires Begin
	return c.conn.Begin()
}

func (c *limitedConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if pc, ok := c.conn.(driver.ConnPrepareContext); ok {
		return pc.PrepareContext(ctx, query)
	}
	return c.conn.Prepare(query)
}

func (c *limitedConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if bt, ok := c.conn.(driver.ConnBeginTx); ok {
		return bt.BeginTx(ctx, opts)
	}

	// Mirror database/sql's fallback for drivers without ConnBeginTx
	if opts.Isolation != driver.IsolationLevel(0) {
		return nil, errors.New("dblocker: underlying driver does not support non-default isolation level")
	}
	if opts.ReadOnly {
		return nil, errors.New("dblocker: underlying driver does not support read-only transactions")
	}
	//lint:ignore SA1019 fallback for drivers without ConnBeginTx
	return c.conn.Begin()
}

func (c *limitedConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if ec, ok := c.conn.(driver.ExecerContext); ok {
		return ec.ExecContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *limitedConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if qc, ok := c.conn.(driver.QueryerContext); ok {
		return qc.QueryContext(ctx, query, args)
	}
	return nil, driver.ErrSkip
}

func (c *limitedConn) Ping(ctx context.Context) error {
	if p, ok := c.conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *limitedConn) ResetSession(ctx context.Context) error {
	if sr, ok := c.conn.(driver.SessionResetter); ok {
		return sr.ResetSession(ctx)
	}
	return nil
}

func (c *limitedConn) IsValid() bool {
	if v, ok := c.conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *limitedConn) CheckNamedValue(nv *driver.NamedValue) error {
	if nvc, ok := c.conn.(driver.NamedValueChecker); ok {
		return nvc.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}
