# Exercise 19: Alert Threshold Scan: An iter.Seq2 That Obeys the Stop Signal

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An alerting pipeline in the shape of Prometheus's rule evaluator runs the
same check every tick: walk a map of metric samples -- one entry per label
combination, tens of thousands on a busy service -- and report the first
handful that cross a threshold, because a paging system wants "show me five
breaching series," not the whole cardinality of the metric. `00-concepts.md`'s
point about `maps.Keys`, `maps.Values`, and `maps.All` returning an
`iter.Seq` rather than a collection is what makes that cheap: a plain range
loop can stop at the fifth match without ever building a slice to hold the
thousands the caller was never going to look at.

That laziness is free only while you *consume* `maps.Keys` and its siblings,
because the standard library's own iterators already do the right thing
when a range loop stops early. The moment you write your *own* `iter.Seq2`
-- wrapping `maps.All` with a filter, the way an alert scanner naturally
does -- you inherit a contract the compiler does not check: the `yield`
function an iterator calls returns `false` the instant the consuming loop
stops, and if the iterator calls `yield` again after that, Go's
range-over-func runtime panics with `range function continued iteration
after function for loop body returned false`. An iterator that ignores this
is invisible in every test that drains it fully, and then panics in
production the first time a caller exercises the early exit it was written
for -- typically the day someone adds the `-limit` flag this exercise
builds.

This module builds `alertscan`, a command-line tool that scans a stream of
metric samples for the first few that cross a threshold and stops the
instant it has enough. The scanning iterator is written correctly, and only
correctly, inside the tool; the version that ignores `yield`'s return value
lives in the test file, where a test recovers its panic.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
alertscan/                    module example.com/alertscan
  go.mod                      go 1.24
  alertscan.go                 package main — AlertScanner; NewAlertScanner; (*AlertScanner) Above
  alertscan_test.go            package main — Above table, malformed-sample table, aliasing,
                                the ignoring-stop panic, concurrency, run() end to end
  main.go                      package main — -threshold/-limit flags, exit codes
```

- Files: `alertscan.go`, `alertscan_test.go`, `main.go`.
- Implement: `NewAlertScanner(samples map[string]float64) (*AlertScanner, error)` cloning `samples` and rejecting a NaN or infinite value with `ErrMalformedSample`; `(*AlertScanner) Above(threshold float64) iter.Seq2[string, float64]` yielding `(name, value)` pairs above `threshold` in ascending name order, and returning the instant `yield` reports `false`.
- Tool: `alertscan` reads `name value` lines from stdin -- one sample per line, a duplicate name resolving last-value-wins -- and writes the first `-limit` alerting samples to stdout, `alerts: N` to stderr. `-threshold` defaults to `0`; `-limit` defaults to `5` and must be at least `1`. Exit 0 on success (including zero matches), exit 2 for a bad flag, a `-limit` below `1`, a malformed sample line, or a rejected NaN/Inf value (all usage errors the caller fixes by changing the command line or the input), exit 1 for a genuine I/O failure reading stdin.
- Test: the `Above` table (mixed samples, threshold above everything, empty map, nil map); `NewAlertScanner` rejecting NaN, `+Inf`, and `-Inf`; the clone-on-construction aliasing contract; an `aboveIgnoringStop` contrast whose panic on early break is recovered and matched against the exact runtime message, alongside `Above` handling the identical early break silently; twenty goroutines calling `Above` concurrently under `-race`; and `run` end to end over a `strings.Reader` and two `bytes.Buffer`s.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/alertscan
cd ~/go-exercises/alertscan
go mod init example.com/alertscan
go mod edit -go=1.24
```

### Writing your own iter.Seq2 means owning the stop contract

`maps.Keys`, `maps.Values`, and `maps.All` are `iter.Seq`/`iter.Seq2`
producers you only ever consume, so the machinery that stops them cleanly is
entirely the standard library's problem. `Above` is different: it is a
producer you are writing, built by wrapping `maps.All` (by way of
`slices.Sorted(maps.Keys(...))`, for a deterministic scan order) with a
threshold filter. The instant you write your own iterator function, the
range-over-func contract stops being someone else's concern and becomes
yours: your function receives a `yield` callback, and it must stop calling
`yield` -- immediately, with no further work -- the first time `yield`
returns `false`.

The bug that breaks this contract reads as harmless, because it is the
version you get by simply not thinking about the return value at all:

```go
func aboveIgnoringStop(samples map[string]float64, threshold float64) iter.Seq2[string, float64] {
    return func(yield func(string, float64) bool) {
        for _, name := range slices.Sorted(maps.Keys(samples)) {
            v := samples[name]
            if v <= threshold {
                continue
            }
            yield(name, v)   // return value ignored
        }
    }
}
```

Every test that ranges over this and lets the loop finish naturally passes:
`yield` always returns `true` when nothing ever asked it to stop. The
failure only appears the moment a caller writes `for name, v := range
aboveIgnoringStop(m, t) { ...; break }` -- exactly what a `-limit`-style
early exit does. `break` makes the *next* call to `yield` return `false`;
`aboveIgnoringStop` calls it again anyway on its next loop iteration, and
Go's runtime panics right there, deterministically, every time, with the
message quoted above. `Above` fixes this with one line --
`if !yield(name, v) { return }` -- the entire difference between a scanner
safe to `break` out of and one that is a time bomb waiting for its first
`-limit` flag.

Create `alertscan.go`:

```go
// Command alertscan scans a snapshot of metric samples for values above a
// threshold, stopping the moment it has found enough matches instead of
// walking the whole snapshot on every alerting tick.
//
// This file holds the domain type, AlertScanner, and its scan method,
// Above. The comment on Above is the point of the module: Above returns an
// iter.Seq2, and the correctness of a hand-written iter.Seq2 rests entirely
// on obeying the boolean yield returns -- something no consumer of
// maps.Keys or maps.Values ever has to think about, because the standard
// library already gets it right. See alertscan_test.go for what happens
// when a hand-written producer does not.
package main

import (
	"errors"
	"fmt"
	"iter"
	"maps"
	"math"
	"slices"
)

// ErrMalformedSample means a metric value was NaN or infinite. Both are
// syntactically valid float64 values -- strconv.ParseFloat("NaN", 64)
// succeeds -- so nothing upstream of NewAlertScanner catches them; a NaN
// reading is usually evidence of a division by zero in the exporter, and
// every comparison against it is false, which would otherwise make the
// affected series silently unalertable forever.
var ErrMalformedSample = errors.New("alertscan: malformed sample")

// AlertScanner scans a fixed snapshot of metric samples for values above a
// caller-supplied threshold.
//
// An AlertScanner is immutable after construction (NewAlertScanner clones
// its input) and is safe for concurrent use by multiple goroutines: each
// call to Above starts a fresh, independent scan over the same snapshot.
type AlertScanner struct {
	samples map[string]float64
}

// NewAlertScanner returns an AlertScanner over a clone of samples; the
// caller's map is never retained or mutated. It returns ErrMalformedSample,
// wrapped with the offending metric name, if any value is NaN or infinite.
// A nil or empty samples map is valid and yields a scanner that never
// reports an alert.
func NewAlertScanner(samples map[string]float64) (*AlertScanner, error) {
	for name, v := range samples {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("%w: %s = %v", ErrMalformedSample, name, v)
		}
	}
	return &AlertScanner{samples: maps.Clone(samples)}, nil
}

// Above returns an iter.Seq2 of (name, value) pairs whose value exceeds
// threshold, visited in ascending name order so two scans of the same
// snapshot always agree.
//
// Above obeys the range-over-func contract: the instant the consuming range
// loop stops -- break, return, or a goto out of the loop body -- the yield
// function it was given returns false, and Above must not call it again.
// A caller that only wants the first few matches can therefore break out of
// the loop early and Above will not evaluate the remaining samples at all.
func (s *AlertScanner) Above(threshold float64) iter.Seq2[string, float64] {
	return func(yield func(string, float64) bool) {
		for _, name := range slices.Sorted(maps.Keys(s.samples)) {
			v := s.samples[name]
			if v <= threshold {
				continue
			}
			if !yield(name, v) {
				return
			}
		}
	}
}
```

### The tool

`alertscan` has no persistent state: everything arrives as flags plus a
stream on stdin, so `run` takes the argument slice, an `io.Reader` for
stdin, and two `io.Writer`s for stdout and stderr, trivial to drive from a
table test with a `strings.Reader` and a pair of `bytes.Buffer`s.
`bufio.Scanner` reads stdin one line at a time instead of loading the whole
stream first, and `AlertScanner.Above` streams its alerting subset without
materializing a slice. Every failure short of an actual stdin read error --
a bad flag, `-limit` below `1`, a malformed line, an unparseable or NaN/Inf
value -- wraps `errUsage` and maps to exit code 2; `sc.Err()` after the
scan loop is the one genuine I/O failure, mapped to exit code 1.

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

// errUsage marks a failure the caller can fix by changing the flags or the
// input: a bad flag, an invalid limit, or a malformed sample line. main
// maps it to exit code 2; every other error maps to exit code 1.
var errUsage = errors.New("usage")

// run parses args, reads "name value" samples from stdin -- one per line,
// duplicate names resolving last-value-wins -- and writes up to -limit
// alerting samples to stdout, in ascending name order, stopping the scan
// the moment that many have been found. run never touches os.Exit, so it
// can be exercised in a test with a strings.Reader and bytes.Buffers.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("alertscan", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	threshold := fs.Float64("threshold", 0, "alert when a sample's value exceeds this")
	limit := fs.Int("limit", 5, "stop after this many alerts")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if *limit < 1 {
		return fmt.Errorf("%w: -limit must be >= 1, got %d", errUsage, *limit)
	}

	samples := map[string]float64{}
	sc := bufio.NewScanner(stdin)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return fmt.Errorf("%w: line %d: want \"name value\", got %q", errUsage, lineNo, line)
		}
		v, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			return fmt.Errorf("%w: line %d: %v", errUsage, lineNo, err)
		}
		samples[fields[0]] = v
	}
	if err := sc.Err(); err != nil {
		return err // runtime failure reading stdin
	}

	scanner, err := NewAlertScanner(samples)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	found := 0
	for name, v := range scanner.Above(*threshold) {
		fmt.Fprintf(stdout, "%s %g\n", name, v)
		found++
		if found >= *limit {
			break
		}
	}
	fmt.Fprintf(stderr, "alerts: %d\n", found)
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: alertscan -threshold N [-limit N] < samples")
		fmt.Fprintln(os.Stderr, "reads \"name value\" lines from stdin and prints the first")
		fmt.Fprintln(os.Stderr, "-limit samples whose value exceeds -threshold.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "alertscan:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'cpu 42\nmem 91\ndisk 12\nnet 88\n' | go run . -threshold 50 -limit 2
printf 'cpu 42\nmem 91\ndisk 12\nnet 88\n' | go run . -threshold 10
printf 'cpu 42\nmem oops\n' | go run . -threshold 0
```

Expected output:

```text
mem 91
net 88
alerts: 2
```

```text
cpu 42
disk 12
mem 91
net 88
alerts: 4
```

```text
alertscan: usage: line 2: strconv.ParseFloat: parsing "oops": invalid syntax
```

The first command has four candidate breaches but a `-limit` of `2`: `Above`
visits `cpu`, `disk`, `mem`, `net` in sorted order, skips the two below 50,
yields `mem` then `net`, and `run` breaks -- `disk` and anything past `net`
is never evaluated. The second command lowers the threshold so all four
samples qualify and raises no limit, listing every match in the same sorted
order the first command only partially walked. The third is the exit-2
usage error: `oops` is not a valid float, so `run` never reaches
`NewAlertScanner`.

### Tests

`TestAbove` is the table: mixed samples, a threshold above every value, an
empty map, and a nil map, asserting exact sorted name order.
`TestNewAlertScannerRejectsMalformedSamples` sweeps NaN, `+Inf`, `-Inf`, and
`TestNewAlertScannerClonesInput` mutates the caller's map after
construction to confirm the scanner never sees it.

`TestIgnoringStopPanicsOnEarlyBreak` is the test the module is built
around. It ranges `aboveIgnoringStop` with a `break` on the first match,
recovers the panic, and asserts the error text contains the exact runtime
message quoted above -- the literal string Go's runtime emits, not a
plausible-looking substring. The same test then takes the identical early
break through `Above` and recovers nothing, the whole point of checking
`yield`'s result. `TestConcurrentAboveReads` runs twenty goroutines calling
`Above` on one shared, immutable scanner under `-race`. `TestRun` drives
the command end to end: limited and unlimited scans, a malformed line, an
unparseable value, an explicit `nan` value, `-limit 0`, and an unknown
flag, each asserting the error wraps `errUsage`.

Create `alertscan_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"iter"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
)

// aboveIgnoringStop is Above written the way a first draft usually is: it
// calls yield but never looks at what it returns. It is never exported and
// never reachable from AlertScanner; it exists so the tests can pin exactly
// what ignoring the stop signal costs.
func aboveIgnoringStop(samples map[string]float64, threshold float64) iter.Seq2[string, float64] {
	return func(yield func(string, float64) bool) {
		for _, name := range slices.Sorted(maps.Keys(samples)) {
			v := samples[name]
			if v <= threshold {
				continue
			}
			yield(name, v) // return value ignored -- the bug
		}
	}
}

func TestAbove(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		samples   map[string]float64
		threshold float64
		want      []string
	}{
		{"mixed above and below", map[string]float64{"a": 1, "b": 5, "c": 10, "d": 3}, 2, []string{"b", "c", "d"}},
		{"threshold above every sample", map[string]float64{"a": 1, "b": 2}, 100, nil},
		{"empty map", map[string]float64{}, 0, nil},
		{"nil map", nil, 0, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := NewAlertScanner(tc.samples)
			if err != nil {
				t.Fatalf("NewAlertScanner: %v", err)
			}
			var got []string
			for name := range s.Above(tc.threshold) {
				got = append(got, name)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Above names = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewAlertScannerRejectsMalformedSamples(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		v    float64
	}{
		{"NaN", math.NaN()}, {"+Inf", math.Inf(1)}, {"-Inf", math.Inf(-1)},
	} {
		samples := map[string]float64{"cpu": tc.v}
		if _, err := NewAlertScanner(samples); !errors.Is(err, ErrMalformedSample) {
			t.Errorf("%s: NewAlertScanner err = %v, want ErrMalformedSample", tc.name, err)
		}
	}
}

// TestNewAlertScannerClonesInput pins the aliasing contract: mutating the
// caller's map after construction must not affect a scanner already built.
func TestNewAlertScannerClonesInput(t *testing.T) {
	t.Parallel()

	samples := map[string]float64{"cpu": 90}
	s, err := NewAlertScanner(samples)
	if err != nil {
		t.Fatalf("NewAlertScanner: %v", err)
	}
	samples["cpu"] = 0
	samples["mem"] = 99

	var got []string
	for name := range s.Above(50) {
		got = append(got, name)
	}
	if want := []string{"cpu"}; !slices.Equal(got, want) {
		t.Fatalf("mutating the caller's map changed the scanner: got %v, want %v", got, want)
	}
}

// TestIgnoringStopPanicsOnEarlyBreak is the heart of the module: a hand
// written iter.Seq2 that calls yield without checking its return value
// keeps producing values after the consumer has already stopped, and Go's
// range-over-func runtime check catches exactly that misuse -- the same
// early break that Above (below) handles silently and correctly.
func TestIgnoringStopPanicsOnEarlyBreak(t *testing.T) {
	t.Parallel()

	samples := map[string]float64{"a": 1, "b": 2, "c": 3, "d": 4}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		for range aboveIgnoringStop(samples, 0) {
			break // consumer wants only the first match
		}
	}()
	if recovered == nil {
		t.Fatal("aboveIgnoringStop: want a panic on early break, got none")
	}
	const want = "range function continued iteration after function for loop body returned false"
	if !strings.Contains(recovered.(error).Error(), want) {
		t.Fatalf("panic value = %v, want it to contain %q", recovered, want)
	}

	s, err := NewAlertScanner(samples)
	if err != nil {
		t.Fatalf("NewAlertScanner: %v", err)
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Above panicked on early stop: %v", r)
			}
		}()
		for range s.Above(0) {
			break
		}
	}()
}

func TestConcurrentAboveReads(t *testing.T) {
	t.Parallel()

	s, err := NewAlertScanner(map[string]float64{"a": 1, "b": 5, "c": 10, "d": 3})
	if err != nil {
		t.Fatalf("NewAlertScanner: %v", err)
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var got []string
			for name := range s.Above(2) {
				got = append(got, name)
			}
			if want := []string{"b", "c", "d"}; !slices.Equal(got, want) {
				t.Errorf("Above names = %v, want %v", got, want)
			}
		}()
	}
	wg.Wait()
}

func TestRun(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		args           []string
		stdin          string
		stdout, stderr string
		wantErr        bool
	}{
		{
			name: "stops after limit matches", args: []string{"-threshold", "2", "-limit", "2"},
			stdin: "a 1\nb 5\nc 10\nd 3\n", stdout: "b 5\nc 10\n", stderr: "alerts: 2\n",
		},
		{
			name: "no matches is not an error", args: []string{"-threshold", "100"},
			stdin: "a 1\nb 2\n", stdout: "", stderr: "alerts: 0\n",
		},
		{name: "wrong field count", args: []string{"-threshold", "0"}, stdin: "a 1\nb\n", wantErr: true},
		{name: "unparseable value", args: []string{"-threshold", "0"}, stdin: "a x\n", wantErr: true},
		{name: "NaN value", args: []string{"-threshold", "0"}, stdin: "a nan\n", wantErr: true},
		{name: "limit below one", args: []string{"-limit", "0"}, stdin: "a 1\n", wantErr: true},
		{name: "unknown flag", args: []string{"-bogus"}, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.stdin), &stdout, &stderr)
			if tc.wantErr {
				if !errors.Is(err, errUsage) {
					t.Fatalf("run error = %v, want it to wrap errUsage", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if stdout.String() != tc.stdout {
				t.Fatalf("stdout = %q, want %q", stdout.String(), tc.stdout)
			}
			if stderr.String() != tc.stderr {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tc.stderr)
			}
		})
	}
}
```

## Review

`Above` is correct when a caller can `break` out of it at any point without
consequence, and the module's central test proves that by proving the
opposite first: `aboveIgnoringStop`, differing from `Above` by exactly one
missing `if !yield(...) { return }`, panics with Go's own range-over-func
runtime message the instant a consumer stops early. The mechanism worth
internalizing: `maps.Keys`/`maps.Values`/`maps.All` make this someone
else's problem, but wrapping one of them in your own `iter.Seq`/`iter.Seq2`
-- a filter, a `take`, any combinator -- makes the obligation to stop
calling `yield` after `false` yours, and Go checks it at runtime, not
compile time, so a violation only surfaces when the early exit is actually
exercised, in a test or in production. Around that core, `NewAlertScanner`
clones its input and rejects a NaN or infinite sample with
`ErrMalformedSample`, `Above` visits samples in sorted order for a
reproducible scan, and `AlertScanner` is safe to share across goroutines
because it is immutable after construction. `run` keeps that logic separate
from `os.Args`/`os.Exit`, mapping every usage mistake to exit code 2 and
reserving 1 for a genuine stdin read failure. Run `go test -count=1 -race
./...` to confirm the table, the aliasing contract, the panic, and `run`'s
end-to-end behavior.

## Resources

- [`iter.Seq2`](https://pkg.go.dev/iter#Seq2) — the type `Above` returns, and the type any hand-written map combinator must implement.
- [The Go Blog: Range over Function Types](https://go.dev/blog/range-functions) — the range-over-func contract, including the runtime check this module pins.
- [`maps.Keys`](https://pkg.go.dev/maps#Keys), [`slices.Sorted`](https://pkg.go.dev/slices#Sorted), [`math.IsNaN`](https://pkg.go.dev/math#IsNaN) — the deterministic scan order and the malformed-value check `Above` relies on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-feature-flag-changeset-clone-and-swap.md](18-feature-flag-changeset-clone-and-swap.md) | Next: [../11-slice-memory-leaks/00-concepts.md](../11-slice-memory-leaks/00-concepts.md)
