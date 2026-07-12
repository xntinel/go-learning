# Exercise 27: Canary Deployment Feature Gate Evaluation via IIFE

**Nivel: Intermedio** — validacion rapida (un test corto).

A feature gate usually has to check several unrelated rules in sequence —
an explicit deny list, an explicit allow list, then a percentage rollout —
and the naive version litters `Enabled`'s own scope with scratch variables
each rule only needed for a moment: a loop index, a hash, a bucket number.
This module builds `Enabled` so each rule runs inside its own immediately
invoked function literal, returning its verdict and taking every scratch
variable it used down with it.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
canary/                       module example.com/canary
  go.mod
  canary.go                    Flag, Enabled (IIFE per rule: deny, allow, percentage)
  canary_test.go                 precedence, percentage edges, stable bucketing
  cmd/demo/main.go              deny/allow/percentage decisions for four users
```

- Files: `canary.go`, `canary_test.go`, `cmd/demo/main.go`.
- Implement: `Flag{Name, Percentage, AllowList, DenyList}`; `Enabled(flag, userID)` evaluating deny, then allow, then a percentage bucket hash, each inside its own IIFE.
- Test: deny list overrides allow list; allow list overrides the percentage bucket; `Percentage` 0 and 100 edges; bucketing is stable across repeated calls with the same input.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/05-anonymous-functions/27-canary-flag-iife/cmd/demo
cd go-solutions/04-functions/05-anonymous-functions/27-canary-flag-iife
go mod edit -go=1.24
```

### Each rule's scratch state dies with its IIFE

`Enabled` evaluates three rules in strict order: deny, allow, percentage.
Each one is written as `func() bool { ... }()` — called immediately, right
there, rather than stored and invoked later. The deny and allow rules each
need a loop variable to scan their list; the percentage rule needs an
`fnv` hash and a `bucket` number. None of those leak into `Enabled`'s own
scope, because they're declared inside a literal whose only job is to
return one `bool` and vanish. The `if denied := func() bool {...}(); denied`
pattern captures that return value in a temporary scoped to the `if`
itself — narrower even than `Enabled`'s scope — so by the time the next
rule runs, there is no earlier rule's scratch state left to read by
mistake or shadow.

Create `canary.go`:

```go
package canary

import "hash/fnv"

// Flag describes a canary feature gate: users on DenyList never get it,
// users on AllowList always get it, everyone else is bucketed by a stable
// hash of their ID into Percentage of the population.
type Flag struct {
	Name       string
	Percentage int // 0-100
	AllowList  []string
	DenyList   []string
}

// Enabled reports whether flag is active for userID. Each rule is
// evaluated inside its own immediately invoked function literal: the
// literal runs to a return the instant it decides, and every scratch
// variable it needed (a loop index, a hash, a bucket number) dies with it
// instead of leaking into Enabled's scope or bleeding into the next rule's
// evaluation.
func Enabled(flag Flag, userID string) bool {
	if denied := func() bool {
		for _, id := range flag.DenyList {
			if id == userID {
				return true
			}
		}
		return false
	}(); denied {
		return false
	}

	if allowed := func() bool {
		for _, id := range flag.AllowList {
			if id == userID {
				return true
			}
		}
		return false
	}(); allowed {
		return true
	}

	return func() bool {
		if flag.Percentage <= 0 {
			return false
		}
		if flag.Percentage >= 100 {
			return true
		}
		h := fnv.New32a()
		h.Write([]byte(flag.Name + ":" + userID))
		bucket := h.Sum32() % 100
		return bucket < uint32(flag.Percentage)
	}()
}
```

### The runnable demo

The demo checks four users against a flag with an allow list, a deny list,
and a 50% rollout.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/canary"
)

func main() {
	flag := canary.Flag{
		Name:       "new-checkout",
		Percentage: 50,
		AllowList:  []string{"vip-1"},
		DenyList:   []string{"banned-1"},
	}

	for _, user := range []string{"vip-1", "banned-1", "u2", "u1"} {
		fmt.Printf("%s -> %v\n", user, canary.Enabled(flag, user))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
vip-1 -> true
banned-1 -> false
u2 -> true
u1 -> false
```

### Tests

`TestEnabledDenyListWinsOverAllowList` puts a user on both lists and checks
deny wins because it is checked first. `TestEnabledAllowListWinsOverPercentage`
picks a user whose hash bucket would normally disable them at 50% and
checks the allow list overrides it. `TestEnabledPercentageEdges` checks 0
disables everyone and 100 enables everyone. `TestEnabledPercentageBucketingIsStableAndTableDriven`
runs a table of known buckets for the `"new-checkout"` flag and checks each
user's result twice, proving the IIFE recomputes deterministically instead
of leaking or caching stale state.

Create `canary_test.go`:

```go
package canary

import "testing"

func TestEnabledDenyListWinsOverAllowList(t *testing.T) {
	t.Parallel()
	flag := Flag{Name: "f", Percentage: 100, AllowList: []string{"u1"}, DenyList: []string{"u1"}}
	if Enabled(flag, "u1") {
		t.Fatal("user on both lists must be denied: deny is checked first")
	}
}

func TestEnabledAllowListWinsOverPercentage(t *testing.T) {
	t.Parallel()
	// u1 hashes into a bucket >= 50 for flag "new-checkout" (see table test
	// below), so at Percentage: 50 it would normally be disabled -- unless
	// the allow list overrides the bucket check.
	flag := Flag{Name: "new-checkout", Percentage: 50, AllowList: []string{"u1"}}
	if !Enabled(flag, "u1") {
		t.Fatal("allow-listed user must be enabled regardless of percentage bucket")
	}
}

func TestEnabledPercentageEdges(t *testing.T) {
	t.Parallel()
	zero := Flag{Name: "f", Percentage: 0}
	if Enabled(zero, "anyone") {
		t.Fatal("Percentage 0 must disable everyone not on the allow list")
	}
	full := Flag{Name: "f", Percentage: 100}
	if !Enabled(full, "anyone") {
		t.Fatal("Percentage 100 must enable everyone not on the deny list")
	}
}

func TestEnabledPercentageBucketingIsStableAndTableDriven(t *testing.T) {
	t.Parallel()
	flag := Flag{Name: "new-checkout", Percentage: 50}
	cases := []struct {
		user string
		want bool
	}{
		{"u2", true},    // bucket 33
		{"u3", true},    // bucket 14
		{"alice", true}, // bucket 2
		{"u1", false},   // bucket 76
		{"u4", false},   // bucket 71
		{"bob", false},  // bucket 93
	}
	for _, tc := range cases {
		if got := Enabled(flag, tc.user); got != tc.want {
			t.Errorf("Enabled(%q) = %v, want %v", tc.user, got, tc.want)
		}
		// Calling twice with the same input must yield the same result --
		// the IIFE recomputes the hash each time rather than leaking state.
		if got2 := Enabled(flag, tc.user); got2 != tc.want {
			t.Errorf("Enabled(%q) second call = %v, want %v (bucketing must be stable)", tc.user, got2, tc.want)
		}
	}
}
```

## Review

`Enabled` is correct when the three rules always apply in the same fixed
order — deny, then allow, then percentage — and no rule's internals ever
affect another's. IIFEs are what make that a structural guarantee instead
of a convention someone could violate by declaring a stray variable at the
top of the function: there is no `bucket` or `h` in scope for the allow
rule to accidentally read, because the percentage rule's IIFE hasn't run
yet and its variables don't exist outside it. The mistake this pattern
prevents is exactly the one that creeps in when rules are written as
sequential `if` blocks sharing `Enabled`'s top-level scope: a variable
named `ok` or `found` reused across two rules that happens to still hold
the previous rule's answer.

## Resources

- [Go Language Specification: Function literals](https://go.dev/ref/spec#Function_literals)
- [hash/fnv](https://pkg.go.dev/hash/fnv)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-metric-aggregator-callback.md](26-metric-aggregator-callback.md) | Next: [28-backpressure-errgroup.md](28-backpressure-errgroup.md)
