# Exercise 2: Storing Embeddings in Postgres with pgvector and pgx

You will build a `Store` that owns the pgvector schema and idempotent writes:
enabling the extension, registering the vector type on every pooled connection,
and upserting documents with `ON CONFLICT`. It picks `vector` versus `halfvec`
from the embedding dimension so a 3072-dimension model does not blow the index
limit, and it records the model that produced each vector. The pure schema logic
is stdlib-only and fully tested offline; the pgx and pgvector I/O lives behind a
build tag and runs against a real Postgres.

## What you'll build

```text
pgvecstore/                       independent module: example.com/pgvecstore
  go.mod                          go 1.26 (stdlib-only default build)
  store.go                        Document, ColumnType, SchemaSQL, UpsertSQL, sentinels
  store_pg.go                     //go:build integration — Store over pgxpool + pgvector
  cmd/
    demo/
      main.go                     offline demo: column-type selection + generated SQL
  store_test.go                   table-driven unit tests; sentinels via errors.Is; Example
  store_pg_integration_test.go    //go:build integration — real Postgres round trip + the trap
```

- Files: `store.go`, `store_pg.go`, `cmd/demo/main.go`, `store_test.go`, `store_pg_integration_test.go`.
- Implement: `ColumnType(dims)` (vector up to 2000, halfvec up to 4000, error otherwise), `SchemaSQL`, idempotent `UpsertSQL`, and a `Store` that wires `pgxvec.RegisterTypes` into `pgxpool.Config.AfterConnect`, enables the extension, and upserts documents.
- Test: table-driven unit tests for the column-type selector and SQL builders with sentinels via `errors.Is`; an integration test that round-trips a document and a second that proves omitting registration fails.
- Verify: `go test -count=1 -race ./...` (unit); `go test -tags integration ./...` with `PGVECTOR_TEST_DSN` set (integration).

Set up the module. The default build is stdlib-only; pgx and pgvector are needed
only for the `integration`-tagged files:

```bash
mkdir -p go-solutions/52-ai-llm-backends/04-embeddings-and-pgvector/02-pgvector-store/cmd/demo
cd go-solutions/52-ai-llm-backends/04-embeddings-and-pgvector/02-pgvector-store
go get github.com/jackc/pgx/v5@latest github.com/pgvector/pgvector-go@latest
```

### Choosing the column type is a decision, not a discovery

The single most important line of schema logic is picking the column type from
the dimension. A `vector(N)` column stores `float32` and its HNSW/IVFFlat indexes
cap at 2000 dimensions, because an index tuple must fit one 8 KB Postgres page. A
`halfvec(N)` column stores `float16` and reaches 4000. `text-embedding-3-large`
is 3072 dimensions, so stored as a plain `vector` you can hold it but you cannot
index it — the `CREATE INDEX` fails at 2000. `ColumnType` encodes that rule once:
`vector(N)` up to 2000, `halfvec(N)` up to 4000, and a wrapped sentinel error on
a non-positive or over-limit dimension, so the choice is made deliberately in
code rather than discovered as a production error.

The `Document` row carries a `model` column alongside the embedding. That is the
operational lifecycle made concrete: embeddings are model-versioned artifacts, so
recording which model produced each vector is what lets a future re-embed
migration run the old and new spaces side by side instead of guessing.

Create `store.go`:

```go
package store

import (
	"errors"
	"fmt"
)

const (
	// vectorIndexLimit is pgvector's HNSW/IVFFlat cap for the float32 vector
	// type: an index tuple must fit an 8KB page.
	vectorIndexLimit = 2000
	// halfvecIndexLimit is the cap for the float16 halfvec type.
	halfvecIndexLimit = 4000
)

// Sentinel errors, wrapped with %w so callers match with errors.Is.
var (
	ErrInvalidDimensions     = errors.New("store: dimensions must be positive")
	ErrUnsupportedDimensions = errors.New("store: dimensions exceed halfvec index limit")
	ErrDimensionMismatch     = errors.New("store: embedding dimension does not match column")
)

// Document is one row: a stable id, its text, the model that produced the
// embedding, and the embedding itself.
type Document struct {
	ID        string
	Content   string
	Model     string
	Embedding []float32
}

// ColumnType returns the pgvector column type for an embedding dimension:
// vector(N) up to 2000, halfvec(N) up to 4000, an error otherwise. The split
// exists because a plain vector index caps at 2000 dimensions.
func ColumnType(dims int) (string, error) {
	switch {
	case dims <= 0:
		return "", fmt.Errorf("%w: got %d", ErrInvalidDimensions, dims)
	case dims <= vectorIndexLimit:
		return fmt.Sprintf("vector(%d)", dims), nil
	case dims <= halfvecIndexLimit:
		return fmt.Sprintf("halfvec(%d)", dims), nil
	default:
		return "", fmt.Errorf("%w: got %d (max %d)", ErrUnsupportedDimensions, dims, halfvecIndexLimit)
	}
}

// UsesHalfVector reports whether dims requires the halfvec type.
func UsesHalfVector(dims int) bool {
	return dims > vectorIndexLimit && dims <= halfvecIndexLimit
}

// CreateExtensionSQL enables pgvector. It is idempotent.
const CreateExtensionSQL = "CREATE EXTENSION IF NOT EXISTS vector"

// SchemaSQL returns the CREATE TABLE statement for table, with an embedding
// column sized for dims. The model column records which model produced each
// vector.
func SchemaSQL(table string, dims int) (string, error) {
	col, err := ColumnType(dims)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s (id text PRIMARY KEY, content text NOT NULL, "+
			"model text NOT NULL, embedding %s NOT NULL)", table, col), nil
}

// UpsertSQL returns the idempotent insert-or-update statement for table, so a
// re-run of a backfill updates in place rather than duplicating rows.
func UpsertSQL(table string) string {
	return fmt.Sprintf(
		"INSERT INTO %s (id, content, model, embedding) VALUES ($1, $2, $3, $4) "+
			"ON CONFLICT (id) DO UPDATE SET "+
			"content = EXCLUDED.content, model = EXCLUDED.model, embedding = EXCLUDED.embedding",
		table)
}
```

### Registering the vector type on every pooled connection

The pgx trap is subtle because it fails late. pgvector's `Vector` implements
`driver.Valuer` and `sql.Scanner`, but pgx still needs the Postgres `vector` OID
registered on the connection with `pgxvec.RegisterTypes` before it can encode or
decode one. With a single connection you call it once; with a `pgxpool.Pool` you
must wire it into `Config.AfterConnect`, which runs on every connection the pool
opens — now and after any reconnect. Forget it and nothing breaks at startup; the
first `Upsert` that passes a `pgvector.Vector` fails with "unable to encode",
often under load when the pool grows a fresh connection. Registration also needs
the extension to exist, so `AfterConnect` runs `CREATE EXTENSION IF NOT EXISTS
vector` (idempotent) before registering. `Upsert` then picks `NewVector` or
`NewHalfVector` from the dimension; both are `driver.Valuer`, so they pass
straight through as query arguments.

Create `store_pg.go`:

```go
//go:build integration

package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

// Store owns the pgvector schema and idempotent writes for one table/dimension.
type Store struct {
	pool  *pgxpool.Pool
	table string
	dims  int
}

// Open connects a pool, wires pgvector type registration into every connection,
// enables the extension, and creates the schema. AfterConnect is the only
// correct place to register pgvector types for a pool: it runs on every pooled
// connection, including ones opened later.
func Open(ctx context.Context, dsn, table string, dims int) (*Store, error) {
	if _, err := ColumnType(dims); err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		// The extension must exist before the vector type can be registered.
		if _, err := conn.Exec(ctx, CreateExtensionSQL); err != nil {
			return err
		}
		return pgxvec.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}

	schema, err := SchemaSQL(table, dims)
	if err != nil {
		pool.Close()
		return nil, err
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &Store{pool: pool, table: table, dims: dims}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Upsert writes one document idempotently. The embedding is passed as a pgvector
// value (Vector or HalfVector by dimension); both implement driver.Valuer, so
// pgx encodes them once the types are registered.
func (s *Store) Upsert(ctx context.Context, doc Document) error {
	if s.dims > 0 && len(doc.Embedding) != s.dims {
		return fmt.Errorf("%w: want %d got %d", ErrDimensionMismatch, s.dims, len(doc.Embedding))
	}
	var arg any
	if UsesHalfVector(s.dims) {
		arg = pgvector.NewHalfVector(doc.Embedding)
	} else {
		arg = pgvector.NewVector(doc.Embedding)
	}
	if _, err := s.pool.Exec(ctx, UpsertSQL(s.table), doc.ID, doc.Content, doc.Model, arg); err != nil {
		return fmt.Errorf("upsert %s: %w", doc.ID, err)
	}
	return nil
}

// Get reads one document back, decoding the embedding into []float32.
func (s *Store) Get(ctx context.Context, id string) (Document, error) {
	var doc Document
	q := fmt.Sprintf("SELECT id, content, model, embedding FROM %s WHERE id = $1", s.table)
	row := s.pool.QueryRow(ctx, q, id)
	if UsesHalfVector(s.dims) {
		var vec pgvector.HalfVector
		if err := row.Scan(&doc.ID, &doc.Content, &doc.Model, &vec); err != nil {
			return Document{}, fmt.Errorf("get %s: %w", id, err)
		}
		doc.Embedding = vec.Slice()
		return doc, nil
	}
	var vec pgvector.Vector
	if err := row.Scan(&doc.ID, &doc.Content, &doc.Model, &vec); err != nil {
		return Document{}, fmt.Errorf("get %s: %w", id, err)
	}
	doc.Embedding = vec.Slice()
	return doc, nil
}
```

### The runnable demo

The demo is offline: it exercises the pure schema logic, showing the column-type
choice flip from `vector` to `halfvec` at 3072 dimensions and printing the
generated schema and upsert SQL.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pgvecstore"
)

func main() {
	for _, dims := range []int{1536, 3072} {
		col, err := store.ColumnType(dims)
		if err != nil {
			fmt.Printf("dim %d -> error: %v\n", dims, err)
			continue
		}
		fmt.Printf("dim %d -> %s\n", dims, col)
	}

	schema, _ := store.SchemaSQL("documents", 1536)
	fmt.Println("schema:", schema)
	fmt.Println("upsert:", store.UpsertSQL("documents"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
dim 1536 -> vector(1536)
dim 3072 -> halfvec(3072)
schema: CREATE TABLE IF NOT EXISTS documents (id text PRIMARY KEY, content text NOT NULL, model text NOT NULL, embedding vector(1536) NOT NULL)
upsert: INSERT INTO documents (id, content, model, embedding) VALUES ($1, $2, $3, $4) ON CONFLICT (id) DO UPDATE SET content = EXCLUDED.content, model = EXCLUDED.model, embedding = EXCLUDED.embedding
```

### Tests

The unit tests cover every branch of the column-type selector, including both
boundaries (2000 and 4000) and both error sentinels, asserted with `errors.Is`.
The SQL builders are checked for the clauses that make them correct: `ON CONFLICT
... DO UPDATE` for idempotency and the right column type in the schema.

Create `store_test.go`:

```go
package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestColumnType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		dims    int
		want    string
		wantErr error
	}{
		{"tiny", 1, "vector(1)", nil},
		{"small model", 1536, "vector(1536)", nil},
		{"vector boundary", 2000, "vector(2000)", nil},
		{"first halfvec", 2001, "halfvec(2001)", nil},
		{"large model", 3072, "halfvec(3072)", nil},
		{"halfvec boundary", 4000, "halfvec(4000)", nil},
		{"zero", 0, "", ErrInvalidDimensions},
		{"negative", -3, "", ErrInvalidDimensions},
		{"over limit", 4001, "", ErrUnsupportedDimensions},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ColumnType(tc.dims)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ColumnType(%d) = %q, want %q", tc.dims, got, tc.want)
			}
		})
	}
}

func TestUsesHalfVector(t *testing.T) {
	t.Parallel()
	cases := map[int]bool{1536: false, 2000: false, 2001: true, 4000: true, 4001: false}
	for dims, want := range cases {
		if got := UsesHalfVector(dims); got != want {
			t.Errorf("UsesHalfVector(%d) = %v, want %v", dims, got, want)
		}
	}
}

func TestUpsertSQLIsIdempotent(t *testing.T) {
	t.Parallel()
	sql := UpsertSQL("documents")
	for _, want := range []string{
		"INSERT INTO documents",
		"ON CONFLICT (id) DO UPDATE",
		"embedding = EXCLUDED.embedding",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("UpsertSQL missing %q in:\n%s", want, sql)
		}
	}
}

func TestSchemaSQL(t *testing.T) {
	t.Parallel()
	sql, err := SchemaSQL("documents", 3072)
	if err != nil {
		t.Fatalf("SchemaSQL: %v", err)
	}
	if !strings.Contains(sql, "embedding halfvec(3072)") {
		t.Fatalf("schema missing halfvec column:\n%s", sql)
	}
	if _, err := SchemaSQL("documents", 0); !errors.Is(err, ErrInvalidDimensions) {
		t.Fatalf("SchemaSQL(0) err = %v, want ErrInvalidDimensions", err)
	}
}

func ExampleColumnType() {
	small, _ := ColumnType(1536)
	large, _ := ColumnType(3072)
	fmt.Println(small)
	fmt.Println(large)
	// Output:
	// vector(1536)
	// halfvec(3072)
}
```

The integration test needs a real Postgres with pgvector installed. It reads the
DSN from `PGVECTOR_TEST_DSN` and skips when unset, so the default `go test` is
unaffected. `TestStoreRoundTrip` upserts a document twice (proving the update
path is idempotent) and reads it back. `TestMissingRegisterTypesFails` is the
trap made executable: a pool that never registers pgvector types cannot encode a
`pgvector.Vector` argument, so the insert must return an error.

Create `store_pg_integration_test.go`:

```go
//go:build integration

package store

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("PGVECTOR_TEST_DSN")
	if dsn == "" {
		t.Skip("PGVECTOR_TEST_DSN not set")
	}
	return dsn
}

func TestStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, testDSN(t), "test_docs", 4)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	if _, err := s.pool.Exec(ctx, "TRUNCATE test_docs"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	doc := Document{
		ID:        "d1",
		Content:   "hello",
		Model:     "text-embedding-3-small",
		Embedding: []float32{0.1, 0.2, 0.3, 0.4},
	}
	if err := s.Upsert(ctx, doc); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	doc.Content = "hello again" // second upsert updates in place
	if err := s.Upsert(ctx, doc); err != nil {
		t.Fatalf("Upsert (update): %v", err)
	}

	got, err := s.Get(ctx, "d1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Content != "hello again" || len(got.Embedding) != 4 {
		t.Fatalf("Get = %+v, want content 'hello again' and 4-dim embedding", got)
	}
}

func TestMissingRegisterTypesFails(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, CreateExtensionSQL); err != nil {
		t.Fatalf("create extension: %v", err)
	}
	schema, err := SchemaSQL("trap_docs", 4)
	if err != nil {
		t.Fatalf("SchemaSQL: %v", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		t.Fatalf("schema: %v", err)
	}

	// No pgxvec.RegisterTypes on this pool: encoding the Vector arg must fail.
	_, err = pool.Exec(ctx,
		"INSERT INTO trap_docs (id, content, model, embedding) VALUES ($1, $2, $3, $4)",
		"x", "c", "m", pgvector.NewVector([]float32{1, 2, 3, 4}))
	if err == nil {
		t.Fatal("expected an encode error without RegisterTypes, got nil")
	}
}
```

## Review

The store is correct when a document round-trips unchanged and a re-upsert
updates rather than duplicates. The idempotency lives entirely in `ON CONFLICT
(id) DO UPDATE`, which is why `TestStoreRoundTrip` upserts twice and expects one
row with the newer content; the unit test pins that clause so a refactor cannot
quietly drop it. The dimension-to-type mapping is proved at both boundaries: 2000
is the last `vector`, 2001 the first `halfvec`, 4000 the last supported, and
anything larger is a wrapped `ErrUnsupportedDimensions`.

The mistakes to avoid are the two that fail late. First, register the pgvector
types on every pooled connection via `AfterConnect`, not once at startup —
`TestMissingRegisterTypesFails` exists to show that a pool without registration
throws an encode error the moment a `Vector` argument is used. Second, do not
index a 3072-dimension embedding as a plain `vector`; `ColumnType` routes it to
`halfvec` precisely so the later `CREATE INDEX` does not hit the 2000-dimension
limit. Keep the `model` column populated: without it a re-embedding migration has
no way to tell old vectors from new, and vectors from different models are
silently incomparable. Run the unit suite with `-race`; run the integration
suite with `-tags integration` and `PGVECTOR_TEST_DSN` pointed at a Postgres that
has the `vector` extension available.

## Resources

- [pgvector — README](https://github.com/pgvector/pgvector) — column types, the 2000/4000 dimension limits, and `CREATE EXTENSION`.
- [pgvector-go — GitHub](https://github.com/pgvector/pgvector-go) — `NewVector`, `NewHalfVector`, and the pgx `RegisterTypes` helper.
- [pgx v5 — pkg.go.dev](https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool) — `pgxpool.ParseConfig`, `Config.AfterConnect`, and `NewWithConfig`.
- [halfvec in pgvector](https://github.com/pgvector/pgvector#half-precision-vectors) — storing high-dimension embeddings at float16.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-embedding-client.md](01-embedding-client.md) | Next: [03-similarity-search.md](03-similarity-search.md)
