package dblocker

import (
	"github.com/jmoiron/sqlx"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
)

// Group is a group storing the shared database for an id
type Group struct {
	requestCount int64

	DB            *sqlx.DB
	rwRequestCh   chan Request
	readRequestCh chan Request
	dbCh          chan *sqlx.DB
}

func (s *Store) startGroup(id interface{}, g *Group) {
	isRW := false
	readCount := 0

	rwDoneCh := make(chan bool)
	readDoneCh := make(chan bool)

	// Connect to the database
	s.Lock()
	g.DB = connectDBAndWait(
		s.Ctx,
		id,
		s.connectDBFunc,
		s.DriverName,
		s.DataSourceName,
		s.StatementTimeout,
	)
	s.Unlock()

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
			s.Lock()
			if g.requestCount == 0 {
				close(g.rwRequestCh)
				close(g.readRequestCh)
				close(g.dbCh)
				close(rwDoneCh)
				close(readDoneCh)

				g.DB.Close()
				g.DB = nil
				delete(s.m, id)

				s.Unlock()
				return
			}
			s.Unlock()

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
					s.Lock()
					if g.requestCount == 0 {
						close(g.rwRequestCh)
						close(g.readRequestCh)
						close(g.dbCh)
						close(rwDoneCh)
						close(readDoneCh)

						g.DB.Close()
						g.DB = nil
						delete(s.m, id)

						s.Unlock()
						return
					}
					s.Unlock()
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
