# Exercise 22: Audit Logger Factory Capturing Tenant Context

**Nivel: Intermedio** — validacion rapida (un test corto).

A multi-tenant system's audit trail is only trustworthy if every entry is
correctly attributed to the tenant it belongs to — and a call site that has
to remember to pass a tenant ID on every log call will eventually forget.
`NewLogger` captures the tenant once, at the point where a handler or worker
is wired up for a specific tenant, and returns a closure whose call sites
only ever pass the action; the tenant is structurally impossible to omit or
mix up.

## What you'll build

```text
auditlog/                  independent module: example.com/audit-log-tenant-context
  go.mod                   go 1.24
  auditlog.go              Entry, NewLogger returns func(action string)
  cmd/
    demo/
      main.go               two tenant loggers writing to one shared sink
  auditlog_test.go          table test: tenant stamped, two loggers isolated
```

- Files: `auditlog.go`, `cmd/demo/main.go`, `auditlog_test.go`.
- Implement: `NewLogger(tenantID string, sink func(Entry)) func(action string)`, closing over `tenantID` and `sink`.
- Test: a table logs several actions through one logger and checks every entry is stamped with the same tenant; a second test proves two loggers writing to the same sink never mix up their tenant.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Capturing context so the caller can't forget it

`NewLogger` is a factory in the plainest sense: it takes the one piece of
context (`tenantID`) that every audit entry for this logger must carry, plus
a `sink` — the function that actually persists or forwards the entry — and
returns a closure over both. The returned `func(action string)` has a
signature that makes it *impossible* to pass the wrong tenant, or to forget
the tenant entirely: there is no tenant parameter for a caller to get wrong,
because the tenant was fixed the moment the logger was constructed, typically
once per request or once per worker bound to a specific tenant.

This is the same shape as a `context.Context` carrying a tenant ID through a
call chain, but even simpler and impossible to strip accidentally: nothing
external can read or clear the captured `tenantID` the way a context value
can be dropped by a handler that forgets to propagate it. Multiple loggers
constructed from the same `sink` — one per tenant — write to one shared
stream but never confuse whose entry is whose, because each closure carries
its own independent `tenantID`.

Create `auditlog.go`:

```go
package auditlog

// Entry is one recorded audit event, always stamped with the tenant it
// belongs to.
type Entry struct {
	Tenant string
	Action string
}

// NewLogger returns a closure that captures tenantID and a sink, so every
// call site only ever passes the action — the tenant is baked in once at
// construction time and cannot be forgotten or spoofed by a caller who
// forgets to pass it.
func NewLogger(tenantID string, sink func(Entry)) func(action string) {
	return func(action string) {
		sink(Entry{Tenant: tenantID, Action: action})
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/audit-log-tenant-context"
)

func main() {
	var entries []auditlog.Entry
	sink := func(e auditlog.Entry) { entries = append(entries, e) }

	acmeLog := auditlog.NewLogger("acme-corp", sink)
	globexLog := auditlog.NewLogger("globex", sink)

	acmeLog("user.created")
	globexLog("invoice.paid")
	acmeLog("user.deleted")

	for _, e := range entries {
		fmt.Printf("tenant=%s action=%s\n", e.Tenant, e.Action)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
tenant=acme-corp action=user.created
tenant=globex action=invoice.paid
tenant=acme-corp action=user.deleted
```

### Tests

Create `auditlog_test.go`:

```go
package auditlog

import "testing"

func TestLoggerStampsTenantOnEveryEntry(t *testing.T) {
	var entries []Entry
	sink := func(e Entry) { entries = append(entries, e) }

	log := NewLogger("acme-corp", sink)

	tests := []string{"user.created", "user.deleted", "invoice.paid"}
	for _, action := range tests {
		log(action)
	}

	if len(entries) != len(tests) {
		t.Fatalf("entries = %d, want %d", len(entries), len(tests))
	}
	for i, action := range tests {
		if entries[i].Tenant != "acme-corp" {
			t.Errorf("entry %d: tenant = %q, want acme-corp", i, entries[i].Tenant)
		}
		if entries[i].Action != action {
			t.Errorf("entry %d: action = %q, want %q", i, entries[i].Action, action)
		}
	}
}

func TestTwoLoggersStampDifferentTenants(t *testing.T) {
	var entries []Entry
	sink := func(e Entry) { entries = append(entries, e) }

	acmeLog := NewLogger("acme-corp", sink)
	globexLog := NewLogger("globex", sink)

	acmeLog("a")
	globexLog("b")

	if entries[0].Tenant != "acme-corp" {
		t.Fatalf("entries[0].Tenant = %q, want acme-corp", entries[0].Tenant)
	}
	if entries[1].Tenant != "globex" {
		t.Fatalf("entries[1].Tenant = %q, want globex — loggers must not share captured tenant", entries[1].Tenant)
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The first test proves the whole point of the factory: every entry logged
through one logger carries the same tenant, regardless of how many different
actions are logged. The second test is the one that matters operationally —
two loggers built from the same `sink` (the same underlying stream or
database table) must never let one tenant's entries appear stamped with the
other tenant's ID, because each `NewLogger` call captures its own independent
`tenantID` with no shared mutable state between them at all.

## Resources

- [Go spec: Function literals](https://go.dev/ref/spec#Function_literals) — how the returned closure captures `tenantID` and `sink`.
- [pkg.go.dev: log/slog](https://pkg.go.dev/log/slog#Logger.With) — the production equivalent, `Logger.With("tenant", id)`, which returns a new logger carrying fixed attributes the same way this closure carries `tenantID`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-oauth-token-auto-refresh-guard.md](21-oauth-token-auto-refresh-guard.md) | Next: [23-distributed-lock-lease-renewal-gate.md](23-distributed-lock-lease-renewal-gate.md)
