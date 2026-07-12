# Exercise 16: Connection Pool Eviction: Age-Based and LRU Reclamation

**Nivel: Intermedio** — validacion rapida (un test corto).

A connection pool that never closes idle connections leaks file
descriptors until the process hits its limit and every new connection
attempt fails — including ones for requests that have nothing to do with
the leak. Reclaiming a connection is a small decision made once per
connection: compare its idle time to a max age, and if the pool is still
over capacity after aging out the old ones, evict the least recently used
survivors too. This module is fully self-contained: its own `go mod init`,
all code inline, its own test file.

## What you'll build

```text
pool/                       independent module: example.com/connection-pool-eviction-lru-age
  go.mod                    go 1.24
  pool.go                   Conn, Reclaim(conns, now, maxAge, maxSize)
  pool_test.go              table: none stale, all stale, mixed, over-capacity LRU trim
```

- Files: `pool.go`, `pool_test.go`.
- Implement: `Reclaim(conns []Conn, now time.Time, maxAge time.Duration, maxSize int) (keep, evicted []Conn)`, where the init-statement `if age := now.Sub(c.LastUsed); age > maxAge` decides age-based eviction first, then an LRU trim by `LastUsed` order handles anything still over `maxSize`.
- Test: a table over no stale connections, all stale, a mix, and a pool within age limits but over `maxSize` that must trim by least-recently-used.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why age-based and capacity-based eviction are two separate passes

Age-based eviction answers "has this specific connection been idle too
long," which is a per-connection guard independent of how many other
connections exist. Capacity-based eviction answers a different question —
"is the pool as a whole too big" — which only has an answer once every
connection's fate from the first pass is known. Running them as one
combined comparison would either evict connections that are still well
within their age budget just because the pool is momentarily large, or
let a genuinely stale connection survive because the pool happens to be
under capacity. Keeping them as two ordered passes over the survivors
means each guard answers exactly one question.

Create `pool.go`:

```go
// Package pool decides which idle connections a connection pool should
// close: first by age, then, if still over capacity, by least-recently-used.
package pool

import (
	"sort"
	"time"
)

// Conn is one pooled connection's identity and last-use timestamp. The zero
// value of ID must not be used as a real connection identifier.
type Conn struct {
	ID       string
	LastUsed time.Time
}

// Reclaim decides which connections in conns to keep and which to evict.
// First, any connection idle longer than maxAge is evicted outright. Then, if
// the survivors still exceed maxSize, the least-recently-used survivors are
// evicted until the pool is back at maxSize. maxSize <= 0 means unlimited.
func Reclaim(conns []Conn, now time.Time, maxAge time.Duration, maxSize int) (keep, evicted []Conn) {
	for _, c := range conns {
		if age := now.Sub(c.LastUsed); age > maxAge {
			evicted = append(evicted, c)
			continue
		}
		keep = append(keep, c)
	}

	if maxSize <= 0 || len(keep) <= maxSize {
		return keep, evicted
	}

	// Still over capacity: trim the least-recently-used survivors. Sorting a
	// copy (rather than keep itself) means the returned keep slice preserves
	// its original relative order instead of being reordered by LastUsed.
	byAge := make([]Conn, len(keep))
	copy(byAge, keep)
	sort.Slice(byAge, func(i, j int) bool {
		return byAge[i].LastUsed.Before(byAge[j].LastUsed)
	})

	overBy := len(keep) - maxSize
	toEvict := make(map[string]bool, overBy)
	for _, c := range byAge[:overBy] {
		toEvict[c.ID] = true
	}

	var survivors []Conn
	for _, c := range keep {
		if toEvict[c.ID] {
			evicted = append(evicted, c)
			continue
		}
		survivors = append(survivors, c)
	}
	return survivors, evicted
}
```

### Tests

The table covers the age guard in isolation (none stale, all stale, a mix)
and then a pool that is entirely within its age budget but must still be
trimmed by `LastUsed` order because it exceeds `maxSize`, proving the two
passes compose correctly.

Create `pool_test.go`:

```go
package pool

import (
	"testing"
	"time"
)

func TestReclaimByAge(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	maxAge := 10 * time.Minute

	tests := []struct {
		name        string
		conns       []Conn
		wantKeep    []string
		wantEvicted []string
	}{
		{
			name: "none stale, all kept",
			conns: []Conn{
				{ID: "a", LastUsed: now.Add(-1 * time.Minute)},
				{ID: "b", LastUsed: now.Add(-5 * time.Minute)},
			},
			wantKeep:    []string{"a", "b"},
			wantEvicted: nil,
		},
		{
			name: "all stale, all evicted",
			conns: []Conn{
				{ID: "a", LastUsed: now.Add(-11 * time.Minute)},
				{ID: "b", LastUsed: now.Add(-30 * time.Minute)},
			},
			wantKeep:    nil,
			wantEvicted: []string{"a", "b"},
		},
		{
			name: "mixed ages split correctly",
			conns: []Conn{
				{ID: "fresh", LastUsed: now.Add(-2 * time.Minute)},
				{ID: "stale", LastUsed: now.Add(-15 * time.Minute)},
			},
			wantKeep:    []string{"fresh"},
			wantEvicted: []string{"stale"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			keep, evicted := Reclaim(tc.conns, now, maxAge, 0)
			if got := ids(keep); !equal(got, tc.wantKeep) {
				t.Errorf("keep = %v, want %v", got, tc.wantKeep)
			}
			if got := ids(evicted); !equal(got, tc.wantEvicted) {
				t.Errorf("evicted = %v, want %v", got, tc.wantEvicted)
			}
		})
	}
}

func TestReclaimTrimsOverCapacityByLRU(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	conns := []Conn{
		{ID: "newest", LastUsed: now.Add(-1 * time.Minute)},
		{ID: "middle", LastUsed: now.Add(-2 * time.Minute)},
		{ID: "oldest", LastUsed: now.Add(-3 * time.Minute)},
	}

	// All are well within maxAge, but maxSize=2 forces one LRU eviction.
	keep, evicted := Reclaim(conns, now, time.Hour, 2)

	if got := ids(keep); !equal(got, []string{"newest", "middle"}) {
		t.Errorf("keep = %v, want [newest middle]", got)
	}
	if got := ids(evicted); !equal(got, []string{"oldest"}) {
		t.Errorf("evicted = %v, want [oldest]", got)
	}
}

func ids(conns []Conn) []string {
	out := make([]string, len(conns))
	for i, c := range conns {
		out[i] = c.ID
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

Verify: `go test -count=1 ./...`

## Review

The age guard runs to completion for every connection before the capacity
trim ever inspects `len(keep)`, which is what keeps the two concerns from
interfering with each other: a connection is never kept alive by capacity
being fine, and never evicted for capacity reasons before its age has
been fairly evaluated. Carry this forward: whenever a reclamation policy
combines two independent criteria, run each as its own complete pass over
the surviving set rather than interleaving the comparisons.

## Resources

- [Go Specification: Comparison operators](https://go.dev/ref/spec#Comparison_operators) — `time.Time` is not comparable with `==`; `Before`/`After`/`Sub` are the correct API.
- [database/sql: DB.SetConnMaxIdleTime](https://pkg.go.dev/database/sql#DB.SetConnMaxIdleTime) — the standard library's own age-based pool eviction.
- [sort.Slice documentation](https://pkg.go.dev/sort#Slice) — the in-place sort used for the LRU trim.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-circuit-breaker-fallback-state-machine.md](15-circuit-breaker-fallback-state-machine.md) | Next: [17-batch-window-flush-decision.md](17-batch-window-flush-decision.md)
