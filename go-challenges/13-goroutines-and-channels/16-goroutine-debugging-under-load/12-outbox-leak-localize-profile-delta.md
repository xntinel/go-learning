# Exercise 12: Localize A Slow Leak By Diffing Two Goroutine Profiles For The Growing Stack

**Level: Intermediate**

An outbox relay is leaking goroutines slowly in production: the total count creeps
up over hours, but the raw number alone does not say *where*. The field technique is
to capture a goroutine profile, wait, capture a second, and diff them — the single
stack whose goroutine count grew names the leak site. This exercise builds that delta
tool over the aggregated goroutine profile (`debug=1`), keyed by symbol frames rather
than addresses (which move run to run), so the offending stack is attributed
automatically instead of eyeballed.

This module is self-contained: its own module, a `gprofdelta` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
gprofdelta/                  independent module: example.com/gprofdelta
  go.mod                     go 1.26
  gprofdelta.go              Snapshot() and Diff(): parse debug=1 profiles, diff by symbol stack
  cmd/demo/main.go           runnable demo: spawn parked workers, localize them from a profile diff
  gprofdelta_test.go         parse/dedup, address-independence, leak localization, non-leaker not attributed
```

- Files: `gprofdelta.go`, `cmd/demo/main.go`, `gprofdelta_test.go`.
- Implement: `Snapshot() []StackCount` capturing `pprof.Lookup("goroutine").WriteTo(w, 1)` and parsing it into per-stack counts keyed by joined symbol frames; `Diff(before, after []StackCount) []StackCount` returning stacks whose count grew, `Count` set to the positive delta, sorted by delta descending.
- Test: the parser aggregates identical symbol stacks and dedups; keys are address-independent; a spawned set of `K` parked workers appears as one `Diff` entry naming `leakWorker` with `Count == K`; a start-and-join workload is not attributed.
- Verify: `go test -count=1 -race ./...`

### Why the delta, and why keying on symbols not addresses

A single goroutine profile answers "how many goroutines, in what stacks, right now?"
That is enough to find a *fast* leak — a stack with a thousand goroutines is obvious.
A *slow* leak is different: the offending stack may hold only a handful more than it
did an hour ago, drowned in the hundreds of legitimate parked goroutines a healthy
service carries (idle pool workers, the HTTP server, background tickers). Staring at
one profile, you cannot tell the slowly-growing stack from the steady ones. The delta
does: capture, wait for the leak to advance, capture again, and subtract. Only the
leaking stack has a positive delta, because everything else is at steady state.

The `debug=1` goroutine profile is already aggregated for you. Its text form is a
banner line followed by one block per distinct stack:

```text
goroutine profile: total 4
3 @ 0x102554a98 0x1024f1644 0x1025ad584 ...
#	0x1025ad583	main.leakWorker+0x43	/outbox/relay.go:12
#	0x10255c2f3	runtime.goexit+0x1	/usr/local/go/src/runtime/asm.s:1
```

The header `3 @ addr addr ...` gives the count and the return-address stack; each
indented `#` line names one frame as `0xADDR<TAB>FUNC+0xOFFSET<TAB>/file:line`. The
trap is the addresses. Absolute program-counter addresses on the `@` header and the
`#` lines are randomized per process (ASLR) and shift with every build, so keying a
stack on them makes two snapshots of *the very same parked goroutines* look like
different stacks and the diff collapses into noise. The fix is to key on the *symbol
names* — `main.leakWorker`, `runtime.goexit` — which are stable across runs. We take
the function field, strip its `+0xOFFSET` (that offset moves with the binary too),
join the frames, and use that as the key. Two blocks that reduce to the same symbol
key are merged and their counts summed, so the parser is address-independent by
construction.

`Diff` then indexes `before` by key, and for each `after` stack emits the positive
delta, sorted so the biggest grower is first — the leak site at the top of the list.

Create `gprofdelta.go`:

```go
// Package gprofdelta localizes a slow goroutine leak by diffing two aggregated
// goroutine profiles. It captures the debug=1 goroutine profile, parses it into
// per-stack counts keyed by symbol frame names (address-independent), and reports
// the stacks whose count grew between two snapshots so the leak site is attributed
// automatically instead of eyeballed.
package gprofdelta

import (
	"bufio"
	"bytes"
	"io"
	"slices"
	"strings"

	"runtime/pprof"
)

// StackCount is one aggregated goroutine stack: Count goroutines share the call
// stack Frames. Key is the joined frame symbols and is stable across runs because
// it contains no absolute addresses.
type StackCount struct {
	Key    string
	Frames []string
	Count  int
}

// Snapshot captures the current goroutine profile via
// pprof.Lookup("goroutine").WriteTo(w, 1) and parses it into per-stack counts,
// keyed by the joined frame symbol names (address-independent).
func Snapshot() []StackCount {
	var buf bytes.Buffer
	// The goroutine profile never errors to a bytes.Buffer; debug=1 is the
	// aggregated "N @ addrs" text form.
	_ = pprof.Lookup("goroutine").WriteTo(&buf, 1)
	return parseProfile(&buf)
}

// parseProfile reads a debug=1 goroutine profile and returns one StackCount per
// distinct symbol stack. Blocks that reduce to the same symbol key (identical
// frames but different absolute addresses) are merged, their counts summed.
func parseProfile(r io.Reader) []StackCount {
	byKey := map[string]int{}
	frames := map[string][]string{}
	var order []string

	var count int    // count for the block currently being read
	var cur []string // symbol frames accumulated for the current block
	inBlock := false

	flush := func() {
		if !inBlock {
			return
		}
		key := strings.Join(cur, ";")
		if _, seen := byKey[key]; !seen {
			order = append(order, key)
			frames[key] = cur
		}
		byKey[key] += count
		inBlock = false
		cur = nil
		count = 0
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "#"):
			// Frame line: "#\t0xADDR\tFUNC+0xOFF\t...\t/file:line".
			// Field index 2 is the symbol regardless of alignment padding.
			fields := strings.Split(line, "\t")
			if len(fields) >= 3 {
				cur = append(cur, symbolName(fields[2]))
			}
		case line == "":
			flush()
		default:
			// A header "N @ addr addr ..." starts a block; anything else
			// (the "goroutine profile: total N" banner) is ignored.
			if n, ok := parseHeader(line); ok {
				flush()
				inBlock = true
				count = n
				cur = nil
			}
		}
	}
	flush()

	out := make([]StackCount, 0, len(order))
	for _, key := range order {
		out = append(out, StackCount{Key: key, Frames: frames[key], Count: byKey[key]})
	}
	return out
}

// parseHeader parses a "N @ addr addr ..." block header, returning N and true.
func parseHeader(line string) (int, bool) {
	at := strings.Index(line, " @")
	if at <= 0 {
		return 0, false
	}
	n := 0
	for i := 0; i < at; i++ {
		c := line[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// symbolName strips the trailing "+0xNN" program-counter offset from a frame
// symbol, leaving the address-independent function name.
func symbolName(field string) string {
	if i := strings.LastIndex(field, "+0x"); i >= 0 {
		return field[:i]
	}
	return field
}

// Diff returns the stacks whose count increased from before to after, Count set
// to the positive delta, sorted by delta descending (Key ascending on ties).
func Diff(before, after []StackCount) []StackCount {
	base := make(map[string]int, len(before))
	for _, s := range before {
		base[s.Key] += s.Count
	}

	out := make([]StackCount, 0)
	for _, s := range after {
		if d := s.Count - base[s.Key]; d > 0 {
			out = append(out, StackCount{Key: s.Key, Frames: s.Frames, Count: d})
		}
	}

	slices.SortFunc(out, func(a, b StackCount) int {
		if a.Count != b.Count {
			return b.Count - a.Count // delta descending
		}
		return strings.Compare(a.Key, b.Key) // stable tiebreak
	})
	return out
}
```

### The runnable demo

The demo takes a baseline snapshot, spawns eight workers that park in `leakWorker`,
snapshots again, and diffs the two — printing the frame that names the leak and the
delta. It then releases and joins the workers, so the demo itself leaks nothing. The
output is deterministic: it reports only the `leakWorker`-bearing stacks, never the
raw profile whose runtime goroutine counts vary.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"
	"sync"

	"example.com/gprofdelta"
)

// leakWorker models an outbox relay goroutine that parks forever waiting for a
// delivery slot. In the leak, release is never closed in production; here the
// demo closes it after the measurement so nothing is left running.
func leakWorker(started *sync.WaitGroup, release <-chan struct{}) {
	started.Done()
	<-release
}

func main() {
	const k = 8

	before := gprofdelta.Snapshot()

	release := make(chan struct{})
	var started, done sync.WaitGroup
	started.Add(k)
	done.Add(k)
	for range k {
		go func() {
			defer done.Done()
			leakWorker(&started, release)
		}()
	}
	started.Wait() // every worker is parked before we measure

	after := gprofdelta.Snapshot()

	// Attribute the growth. Sum the delta across every grown stack that names
	// our worker (a still-parking set may briefly span two stacks).
	site, delta := "", 0
	for _, s := range gprofdelta.Diff(before, after) {
		if !strings.Contains(s.Key, "leakWorker") {
			continue
		}
		delta += s.Count
		for _, f := range s.Frames {
			if strings.Contains(f, "leakWorker") {
				site = f
			}
		}
	}

	fmt.Println("leak localized at:", site)
	fmt.Println("goroutine delta:", delta)

	close(release) // release the parked workers
	done.Wait()    // and join them: the demo leaks nothing
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
leak localized at: main.leakWorker
goroutine delta: 8
```

### Tests

`TestParseAggregatesAndDedups` feeds a synthetic `debug=1` profile whose two `N @`
blocks reduce to the same symbol stack (identical frames, different absolute
addresses) and asserts they merge into one `StackCount` with the summed count and the
bare symbol frames. `TestKeysAreAddressIndependent` parses two profiles that share a
symbol stack but use entirely different addresses on both the `@` header and the `#`
lines, and asserts the derived keys are identical. `TestDiffLocalizesLeak` is the core
property: snapshot, spawn `K` parked workers, wait (via `runtime.Gosched`, not sleep)
until they converge to one aggregated stack, snapshot again, and assert the diff names
`leakWorker` with delta exactly `K`; the workers are released and joined after the
assertion so the test leaks nothing under `-race`. `TestNonLeakingWorkloadNotAttributed`
runs a start-and-join workload and asserts that once its goroutines have exited the
diff carries no positive-delta stack naming it — a leaker would never converge here, a
non-leaker always does.

Create `gprofdelta_test.go`:

```go
package gprofdelta

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// leakWorker parks forever on release: this is the leaking stack the diff must
// localize. joinWorker below returns promptly: it must NOT be attributed.
func leakWorker(started *sync.WaitGroup, release <-chan struct{}) {
	started.Done()
	<-release
}

func joinWorker(done *sync.WaitGroup) {
	defer done.Done()
	sum := 0
	for i := range 64 {
		sum += i
	}
	_ = sum
}

// namedStacks returns the stacks whose key contains name, and how many there are.
func namedStacks(snap []StackCount, name string) (StackCount, int) {
	var hit StackCount
	n := 0
	for _, s := range snap {
		if strings.Contains(s.Key, name) {
			hit = s
			n++
		}
	}
	return hit, n
}

// TestParseAggregatesAndDedups feeds a synthetic debug=1 profile with two "N @"
// blocks that reduce to the same symbol stack (identical frames, different
// absolute addresses) and asserts they merge into one StackCount with the summed
// count and the symbol frames only.
func TestParseAggregatesAndDedups(t *testing.T) {
	prof := "goroutine profile: total 5\n" +
		"2 @ 0x1000 0x2000\n" +
		"#\t0x1000\tmain.leakWorker+0x43\t/outbox/relay.go:12\n" +
		"#\t0x2000\truntime.goexit+0x1\t/usr/local/go/src/runtime/asm.s:1\n" +
		"\n" +
		"3 @ 0x9000 0xa000\n" +
		"#\t0x9000\tmain.leakWorker+0x43\t/outbox/relay.go:12\n" +
		"#\t0xa000\truntime.goexit+0x1\t/usr/local/go/src/runtime/asm.s:1\n" +
		"\n"

	got := parseProfile(strings.NewReader(prof))
	if len(got) != 1 {
		t.Fatalf("parseProfile returned %d stacks, want 1 (merged): %+v", len(got), got)
	}
	if got[0].Count != 5 {
		t.Errorf("merged Count = %d, want 5", got[0].Count)
	}
	want := []string{"main.leakWorker", "runtime.goexit"}
	if strings.Join(got[0].Frames, ";") != strings.Join(want, ";") {
		t.Errorf("Frames = %v, want %v", got[0].Frames, want)
	}
}

// TestKeysAreAddressIndependent parses two profiles that share the same symbol
// stack but use entirely different absolute addresses on both the "@" header and
// the "#" frame lines, and asserts the derived keys are identical.
func TestKeysAreAddressIndependent(t *testing.T) {
	frame := func(hdr, a1, a2 string) string {
		return hdr + " @ " + a1 + " " + a2 + "\n" +
			"#\t" + a1 + "\tmain.leakWorker+0x43\t/outbox/relay.go:12\n" +
			"#\t" + a2 + "\truntime.goexit+0x1\t/asm.s:1\n\n"
	}
	p1 := "goroutine profile: total 1\n" + frame("1", "0x1", "0x2")
	p2 := "goroutine profile: total 1\n" + frame("1", "0xdead0000", "0xbeef1111")

	k1 := parseProfile(strings.NewReader(p1))
	k2 := parseProfile(strings.NewReader(p2))
	if len(k1) != 1 || len(k2) != 1 {
		t.Fatalf("want one stack each, got %d and %d", len(k1), len(k2))
	}
	if k1[0].Key != k2[0].Key {
		t.Errorf("keys differ despite identical symbols:\n %q\n %q", k1[0].Key, k2[0].Key)
	}
}

// TestDiffLocalizesLeak is the core property: snapshot, spawn K parked workers,
// snapshot again, and the diff names leakWorker with delta exactly K. It waits
// (via Gosched, not sleep) until all K are parked in one aggregated stack, which
// is the real capture/wait/capture technique.
func TestDiffLocalizesLeak(t *testing.T) {
	const k = 5
	before := Snapshot()

	release := make(chan struct{})
	var started, done sync.WaitGroup
	started.Add(k)
	done.Add(k)
	for range k {
		go func() {
			defer done.Done()
			leakWorker(&started, release)
		}()
	}
	started.Wait()

	// Wait until the K workers have all parked as a single aggregated stack.
	var after []StackCount
	for spin := 0; ; spin++ {
		after = Snapshot()
		if hit, n := namedStacks(after, "leakWorker"); n == 1 && hit.Count == k {
			break
		}
		if spin > 1_000_000 {
			t.Fatal("workers never converged to one parked leakWorker stack")
		}
		runtime.Gosched()
	}

	diff := Diff(before, after)
	hit, n := namedStacks(diff, "leakWorker")
	if n != 1 {
		t.Fatalf("Diff named leakWorker in %d stacks, want 1: %+v", n, diff)
	}
	if hit.Count != k {
		t.Errorf("delta = %d, want %d", hit.Count, k)
	}

	close(release) // release the leaked workers
	done.Wait()    // and join: this test leaks nothing under -race
}

// TestNonLeakingWorkloadNotAttributed runs a start-and-join workload and asserts
// that once its goroutines have exited, the diff carries no positive-delta stack
// naming it. A leaker would never converge here; a non-leaker always does.
func TestNonLeakingWorkloadNotAttributed(t *testing.T) {
	const k = 6
	before := Snapshot()

	var done sync.WaitGroup
	done.Add(k)
	for range k {
		go joinWorker(&done)
	}
	done.Wait()

	for spin := 0; ; spin++ {
		runtime.GC()
		after := Snapshot()
		if _, n := namedStacks(Diff(before, after), "joinWorker"); n == 0 {
			return // exited and produced no positive delta: correct
		}
		if spin > 1_000_000 {
			t.Fatal("joinWorker still attributed after exit; false leak")
		}
		runtime.Gosched()
	}
}

func ExampleDiff() {
	before := []StackCount{
		{Key: "a", Frames: []string{"a"}, Count: 3},
		{Key: "b", Frames: []string{"b"}, Count: 1},
	}
	after := []StackCount{
		{Key: "a", Frames: []string{"a"}, Count: 10}, // +7
		{Key: "b", Frames: []string{"b"}, Count: 1},  // 0
		{Key: "c", Frames: []string{"c"}, Count: 2},  // +2
	}
	for _, s := range Diff(before, after) {
		fmt.Printf("%s +%d\n", s.Key, s.Count)
	}
	// Output:
	// a +7
	// c +2
}
```

## Review

The tool is correct when the diff names exactly the stack that grew, and only that
stack, regardless of how the runtime laid out addresses in either process. The
guarantee rests on the key being derived from symbol names with the `+0xOFFSET`
stripped: two profiles of the same parked goroutines produce identical keys even
though every absolute address differs, which `TestKeysAreAddressIndependent` proves
and `TestParseAggregatesAndDedups` reinforces by merging two address-distinct blocks
into one summed count. `TestDiffLocalizesLeak` proves the end-to-end property — `K`
new goroutines parked in `leakWorker` surface as a single diff entry with `Count == K`
— and `TestNonLeakingWorkloadNotAttributed` proves the converse, that work which
finishes leaves no positive delta. The production bug this prevents is the misdiagnosis
that comes from keying on addresses: a naive diff of two raw profiles reports every
stack as "changed" because the PCs moved, burying the one stack that actually leaked;
keying on symbols makes the slow leak the single line at the top of the sorted delta.

## Resources

- [`runtime/pprof`](https://pkg.go.dev/runtime/pprof) -- `Lookup` and `Profile.WriteTo`, and the `debug` level that selects the aggregated `N @ addrs` text form parsed here.
- [Profiling Go Programs](https://go.dev/blog/pprof) -- how the goroutine profile is captured and read, and why stacks are the unit of aggregation.
- [Diagnostics](https://go.dev/doc/diagnostics) -- the goroutine profile among Go's diagnostic tools, and when a count-over-time delta is the right instrument for a leak.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-consumer-drain-stall-state-histogram.md](11-consumer-drain-stall-state-histogram.md) | Next: [13-request-hedging-leak-free-cancellation.md](13-request-hedging-leak-free-cancellation.md)
