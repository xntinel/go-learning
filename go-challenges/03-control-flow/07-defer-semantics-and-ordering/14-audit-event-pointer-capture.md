# Exercise 14: Deferred Audit Event — Closure Over a Pointer, Not a Snapshot

**Nivel: Intermedio** — validacion rapida (un test corto).

An order-processing audit trail needs to record the final outcome of a
multi-step function — was it priced, was it charged, how did it end — even
when the function returns early from a mid-step failure. This module builds
that audit event as a struct filled in progressively and shows why the
deferred recorder must close over a pointer to it, not a value copied at the
`defer` statement.

## What you'll build

```text
audit/                      independent module: example.com/audit-event-pointer-capture
  go.mod                     go 1.24
  audit.go                   Event, Recorder, Process(ev, price, charge, record)
  audit_test.go              table test over price/charge outcomes
```

- Files: `audit.go`, `audit_test.go`.
- Implement: `Event` struct, `Recorder` func type, and `Process(ev *Event, price, charge func() bool, record Recorder)`.
- Test: a table over pricing/charging success combinations, asserting the recorded event's final field values.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/07-defer-semantics-and-ordering/14-audit-event-pointer-capture
cd go-solutions/03-control-flow/07-defer-semantics-and-ordering/14-audit-event-pointer-capture
go mod edit -go=1.24
```

### Pointer capture versus value snapshot

`Process` fills in `ev`'s fields as it works: it marks the status, then
`Priced` once pricing succeeds, then `Charged` once charging succeeds, with
early returns on either failure. The deferred closure that hands `ev` to
`record` runs at function return, by which point every field assignment
above it — however many of them actually ran — has already happened.

The closure captures the *pointer* `ev`, which is what makes this work: it
reads through the pointer at call time, so it sees the struct exactly as
`Process` last left it, on whichever path was taken. A deferred call written
instead as `defer record(*ev)` would dereference `ev` immediately, at the
`defer` statement, copying the struct's state at that instant — `Priced` and
`Charged` both still `false`, `Status` not yet set to anything meaningful —
and freeze that stale snapshot as what gets recorded, regardless of how the
rest of `Process` goes on to change the real event.

Create `audit.go`:

```go
package audit

// Event captures the outcome of processing one order for an audit trail.
// Its fields are filled in progressively as Process runs.
type Event struct {
	OrderID string
	Priced  bool
	Charged bool
	Status  string
}

// Recorder persists a finished audit event. Production code writes to an
// append-only audit log; here it is injected so the test can capture it.
type Recorder func(Event)

// Process fills in ev as it works through pricing and charging an order, and
// defers a closure that hands ev to record. Because the closure captures
// the pointer ev — not a copy of *ev taken at the defer statement — it
// always observes every field exactly as Process left them at the moment of
// return, including on the early-return paths where pricing or charging
// fails partway through. A deferred call written as `defer record(*ev)`
// instead would copy the struct immediately, freezing it at whatever state
// it held right after the defer statement — Priced and Charged both false,
// Status still empty — regardless of what Process goes on to do.
func Process(ev *Event, price func() bool, charge func() bool, record Recorder) {
	defer func() {
		record(*ev)
	}()

	ev.Status = "processing"

	if !price() {
		ev.Status = "price_failed"
		return
	}
	ev.Priced = true

	if !charge() {
		ev.Status = "charge_failed"
		return
	}
	ev.Charged = true
	ev.Status = "complete"
}
```

### Test

`TestProcess` drives `Process` with fake `price` and `charge` functions over
the three outcomes that matter — pricing fails, charging fails, both
succeed — and checks every field of the recorded event.

Create `audit_test.go`:

```go
package audit

import "testing"

func TestProcess(t *testing.T) {
	tests := []struct {
		name        string
		priceOK     bool
		chargeOK    bool
		wantStatus  string
		wantPriced  bool
		wantCharged bool
	}{
		{"pricing fails", false, false, "price_failed", false, false},
		{"charging fails after pricing succeeds", true, false, "charge_failed", true, false},
		{"completes successfully", true, true, "complete", true, true},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ev := &Event{OrderID: "ord-1"}
			var recorded Event

			record := func(e Event) { recorded = e }
			price := func() bool { return tc.priceOK }
			charge := func() bool { return tc.chargeOK }

			Process(ev, price, charge, record)

			if recorded.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", recorded.Status, tc.wantStatus)
			}
			if recorded.Priced != tc.wantPriced {
				t.Errorf("priced = %v, want %v", recorded.Priced, tc.wantPriced)
			}
			if recorded.Charged != tc.wantCharged {
				t.Errorf("charged = %v, want %v", recorded.Charged, tc.wantCharged)
			}
			if recorded.OrderID != "ord-1" {
				t.Errorf("orderID = %q, want %q", recorded.OrderID, "ord-1")
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The three test cases are really testing three different amounts of
`Process`'s body running before it returns, and in every one of them the
recorded event matches reality. That is only true because the deferred
closure reads `ev` through its pointer at call time rather than capturing a
value at `defer` time. The general lesson travels well beyond audit events:
any time a deferred closure needs to see "whatever this ended up being," it
must close over the pointer or the variable, not a value copied into the
`defer` statement's argument list.

## Resources

- [Go Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — arguments to a deferred call are evaluated when the `defer` statement executes.
- [Effective Go: Defer](https://go.dev/doc/effective_go#defer) — the canonical cleanup idiom this pattern extends to a mutable audit record.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-inflight-request-gauge.md](13-inflight-request-gauge.md) | Next: [15-connection-pool-health-check.md](15-connection-pool-health-check.md)
