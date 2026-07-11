# Exercise 18: Check multiple rate-limit buckets and exit on first exhausted

**Nivel: Intermedio** — validacion rapida (un test corto).

A rate limiter in front of a public API enforces several independent
buckets at once: a global cap for the whole service, a per-user cap, and a
per-IP cap, checked cheapest-and-most-impactful first. A request must clear
every bucket to proceed, but the moment any one of them is already over its
limit, checking the rest is wasted CPU on a request that is getting rejected
regardless. This module is fully self-contained: its own `go mod init`, all
code inline, its own demo and tests.

## What you'll build

```text
ratelimit/                  independent module: example.com/ratelimit
  go.mod                     go 1.24
  ratelimit.go               Bucket, CheckLimiters
  cmd/
    demo/
      main.go                runnable demo: one request under limit, one over
  ratelimit_test.go           table test: no buckets, all under, first exhausts, later exhausts, boundary equal-to-limit
```

- Files: `ratelimit.go`, `cmd/demo/main.go`, `ratelimit_test.go`.
- Implement: `CheckLimiters(buckets []Bucket) (ok bool, limitedBy string)`, summing each bucket's recent window counts and stopping the instant any bucket's sum exceeds its limit.
- Test: no buckets configured, every bucket under its limit, the first bucket in priority order exhausting, a later bucket exhausting while earlier ones pass, and the exactly-at-limit boundary not counting as exceeded.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ratelimit/cmd/demo
cd ~/go-exercises/ratelimit
go mod init example.com/ratelimit
go mod edit -go=1.24
```

### Why the sum-and-check needs a labeled break

`CheckLimiters` sums each bucket's recent window counts incrementally rather
than all at once, so it can stop the instant the running total crosses the
limit instead of always summing every window first. That incremental check
sits inside the per-window loop, nested one level below the per-bucket loop.
A bare `break` there would only stop summing the CURRENT bucket's windows —
the outer loop would then move on and check the next bucket anyway, wasting
work on a request that is already rejected and, worse, potentially reporting
the wrong bucket as the limiting factor if a later bucket's check happened
to run first in the code's control flow. The labeled `break buckets` leaves
both loops together: it stops summing this bucket's remaining windows *and*
skips every bucket that would have been checked after it, since one
exhausted bucket is already a rejection.

Create `ratelimit.go`:

```go
package ratelimit

// Bucket is one rate-limit dimension (global, per-user, per-IP, ...). Counts
// holds the request count observed in each of the last few time windows
// (oldest first); Limit is the maximum total allowed across those windows.
type Bucket struct {
	Name   string
	Counts []int
	Limit  int
}

// CheckLimiters checks buckets in the given order — global, then per-user,
// then per-IP is the typical order, cheapest and most impactful checks
// first. For each bucket it sums the recent window counts, and the instant
// that running sum exceeds the bucket's limit, the request is rejected by
// THIS bucket: there is no reason to keep summing its remaining windows, and
// no reason to check any bucket after it, since one rejection is enough. A
// labeled break on the buckets loop, fired from inside the per-window
// summing loop, stops the whole check in one statement and reports which
// bucket was the limiting factor.
func CheckLimiters(buckets []Bucket) (ok bool, limitedBy string) {
buckets:
	for _, b := range buckets {
		total := 0
		for _, c := range b.Counts {
			total += c
			if total > b.Limit {
				limitedBy = b.Name
				break buckets
			}
		}
	}
	return limitedBy == "", limitedBy
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/ratelimit"
)

func main() {
	underLimit := []ratelimit.Bucket{
		{Name: "global", Counts: []int{100, 120, 90}, Limit: 1000},
		{Name: "user:42", Counts: []int{2, 3, 1}, Limit: 20},
		{Name: "ip:10.0.0.1", Counts: []int{5, 5}, Limit: 50},
	}
	ok, limitedBy := ratelimit.CheckLimiters(underLimit)
	fmt.Println("under limit:", ok, limitedBy)

	overLimit := []ratelimit.Bucket{
		{Name: "global", Counts: []int{100, 120, 90}, Limit: 1000},
		{Name: "user:42", Counts: []int{15, 8}, Limit: 20},
		{Name: "ip:10.0.0.1", Counts: []int{5, 5}, Limit: 50},
	}
	ok, limitedBy = ratelimit.CheckLimiters(overLimit)
	fmt.Println("over limit: ", ok, limitedBy)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
under limit: true 
over limit:  false user:42
```

In the second request, the global and IP buckets are both fine, but the
per-user bucket's two most recent windows sum to 23 against a limit of 20 —
so `user:42` is reported as the limiting factor, and the IP bucket after it
is never even summed.

### Tests

`TestCheckLimiters` covers no buckets at all, every bucket comfortably under
its limit, the first bucket in priority order being the one that exhausts,
a later bucket exhausting while the ones before it pass cleanly, exceeding
on the very last window summed, and the boundary case where the sum equals
the limit exactly (not over it).

Create `ratelimit_test.go`:

```go
package ratelimit

import "testing"

func TestCheckLimiters(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		buckets       []Bucket
		wantOK        bool
		wantLimitedBy string
	}{
		"no buckets configured": {
			buckets: nil,
			wantOK:  true,
		},
		"every bucket under its limit": {
			buckets: []Bucket{
				{Name: "global", Counts: []int{10, 10}, Limit: 100},
				{Name: "user", Counts: []int{1, 1}, Limit: 10},
			},
			wantOK: true,
		},
		"the first bucket in order exhausts first": {
			buckets: []Bucket{
				{Name: "global", Counts: []int{500, 600}, Limit: 1000},
				{Name: "user", Counts: []int{1}, Limit: 10},
			},
			wantOK:        false,
			wantLimitedBy: "global",
		},
		"a later bucket is the limiting factor when earlier ones pass": {
			buckets: []Bucket{
				{Name: "global", Counts: []int{10}, Limit: 1000},
				{Name: "user", Counts: []int{15, 8}, Limit: 20},
				{Name: "ip", Counts: []int{1}, Limit: 5},
			},
			wantOK:        false,
			wantLimitedBy: "user",
		},
		"exceeding on the last window of the sum still counts": {
			buckets: []Bucket{
				{Name: "ip", Counts: []int{1, 1, 1, 1, 1, 1}, Limit: 5},
			},
			wantOK:        false,
			wantLimitedBy: "ip",
		},
		"exactly at the limit is not exceeding it": {
			buckets: []Bucket{
				{Name: "ip", Counts: []int{2, 3}, Limit: 5},
			},
			wantOK: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ok, limitedBy := CheckLimiters(tc.buckets)
			if ok != tc.wantOK || limitedBy != tc.wantLimitedBy {
				t.Fatalf("CheckLimiters = (%v, %q), want (%v, %q)", ok, limitedBy, tc.wantOK, tc.wantLimitedBy)
			}
		})
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

The limiter is correct when it stops at the *first* bucket whose sum
exceeds its limit and never touches the buckets after it — the "a later
bucket is the limiting factor" test proves the earlier bucket really was
checked and passed, not skipped. The bug this exercise guards against is a
bare `break` inside the per-window summing loop: it would leave only that
loop, and the outer loop would proceed to check the next bucket anyway,
which is harmless for correctness here but wastes work on every rejected
request and would misattribute the limiting bucket if a later one happened
to compute a smaller (still over-limit) value. The exactly-at-limit test
pins the boundary: `total > limit` rejects, `total == limit` does not.

## Resources

- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — a labeled `break` can leave any number of enclosing loops at once.
- [Token bucket algorithm (Wikipedia)](https://en.wikipedia.org/wiki/Token_bucket) — background on the rate-limiting shape this bucket check enforces.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-outbox-event-batcher.md](17-outbox-event-batcher.md) | Next: [19-event-stream-corruption-detector.md](19-event-stream-corruption-detector.md)
