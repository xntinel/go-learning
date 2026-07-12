# Exercise 28: Gradual Feature Rollout Routing Users to Variants

**Nivel: Intermedio** — validacion rapida (un test corto).

A canary rollout needs the same user to land in the same variant on every
request — flip-flopping between the old and new checkout flow mid-session
is a broken experience — but it must not pay for a database lookup or a
sticky session just to remember which bucket a user is in. `NewRouter`
closes over a compiled set of cumulative percentage ranges; the returned
closure hashes the user ID into a stable bucket and looks up which range it
falls in. No map of user-to-variant is ever stored: the hash *is* the
assignment, recomputed identically every call.

## What you'll build

```text
feature-rollout/             independent module: example.com/feature-rollout
  go.mod                      go 1.24
  rollout.go                  NewRouter returns func(userID string) string
  cmd/
    demo/
      main.go                  four users routed, repeat calls prove stability
  rollout_test.go              table test: known assignments, stability, bad config
```

- Files: `rollout.go`, `cmd/demo/main.go`, `rollout_test.go`.
- Implement: `NewRouter(percentages map[string]int, order []string) (func(userID string) string, error)`, closing over a precomputed slice of cumulative percentage boundaries.
- Test: a table of known user IDs asserts the exact variant their FNV-1a hash bucket falls into; a stability test calls the router repeatedly per user and requires the same answer every time; a table of malformed configs (percentages not summing to 100, a variant missing from the map) must error out of `NewRouter` instead of misrouting at request time.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Hashing replaces stored per-user state

`NewRouter` does its validation and arithmetic once, at construction time:
it walks `order` accumulating each variant's percentage into a cumulative
boundary slice (`[70, 100]` for a 70/30 split named `[control,
new-checkout]`), and rejects a config whose percentages do not sum to
exactly 100 or that references a variant missing from the map — a
misconfigured rollout should fail loudly at startup, not silently misroute
every request in production. The returned closure captures only that small
boundary slice and the `order` names; per call it hashes `userID` with
FNV-1a into `[0, 100)` and walks the boundaries to find the first one the
bucket falls under.

Because the bucket is a pure function of the user ID, calling the router
twice for the same user always returns the same variant — no session
affinity, no cache, no per-user row anywhere — and because the boundaries
are cumulative, growing one variant's percentage only shifts users at the
*edges* of the ranges, never reassigns everyone. This is the same
deterministic-hash-bucket technique load balancers and sharding schemes use
to route without shared state.

Create `rollout.go`:

```go
// Package rollout assigns each user deterministically to one variant of a
// feature, according to configured rollout percentages, with no per-user
// state stored anywhere.
package rollout

import (
	"fmt"
	"hash/fnv"
)

// NewRouter returns a closure that assigns each userID to one of the
// variants named in order, using rollout percentages that must sum to
// exactly 100. The router hashes userID with FNV-1a into a bucket in
// [0, 100) and walks the cumulative percentage ranges in the given order, so
// the same userID always resolves to the same variant -- no database row,
// no map entry, no per-user state anywhere -- and growing a variant's
// percentage only ever moves users from later ranges into it, never
// reassigns users already inside an earlier range.
func NewRouter(percentages map[string]int, order []string) (func(userID string) string, error) {
	if len(order) == 0 {
		return nil, fmt.Errorf("rollout: order must list at least one variant")
	}

	total := 0
	cumulative := make([]int, len(order))
	for i, name := range order {
		pct, ok := percentages[name]
		if !ok {
			return nil, fmt.Errorf("rollout: variant %q missing from percentages", name)
		}
		if pct < 0 {
			return nil, fmt.Errorf("rollout: variant %q has negative percentage %d", name, pct)
		}
		total += pct
		cumulative[i] = total
	}
	if total != 100 {
		return nil, fmt.Errorf("rollout: percentages sum to %d, want 100", total)
	}

	return func(userID string) string {
		h := fnv.New32a()
		h.Write([]byte(userID))
		bucket := int(h.Sum32() % 100)
		for i, boundary := range cumulative {
			if bucket < boundary {
				return order[i]
			}
		}
		return order[len(order)-1]
	}, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/feature-rollout"
)

func main() {
	variant, err := rollout.NewRouter(
		map[string]int{"control": 70, "new-checkout": 30},
		[]string{"control", "new-checkout"},
	)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	users := []string{"user-1", "user-2", "user-6", "dave"}
	for _, u := range users {
		v1 := variant(u)
		v2 := variant(u) // same user, called again: must match v1
		fmt.Printf("%s -> %s (stable: %v)\n", u, v1, v1 == v2)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user-1 -> control (stable: true)
user-2 -> control (stable: true)
user-6 -> new-checkout (stable: true)
dave -> new-checkout (stable: true)
```

### Tests

Create `rollout_test.go`:

```go
package rollout

import "testing"

func TestRouterAssignsKnownUsersDeterministically(t *testing.T) {
	variant, err := NewRouter(
		map[string]int{"control": 70, "new-checkout": 30},
		[]string{"control", "new-checkout"},
	)
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	// user-1 hashes to bucket 0, user-2 to 57 (both < 70: control);
	// user-6 hashes to bucket 81, "dave" to 71 (both >= 70: new-checkout).
	tests := []struct {
		userID string
		want   string
	}{
		{"user-1", "control"},
		{"user-2", "control"},
		{"user-6", "new-checkout"},
		{"dave", "new-checkout"},
	}

	for _, tc := range tests {
		if got := variant(tc.userID); got != tc.want {
			t.Fatalf("variant(%q) = %q, want %q", tc.userID, got, tc.want)
		}
	}
}

func TestRouterIsStablePerUser(t *testing.T) {
	variant, err := NewRouter(
		map[string]int{"a": 50, "b": 50},
		[]string{"a", "b"},
	)
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	for _, id := range []string{"stable-1", "stable-2", "stable-3"} {
		first := variant(id)
		for range 10 {
			if got := variant(id); got != first {
				t.Fatalf("variant(%q) = %q on repeat call, want stable %q (router keeps no per-user state)", id, got, first)
			}
		}
	}
}

func TestNewRouterRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name        string
		percentages map[string]int
		order       []string
	}{
		{"empty order", map[string]int{}, nil},
		{"percentages sum to less than 100", map[string]int{"a": 40, "b": 40}, []string{"a", "b"}},
		{"percentages sum to more than 100", map[string]int{"a": 60, "b": 60}, []string{"a", "b"}},
		{"variant missing from percentages", map[string]int{"a": 100}, []string{"a", "b"}},
	}

	for _, tc := range tests {
		if _, err := NewRouter(tc.percentages, tc.order); err == nil {
			t.Fatalf("%s: NewRouter() error = nil, want error", tc.name)
		}
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The known-users table pins down the exact bucket arithmetic against real
FNV-1a hash values, so a future refactor that changes the hash function or
the boundary walk would be caught immediately. The stability test is the
router's whole reason to exist: the same user ID must never see a different
variant across calls, since the router keeps no per-user state to make that
guarantee any other way. The bad-config table pushes every misconfiguration
to fail at `NewRouter` construction time — loudly, at startup — rather than
silently misrouting live traffic.

## Resources

- [pkg.go.dev: hash/fnv](https://pkg.go.dev/hash/fnv) — the FNV-1a hash used to bucket user IDs deterministically.
- [Martin Fowler: Canary Release](https://martinfowler.com/bliki/CanaryRelease.html) — the gradual-rollout pattern this router implements.
- [Go spec: Function types](https://go.dev/ref/spec#Function_types) — the `func(userID string) string` closure type the factory returns.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-encryption-key-versioning-wrapper.md](27-encryption-key-versioning-wrapper.md) | Next: [29-write-ahead-log-sequential-replay.md](29-write-ahead-log-sequential-replay.md)
