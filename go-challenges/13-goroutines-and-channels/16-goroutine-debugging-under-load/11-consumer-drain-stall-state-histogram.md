# Exercise 11: Classify a Stalled Consumer by Parsing a Goroutine Dump into a State Histogram

**Level: Intermediate**

A Kafka/SQS consumer-group worker has stopped draining its partition backlog and the
goroutine count is flat-high, not growing — so it is not a leak, it is a stall. On-call
cannot eyeball a 4000-goroutine `runtime.Stack(all=true)` dump by hand, and the naive
`grep chan` scrolls past the answer. This exercise builds the triage tool that parses a
full dump into structured goroutines, classifies each by its parked state, and reports
the dominant one, so "a wall of `chan send`" instantly fingerprints a stalled downstream
sink while "a wall of `semacquire`" fingerprints lock contention.

This module is self-contained: its own module, a `dumpscan` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
dumpscan/                    independent module: example.com/dumpscan
  go.mod                     go 1.26
  dumpscan.go                Dump, Parse, StateHistogram, DominantState, WithFrame
  cmd/demo/main.go           runnable demo: classify a synthetic dump, then a live wedge
  dumpscan_test.go           exact parse of a synthetic dump; a live wedge classifies as chan send
```

- Files: `dumpscan.go`, `cmd/demo/main.go`, `dumpscan_test.go`.
- Implement: `Dump() string` (growing-buffer `runtime.Stack`), `Parse(dump string) []Goroutine`,
  `StateHistogram(gs []Goroutine) map[string]int`, `DominantState(gs []Goroutine) (string, int)`,
  `WithFrame(gs []Goroutine, substr string) []Goroutine`.
- Test: `Parse` of a hand-written dump yields exact IDs, states, and stack text; after
  wedging N workers on a reader-less send, the live dump classifies as `chan send`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/dumpscan/cmd/demo
cd ~/go-exercises/dumpscan
go mod init example.com/dumpscan
```

### The state keyword is the diagnosis, and it lives in the header line

Every entry in a dump begins with a header of the form `goroutine <id> [<state>...]:`.
The bracket holds the parked-on state — `running`, `chan send`, `chan receive`,
`select`, `semacquire`, `IO wait` — and that single token is the class of bug before you
have read a single stack frame. The whole tool is therefore a header parser: split the
dump into entries, pull the ID and the state out of each header, keep the frames below it
as searchable text, then count.

Two details separate a correct parser from a fragile one.

1. **Strip the duration before you classify.** The runtime annotates a goroutine that has
   been parked a while with a duration: `goroutine 18 [chan send, 3 minutes]:`. A freshly
   parked goroutine has none: `goroutine 17 [chan send]:`. If you key your histogram on
   the whole bracket, "chan send" and "chan send, 3 minutes" become two different buckets
   and the wall you are hunting for splits in half. Take the state keyword as everything
   before the first comma, so aged and fresh goroutines land in the same bucket.

2. **Attribute the dump, then narrow it.** A histogram tells you *what* is stuck; it does
   not tell you *which of your functions*. `WithFrame` filters to the goroutines whose
   stack names a frame substring, so once the histogram says `chan send` you confirm the
   culprit is `drainWorker` and not some unrelated send. That is the difference between
   "something is blocked on a channel" and "the consumer's result-emit path is blocked."

`Dump` itself follows the growing-buffer idiom from the concepts file: `runtime.Stack`
truncates *silently* and returns `len(buf)` when the dump does not fit, and under load the
frames it drops are the deepest — the ones that matter. Double the buffer until the call
returns strictly fewer bytes than it holds; `n < len(buf)` is the only reliable
"complete" signal.

Create `dumpscan.go`:

```go
// Package dumpscan parses a runtime.Stack(all=true) goroutine dump into
// structured goroutines and classifies them by their parked state, so a wall of
// "chan send" (a stalled downstream sink) is instantly distinguishable from a
// wall of "semacquire" (lock contention) in a 4000-goroutine on-call dump.
package dumpscan

import (
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Goroutine is one entry from a dump: its numeric ID, its parked-on state
// keyword (the classifier), and the call-stack text below the header line.
type Goroutine struct {
	ID    int
	State string
	Stack string
}

// headerRe matches the header line "goroutine <id> [<state>...]:". The state
// group captures everything inside the brackets; Parse trims it to the keyword.
var headerRe = regexp.MustCompile(`^goroutine (\d+) \[([^\]]*)\]:`)

// Dump returns a complete goroutine dump using runtime.Stack(all=true), growing
// the buffer until the call returns fewer bytes than the buffer holds. n ==
// len(buf) means the dump was silently truncated, so we double and retry rather
// than lose the deepest frames.
func Dump() string {
	size := 1 << 16
	for {
		buf := make([]byte, size)
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		size *= 2
	}
}

// Parse splits a dump into one Goroutine per entry. It extracts the numeric ID
// and the state keyword from each header line; the state is taken before any
// comma or duration bracket ("chan receive, 2 minutes" classifies as "chan
// receive") so a fresh goroutine with no duration still classifies identically.
func Parse(dump string) []Goroutine {
	var gs []Goroutine
	var cur *Goroutine
	var stack strings.Builder

	flush := func() {
		if cur != nil {
			cur.Stack = strings.TrimRight(stack.String(), "\n")
			gs = append(gs, *cur)
			cur = nil
			stack.Reset()
		}
	}

	for _, line := range strings.Split(dump, "\n") {
		if m := headerRe.FindStringSubmatch(line); m != nil {
			flush()
			id, _ := strconv.Atoi(m[1])
			state := m[2]
			if i := strings.IndexByte(state, ','); i >= 0 {
				state = state[:i] // drop the duration: "chan send, 3 minutes" -> "chan send"
			}
			cur = &Goroutine{ID: id, State: strings.TrimSpace(state)}
			continue
		}
		if cur != nil {
			stack.WriteString(line)
			stack.WriteByte('\n')
		}
	}
	flush()
	return gs
}

// StateHistogram counts goroutines by their parked state.
func StateHistogram(gs []Goroutine) map[string]int {
	h := make(map[string]int, 8)
	for _, g := range gs {
		h[g.State]++
	}
	return h
}

// DominantState returns the state with the most goroutines parked in it. Ties
// break on the lexicographically smallest state so the result is deterministic
// regardless of map iteration order.
func DominantState(gs []Goroutine) (state string, count int) {
	for s, c := range StateHistogram(gs) {
		if c > count || (c == count && s < state) {
			state, count = s, c
		}
	}
	return state, count
}

// WithFrame returns the goroutines whose stack names the given function/frame
// substring, in dump order.
func WithFrame(gs []Goroutine, substr string) []Goroutine {
	var out []Goroutine
	for _, g := range gs {
		if strings.Contains(g.Stack, substr) {
			out = append(out, g)
		}
	}
	return out
}
```

### The runnable demo

The demo runs twice. First it classifies a hand-written synthetic dump — deterministic
input, so the printed IDs, histogram, and dominant state never depend on the live runtime.
Then it reproduces the real incident: it wedges eight workers on a bare send to a
reader-less channel, polls the live dump until every one is parked on `chan send` (the wait
synchronizes on the observed state, not on a clock), and prints the stable triage facts
before releasing the senders so the process exits clean.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"maps"
	"runtime"
	"slices"
	"sync"

	"example.com/dumpscan"
)

// synthetic is a hand-written dump: deterministic input so the parse-and-classify
// output never depends on the live runtime.
const synthetic = `goroutine 1 [running]:
main.main()
	/app/main.go:10 +0x20

goroutine 17 [chan send]:
main.drainWorker(0x1)
	/app/worker.go:42 +0x88

goroutine 18 [chan send, 3 minutes]:
main.drainWorker(0x2)
	/app/worker.go:42 +0x88

goroutine 23 [semacquire]:
sync.runtime_SemacquireMutex(...)
	/app/lock.go:9 +0x30
`

func main() {
	gs := dumpscan.Parse(synthetic)
	fmt.Println("parsed goroutines:", len(gs))
	for _, g := range gs {
		fmt.Printf("  id=%d state=%q\n", g.ID, g.State)
	}

	hist := dumpscan.StateHistogram(gs)
	fmt.Println("histogram:")
	for _, s := range slices.Sorted(maps.Keys(hist)) {
		fmt.Printf("  %-12s %d\n", s, hist[s])
	}

	state, count := dumpscan.DominantState(gs)
	fmt.Printf("dominant: %q x%d\n", state, count)
	fmt.Println("drainWorker frames:", len(dumpscan.WithFrame(gs, "drainWorker")))

	// Live triage: wedge N workers on a bare send to a reader-less channel, then
	// classify the real dump. The reported facts (counts, dominant) are stable.
	const n = 8
	ch := make(chan int)
	var done sync.WaitGroup
	for i := range n {
		done.Go(func() { drainWorker(ch, i) })
	}

	live := waitWedged(n)
	fmt.Println("live chan send >= n:", dumpscan.StateHistogram(live)["chan send"] >= n)
	ds, _ := dumpscan.DominantState(live)
	fmt.Println("live dominant:", ds)
	fmt.Println("live drainWorker count:", len(dumpscan.WithFrame(live, "drainWorker")))

	// Release every wedged sender so the process exits clean.
	for range n {
		<-ch
	}
	done.Wait()
	fmt.Println("released cleanly:", true)
}

// drainWorker is the named frame the triage tool fingerprints. It sends one value
// and returns; with no reader it wedges on the send.
func drainWorker(ch chan int, id int) { ch <- id }

// waitWedged polls the live dump until at least n drainWorker goroutines are
// parked on "chan send". It waits on the real condition, not on a clock.
func waitWedged(n int) []dumpscan.Goroutine {
	for {
		gs := dumpscan.Parse(dumpscan.Dump())
		parked := 0
		for _, g := range dumpscan.WithFrame(gs, "drainWorker") {
			if g.State == "chan send" {
				parked++
			}
		}
		if parked >= n {
			return gs
		}
		runtime.Gosched()
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
parsed goroutines: 4
  id=1 state="running"
  id=17 state="chan send"
  id=18 state="chan send"
  id=23 state="semacquire"
histogram:
  chan send    2
  running      1
  semacquire   1
dominant: "chan send" x2
drainWorker frames: 2
live chan send >= n: true
live dominant: chan send
live drainWorker count: 8
released cleanly: true
```

### Tests

`TestParseSyntheticExact` runs `Parse` on the hand-written dump as a pure function — no
runtime — and pins the exact IDs `[1 17 18 23]`, the exact states, positive IDs, and the
exact stack text of one entry. It also asserts that the aged `chan send, 3 minutes` and
the fresh `chan send` classify identically, proving the duration is stripped.
`TestHistogramAndDominantSynthetic` pins the counts, the dominant state, and the frame
filter on that same deterministic input. `TestWedgedConsumerClassifies` reproduces the
incident: it wedges N workers on a reader-less send, polls until all N are parked (a
state-based wait, never a sleep), then asserts `StateHistogram["chan send"] >= N`,
`DominantState == "chan send"`, `WithFrame(gs, "drainWorker")` returns exactly N, and every
ID is positive — then drains the channel to release every sender so the binary stays clean
under `-race`.

Create `dumpscan_test.go`:

```go
package dumpscan

import (
	"runtime"
	"sync"
	"testing"
)

// synthetic exercises Parse as a pure function: no runtime involved, so the IDs,
// states, and stack text are pinned down exactly and deterministically.
const synthetic = `goroutine 1 [running]:
main.main()
	/app/main.go:10 +0x20

goroutine 17 [chan send]:
main.drainWorker(0x1)
	/app/worker.go:42 +0x88

goroutine 18 [chan send, 3 minutes]:
main.drainWorker(0x2)
	/app/worker.go:42 +0x88

goroutine 23 [semacquire]:
sync.runtime_SemacquireMutex(...)
	/app/lock.go:9 +0x30
`

func TestParseSyntheticExact(t *testing.T) {
	gs := Parse(synthetic)

	wantIDs := []int{1, 17, 18, 23}
	wantStates := []string{"running", "chan send", "chan send", "semacquire"}
	if len(gs) != len(wantIDs) {
		t.Fatalf("Parse returned %d goroutines; want %d", len(gs), len(wantIDs))
	}
	for i, g := range gs {
		if g.ID != wantIDs[i] {
			t.Errorf("gs[%d].ID = %d; want %d", i, g.ID, wantIDs[i])
		}
		if g.State != wantStates[i] {
			t.Errorf("gs[%d].State = %q; want %q", i, g.State, wantStates[i])
		}
		if g.ID <= 0 {
			t.Errorf("gs[%d].ID = %d; want positive", i, g.ID)
		}
	}

	// The duration on goroutine 18 ("chan send, 3 minutes") must be stripped so a
	// fresh, no-duration goroutine classifies identically to an aged one.
	if gs[1].State != gs[2].State {
		t.Errorf("aged and fresh chan-send classify differently: %q vs %q", gs[2].State, gs[1].State)
	}

	// Stack text is the frames below the header, exactly.
	const wantStack = "main.drainWorker(0x1)\n\t/app/worker.go:42 +0x88"
	if gs[1].Stack != wantStack {
		t.Errorf("gs[1].Stack = %q; want %q", gs[1].Stack, wantStack)
	}
}

func TestHistogramAndDominantSynthetic(t *testing.T) {
	gs := Parse(synthetic)
	h := StateHistogram(gs)
	if h["chan send"] != 2 {
		t.Errorf("StateHistogram[chan send] = %d; want 2", h["chan send"])
	}
	if h["running"] != 1 || h["semacquire"] != 1 {
		t.Errorf("histogram = %v; want running:1 semacquire:1", h)
	}
	state, count := DominantState(gs)
	if state != "chan send" || count != 2 {
		t.Errorf("DominantState = (%q, %d); want (chan send, 2)", state, count)
	}
	if got := WithFrame(gs, "drainWorker"); len(got) != 2 {
		t.Errorf("WithFrame(drainWorker) = %d; want 2", len(got))
	}
}

// drainWorker is the named frame a wedged consumer parks under. With no reader it
// blocks forever on the send.
func drainWorker(ch chan int, id int) { ch <- id }

// waitWedged polls the live dump until at least n drainWorker goroutines are
// parked on "chan send". It synchronizes on the observed state, not on a timer,
// so it cannot pass before the wedge is real.
func waitWedged(t *testing.T, n int) []Goroutine {
	t.Helper()
	for {
		gs := Parse(Dump())
		parked := 0
		for _, g := range WithFrame(gs, "drainWorker") {
			if g.State == "chan send" {
				parked++
			}
		}
		if parked >= n {
			return gs
		}
		runtime.Gosched()
	}
}

func TestWedgedConsumerClassifies(t *testing.T) {
	const n = 50
	ch := make(chan int)
	var done sync.WaitGroup
	for i := range n {
		done.Go(func() { drainWorker(ch, i) })
	}

	gs := waitWedged(t, n)

	if h := StateHistogram(gs); h["chan send"] < n {
		t.Errorf("StateHistogram[chan send] = %d; want >= %d", h["chan send"], n)
	}
	if state, count := DominantState(gs); state != "chan send" || count < n {
		t.Errorf("DominantState = (%q, %d); want (chan send, >= %d)", state, count, n)
	}
	if got := WithFrame(gs, "drainWorker"); len(got) != n {
		t.Errorf("WithFrame(drainWorker) = %d; want exactly %d", len(got), n)
	}
	for _, g := range gs {
		if g.ID <= 0 {
			t.Errorf("goroutine ID %d is not positive", g.ID)
		}
	}

	// Release every wedged sender so the test binary stays clean under -race.
	for range n {
		<-ch
	}
	done.Wait()
}
```

## Review

The tool is correct when the state keyword it extracts is the same regardless of how long a
goroutine has been parked, and when the histogram it builds names the true bottleneck.
Stripping everything after the first comma is the invariant that guarantees this: without
it, `chan send` and `chan send, 3 minutes` fragment into separate buckets and the dominant
state is undercounted, exactly when the incident is worst. The pure-function tests pin the
parse down to exact bytes so a regex change cannot silently drift; the live test proves the
end-to-end path on a real `runtime.Stack` dump, and its state-based `waitWedged` loop
(never a `time.Sleep`) means the assertion cannot pass before all N workers are genuinely
parked on the send. The production bug this prevents is the on-call reflex of grepping a
4000-line dump by eye and guessing: the histogram makes "a wall of chan send under
`drainWorker`" — a stalled downstream sink starving a consumer group — a one-line readout
instead of a twenty-minute scroll, and `WithFrame` names the offending function so the fix
(a cancellable `select` send) lands on the right line.

## Resources

- [`runtime.Stack`](https://pkg.go.dev/runtime#Stack) — the `all=true` dump this parses, and the silent-truncation contract that forces the growing buffer.
- [Diagnostics](https://go.dev/doc/diagnostics) — where goroutine dumps and profiles fit in the Go runtime's diagnostic toolkit.
- [`regexp`](https://pkg.go.dev/regexp) — `FindStringSubmatch` and the capture groups used to pull the ID and state from each header line.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-execution-trace-scheduler-stalls.md](10-execution-trace-scheduler-stalls.md) | Next: [12-outbox-leak-localize-profile-delta.md](12-outbox-leak-localize-profile-delta.md)
