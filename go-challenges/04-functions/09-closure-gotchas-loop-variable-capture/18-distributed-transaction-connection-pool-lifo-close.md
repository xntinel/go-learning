# Exercise 18: Distributed Transaction: Deferred Connection Closes in LIFO Order

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde).

A distributed transaction commits work across several database shards, one
connection per shard. The obvious `defer conn.Close()` inside the loop body
holds every shard's connection — and the row locks it carries — open until
the whole transaction function returns, instead of releasing each shard's
lock right after its own commit. Worse, when those closes finally run, they
fire in the REVERSE of the order the shards were opened, not the order the
coordinator expects participants to release in.

## What you'll build

```text
dtxpool/                     independent module: example.com/dtxpool
  go.mod                     go 1.24
  dtxpool.go                   Tracker, Conn, RunDistributedTxLeaky, RunDistributedTxScoped
  cmd/
    demo/
      main.go                runnable demo: run both, print peak-open and close order
  dtxpool_test.go              table test: peak-open==1 vs ==N, close order, join errors
```

- Files: `dtxpool.go`, `cmd/demo/main.go`, `dtxpool_test.go`.
- Implement: `RunDistributedTxLeaky` (commit then defer close inside the loop, every connection outlives the loop) and `RunDistributedTxScoped` (a per-shard helper that scopes the defer), both aggregating commit errors with `errors.Join`.
- Test: a `Tracker` records open/close events; assert scoped keeps peak concurrent-open at 1 and closes each shard before the next opens, while leaky peaks at N and closes in reverse order; assert commit errors surface through the joined error.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the naive version holds every shard's locks at once

`defer` schedules a call to run when the *enclosing function* returns, not
when the loop iteration ends. In `RunDistributedTxLeaky`, every
`conn.Close()` is queued and none runs until `RunDistributedTxLeaky` itself
returns. During the loop, the `orders` shard's connection — and whatever row
locks it holds — is still open while the `shipping` shard is being committed,
and so on: the peak number of simultaneously open shard connections equals
the shard count. In a real two-phase-commit this is a deadlock risk multiplier
— every extra shard held open at once is another lock another concurrent
transaction can collide with. When the function finally returns, the
deferred closes run LIFO: the last-opened shard closes first, backwards from
the order the coordinator expects participants to release.

`RunDistributedTxScoped` fixes it by moving the per-shard work into a helper
function whose own `return` scopes the `defer`. Each call to the helper opens
a shard connection, defers its close, commits, and returns — at which point
the deferred close runs, before the next shard's connection even opens. Peak
concurrent-open is 1, and shards close in the same order they were opened.

Create `dtxpool.go`:

```go
package dtxpool

import (
	"errors"
	"sync"
)

// Tracker stands in for a pool of per-shard database connections used to run
// a distributed transaction across several participants. It records how many
// connections are simultaneously open (holding a shard's row locks) and the
// peak, plus the order connections actually close in.
type Tracker struct {
	mu    sync.Mutex
	live  int
	peak  int
	order []string
}

// NewTracker returns an empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{}
}

// Conn is one participant's connection, held open for the duration of its
// piece of the distributed transaction.
type Conn struct {
	shard  string
	t      *Tracker
	failOn error
}

// Open connects to a shard and marks it live.
func (t *Tracker) Open(shard string, failOn error) *Conn {
	t.mu.Lock()
	t.live++
	if t.live > t.peak {
		t.peak = t.live
	}
	t.mu.Unlock()
	return &Conn{shard: shard, t: t, failOn: failOn}
}

// Commit applies this participant's part of the transaction.
func (c *Conn) Commit() error {
	return c.failOn
}

// Close releases the shard connection, recording the close order.
func (c *Conn) Close() error {
	c.t.mu.Lock()
	c.t.live--
	c.t.order = append(c.t.order, c.shard)
	c.t.mu.Unlock()
	return nil
}

// Peak reports the maximum number of shard connections held open at once.
func (t *Tracker) Peak() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.peak
}

// CloseOrder returns the order connections were closed in.
func (t *Tracker) CloseOrder() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.order...)
}

// RunDistributedTxLeaky commits each participant shard but defers its
// connection Close inside the loop body. Every shard's connection --
// and the row locks it holds -- stays open until RunDistributedTxLeaky itself
// returns, so all N shards hold locks simultaneously for the whole
// transaction instead of releasing each one right after its own commit. When
// it finally returns, connections close in REVERSE of open order (LIFO),
// which is backwards from the order the transaction coordinator expects
// participants to release in.
func RunDistributedTxLeaky(t *Tracker, shards []string, failOn map[string]error) error {
	var errs []error
	for _, shard := range shards {
		conn := t.Open(shard, failOn[shard])
		defer func() {
			if err := conn.Close(); err != nil {
				errs = append(errs, err)
			}
		}()
		if err := conn.Commit(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RunDistributedTxScoped commits and closes each shard's connection inside a
// per-shard helper whose own return scopes the defer, so a shard's lock is
// released immediately after its commit and before the next shard's
// connection even opens. Peak concurrently open is 1, and connections close
// in the same order they were opened.
func RunDistributedTxScoped(t *Tracker, shards []string, failOn map[string]error) error {
	var errs []error
	for _, shard := range shards {
		err := func() (err error) {
			conn := t.Open(shard, failOn[shard])
			defer func() {
				if cerr := conn.Close(); cerr != nil {
					err = cerr
				}
			}()
			return conn.Commit()
		}()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
```

### The runnable demo

The demo runs both forms over three shards and prints the peak-open count and
the close order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/dtxpool"
)

func main() {
	shards := []string{"orders", "inventory", "ledger"}

	leaky := dtxpool.NewTracker()
	_ = dtxpool.RunDistributedTxLeaky(leaky, shards, nil)
	fmt.Println("leaky  peak-open:", leaky.Peak(), "close-order:", leaky.CloseOrder())

	scoped := dtxpool.NewTracker()
	_ = dtxpool.RunDistributedTxScoped(scoped, shards, nil)
	fmt.Println("scoped peak-open:", scoped.Peak(), "close-order:", scoped.CloseOrder())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
leaky  peak-open: 3 close-order: [ledger inventory orders]
scoped peak-open: 1 close-order: [orders inventory ledger]
```

### Tests

`TestRunDistributedTx` is a table test asserting scoped peaks at 1
concurrently open connection and closes in open order, while leaky peaks at
the shard count and closes in reverse. `TestRunDistributedTxSingleShardEdge
Case` covers the boundary of a single-shard transaction, where LIFO and
open-order close are indistinguishable. `TestRunDistributedTxJoinsCommit
Errors` injects two commit failures and asserts both are reachable through
`errors.Is` on the joined error.

Create `dtxpool_test.go`:

```go
package dtxpool

import (
	"errors"
	"fmt"
	"testing"
)

func TestRunDistributedTx(t *testing.T) {
	shards := []string{"orders", "inventory", "ledger", "shipping"}

	tests := []struct {
		name      string
		run       func(*Tracker, []string, map[string]error) error
		wantPeak  int
		wantOrder []string
	}{
		{
			name:      "scoped releases each shard before the next opens",
			run:       RunDistributedTxScoped,
			wantPeak:  1,
			wantOrder: []string{"orders", "inventory", "ledger", "shipping"},
		},
		{
			name:      "leaky holds every shard open until the batch returns",
			run:       RunDistributedTxLeaky,
			wantPeak:  len(shards),
			wantOrder: []string{"shipping", "ledger", "inventory", "orders"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := NewTracker()
			if err := tt.run(tr, shards, nil); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := tr.Peak(); got != tt.wantPeak {
				t.Fatalf("peak-open = %d, want %d", got, tt.wantPeak)
			}
			got := tr.CloseOrder()
			if len(got) != len(tt.wantOrder) {
				t.Fatalf("close-order = %v, want %v", got, tt.wantOrder)
			}
			for i := range tt.wantOrder {
				if got[i] != tt.wantOrder[i] {
					t.Fatalf("close-order = %v, want %v", got, tt.wantOrder)
				}
			}
		})
	}
}

func TestRunDistributedTxSingleShardEdgeCase(t *testing.T) {
	tr := NewTracker()
	if err := RunDistributedTxScoped(tr, []string{"solo"}, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := tr.Peak(); got != 1 {
		t.Fatalf("peak-open = %d, want 1", got)
	}
}

func TestRunDistributedTxJoinsCommitErrors(t *testing.T) {
	errA := errors.New("inventory commit failed")
	errB := errors.New("ledger commit failed")
	shards := []string{"orders", "inventory", "ledger"}
	failOn := map[string]error{"inventory": errA, "ledger": errB}

	tr := NewTracker()
	err := RunDistributedTxScoped(tr, shards, failOn)
	if err == nil {
		t.Fatal("want a joined error, got nil")
	}
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("joined error does not wrap both commit failures: %v", err)
	}
}

func ExampleRunDistributedTxScoped() {
	tr := NewTracker()
	_ = RunDistributedTxScoped(tr, []string{"a", "b"}, nil)
	fmt.Println("peak:", tr.Peak())
	// Output: peak: 1
}
```

## Review

The transaction runner is correct when scoped commits keep peak concurrently
open connections at 1 and close in open order, while leaky peaks at the shard
count and closes LIFO — the two table rows encode exactly that contrast. The
mechanism to keep straight is that `defer` fires at function return: inside a
bare loop over shards that means "at the end of the whole transaction," which
holds every shard's lock simultaneously; inside a per-shard helper it means
"at the end of this shard's commit," which releases each lock as soon as it's
no longer needed. `errors.Join` is what stops the second commit failure from
being swallowed; `errors.Is` reaching both injected errors proves it.

## Resources

- [Effective Go: defer](https://go.dev/doc/effective_go#defer) — defer timing and LIFO ordering.
- [`database/sql.Tx`](https://pkg.go.dev/database/sql#Tx) — the production transaction type this exercise's `Conn` stands in for.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — aggregating multiple errors into one that `errors.Is` can match.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-parallel-shard-migration-worker-shared-result-index.md](17-parallel-shard-migration-worker-shared-result-index.md) | Next: [19-event-context-enrichment-shared-pointer-mutation.md](19-event-context-enrichment-shared-pointer-mutation.md)
