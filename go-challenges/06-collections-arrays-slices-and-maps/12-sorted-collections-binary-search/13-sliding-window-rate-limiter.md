# Exercise 13: Sliding-Window Rate Limiter (Sorted Timestamp Trim)

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every API gateway that enforces "at most N requests per client per minute" is
running some form of the sliding-window-log algorithm: keep a sorted list of
the timestamps a client's recent requests arrived at, discard the ones that
have aged out of the window, and allow the new request only if what remains
is under the limit. Redis implementations do this with `ZADD` plus
`ZREMRANGEBYSCORE`; Stripe and GitHub's documented throttling behavior is
this algorithm's black-box description. The data structure underneath is
exactly this lesson's subject: a sorted slice, trimmed at a moving boundary
on every operation.

The boundary is a half-open interval — `(now-window, now]` — and finding
where it starts is `sort.Search`'s textbook use: the predicate `log[i] >
cutoff` is monotone, false for the stale prefix and true for everything
still inside the window, so the search returns exactly the index to cut at.
Where a real limiter gets quietly wrong is not the search; it is the trim.
`slices.Delete(log, 0, idx)` returns a new slice header with `idx` fewer
elements, and if that header is not reassigned back to the limiter's stored
slice, the length never actually decreases. The bug does not crash and does
not show up in a load test with a handful of requests — it shows up hours
into production traffic, when the window has silently grown to contain
every request the service has ever seen and the limiter starts denying
everything.

This module builds `ratelimit`, a command that reads a stream of
already-ordered request timestamps and prints `ALLOW` or `DENY` for each
one, backed by a `Limiter` type whose `Allow` method gets the trim-and-
reassign step right. The unbounded-growth mistake is not part of that type;
it lives only in the tests, where it belongs, as the thing the tests prove
wrong.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
ratelimit/                    module example.com/ratelimit
  go.mod                      go 1.24
  ratelimit.go                package main — Limiter; NewLimiter, Allow, Log;
                              three sentinel errors
  ratelimit_test.go           package main — window-sliding table, out-of-order
                              guard, aliasing, the unbounded-growth contrast,
                              run() end to end
  main.go                     package main — -limit/-window flags, exit codes
```

- Files: `ratelimit.go`, `ratelimit_test.go`, `main.go`.
- Implement: `NewLimiter(limit int, window int64) (*Limiter, error)` rejecting a non-positive limit or window with `ErrInvalidLimit`/`ErrInvalidWindow`; `(*Limiter).Allow(now int64) (bool, error)` evicting stale timestamps with `sort.Search` plus a reassigned `slices.Delete`, then admitting the request if the trimmed log is under the limit, and rejecting an out-of-order `now` with `ErrOutOfOrder`; `(*Limiter).Log() []int64` returning a clone of the timestamps currently inside the window.
- Tool: `ratelimit -limit N -window W` reads newline-delimited integer timestamps from stdin, one per line, and writes `ALLOW` or `DENY` per line to stdout. Blank lines are skipped. Exit 0 on success; exit 2 for a bad flag, a non-positive `-limit`/`-window`, a non-integer line, or an out-of-order timestamp (all usage errors); exit 1 for a stdin read failure, the one runtime failure this tool can hit.
- Test: the sliding-window sequence through six requests including a stale-entry eviction, the four constructor rejections, an out-of-order `Allow` call, `Log` never aliasing internal state, an `evictBuggy` contrast proving the discarded-result mistake grows the log without bound, `run` end to end over valid and invalid input, and a simulated stdin I/O failure mapping to exit 1.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### `sort.Search` finds the cut; reassigning `slices.Delete`'s result performs it

`Allow` evicts every timestamp that fell out of the trailing window before it
decides whether to admit the new request:

```go
cutoff := now - l.window
idx := sort.Search(len(l.log), func(i int) bool { return l.log[i] > cutoff })
l.log = slices.Delete(l.log, 0, idx)
```

`sort.Search` returns the first index whose timestamp is still inside
`(cutoff, now]` — the predicate is false over the stale prefix and true over
the rest, exactly the monotone shape `sort.Search` requires. `slices.Delete`
then removes indices `[0, idx)`. The critical detail is the assignment:
`slices.Delete` does not shrink `l.log` in place through the caller's old
slice header, it returns a new one with the smaller length, and that
returned header has to overwrite `l.log` for the shrink to exist at all. The
version that looks almost identical and ships anyway is this:

```go
idx := sort.Search(len(l.log), func(i int) bool { return l.log[i] > cutoff })
slices.Delete(l.log, 0, idx)   // return value discarded: l.log is unchanged
```

`go vet` actually catches this exact shape — `slices.Delete`'s result is
one of the calls the standard `unusedresult` check flags — which is worth
knowing precisely because it means this bug more often survives as
`l.log = append(l.log[:0], slices.Delete(l.log, 0, idx)...)`-style near
misses or a trim step that got refactored into a helper whose return value
the caller forgot to use, not as the bare statement above. Whatever the
shape, the effect is the same: `len(l.log)` never decreases, every request
ever seen counts against the limit forever, and the limiter that looked
perfectly correct in a five-request test starts denying every request in
week two of production.

Create `ratelimit.go`:

```go
// Command ratelimit implements the sliding-window-log rate-limit algorithm
// behind Redis ZREMRANGEBYSCORE-based limiters and Stripe/GitHub-style API
// throttling: a sorted log of recent request timestamps per key, trimmed to
// the current window on every request.
//
// This file holds the domain logic; main.go wires it to flags and stdio.
package main

import (
	"errors"
	"fmt"
	"slices"
	"sort"
)

// Sentinel errors returned by NewLimiter and Allow. Callers should test for
// them with errors.Is rather than by comparing error strings.
var (
	// ErrInvalidLimit means the configured limit was not positive.
	ErrInvalidLimit = errors.New("ratelimit: limit must be positive")
	// ErrInvalidWindow means the configured window was not positive.
	ErrInvalidWindow = errors.New("ratelimit: window must be positive")
	// ErrOutOfOrder means Allow was called with a timestamp earlier than one
	// already seen, violating the caller's obligation to feed timestamps in
	// non-decreasing order.
	ErrOutOfOrder = errors.New("ratelimit: timestamps must be non-decreasing")
)

// Limiter enforces "at most limit requests in any trailing window-second
// interval" for one key, using a sorted, in-memory log of the timestamps of
// requests currently inside the window.
//
// A Limiter is not safe for concurrent use: Allow mutates the internal log
// on every call. Callers that shard by key should give each key its own
// Limiter and confine each one to a single goroutine, or guard it with a
// mutex.
type Limiter struct {
	limit    int
	window   int64
	log      []int64
	lastSeen int64
	hasSeen  bool
}

// NewLimiter returns a Limiter that allows at most limit requests inside any
// trailing window-second interval. It returns ErrInvalidLimit or
// ErrInvalidWindow if either argument is not positive.
func NewLimiter(limit int, window int64) (*Limiter, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidLimit, limit)
	}
	if window <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidWindow, window)
	}
	return &Limiter{limit: limit, window: window}, nil
}

// Allow decides whether a request arriving at time now (Unix seconds, or
// any consistent monotonic unit) is allowed. It first evicts every logged
// timestamp that has aged out of the trailing window, then admits the
// request if fewer than limit timestamps remain.
//
// now must be greater than or equal to every timestamp given to a previous
// call; Allow returns ErrOutOfOrder otherwise, since an out-of-order
// timestamp would corrupt the sorted invariant the eviction search depends
// on.
func (l *Limiter) Allow(now int64) (bool, error) {
	if l.hasSeen && now < l.lastSeen {
		return false, fmt.Errorf("%w: got %d after %d", ErrOutOfOrder, now, l.lastSeen)
	}
	l.lastSeen = now
	l.hasSeen = true

	// Evict every entry that fell out of the trailing window (now-window, now].
	// sort.Search finds the first index still inside the window; everything
	// before it is stale and slices.Delete removes it, and the result is
	// reassigned back into l.log so the shrink actually takes effect.
	cutoff := now - l.window
	idx := sort.Search(len(l.log), func(i int) bool { return l.log[i] > cutoff })
	l.log = slices.Delete(l.log, 0, idx)

	if len(l.log) < l.limit {
		l.log = append(l.log, now)
		return true, nil
	}
	return false, nil
}

// Log returns a clone of the timestamps currently counted inside the
// window. The returned slice never aliases the Limiter's internal storage,
// so the caller may retain or mutate it freely.
func (l *Limiter) Log() []int64 {
	return slices.Clone(l.log)
}
```

### The tool

`ratelimit` streams: it reads one timestamp per line with `bufio.Scanner`
rather than loading the whole input into memory, which matters for exactly
the workload this module is about — a request log can run far longer than
the bounded window `Limiter` actually needs to hold. `run` takes the
argument slice and an `io.Reader`/`io.Writer` pair instead of touching
`os.Stdin`/`os.Stdout` directly, so a test can drive it with a
`strings.Reader` and a `bytes.Buffer` without a real process. Every failure
that traces back to something the caller can fix by changing the command
line or the input — a bad flag, a non-positive `-limit` or `-window`, a line
that is not an integer, an out-of-order timestamp — wraps the `errUsage`
sentinel, and `main` maps that to exit code 2. A `bufio.Scanner` read error
on `stdin` is different in kind: no flag or input line caused it, so it is
reported as a plain error and `main` maps it to exit code 1.

Create `main.go`:

```go
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// errUsage marks a failure the caller can fix by changing the command line
// or the input stream: a bad flag, an invalid -limit/-window, a line that is
// not an integer timestamp, or an out-of-order timestamp. main maps it to
// exit code 2. Any other error -- an I/O failure reading stdin -- maps to
// exit code 1.
var errUsage = errors.New("usage")

// run reads newline-delimited, already time-ordered integer timestamps from
// stdin and writes "ALLOW" or "DENY" to stdout for each one, one per line.
// It never touches os.Args or os.Exit, so it can be exercised in a test with
// a strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("ratelimit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limit := fs.Int("limit", 0, "max allowed requests per window (required, > 0)")
	window := fs.Int64("window", 0, "window size in seconds (required, > 0)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	lim, err := NewLimiter(*limit, *window)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	scanner := bufio.NewScanner(stdin)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		ts, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			return fmt.Errorf("%w: line %d: %q is not an integer timestamp", errUsage, lineNo, line)
		}
		allowed, err := lim.Allow(ts)
		if err != nil {
			return fmt.Errorf("%w: line %d: %v", errUsage, lineNo, err)
		}
		if allowed {
			fmt.Fprintln(stdout, "ALLOW")
		} else {
			fmt.Fprintln(stdout, "DENY")
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("ratelimit: reading input: %w", err)
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: ratelimit -limit N -window W")
		fmt.Fprintln(os.Stderr, "reads newline-delimited, time-ordered integer timestamps from")
		fmt.Fprintln(os.Stderr, "stdin and prints ALLOW or DENY per line to stdout.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "ratelimit:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf '0\n1\n2\n3\n11\n12\n' | go run . -limit 3 -window 10
printf '5\nnot-a-timestamp\n' | go run . -limit 5 -window 5
```

Expected output:

```text
ALLOW
ALLOW
ALLOW
DENY
ALLOW
ALLOW
ALLOW
ratelimit: usage: line 2: "not-a-timestamp" is not an integer timestamp
```

With `-limit 3 -window 10`, requests at `0`, `1`, `2` fill the window and are
allowed; `3` is still inside the window of all three earlier requests and is
denied. By `11`, the window is `(1, 11]`, so `sort.Search` finds that
timestamps `0` and `1` have aged out, `slices.Delete` evicts them, and the
request is allowed with room to spare; `12` repeats the same eviction and
allowance. The second command shows the exit-2 usage error: line 1
(`5`) is allowed and printed before line 2 fails to parse as an integer,
which is exactly what a caller debugging a malformed log line needs to see.

### Tests

`TestNewLimiterRejectsInvalidConfig` and `TestAllowRejectsOutOfOrderTimestamps`
cover the constructor's and `Allow`'s validation. `TestAllowSlidesTheWindow`
is the table that matters most: the same six-request sequence shown above,
each step's `ALLOW`/`DENY` pinned individually so a regression in either the
cutoff arithmetic or the trim shows up at the exact request that exposes it.
`TestLogDoesNotAliasInternalStorage` pins the aliasing contract on `Log`.

`TestBuggyEvictionNeverShrinksLog` is the heart of the module. `evictBuggy`
is unexported and unreachable from the package API; it performs the same
`sort.Search` cutoff but discards `slices.Delete`'s result, and the test
replays ten widely-spaced timestamps through it to show the log grows to
exactly ten entries — one per request, no eviction ever took effect — while
the same ten timestamps through the real `Limiter` keep `Log()` bounded by
the configured limit. `TestRun` drives the command end to end over valid
sequences and every usage-error input, and `TestRunIOFailureIsNotAUsageError`
confirms a stdin read failure is reported without wrapping `errUsage`, so it
maps to exit code 1 instead of 2.

Create `ratelimit_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"
)

// evictBuggy is the mistake this module exists to prevent: it discards the
// slice slices.Delete returns instead of reassigning it, so the caller's
// slice header keeps its old length. Never exported, never reachable from
// the package API; it exists so the tests can pin how the window grows
// unbounded. (Assigning to _ is only what makes `go vet` accept the
// statement; the bug is that the result never reaches log.)
func evictBuggy(log []int64, cutoff int64) []int64 {
	idx := sort.Search(len(log), func(i int) bool { return log[i] > cutoff })
	_ = slices.Delete(log, 0, idx) // bug: return value discarded
	return log
}

func TestNewLimiterRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		limit  int
		window int64
		want   error
	}{
		{name: "zero limit", limit: 0, window: 10, want: ErrInvalidLimit},
		{name: "negative limit", limit: -1, window: 10, want: ErrInvalidLimit},
		{name: "zero window", limit: 5, window: 0, want: ErrInvalidWindow},
		{name: "negative window", limit: 5, window: -10, want: ErrInvalidWindow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewLimiter(tc.limit, tc.window); !errors.Is(err, tc.want) {
				t.Fatalf("NewLimiter(%d, %d) error = %v, want %v", tc.limit, tc.window, err, tc.want)
			}
		})
	}
}

// TestAllowSlidesTheWindow drives a single Limiter through a sequence of
// timestamps, including one far enough ahead that stale entries must be
// evicted before the new one is admitted.
func TestAllowSlidesTheWindow(t *testing.T) {
	t.Parallel()

	lim, err := NewLimiter(3, 10)
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}
	tests := []struct {
		now  int64
		want bool
	}{
		{0, true},  // log: [0]
		{1, true},  // log: [0,1]
		{2, true},  // log: [0,1,2], at the limit
		{3, false}, // still within window of all three: denied
		{11, true}, // cutoff=1: evicts 0 and 1, log: [2] then [2,11]
		{12, true}, // cutoff=2: evicts 2, log: [11] then [11,12]
	}
	for _, tc := range tests {
		got, err := lim.Allow(tc.now)
		if err != nil {
			t.Fatalf("Allow(%d): %v", tc.now, err)
		}
		if got != tc.want {
			t.Fatalf("Allow(%d) = %v, want %v (log=%v)", tc.now, got, tc.want, lim.Log())
		}
	}
}

func TestAllowRejectsOutOfOrderTimestamps(t *testing.T) {
	t.Parallel()

	lim, err := NewLimiter(5, 10)
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}
	if _, err := lim.Allow(10); err != nil {
		t.Fatalf("Allow(10): %v", err)
	}
	if _, err := lim.Allow(9); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("Allow(9) after Allow(10) error = %v, want ErrOutOfOrder", err)
	}
}

// TestLogDoesNotAliasInternalStorage pins the aliasing contract: mutating
// the slice Log returns must never corrupt the Limiter's own state.
func TestLogDoesNotAliasInternalStorage(t *testing.T) {
	t.Parallel()

	lim, err := NewLimiter(5, 60)
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}
	if _, err := lim.Allow(100); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	got := lim.Log()
	got[0] = -1
	if again := lim.Log(); again[0] == -1 {
		t.Fatalf("mutating a Log result corrupted the limiter's internal state")
	}
}

// TestBuggyEvictionNeverShrinksLog is the heart of the module: ten
// widely-spaced timestamps through evictBuggy leave the log exactly as long
// as the number of requests seen, growing without bound; the same
// timestamps through the real Limiter stay bounded by the configured
// limit, because Allow reassigns slices.Delete's result back into l.log.
func TestBuggyEvictionNeverShrinksLog(t *testing.T) {
	t.Parallel()

	const limit, window, requests = 2, int64(5), 10

	var buggyLog []int64
	for i := 0; i < requests; i++ {
		now := int64(i) * 100 // 100s apart: never overlaps a 5s window
		buggyLog = evictBuggy(buggyLog, now-window)
		buggyLog = append(buggyLog, now)
	}
	if len(buggyLog) != requests {
		t.Fatalf("evictBuggy log length = %d, want %d (unbounded growth)", len(buggyLog), requests)
	}

	lim, err := NewLimiter(limit, window)
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}
	for i := 0; i < requests; i++ {
		if _, err := lim.Allow(int64(i) * 100); err != nil {
			t.Fatalf("Allow: %v", err)
		}
	}
	if got := len(lim.Log()); got > limit {
		t.Fatalf("Limiter.Log() length = %d, want <= %d (bounded)", got, limit)
	}
	if len(buggyLog) <= len(lim.Log()) {
		t.Fatalf("buggy log (%d) should be strictly larger than the real log (%d)", len(buggyLog), len(lim.Log()))
	}
}

// TestRun exercises the command end to end: flag parsing, Limiter, and the
// exact ALLOW/DENY stdout lines, without ever going through os.Args or
// os.Exit.
func TestRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		args    []string
		stdin   string
		want    string
		wantErr bool
	}{
		{name: "sliding window over six requests", args: []string{"-limit", "3", "-window", "10"}, stdin: "0\n1\n2\n3\n11\n12\n", want: "ALLOW\nALLOW\nALLOW\nDENY\nALLOW\nALLOW\n"},
		{name: "blank lines are skipped", args: []string{"-limit", "1", "-window", "5"}, stdin: "1\n\n  \n100\n", want: "ALLOW\nALLOW\n"},
		{name: "zero limit is a usage error", args: []string{"-limit", "0", "-window", "5"}, stdin: "1\n", wantErr: true},
		{name: "non-integer line is a usage error", args: []string{"-limit", "5", "-window", "5"}, stdin: "nope\n", wantErr: true},
		{name: "out of order line is a usage error", args: []string{"-limit", "5", "-window", "5"}, stdin: "10\n9\n", wantErr: true},
		{name: "unknown flag is a usage error", args: []string{"-bogus"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.stdin), &stdout)
			if tc.wantErr {
				if !errors.Is(err, errUsage) {
					t.Fatalf("run(%v) error = %v, want it to wrap errUsage", tc.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(%v): %v", tc.args, err)
			}
			if stdout.String() != tc.want {
				t.Fatalf("run(%v) stdout = %q, want %q", tc.args, stdout.String(), tc.want)
			}
		})
	}
}

// failingReader simulates an I/O error on stdin, distinct from a malformed
// input line: it must be reported without wrapping errUsage, so main maps
// it to exit code 1, not 2.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("simulated stdin failure") }

func TestRunIOFailureIsNotAUsageError(t *testing.T) {
	t.Parallel()
	err := run([]string{"-limit", "5", "-window", "5"}, failingReader{}, &bytes.Buffer{})
	if err == nil || errors.Is(err, errUsage) {
		t.Fatalf("run with a failing reader = %v, want a plain runtime error", err)
	}
}
```

## Review

`Allow` is correct when the log it holds after every call contains exactly
the timestamps still inside the trailing window — never more. The mechanism
is `sort.Search` locating the boundary and `slices.Delete` cutting there,
with the non-negotiable step being `l.log = slices.Delete(...)`: without the
reassignment the trim computes a smaller slice and then throws it away,
`len(l.log)` never drops, and the limiter degrades from "allow bursts, deny
sustained overload" into "deny everything, forever" as soon as traffic
outlasts the window a few times over. Around that core, `NewLimiter` rejects
a non-positive limit or window, and `Allow` rejects an out-of-order
timestamp, both checkable with `errors.Is`; `Log` clones before returning,
so a caller inspecting the current window can never corrupt the limiter's
own state. `run` keeps that logic separate from `os.Args`/`os.Exit`,
streaming stdin one line at a time, mapping every input mistake to exit code
2, and reserving exit code 1 for the one failure that is not the caller's
fault: an I/O error reading the request stream itself. Run
`go test -count=1 -race ./...` to confirm the window-sliding table, the
validation guards, the aliasing guarantee, the unbounded-growth contrast,
and `run`'s end-to-end behavior including both exit paths.

## Resources

- [`sort.Search`](https://pkg.go.dev/sort#Search) — the monotone-predicate primitive that locates the trailing window's boundary.
- [`slices.Delete`](https://pkg.go.dev/slices#Delete) — why its result must be reassigned, and the `go vet` `unusedresult` check that flags the bare-statement form.
- [Redis: rate limiting patterns](https://redis.io/glossary/rate-limiting/) — `ZADD`/`ZREMRANGEBYSCORE` as the sorted-set version of this same sliding-window-log algorithm.
- [Cloudflare: how we built rate limiting](https://blog.cloudflare.com/counting-things-a-lot-of-different-things/) — a production discussion of sliding-window rate limiting trade-offs.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-content-addressed-pack-index.md](12-content-addressed-pack-index.md) | Next: [14-ttl-expiry-sweep.md](14-ttl-expiry-sweep.md)
