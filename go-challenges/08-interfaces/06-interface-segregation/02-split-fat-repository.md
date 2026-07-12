# Exercise 2: Refactor a God Store Into Reader, Writer, and TxRunner Role Interfaces

The most common place a backend accretes a fat interface is the data-access
layer. An `OrderStore` starts with `Get` and `List`, then grows `Create`,
`Update`, `Delete`, `CountByStatus`, `WithinTx`, `Ping`, and now a read-only
reporting job is forced to depend on every mutation the store can perform. This
module refactors that god store into three role interfaces — `OrderReader`,
`OrderWriter`, `TxRunner` — so each consumer accepts only the role it calls,
while one concrete `pgStore` keeps satisfying the whole set.

## What you'll build

```text
orderstore/                    independent module: example.com/orderstore
  go.mod                       go 1.24
  store.go                     Order; OrderReader/OrderWriter/TxRunner; fat OrderStore = embed of all three
  consumers.go                 generateReport(OrderReader), mutateStatus(OrderWriter)
  fake.go                      fakeStore implements every role in memory
  cmd/
    demo/
      main.go                  drives report (reader) and a status change (writer)
  store_test.go                reader-only fake drives report; writer path proves reporting seam has no write methods
```

Files: `store.go`, `consumers.go`, `fake.go`, `cmd/demo/main.go`, `store_test.go`.
Implement: `OrderReader` (`Get`/`List`), `OrderWriter` (`Create`/`Update`/`Delete`), `TxRunner` (`WithinTx`), a composed `OrderStore`, and consumers that each take exactly one role.
Test: an in-memory `fakeStore` satisfies every role (compile-time `var _` checks); `generateReport` works with a reader-only fake; `mutateStatus` proves the write seam is separate.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The refactor: keep the type, narrow the parameters

The key insight is that splitting the fat interface changes *nothing* about the
concrete type. `pgStore` (here modeled by an in-memory `fakeStore`, since a live
database is out of scope) still has all the methods it always had. What changes
is the *parameter type* at each consumer. `generateReport` used to take the fat
`OrderStore`; now it takes `OrderReader`, and it structurally cannot call
`Create`, `Update`, or `Delete` — the reporting path is incapable of mutating
order state, enforced by the compiler. `mutateStatus` takes `OrderWriter`. A
consumer that needs to wrap several writes in a transaction takes both
`OrderWriter` and `TxRunner`.

The fat `OrderStore` still exists, defined by *embedding* the three roles. That
is the migration story: existing callers that genuinely need everything keep
using `OrderStore` and do not break, while new or refactored callers narrow to a
role. Because satisfaction is structural, the same `fakeStore` value flows into a
function expecting `OrderReader` and a function expecting `OrderWriter` without
any adaptation.

Every method that touches the store takes `context.Context` as its first
parameter — the standard port shape for a data layer, so cancellation and
deadlines propagate to the driver. Errors from a missing row are a wrapped
sentinel (`ErrNotFound`) so callers match with `errors.Is` rather than parsing
driver-specific strings.

Create `store.go`:

```go
package orderstore

import (
	"context"
	"errors"
)

// ErrNotFound is returned when no order matches the given id.
var ErrNotFound = errors.New("order not found")

// Order is a row in the orders table.
type Order struct {
	ID     string
	Status string
	Total  int64 // cents
}

// OrderReader is the read side. A reporting consumer depends on this and
// structurally cannot mutate.
type OrderReader interface {
	Get(ctx context.Context, id string) (Order, error)
	List(ctx context.Context) ([]Order, error)
}

// OrderWriter is the mutation side.
type OrderWriter interface {
	Create(ctx context.Context, o Order) error
	Update(ctx context.Context, o Order) error
	Delete(ctx context.Context, id string) error
}

// TxRunner runs a function inside a transaction.
type TxRunner interface {
	WithinTx(ctx context.Context, fn func(context.Context) error) error
}

// OrderStore is the full data-access surface, composed by embedding the roles.
// Callers that genuinely need everything depend on this; most depend on a role.
type OrderStore interface {
	OrderReader
	OrderWriter
	TxRunner
}
```

Create `consumers.go`. Each function's parameter type is the exact role it uses:

```go
package orderstore

import (
	"context"
	"fmt"
)

// Report is the output of the reporting job.
type Report struct {
	Count   int
	Revenue int64
}

// generateReport reads orders and sums revenue. It takes OrderReader, so it
// cannot Create/Update/Delete: the reporting seam has no write authority.
func generateReport(ctx context.Context, r OrderReader) (Report, error) {
	orders, err := r.List(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("list orders: %w", err)
	}
	var rep Report
	for _, o := range orders {
		if o.Status == "paid" {
			rep.Count++
			rep.Revenue += o.Total
		}
	}
	return rep, nil
}

// mutateStatus updates one order's status. It takes OrderWriter (plus a read to
// fetch the row) — deliberately a different seam from the reporting path.
func mutateStatus(ctx context.Context, store interface {
	OrderReader
	OrderWriter
}, id, status string) error {
	o, err := store.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("get %s: %w", id, err)
	}
	o.Status = status
	if err := store.Update(ctx, o); err != nil {
		return fmt.Errorf("update %s: %w", id, err)
	}
	return nil
}
```

Create `fake.go`. The in-memory implementation satisfies every role:

```go
package orderstore

import (
	"context"
	"sync"
)

// fakeStore is an in-memory OrderStore standing in for a real pgStore.
type fakeStore struct {
	mu     sync.Mutex
	orders map[string]Order
}

func newFakeStore() *fakeStore {
	return &fakeStore{orders: make(map[string]Order)}
}

func (s *fakeStore) Get(_ context.Context, id string) (Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, ErrNotFound
	}
	return o, nil
}

func (s *fakeStore) List(_ context.Context) ([]Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Order, 0, len(s.orders))
	for _, o := range s.orders {
		out = append(out, o)
	}
	return out, nil
}

func (s *fakeStore) Create(_ context.Context, o Order) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[o.ID] = o
	return nil
}

func (s *fakeStore) Update(_ context.Context, o Order) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orders[o.ID]; !ok {
		return ErrNotFound
	}
	s.orders[o.ID] = o
	return nil
}

func (s *fakeStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orders[id]; !ok {
		return ErrNotFound
	}
	delete(s.orders, id)
	return nil
}

func (s *fakeStore) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	// A real pgStore would BEGIN/COMMIT; the fake just runs the function.
	return fn(ctx)
}

// Compile-time proof one concrete type satisfies every role and the full store.
var (
	_ OrderReader = (*fakeStore)(nil)
	_ OrderWriter = (*fakeStore)(nil)
	_ TxRunner    = (*fakeStore)(nil)
	_ OrderStore  = (*fakeStore)(nil)
)
```

### The runnable demo

Create `cmd/demo/main.go`. Because `generateReport` and `fakeStore` are
unexported, the demo drives the store through exported seams. To keep the module
self-contained yet demonstrable, expose a tiny exported façade.

Add to `store.go`:

```go
// Seed builds a store preloaded with orders, for demos and callers that want a
// ready OrderStore without touching the unexported fake.
func Seed(orders ...Order) OrderStore {
	s := newFakeStore()
	for _, o := range orders {
		s.orders[o.ID] = o
	}
	return s
}

// Revenue reads paid orders through an OrderReader and returns count + cents.
// Exported wrapper over generateReport so external callers get the read seam.
func Revenue(ctx context.Context, r OrderReader) (int, int64, error) {
	rep, err := generateReport(ctx, r)
	if err != nil {
		return 0, 0, err
	}
	return rep.Count, rep.Revenue, nil
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/orderstore"
)

func main() {
	ctx := context.Background()
	store := orderstore.Seed(
		orderstore.Order{ID: "o1", Status: "paid", Total: 1200},
		orderstore.Order{ID: "o2", Status: "pending", Total: 900},
		orderstore.Order{ID: "o3", Status: "paid", Total: 4300},
	)

	// The report path receives the store as an OrderReader only.
	var reader orderstore.OrderReader = store
	count, cents, _ := orderstore.Revenue(ctx, reader)
	fmt.Printf("paid orders: %d, revenue: %d cents\n", count, cents)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
paid orders: 2, revenue: 5500 cents
```

### Tests

Create `store_test.go`:

```go
package orderstore

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestGenerateReportWorksWithReaderOnlyFake(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	ctx := context.Background()
	_ = s.Create(ctx, Order{ID: "o1", Status: "paid", Total: 1000})
	_ = s.Create(ctx, Order{ID: "o2", Status: "paid", Total: 2500})
	_ = s.Create(ctx, Order{ID: "o3", Status: "refunded", Total: 500})

	// Pass the fake through the narrow reader seam.
	var r OrderReader = s
	rep, err := generateReport(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Count != 2 {
		t.Fatalf("Count = %d, want 2", rep.Count)
	}
	if rep.Revenue != 3500 {
		t.Fatalf("Revenue = %d, want 3500", rep.Revenue)
	}
}

func TestMutateStatusUsesWriteSeam(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	ctx := context.Background()
	_ = s.Create(ctx, Order{ID: "o1", Status: "pending", Total: 1000})

	if err := mutateStatus(ctx, s, "o1", "paid"); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(ctx, "o1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "paid" {
		t.Fatalf("Status = %q, want paid", got.Status)
	}
}

func TestMutateStatusPropagatesNotFound(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	err := mutateStatus(context.Background(), s, "missing", "paid")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestWithinTxRunsFunction(t *testing.T) {
	t.Parallel()

	s := newFakeStore()
	ctx := context.Background()
	var ran bool
	err := s.WithinTx(ctx, func(ctx context.Context) error {
		ran = true
		return s.Create(ctx, Order{ID: "o1", Status: "paid", Total: 700})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("tx function did not run")
	}
	if _, err := s.Get(ctx, "o1"); err != nil {
		t.Fatalf("order not persisted: %v", err)
	}
}

func ExampleRevenue() {
	store := Seed(
		Order{ID: "o1", Status: "paid", Total: 1200},
		Order{ID: "o2", Status: "paid", Total: 300},
	)
	count, cents, _ := Revenue(context.Background(), store)
	fmt.Println(count, cents)
	// Output: 2 1500
}
```

## Review

The refactor is correct when the concrete type is untouched and only parameter
types narrowed: `generateReport` compiles against a value typed as `OrderReader`
and physically has no `Create`/`Delete` in scope, so a reviewer knows at a glance
that reporting cannot mutate. The compile-time `var _ OrderReader = (*fakeStore)(nil)`
block is what makes a dropped method fail at the type definition instead of at a
distant call site. The trap to avoid is re-exporting the fat `OrderStore` from a
constructor and forcing every caller to depend on the whole surface; instead the
full interface exists only for the rare caller that needs all roles, and each
consumer declares the role it uses. Run `go test -race` to confirm the fake's map
is safe under the concurrent access a real store would see.

## Resources

- [Go Code Review Comments: Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces)
- [Effective Go: Interfaces](https://go.dev/doc/effective_go#interfaces)
- [database/sql package](https://pkg.go.dev/database/sql)
- [Interface Segregation Principle](https://en.wikipedia.org/wiki/Interface_segregation_principle)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-segregated-job-queue.md](01-segregated-job-queue.md) | Next: [03-consumer-defined-handler-dep.md](03-consumer-defined-handler-dep.md)
