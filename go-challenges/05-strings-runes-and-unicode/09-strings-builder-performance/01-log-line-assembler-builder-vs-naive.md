# Exercise 1: Build a Request Log-Line Assembler: + Concatenation vs strings.Builder

Every request in a backend service produces at least one log line, assembled from a
level, a timestamp, a message, and a variable number of `key=value` fields. This
exercise builds that assembler twice — once the naive way with `+` in a loop, once
with `strings.Builder` and `Grow` pre-sizing — and proves with a table test that the
two produce byte-identical output, so the next exercise can benchmark equivalent work.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
logline/                     independent module: example.com/logline
  go.mod
  logline.go                 Naive (+ loop) and Builder (strings.Builder + Grow)
  cmd/
    demo/
      main.go                assembles a request log line both ways, prints both
  logline_test.go            equivalence table test + empty-kv pin + Example
```

Files: `logline.go`, `cmd/demo/main.go`, `logline_test.go`.
Implement: `Naive(level, ts, msg string, kv []string) string` and `Builder(level, ts, msg string, kv []string) string`, both joining the header fields plus `key=value` pairs from a flat `kv` slice.
Test: a table test asserting `Naive(...) == Builder(...)` over representative and empty/nil field sets, plus cases pinning the exact empty-kv output.
Verify: `go test -count=1 -race ./...`

### Why two implementations

`Naive` is written the way a hurried engineer reaches for first: accumulate into a
`string` with `+=` inside the loop over the key-value pairs. It is correct and
readable, and for a handful of fields on a cold path it is fine. Its cost is that
every `+=` allocates a fresh string and copies everything accumulated so far, so the
work grows quadratically with the number of fields and the allocator sees one
throwaway string per iteration.

`Builder` writes into a single `strings.Builder`. It calls `Grow` once up front with
an estimate — the exact lengths of the fixed header parts plus a rough `16 bytes per
field` for the pairs — so the backing array is (usually) allocated once. Then each
`WriteString`/`WriteByte` appends into that array with no intermediate string. The
final `String()` hands the bytes out without copying. The estimate does not have to
be right: if it is low the Builder grows on its own, if it is high a little memory is
wasted. Either way the output is identical to `Naive`, which is the property the
test locks down. The two functions must agree exactly — same spaces, same `=`, same
handling of an empty `kv` — or the benchmark in the next exercise would be comparing
different work.

The `kv` slice is flat: `["user","alice","ip","10.0.0.1"]` means `user=alice` and
`ip=10.0.0.1`. We iterate two at a time. A defensive `i+1 < len(kv)` guard makes an
odd-length slice drop the dangling key rather than panic, which is the kind of
robustness a logging helper needs when callers pass fields dynamically.

Create `logline.go`:

```go
package logline

import "strings"

// Naive assembles a log line using string concatenation in a loop. Each += in
// the loop allocates a new string and copies the accumulated bytes, so the cost
// grows quadratically with the number of key=value pairs.
func Naive(level, ts, msg string, kv []string) string {
	s := level + " " + ts + " " + msg
	for i := 0; i+1 < len(kv); i += 2 {
		s += " " + kv[i] + "=" + kv[i+1]
	}
	return s
}

// Builder assembles the same log line using a single strings.Builder. Grow
// pre-sizes the backing array from the known field lengths plus an estimate for
// the pairs, so the buffer is typically allocated once and String() returns it
// with no extra copy.
func Builder(level, ts, msg string, kv []string) string {
	var b strings.Builder
	b.Grow(len(level) + 1 + len(ts) + 1 + len(msg) + len(kv)*16)
	b.WriteString(level)
	b.WriteByte(' ')
	b.WriteString(ts)
	b.WriteByte(' ')
	b.WriteString(msg)
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteByte(' ')
		b.WriteString(kv[i])
		b.WriteByte('=')
		b.WriteString(kv[i+1])
	}
	return b.String()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/logline"
)

func main() {
	kv := []string{"user", "alice", "ip", "10.0.0.1", "status", "200"}
	line := logline.Builder("INFO", "2024-01-15T10:30:00Z", "request handled", kv)
	fmt.Println(line)

	naive := logline.Naive("INFO", "2024-01-15T10:30:00Z", "request handled", kv)
	fmt.Println("identical:", naive == line)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
INFO 2024-01-15T10:30:00Z request handled user=alice ip=10.0.0.1 status=200
identical: true
```

### Tests

The core test is equivalence: for a representative record, for an empty `kv`, for a
`nil` `kv`, and for an odd-length `kv`, `Naive` and `Builder` must return the same
string. Two extra cases pin the exact empty-kv output `"DEBUG ts msg"` so a
regression in either function's header handling is caught precisely rather than only
by the pair.

Create `logline_test.go`:

```go
package logline

import (
	"fmt"
	"testing"
)

func TestNaiveBuilderEquivalent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		level string
		ts    string
		msg   string
		kv    []string
	}{
		{"full", "INFO", "2024-01-15T10:30:00Z", "request handled", []string{"user", "alice", "ip", "10.0.0.1"}},
		{"empty-kv", "DEBUG", "ts", "msg", []string{}},
		{"nil-kv", "DEBUG", "ts", "msg", nil},
		{"odd-kv-drops-tail", "WARN", "ts", "msg", []string{"a", "1", "dangling"}},
		{"many-fields", "ERROR", "t", "m", []string{"a", "1", "b", "2", "c", "3", "d", "4"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := Naive(tc.level, tc.ts, tc.msg, tc.kv)
			b := Builder(tc.level, tc.ts, tc.msg, tc.kv)
			if n != b {
				t.Fatalf("Naive and Builder differ:\n  naive = %q\n  build = %q", n, b)
			}
		})
	}
}

func TestEmptyKVExactOutput(t *testing.T) {
	t.Parallel()

	const want = "DEBUG ts msg"
	if got := Builder("DEBUG", "ts", "msg", nil); got != want {
		t.Fatalf("Builder empty kv = %q, want %q", got, want)
	}
	if got := Naive("DEBUG", "ts", "msg", nil); got != want {
		t.Fatalf("Naive empty kv = %q, want %q", got, want)
	}
}

func ExampleBuilder() {
	fmt.Println(Builder("INFO", "ts", "ok", []string{"code", "200"}))
	// Output: INFO ts ok code=200
}
```

## Review

The assembler is correct when both implementations agree on every input, including
the edge cases: empty `kv`, `nil` `kv`, and an odd-length `kv` that drops its
dangling key. The equivalence test is the real guard — if you later change one
function's spacing or `=` handling and forget the other, the table fails immediately.
The common trap this exercise inoculates against is trusting `Grow`: an engineer
sometimes assumes an exact `Grow` is required for correctness and panics when the
estimate is off. It is only a hint; the two functions produce identical bytes whether
`Grow` over- or under-estimates. The other trap is the `+=` loop itself, which is the
naive baseline the rest of the lesson measures and replaces.

## Resources

- [strings.Builder](https://pkg.go.dev/strings#Builder) — `WriteString`, `WriteByte`, `Grow`, `String`.
- [Effective Go: allocation and strings](https://go.dev/doc/effective_go#allocation_new) — why strings are immutable and what that costs.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-benchmark-assemblers-with-benchmem.md](02-benchmark-assemblers-with-benchmem.md)
