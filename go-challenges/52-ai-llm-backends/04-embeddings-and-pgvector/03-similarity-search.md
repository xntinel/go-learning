# Exercise 3: Top-K Similarity Search and ANN Indexing

You will build the query side: a KNN search that returns the top-k nearest
documents using the correct distance operator for a chosen metric, converts each
raw distance into a 0..1 similarity score, and provisions a matching ANN index
with the right operator class. It exposes the recall/latency knob and supports a
metadata pre-filter. The metric-to-operator mapping and the distance conversion
are pure and fully tested offline; the pgx query path lives behind a build tag.

## What you'll build

```text
vecsearch/                        independent module: example.com/vecsearch
  go.mod                          go 1.26 (stdlib-only default build)
  search.go                       Metric, OperatorFor, OperatorClassFor, similarity, SQL builders
  search_pg.go                    //go:build integration — Search over pgxpool + pgx.CollectRows
  cmd/
    demo/
      main.go                     offline demo: operators, classes, generated SQL, similarity
  search_test.go                  table-driven unit tests; sentinels via errors.Is; Example
  search_pg_integration_test.go   //go:build integration — HNSW ranking + pre-filter + your-turn
```

- Files: `search.go`, `search_pg.go`, `cmd/demo/main.go`, `search_test.go`, `search_pg_integration_test.go`.
- Implement: `OperatorFor`, `OperatorClassFor`, `SimilarityFromDistance`, `BuildKNNSQL` (with the `ORDER BY ... LIMIT` shape and optional model pre-filter), `BuildIndexSQL` (HNSW / IVFFlat), and a `Search` that acquires one connection, sets `hnsw.ef_search`, queries, and scores.
- Test: table-driven unit tests for the operator/operator-class mapping and the distance-to-similarity conversion with edge cases; an integration test asserting the known-nearest document ranks first, ascending distance order, and a respected pre-filter.
- Verify: `go test -count=1 -race ./...` (unit); `go test -tags integration ./...` with `PGVECTOR_TEST_DSN` set (integration).

Set up the module:

```bash
go get github.com/jackc/pgx/v5@latest github.com/pgvector/pgvector-go@latest
```

### The operator must match the index, and distance is not similarity

Two correctness facts sit at the center of this exercise. First, the query
operator must match the operator class the index was built with, or pgvector
silently ignores the index and does an exact sequential scan. `OperatorFor`
returns `<=>` for cosine, `<->` for L2, and `<#>` for negative inner product;
`OperatorClassFor` returns the parallel `vector_cosine_ops` / `vector_l2_ops` /
`vector_ip_ops` (or the `halfvec_*` family), and the two are meant to be chosen
together so a mismatch is hard to write. Second, pgvector returns and sorts by
*distance* — smaller is closer — but a caller wants a *similarity* where larger
is better. `SimilarityFromDistance` derives it: `1 - distance` for cosine, the
negation `-distance` for the `<#>` operator (which returns the negated inner
product), and a monotonic `1/(1+distance)` for L2. Reporting the raw distance as
a score, so the best hit shows the lowest number, is one of the most common
vector-search bugs, and no type checker catches it.

The other load-bearing detail is the query *shape*. An ANN index only engages
for `ORDER BY column <op> $1 LIMIT k`. Drop the `LIMIT` and the planner must sort
the whole table, falling back to an exact scan; that is why `BuildKNNSQL` always
emits the `LIMIT`. The query also projects only `id`, `content`, and the computed
`distance` — never the embedding column itself, which would ship thousands of
floats per row that the caller never displays.

Create `search.go`:

```go
package search

import (
	"errors"
	"fmt"
)

// Metric is the distance measure for a search.
type Metric string

const (
	MetricCosine       Metric = "cosine"
	MetricL2           Metric = "l2"
	MetricInnerProduct Metric = "ip"
)

// IndexType selects the ANN index family.
type IndexType string

const (
	IndexHNSW    IndexType = "hnsw"
	IndexIVFFlat IndexType = "ivfflat"
)

// Sentinel errors, wrapped with %w so callers match with errors.Is.
var (
	ErrUnknownMetric    = errors.New("search: unknown metric")
	ErrUnknownIndexType = errors.New("search: unknown index type")
	ErrInvalidLists     = errors.New("search: ivfflat lists must be positive")
	ErrInvalidK         = errors.New("search: k must be positive")
)

// OperatorFor returns the pgvector distance operator for a metric.
func OperatorFor(m Metric) (string, error) {
	switch m {
	case MetricCosine:
		return "<=>", nil
	case MetricL2:
		return "<->", nil
	case MetricInnerProduct:
		return "<#>", nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownMetric, m)
	}
}

// OperatorClassFor returns the index operator class for a metric. It must match
// the query operator or the index is ignored. half selects the halfvec_* family.
func OperatorClassFor(m Metric, half bool) (string, error) {
	prefix := "vector"
	if half {
		prefix = "halfvec"
	}
	switch m {
	case MetricCosine:
		return prefix + "_cosine_ops", nil
	case MetricL2:
		return prefix + "_l2_ops", nil
	case MetricInnerProduct:
		return prefix + "_ip_ops", nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownMetric, m)
	}
}

// SimilarityFromDistance converts a raw pgvector distance into a score where
// larger is better. pgvector sorts by distance ascending; a user-facing score is
// a derived quantity, and reporting the raw distance is a common bug.
func SimilarityFromDistance(m Metric, distance float64) float64 {
	switch m {
	case MetricCosine:
		// cosine distance in [0,2]; similarity = 1 - distance, in [-1,1].
		return 1 - distance
	case MetricInnerProduct:
		// <#> returns the NEGATIVE inner product; negate to recover it.
		return -distance
	case MetricL2:
		// L2 distance in [0, inf); map monotonically into (0,1].
		return 1 / (1 + distance)
	default:
		return 0
	}
}

// BuildKNNSQL builds a top-k nearest-neighbour query. The ORDER BY ... LIMIT
// shape with a matching operator class is what lets pgvector use the ANN index;
// dropping the LIMIT forces an exact scan. It projects id, content, and the
// computed distance only, never the embedding. modelFilter adds a $3 pre-filter.
func BuildKNNSQL(table, op string, modelFilter bool) string {
	where := ""
	if modelFilter {
		where = " WHERE model = $3"
	}
	return fmt.Sprintf(
		"SELECT id, content, embedding %s $1 AS distance FROM %s%s ORDER BY embedding %s $1 LIMIT $2",
		op, table, where, op)
}

// BuildIndexSQL builds the ANN index DDL. HNSW builds a good graph without data
// present; IVFFlat learns its `lists` centroids from data, so build it AFTER
// loading representative rows.
func BuildIndexSQL(name, table, opclass string, it IndexType, lists int) (string, error) {
	switch it {
	case IndexHNSW:
		return fmt.Sprintf("CREATE INDEX %s ON %s USING hnsw (embedding %s)", name, table, opclass), nil
	case IndexIVFFlat:
		if lists <= 0 {
			return "", fmt.Errorf("%w: got %d", ErrInvalidLists, lists)
		}
		return fmt.Sprintf("CREATE INDEX %s ON %s USING ivfflat (embedding %s) WITH (lists = %d)",
			name, table, opclass, lists), nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownIndexType, it)
	}
}
```

### Running the query on one connection

The search itself carries two production details. First, `hnsw.ef_search` — the
recall/latency knob — is a session setting, so the `SET` and the query must run
on the *same* connection; with a pool that means `pool.Acquire` one connection,
`SET` on it, query on it, release it. Issuing the `SET` on the pool directly can
land on a different connection than the query and do nothing. Second, decoding
uses `pgx.CollectRows` with `pgx.RowToStructByName[Result]`, which maps the
`id`, `content`, and `distance` columns to the struct by name; the `Similarity`
field is tagged `db:"-"` and filled afterwards from `SimilarityFromDistance`.

Create `search_pg.go`:

```go
//go:build integration

package search

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

// Result is one ranked hit. Distance is what pgvector returns; Similarity is the
// derived score the caller should display.
type Result struct {
	ID         string  `db:"id"`
	Content    string  `db:"content"`
	Distance   float64 `db:"distance"`
	Similarity float64 `db:"-"`
}

// SearchParams configures one KNN query.
type SearchParams struct {
	Table    string
	Metric   Metric
	Query    []float32
	K        int
	EFSearch int    // 0 = server default; sets hnsw.ef_search for this query
	Model    string // non-empty = pre-filter on the model column
}

// Search runs a top-k nearest-neighbour query. It acquires one connection so the
// SET hnsw.ef_search and the query share a session, then scores each hit.
func Search(ctx context.Context, pool *pgxpool.Pool, p SearchParams) ([]Result, error) {
	if p.K <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidK, p.K)
	}
	op, err := OperatorFor(p.Metric)
	if err != nil {
		return nil, err
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	if p.EFSearch > 0 {
		// ef_search trades recall for latency and applies to the session.
		if _, err := conn.Exec(ctx, "SET hnsw.ef_search = "+strconv.Itoa(p.EFSearch)); err != nil {
			return nil, fmt.Errorf("set ef_search: %w", err)
		}
	}

	sql := BuildKNNSQL(p.Table, op, p.Model != "")
	args := []any{pgvector.NewVector(p.Query), p.K}
	if p.Model != "" {
		args = append(args, p.Model)
	}

	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("knn query: %w", err)
	}
	results, err := pgx.CollectRows(rows, pgx.RowToStructByName[Result])
	if err != nil {
		return nil, fmt.Errorf("collect: %w", err)
	}
	for i := range results {
		results[i].Similarity = SimilarityFromDistance(p.Metric, results[i].Distance)
	}
	return results, nil
}
```

### The runnable demo

The demo is offline: it prints each metric's operator and operator class
side by side (so the pairing is visible), the generated KNN and index SQL, and a
distance-to-similarity conversion.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/vecsearch"
)

func main() {
	for _, m := range []search.Metric{search.MetricCosine, search.MetricL2, search.MetricInnerProduct} {
		op, _ := search.OperatorFor(m)
		oc, _ := search.OperatorClassFor(m, false)
		fmt.Printf("%-4s operator %s  class %s\n", m, op, oc)
	}

	op, _ := search.OperatorFor(search.MetricCosine)
	fmt.Println("knn:", search.BuildKNNSQL("documents", op, false))

	idx, _ := search.BuildIndexSQL("documents_cos", "documents", "vector_cosine_ops", search.IndexHNSW, 0)
	fmt.Println("index:", idx)

	fmt.Printf("cosine distance 0.20 -> similarity %.2f\n", search.SimilarityFromDistance(search.MetricCosine, 0.20))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cosine operator <=>  class vector_cosine_ops
l2   operator <->  class vector_l2_ops
ip   operator <#>  class vector_ip_ops
knn: SELECT id, content, embedding <=> $1 AS distance FROM documents ORDER BY embedding <=> $1 LIMIT $2
index: CREATE INDEX documents_cos ON documents USING hnsw (embedding vector_cosine_ops)
cosine distance 0.20 -> similarity 0.80
```

### Tests

The unit tests pin the two mappings and the conversion. `TestSimilarityFromDistance`
covers the edges that matter: cosine distance 0 is similarity 1, distance 1 is 0,
distance 2 (opposite) is -1; the inner-product case shows the negation; L2 shows
the monotonic map. `TestBuildKNNSQL` asserts the `ORDER BY ... LIMIT` shape and
that the pre-filter adds the `WHERE`.

Create `search_test.go`:

```go
package search

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestOperatorFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		m       Metric
		want    string
		wantErr error
	}{
		{MetricCosine, "<=>", nil},
		{MetricL2, "<->", nil},
		{MetricInnerProduct, "<#>", nil},
		{Metric("bogus"), "", ErrUnknownMetric},
	}
	for _, tc := range tests {
		t.Run(string(tc.m), func(t *testing.T) {
			t.Parallel()
			got, err := OperatorFor(tc.m)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("OperatorFor(%q) = %q,%v want %q,nil", tc.m, got, err, tc.want)
			}
		})
	}
}

func TestOperatorClassFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		m    Metric
		half bool
		want string
	}{
		{MetricCosine, false, "vector_cosine_ops"},
		{MetricL2, false, "vector_l2_ops"},
		{MetricInnerProduct, false, "vector_ip_ops"},
		{MetricCosine, true, "halfvec_cosine_ops"},
		{MetricL2, true, "halfvec_l2_ops"},
	}
	for _, tc := range tests {
		got, err := OperatorClassFor(tc.m, tc.half)
		if err != nil || got != tc.want {
			t.Errorf("OperatorClassFor(%q,%v) = %q,%v want %q", tc.m, tc.half, got, err, tc.want)
		}
	}
	if _, err := OperatorClassFor(Metric("bogus"), false); !errors.Is(err, ErrUnknownMetric) {
		t.Errorf("expected ErrUnknownMetric for bogus metric")
	}
}

func TestSimilarityFromDistance(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		m    Metric
		dist float64
		want float64
	}{
		{"cosine identical", MetricCosine, 0, 1},
		{"cosine orthogonal", MetricCosine, 1, 0},
		{"cosine opposite", MetricCosine, 2, -1},
		{"inner product", MetricInnerProduct, -0.8, 0.8},
		{"l2 zero", MetricL2, 0, 1},
		{"l2 three", MetricL2, 3, 0.25},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SimilarityFromDistance(tc.m, tc.dist); math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("SimilarityFromDistance(%q,%v) = %v, want %v", tc.m, tc.dist, got, tc.want)
			}
		})
	}
}

func TestBuildKNNSQL(t *testing.T) {
	t.Parallel()
	op, _ := OperatorFor(MetricCosine)
	sql := BuildKNNSQL("docs", op, false)
	for _, want := range []string{"embedding <=> $1 AS distance", "ORDER BY embedding <=> $1", "LIMIT $2"} {
		if !strings.Contains(sql, want) {
			t.Errorf("KNN SQL missing %q in:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "WHERE") {
		t.Errorf("unfiltered KNN SQL should have no WHERE:\n%s", sql)
	}
	if filtered := BuildKNNSQL("docs", op, true); !strings.Contains(filtered, "WHERE model = $3") {
		t.Errorf("filtered KNN SQL missing WHERE:\n%s", filtered)
	}
}

func TestBuildIndexSQL(t *testing.T) {
	t.Parallel()
	hnsw, err := BuildIndexSQL("i", "docs", "vector_cosine_ops", IndexHNSW, 0)
	if err != nil {
		t.Fatalf("hnsw: %v", err)
	}
	if !strings.Contains(hnsw, "USING hnsw (embedding vector_cosine_ops)") {
		t.Errorf("bad hnsw ddl: %s", hnsw)
	}
	ivf, err := BuildIndexSQL("i", "docs", "vector_l2_ops", IndexIVFFlat, 100)
	if err != nil {
		t.Fatalf("ivfflat: %v", err)
	}
	if !strings.Contains(ivf, "USING ivfflat (embedding vector_l2_ops) WITH (lists = 100)") {
		t.Errorf("bad ivfflat ddl: %s", ivf)
	}
	if _, err := BuildIndexSQL("i", "docs", "vector_l2_ops", IndexIVFFlat, 0); !errors.Is(err, ErrInvalidLists) {
		t.Errorf("expected ErrInvalidLists for lists=0")
	}
}

func ExampleSimilarityFromDistance() {
	fmt.Printf("%.2f\n", SimilarityFromDistance(MetricCosine, 0.25))
	// Output: 0.75
}
```

The integration test seeds a three-axis corpus, builds an HNSW cosine index, and
asserts the known-nearest document ranks first with results ordered by ascending
distance. A second case proves the pre-filter respects its `WHERE`. The final
test is yours to complete.

Create `search_pg_integration_test.go`:

```go
//go:build integration

package search

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

func setupPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("PGVECTOR_TEST_DSN")
	if dsn == "" {
		t.Skip("PGVECTOR_TEST_DSN not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
			return err
		}
		return pgxvec.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, s := range []string{
		"DROP TABLE IF EXISTS knn_docs",
		"CREATE TABLE knn_docs (id text PRIMARY KEY, content text NOT NULL, model text NOT NULL, embedding vector(3) NOT NULL)",
	} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("seed ddl: %v", err)
		}
	}
	docs := []struct {
		id, content, model string
		vec                []float32
	}{
		{"x", "along x", "m1", []float32{1, 0, 0}},
		{"y", "along y", "m1", []float32{0, 1, 0}},
		{"z", "along z", "m2", []float32{0, 0, 1}},
	}
	for _, d := range docs {
		if _, err := pool.Exec(ctx,
			"INSERT INTO knn_docs (id, content, model, embedding) VALUES ($1, $2, $3, $4)",
			d.id, d.content, d.model, pgvector.NewVector(d.vec)); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	}
	if _, err := pool.Exec(ctx,
		"CREATE INDEX knn_docs_cos ON knn_docs USING hnsw (embedding vector_cosine_ops)"); err != nil {
		t.Fatalf("create index: %v", err)
	}
}

func TestSearchRanksNearestFirst(t *testing.T) {
	pool := setupPool(t)
	seed(t, pool)
	res, err := Search(context.Background(), pool, SearchParams{
		Table:  "knn_docs",
		Metric: MetricCosine,
		Query:  []float32{0.9, 0.1, 0},
		K:      3,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("got %d results, want 3", len(res))
	}
	if res[0].ID != "x" {
		t.Fatalf("nearest = %q, want x", res[0].ID)
	}
	for i := 1; i < len(res); i++ {
		if res[i].Distance < res[i-1].Distance {
			t.Fatalf("results not ascending by distance: %+v", res)
		}
	}
	if res[0].Similarity <= res[len(res)-1].Similarity {
		t.Fatalf("nearest should have the highest similarity: %+v", res)
	}
}

func TestSearchPreFilterRespectsWhere(t *testing.T) {
	pool := setupPool(t)
	seed(t, pool)
	res, err := Search(context.Background(), pool, SearchParams{
		Table:  "knn_docs",
		Metric: MetricCosine,
		Query:  []float32{0, 0, 1},
		K:      3,
		Model:  "m2",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range res {
		if r.ID != "z" {
			t.Fatalf("pre-filter leaked a non-m2 document: %q", r.ID)
		}
	}
}

// Your turn: complete TestL2IndexNotUsedForCosineQuery. Build a second index
// with vector_l2_ops, run EXPLAIN on a cosine (<=>) query against knn_docs, and
// assert the plan text contains "Seq Scan" -- proving an operator-class mismatch
// makes pgvector ignore the index and fall back to an exact scan.
func TestL2IndexNotUsedForCosineQuery(t *testing.T) {
	t.Skip("your turn: assert EXPLAIN shows a Seq Scan for the mismatched operator")
}
```

## Review

The search is correct when the nearest vector ranks first, results come back in
ascending distance order, and the reported score rises as distance falls.
`TestSearchRanksNearestFirst` pins all three at once: a query near the x-axis puts
document `x` first, the distances are non-decreasing, and the nearest hit has the
highest similarity. `TestSearchPreFilterRespectsWhere` proves the `WHERE`
actually constrains the candidate set rather than being ignored.

The mistakes to avoid are the silent ones. Always emit the `LIMIT`: without it
the ANN index cannot be used and the query degrades to an O(n) scan that is
correct but slow — the symptom is latency that grows with the table, not an
error. Always match the query operator to the index operator class; a
`vector_l2_ops` index with a `<=>` query is ignored, which is exactly what the
your-turn `EXPLAIN` test makes visible. Convert distance to similarity before
returning a score; `SimilarityFromDistance` exists so the best hit does not show
the lowest number. Set `hnsw.ef_search` on the same acquired connection as the
query, never on the pool, or it lands on a different session and does nothing.
And remember these results are approximate: raising `ef_search` improves recall
at a latency cost, and top-k can still miss a true neighbour, so do not use ANN
where exactness is a correctness requirement.

## Resources

- [pgvector — README](https://github.com/pgvector/pgvector) — distance operators, operator classes, the `ORDER BY ... LIMIT` query shape, and the `hnsw.ef_search` query-time knob.
- [Supabase — HNSW indexes for pgvector](https://supabase.com/docs/guides/ai/vector-indexes/hnsw-indexes) — HNSW build trade-offs and when to prefer HNSW over IVFFlat.
- [pgx v5 — pkg.go.dev](https://pkg.go.dev/github.com/jackc/pgx/v5) — `CollectRows`, `RowToStructByName`, and `pgxpool.Pool.Acquire`.
- [PostgreSQL EXPLAIN](https://www.postgresql.org/docs/current/using-explain.html) — reading a query plan to confirm whether an index was used.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-pgvector-store.md](02-pgvector-store.md) | Next: [../05-rag-pipeline/00-concepts.md](../05-rag-pipeline/00-concepts.md)
