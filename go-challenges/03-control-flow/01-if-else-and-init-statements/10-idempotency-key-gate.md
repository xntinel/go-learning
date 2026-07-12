# Exercise 10: Idempotency Key Gate: Comma-Ok Replay for a Payments Endpoint

**Nivel: Intermedio** — validacion rapida (un test corto).

A payments endpoint must not charge a customer twice because a client retried a
POST after a lost response. The fix is one `if` with an init statement: look up
the idempotency key with the comma-ok idiom, and if it is already there,
return the cached result instead of running the charge again.

This module is fully self-contained: its own `go mod init`, code, demo, and test.

## What you'll build

```text
idem/                       independent module: example.com/idempotency-key-gate
  go.mod                    go 1.24
  idem.go                   type Result; Process(store, key, charge) (Result, bool)
  cmd/
    demo/
      main.go               same key called twice, second call replays
  idem_test.go              table: first call executes, replay skips charge, new key executes
```

- Files: `idem.go`, `cmd/demo/main.go`, `idem_test.go`.
- Implement: `Process(store map[string]Result, key string, charge func() Result) (Result, bool)` using `if prev, ok := store[key]; ok { ... }` for the cached-response early return.
- Test: a table over three cases — first call for a key executes `charge` and stores the result; a repeat call with the same key replays the cached result without invoking `charge` again; a different key executes independently.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why comma-ok, not a `nil` check

`store[key]` on a miss returns the zero `Result`, a perfectly valid zero-value
struct — you cannot tell "key absent" from "key present with a zero-value
result" from the returned value alone. The comma-ok form `prev, ok :=
store[key]` asks the map directly: `ok` is the map's own answer to "was this
key ever written," decoupled from what was written. That is exactly the
question a replay guard needs, and it is why the init statement belongs on
the `if` itself — `prev` only means anything inside the branch where `ok` is
true, so scoping it to that branch is correct, not just tidy.

The shape is a guard clause: on a hit, return immediately; the "miss" logic
falls through below the `if`, at the outer level, exactly once. `store` is a
plain map with no mutex and no expiry on purpose — a production gate needs a
lock (or `sync.Map`) plus a TTL, but this exercise isolates the comma-ok idiom.

Create `idem.go`:

```go
// Package idem gates a payment side effect behind an idempotency key.
package idem

// Result is the outcome of a charge, cached under an idempotency key so a
// retried request replays it instead of charging again.
type Result struct {
	Status string
	Amount int
}

// Process returns the Result for key, running charge only on a cache miss.
// The second return value reports whether the result was a replay (true) or
// a fresh execution (false). store is a plain map: single-goroutine only, no
// TTL — a production gate needs a lock (or sync.Map) and expiry.
func Process(store map[string]Result, key string, charge func() Result) (Result, bool) {
	if prev, ok := store[key]; ok {
		return prev, true
	}

	result := charge()
	store[key] = result
	return result, false
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/idempotency-key-gate"
)

func main() {
	store := make(map[string]idem.Result)
	charges := 0

	charge := func() idem.Result {
		charges++
		return idem.Result{Status: "captured", Amount: 4200}
	}

	first, replay := idem.Process(store, "req-abc", charge)
	fmt.Printf("first:  %+v replay=%v charges=%d\n", first, replay, charges)

	second, replay := idem.Process(store, "req-abc", charge)
	fmt.Printf("second: %+v replay=%v charges=%d\n", second, replay, charges)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first:  {Status:captured Amount:4200} replay=false charges=1
second: {Status:captured Amount:4200} replay=true charges=1
```

`charges` stays at 1 after the second call — proof the guard, not the caller,
decided not to charge again.

### Tests

The table drives `Process` three times against one shared `store`, each case
wrapping its own `charge` closure that counts invocations per key. The replay
case's closure returns a *different* amount; the test fails if it ever runs,
since the count would climb and the replayed result would silently change.

Create `idem_test.go`:

```go
package idem

import "testing"

func TestProcess(t *testing.T) {
	t.Parallel()

	store := make(map[string]Result)
	invocations := make(map[string]int)

	charge := func(key string, status string, amount int) func() Result {
		return func() Result {
			invocations[key]++
			return Result{Status: status, Amount: amount}
		}
	}

	tests := []struct {
		name        string
		key         string
		charge      func() Result
		wantResult  Result
		wantReplay  bool
		wantInvokes int
	}{
		{
			name:        "first call for a new key executes the charge",
			key:         "order-1",
			charge:      charge("order-1", "captured", 1000),
			wantResult:  Result{Status: "captured", Amount: 1000},
			wantReplay:  false,
			wantInvokes: 1,
		},
		{
			name:        "second call with the same key replays the cached result",
			key:         "order-1",
			charge:      charge("order-1", "captured", 9999), // must NOT run: different amount would leak in
			wantResult:  Result{Status: "captured", Amount: 1000},
			wantReplay:  true,
			wantInvokes: 1, // unchanged from the first call
		},
		{
			name:        "a different key executes independently",
			key:         "order-2",
			charge:      charge("order-2", "captured", 2500),
			wantResult:  Result{Status: "captured", Amount: 2500},
			wantReplay:  false,
			wantInvokes: 1,
		},
	}

	for _, tc := range tests {
		got, replay := Process(store, tc.key, tc.charge)
		if got != tc.wantResult || replay != tc.wantReplay {
			t.Errorf("%s: Process(%q) = %+v, replay=%v, want %+v, replay=%v",
				tc.name, tc.key, got, replay, tc.wantResult, tc.wantReplay)
		}
		if invocations[tc.key] != tc.wantInvokes {
			t.Errorf("%s: invocations[%q] = %d, want %d",
				tc.name, tc.key, invocations[tc.key], tc.wantInvokes)
		}
	}
}
```

## Review

The gate is correct when a repeated key never re-runs `charge`, which the
test proves by making the would-be-second charge distinguishable and counting
invocations directly rather than only checking the returned value. Carry this
forward: comma-ok inside an `if`'s init statement is the idiomatic way to ask
a map "was this key ever written," useful whenever "have I seen this before"
must be a different question from "what value is stored here."

## Resources

- [Go Specification: Index expressions](https://go.dev/ref/spec#Index_expressions) — the two-result form of a map index and what `ok` means.
- [Go Specification: If statements](https://go.dev/ref/spec#If_statements) — the init-statement form and its scoping.
- [Stripe API docs: Idempotent requests](https://stripe.com/docs/api/idempotent_requests) — the production shape of this pattern.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-readiness-health-aggregator.md](09-readiness-health-aggregator.md) | Next: [11-feature-flag-rollout-gate.md](11-feature-flag-rollout-gate.md)
