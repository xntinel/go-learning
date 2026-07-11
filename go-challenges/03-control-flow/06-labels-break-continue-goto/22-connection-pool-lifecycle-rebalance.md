# Exercise 22: Drain idle connections, health-check active, close on error quota

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A connection pool entering its shutdown phase must close every connection it
holds, but not blindly: idle connections close first since nothing is using
them, and active (in-flight) connections need a health check first, closing
only the ones that fail it. Real shutdowns run against a deadline, and a
pool talking to an unreachable host will see the same `Close` failure over
and over — continuing to retry each one individually just stalls the
shutdown. This module is fully self-contained: its own `go mod init`, all
code inline, its own demo and tests.

## What you'll build

```text
pooldrain/                  independent module: example.com/pooldrain
  go.mod                     go 1.24
  pooldrain.go                Conn, Result, Drain
  cmd/
    demo/
      main.go                runnable demo: mixed idle/active pool, quota hit mid-drain
  pooldrain_test.go            table test: empty pool, all succeed, idle failure hits quota, active-stage quota halt, healthy conn never counts, plus a concurrent-callers race check
```

- Files: `pooldrain.go`, `cmd/demo/main.go`, `pooldrain_test.go`.
- Implement: `Drain(idle, active []Conn, errQuota int) (results []Result, quotaHit bool)`, closing idle connections directly, health-checking active ones first, and halting the entire drain once `errQuota` close failures accumulate.
- Test: an empty pool, every connection closing successfully, an idle-stage failure alone hitting the quota, a quota hit partway through the active stage, and a healthy active connection that never risks a close failure. A concurrency test confirms `Drain` is safe to call from many goroutines against shared, read-only inputs.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/pooldrain/cmd/demo
cd ~/go-exercises/pooldrain
go mod init example.com/pooldrain
go mod edit -go=1.24
```

### Why the quota halt needs a labeled break, and why it spans stages

`Drain` models "idle first, then active" as two stages inside a single
outer loop over `stages := [][]Conn{idle, active}`, so both phases share one
error budget without duplicating the quota-check logic. Inside that, a
per-connection loop does the actual work, and for active connections it
runs a nested per-probe health-check loop before deciding whether a close
attempt is even needed. The quota check itself sits inside the
per-connection loop, one level below the stage loop that must stop. A bare
`break` there would only leave the per-connection loop for the CURRENT
stage — if the quota were hit while still draining idle connections, the
active stage would start anyway, immediately hitting the same unreachable
host and accumulating more (pointless) failures. The labeled `break drain`
exits both the stage loop and the connection loop together, so a quota hit
during idle draining also cancels the entire active stage, and every
connection not yet visited is left for a forced cleanup pass instead of
being touched by a doomed `Close` call.

Create `pooldrain.go`:

```go
package pooldrain

// Conn is one pooled connection under a shutdown drain.
type Conn struct {
	ID         string
	Health     []bool // health probe attempts; only consulted for active conns
	CloseFails bool   // true if Close() would fail (e.g. an unreachable host)
}

// Result records what happened to one connection during the drain.
type Result struct {
	ID     string
	Status string // "closed" or "close-failed"
}

// Drain closes idle connections first, then health-checks active
// connections and closes the unhealthy ones, stopping the ENTIRE scan the
// moment errQuota close failures have been observed: further failures in
// the same drain almost always share a root cause (an unreachable host), so
// continuing would only stall the shutdown waiting on doomed Close calls.
// The stage loop (idle, then active) and the per-connection loop inside it
// are two levels deep; the labeled break fires from inside the
// per-connection body — itself past a nested per-probe health-check loop
// for active connections — and exits both loops at once, leaving every
// connection not yet visited untouched for a forced cleanup pass later.
func Drain(idle, active []Conn, errQuota int) (results []Result, quotaHit bool) {
	stages := [][]Conn{idle, active}
	errCount := 0

drain:
	for stageIdx, stage := range stages {
		for _, c := range stage {
			if stageIdx == 1 { // active stage: health-check before deciding
				healthy := false
				for _, ok := range c.Health {
					if ok {
						healthy = true
						break
					}
				}
				if healthy {
					results = append(results, Result{ID: c.ID, Status: "closed"})
					continue
				}
			}
			// Idle connections, and unhealthy active ones, are closed directly.
			if c.CloseFails {
				errCount++
				results = append(results, Result{ID: c.ID, Status: "close-failed"})
				if errCount >= errQuota {
					quotaHit = true
					break drain
				}
				continue
			}
			results = append(results, Result{ID: c.ID, Status: "closed"})
		}
	}
	return results, quotaHit
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pooldrain"
)

func main() {
	idle := []pooldrain.Conn{
		{ID: "idle-1"},
		{ID: "idle-2", CloseFails: true},
		{ID: "idle-3"},
	}
	active := []pooldrain.Conn{
		{ID: "active-1", Health: []bool{true}},
		{ID: "active-2", Health: []bool{false}, CloseFails: true},
		{ID: "active-3", Health: []bool{false}},
	}

	results, quotaHit := pooldrain.Drain(idle, active, 2)
	for _, r := range results {
		fmt.Printf("%s: %s\n", r.ID, r.Status)
	}
	fmt.Println("quota hit:", quotaHit)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
idle-1: closed
idle-2: close-failed
idle-3: closed
active-1: closed
active-2: close-failed
quota hit: true
```

`idle-2`'s close failure is the first error, but the quota of two is not
yet reached, so `idle-3` and the active stage still proceed. `active-2` is
unhealthy and its close also fails — the second error — which trips the
quota. `active-3` is never touched, even though it is unhealthy and would
have needed closing too.

### Tests

`TestDrain` covers an empty pool, every connection closing without issue, an
idle-stage failure alone reaching the quota (before the active stage even
starts), a quota hit partway through the active stage, and a healthy active
connection that never risks contributing to the error count.
`TestDrainConcurrentCallers` runs `Drain` from fifty goroutines against
shared input slices to confirm the read-only scan introduces no data race.

Create `pooldrain_test.go`:

```go
package pooldrain

import (
	"slices"
	"sync"
	"testing"
)

func ids(results []Result) []string {
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = r.ID
	}
	return out
}

func TestDrain(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		idle      []Conn
		active    []Conn
		errQuota  int
		wantIDs   []string
		wantQuota bool
	}{
		"empty pool drains cleanly": {
			idle:      nil,
			active:    nil,
			errQuota:  1,
			wantIDs:   nil,
			wantQuota: false,
		},
		"all idle and active close successfully": {
			idle: []Conn{{ID: "i1"}, {ID: "i2"}},
			active: []Conn{
				{ID: "a1", Health: []bool{true}},
				{ID: "a2", Health: []bool{false}}, // unhealthy but closes fine
			},
			errQuota:  10,
			wantIDs:   []string{"i1", "i2", "a1", "a2"},
			wantQuota: false,
		},
		"an idle close failure counts toward the quota": {
			idle: []Conn{
				{ID: "i1", CloseFails: true},
				{ID: "i2", CloseFails: true},
				{ID: "i3"},
			},
			active:    []Conn{{ID: "a1", Health: []bool{true}}},
			errQuota:  2,
			wantIDs:   []string{"i1", "i2"},
			wantQuota: true,
		},
		"quota reached mid-active-stage halts the rest of the pool": {
			idle: []Conn{{ID: "i1"}},
			active: []Conn{
				{ID: "a1", Health: []bool{false}, CloseFails: true},
				{ID: "a2", Health: []bool{false}, CloseFails: true},
				{ID: "a3", Health: []bool{true}},
			},
			errQuota:  2,
			wantIDs:   []string{"i1", "a1", "a2"},
			wantQuota: true,
		},
		"a healthy active connection never counts against the quota": {
			idle: nil,
			active: []Conn{
				{ID: "a1", Health: []bool{false, false, true}},
			},
			errQuota:  1,
			wantIDs:   []string{"a1"},
			wantQuota: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			results, quotaHit := Drain(tc.idle, tc.active, tc.errQuota)
			if !slices.Equal(ids(results), tc.wantIDs) {
				t.Fatalf("ids(results) = %v, want %v", ids(results), tc.wantIDs)
			}
			if quotaHit != tc.wantQuota {
				t.Fatalf("quotaHit = %v, want %v", quotaHit, tc.wantQuota)
			}
		})
	}
}

// TestDrainConcurrentCallers checks that Drain, which only reads its input
// slices and returns fresh output, is safe to call concurrently -- run with
// -race to confirm no data race is introduced by sharing the same inputs.
func TestDrainConcurrentCallers(t *testing.T) {
	idle := []Conn{{ID: "i1"}}
	active := []Conn{{ID: "a1", Health: []bool{true}}}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results, quotaHit := Drain(idle, active, 10)
			if quotaHit || len(results) != 2 {
				t.Errorf("Drain = (%v, %v), want (2 results, false)", results, quotaHit)
			}
		}()
	}
	wg.Wait()
}
```

Verify:

```bash
go test -count=1 -race ./...
```

## Review

The drain is correct when a quota hit stops *everything* remaining,
regardless of which stage it happened in — the "quota reached
mid-active-stage" test is the one to study, since `a3` is both unhealthy and
perfectly closeable, and still never appears in the results. The bug this
exercise guards against is a `break` that only reaches the per-connection
loop (or worse, a labeled break scoped to just one stage): the drain would
finish closing the current stage's remaining connections, or start the next
stage fresh, either way continuing to burn time on a host that has already
demonstrated it will keep failing. The "healthy active connection" test
confirms the other half of the contract: health-checking happens strictly
before any close attempt, so a connection that is still good is never even
at risk of contributing to the error count.

## Resources

- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — a labeled `break` can leave any number of enclosing loops at once.
- [database/sql, Conn and pool lifecycle](https://pkg.go.dev/database/sql#Conn) — the real-world shape of idle versus in-use connections this exercise models.
- [The Go Memory Model](https://go.dev/ref/mem) — why a function that only reads shared input needs no synchronization to be race-free.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-quorum-leader-election.md](21-quorum-leader-election.md) | Next: [23-distributed-changelog-sync.md](23-distributed-changelog-sync.md)
