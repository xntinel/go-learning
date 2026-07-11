# 4. Prepared Statements

Prepared statements are SQL plans compiled once and executed many times. The key discipline is closing them — a leaked `*sql.Stmt` holds a server-side cursor indefinitely. This lesson builds a user repository that caches one prepared statement and closes it correctly.

```text
dbstmt/
  go.mod
  driver.go        (fake sql driver — the test seam)
  store.go         (package dbstmt: User, Store, prepared lookup)
  store_test.go
  example_test.go
  cmd/demo/main.go
```

## Concepts

### When to Use Prepared Statements

`db.PrepareContext` sends the SQL to the server for parsing and returns a `*sql.Stmt` that can be executed many times without re-parsing. The benefit is worth the extra round trip only when the same SQL shape is executed repeatedly — tight loops, frequent parameterized lookups. For one-off queries, `db.QueryRowContext` is simpler.

### `*sql.Stmt` Holds a Connection Slot

A `*sql.Stmt` does not hold a dedicated connection, but each `stmt.QueryRowContext` call acquires a connection from the pool for the duration of the query. If `stmt.Close` is never called the driver-side resource (a cursor or prepared plan ID) is never released to the server. Defer `stmt.Close` immediately after `PrepareContext`.

### Prepared Statements and Connection Pools

`database/sql` transparently re-prepares a statement on a new connection if the original connection is closed or recycled. This is invisible to the caller but means the server can see more prepared plans than you expect under pool churn. For databases with strict prepared-statement limits, this matters.

### `stmt.QueryRowContext` vs `db.QueryRowContext`

Both accept the same context and argument list. The difference is that `stmt.QueryRowContext` reuses the parsed plan; `db.QueryRowContext` re-parses the SQL each time. The API shape is identical so migrating from one to the other is a one-line change.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/dbstmt/cmd/demo
cd ~/go-exercises/dbstmt
go mod init example.com/dbstmt
go mod edit -go=1.26
```

This is a library package verified by `go test`.

### Exercise 1: The Fake Driver

Create `driver.go`:

```go
package dbstmt

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

### Exercise 2: User Repository with Cached Prepared Statement

Create `store.go`:

```go
package dbstmt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var (
	// ErrEmptyEmail is returned when the email is blank.
	ErrEmptyEmail = errors.New("email must not be empty")
	// ErrNotFound is returned when no user matches the query.
	ErrNotFound = errors.New("user not found")
)

// User is a value type with unexported fields and exported accessors.
type User struct {
	id    int64
	email string
	role  string
}

func (u User) ID() int64     { return u.id }
func (u User) Email() string { return u.email }
func (u User) Role() string  { return u.role }

// Store holds a *sql.DB and a cached prepared statement.
// Call Close to release the prepared statement when the Store is no longer needed.
type Store struct {
	db       *sql.DB
	findStmt *sql.Stmt
}

// NewStore prepares the lookup statement. Returns an error if preparation fails.
func NewStore(ctx context.Context, db *sql.DB) (*Store, error) {
	stmt, err := db.PrepareContext(ctx,
		"SELECT id, email, role FROM users WHERE email = ?")
	if err != nil {
		return nil, fmt.Errorf("prepare find user: %w", err)
	}
	return &Store{db: db, findStmt: stmt}, nil
}

// Close releases the prepared statement. Call it when the Store is done.
func (s *Store) Close() error {
	return s.findStmt.Close()
}

// FindByEmail executes the pre-prepared query. Missing rows become ErrNotFound.
func (s *Store) FindByEmail(ctx context.Context, email string) (User, error) {
	if email == "" {
		return User{}, fmt.Errorf("find user: %w", ErrEmptyEmail)
	}
	row := s.findStmt.QueryRowContext(ctx, email)
	var u User
	if err := row.Scan(&u.id, &u.email, &u.role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, fmt.Errorf("find user %q: %w", email, ErrNotFound)
		}
		return User{}, fmt.Errorf("find user %q: %w", email, err)
	}
	return u, nil
}
```

### Exercise 3: Table-Driven Tests

Create `store_test.go`:

```go
package dbstmt

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
)

func TestFindByEmail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		email   string
		seed    bool
		want    string
		wantErr error
	}{
		{
			name:  "found",
			email: "alice@example.com",
			seed:  true,
			want:  "admin",
		},
		{
			name:    "not found",
			email:   "nobody@example.com",
			seed:    false,
			wantErr: ErrNotFound,
		},
		{
			name:    "empty email",
			email:   "",
			seed:    false,
			wantErr: ErrEmptyEmail,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sqlDB, fdb := newFakeDB("find-" + tt.name)
			defer sqlDB.Close()

			if tt.seed {
				fdb.addRow(
					[]string{"id", "email", "role"},
					driver.Value(int64(1)),
					driver.Value("alice@example.com"),
					driver.Value("admin"),
				)
			}

			store, err := NewStore(context.Background(), sqlDB)
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			defer store.Close()

			u, err := store.FindByEmail(context.Background(), tt.email)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("FindByEmail err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && u.Role() != tt.want {
				t.Fatalf("Role = %q, want %q", u.Role(), tt.want)
			}
		})
	}
}

func TestStoreCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	sqlDB, _ := newFakeDB("close-test")
	defer sqlDB.Close()

	store, err := NewStore(context.Background(), sqlDB)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// Close once — must not panic or error.
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
```

Create `example_test.go`:

```go
package dbstmt

import (
	"context"
	"database/sql/driver"
	"fmt"
)

func ExampleStore_FindByEmail() {
	sqlDB, fdb := newFakeDB("example-stmt")
	defer sqlDB.Close()

	fdb.addRow(
		[]string{"id", "email", "role"},
		driver.Value(int64(1)),
		driver.Value("alice@example.com"),
		driver.Value("admin"),
	)

	store, _ := NewStore(context.Background(), sqlDB)
	defer store.Close()

	u, _ := store.FindByEmail(context.Background(), "alice@example.com")
	fmt.Println(u.Role())
	// Output: admin
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

	"example.com/dbstmt"
)

func main() {
	sqlDB, fdb := dbstmt.ExposedNewFakeDB("demo")
	defer sqlDB.Close()

	fdb.AddRow(
		[]string{"id", "email", "role"},
		driver.Value(int64(1)),
		driver.Value("alice@example.com"),
		driver.Value("admin"),
	)

	ctx := context.Background()
	store, err := dbstmt.NewStore(ctx, sqlDB)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	u, err := store.FindByEmail(ctx, "alice@example.com")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d: %s (%s)\n", u.ID(), u.Email(), u.Role())
}
```

Add one more test of your own: call `FindByEmail` with a valid email against an empty database and assert the error wraps `ErrNotFound`.

## Common Mistakes

Wrong: Never call `stmt.Close`. What happens: each prepared statement holds a server-side cursor open. Under load, the server hits its prepared-statement limit and begins refusing new connections. Fix: defer `stmt.Close()` immediately after `PrepareContext` succeeds, or call it in a repository `Close` method.

Wrong: Prepare every query inside the per-request handler. What happens: each request sends an extra round trip for statement preparation, and each creates a new server-side plan that is immediately discarded. Fix: prepare statements once at startup and cache them in the repository struct.

Wrong: Ignore the error from `stmt.Close`. What happens: errors such as "connection lost" during close are silently dropped, masking driver state problems. Fix: check and log the error even if you cannot act on it.

## Verification

From the module directory:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

- `db.PrepareContext` compiles the SQL once and returns a `*sql.Stmt` for repeated execution.
- Always close `*sql.Stmt` — defer `stmt.Close()` immediately after the successful `PrepareContext` call.
- `stmt.QueryRowContext` has the same API as `db.QueryRowContext`; migrating between them is a one-line change.
- Cache statements in the repository struct and close them in a `Close()` method to keep the lifecycle clear.

## What's Next

Continue with [Transactions](../05-transactions/05-transactions.md).

## Resources

- [Go database guide: prepared statements](https://go.dev/doc/database/prepared-statements)
- [database/sql.Stmt](https://pkg.go.dev/database/sql#Stmt)
- [database/sql package reference](https://pkg.go.dev/database/sql)
- [Go testing package reference](https://pkg.go.dev/testing)
