# Exercise 6: Detached Audit Write That Outlives the Request

Some work must survive the request that triggered it: an audit record dropped
because the user closed the tab is a compliance hole. But that work still needs
the request-scoped values — the request id, the principal — for its own logging.
`context.WithoutCancel` is the exact tool: it keeps the parent's values while
dropping the parent's cancellation. This exercise builds an audit handler that
detaches its write correctly, and a control that shows the bug of passing
`r.Context()` straight through.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
detachedaudit/             independent module: example.com/detachedaudit
  go.mod                   go 1.26
  audit.go                 AuditStore fake; RequestIDFrom; AuditHandler(store, wg, detached)
  cmd/
    demo/
      main.go              detached write survives a cancelled request
  audit_test.go            detached-survives test, r.Context()-aborts control test
```

Files: `audit.go`, `cmd/demo/main.go`, `audit_test.go`.
Implement: `context.WithoutCancel(r.Context())` bounded by its own `context.WithTimeout`, joined with a `sync.WaitGroup`; a control path that passes `r.Context()` directly.
Test: cancel `r.Context()` right after the handler returns; the detached write still completes and carries the correct request id; the control write is aborted.
Verify: `go test -count=1 -race ./...`

## The design

The handler responds to the client first, then persists an audit record in a
background goroutine. If that goroutine used `r.Context()`, the write would be
aborted the instant the client disconnected — which is common and legitimate
client behavior — so the audit trail would have holes exactly when you most want
it (a user who bailed mid-action). The fix is `context.WithoutCancel(r.Context())`:
the returned context is never cancelled by the parent, but it still resolves the
parent's `Value` lookups, so `RequestIDFrom` inside the goroutine returns the same
id the handler saw. The detached work is then bounded by its own
`context.WithTimeout` so a stuck audit store cannot leak a goroutine forever, and
joined with a `sync.WaitGroup` so a test — and a graceful shutdown — can wait for
it to finish.

The fake `AuditStore.Save` honors its context (a `select` on `ctx.Done()` versus a
short simulated write) precisely so the two paths are distinguishable: with a
detached context the write completes; with the raw `r.Context()`, cancelling the
request makes `Save` return `ctx.Err()` and record nothing. The `detached bool`
parameter selects which context the handler hands to `Save`, so one handler
constructor demonstrates both the correct pattern and the bug.

Create `audit.go`:

```go
package detachedaudit

import (
	"context"
	"net/http"
	"sync"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = 0

// RequestIDFrom returns the request id stored in ctx, or "" if none.
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// WithRequestID stores id in ctx (used by the handler to seed the value that
// WithoutCancel must preserve).
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// Record is one persisted audit entry.
type Record struct {
	RequestID string
	Action    string
}

// AuditStore is an in-memory fake whose Save honors its context so the two
// handler paths (detached vs raw r.Context()) are observably different.
type AuditStore struct {
	mu      sync.Mutex
	records []Record
}

// Save simulates a short database write. If ctx is cancelled before the write
// completes, it records nothing and returns ctx.Err().
func (s *AuditStore) Save(ctx context.Context, rec Record) error {
	select {
	case <-time.After(20 * time.Millisecond):
		s.mu.Lock()
		s.records = append(s.records, rec)
		s.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Records returns a copy of the persisted records.
func (s *AuditStore) Records() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.records...)
}

// AuditHandler responds immediately, then persists an audit record in the
// background. When detached is true it uses context.WithoutCancel(r.Context())
// (keeps the request id, drops cancellation) bounded by its own timeout — the
// correct pattern. When detached is false it passes r.Context() straight to the
// store — the bug: the write aborts the instant the client disconnects. wg lets
// callers join the background work.
func AuditHandler(store *AuditStore, wg *sync.WaitGroup, detached bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("accepted"))

		var bg context.Context
		var cancel context.CancelFunc
		if detached {
			bg, cancel = context.WithTimeout(context.WithoutCancel(r.Context()), time.Second)
		} else {
			bg, cancel = context.WithCancel(r.Context())
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			_ = store.Save(bg, Record{RequestID: RequestIDFrom(bg), Action: "login"})
		}()
	}
}
```

## The runnable demo

The demo serves one request through the detached handler, cancels the request
context immediately after the handler returns, waits for the background write,
and prints the persisted record — proving cancellation was severed but the id
survived.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"

	"example.com/detachedaudit"
)

func main() {
	store := &detachedaudit.AuditStore{}
	var wg sync.WaitGroup
	h := detachedaudit.AuditHandler(store, &wg, true)

	ctx, cancel := context.WithCancel(detachedaudit.WithRequestID(context.Background(), "req-42"))
	req := httptest.NewRequest("POST", "/login", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	h(rec, req)
	cancel() // client disconnects the instant the response is sent
	wg.Wait()

	records := store.Records()
	fmt.Printf("response=%d records=%d\n", rec.Code, len(records))
	if len(records) == 1 {
		fmt.Printf("persisted request_id=%s action=%s\n", records[0].RequestID, records[0].Action)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
response=200 records=1
persisted request_id=req-42 action=login
```

## Tests

`TestDetachedWriteSurvivesCancel` is the positive contract: cancel the request
context right after the handler returns, and the detached write still completes
and carries the request id (proving `WithoutCancel` severed cancellation but
preserved the value). `TestRawContextAbortsWrite` is the control: the same
scenario with `detached=false` records nothing, demonstrating the failure mode.
Both use `httptest.NewRequest` + `NewRecorder` so the request context is fully
under the test's control, and a `WaitGroup` joins the background work before the
assertions.

Create `audit_test.go`:

```go
package detachedaudit

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
)

func serveAndCancel(t *testing.T, detached bool) *AuditStore {
	t.Helper()
	store := &AuditStore{}
	var wg sync.WaitGroup
	h := AuditHandler(store, &wg, detached)

	ctx, cancel := context.WithCancel(WithRequestID(context.Background(), "req-42"))
	req := httptest.NewRequest("POST", "/login", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	h(rec, req)
	if rec.Code != 200 {
		t.Fatalf("response code = %d, want 200", rec.Code)
	}
	cancel() // simulate client disconnect immediately after the response
	wg.Wait()
	return store
}

func TestDetachedWriteSurvivesCancel(t *testing.T) {
	t.Parallel()
	store := serveAndCancel(t, true)

	records := store.Records()
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1 (detached write should survive cancel)", len(records))
	}
	if records[0].RequestID != "req-42" {
		t.Fatalf("record request id = %q, want req-42 (value must survive WithoutCancel)", records[0].RequestID)
	}
}

func TestRawContextAbortsWrite(t *testing.T) {
	t.Parallel()
	store := serveAndCancel(t, false)

	if records := store.Records(); len(records) != 0 {
		t.Fatalf("records = %d, want 0 (raw r.Context() write must abort on cancel)", len(records))
	}
}
```

## Review

The handler is correct when the background write descends from
`context.WithoutCancel(r.Context())` (values kept, cancellation dropped) and is
bounded by its own timeout and joined with a `WaitGroup`. The control test
encodes the bug it prevents: pass `r.Context()` directly and the write is aborted
the instant the client disconnects, silently losing audit records exactly for the
users who bail. Two things are easy to get wrong: forgetting the private
`WithTimeout` on the detached context (so a stuck store leaks a goroutine
forever), and forgetting to join the goroutine (so a test races the write). Run
with `-race`; the store's mutex and the `WaitGroup` keep the background write and
the assertions ordered.

## Resources

- [`context.WithoutCancel`](https://pkg.go.dev/context#WithoutCancel) — a context that keeps values but ignores the parent's cancellation.
- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) — bounding the detached work so it cannot run forever.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — joining background work deterministically.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-cancellation-cause-classification.md](05-cancellation-cause-classification.md) | Next: [07-deadline-budget-retry-client.md](07-deadline-budget-retry-client.md)
