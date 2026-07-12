# Exercise 11: Correlation ID Rewrite — Defer Argument Snapshot vs Closure

**Nivel: Intermedio** — validacion rapida (un test corto).

A downstream service sometimes returns a canonical order ID that must replace
the correlation ID a request arrived with, so every later audit line refers to
the one ID that actually matters. This module builds the handler that
performs that rewrite and proves its audit log always reports the final ID,
never the one the request started with.

## What you'll build

```text
corrid/                    independent module: example.com/correlation-id-defer-capture
  go.mod                    go 1.24
  corrid.go                 Process(incomingID, canonicalID, log) (status string)
  corrid_test.go            table test: id override cases, logged id and status
```

- Files: `corrid.go`, `corrid_test.go`.
- Implement: `Process(incomingID, canonicalID string, log Logger) (status string)`, where `Logger` is `func(id, status string)`.
- Test: a table over incoming/canonical ID combinations, asserting the id passed to `log` matches whichever id was current at return, not at the `defer` statement.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/07-defer-semantics-and-ordering/11-correlation-id-defer-argument-capture
cd go-solutions/03-control-flow/07-defer-semantics-and-ordering/11-correlation-id-defer-argument-capture
go mod edit -go=1.24
```

### The reassignment defer must still see

`incomingID` is what the request arrived with. If a downstream call already
established a `canonicalID` — the authoritative identifier for this
transaction — every audit line from here on must use it instead, so an
operator searching logs by the canonical ID finds this request too. The audit
log entry itself is written by a deferred closure, and the reassignment of
`id` happens *after* the `defer` statement runs, inside the same function.

That ordering is the entire exercise. A closure that captures the variable
`id` sees whatever `id` holds at the moment the closure actually runs — at
function return — including any reassignment that happened in between. A
deferred call written instead as `defer log(id, status)`, with `id` passed as
a plain argument, would evaluate `id` immediately, at the `defer` statement,
snapshotting the pre-override value. It would keep logging the customer's
original correlation ID even after the function had moved on to using the
canonical one, and every audit line downstream of that would look up the
wrong request.

Create `corrid.go`:

```go
package corrid

// Logger records the correlation id and status for a completed request. In
// production this writes a structured audit log line.
type Logger func(id, status string)

// Process handles a request identified by incomingID. A downstream call can
// return a canonicalID — the authoritative identifier for this transaction
// — which must replace incomingID everywhere afterwards, including in the
// audit log entry Process is responsible for recording.
//
// The audit log is recorded through a deferred closure, not a deferred call
// with id passed as a plain argument. That distinction is the entire point:
// id is reassigned below, after the defer statement runs, and only a
// closure observes that reassignment; a bare `defer log(id, status)` would
// have snapshotted the pre-override id right where it stood.
func Process(incomingID, canonicalID string, log Logger) (status string) {
	id := incomingID
	status = "ok"

	defer func() {
		log(id, status)
	}()

	if canonicalID != "" {
		id = canonicalID
	}

	return
}
```

### Test

`TestProcess` drives `Process` over incoming/canonical ID combinations and
checks both the returned status and the id the injected `Logger` actually
received.

Create `corrid_test.go`:

```go
package corrid

import "testing"

func TestProcess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		incomingID  string
		canonicalID string
		wantID      string
		wantStatus  string
	}{
		{"no canonical id keeps the incoming id", "req-abc", "", "req-abc", "ok"},
		{"canonical id overrides the incoming id", "req-abc", "ord-999", "ord-999", "ok"},
		{"canonical id overrides an empty incoming id", "", "ord-42", "ord-42", "ok"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var gotID, gotStatus string
			log := func(id, status string) {
				gotID = id
				gotStatus = status
			}

			status := Process(tc.incomingID, tc.canonicalID, log)

			if status != tc.wantStatus {
				t.Errorf("returned status = %q, want %q", status, tc.wantStatus)
			}
			if gotID != tc.wantID {
				t.Errorf("logged id = %q, want %q", gotID, tc.wantID)
			}
			if gotStatus != tc.wantStatus {
				t.Errorf("logged status = %q, want %q", gotStatus, tc.wantStatus)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The bug this module guards against never shows up as a compile error or even
a wrong return value — `Process` still returns the right status either way.
It only shows up as an audit trail that quietly points at the wrong ID, which
nobody notices until an operator goes looking for a canonical order ID and
finds nothing, because every log line was written under the customer's
original correlation ID instead. The fix is not "add more logging"; it is
remembering that a deferred closure reads variables live, at call time, while
a deferred argument is frozen at `defer` time.

## Resources

- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — arguments to a deferred call are evaluated when the `defer` statement executes.
- [Go Wiki: CommonMistakes — defer argument evaluation](https://go.dev/wiki/CommonMistakes) — the general form of this trap.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-slow-query-warner.md](10-slow-query-warner.md) | Next: [12-nested-savepoint-release-order.md](12-nested-savepoint-release-order.md)
