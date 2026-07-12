# Exercise 13: A WAL Record Reader That Never Materializes the Whole Log

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every storage engine with a write-ahead log -- PostgreSQL's WAL, etcd's WAL,
BoltDB's freelist replay -- has a component that walks a log segment record by
record during startup replay, crash recovery, or a debugging tool that just
wants to inspect the first few entries. The obvious Go signature for "give me
the records" is `func ReadAll(r io.Reader) ([]Record, error)`, and it is
exactly wrong for this job: it forces the *entire* segment to be decoded and
held in memory before the function can return anything at all, even when the
caller -- a recovery routine looking only for the last checkpoint, an
operator's `waldump -n 5` -- only ever touches the first handful of records. A
production WAL segment is commonly tens to hundreds of megabytes; decoding all
of it to answer a question about its first kilobyte is the same shape of waste
this lesson has been fighting throughout, just moved from "one small reference
pins one large backing array" to "one small request forces one large
collection to exist at all."

Go 1.23's `iter.Seq` is the structural fix, not a workaround. A function that
returns `iter.Seq[Record]` decodes one record, hands it to the caller's loop
body, and only decodes the next one if the loop asks for it by not breaking.
`range` over such a function, and a `break` inside that range, tells the
iterator to stop pulling bytes off the underlying reader immediately -- the
records after the break point are never allocated, because they are never
even parsed. Nothing is retained that was never created in the first place;
this is the same reachability principle as the rest of the lesson, expressed
as "don't build it" rather than "let go of it."

This module builds `Reader`, a length-prefixed WAL record decoder whose
`Records` method returns an `iter.Seq[Record]`. The full-materialization
signature is not part of that API: it exists only in the test file, as the
antipattern the tests measure the cost of.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
walseq/                   module example.com/walseq
  go.mod                  go 1.24
  walseq.go               Record, Reader; NewReader, Records, Err; three sentinel errors
  walseq_test.go          decode table, boundary/error table, early-break behavior, the
                          full-materialization contrast via MemStats, ExampleReader_Records
```

- Files: `walseq.go`, `walseq_test.go`.
- Implement: `NewReader(r io.Reader, maxSize int) (*Reader, error)` rejecting a non-positive `maxSize` with `ErrInvalidMaxSize`; `(*Reader).Records() iter.Seq[Record]` decoding one 4-byte-length-prefixed record at a time and yielding it, stopping cleanly at EOF, stopping with `ErrTruncatedRecord` on a short length prefix or payload, and with `ErrRecordTooLarge` when a declared length exceeds `maxSize`; `(*Reader).Err() error` reporting the first decode error after iteration.
- Test: in-order decoding with correct offsets; a boundary table covering an empty log, a truncated length prefix, a truncated payload, and an oversized record; that a `break` mid-range stops decoding early with `Err() == nil`; the full-materialization contrast -- a naive `[]Record`-returning reader pays for the whole log regardless of what the caller needed, `Records` with an early break does not; and `ExampleReader_Records` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/11-slice-memory-leaks/13-wal-lazy-iter-seq-reader
cd go-solutions/06-collections-arrays-slices-and-maps/11-slice-memory-leaks/13-wal-lazy-iter-seq-reader
go mod edit -go=1.24
```

### Returning a collection forces the collection to exist

Every other module in this lesson is about severing a pin on data that
already exists: cloning a sub-slice, zeroing a vacated tail, rebuilding a
map. `iter.Seq` addresses an earlier point in the same failure's timeline --
whether the data gets materialized as a collection at all. Compare the two
possible shapes of a WAL reader:

```go
// The trap: the signature itself forces full materialization.
func ReadAll(r io.Reader, maxSize int) ([]Record, error) {
    var all []Record
    for { /* decode one record */; all = append(all, rec) }
    return all, nil   // every caller pays for every record, every time
}
```

There is no bug in this loop -- it decodes correctly. The cost is structural:
a caller that wants only the WAL's first checkpoint record still waits for
`ReadAll` to decode, allocate, and append every record in the segment, because
the function cannot return anything until the loop finishes. `iter.Seq` moves
the loop *into* the caller's control:

```go
func (rd *Reader) Records() iter.Seq[Record] {
    return func(yield func(Record) bool) {
        for { /* decode one record */; if !yield(rec) { return } }
    }
}
```

`yield` is called once per record, and its return value is the caller's
answer to "keep going?" -- `false` on `break`. The decode loop lives inside
`Records`, but control genuinely passes back to the caller's loop body
between records, and a `break` there propagates directly into a `return`
inside the iterator, ending the underlying reads for good. A caller who
writes `for rec := range rd.Records() { ...; break }` never pays for, or
holds in memory, a single byte past what it looked at.

Create `walseq.go`:

```go
// Package walseq reads length-prefixed write-ahead-log records lazily via
// an iter.Seq, the pattern etcd's WAL reader and PostgreSQL's WAL replay
// use to process gigabyte-scale log segments without materializing them.
//
// Records never builds a []Record for the whole log: each record is
// decoded, yielded, and forgotten before the next is even read off the
// underlying io.Reader. A caller who only needs the first few records --
// or who filters and stops early with break -- never pays for, or holds in
// memory, records it did not consume.
package walseq

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"iter"
)

var (
	// ErrInvalidMaxSize means NewReader was called with a non-positive
	// maxSize.
	ErrInvalidMaxSize = errors.New("walseq: maxSize must be positive")
	// ErrTruncatedRecord means the log ended (or an underlying read failed)
	// partway through a record's length prefix or payload.
	ErrTruncatedRecord = errors.New("walseq: truncated record")
	// ErrRecordTooLarge means a record's declared length exceeded the
	// Reader's configured maxSize.
	ErrRecordTooLarge = errors.New("walseq: record exceeds max size")
)

// Record is one decoded WAL entry.
type Record struct {
	// Offset is the byte position of this record's length prefix within
	// the log, counting from the start of the segment.
	Offset int64
	// Payload is this record's decoded body. It is freshly allocated per
	// record and never aliases Reader's internal buffer.
	Payload []byte
}

// Reader decodes length-prefixed records -- a 4-byte big-endian length
// followed by that many payload bytes -- from an underlying WAL segment.
//
// Reader is not safe for concurrent use: a caller must not call Records
// from multiple goroutines on the same Reader, nor start a new iteration
// while a previous one is still in progress.
type Reader struct {
	r       *bufio.Reader
	maxSize int
	err     error
}

// NewReader returns a Reader over r that rejects any record whose declared
// length exceeds maxSize. It returns ErrInvalidMaxSize if maxSize is not
// positive.
func NewReader(r io.Reader, maxSize int) (*Reader, error) {
	if maxSize <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidMaxSize, maxSize)
	}
	return &Reader{r: bufio.NewReader(r), maxSize: maxSize}, nil
}

// Records returns an iterator over the log's records, in order.
//
// Records decodes one record at a time and yields it immediately; it never
// reads ahead of what the caller consumes. If the range loop breaks, the
// iterator stops reading right there and every not-yet-decoded record is
// never allocated at all -- the caller pays only for what it consumed.
//
// If decoding fails partway through, the iterator stops and the error is
// available afterward from Err; range-over-func has no channel for
// mid-loop errors, so this mirrors the bufio.Scanner Scan/Err convention.
func (rd *Reader) Records() iter.Seq[Record] {
	return func(yield func(Record) bool) {
		var offset int64
		for {
			var lenBuf [4]byte
			n, err := io.ReadFull(rd.r, lenBuf[:])
			if err == io.EOF && n == 0 {
				return // clean end of log
			}
			if err != nil {
				rd.err = fmt.Errorf("%w: length prefix at offset %d: %v", ErrTruncatedRecord, offset, err)
				return
			}
			length := int(binary.BigEndian.Uint32(lenBuf[:]))
			if length > rd.maxSize {
				rd.err = fmt.Errorf("%w: record at offset %d declares %d bytes, max %d", ErrRecordTooLarge, offset, length, rd.maxSize)
				return
			}
			payload := make([]byte, length)
			if _, err := io.ReadFull(rd.r, payload); err != nil {
				rd.err = fmt.Errorf("%w: payload at offset %d: %v", ErrTruncatedRecord, offset, err)
				return
			}
			rec := Record{Offset: offset, Payload: payload}
			offset += int64(4 + length)
			if !yield(rec) {
				return
			}
		}
	}
}

// Err returns the first error encountered while iterating Records, or nil
// if the most recent iteration reached a clean end of log, stopped early
// via break with no decode error, or has not run yet.
func (rd *Reader) Err() error { return rd.err }
```

### Using it

Construct one `Reader` per segment with a `maxSize` bounding how large a
single record's declared length may be (a corruption or framing guard, the
same idea as the framing window elsewhere in this lesson), then `range` over
`Records()`. Check `Err()` after the loop: a `nil` result means either a
clean end of log or an intentional early `break`, and a non-nil result means
decoding stopped because of `ErrTruncatedRecord` or `ErrRecordTooLarge`.

Each yielded `Record.Payload` is freshly allocated and does not alias
`Reader`'s internal `bufio.Reader` buffer, so the caller may retain it past
the current loop iteration without a defensive copy -- the aliasing
discipline the rest of this lesson drills into every module is already
handled inside `Records` itself.

`ExampleReader_Records` is the runnable demonstration of this module: `go
test` runs it and compares its standard output against the `// Output:`
comment, so the usage shown below cannot drift from the code.

```go
func ExampleReader_Records() {
	var buf bytes.Buffer
	appendRecord(&buf, []byte("checkpoint"))
	appendRecord(&buf, []byte("put key=a val=1"))
	appendRecord(&buf, []byte("put key=b val=2"))

	rd, err := NewReader(&buf, 4096)
	if err != nil {
		panic(err)
	}

	for rec := range rd.Records() {
		fmt.Printf("offset=%d %q\n", rec.Offset, rec.Payload)
	}
	if err := rd.Err(); err != nil {
		panic(err)
	}
	fmt.Println("clean end of log")

	// Output:
	// offset=0 "checkpoint"
	// offset=14 "put key=a val=1"
	// offset=33 "put key=b val=2"
	// clean end of log
}
```

(`appendRecord`, used above and throughout the tests, writes one
length-prefixed record to a `*bytes.Buffer` -- the inverse of what `Records`
decodes; see `walseq_test.go` below.)

### Tests

`TestNewReaderRejectsNonPositiveMaxSize` pins the constructor's validation.
`TestRecordsDecodesInOrder` checks payload content and offset arithmetic
across three records, including an empty-payload record, and that
`Payload` is never nil even when a record is zero-length.
`TestRecordsEdgeCasesAndErrors` is the boundary table: an empty log (zero
records, no error), a length prefix cut short, a payload cut short, and a
declared length exceeding `maxSize` -- the last three all confirmed to yield
no records before `Err()` reports the matching sentinel.
`TestRecordsBreakStopsReadingEarly` confirms a `break` after three records
out of ten leaves `Err()` nil, distinguishing "the caller stopped" from "the
decoder failed."

`TestNaiveReadAllMaterializesWholeLog` and
`TestRecordsEarlyBreakAvoidsMaterializingWholeLog` are the heart of the
module, reusing the `runtime.ReadMemStats` discipline from Exercise 2: force
GC twice, read `HeapAlloc`, compare a 4 MiB log's (500 records, 8 KiB each)
delta against a 2 MiB threshold. `readAllNaive` is an unexported test helper
that decodes through the same `Records` iterator but appends every result to
a slice before returning -- never exported, never reachable from the package
API -- and its test shows the heap growing with the whole log regardless of
how many records the caller actually wanted. The second test reads the
identical log through `Records` directly and breaks after two records,
showing the heap staying far below the threshold. Both tests hold the source
log alive with `runtime.KeepAlive` across the "after" reading so the log's
own bytes -- already counted in the baseline -- are not coincidentally
collected mid-measurement and skew the result. Neither test calls
`t.Parallel`: `HeapAlloc` is process-global, so a concurrently allocating
test would perturb the reading.

`Reader` is documented as not safe for concurrent use (its own decode
position is unsynchronized mutable state), so this module has no
concurrency test -- per this lesson's convention, that test exists only for
types that declare the concurrency-safe contract.

Create `walseq_test.go`:

```go
package walseq

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"testing"
)

// readHeap returns HeapAlloc after two full GC cycles (the second completes
// the sweep the first starts), so the reading is stable.
func readHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// appendRecord writes one length-prefixed record to buf, matching the wire
// format Reader.Records decodes.
func appendRecord(buf *bytes.Buffer, payload []byte) {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload)))
	buf.Write(lenBuf[:])
	buf.Write(payload)
}

// buildLog returns n concatenated records of recSize zero bytes each, the
// synthetic log both memory tests below read.
func buildLog(n, recSize int) []byte {
	var buf bytes.Buffer
	for range n {
		appendRecord(&buf, make([]byte, recSize))
	}
	return buf.Bytes()
}

func TestNewReaderRejectsNonPositiveMaxSize(t *testing.T) {
	t.Parallel()

	for _, m := range []int{0, -1} {
		if _, err := NewReader(bytes.NewReader(nil), m); !errors.Is(err, ErrInvalidMaxSize) {
			t.Errorf("NewReader(maxSize=%d) error = %v, want ErrInvalidMaxSize", m, err)
		}
	}
}

func TestRecordsDecodesInOrder(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	appendRecord(&buf, []byte("first"))
	appendRecord(&buf, []byte("second"))
	appendRecord(&buf, []byte(""))

	rd, err := NewReader(&buf, 1<<20)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	var got []Record
	for rec := range rd.Records() {
		got = append(got, rec)
	}
	if err := rd.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}

	want := []string{"first", "second", ""}
	if len(got) != len(want) {
		t.Fatalf("decoded %d records, want %d: %+v", len(got), len(want), got)
	}
	var offset int64
	for i, w := range want {
		if string(got[i].Payload) != w || got[i].Offset != offset || got[i].Payload == nil {
			t.Fatalf("record %d = %+v, want payload %q at offset %d (non-nil)", i, got[i], w, offset)
		}
		offset += int64(4 + len(w))
	}
}

// TestRecordsEdgeCasesAndErrors is the boundary table: an empty log yields
// zero records and no error, a length prefix cut short and a payload cut
// short both wrap ErrTruncatedRecord, and a record whose declared length
// exceeds maxSize wraps ErrRecordTooLarge -- in every failing case, no
// record is yielded before the error.
func TestRecordsEdgeCasesAndErrors(t *testing.T) {
	t.Parallel()

	truncatedPayload := func() []byte {
		var b bytes.Buffer
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], 10) // declares 10 bytes
		b.Write(lenBuf[:])
		b.WriteString("short") // only 5 delivered
		return b.Bytes()
	}
	oversized := func() []byte {
		var b bytes.Buffer
		appendRecord(&b, make([]byte, 100))
		return b.Bytes()
	}

	tests := []struct {
		name    string
		data    []byte
		maxSize int
		wantErr error
	}{
		{name: "empty log", data: nil, maxSize: 1024, wantErr: nil},
		{name: "truncated length prefix", data: []byte{0x00, 0x00}, maxSize: 1024, wantErr: ErrTruncatedRecord},
		{name: "truncated payload", data: truncatedPayload(), maxSize: 1024, wantErr: ErrTruncatedRecord},
		{name: "oversized record", data: oversized(), maxSize: 10, wantErr: ErrRecordTooLarge},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rd, err := NewReader(bytes.NewReader(tc.data), tc.maxSize)
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			count := 0
			for range rd.Records() {
				count++
			}
			if tc.wantErr == nil {
				if count != 0 || rd.Err() != nil {
					t.Fatalf("count=%d err=%v, want 0 records and nil error", count, rd.Err())
				}
				return
			}
			if count != 0 {
				t.Fatalf("count=%d, want 0 records before an error", count)
			}
			if !errors.Is(rd.Err(), tc.wantErr) {
				t.Fatalf("Err() = %v, want %v", rd.Err(), tc.wantErr)
			}
		})
	}
}

func TestRecordsBreakStopsReadingEarly(t *testing.T) {
	t.Parallel()

	rd, err := NewReader(bytes.NewReader(buildLog(10, 8)), 1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	var got []Record
	for rec := range rd.Records() {
		got = append(got, rec)
		if len(got) == 3 {
			break
		}
	}
	if len(got) != 3 {
		t.Fatalf("decoded %d records before break, want 3", len(got))
	}
	if err := rd.Err(); err != nil {
		t.Fatalf("Err() after break = %v, want nil (break is not a decode error)", err)
	}
}

// readAllNaive is the antipattern this module exists to avoid: it decodes
// exactly the same wire format, record by record, but its signature forces
// full materialization -- every call holds every record in memory before
// returning anything to the caller, no matter how many the caller actually
// wants. It is never exported and never reachable from the package API.
func readAllNaive(r *bytes.Reader, maxSize int) ([]Record, error) {
	rd, err := NewReader(r, maxSize)
	if err != nil {
		return nil, err
	}
	var all []Record
	for rec := range rd.Records() {
		all = append(all, rec)
	}
	return all, rd.Err()
}

// TestNaiveReadAllMaterializesWholeLog is the core of this module: a
// caller that wants only the first couple of records out of a large log
// still pays, with readAllNaive, for decoding and holding every record.
//
// This test deliberately does not call t.Parallel: it forces GC and reads
// process-global heap stats, which a concurrently allocating goroutine
// would perturb.
func TestNaiveReadAllMaterializesWholeLog(t *testing.T) {
	const n = 500
	const recSize = 8 << 10 // 8 KiB per record, ~4 MiB total
	half := int64(n*recSize) / 2
	data := buildLog(n, recSize)

	base := readHeap()
	all, err := readAllNaive(bytes.NewReader(data), 1<<20)
	if err != nil {
		t.Fatalf("readAllNaive: %v", err)
	}
	if len(all) != n {
		t.Fatalf("readAllNaive decoded %d records, want %d", len(all), n)
	}
	after := readHeap()
	runtime.KeepAlive(data) // keep the source log counted in base through the "after" read
	if delta := int64(after) - int64(base); delta < half {
		t.Fatalf("readAllNaive did not materialize the whole log: delta %d bytes, want >= %d", delta, half)
	}
	runtime.KeepAlive(all)
}

// TestRecordsEarlyBreakAvoidsMaterializingWholeLog is the fix, measured the
// same way: reading the identical log but stopping after 2 records via
// Records leaves the heap far below the naive full-decode footprint.
func TestRecordsEarlyBreakAvoidsMaterializingWholeLog(t *testing.T) {
	const n = 500
	const recSize = 8 << 10
	half := int64(n*recSize) / 2
	data := buildLog(n, recSize)

	base := readHeap()
	rd, err := NewReader(bytes.NewReader(data), 1<<20)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	var got []Record
	for rec := range rd.Records() {
		got = append(got, rec)
		if len(got) == 2 {
			break
		}
	}
	after := readHeap()
	runtime.KeepAlive(data) // keep the source log counted in base through the "after" read
	if delta := int64(after) - int64(base); delta >= half {
		t.Fatalf("early break still materialized the whole log: delta %d bytes, want < %d", delta, half)
	}
	runtime.KeepAlive(got)
}

// ExampleReader_Records is the runnable demonstration of this module: go
// test executes it and compares its stdout against the Output comment.
func ExampleReader_Records() {
	var buf bytes.Buffer
	appendRecord(&buf, []byte("checkpoint"))
	appendRecord(&buf, []byte("put key=a val=1"))
	appendRecord(&buf, []byte("put key=b val=2"))

	rd, err := NewReader(&buf, 4096)
	if err != nil {
		panic(err)
	}

	for rec := range rd.Records() {
		fmt.Printf("offset=%d %q\n", rec.Offset, rec.Payload)
	}
	if err := rd.Err(); err != nil {
		panic(err)
	}
	fmt.Println("clean end of log")

	// Output:
	// offset=0 "checkpoint"
	// offset=14 "put key=a val=1"
	// offset=33 "put key=b val=2"
	// clean end of log
}
```

## Review

`Reader` is correct when `Records` decodes every well-formed record in order
with the right offsets -- `TestRecordsDecodesInOrder` pins that -- and stops
cleanly with the right sentinel on every malformed input the boundary table
covers. The mechanism worth internalizing is that `iter.Seq` moves the
decision of "how much to materialize" from the function that produces data to
the loop that consumes it: a naive `[]Record`-returning reader cannot return
anything until it has decoded everything, while `Records` decodes exactly as
far as the caller's `range` loop asks it to and no further.
`TestNaiveReadAllMaterializesWholeLog` and
`TestRecordsEarlyBreakAvoidsMaterializingWholeLog` measure that difference
directly with a `runtime.ReadMemStats` delta rather than asserting it by
inspection. `ErrInvalidMaxSize`, `ErrTruncatedRecord`, and
`ErrRecordTooLarge` are all checkable with `errors.Is`, and `Err()` after a
`range` loop distinguishes an intentional `break` (nil) from a genuine decode
failure (non-nil). Run `go test -count=1 -race ./...`.

## Resources

- [`iter.Seq`](https://pkg.go.dev/iter#Seq) — the range-over-func iterator type `Records` returns.
- [Go blog: Range over Function Types](https://go.dev/blog/range-functions) — how `break` inside a range loop maps to the iterator's `yield` returning `false`.
- [`bufio.Scanner.Err`](https://pkg.go.dev/bufio#Scanner.Err) — the Scan/Err convention `Reader.Err` mirrors for reporting a mid-iteration failure.
- [`runtime.MemStats` and `ReadMemStats`](https://pkg.go.dev/runtime#MemStats) — the leak-detection technique this module's core test reuses from Exercise 2.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-tls-cert-atomic-swap-reachability.md](12-tls-cert-atomic-swap-reachability.md) | Next: [14-ndjson-batch-redactor-clear-reuse.md](14-ndjson-batch-redactor-clear-reuse.md)
