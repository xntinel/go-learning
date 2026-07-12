# Exercise 1: Event Processor: Deliberate Discards and a Non-Shadowed Error Path

An event ingestion component decodes JSON, validates, and saves through a `Store`
interface. It reuses `:=` for the first declaration and `=` for the save error so
the returned error is never lost, uses a compile-time interface guard, and
discards the range index deliberately when building IDs.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
eventprocessor/                 module: example.com/eventprocessor
  go.mod
  processor.go                  Event, Store, MemoryStore, Decode, Process, ProcessBatch, IDs
  cmd/
    demo/
      main.go                   runnable demo: batch-process two events, print IDs
  processor_test.go             happy path, save-error, bad payload, canceled context
```

- Files: `processor.go`, `cmd/demo/main.go`, `processor_test.go`.
- Implement: `Decode`, `Process` (decode with `:=`, save with `=`), `ProcessBatch`, `IDs` (discarding the range index), and a `MemoryStore` guarded by `var _ Store = (*MemoryStore)(nil)`.
- Test: happy-path batch, save-error propagation via a `failingStore` (`errors.Is`), bad-payload rejection leaving the store empty, and an already-canceled context surfacing `ctx.Err()` from `Save`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/09-blank-identifier-and-shadowing/01-event-processor-no-shadow/cmd/demo
cd go-solutions/02-variables-types-and-constants/09-blank-identifier-and-shadowing/01-event-processor-no-shadow
```

### Why `=` on the save error is the whole point

`Process` produces its `err` twice: once from `Decode`, once from `Store.Save`.
The first uses `:=` because `err` is new in this scope. The second must use `=`,
because the intent is to reuse the same variable and let it flow to the single
`return`. If you write `if err := store.Save(...); err != nil` with a colon, you
declare a fresh inner `err`; the guard inside the `if` sees it, but any code after
the `if` that reads the outer `err` reads a stale value. Here the return is inside
the `if`, so the naive shadow would still return the right thing — which is
exactly why this bug survives review. The test `TestProcessReturnsSaveError`
proves the wired-up version propagates the save error; the discipline is to reach
for `=` whenever you mean "this is the same error I already have."

`IDs` uses `for _, event := range events`: the index is genuinely unwanted, so `_`
is correct and self-documenting. `var _ Store = (*MemoryStore)(nil)` proves at
compile time that `*MemoryStore` implements `Store` — a typed nil pointer that
allocates nothing.

`Save` checks `ctx.Done()` first via a non-blocking `select`, so an
already-canceled context is surfaced as `ctx.Err()` before any work happens.

Create `processor.go`:

```go
package processor

import (
	"context"
	"encoding/json"
	"fmt"
)

// Event is a single ingested domain event.
type Event struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// Store persists events. Save must honor context cancellation.
type Store interface {
	Save(context.Context, Event) error
}

// MemoryStore is an in-memory Store for tests and demos.
type MemoryStore struct {
	events []Event
}

var _ Store = (*MemoryStore)(nil)

func (m *MemoryStore) Save(ctx context.Context, event Event) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	if event.ID == "" {
		return fmt.Errorf("event id is required")
	}
	m.events = append(m.events, event)
	return nil
}

// Events returns a copy of the stored events.
func (m *MemoryStore) Events() []Event {
	out := make([]Event, len(m.events))
	copy(out, m.events)
	return out
}

// Decode parses and validates a single raw JSON event.
func Decode(raw []byte) (Event, error) {
	var event Event
	if err := json.Unmarshal(raw, &event); err != nil {
		return Event{}, fmt.Errorf("decode event: %w", err)
	}
	if event.ID == "" {
		return Event{}, fmt.Errorf("event id is required")
	}
	if event.Kind == "" {
		return Event{}, fmt.Errorf("event kind is required")
	}
	return event, nil
}

// Process decodes one payload and saves it. The save error is reassigned with =
// into the same err, so a failed Save is never lost.
func Process(ctx context.Context, store Store, raw []byte) error {
	if store == nil {
		return fmt.Errorf("store is required")
	}

	event, err := Decode(raw)
	if err != nil {
		return err
	}

	if err = store.Save(ctx, event); err != nil {
		return fmt.Errorf("save event %q: %w", event.ID, err)
	}
	return nil
}

// ProcessBatch processes each payload, wrapping the first failure with its index.
func ProcessBatch(ctx context.Context, store Store, payloads [][]byte) error {
	for i, payload := range payloads {
		if err := Process(ctx, store, payload); err != nil {
			return fmt.Errorf("payload %d: %w", i, err)
		}
	}
	return nil
}

// IDs extracts the ID of each event; the range index is deliberately discarded.
func IDs(events []Event) []string {
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ID)
	}
	return ids
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"strings"

	"example.com/eventprocessor"
)

func main() {
	var store processor.MemoryStore
	payloads := [][]byte{
		[]byte(`{"id":"evt_1","kind":"created"}`),
		[]byte(`{"id":"evt_2","kind":"updated"}`),
	}

	if err := processor.ProcessBatch(context.Background(), &store, payloads); err != nil {
		fmt.Println("batch failed:", err)
		return
	}

	ids := processor.IDs(store.Events())
	fmt.Printf("stored %d events: %s\n", len(ids), strings.Join(ids, ", "))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stored 2 events: evt_1, evt_2
```

### Tests

`TestProcessReturnsSaveError` fails if `Process` shadows the save `err` and
returns `nil`. `TestProcessCanceledContext` proves an already-canceled context is
surfaced as `context.Canceled` through `Save`.

Create `processor_test.go`:

```go
package processor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

type failingStore struct {
	err error
}

var _ Store = failingStore{}

func (s failingStore) Save(context.Context, Event) error {
	return s.err
}

func TestProcessBatchStoresEvents(t *testing.T) {
	t.Parallel()

	var store MemoryStore
	payloads := [][]byte{
		[]byte(`{"id":"evt_1","kind":"created"}`),
		[]byte(`{"id":"evt_2","kind":"updated"}`),
	}

	if err := ProcessBatch(context.Background(), &store, payloads); err != nil {
		t.Fatal(err)
	}

	if got := IDs(store.Events()); !reflect.DeepEqual(got, []string{"evt_1", "evt_2"}) {
		t.Fatalf("IDs() = %#v", got)
	}
}

func TestProcessReturnsSaveError(t *testing.T) {
	t.Parallel()

	boom := errors.New("store down")
	err := Process(
		context.Background(),
		failingStore{err: boom},
		[]byte(`{"id":"evt_1","kind":"created"}`),
	)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want wrap of %v", err, boom)
	}
}

func TestProcessRejectsBadPayload(t *testing.T) {
	t.Parallel()

	var store MemoryStore
	err := Process(context.Background(), &store, []byte(`{"kind":"created"}`))
	if err == nil {
		t.Fatal("expected error for missing id")
	}
	if len(store.Events()) != 0 {
		t.Fatalf("store should be empty: %#v", store.Events())
	}
}

func TestProcessCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before Process runs

	var store MemoryStore
	err := Process(ctx, &store, []byte(`{"id":"evt_1","kind":"created"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if len(store.Events()) != 0 {
		t.Fatalf("store should be empty after cancel: %#v", store.Events())
	}
}

func Example() {
	var store MemoryStore
	_ = Process(context.Background(), &store, []byte(`{"id":"evt_9","kind":"created"}`))
	fmt.Println(IDs(store.Events()))
	// Output: [evt_9]
}
```

## Review

The processor is correct when a save failure always reaches the caller. The
load-bearing line is `if err = store.Save(ctx, event); err != nil` — the `=`, not
`:=`, keeps it the same `err`. `TestProcessReturnsSaveError` and
`TestProcessCanceledContext` both exercise the failure path that a careless shadow
or a discarded error would hide. The `var _ Store = (*MemoryStore)(nil)` guard
means a signature drift on `Save` breaks the build at the guard, not at some
distant call site. `IDs` discards the range index because it is truly unused;
that is the correct, idiomatic use of `_`. Run `go test -race` to confirm the
batch path and the store hold up.

## Resources

- [Go Specification: Blank identifier](https://go.dev/ref/spec#Blank_identifier) — what `_` is and is not.
- [Effective Go: The blank identifier](https://go.dev/doc/effective_go#blank) — idiomatic discards and interface checks.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel error.
- [`context.Context`](https://pkg.go.dev/context#Context) — `Done` and `Err` semantics.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-sql-row-scanner-discard.md](02-sql-row-scanner-discard.md)
