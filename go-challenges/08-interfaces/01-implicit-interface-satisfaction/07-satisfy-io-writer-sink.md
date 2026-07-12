# Exercise 7: A Structured Log Sink That Satisfies io.Writer

`io.Writer` is one method — `Write([]byte) (int, error)` — and satisfying it drops
your type into `log.New`, `fmt.Fprintf`, `io.Copy`, and every stdlib API that
writes bytes. This module builds an audit sink that redacts secrets and counts
lines, and because it implements `io.Writer` implicitly, it plugs straight into
the standard logger with no adapter.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests.

## What you'll build

```text
auditsink/                    independent module: example.com/auditsink
  go.mod                      go 1.26
  sink.go                     AuditSink with Write([]byte)(int,error); redaction + line count
  cmd/
    demo/
      main.go                 runnable demo: wire sink into log.New and fmt.Fprintf
  sink_test.go                io.Writer contract; redaction; line count; passed as io.Writer
```

- Files: `sink.go`, `cmd/demo/main.go`, `sink_test.go`.
- Implement: an `AuditSink` whose `Write` redacts a secret token, counts newlines, and buffers the result, honoring the `io.Writer` contract (return `len(p), nil` on success).
- Test: assert redaction and line count, that the sink is assignable to `io.Writer`, and that `Write` returns `len(p)` for the original input.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/01-implicit-interface-satisfaction/07-satisfy-io-writer-sink/cmd/demo
cd go-solutions/08-interfaces/01-implicit-interface-satisfaction/07-satisfy-io-writer-sink
```

### The io.Writer contract and the redaction subtlety

`io.Writer` requires: `Write` must return the number of bytes consumed from `p`
and, if that is less than `len(p)`, a non-nil error explaining why. Crucially, a
`Write` that reports `n == len(p)` must also report `err == nil`, and callers rely
on it: `log`, `bufio.Writer`, and `fmt.Fprintf` treat `n < len(p)` as a short
write and error out.

That contract creates a subtle trap for a *transforming* writer. Our sink redacts a
secret before storing it, so the number of bytes it actually stores differs from
the input length — in either direction, depending on whether the replacement is
shorter or longer than what it replaces (here `[REDACTED]` is longer than
`hunter2`). It must nonetheless return `len(p)` — the number of input bytes it
consumed — not the length of the redacted output. Returning the transformed length
would make `log` think the write was short and error. The rule: report how much of
`p` you *consumed*, which is all of it, regardless of how many bytes you chose to
persist.

`AuditSink.Write` therefore: (1) redacts occurrences of a secret token in a copy of
the input, (2) counts newlines in the input to maintain a running line count,
(3) writes the redacted bytes to an internal `bytes.Buffer`, and (4) returns
`len(p), nil`. It holds a `sync.Mutex` because `log.Logger` may write from multiple
goroutines, and a shared audit sink must be safe for that.

Create `sink.go`:

```go
package auditsink

import (
	"bytes"
	"sync"
)

// AuditSink is an io.Writer that redacts a secret token and counts lines as it
// buffers what is written. Because it satisfies io.Writer, it drops into
// log.New, fmt.Fprintf, and any stdlib API that writes bytes.
type AuditSink struct {
	mu     sync.Mutex
	secret []byte
	buf    bytes.Buffer
	lines  int
}

// NewAuditSink returns a sink that redacts occurrences of secret.
func NewAuditSink(secret string) *AuditSink {
	return &AuditSink{secret: []byte(secret)}
}

// Write redacts the secret, counts newlines, and buffers the result. It honors
// the io.Writer contract: it returns len(p), nil on success even though the
// number of bytes it stores after redaction differs from len(p). Returning the
// stored length would make callers like log.Logger treat the write as short.
func (s *AuditSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lines += bytes.Count(p, []byte("\n"))

	out := p
	if len(s.secret) > 0 {
		out = bytes.ReplaceAll(p, s.secret, []byte("[REDACTED]"))
	}
	s.buf.Write(out)

	return len(p), nil
}

// String returns the redacted, buffered content.
func (s *AuditSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// Lines returns the number of newlines written so far.
func (s *AuditSink) Lines() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lines
}
```

### The runnable demo

Create `cmd/demo/main.go`. The sink goes straight into `log.New` (which wants an
`io.Writer`) and `fmt.Fprintf` (which also wants one) — proof of implicit
satisfaction in real stdlib call sites.

```go
package main

import (
	"fmt"
	"log"

	"example.com/auditsink"
)

func main() {
	sink := auditsink.NewAuditSink("hunter2")

	// log.New accepts an io.Writer; the sink satisfies it implicitly.
	logger := log.New(sink, "", 0)
	logger.Println("login ok user=alice password=hunter2")

	// fmt.Fprintf also accepts an io.Writer.
	fmt.Fprintf(sink, "token refreshed secret=hunter2\n")

	fmt.Print(sink.String())
	fmt.Printf("lines=%d\n", sink.Lines())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
login ok user=alice password=[REDACTED]
token refreshed secret=[REDACTED]
lines=2
```

### Tests

`TestWriteHonorsContract` pins the crucial invariant: `Write` returns `len(p)` for
the *input* even though the stored byte count differs after redaction.
`TestRedaction` checks the secret never lands in the buffer. `TestSatisfiesWriter`
passes the sink where an `io.Writer` is required, proving implicit satisfaction.

Create `sink_test.go`:

```go
package auditsink

import (
	"fmt"
	"io"
	"testing"
)

func TestWriteHonorsContract(t *testing.T) {
	t.Parallel()

	s := NewAuditSink("hunter2")
	in := []byte("password=hunter2\n") // 17 bytes; redacted form is longer, but n must be len(in)

	n, err := s.Write(in)
	if err != nil {
		t.Fatalf("Write err = %v, want nil", err)
	}
	if n != len(in) {
		t.Fatalf("Write returned n = %d, want %d (len of input, not stored bytes)", n, len(in))
	}
}

func TestRedaction(t *testing.T) {
	t.Parallel()

	s := NewAuditSink("hunter2")
	_, _ = s.Write([]byte("secret=hunter2 rest\n"))

	got := s.String()
	want := "secret=[REDACTED] rest\n"
	if got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
	if s.Lines() != 1 {
		t.Fatalf("Lines = %d, want 1", s.Lines())
	}
}

func TestSatisfiesWriter(t *testing.T) {
	t.Parallel()

	// Passed as io.Writer: implicit satisfaction at the call boundary.
	var w io.Writer = NewAuditSink("")
	if _, err := fmt.Fprintf(w, "line one\nline two\n"); err != nil {
		t.Fatalf("Fprintf err = %v", err)
	}
	sink := w.(*AuditSink)
	if sink.Lines() != 2 {
		t.Fatalf("Lines = %d, want 2", sink.Lines())
	}
}

func Example() {
	s := NewAuditSink("hunter2")
	fmt.Fprintf(s, "token=hunter2\n")
	fmt.Print(s.String())
	// Output: token=[REDACTED]
}
```

## Review

The sink is correct when `Write` returns `len(p), nil` on success regardless of how
many bytes it persists — the single most common bug in a transforming `io.Writer`
is returning the *output* length, which callers read as a short write and turn into
an error. `TestWriteHonorsContract` exists precisely to catch that. Redaction
operates on a copy (`bytes.ReplaceAll` allocates a new slice) so the caller's buffer
is never mutated, which matters because `io.Writer` implementations must not retain
or modify `p`. Because the sink is one method, it plugs into `log.New` and
`fmt.Fprintf` with no adapter — that is the whole value of satisfying the smallest
possible stdlib interface. Run `go test -race` to confirm the mutex guards the
buffer under concurrent logger writes.

## Resources

- [`io.Writer`](https://pkg.go.dev/io#Writer) — the one-method contract and what `n`/`err` must mean.
- [`log.New`](https://pkg.go.dev/log#New) — the logger takes any `io.Writer`.
- [`bytes.ReplaceAll`](https://pkg.go.dev/bytes#ReplaceAll) — allocating replacement that does not touch the input.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-satisfy-stdlib-stringer-error-json.md](08-satisfy-stdlib-stringer-error-json.md)
