package dblocker

import (
	"testing"
	"time"
)

// TestStatementTimeoutDSN checks that the statement timeout is added to the
// data source name in each supported format, so that every connection the
// pool dials gets the timeout — not just the first.
func TestStatementTimeoutDSN(t *testing.T) {
	timeout := 4 * time.Minute // 240000 ms

	tests := []struct {
		name       string
		driverName string
		dsn        string
		timeout    *time.Duration
		want       string
		wantErr    bool
	}{
		{
			name:       "nil timeout returns dsn unchanged",
			driverName: "postgres",
			dsn:        "host=localhost dbname=app",
			timeout:    nil,
			want:       "host=localhost dbname=app",
		},
		{
			name:       "postgres keyword/value",
			driverName: "postgres",
			dsn:        "host=localhost dbname=app",
			timeout:    &timeout,
			want:       "host=localhost dbname=app options='-c statement_timeout=240000'",
		},
		{
			name:       "postgres URL",
			driverName: "postgres",
			dsn:        "postgres://user:pass@localhost:5432/app?sslmode=disable",
			timeout:    &timeout,
			want:       "postgres://user:pass@localhost:5432/app?options=-c+statement_timeout%3D240000&sslmode=disable",
		},
		{
			name:       "postgres URL with existing options",
			driverName: "postgres",
			dsn:        "postgres://localhost/app?options=-c%20search_path%3Dmyschema",
			timeout:    &timeout,
			want:       "postgres://localhost/app?options=-c+search_path%3Dmyschema+-c+statement_timeout%3D240000",
		},
		{
			name:       "mysql without params",
			driverName: "mysql",
			dsn:        "user:pass@tcp(localhost:3306)/app",
			timeout:    &timeout,
			want:       "user:pass@tcp(localhost:3306)/app?max_execution_time=240000",
		},
		{
			name:       "mysql with params",
			driverName: "mysql",
			dsn:        "user:pass@tcp(localhost:3306)/app?parseTime=true",
			timeout:    &timeout,
			want:       "user:pass@tcp(localhost:3306)/app?parseTime=true&max_execution_time=240000",
		},
		{
			name:       "unsupported driver",
			driverName: "sqlite3",
			dsn:        "file.db",
			timeout:    &timeout,
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := statementTimeoutDSN(tt.driverName, tt.dsn, tt.timeout)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("statementTimeoutDSN = %q, want %q", got, tt.want)
			}
		})
	}
}
