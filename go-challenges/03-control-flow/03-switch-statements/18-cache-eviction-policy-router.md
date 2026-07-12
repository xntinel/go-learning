# Exercise 18: Route Cache Eviction Policies With Multi-Value Cases

**Nivel: Intermedio** — validacion rapida (un test corto).

An in-memory cache layer rarely gets to pick one eviction strategy for
life: under light memory pressure a cheap LRU is enough, but once the pool
is nearly full and the workload is read-heavy, tracking genuine frequency
(LFU) beats recency, and a write-heavy workload under pressure is better
served by simply expiring on age (TTL) than by paying for either's
bookkeeping. This module builds that policy selector as an expression
switch over a composed tag, using comma-separated cases to group the
combinations that share a strategy. It is self-contained: its own
`go mod init`, code, demo, and test.

## What you'll build

```text
evictpolicy/                independent module: example.com/cache-eviction-policy-router
  go.mod                     go 1.24
  evictpolicy.go              package evictpolicy; SelectPolicy(memUsage, hitRate, workload) string
  cmd/demo/main.go            runnable demo over six representative combinations
  evictpolicy_test.go         table over low/high pressure, both hit-rate outcomes, and an unknown workload
```

- Implement: `SelectPolicy(memUsage, hitRate float64, workload string) string` — bucket memory usage into a tier, compose it with the workload hint into one tag, and dispatch with an expression switch and comma-separated cases; consult hit rate only inside the one branch where it changes the answer.
- Test: a table covering every low-pressure workload (all collapse to one case), the high-pressure write-heavy case, both high-pressure read/mixed outcomes split by hit rate, the exact tier boundary, and an unrecognized workload hint.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why a composed tag, not three separate switches

`SelectPolicy` genuinely has three inputs, but only two of them — the memory
tier and the workload hint — determine *which case applies*; the third,
hit rate, only matters inside a single case where the answer is otherwise
ambiguous. Building `key := memoryTier(memUsage) + ":" + workload` collapses
the first two axes into one string an expression switch can compare with
`==`, which is exactly what comma-separated cases are for:
`case "low:read-heavy", "low:mixed", "low:write-heavy":` says "any workload,
as long as memory isn't under pressure, gets the same cheap answer" in one
line, instead of three near-duplicate cases or a nested `if workload ==
"read-heavy" || workload == "mixed" || ...` buried inside a memory-tier
switch. Hit rate only enters the picture in the `"high:read-heavy",
"high:mixed"` case, and it is deliberately an `if` *inside* that case body
rather than a third axis folded into the tag — folding it in would have
multiplied the case list by every hit-rate bucket, most of which would
never differ from their neighbors.

The two-tier `memoryTier` helper is itself a tiny tagless switch, kept
separate from the main dispatch: it answers one question (is memory
pressure high or not) so that `SelectPolicy`'s switch only has to reason
about the composed tag, not raw floats.

Create `evictpolicy.go`:

```go
// Package evictpolicy selects a cache eviction strategy (LRU, LFU, or
// TTL-based) from memory pressure, hit rate, and a workload hint, using an
// expression switch over a composed tag with comma-separated cases to
// group memory/workload combinations that share a strategy.
package evictpolicy

// SelectPolicy chooses an eviction policy name: "lru", "lfu", or "ttl".
// The memory tier and workload hint are composed into a single tag so the
// dominant decision is a plain expression switch; hit rate is only
// consulted inside the one branch where it actually changes the answer.
func SelectPolicy(memUsage, hitRate float64, workload string) string {
	key := memoryTier(memUsage) + ":" + workload

	switch key {
	case "low:read-heavy", "low:mixed", "low:write-heavy":
		return "lru" // memory isn't under pressure; LRU's O(1) bookkeeping is cheap enough for any workload
	case "high:write-heavy":
		return "ttl" // under pressure and writes dominate; tracking recency/frequency on doomed keys wastes cycles
	case "high:read-heavy", "high:mixed":
		if hitRate < 0.4 {
			return "lfu" // under pressure, and recency isn't finding the popular set; track frequency directly
		}
		return "lru" // under pressure but recency already captures the hot set well
	default:
		return "ttl" // unrecognized workload hint: fail safe to time-based expiry, not to unlimited retention
	}
}

// memoryTier buckets raw utilization into the two tiers the policy switch
// above dispatches on.
func memoryTier(usage float64) string {
	switch {
	case usage >= 0.85:
		return "high"
	default:
		return "low"
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	evictpolicy "example.com/cache-eviction-policy-router"
)

func main() {
	cases := []struct {
		memUsage, hitRate float64
		workload          string
	}{
		{0.40, 0.90, "read-heavy"},
		{0.40, 0.10, "write-heavy"},
		{0.92, 0.90, "write-heavy"},
		{0.92, 0.20, "read-heavy"},
		{0.92, 0.80, "mixed"},
		{0.30, 0.50, "bulk-import"},
	}

	for _, c := range cases {
		policy := evictpolicy.SelectPolicy(c.memUsage, c.hitRate, c.workload)
		fmt.Printf("mem=%.2f hit=%.2f workload=%-12s -> %s\n", c.memUsage, c.hitRate, c.workload, policy)
	}
}
```

Run `go run ./cmd/demo`, expected output:

```
mem=0.40 hit=0.90 workload=read-heavy   -> lru
mem=0.40 hit=0.10 workload=write-heavy  -> lru
mem=0.92 hit=0.90 workload=write-heavy  -> ttl
mem=0.92 hit=0.20 workload=read-heavy   -> lfu
mem=0.92 hit=0.80 workload=mixed        -> lru
mem=0.30 hit=0.50 workload=bulk-import  -> ttl
```

### Tests

`TestSelectPolicy` runs a table over every low-pressure workload (all three
collapse to the same comma case, so the table proves it), the high-pressure
write-heavy case, both hit-rate outcomes of the high-pressure
read/mixed case, the exact tier boundary at 0.85, and an unrecognized
workload hint that must fail safe to `"ttl"`.

Create `evictpolicy_test.go`:

```go
package evictpolicy

import "testing"

func TestSelectPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		memUsage, hitRate float64
		workload          string
		want              string
	}{
		{"low pressure read-heavy", 0.40, 0.90, "read-heavy", "lru"},
		{"low pressure write-heavy", 0.40, 0.10, "write-heavy", "lru"},
		{"low pressure mixed", 0.50, 0.05, "mixed", "lru"},
		{"high pressure write-heavy", 0.92, 0.90, "write-heavy", "ttl"},
		{"high pressure read-heavy cold", 0.92, 0.20, "read-heavy", "lfu"},
		{"high pressure read-heavy hot", 0.92, 0.80, "read-heavy", "lru"},
		{"high pressure mixed cold", 0.90, 0.10, "mixed", "lfu"},
		{"boundary at exactly high threshold", 0.85, 0.10, "read-heavy", "lfu"},
		{"unknown workload", 0.30, 0.50, "bulk-import", "ttl"},
	}

	for _, tc := range tests {
		if got := SelectPolicy(tc.memUsage, tc.hitRate, tc.workload); got != tc.want {
			t.Errorf("%s: SelectPolicy(%v, %v, %q) = %q, want %q", tc.name, tc.memUsage, tc.hitRate, tc.workload, got, tc.want)
		}
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The selector is correct when every low-pressure workload gets the cheap
answer through one shared case, when hit rate only changes the outcome in
the one place it should, and when a workload hint nobody anticipated fails
safe to the simplest strategy (TTL) instead of picking something that
assumes a shape the caller never promised. Carry this forward: when a
dispatch decision genuinely depends on two axes but a third only matters
inside one branch, compose the two into a single tag for the switch and
keep the third as an `if` inside the case body — don't multiply the case
list just to make every input a first-class part of the tag.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — expression switch with comma-separated case lists.
- [Wikipedia: Cache replacement policies](https://en.wikipedia.org/wiki/Cache_replacement_policies) — LRU, LFU, and TTL-based eviction compared.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-connection-pool-health-classifier.md](17-connection-pool-health-classifier.md) | Next: [19-binary-frame-type-demultiplexer.md](19-binary-frame-type-demultiplexer.md)
