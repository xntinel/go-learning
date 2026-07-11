# Exercise 15: A Resumable Scanner Advances a Named Cursor

A hand-rolled line scanner over `key=value` records needs to hand the caller back
an offset to resume from, so the caller can decode one record at a time — the same
shape `bufio.Scanner` uses internally. Tracking that offset, plus the decoded
record and an error, in three named results is what keeps a naked return legible
even though several exit paths compute different things.

**Nivel: Intermedio** — validacion rapida (un test corto).

## What you'll build

```text
linescanner/                  independent module: example.com/linescanner
  go.mod
  linescanner.go               Record; Next (named next cursor, mixed returns)
  linescanner_test.go           sequential decode, malformed line, end of input
```

- Files: `linescanner.go`, `linescanner_test.go`.
- Implement: `Next(data []byte, pos int) (rec Record, next int, err error)` that decodes one `key=value` line starting at `pos` and reports the offset the next call should resume from, including past a malformed line.
- Test: a sequence of calls decodes records in order, a malformed line still reports a correct `next` offset alongside an error, and reading past the end returns `io.EOF`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/linescanner
cd ~/go-exercises/linescanner
go mod init example.com/linescanner
go mod edit -go=1.24
```

### Naked once the names already say it

Once `rec`, `next`, and `err` all hold the right values for a given exit, a bare
`return` is the clearest way to say so — the reader already knows the three names
from the signature:

```go
i := bytes.IndexByte(line, '=')
if i < 0 {
    err = fmt.Errorf("line at offset %d: missing '='", pos)
    return
}
rec = Record{Key: string(line[:i]), Value: string(line[i+1:])}
return
```

The end-of-input case is the one exception: it returns an explicit
`Record{}, pos, io.EOF` tuple rather than three prior assignments, because a
literal reads more clearly than tracing back through the names for a single-line
guard clause at the top of the function.

Create `linescanner.go`:

```go
package linescanner

import (
	"bytes"
	"fmt"
	"io"
)

// Record is one decoded "key=value" line.
type Record struct {
	Key   string
	Value string
}

// Next decodes one record from data starting at pos and reports next, the
// offset the following call should resume from — the resumable-scanner
// shape used by bufio.Scanner-like readers. next is a named result set on
// every path before any return, including the malformed-line path, so a
// caller can always skip past a bad line and keep scanning from the right
// offset. A naked return is used once rec, next, and err already hold the
// right values for that exit; the end-of-input case returns an explicit
// tuple instead, since io.EOF and a zero Record are easier to read as a
// literal than as three prior assignments a reader must scroll back for.
func Next(data []byte, pos int) (rec Record, next int, err error) {
	if pos >= len(data) {
		return Record{}, pos, io.EOF
	}

	end := bytes.IndexByte(data[pos:], '\n')
	var line []byte
	if end == -1 {
		line = data[pos:]
		next = len(data)
	} else {
		line = data[pos : pos+end]
		next = pos + end + 1
	}

	i := bytes.IndexByte(line, '=')
	if i < 0 {
		err = fmt.Errorf("line at offset %d: missing '='", pos)
		return
	}
	rec = Record{Key: string(line[:i]), Value: string(line[i+1:])}
	return
}
```

### Tests

Create `linescanner_test.go`:

```go
package linescanner

import (
	"errors"
	"io"
	"testing"
)

func TestNext(t *testing.T) {
	t.Parallel()

	data := []byte("a=1\nb=2\nbad-line\nc=3")

	tests := []struct {
		name     string
		pos      int
		wantRec  Record
		wantNext int
		wantErr  error
	}{
		{name: "first record", pos: 0, wantRec: Record{Key: "a", Value: "1"}, wantNext: 4},
		{name: "second record", pos: 4, wantRec: Record{Key: "b", Value: "2"}, wantNext: 8},
		{name: "malformed line still advances", pos: 8, wantRec: Record{}, wantNext: 17},
		{name: "last record with no trailing newline", pos: 17, wantRec: Record{Key: "c", Value: "3"}, wantNext: 20},
		{name: "end of input", pos: 20, wantRec: Record{}, wantNext: 20, wantErr: io.EOF},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec, next, err := Next(data, tt.pos)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			} else if err != nil && tt.name != "malformed line still advances" {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.name == "malformed line still advances" && err == nil {
				t.Fatal("want error for malformed line, got nil")
			}
			if next != tt.wantNext {
				t.Errorf("next = %d, want %d", next, tt.wantNext)
			}
			if rec != tt.wantRec {
				t.Errorf("rec = %+v, want %+v", rec, tt.wantRec)
			}
		})
	}
}
```

Verify: `go test -count=1 ./...`

## Review

The named `next` result is what makes this scanner resumable: every path,
including the malformed-line error, sets it to a correct offset before returning,
so a caller can log the bad line and keep decoding instead of getting stuck. The
mix of naked and explicit returns is deliberate — naked once all three names hold
the right values, explicit at the one guard clause where a literal is clearer than
three assignments to trace back through. The mistake to avoid is computing `next`
only on the success path and leaving it stale on the error path, which would strand
a caller unable to skip past a bad line.

## Resources

- [`bytes.IndexByte`](https://pkg.go.dev/bytes#IndexByte)
- [`bufio.Scanner`](https://pkg.go.dev/bufio#Scanner)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-audit-reason-backfill-guard.md](14-audit-reason-backfill-guard.md) | Next: [16-queue-backpressure-overflow-guard.md](16-queue-backpressure-overflow-guard.md)
