# Exercise 16: Webhook Batch Processor: Defer-Accumulation Timeout Leak

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde).

A batch processor delivers a list of webhooks, giving each its own
cancellable timeout context. The obvious `defer wc.Cancel()` inside the loop
body is a timeout-budget leak: every deferred cancel queues up and none of
them run until the whole batch function returns, so every webhook's context
stays active — and every other in-flight system still watching it — for the
entire batch instead of just its own delivery.

## What you'll build

```text
webhookbatch/                independent module: example.com/webhookbatch
  go.mod                     go 1.24
  webhookbatch.go             Tracker, WebhookCtx, ProcessBatchLeaky, ProcessBatchScoped
  cmd/
    demo/
      main.go                runnable demo: run both, print peak-active count
  webhookbatch_test.go         table test: peak-active==1 vs ==N, join errors, close order
```

- Files: `webhookbatch.go`, `cmd/demo/main.go`, `webhookbatch_test.go`.
- Implement: `ProcessBatchLeaky` (defer cancel inside the loop, every context outlives the loop) and `ProcessBatchScoped` (a per-webhook helper that scopes the defer), both aggregating delivery errors with `errors.Join`.
- Test: a `Tracker` records how many webhook contexts are simultaneously active; assert scoped keeps peak active at 1 and cancels each before opening the next, while leaky peaks at N; assert delivery errors surface through the joined error.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/webhookbatch/cmd/demo
cd ~/go-exercises/webhookbatch
go mod init example.com/webhookbatch
go mod edit -go=1.24
```

### Why the naive version leaks timeout budgets

`defer` schedules a call to run when the *enclosing function* returns, not
when the loop iteration ends. In `ProcessBatchLeaky`, every `wc.Cancel()` is
queued and none runs until `ProcessBatchLeaky` itself returns. During the
loop, webhook 0's context is still active when webhook 1 opens, and so on:
the peak number of simultaneously active timeout contexts equals N. In
production this is the same shape as a file-descriptor leak, but for
cancellation budgets — a batch of ten thousand webhooks holds ten thousand
contexts alive at once, and anything downstream still watching `wc.Ctx.Done()`
(a retry goroutine, a circuit breaker) keeps waiting on all of them until the
whole batch finishes.

`ProcessBatchScoped` fixes it by moving the per-webhook work into a helper
function whose own `return` scopes the `defer`. Each call to the helper opens
a context, defers its cancel, delivers the webhook, and returns — at which
point the deferred cancel runs, before the next iteration even opens a
context. Peak active is 1, and every context is fully released before the
next one exists.

The `Tracker` is an in-memory fake that models a pool of webhook timeout
contexts. It records how many are active and tracks the peak so a test can
read it, plus the order contexts were cancelled in. `errors.Join` collects a
delivery error from every webhook into one error whose `Unwrap() []error`
lets `errors.Is` match any of them.

Create `webhookbatch.go`:

```go
package webhookbatch

import (
	"context"
	"errors"
	"sync"
)

// WebhookCtx pairs a webhook delivery's context with the cancel func that
// releases whatever timeout budget it was given.
type WebhookCtx struct {
	ID     string
	Ctx    context.Context
	Cancel context.CancelFunc
}

// Tracker stands in for a pool of outstanding webhook timeout budgets. It
// records how many are simultaneously active (context not yet cancelled) and
// the peak concurrently active count, so a test can prove whether cancels
// happened promptly or piled up.
type Tracker struct {
	mu     sync.Mutex
	active map[string]bool
	peak   int
	closed []string
}

// NewTracker returns an empty Tracker.
func NewTracker() *Tracker {
	return &Tracker{active: make(map[string]bool)}
}

// Open starts a webhook's timeout context and marks it active.
func (t *Tracker) Open(id string) *WebhookCtx {
	ctx, cancel := context.WithCancel(context.Background())
	t.mu.Lock()
	t.active[id] = true
	if len(t.active) > t.peak {
		t.peak = len(t.active)
	}
	t.mu.Unlock()

	wc := &WebhookCtx{ID: id, Ctx: ctx}
	wc.Cancel = func() {
		cancel()
		t.mu.Lock()
		if t.active[id] {
			delete(t.active, id)
			t.closed = append(t.closed, id)
		}
		t.mu.Unlock()
	}
	return wc
}

// ActiveCount reports how many webhook contexts are currently un-cancelled.
func (t *Tracker) ActiveCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.active)
}

// Peak reports the maximum number of webhook contexts active at once.
func (t *Tracker) Peak() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.peak
}

// ClosedOrder returns the order in which contexts were cancelled.
func (t *Tracker) ClosedOrder() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.closed...)
}

// ProcessBatchLeaky delivers every webhook, but defers the cancel of each
// one's timeout context inside the loop body. `defer` only runs when
// ProcessBatchLeaky itself returns, so EVERY webhook's timeout budget stays
// active for the whole batch: webhook 0's context is still live while webhook
// 50 is being delivered. In production this is the classic "context leak in
// a loop" -- a batch of ten thousand webhooks holds ten thousand timeout
// contexts (and their timers, if real ones were used) alive at once.
func ProcessBatchLeaky(t *Tracker, ids []string, deliver func(context.Context, string) error) error {
	var errs []error
	for _, id := range ids {
		wc := t.Open(id)
		defer wc.Cancel() // BUG: queued until ProcessBatchLeaky returns
		if err := deliver(wc.Ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ProcessBatchScoped delivers every webhook through a per-item helper whose
// own return scopes the defer, so each webhook's timeout context is
// cancelled before the next one is even opened. Peak active count is 1.
func ProcessBatchScoped(t *Tracker, ids []string, deliver func(context.Context, string) error) error {
	var errs []error
	for _, id := range ids {
		err := func() (err error) {
			wc := t.Open(id)
			defer wc.Cancel()
			return deliver(wc.Ctx, id)
		}()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
```

### The runnable demo

The demo processes three webhooks with both variants and prints the peak
active-context count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/webhookbatch"
)

func main() {
	ids := []string{"wh-1", "wh-2", "wh-3"}
	deliver := func(_ context.Context, _ string) error { return nil }

	leaky := webhookbatch.NewTracker()
	_ = webhookbatch.ProcessBatchLeaky(leaky, ids, deliver)
	fmt.Println("leaky  peak-active:", leaky.Peak())

	scoped := webhookbatch.NewTracker()
	_ = webhookbatch.ProcessBatchScoped(scoped, ids, deliver)
	fmt.Println("scoped peak-active:", scoped.Peak())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
leaky  peak-active: 3
scoped peak-active: 1
```

### Tests

`TestProcessBatch` is a table test asserting scoped peaks at 1 active
context and leaky peaks at N, and that both leave zero contexts active once
the batch returns. `TestProcessBatchScopedCancelsEachBeforeNextOpen` asserts
the delivery func never observes an already-cancelled context and that
contexts close in delivery order. `TestProcessBatchAggregatesDeliveryErrors`
injects two delivery failures and asserts both are reachable through
`errors.Is` on the joined error.

Create `webhookbatch_test.go`:

```go
package webhookbatch

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestProcessBatch(t *testing.T) {
	deliver := func(_ context.Context, _ string) error { return nil }
	ids := []string{"a", "b", "c", "d"}

	tests := []struct {
		name    string
		process func(*Tracker, []string, func(context.Context, string) error) error
		want    int // peak active count
	}{
		{name: "scoped cancels before the next open", process: ProcessBatchScoped, want: 1},
		{name: "leaky holds every context active until return", process: ProcessBatchLeaky, want: len(ids)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := NewTracker()
			if err := tt.process(tr, ids, deliver); err != nil {
				t.Fatalf("process: %v", err)
			}
			if got := tr.Peak(); got != tt.want {
				t.Fatalf("peak-active = %d, want %d", got, tt.want)
			}
			if got := tr.ActiveCount(); got != 0 {
				t.Fatalf("active-count after return = %d, want 0", got)
			}
		})
	}
}

func TestProcessBatchScopedCancelsEachBeforeNextOpen(t *testing.T) {
	ids := []string{"a", "b", "c"}
	deliver := func(ctx context.Context, id string) error {
		select {
		case <-ctx.Done():
			t.Fatalf("context for %s already cancelled during delivery", id)
		default:
		}
		return nil
	}

	tr := NewTracker()
	if err := ProcessBatchScoped(tr, ids, deliver); err != nil {
		t.Fatalf("ProcessBatchScoped: %v", err)
	}
	if got := tr.Peak(); got != 1 {
		t.Fatalf("peak-active = %d, want 1", got)
	}
	want := []string{"a", "b", "c"}
	got := tr.ClosedOrder()
	if len(got) != len(want) {
		t.Fatalf("closed order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("closed order = %v, want %v", got, want)
		}
	}
}

func TestProcessBatchAggregatesDeliveryErrors(t *testing.T) {
	errBoom1 := errors.New("wh-2 delivery failed")
	errBoom2 := errors.New("wh-4 delivery failed")
	ids := []string{"wh-1", "wh-2", "wh-3", "wh-4"}
	deliver := func(_ context.Context, id string) error {
		switch id {
		case "wh-2":
			return errBoom1
		case "wh-4":
			return errBoom2
		default:
			return nil
		}
	}

	tr := NewTracker()
	err := ProcessBatchScoped(tr, ids, deliver)
	if err == nil {
		t.Fatal("want a joined error, got nil")
	}
	if !errors.Is(err, errBoom1) || !errors.Is(err, errBoom2) {
		t.Fatalf("joined error does not wrap both failures: %v", err)
	}
}

func ExampleProcessBatchScoped() {
	deliver := func(_ context.Context, _ string) error { return nil }
	tr := NewTracker()
	_ = ProcessBatchScoped(tr, []string{"x", "y"}, deliver)
	fmt.Println("peak:", tr.Peak())
	// Output: peak: 1
}
```

## Review

The processor is correct when scoped delivery keeps peak active contexts at
1, while leaky peaks at N — the table test encodes exactly that contrast, so
the leak cannot silently reappear. The mechanism to keep straight is that
`defer` fires at function return: inside a bare loop that means "at the end
of the whole batch," which is the leak; inside a per-webhook helper it means
"at the end of this delivery," which is the fix. `errors.Join` is what stops
the second delivery failure from being swallowed; `errors.Is` reaching both
injected errors proves it.

## Resources

- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) — the cancel func this exercise's timeout budgets stand in for.
- [Effective Go: defer](https://go.dev/doc/effective_go#defer) — defer timing and why it fires at function return, not loop-iteration end.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — aggregating multiple errors into one that `errors.Is` can match.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-sliding-window-rate-limit-ticker-capture.md](15-sliding-window-rate-limit-ticker-capture.md) | Next: [17-parallel-shard-migration-worker-shared-result-index.md](17-parallel-shard-migration-worker-shared-result-index.md)
