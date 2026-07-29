package dblocker

import (
	"context"
	"time"

	"github.com/jmoiron/sqlx"
)

// Group is a group storing the shared database for an id
type Group struct {
	requestCount int64

	DB            *sqlx.DB
	connectErr    error
	rwRequestCh   chan Request
	readRequestCh chan Request
	dbCh          chan *sqlx.DB

	// connSem bounds the number of concurrent database sessions for this
	// id when the Store's MaxConnsPerID > 0 (nil means no limit).  A slot
	// is held from when access for a session is granted until the
	// session's cancel function is called (or its context ends).
	connSem chan struct{}
}

func (s *Store) startGroup(id interface{}, g *Group) {
	isRW := false
	readCount := 0

	rwDoneCh := make(chan bool)
	readDoneCh := make(chan bool)

	// All lifecycle state for this id (requestCount, map entry) is guarded
	// by the shard mutex for this id.
	sh := s.shardFor(id)

	// Connect to the database WITHOUT holding the shard mutex.  Holding it
	// here would block every waitGetDB call for ids on this shard — a plain
	// mutex wait that ignores contexts and the UnlockTimeout — so a database
	// that stays unopenable would wedge those ids indefinitely.
	//
	// The connect retry window is kept shorter than UnlockTimeout so that
	// waiters receive the real connect error (via the nil db handed out by
	// drainFailedGroup) rather than a generic context deadline.
	maxWait := time.Minute
	if s.UnlockTimeout != nil {
		maxWait = *s.UnlockTimeout / 2
	}
	db, err := connectDBAndWait(
		s.Ctx,
		func(ctx context.Context) (*sqlx.DB, error) {
			return s.connectDB(ctx, id, s.StatementTimeout)
		},
		maxWait,
	)
	if err != nil {
		// Surface the connect error to every waiter, then delete the group
		// so the next request dials a fresh connection.
		g.connectErr = err
		s.drainFailedGroup(id, g, rwDoneCh, readDoneCh)
		return
	}

	sh.Lock()
	g.DB = db
	sh.Unlock()

	for {

		switch {

		// Reading and writing
		case isRW:
			for isRW {
				select {

				// Send shared database to channel if requested
				case g.dbCh <- g.DB:

				// Wait for rw request to finish
				case <-rwDoneCh:
					isRW = false

				case <-s.Ctx.Done():
					return
				}
			}

			// Close connection and delete group when done
			sh.Lock()
			if g.requestCount == 0 {
				close(g.rwRequestCh)
				close(g.readRequestCh)
				close(g.dbCh)
				close(rwDoneCh)
				close(readDoneCh)

				g.DB.Close()
				g.DB = nil
				delete(sh.m, id)

				sh.Unlock()
				return
			}
			sh.Unlock()

		// Reading
		case readCount > 0:
			select {

			// Send shared database to channel if requested
			case g.dbCh <- g.DB:

			// Read request
			case r := <-g.readRequestCh:
				readCount++

				// Send message to readDoneCh when the request context is cancelled
				go func() {
					select {
					case <-r.ctx.Done():
					case <-s.Ctx.Done():
						return
					}
					select {
					case readDoneCh <- true:
					case <-s.Ctx.Done():
						return
					}
				}()

			// Read request is finished
			case <-readDoneCh:
				readCount--

				// Close connection and delete group when all read requests are done
				if readCount == 0 {
					sh.Lock()
					if g.requestCount == 0 {
						close(g.rwRequestCh)
						close(g.readRequestCh)
						close(g.dbCh)
						close(rwDoneCh)
						close(readDoneCh)

						g.DB.Close()
						g.DB = nil
						delete(sh.m, id)

						sh.Unlock()
						return
					}
					sh.Unlock()
				}

			case <-s.Ctx.Done():
				return
			}

		// Database is unused
		default:
			select {
			case <-s.Ctx.Done():
				return

			// Send shared database to channel if requested
			case g.dbCh <- g.DB:

			// RW request
			case r := <-g.rwRequestCh:
				isRW = true

				// Send message to rwDoneCh when the request context is cancelled
				go func() {
					select {
					case <-r.ctx.Done():
					case <-s.Ctx.Done():
						return
					}
					select {
					case rwDoneCh <- true:
					case <-s.Ctx.Done():
						return
					}
				}()

			// Read request
			case r := <-g.readRequestCh:
				readCount++

				// Send message to readDoneCh when the request context is cancelled
				go func() {
					select {
					case <-r.ctx.Done():
					case <-s.Ctx.Done():
						return
					}
					select {
					case readDoneCh <- true:
					case <-s.Ctx.Done():
						return
					}
				}()
			}
		}
	}
}

// drainFailedGroup serves a group whose database connect failed.  Requests
// are accepted and handed a nil database (waitGetDB translates that into
// g.connectErr) until no requests remain, then the group is deleted so the
// next request reconnects from scratch.  The ticker case re-checks the
// request count when a waiter gives up (context cancelled) without ever
// taking a request or a nil db from us.
func (s *Store) drainFailedGroup(id interface{}, g *Group, rwDoneCh, readDoneCh chan bool) {
	sh := s.shardFor(id)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		sh.Lock()
		if g.requestCount == 0 {
			close(g.rwRequestCh)
			close(g.readRequestCh)
			close(g.dbCh)
			close(rwDoneCh)
			close(readDoneCh)

			delete(sh.m, id)

			sh.Unlock()
			return
		}
		sh.Unlock()

		select {
		case <-s.Ctx.Done():
			return
		case <-g.rwRequestCh:
		case <-g.readRequestCh:
		case g.dbCh <- nil:
		case <-ticker.C:
		}
	}
}
