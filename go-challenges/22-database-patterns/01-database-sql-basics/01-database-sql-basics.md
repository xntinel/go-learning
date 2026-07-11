# 1. Database/SQL Basics

The hard part of `database/sql` is not calling `QueryRowContext`; it is designing a repository boundary that keeps SQL errors, missing rows, and validation failures explicit and stable for callers. This lesson builds a small album repository using a fake in-memory driver so every `database/sql` API call compiles and runs offline.

```text
dbbasics/
  go.mod
  driver.go        (fake sql driver — the test seam)
  store.go         (package dbbasics: Album, Store, repository methods)
  store_test.go
  example_test.go
  cmd/demo/main.go
```

## Concepts

### `*sql.DB` Is a Pool Handle, Not a Connection

`sql.Open` does not dial the database; it validates the driver name and returns a pool handle. The first actual connection attempt happens lazily on the first query. `*sql.DB` is concurrency-safe and should be created once and shared for the lifetime of the process. Calling `sql.Open` per request defeats connection pooling entirely.

### Row Errors Surface at Scan Time

`db.QueryRowContext` always returns a non-nil `*sql.Row`. When the query matches no rows the error is deferred: it surfaces only when `row.Scan(...)` is called, at which point it returns `sql.ErrNoRows`. Repository code must translate this internal error into a stable package sentinel so callers can use `errors.Is` without importing `database/sql`.

### Drivers Implement a Small Interface

`database/sql` delegates all real work to a registered driver. The driver package `database/sql/driver` defines the contracts: `driver.Driver`, `driver.Conn`, `driver.Stmt`, and `driver.Rows`. For offline testing you implement those interfaces in-process, register the driver with `sql.Register`, and open it with `sql.Open("<name>", "")`. The rest of the `database/sql` API works normally against that stub.

### Repository Errors Should Be Stable Package Sentinels

Sentinel errors defined with `errors.New` are stable identifiers. Wrapping them with `fmt.Errorf("...: %w", sentinel)` lets callers assert `errors.Is(err, ErrNotFound)` even when the error carries context. Never expose raw `sql.ErrNoRows` or driver errors to callers — those are implementation details.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/dbbasics/cmd/demo
cd ~/go-exercises/dbbasics
go mod init example.com/dbbasics
go mod edit -go=1.26
```

This is a library package. Verification is `go test`, not a `main` that prints.

### Exercise 1: The Fake Driver

The fake driver lets every `database/sql` API call work offline. It registers under the name `"fakedb"` and stores rows in memory.

Create `driver.go`:

```go
package dbbasics

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"sync"
)

// fakeDriver is an in-memory sql/driver.Driver that serves rows registered
// by fakeDB.addRow. Register it once at init time.
type fakeDriver struct {
	mu  sync.Mutex
	dbs map[string]*fakeDB
}

type fakeDB struct {
	mu   sync.Mutex
	rows [][]driver.Value
	cols []string
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

// newFakeDB opens a named fakedb handle. The name must be unique per test.
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

// addRow registers one row of values to return from the next query.
func (fdb *fakeDB) addRow(cols []string, vals ...driver.Value) {
	fdb.mu.Lock()
	defer fdb.mu.Unlock()
	fdb.cols = cols
	fdb.rows = append(fdb.rows, vals)
}

// fakeConn satisfies driver.Conn.
type fakeConn struct{ db *fakeDB }

func (c *fakeConn) Prepare(query string) (driver.Stmt, error) {
	return &fakeStmt{db: c.db}, nil
}
func (c *fakeConn) Close() error              { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) { return &fakeTx{}, nil }

// fakeStmt satisfies driver.Stmt.
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

// fakeRows satisfies driver.Rows.
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

// fakeResult satisfies driver.Result.
type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeResult) RowsAffected() (int64, error) { return 1, nil }

// fakeTx satisfies driver.Tx.
type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

// ExposedFakeDB is the exported alias used by cmd/demo.
type ExposedFakeDB = fakeDB

// ExposedNewFakeDB opens a named fake database, returning both the *sql.DB
// and the seed handle. Use only in main programs and examples.
func ExposedNewFakeDB(name string) (*sql.DB, *ExposedFakeDB) {
	return newFakeDB(name)
}

// AddRow seeds one row into the fake database.
func (fdb *fakeDB) AddRow(cols []string, vals ...driver.Value) {
	fdb.addRow(cols, vals...)
}
```

### Exercise 2: The Album Repository

Create `store.go`:

```go
package dbbasics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var (
	// ErrEmptyTitle is returned by NewAlbum when the title is blank.
	ErrEmptyTitle = errors.New("title must not be empty")
	// ErrNotFound is returned by Store.FindAlbum when no row matches.
	ErrNotFound = errors.New("album not found")
)

// Album is a value type; copy is cheap and there is no identity mutation.
type Album struct {
	id     int64
	title  string
	artist string
}

func (a Album) ID() int64      { return a.id }
func (a Album) Title() string  { return a.title }
func (a Album) Artist() string { return a.artist }

// NewAlbum validates and constructs an Album.
func NewAlbum(id int64, title, artist string) (Album, error) {
	if title == "" {
		return Album{}, fmt.Errorf("album: %w", ErrEmptyTitle)
	}
	return Album{id: id, title: title, artist: artist}, nil
}

// Store wraps a *sql.DB and exposes repository methods.
type Store struct {
	db *sql.DB
}

// NewStore creates a Store that owns the given database handle.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// FindAlbum queries for one row by id. Missing rows become ErrNotFound.
func (s *Store) FindAlbum(ctx context.Context, id int64) (Album, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT id, title, artist FROM albums WHERE id = ?", id)

	var a Album
	if err := row.Scan(&a.id, &a.title, &a.artist); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Album{}, fmt.Errorf("find album %d: %w", id, ErrNotFound)
		}
		return Album{}, fmt.Errorf("find album %d: %w", id, err)
	}
	return a, nil
}

// InsertAlbum persists one album and returns the inserted value.
func (s *Store) InsertAlbum(ctx context.Context, a Album) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO albums (id, title, artist) VALUES (?, ?, ?)",
		a.id, a.title, a.artist)
	if err != nil {
		return fmt.Errorf("insert album: %w", err)
	}
	return nil
}
```

### Exercise 3: Table-Driven Tests

Create `store_test.go`:

```go
package dbbasics

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
)

func TestFindAlbum(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		seedID  int64
		seedRow bool
		queryID int64
		want    string
		wantErr error
	}{
		{
			name:    "found",
			seedID:  1,
			seedRow: true,
			queryID: 1,
			want:    "Blue Train",
		},
		{
			name:    "not found returns ErrNotFound",
			seedRow: false,
			queryID: 99,
			wantErr: ErrNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sqlDB, fdb := newFakeDB(tt.name)
			defer sqlDB.Close()

			if tt.seedRow {
				fdb.addRow(
					[]string{"id", "title", "artist"},
					driver.Value(tt.seedID),
					driver.Value("Blue Train"),
					driver.Value("John Coltrane"),
				)
			}

			store := NewStore(sqlDB)
			got, err := store.FindAlbum(context.Background(), tt.queryID)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("FindAlbum err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && got.Title() != tt.want {
				t.Fatalf("Title = %q, want %q", got.Title(), tt.want)
			}
		})
	}
}

func TestNewAlbumRejectsEmptyTitle(t *testing.T) {
	t.Parallel()

	_, err := NewAlbum(1, "", "Miles Davis")
	if !errors.Is(err, ErrEmptyTitle) {
		t.Fatalf("err = %v, want ErrEmptyTitle", err)
	}
}

func TestInsertAlbumRoundTrip(t *testing.T) {
	t.Parallel()

	sqlDB, fdb := newFakeDB("insert-roundtrip")
	defer sqlDB.Close()

	a, err := NewAlbum(2, "Kind of Blue", "Miles Davis")
	if err != nil {
		t.Fatal(err)
	}

	store := NewStore(sqlDB)
	if err := store.InsertAlbum(context.Background(), a); err != nil {
		t.Fatalf("InsertAlbum: %v", err)
	}

	// Seed the read-back row that FindAlbum will consume.
	fdb.addRow(
		[]string{"id", "title", "artist"},
		driver.Value(int64(2)),
		driver.Value("Kind of Blue"),
		driver.Value("Miles Davis"),
	)

	got, err := store.FindAlbum(context.Background(), 2)
	if err != nil {
		t.Fatalf("FindAlbum: %v", err)
	}
	if got.Title() != "Kind of Blue" {
		t.Fatalf("Title = %q, want Kind of Blue", got.Title())
	}
}
```

Create `example_test.go`:

```go
package dbbasics

import (
	"context"
	"database/sql/driver"
	"fmt"
)

func ExampleStore_FindAlbum() {
	sqlDB, fdb := newFakeDB("example-find")
	defer sqlDB.Close()

	fdb.addRow(
		[]string{"id", "title", "artist"},
		driver.Value(int64(1)),
		driver.Value("Blue Train"),
		driver.Value("John Coltrane"),
	)

	store := NewStore(sqlDB)
	album, _ := store.FindAlbum(context.Background(), 1)
	fmt.Println(album.Title())
	// Output: Blue Train
}
```

### Exercise 4: The Demo Binary

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"database/sql/driver"
	"fmt"
	"log"

	"example.com/dbbasics"
)

func main() {
	sqlDB, fdb := dbbasics.ExposedNewFakeDB("demo")
	defer sqlDB.Close()

	fdb.AddRow(
		[]string{"id", "title", "artist"},
		driver.Value(int64(1)),
		driver.Value("Blue Train"),
		driver.Value("John Coltrane"),
	)

	store := dbbasics.NewStore(sqlDB)
	album, err := store.FindAlbum(context.Background(), 1)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d: %s by %s\n", album.ID(), album.Title(), album.Artist())
}
```

Add one more test of your own: assert that `FindAlbum` with a cancelled context returns an error that wraps `context.Canceled`.

## Common Mistakes

Wrong: Expose `sql.ErrNoRows` directly to callers. What happens: callers must import `database/sql` just to check for missing rows, and the internal detail leaks into the public contract. Fix: translate `sql.ErrNoRows` into a package sentinel (`ErrNotFound`) before returning.

Wrong: Call `sql.Open` inside every handler or request. What happens: each call allocates a new pool, bypassing connection reuse, and the application runs out of file descriptors under load. Fix: open one `*sql.DB` at startup, pass it into a repository constructor, and reuse it.

Wrong: Call `row.Scan` without checking `sql.ErrNoRows`. What happens: if the query matches no rows, `Scan` returns `sql.ErrNoRows` and the destination variables remain at zero — silent wrong data. Fix: always check the error returned by `Scan`.

## Verification

From the module directory:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. `go test` is the gate; `cmd/demo` proves the exported API is usable from outside the package.

## Summary

- `sql.Open` validates the driver name and returns a pool handle; it does not dial.
- `QueryRowContext` errors surface at `row.Scan`, where `sql.ErrNoRows` signals no match.
- Translate internal errors into package sentinels so callers use `errors.Is` without importing driver packages.
- A fake in-process driver implements `driver.Driver`, `driver.Conn`, `driver.Stmt`, and `driver.Rows` and lets `database/sql` code run offline under `go test`.

## What's Next

Continue with [Row Scanning and Struct Mapping](../02-row-scanning-and-struct-mapping/02-row-scanning-and-struct-mapping.md).

## Resources

- [Go database guide](https://go.dev/doc/database/)
- [database/sql package reference](https://pkg.go.dev/database/sql)
- [database/sql/driver package reference](https://pkg.go.dev/database/sql/driver)
- [Go testing package reference](https://pkg.go.dev/testing)
