# 3. Connection Pool Configuration

`*sql.DB` is a managed pool. The four pool knobs — `SetMaxOpenConns`, `SetMaxIdleConns`, `SetConnMaxLifetime`, and `SetConnMaxIdleTime` — are correctness controls as much as performance knobs. Misconfiguring them can cause connection storms, stale connections, or idle goroutines waiting forever.

```text
dbpool/
  go.mod
  driver.go        (fake sql driver — the test seam)
  pool.go          (package dbpool: Config, OpenDB, pool configuration)
  pool_test.go
  example_test.go
  cmd/demo/main.go
```

## Concepts

### The Four Pool Settings

`db.SetMaxOpenConns(n)` caps the total number of open connections (in use + idle). When all connections are busy, new callers block until one is released or the context expires. Setting this too low causes unnecessary queuing; leaving it at the default (unlimited) lets one traffic spike exhaust OS file descriptors.

`db.SetMaxIdleConns(n)` caps how many idle connections are kept ready. An idle connection is one that is open but not currently executing a query. Idle connections cost resources on both the client and the server. The default is 2, which is too low for most web services.

`db.SetConnMaxLifetime(d)` closes a connection after it has been open for `d` regardless of whether it is idle. Use it to survive database server restarts and load-balancer connection draining. A sensible value is 5–30 minutes.

`db.SetConnMaxIdleTime(d)` closes a connection that has been idle for `d`. Use it to shed connections during quiet periods. Added in Go 1.15; pair it with `SetConnMaxLifetime` for full lifecycle control.

### Configuration Must Happen Before First Query

Pool settings take effect on the next connection acquired from the pool. Changing them after queries have started is legal but affects only connections opened after the change. Configure the pool immediately after `sql.Open` and before the first `Ping` or query.

### `db.PingContext` Validates the DSN

`sql.Open` does not open any connections. `db.PingContext` forces the pool to open one connection and send a no-op to the server, validating the DSN. Call it during application startup to fail fast instead of discovering a bad DSN on the first real query.

### Wrap the Configuration in a Constructor

Hardcoding pool settings at every call site causes drift. A single constructor function that takes a `Config` and returns a `*sql.DB` with the right pool settings is easier to test and review.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/22-database-patterns/03-connection-pool-configuration/03-connection-pool-configuration/cmd/demo
cd go-solutions/22-database-patterns/03-connection-pool-configuration/03-connection-pool-configuration
go mod edit -go=1.26
```

This is a library package verified by `go test`.

### Exercise 1: The Fake Driver

Create `driver.go`:

```go
package dbpool

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"sync"
)

type fakeDriver struct {
	mu  sync.Mutex
	dbs map[string]*fakeDB
}

type fakeDB struct {
	mu   sync.Mutex
	cols []string
	rows [][]driver.Value
}

func (d *fakeDriver) Open(name string) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dbs == nil {
		d.dbs = map[string]*fakeDB{}
	}
	db, ok := d.dbs[name]
	if !ok {
		db = &fakeDB{}
		d.dbs[name] = db
	}
	return &fakeConn{db: db}, nil
}

var globalDriver = &fakeDriver{}

func init() {
	sql.Register("fakedb", globalDriver)
}

func newFakeDB(name string) (*sql.DB, *fakeDB) {
	globalDriver.mu.Lock()
	if globalDriver.dbs == nil {
		globalDriver.dbs = map[string]*fakeDB{}
	}
	fdb := &fakeDB{}
	globalDriver.dbs[name] = fdb
	globalDriver.mu.Unlock()
	db, err := sql.Open("fakedb", name)
	if err != nil {
		panic(fmt.Sprintf("fakedb open: %v", err))
	}
	return db, fdb
}

func (fdb *fakeDB) addRow(cols []string, vals ...driver.Value) {
	fdb.mu.Lock()
	defer fdb.mu.Unlock()
	fdb.cols = cols
	fdb.rows = append(fdb.rows, vals)
}

type fakeConn struct{ db *fakeDB }

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return &fakeStmt{db: c.db}, nil
}
func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

type fakeStmt struct{ db *fakeDB }

func (s *fakeStmt) Close() error                                    { return nil }
func (s *fakeStmt) NumInput() int                                   { return -1 }
func (s *fakeStmt) Exec(args []driver.Value) (driver.Result, error) { return fakeResult{}, nil }
func (s *fakeStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.db.mu.Lock()
	defer s.db.mu.Unlock()
	rows := s.db.rows
	cols := s.db.cols
	s.db.rows = nil
	return &fakeRows{cols: cols, rows: rows}, nil
}

type fakeRows struct {
	cols []string
	rows [][]driver.Value
	pos  int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.pos])
	r.pos++
	return nil
}

type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeResult) RowsAffected() (int64, error) { return 1, nil }

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

// ExposedFakeDB is the exported alias used by cmd/demo.
type ExposedFakeDB = fakeDB

// ExposedNewFakeDB opens a named fake database for use in cmd/demo.
func ExposedNewFakeDB(name string) (*sql.DB, *ExposedFakeDB) {
	return newFakeDB(name)
}

// AddRow seeds one row into the fake database.
func (fdb *fakeDB) AddRow(cols []string, vals ...driver.Value) {
	fdb.addRow(cols, vals...)
}
```

### Exercise 2: Pool Configuration Constructor

Create `pool.go`:

```go
package dbpool

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrZeroMaxOpen is returned when MaxOpenConns is zero or negative.
	ErrZeroMaxOpen = errors.New("MaxOpenConns must be at least 1")
	// ErrIdleExceedsOpen is returned when MaxIdleConns exceeds MaxOpenConns.
	ErrIdleExceedsOpen = errors.New("MaxIdleConns must not exceed MaxOpenConns")
)

// Config holds the pool tuning parameters. Zero values for durations mean
// no limit (the database/sql default).
type Config struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
}

// Validate checks that the pool settings are self-consistent.
func (c Config) Validate() error {
	if c.MaxOpenConns < 1 {
		return fmt.Errorf("pool config: %w", ErrZeroMaxOpen)
	}
	if c.MaxIdleConns > c.MaxOpenConns {
		return fmt.Errorf("pool config: %w", ErrIdleExceedsOpen)
	}
	return nil
}

// OpenDB opens a *sql.DB with the driver/DSN and applies the pool settings.
// It validates Config before opening and returns an error for invalid settings.
func OpenDB(driver, dsn string, cfg Config) (*sql.DB, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	if cfg.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	}
	if cfg.ConnMaxIdleTime > 0 {
		db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	}
	return db, nil
}

// DefaultConfig returns a sensible starting point for a web service:
// 25 open, 10 idle, 5-minute lifetime, 1-minute idle time.
func DefaultConfig() Config {
	return Config{
		MaxOpenConns:    25,
		MaxIdleConns:    10,
		ConnMaxLifetime: 5 * time.Minute,
		ConnMaxIdleTime: 1 * time.Minute,
	}
}
```

### Exercise 3: Table-Driven Tests

Create `pool_test.go`:

```go
package dbpool

import (
	"errors"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{
			name:    "valid",
			cfg:     Config{MaxOpenConns: 10, MaxIdleConns: 5},
			wantErr: nil,
		},
		{
			name:    "zero open conns",
			cfg:     Config{MaxOpenConns: 0, MaxIdleConns: 0},
			wantErr: ErrZeroMaxOpen,
		},
		{
			name:    "negative open conns",
			cfg:     Config{MaxOpenConns: -1, MaxIdleConns: 0},
			wantErr: ErrZeroMaxOpen,
		},
		{
			name:    "idle exceeds open",
			cfg:     Config{MaxOpenConns: 5, MaxIdleConns: 10},
			wantErr: ErrIdleExceedsOpen,
		},
		{
			name:    "idle equals open is valid",
			cfg:     Config{MaxOpenConns: 5, MaxIdleConns: 5},
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.cfg.Validate()
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Validate() err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestOpenDBRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	_, err := OpenDB("fakedb", "test-reject", Config{MaxOpenConns: 0})
	if !errors.Is(err, ErrZeroMaxOpen) {
		t.Fatalf("err = %v, want ErrZeroMaxOpen", err)
	}
}

func TestOpenDBAppliesSettings(t *testing.T) {
	t.Parallel()

	// newFakeDB is not used here; we call OpenDB directly so the pool
	// settings path is exercised.
	db, err := OpenDB("fakedb", "test-open-settings", Config{
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: 10 * time.Minute,
		ConnMaxIdleTime: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	// database/sql does not expose the applied settings for direct assertion,
	// but Stats confirms the pool is functional.
	stats := db.Stats()
	if stats.MaxOpenConnections != 5 {
		t.Fatalf("MaxOpenConnections = %d, want 5", stats.MaxOpenConnections)
	}
}

func TestDefaultConfigIsValid(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() = %v, want nil", err)
	}
	if cfg.MaxOpenConns != 25 {
		t.Fatalf("MaxOpenConns = %d, want 25", cfg.MaxOpenConns)
	}
}
```

Create `example_test.go`:

```go
package dbpool

import "fmt"

func ExampleDefaultConfig() {
	cfg := DefaultConfig()
	fmt.Println(cfg.MaxOpenConns)
	// Output: 25
}
```

### Exercise 4: The Demo Binary

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"time"

	"example.com/dbpool"
)

func main() {
	cfg := dbpool.Config{
		MaxOpenConns:    10,
		MaxIdleConns:    3,
		ConnMaxLifetime: 5 * time.Minute,
		ConnMaxIdleTime: 30 * time.Second,
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	db, err := dbpool.OpenDB("fakedb", "demo", cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	stats := db.Stats()
	fmt.Printf("pool configured: max_open=%d max_idle=%d\n",
		stats.MaxOpenConnections, cfg.MaxIdleConns)
}
```

Add one more test of your own: assert that `OpenDB` with `MaxIdleConns` equal to zero and `MaxOpenConns` equal to one is valid (zero idle is a legal constraint that means no connections are held idle).

## Common Mistakes

Wrong: Leave `MaxOpenConns` at the default (unlimited). What happens: under load, the application opens a connection per goroutine, eventually hitting the database server's connection limit and causing errors for all clients. Fix: set `MaxOpenConns` based on the server's connection limit divided by the number of application instances.

Wrong: Set `MaxIdleConns` higher than `MaxOpenConns`. What happens: idle connections can never exceed open connections; the higher limit is silently ignored. Fix: keep `MaxIdleConns <= MaxOpenConns`, as `Validate` enforces.

Wrong: Never set `ConnMaxLifetime`. What happens: connections opened before a database server restart or load-balancer failover become permanently stale; the next query on a stale connection returns a driver error. Fix: set `ConnMaxLifetime` to a value shorter than the server's idle timeout (e.g. 5 minutes).

## Verification

From the module directory:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

- Configure the pool immediately after `sql.Open` and before the first query.
- `SetMaxOpenConns` caps total connections; `SetMaxIdleConns` caps idle connections; keep idle <= open.
- `SetConnMaxLifetime` handles server restarts; `SetConnMaxIdleTime` sheds idle connections during quiet periods.
- `db.Stats()` exposes the current pool state for monitoring and assertions in tests.

## What's Next

Continue with [Prepared Statements](../04-prepared-statements/04-prepared-statements.md).

## Resources

- [Go database guide: opening a handle](https://go.dev/doc/database/open)
- [database/sql.DB.Stats](https://pkg.go.dev/database/sql#DB.Stats)
- [database/sql package reference](https://pkg.go.dev/database/sql)
- [Go 1.15 release notes: ConnMaxIdleTime](https://go.dev/doc/go1.15#database/sql)
