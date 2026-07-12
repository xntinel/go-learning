# Exercise 11: Build a Deterministic Tenant Usage Rollup

**Nivel: Intermedio** — validacion rapida (un test corto).

A multi-tenant SaaS bills by usage: every event a tenant generates carries a
byte count, and the billing job has to turn a raw event stream into one row per
tenant before it can produce an invoice. This module builds that rollup with
the range-over-collections idiom that shows up constantly in backend code:
range a slice to accumulate into a map, then range the map to flatten it back
into a slice — and sort that slice, because nothing that crosses a process
boundary can rely on map iteration order.

## What you'll build

```text
rollup/                    independent module: example.com/tenant-usage-rollup
  go.mod                   go 1.24
  rollup.go                type Event, TenantUsage; Rollup(events) []TenantUsage
  rollup_test.go           table test: interleaved events + empty input
```

- Files: `rollup.go`, `rollup_test.go`.
- Implement: `Rollup(events []Event) []TenantUsage` accumulating via a
  `map[string]*TenantUsage`, then flattening and sorting by `TenantID`.
- Test: one table with events for three tenants interleaved, plus an empty-input
  case, asserted with `reflect.DeepEqual`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/05-range-over-collections/11-tenant-usage-rollup
cd go-solutions/03-control-flow/05-range-over-collections/11-tenant-usage-rollup
go mod edit -go=1.24
```

### Why accumulate through a pointer, and why sort at the end

The natural first draft reads: for each event, look up the tenant's usage, add
to it, write it back. With a plain `map[string]TenantUsage`, that "write it
back" step is mandatory — a map holding structs by value gives you a *copy* on
read, so `byTenant[id].TotalBytes += e.Bytes` will not even compile (the map
index expression is not addressable), and the naive fix ends up reading,
copying, mutating the copy, and re-storing it on every single event. Store
`*TenantUsage` instead: the first time a tenant is seen, allocate one entry and
put its pointer in the map; every later event for that tenant dereferences the
same pointer and mutates in place. No read-modify-write dance, and range over
the map for the second pass yields those same pointers directly.

The second pass — flattening the map into `[]TenantUsage` — is where the real
lesson lives. Go deliberately randomizes map iteration order across runs, so
two calls to `Rollup` with identical input can range the map in different
orders and produce differently-ordered slices. That is invisible in a demo
printout but fatal the moment the result leaves the process: an invoice API
response, a CSV export, or a golden-file test all depend on a stable order.
`slices.SortFunc` on `TenantID` after the flatten is not a nicety, it is the
line that makes the function's output actually deterministic — omit it and the
function is still "correct" by value but wrong by contract.

Create `rollup.go`:

```go
package rollup

import "slices"

// Event is one usage record for a tenant: how many bytes it moved.
type Event struct {
	TenantID string
	Bytes    int64
}

// TenantUsage is the accumulated usage for one tenant across all its events.
type TenantUsage struct {
	TenantID   string
	TotalBytes int64
	Count      int
}

// Rollup accumulates events per tenant and returns the result sorted by
// TenantID, so the output is deterministic no matter how the input was
// ordered or how the runtime happened to iterate the intermediate map.
func Rollup(events []Event) []TenantUsage {
	byTenant := make(map[string]*TenantUsage)

	for _, e := range events {
		u, ok := byTenant[e.TenantID]
		if !ok {
			u = &TenantUsage{TenantID: e.TenantID}
			byTenant[e.TenantID] = u
		}
		u.TotalBytes += e.Bytes
		u.Count++
	}

	out := make([]TenantUsage, 0, len(byTenant))
	for _, u := range byTenant {
		out = append(out, *u)
	}

	slices.SortFunc(out, func(a, b TenantUsage) int {
		if a.TenantID < b.TenantID {
			return -1
		}
		if a.TenantID > b.TenantID {
			return 1
		}
		return 0
	})

	return out
}
```

### Test

The table covers events for three tenants interleaved in the input — proving
the accumulation is per-tenant regardless of arrival order — plus an
empty-input case that must return a zero-length slice.

Create `rollup_test.go`:

```go
package rollup

import (
	"reflect"
	"testing"
)

func TestRollup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events []Event
		want   []TenantUsage
	}{
		{
			name:   "empty input",
			events: []Event{},
			want:   []TenantUsage{},
		},
		{
			name: "interleaved events across three tenants",
			events: []Event{
				{TenantID: "acme", Bytes: 100},
				{TenantID: "globex", Bytes: 50},
				{TenantID: "initech", Bytes: 10},
				{TenantID: "acme", Bytes: 200},
				{TenantID: "globex", Bytes: 25},
				{TenantID: "acme", Bytes: 1},
			},
			want: []TenantUsage{
				{TenantID: "acme", TotalBytes: 301, Count: 3},
				{TenantID: "globex", TotalBytes: 75, Count: 2},
				{TenantID: "initech", TotalBytes: 10, Count: 1},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Rollup(tc.events)
			if len(got) != len(tc.want) {
				t.Fatalf("Rollup() len = %d, want %d", len(got), len(tc.want))
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Rollup() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
```

Run it:

```bash
go test -count=1 ./...
```

## Review

The rollup is correct when every event lands in exactly one tenant's total, the
counts match how many events each tenant produced, and the output order never
depends on how the map happened to iterate this run. The pointer-valued map
gets the first property for free — no read-modify-write copy bug possible —
and `slices.SortFunc` on `TenantID` gets the second. The design point to carry
forward: any time a map is used as scratch space for accumulation, the moment
its contents need to leave as a slice, decide the order explicitly. "Whatever
range happened to produce" is not a real design decision.

## Resources

- [Go Specification: For statements (range over map)](https://go.dev/ref/spec#For_range) — map iteration order is unspecified.
- [package slices (SortFunc)](https://pkg.go.dev/slices#SortFunc)
- [Go Maps in Action](https://go.dev/blog/maps) — map internals and why order is randomized.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-batch-export-chunker.md](10-batch-export-chunker.md) | Next: [12-header-canonicalization.md](12-header-canonicalization.md)
