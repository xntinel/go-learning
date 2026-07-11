# Exercise 2: Vector Retrieval — Cosine Ranking, Top-K, and MMR Re-ranking

Retrieval is the stage that dominates answer quality, and it is pure math: cosine
similarity, a top-k sort with deterministic tie-breaking, and MMR re-ranking to
keep near-duplicates from crowding the context. This exercise builds all of that
as an in-memory `Store` you can unit-test to the decimal, and mirrors the
production path with a pgvector-backed `Store` isolated behind a build tag.

This module is fully self-contained. The default build and test path uses only the
standard library; the Postgres-backed store lives in a `//go:build online` file
that is compiled only with `-tags online`.

## What you'll build

```text
retriever/                   independent module: example.com/retriever
  go.mod                     go 1.26
  retriever.go               Cosine, Document, Match, Store, MemoryStore, MMR
  store_pgvector.go          //go:build online — pgx + pgvector Store
  cmd/
    demo/
      main.go                runnable demo: cosine, top-k, MMR
  retriever_test.go          hand-computed cosine, ordering, threshold, MMR
```

- Files: `retriever.go`, `store_pgvector.go`, `cmd/demo/main.go`, `retriever_test.go`.
- Implement: `Cosine`, a `Store` interface (`Query(ctx, queryVec, k) ([]Match, error)`), an in-memory `MemoryStore` with a minimum-score threshold, and `MMR` re-ranking with a lambda knob. A pgvector `Store` behind `//go:build online`.
- Test: hand-computed cosine values; top-k descending with tie-break on ID; threshold filtering; MMR at lambda=1 equals pure relevance and at lambda<1 demotes a near-duplicate; dimension mismatch returns `ErrDimensionMismatch`.
- Verify: `go test -count=1 -race ./...` (offline); `go build -tags online ./...` to compile the pgvector store.

Set up the module:

```bash
mkdir -p ~/rag-exercises/retriever/cmd/demo
cd ~/rag-exercises/retriever
go mod init example.com/retriever
go mod edit -go=1.26
```

### Cosine similarity, and the zero-vector trap

Cosine similarity is the dot product divided by the product of the magnitudes. It
ranges from -1 (opposite) through 0 (orthogonal) to 1 (identical), and larger
means more similar. The accumulation runs in `float64` even though the vectors are
`float32`, because summing thousands of `float32` products loses precision;
converting back to `float32` at the end keeps the stored type compact while the
math stays accurate.

The trap is the zero vector. If either vector has magnitude zero, the denominator
is zero and the naive formula returns `NaN`, which then poisons every sort
comparison it touches (`NaN` is unordered). The fix is explicit: when either norm
is zero, return `0` — an undefined similarity treated as "not similar." This is
the difference between a retriever that degrades gracefully on a bad embedding and
one that silently corrupts its ranking.

Dimension mismatch is a real production failure — it happens the moment you change
embedding models without re-embedding the corpus — so `Cosine` returns a wrapped
`ErrDimensionMismatch` rather than panicking on the index out of range.

### Top-k with deterministic tie-breaking

`MemoryStore.Query` scores every document against the query, drops anything below
the store's `MinScore` threshold, sorts descending by score, and returns the top
k. The sort must be *deterministic*: two chunks with identical scores have to come
back in the same order every run, or your prompt (and your cache key, and your
tests) become flaky. The comparator therefore sorts by score descending and
breaks ties by ID ascending, expressed compactly with `cmp.Or`:

```go
cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.ID, b.ID))
```

`cmp.Or` returns the first non-zero comparison, so equal scores fall through to
the ID comparison. The threshold matters as much as the ordering: a below-
threshold "nearest" neighbour is worse than no result, because it invites the
grounding stage to answer from an irrelevant passage.

### MMR: trading relevance for diversity

Pure top-k has a failure mode — the k nearest vectors are often paraphrases of one
another. Maximal marginal relevance fixes it greedily. Starting from the scored
candidates, MMR repeatedly selects the one that maximizes

```
lambda * relevance - (1 - lambda) * maxSimilarityToAlreadySelected
```

where `relevance` is the candidate's cosine to the query and
`maxSimilarityToAlreadySelected` is the largest cosine to anything already picked
(zero when nothing is picked yet, so the first selection is pure relevance). At
`lambda = 1` the diversity term vanishes and MMR reduces to relevance order. Below
1, a candidate that closely resembles an already-selected chunk is penalized, so
the packed context covers more of the query. The reported `Score` on each `Match`
stays the raw relevance, so downstream stages still see a meaningful similarity.

### The pgvector store behind a build tag

The in-memory store is what CI runs; the pgvector store is what production runs,
and it embodies the single most important retrieval invariant. pgvector's `<=>`
operator returns cosine *distance* — smaller is closer — so the nearest-neighbour
query orders *ascending* and converts back to a similarity with
`1 - (embedding <=> $1)`. Ordering descending would silently return the worst
matches. Because this file imports `pgx` and `pgvector-go`, it sits behind
`//go:build online`; the default gate never compiles it, so CI stays fast and
free, and you opt in with `go build -tags online ./...` when a database is
present.

Create `retriever.go`:

```go
package retriever

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
)

// ErrDimensionMismatch is returned when two vectors have different lengths,
// which happens when embedding models or dimensions are mixed without
// re-embedding the corpus. Wrapped with %w; assert via errors.Is.
var ErrDimensionMismatch = errors.New("vector dimension mismatch")

// Document is a stored chunk with its embedding.
type Document struct {
	ID     string
	Vector []float32
	Text   string
}

// Match is a retrieved chunk with its similarity score.
type Match struct {
	ID    string
	Score float32
	Text  string
}

// Store retrieves the k nearest chunks to a query vector.
type Store interface {
	Query(ctx context.Context, queryVec []float32, k int) ([]Match, error)
}

// Cosine returns the cosine similarity of a and b in [-1, 1]. A zero-magnitude
// vector yields 0 (never NaN). Length mismatch returns ErrDimensionMismatch.
func Cosine(a, b []float32) (float32, error) {
	if len(a) != len(b) {
		return 0, fmt.Errorf("retriever: cosine %d vs %d: %w", len(a), len(b), ErrDimensionMismatch)
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0, nil
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb))), nil
}

// MemoryStore is an in-memory Store over []float32 vectors.
type MemoryStore struct {
	docs     []Document
	MinScore float32
}

// NewMemoryStore builds a store that drops matches scoring below minScore.
func NewMemoryStore(minScore float32, docs ...Document) *MemoryStore {
	return &MemoryStore{docs: docs, MinScore: minScore}
}

// Query returns up to k matches with score >= MinScore, ordered by score
// descending with ties broken by ID ascending.
func (s *MemoryStore) Query(ctx context.Context, queryVec []float32, k int) ([]Match, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	matches := make([]Match, 0, len(s.docs))
	for _, d := range s.docs {
		score, err := Cosine(queryVec, d.Vector)
		if err != nil {
			return nil, err
		}
		if score < s.MinScore {
			continue
		}
		matches = append(matches, Match{ID: d.ID, Score: score, Text: d.Text})
	}
	slices.SortFunc(matches, func(a, b Match) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.ID, b.ID))
	})
	if k >= 0 && k < len(matches) {
		matches = matches[:k]
	}
	return matches, nil
}

var _ Store = (*MemoryStore)(nil)

// MMR re-ranks candidates by maximal marginal relevance, returning up to k
// matches. lambda in [0,1] trades relevance (1.0) against diversity (0.0).
func MMR(queryVec []float32, candidates []Document, lambda float32, k int) ([]Match, error) {
	rel := make([]float32, len(candidates))
	for i, d := range candidates {
		s, err := Cosine(queryVec, d.Vector)
		if err != nil {
			return nil, err
		}
		rel[i] = s
	}
	selected := make([]bool, len(candidates))
	var out []Match
	for len(out) < k && len(out) < len(candidates) {
		best := -1
		var bestScore float32
		for i := range candidates {
			if selected[i] {
				continue
			}
			var maxSim float32
			for j := range candidates {
				if !selected[j] {
					continue
				}
				s, err := Cosine(candidates[i].Vector, candidates[j].Vector)
				if err != nil {
					return nil, err
				}
				if s > maxSim {
					maxSim = s
				}
			}
			score := lambda*rel[i] - (1-lambda)*maxSim
			if best == -1 || score > bestScore ||
				(score == bestScore && candidates[i].ID < candidates[best].ID) {
				best, bestScore = i, score
			}
		}
		selected[best] = true
		out = append(out, Match{
			ID:    candidates[best].ID,
			Score: rel[best],
			Text:  candidates[best].Text,
		})
	}
	return out, nil
}
```

Create `store_pgvector.go`. Note the `//go:build online` constraint on the first
line — this file is excluded from the default build:

```go
//go:build online

package retriever

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
)

// PgVectorStore is a Store backed by Postgres + pgvector. It lives behind the
// online build tag so the default gate stays offline and dependency-free.
type PgVectorStore struct {
	pool     *pgxpool.Pool
	minScore float32
}

func NewPgVectorStore(pool *pgxpool.Pool, minScore float32) *PgVectorStore {
	return &PgVectorStore{pool: pool, minScore: minScore}
}

var _ Store = (*PgVectorStore)(nil)

// Query returns the k nearest chunks. <=> is cosine DISTANCE (smaller is
// closer), so ORDER BY ascending and convert to a similarity with 1 - distance.
// Ordering descending would return the least relevant rows.
func (s *PgVectorStore) Query(ctx context.Context, queryVec []float32, k int) ([]Match, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, text, 1 - (embedding <=> $1) AS score
		FROM chunks
		ORDER BY embedding <=> $1
		LIMIT $2`,
		pgvector.NewVector(queryVec), k)
	if err != nil {
		return nil, fmt.Errorf("retriever: pgvector query: %w", err)
	}
	defer rows.Close()

	var matches []Match
	for rows.Next() {
		var (
			m     Match
			score float64
		)
		if err := rows.Scan(&m.ID, &m.Text, &score); err != nil {
			return nil, fmt.Errorf("retriever: scan: %w", err)
		}
		m.Score = float32(score)
		if m.Score < s.minScore {
			continue
		}
		matches = append(matches, m)
	}
	return matches, rows.Err()
}
```

### The runnable demo

The demo scores a query against four documents, shows the raw top-k, then shows
how MMR at a low lambda aggressively favours diversity: the near-duplicate `b` is
pushed out of the top-3 entirely, so far that even the unrelated `d` outranks it.
That is the cost of a low lambda — raise it toward 1 to weight relevance more.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/retriever"
)

func main() {
	docs := []retriever.Document{
		{ID: "a", Vector: []float32{1, 0, 0}, Text: "cats are mammals"},
		{ID: "b", Vector: []float32{0.98, 0.10, 0}, Text: "cats are feline mammals"},
		{ID: "c", Vector: []float32{0.30, 0.95, 0}, Text: "dogs are loyal companions"},
		{ID: "d", Vector: []float32{0, 0, 1}, Text: "the stock market fell today"},
	}
	query := []float32{1, 0, 0}

	store := retriever.NewMemoryStore(0.15, docs...)
	top, _ := store.Query(context.Background(), query, 3)
	fmt.Println("top-k by cosine:")
	for _, m := range top {
		fmt.Printf("  %s %.3f | %s\n", m.ID, m.Score, m.Text)
	}

	reranked, _ := retriever.MMR(query, docs, 0.3, 3)
	fmt.Println("MMR (lambda=0.3):")
	for _, m := range reranked {
		fmt.Printf("  %s %.3f | %s\n", m.ID, m.Score, m.Text)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
top-k by cosine:
  a 1.000 | cats are mammals
  b 0.995 | cats are feline mammals
  c 0.301 | dogs are loyal companions
MMR (lambda=0.3):
  a 1.000 | cats are mammals
  d 0.000 | the stock market fell today
  c 0.301 | dogs are loyal companions
```

### Tests

The tests pin the math to hand-computed values and the ranking behaviour to fixed
vectors. `TestCosine` checks the three canonical cases (orthogonal 0, identical 1,
opposite -1), the zero-vector case (0, not NaN), and the dimension-mismatch error.
`TestQueryOrdering` checks descending order, the threshold, and deterministic
tie-breaking on ID. `TestMMR` is the diversity proof: with `lambda=1` the second
result is the near-duplicate `b`; with `lambda=0.2` the near-duplicate is demoted
and the diverse `d` takes its place. `TestZeroVector` is the learner extension,
confirming a zero query never produces NaN.

Create `retriever_test.go`:

```go
package retriever

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
)

func approxEqual(a, b float32) bool {
	return math.Abs(float64(a)-float64(b)) < 1e-6
}

func TestCosine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b []float32
		want float32
	}{
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"identical", []float32{1, 1}, []float32{1, 1}, 1},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1},
		{"zero vector", []float32{0, 0}, []float32{1, 1}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Cosine(tc.a, tc.b)
			if err != nil {
				t.Fatalf("Cosine: %v", err)
			}
			if !approxEqual(got, tc.want) {
				t.Fatalf("Cosine = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCosineDimensionMismatch(t *testing.T) {
	t.Parallel()
	_, err := Cosine([]float32{1, 0, 0}, []float32{1, 0})
	if !errors.Is(err, ErrDimensionMismatch) {
		t.Fatalf("err = %v, want ErrDimensionMismatch", err)
	}
}

func TestQueryOrdering(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore(0.5,
		Document{ID: "low", Vector: []float32{0, 1}, Text: "orthogonal"}, // score 0, filtered
		Document{ID: "hi2", Vector: []float32{1, 0}, Text: "exact tie 2"},
		Document{ID: "hi1", Vector: []float32{1, 0}, Text: "exact tie 1"},
		Document{ID: "mid", Vector: []float32{1, 1}, Text: "diagonal"},
	)
	matches, err := store.Query(context.Background(), []float32{1, 0}, 10)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(matches) != 3 {
		t.Fatalf("got %d matches, want 3 (low filtered by threshold)", len(matches))
	}
	// Descending score, ties broken by ID ascending: hi1, hi2 (both 1.0), then mid.
	wantOrder := []string{"hi1", "hi2", "mid"}
	for i, id := range wantOrder {
		if matches[i].ID != id {
			t.Fatalf("position %d = %q, want %q", i, matches[i].ID, id)
		}
	}
	for i := 0; i+1 < len(matches); i++ {
		if matches[i].Score < matches[i+1].Score {
			t.Fatalf("not descending at %d: %v < %v", i, matches[i].Score, matches[i+1].Score)
		}
	}
}

func TestMMR(t *testing.T) {
	t.Parallel()
	query := []float32{1, 0}
	docs := []Document{
		{ID: "a", Vector: []float32{1, 0}, Text: "top"},
		{ID: "b", Vector: []float32{1, 0}, Text: "near-duplicate of top"},
		{ID: "d", Vector: []float32{0.6, 0.8}, Text: "diverse but relevant"},
	}

	pure, err := MMR(query, docs, 1.0, 3)
	if err != nil {
		t.Fatalf("MMR: %v", err)
	}
	if pure[1].ID != "b" {
		t.Fatalf("lambda=1 second result = %q, want b (pure relevance)", pure[1].ID)
	}

	diverse, err := MMR(query, docs, 0.2, 3)
	if err != nil {
		t.Fatalf("MMR: %v", err)
	}
	if diverse[1].ID != "d" {
		t.Fatalf("lambda=0.2 second result = %q, want d (near-duplicate demoted)", diverse[1].ID)
	}
}

// TestZeroVector is the learner extension: a zero query must not yield NaN, which
// would poison the ranking.
func TestZeroVector(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore(-1,
		Document{ID: "a", Vector: []float32{1, 0}, Text: "x"},
	)
	matches, err := store.Query(context.Background(), []float32{0, 0}, 1)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}
	if math.IsNaN(float64(matches[0].Score)) {
		t.Fatal("score is NaN")
	}
}

func ExampleMemoryStore_Query() {
	store := NewMemoryStore(0,
		Document{ID: "a", Vector: []float32{1, 0}, Text: "exact"},
		Document{ID: "b", Vector: []float32{0, 1}, Text: "orthogonal"},
		Document{ID: "c", Vector: []float32{1, 1}, Text: "diagonal"},
	)
	matches, _ := store.Query(context.Background(), []float32{1, 0}, 2)
	for _, m := range matches {
		fmt.Printf("%s %.2f\n", m.ID, m.Score)
	}
	// Output:
	// a 1.00
	// c 0.71
}
```

## Review

The retriever is correct when the cosine math matches hand computation and the
ranking is deterministic. `Cosine` must return 0 (not NaN) for a zero vector —
`TestZeroVector` guards it — and a wrapped `ErrDimensionMismatch` for mixed
dimensions, which is the concrete symptom of switching embedding models without
re-embedding. `Query` must sort descending with a stable tie-break; if a test
flakes on ordering, the comparator is missing its secondary ID compare. `MMR` must
collapse to pure relevance at `lambda=1` and demote near-duplicates below it —
`TestMMR` pins both ends.

The one subtlety to internalize is the pgvector store: `<=>` is cosine *distance*,
so the query orders ascending and converts with `1 - distance`. Writing `DESC`
compiles, runs, and returns the worst matches — a bug the offline gate cannot
catch, which is exactly why the concept is worth stating twice. Run `go test
-race` for the pure path; run `go build -tags online ./...` (with the modules
fetched) to confirm the pgvector store compiles.

## Resources

- [`cmp`](https://pkg.go.dev/cmp) — `cmp.Compare` and `cmp.Or` for deterministic multi-key ordering.
- [pgvector](https://github.com/pgvector/pgvector) — distance operators (`<=>`, `<#>`, `<+>`) and nearest-neighbour `ORDER BY ... LIMIT`.
- [pgvector-go](https://github.com/pgvector/pgvector-go) — `pgvector.NewVector([]float32)` and pgx integration.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-chunker.md](01-chunker.md) | Next: [03-grounding.md](03-grounding.md)
