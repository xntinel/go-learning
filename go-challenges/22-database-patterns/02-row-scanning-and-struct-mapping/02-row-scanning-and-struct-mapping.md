# 2. Row Scanning and Struct Mapping

Row scanning is where type contracts between SQL and Go are enforced. The common failure modes — wrong column order, mismatched types, unhandled nullable columns — are invisible until runtime. This lesson builds a product repository that scans multiple rows into structs and handles type coercion explicitly.

```text
rowscan/
  go.mod
  driver.go        (fake sql driver — the test seam)
  store.go         (package rowscan: Product, Store, multi-row query)
  store_test.go
  example_test.go
  cmd/demo/main.go
```

## Concepts

### `rows.Scan` Is Order-Dependent

`(*sql.Rows).Scan` maps columns positionally to destination pointers. If your `SELECT` returns `id, name, price` and you scan into `&p.Name, &p.ID, &p.Price`, the data silently populates the wrong fields. Column names in the result set do not drive the mapping — only position does. A type mismatch between a driver value and the destination pointer produces an error at scan time, not at compile time.

### Multi-Row Queries Use `db.QueryContext`

`QueryRowContext` consumes exactly one row. For multi-row results use `db.QueryContext`, which returns `*sql.Rows`. The idiom is:

```go
rows, err := db.QueryContext(ctx, query, args...)
if err != nil { ... }
defer rows.Close()
for rows.Next() {
    var p Product
    if err := rows.Scan(&p.ID, &p.Name, &p.Price); err != nil { ... }
    results = append(results, p)
}
if err := rows.Err(); err != nil { ... }
```

`rows.Err()` catches errors that occur during iteration (network interruptions, partial reads). Omitting it means silent data loss.

### `rows.Close` Must Be Deferred Immediately

After `db.QueryContext`, call `defer rows.Close()` before any error check on the first `rows.Next`. If you skip the defer and return early on a scan error, the connection leaks back into the pool in a dirty state. Defer closes the rows even on early returns.

### Destination Pointers Must Match Driver Types

The `database/sql` driver converts its internal values to Go types based on what the pointer accepts. Scanning a `driver.Value` of type `int64` into `*int` fails. Scanning into `*int64` succeeds. Use the types that match what the driver produces; add explicit casts in accessors if the public API needs a different type.

## Exercises

Set up the module:

```bash
go mod edit -go=1.26
```

This is a library package verified by `go test`.

### Exercise 1: The Fake Driver

Create `driver.go`:

```go
package rowscan

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

### Exercise 2: Product Repository with Multi-Row Scan

Create `store.go`:

```go
package rowscan

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var (
	// ErrEmptyName is returned by NewProduct when the name is blank.
	ErrEmptyName = errors.New("product name must not be empty")
	// ErrNegativePrice is returned by NewProduct for non-positive prices.
	ErrNegativePrice = errors.New("product price must be positive")
)

// Product is a value type. Accessors prevent callers from building invalid
// Products by direct field assignment.
type Product struct {
	id    int64
	name  string
	price int64 // price in cents to avoid floating-point errors
}

func (p Product) ID() int64    { return p.id }
func (p Product) Name() string { return p.name }

// PriceCents returns the price in the smallest currency unit.
func (p Product) PriceCents() int64 { return p.price }

// NewProduct validates and constructs a Product.
func NewProduct(id int64, name string, priceCents int64) (Product, error) {
	if name == "" {
		return Product{}, fmt.Errorf("product: %w", ErrEmptyName)
	}
	if priceCents <= 0 {
		return Product{}, fmt.Errorf("product: %w", ErrNegativePrice)
	}
	return Product{id: id, name: name, price: priceCents}, nil
}

// Store wraps a *sql.DB and exposes repository methods.
type Store struct {
	db *sql.DB
}

// NewStore creates a Store backed by the given *sql.DB.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// ListAll returns every product in the result set.
// Column order in SELECT must match the Scan destination order.
func (s *Store) ListAll(ctx context.Context) ([]Product, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, price FROM products")
	if err != nil {
		return nil, fmt.Errorf("list all: %w", err)
	}
	defer rows.Close()

	var results []Product
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.id, &p.name, &p.price); err != nil {
			return nil, fmt.Errorf("scan product: %w", err)
		}
		results = append(results, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}
	return results, nil
}

// FindByName returns all products whose name equals the given string.
func (s *Store) FindByName(ctx context.Context, name string) ([]Product, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, name, price FROM products WHERE name = ?", name)
	if err != nil {
		return nil, fmt.Errorf("find by name: %w", err)
	}
	defer rows.Close()

	var results []Product
	for rows.Next() {
		var p Product
		if err := rows.Scan(&p.id, &p.name, &p.price); err != nil {
			return nil, fmt.Errorf("scan product: %w", err)
		}
		results = append(results, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}
	return results, nil
}
```

### Exercise 3: Table-Driven Tests

Create `store_test.go`:

```go
package rowscan

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
)

func TestListAll(t *testing.T) {
	t.Parallel()

	sqlDB, fdb := newFakeDB("list-all")
	defer sqlDB.Close()

	cols := []string{"id", "name", "price"}
	fdb.addRow(cols, driver.Value(int64(1)), driver.Value("Widget"), driver.Value(int64(999)))
	fdb.addRow(cols, driver.Value(int64(2)), driver.Value("Gadget"), driver.Value(int64(1499)))

	store := NewStore(sqlDB)
	products, err := store.ListAll(context.Background())
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(products) != 2 {
		t.Fatalf("len = %d, want 2", len(products))
	}
	if products[0].Name() != "Widget" {
		t.Fatalf("products[0].Name = %q, want Widget", products[0].Name())
	}
	if products[1].PriceCents() != 1499 {
		t.Fatalf("products[1].PriceCents = %d, want 1499", products[1].PriceCents())
	}
}

func TestListAllEmpty(t *testing.T) {
	t.Parallel()

	sqlDB, _ := newFakeDB("list-all-empty")
	defer sqlDB.Close()

	store := NewStore(sqlDB)
	products, err := store.ListAll(context.Background())
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(products) != 0 {
		t.Fatalf("len = %d, want 0", len(products))
	}
}

func TestNewProductRejectsEmptyName(t *testing.T) {
	t.Parallel()

	_, err := NewProduct(1, "", 100)
	if !errors.Is(err, ErrEmptyName) {
		t.Fatalf("err = %v, want ErrEmptyName", err)
	}
}

func TestNewProductRejectsNonPositivePrice(t *testing.T) {
	t.Parallel()

	cases := []int64{0, -1, -999}
	for _, price := range cases {
		_, err := NewProduct(1, "Widget", price)
		if !errors.Is(err, ErrNegativePrice) {
			t.Fatalf("price %d: err = %v, want ErrNegativePrice", price, err)
		}
	}
}
```

Create `example_test.go`:

```go
package rowscan

import (
	"context"
	"database/sql/driver"
	"fmt"
)

func ExampleStore_ListAll() {
	sqlDB, fdb := newFakeDB("example-list")
	defer sqlDB.Close()

	cols := []string{"id", "name", "price"}
	fdb.addRow(cols, driver.Value(int64(1)), driver.Value("Widget"), driver.Value(int64(999)))

	store := NewStore(sqlDB)
	products, _ := store.ListAll(context.Background())
	fmt.Println(products[0].Name())
	// Output: Widget
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

	"example.com/rowscan"
)

func main() {
	sqlDB, fdb := rowscan.ExposedNewFakeDB("demo")
	defer sqlDB.Close()

	cols := []string{"id", "name", "price"}
	fdb.AddRow(cols, driver.Value(int64(1)), driver.Value("Widget"), driver.Value(int64(999)))
	fdb.AddRow(cols, driver.Value(int64(2)), driver.Value("Gadget"), driver.Value(int64(1499)))

	store := rowscan.NewStore(sqlDB)
	products, err := store.ListAll(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	for _, p := range products {
		fmt.Printf("%d: %s — %d cents\n", p.ID(), p.Name(), p.PriceCents())
	}
}
```

Add one more test of your own: call `ListAll` after seeding three rows and assert the returned slice has length three and the third product has the correct name.

## Common Mistakes

Wrong: Omit `defer rows.Close()` after a successful `QueryContext`. What happens: the underlying connection is not returned to the pool until GC runs, eventually exhausting the pool under load. Fix: always `defer rows.Close()` immediately after checking the `QueryContext` error.

Wrong: Omit `rows.Err()` after the loop. What happens: if the server terminates the stream mid-iteration, the loop ends silently with partial data and no error signal. Fix: check `rows.Err()` after every `for rows.Next()` loop.

Wrong: Scan columns in a different order than the SELECT lists them. What happens: values land in the wrong struct fields; the type might match by accident so there is no scan error, only wrong data. Fix: keep the SELECT list and the `Scan` argument list in the same order.

## Verification

From the module directory:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

- `db.QueryContext` returns `*sql.Rows` for multi-row results; `QueryRowContext` is for single-row queries.
- Always `defer rows.Close()` immediately after the successful call to `QueryContext`.
- `rows.Scan` is positional; column order in `SELECT` must match the destination pointer order.
- Check `rows.Err()` after the loop to catch errors that occurred during iteration.

## What's Next

Continue with [Connection Pool Configuration](../03-connection-pool-configuration/03-connection-pool-configuration.md).

## Resources

- [Go database guide: querying](https://go.dev/doc/database/querying)
- [database/sql package reference](https://pkg.go.dev/database/sql)
- [database/sql/driver package reference](https://pkg.go.dev/database/sql/driver)
- [Go testing package reference](https://pkg.go.dev/testing)
