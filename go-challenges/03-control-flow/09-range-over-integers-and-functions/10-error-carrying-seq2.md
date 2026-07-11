# Exercise 10: Streaming Decoder — Propagate Errors Mid-Iteration with `iter.Seq2[T, error]`

The single most-asked range-over-func question is: how does an iterator surface an
error? This exercise answers it. `DecodeRecords` streams parsed records from a
CSV-shaped reader as an `iter.Seq2[Record, error]`; on the first decode failure it
yields a zero record with the error and stops. The canonical consumer checks
`err` each step and returns on the first non-nil — and we contrast this per-item
idiom against the captured-`*error` idiom used earlier for terminal scan errors.

## What you'll build

```text
decode/                   independent module: example.com/decode
  go.mod                  module example.com/decode
  decode.go               Record, ErrMalformed, DecodeRecords, Load
  cmd/
    demo/
      main.go             runnable demo: decode a CSV blob with a bad line
  decode_test.go          clean, mid-stream error, stop-after-error, empty tests
```

Files: `decode.go`, `cmd/demo/main.go`, `decode_test.go`.
Implement: `DecodeRecords(r io.Reader) iter.Seq2[Record, error]` yielding `(record, nil)` per good line and `(zero, err)` then stop on the first bad line, plus the canonical `Load` consumer.
Test: clean input yields all with nil errors; a bad line at position k yields k good then one error and no more; the wrapped error matches `ErrMalformed` via `errors.Is`; an empty reader yields nothing.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/decode/cmd/demo
cd ~/go-exercises/decode
go mod init example.com/decode
```

## The design

`DecodeRecords` returns an `iter.Seq2[Record, error]`. It scans the reader line by
line with a `bufio.Scanner`, parses each line, and yields `(record, nil)` for a
good line. On a parse failure it yields `(Record{}, err)` — a zero value paired
with the error — and then returns unconditionally, so iteration stops at the first
bad record. The error is wrapped with `fmt.Errorf("...: %w", err)` around the
package sentinel `ErrMalformed`, so a consumer can identify the failure class with
`errors.Is(err, ErrMalformed)` while still seeing the line number in the message.

The canonical consumer is the point of the whole exercise:

```go
for rec, err := range DecodeRecords(r) {
	if err != nil {
		return err
	}
	// use rec
}
```

The consumer checks `err` on every step and returns on the first non-nil, which
propagates as `yield` returning `false` — though here the decoder has already
decided to stop after yielding the error, so it stops regardless. `Load` is that
consumer packaged as a function returning `([]Record, error)`.

Why `Seq2[T, error]` here and a captured `*error` in the repository exercise? The
distinction is whether the failure is *per-item* or *terminal*. A decode error
belongs to a specific record and should halt iteration immediately, so pairing it
with the value and checking each step is natural. A `database/sql` scan error is
terminal — the cursor as a whole failed — so surfacing it once after the loop via
`rows.Err()` (the captured-`*error` idiom) fits better. Neither is universally
correct; pick by the shape of the failure.

Create `decode.go`:

```go
package decode

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"
)

// ErrMalformed is the sentinel wrapped by every decode failure.
var ErrMalformed = errors.New("malformed record")

// Record is one decoded row.
type Record struct {
	ID   int
	Name string
}

func parseRecord(line string) (Record, error) {
	parts := strings.Split(line, ",")
	if len(parts) != 2 {
		return Record{}, fmt.Errorf("%w: want 2 fields, got %d", ErrMalformed, len(parts))
	}
	id, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return Record{}, fmt.Errorf("%w: bad id %q", ErrMalformed, parts[0])
	}
	return Record{ID: id, Name: strings.TrimSpace(parts[1])}, nil
}

// DecodeRecords streams parsed records. It yields (record, nil) per good line
// and, on the first decode failure, (zero, err) and then stops.
func DecodeRecords(r io.Reader) iter.Seq2[Record, error] {
	return func(yield func(Record, error) bool) {
		sc := bufio.NewScanner(r)
		line := 0
		for sc.Scan() {
			line++
			rec, err := parseRecord(sc.Text())
			if err != nil {
				yield(Record{}, fmt.Errorf("line %d: %w", line, err))
				return
			}
			if !yield(rec, nil) {
				return
			}
		}
		if err := sc.Err(); err != nil {
			yield(Record{}, err)
		}
	}
}

// Load is the canonical consumer: collect records, return on the first error.
func Load(r io.Reader) ([]Record, error) {
	var out []Record
	for rec, err := range DecodeRecords(r) {
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}
```

## Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/decode"
)

func main() {
	blob := "1,alice\n2,bob\nthree,carol\n4,dave\n"

	for rec, err := range decode.DecodeRecords(strings.NewReader(blob)) {
		if err != nil {
			fmt.Println("stop:", err)
			break
		}
		fmt.Printf("record %d: %s\n", rec.ID, rec.Name)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
record 1: alice
record 2: bob
stop: line 3: malformed record: bad id "three"
```

## Tests

Create `decode_test.go`:

```go
package decode

import (
	"errors"
	"strings"
	"testing"
)

func TestCleanInput(t *testing.T) {
	t.Parallel()

	recs, err := Load(strings.NewReader("1,alice\n2,bob\n3,carol\n"))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(recs) != 3 || recs[0].Name != "alice" || recs[2].ID != 3 {
		t.Fatalf("recs = %+v", recs)
	}
}

func TestErrorMidStreamStops(t *testing.T) {
	t.Parallel()

	var good []Record
	var errs []error
	for rec, err := range DecodeRecords(strings.NewReader("1,a\n2,b\nbad\n3,c\n")) {
		if err != nil {
			errs = append(errs, err)
			continue // keep ranging to prove the decoder yields no more
		}
		good = append(good, rec)
	}

	if len(good) != 2 {
		t.Fatalf("good = %+v, want 2 records before the error", good)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly one error then stop", errs)
	}
	if !errors.Is(errs[0], ErrMalformed) {
		t.Fatalf("err = %v, want wrapped ErrMalformed", errs[0])
	}
}

func TestConsumerReturnsOnFirstError(t *testing.T) {
	t.Parallel()

	_, err := Load(strings.NewReader("1,a\nnope\n2,b\n"))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want wrapped ErrMalformed", err)
	}
}

func TestEmptyReaderYieldsNothing(t *testing.T) {
	t.Parallel()

	count := 0
	for range DecodeRecords(strings.NewReader("")) {
		count++
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
}
```

## Review

The decoder is correct when it yields every good record with a nil error, and on
the first malformed line yields exactly one `(zero, err)` and then stops — the
mid-stream test keeps ranging after the error and asserts no further yields, which
holds because the decoder `return`s right after yielding the error. The error is
wrapped with `%w` around `ErrMalformed`, so `errors.Is` recovers the class while
the message still carries the line number. This `Seq2[T, error]` idiom fits
per-item failures that should halt iteration; the captured-`*error` idiom from the
repository exercise fits terminal scan errors. Choosing between them by the shape
of the failure is the real skill this exercise teaches.

## Resources

- [`iter` package documentation (Seq2)](https://pkg.go.dev/iter#Seq2)
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner)
- [`errors.Is` and `%w` wrapping](https://pkg.go.dev/errors#Is)
- [Go blog: Range Over Function Types](https://go.dev/blog/range-functions)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-dedup-idempotency-combinator.md](09-dedup-idempotency-combinator.md) | Next: [11-sharded-request-id-generator.md](11-sharded-request-id-generator.md)
