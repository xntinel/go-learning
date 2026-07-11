# Exercise 19: Elasticsearch Query Builder with Callback-Based Filter Composition

**Nivel: Intermedio** — validacion rapida (un test corto).

A log search UI lets a user stack filters — service name, a date range,
excluded log levels, a tag match inside a nested array field — and the
backend has to turn that into an Elasticsearch `bool` query. Each filter is
an independent `Option` callback that mutates the query under construction,
so adding a new filter kind never touches the ones that already exist.

## What you'll build

```text
esquery/                    independent module: example.com/elasticsearch-query-builder-callback
  go.mod                     go 1.24
  esquery.go                   type BoolQuery, type Option, Term, Range, MustNot, Nested, func Build, (BoolQuery) ToMap
  cmd/
    demo/
      main.go                  runnable demo: term + range + must_not + nested tag filter, printed as JSON
  esquery_test.go              table test: each Option in isolation, nested composition, full build, empty build
```

Files: `esquery.go`, `cmd/demo/main.go`, `esquery_test.go`.
Implement: `type BoolQuery struct { Must, Filter, Should, MustNot []map[string]any }`, `type Option func(q *BoolQuery)`, the option constructors `Term`, `Range`, `MustNot`, `Nested`, `func Build(opts ...Option) BoolQuery`, and `func (q BoolQuery) ToMap() map[string]any`.
Test: `Term` alone, `Range` with both bounds and with one bound nil, `MustNot` alone, `Nested` embedding a sub-query with two `Term` filters under `filter`, a full build exercising every clause kind at once, and an empty build rendering an empty (but present) `bool` clause.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/elasticsearch-query-builder-callback/cmd/demo
cd ~/go-exercises/elasticsearch-query-builder-callback
go mod init example.com/elasticsearch-query-builder-callback
go mod edit -go=1.24
```

### Why each filter is its own `Option` callback

An Elasticsearch `bool` query has four independent clause lists —
`must`, `filter`, `should`, `must_not` — and any real search feature ends up
needing several of them combined, in whatever order the caller's UI
happened to add filters. Writing one `BuildQuery(term string, gte, lte any,
excludeLevel string, ...)` function forces every caller through the same
fixed parameter list, and adding a fifth filter kind means changing that
signature (and every call site) again. `Option func(q *BoolQuery)` sidesteps
that: `Term`, `Range`, `MustNot`, and `Nested` are each a tiny closure that
knows how to append itself to one clause list, and `Build` just runs
whichever ones the caller passed, in whatever order and combination the
caller wants — including zero, or the same kind twice. `Nested` shows the
pattern composes with itself: it calls `Build` recursively to construct the
sub-query it embeds, so a nested filter is not a special case in `Build` at
all.

Create `esquery.go`:

```go
// Package esquery composes Elasticsearch-style bool queries from small,
// independently testable Option callbacks.
package esquery

// BoolQuery mirrors the clauses of an Elasticsearch "bool" query.
type BoolQuery struct {
	Must    []map[string]any
	Filter  []map[string]any
	Should  []map[string]any
	MustNot []map[string]any
}

// Option mutates a BoolQuery under construction. Every query fragment —
// a term match, a range filter, a nested sub-query — is one Option, and
// Build composes any number of them into a single query.
type Option func(q *BoolQuery)

// Term adds an exact-match clause to must.
func Term(field string, value any) Option {
	return func(q *BoolQuery) {
		q.Must = append(q.Must, map[string]any{
			"term": map[string]any{field: value},
		})
	}
}

// Range adds a range filter. A nil bound is omitted from the clause.
func Range(field string, gte, lte any) Option {
	return func(q *BoolQuery) {
		bounds := map[string]any{}
		if gte != nil {
			bounds["gte"] = gte
		}
		if lte != nil {
			bounds["lte"] = lte
		}
		q.Filter = append(q.Filter, map[string]any{
			"range": map[string]any{field: bounds},
		})
	}
}

// MustNot adds an exclusion clause: documents matching field=value are
// excluded from the result set.
func MustNot(field string, value any) Option {
	return func(q *BoolQuery) {
		q.MustNot = append(q.MustNot, map[string]any{
			"term": map[string]any{field: value},
		})
	}
}

// Nested composes a sub-query from opts and embeds it as a nested filter
// under path — the pattern Elasticsearch uses to query fields that are
// arrays of objects.
func Nested(path string, opts ...Option) Option {
	return func(q *BoolQuery) {
		sub := Build(opts...)
		q.Filter = append(q.Filter, map[string]any{
			"nested": map[string]any{
				"path":  path,
				"query": sub.ToMap(),
			},
		})
	}
}

// Build applies every Option in order to a fresh BoolQuery.
func Build(opts ...Option) BoolQuery {
	var q BoolQuery
	for _, opt := range opts {
		opt(&q)
	}
	return q
}

// ToMap renders the query in the Elasticsearch Query DSL shape:
// {"bool": {"must": [...], "filter": [...], ...}}. Empty clauses are
// omitted so an unused clause doesn't show up as "must": [] in the JSON.
func (q BoolQuery) ToMap() map[string]any {
	clause := map[string]any{}
	if len(q.Must) > 0 {
		clause["must"] = q.Must
	}
	if len(q.Filter) > 0 {
		clause["filter"] = q.Filter
	}
	if len(q.Should) > 0 {
		clause["should"] = q.Should
	}
	if len(q.MustNot) > 0 {
		clause["must_not"] = q.MustNot
	}
	return map[string]any{"bool": clause}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"example.com/elasticsearch-query-builder-callback"
)

func main() {
	q := esquery.Build(
		esquery.Term("service", "checkout"),
		esquery.Range("timestamp", "2026-01-01", "2026-01-31"),
		esquery.MustNot("level", "debug"),
		esquery.Nested("tags",
			esquery.Term("tags.key", "env"),
			esquery.Term("tags.value", "prod"),
		),
	)

	out, err := json.MarshalIndent(q.ToMap(), "", "  ")
	if err != nil {
		fmt.Println("marshal error:", err)
		return
	}
	fmt.Println(string(out))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{
  "bool": {
    "filter": [
      {
        "range": {
          "timestamp": {
            "gte": "2026-01-01",
            "lte": "2026-01-31"
          }
        }
      },
      {
        "nested": {
          "path": "tags",
          "query": {
            "bool": {
              "must": [
                {
                  "term": {
                    "tags.key": "env"
                  }
                },
                {
                  "term": {
                    "tags.value": "prod"
                  }
                }
              ]
            }
          }
        }
      }
    ],
    "must": [
      {
        "term": {
          "service": "checkout"
        }
      }
    ],
    "must_not": [
      {
        "term": {
          "level": "debug"
        }
      }
    ]
  }
}
```

### Tests

Create `esquery_test.go`:

```go
package esquery

import "testing"

func TestTermAddsMustClause(t *testing.T) {
	t.Parallel()
	q := Build(Term("status", "active"))
	if len(q.Must) != 1 {
		t.Fatalf("len(Must) = %d, want 1", len(q.Must))
	}
	term, ok := q.Must[0]["term"].(map[string]any)
	if !ok {
		t.Fatalf("Must[0][\"term\"] not a map: %v", q.Must[0])
	}
	if term["status"] != "active" {
		t.Fatalf("term status = %v, want active", term["status"])
	}
}

func TestRangeAddsFilterClauseWithBothBounds(t *testing.T) {
	t.Parallel()
	q := Build(Range("age", 18, 65))
	if len(q.Filter) != 1 {
		t.Fatalf("len(Filter) = %d, want 1", len(q.Filter))
	}
	rangeClause := q.Filter[0]["range"].(map[string]any)
	bounds := rangeClause["age"].(map[string]any)
	if bounds["gte"] != 18 || bounds["lte"] != 65 {
		t.Fatalf("bounds = %v, want gte=18 lte=65", bounds)
	}
}

func TestRangeOmitsNilBound(t *testing.T) {
	t.Parallel()
	q := Build(Range("age", 18, nil))
	bounds := q.Filter[0]["range"].(map[string]any)["age"].(map[string]any)
	if _, hasLte := bounds["lte"]; hasLte {
		t.Fatalf("lte should be omitted, got %v", bounds)
	}
	if bounds["gte"] != 18 {
		t.Fatalf("gte = %v, want 18", bounds["gte"])
	}
}

func TestMustNotAddsExclusionClause(t *testing.T) {
	t.Parallel()
	q := Build(MustNot("level", "debug"))
	if len(q.MustNot) != 1 {
		t.Fatalf("len(MustNot) = %d, want 1", len(q.MustNot))
	}
}

func TestNestedEmbedsSubQueryUnderFilter(t *testing.T) {
	t.Parallel()
	q := Build(Nested("tags", Term("tags.key", "env"), Term("tags.value", "prod")))
	if len(q.Filter) != 1 {
		t.Fatalf("len(Filter) = %d, want 1", len(q.Filter))
	}
	nested := q.Filter[0]["nested"].(map[string]any)
	if nested["path"] != "tags" {
		t.Fatalf("path = %v, want tags", nested["path"])
	}
	subQuery := nested["query"].(map[string]any)
	subBool := subQuery["bool"].(map[string]any)
	subMust := subBool["must"].([]map[string]any)
	if len(subMust) != 2 {
		t.Fatalf("nested sub-query must clauses = %d, want 2", len(subMust))
	}
}

func TestBuildComposesAllClauseKinds(t *testing.T) {
	t.Parallel()
	q := Build(
		Term("service", "checkout"),
		Range("timestamp", "2026-01-01", "2026-01-31"),
		MustNot("level", "debug"),
		Nested("tags", Term("tags.key", "env")),
	)
	rendered := q.ToMap()
	boolClause := rendered["bool"].(map[string]any)
	for _, key := range []string{"must", "filter", "must_not"} {
		if _, ok := boolClause[key]; !ok {
			t.Errorf("rendered bool query missing %q clause", key)
		}
	}
	if _, hasShould := boolClause["should"]; hasShould {
		t.Errorf("unused should clause should be omitted from rendered query")
	}
}

func TestEmptyBuildRendersEmptyBoolQuery(t *testing.T) {
	t.Parallel()
	q := Build()
	rendered := q.ToMap()
	boolClause := rendered["bool"].(map[string]any)
	if len(boolClause) != 0 {
		t.Fatalf("empty query should render an empty bool clause, got %v", boolClause)
	}
}
```

## Review

`Build` is the entire composition mechanism: it holds no knowledge of what
`Term`, `Range`, `MustNot`, or `Nested` do, only that each is a function it
can call with a `*BoolQuery`. `Nested` proves this scales — it builds its
sub-query by calling `Build` on its own `opts`, exactly the way the
top-level caller does, so nested filters are not a special path through the
package. `ToMap`'s job is narrower than it looks: an Elasticsearch client
(or a human reading the JSON) should never see `"must": []` for a clause
nobody used, so `ToMap` includes a clause only when at least one `Option`
populated it — that is what `TestEmptyBuildRendersEmptyBoolQuery` and the
"should" check in `TestBuildComposesAllClauseKinds` both guard.

## Resources

- [Elasticsearch: Bool query](https://www.elastic.co/guide/en/elasticsearch/reference/current/query-dsl-bool-query.html)
- [Elasticsearch: Nested query](https://www.elastic.co/guide/en/elasticsearch/reference/current/query-dsl-nested-query.html)
- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-dns-resolver-strategy-callback.md](18-dns-resolver-strategy-callback.md) | Next: [20-feature-rule-evaluator-callback.md](20-feature-rule-evaluator-callback.md)
