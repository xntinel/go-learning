# Exercise 8: Fanning Writes to a Primary Sink and an Audit Sink

An audit component built on `io.MultiWriter`: writes go to a primary destination
and a secondary audit destination at once. Then you confront `MultiWriter`'s
coupling failure mode and build a best-effort variant that isolates the audit
sink's errors from the primary write path.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
auditfanout/                independent module: example.com/auditfanout
  go.mod                    go 1.26
  auditfanout.go            type Fanout (best-effort); New, Write, AuditErr
  cmd/
    demo/
      main.go               io.MultiWriter fans out and couples; Fanout isolates the audit failure
  auditfanout_test.go       MultiWriter fans+propagates; Fanout isolates audit error, primary byte count
```

- Files: `auditfanout.go`, `cmd/demo/main.go`, `auditfanout_test.go`.
- Implement: a `Fanout` that writes to a primary and, best-effort, to an audit sink; a failing audit sink does not fail the primary write, and its error is captured out of band via `AuditErr()`.
- Test: `io.MultiWriter` lands identical bytes in both sinks and propagates/short-circuits on the first error (documenting the coupling); the best-effort `Fanout` does not fail the primary on an audit failure, captures the audit error, and returns the primary byte count.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/05-interface-composition-and-embedding/08-audit-fanout-multiwriter/cmd/demo
cd go-solutions/08-interfaces/05-interface-composition-and-embedding/08-audit-fanout-multiwriter
```

### What io.MultiWriter does, and where it hurts

`io.MultiWriter(a, b)` returns an `io.Writer` whose `Write` calls `a.Write(p)`
then `b.Write(p)`, in order. It is the canonical fan-out adapter and it is exactly
right for "tee this to stdout and a buffer". But read its contract carefully: it
returns on the *first* error, and if a short write occurs it returns
`io.ErrShortWrite`. Two consequences bite on the hot path. First, if the audit sink
is *slow*, every `Write` blocks on it — the audit backend's latency is added to the
caller's latency. Second, if the audit sink *fails*, the whole `Write` fails, even
though the primary write already succeeded; the caller sees an error and may abort a
request that actually completed. You have coupled request success to the health of a
best-effort audit backend. That is almost never what you want for auditing.

The fix is a small wrapper that inverts the priority: write to the primary and
return *its* result, then attempt the audit write best-effort, recording any audit
error out of band (in a field the observability layer can read) instead of
propagating it. The primary path neither fails nor — in a production version —
waits on the audit sink; here we keep it synchronous for testability and isolate
only the error, and the concepts note points at the async/buffered sink you would
add to also decouple latency.

Create `auditfanout.go`:

```go
package auditfanout

import (
	"io"
	"sync"
)

// Fanout writes each payload to a primary sink and, best-effort, to an audit
// sink. Unlike io.MultiWriter, a failing audit sink does not fail the primary
// write; the audit error is captured out of band for observability.
type Fanout struct {
	primary  io.Writer
	audit    io.Writer
	mu       sync.Mutex
	auditErr error
}

// New returns a Fanout writing primarily to primary and auditing to audit.
func New(primary, audit io.Writer) *Fanout {
	return &Fanout{primary: primary, audit: audit}
}

// Write sends p to the primary sink and returns its result. The audit sink is
// written best-effort: its error is recorded, not propagated.
func (f *Fanout) Write(p []byte) (int, error) {
	n, err := f.primary.Write(p)
	if _, aerr := f.audit.Write(p[:n]); aerr != nil {
		f.mu.Lock()
		f.auditErr = aerr
		f.mu.Unlock()
	}
	return n, err
}

// AuditErr returns the most recent audit-sink error, if any.
func (f *Fanout) AuditErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auditErr
}

// Static assertion: Fanout is an io.Writer.
var _ io.Writer = (*Fanout)(nil)
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"example.com/auditfanout"
)

// failingWriter stands in for a down audit backend.
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) {
	return 0, errors.New("audit backend down")
}

func main() {
	// io.MultiWriter fans out to both sinks identically.
	var primary, audit bytes.Buffer
	mw := io.MultiWriter(&primary, &audit)
	fmt.Fprint(mw, "event-1")
	fmt.Printf("multiwriter: primary=%q audit=%q\n", primary.String(), audit.String())

	// But a failing audit sink fails the whole MultiWriter write.
	_, err := io.MultiWriter(&primary, failingWriter{}).Write([]byte("event-2"))
	fmt.Println("multiwriter error:", err)

	// The best-effort Fanout isolates the audit failure from the primary write.
	primary.Reset()
	f := auditfanout.New(&primary, failingWriter{})
	n, err := f.Write([]byte("event-3"))
	fmt.Printf("fanout: n=%d err=%v primary=%q auditErr=%v\n",
		n, err, primary.String(), f.AuditErr())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
multiwriter: primary="event-1" audit="event-1"
multiwriter error: audit backend down
fanout: n=7 err=<nil> primary="event-3" auditErr=audit backend down
```

### Tests

Create `auditfanout_test.go`:

```go
package auditfanout

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
)

// failWriter always fails, standing in for a broken audit sink.
type failWriter struct{}

func (failWriter) Write(p []byte) (int, error) {
	return 0, errors.New("audit down")
}

func TestMultiWriterFansAndPropagates(t *testing.T) {
	t.Parallel()

	// Fans identical bytes to both sinks.
	var a, b bytes.Buffer
	if _, err := io.MultiWriter(&a, &b).Write([]byte("payload")); err != nil {
		t.Fatalf("MultiWriter write: %v", err)
	}
	if a.String() != "payload" || b.String() != "payload" {
		t.Fatalf("a=%q b=%q, want both payload", a.String(), b.String())
	}

	// Propagates and short-circuits on the first sink error (the coupling).
	var primary bytes.Buffer
	if _, err := io.MultiWriter(&primary, failWriter{}).Write([]byte("x")); err == nil {
		t.Fatal("MultiWriter did not propagate the audit error")
	}
}

func TestFanoutIsolatesAuditFailure(t *testing.T) {
	t.Parallel()
	var primary bytes.Buffer
	f := New(&primary, failWriter{})

	n, err := f.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("primary write failed because of audit sink: %v", err)
	}
	if n != 5 {
		t.Fatalf("n = %d, want 5 (primary byte count)", n)
	}
	if primary.String() != "hello" {
		t.Fatalf("primary = %q, want hello", primary.String())
	}
	if f.AuditErr() == nil {
		t.Fatal("audit error was not captured out of band")
	}
}

func TestFanoutWritesBothWhenHealthy(t *testing.T) {
	t.Parallel()
	var primary, audit bytes.Buffer
	f := New(&primary, &audit)
	if _, err := f.Write([]byte("event")); err != nil {
		t.Fatal(err)
	}
	if primary.String() != "event" || audit.String() != "event" {
		t.Fatalf("primary=%q audit=%q, want both event", primary.String(), audit.String())
	}
	if f.AuditErr() != nil {
		t.Fatalf("unexpected audit error: %v", f.AuditErr())
	}
}

func ExampleFanout() {
	var primary, audit bytes.Buffer
	f := New(&primary, &audit)
	f.Write([]byte("login"))
	fmt.Println(primary.String(), audit.String())
	// Output: login login
}
```

## Review

The contrast is the whole lesson. `TestMultiWriterFansAndPropagates` documents that
`io.MultiWriter` couples the caller to every sink: a failing audit writer fails the
whole write. `TestFanoutIsolatesAuditFailure` proves the best-effort wrapper breaks
that coupling — the primary write succeeds and returns its own byte count, while the
audit error is captured in `AuditErr()` for the observability layer rather than
propagated. The mistake to avoid is reaching for `io.MultiWriter` on a request hot
path with a remote or fragile secondary sink; keep the primary write's success and
latency independent of the audit backend, and in production move the audit write to
a buffered/async sink so latency is decoupled too, not just errors.

## Resources

- [`io.MultiWriter`](https://pkg.go.dev/io#MultiWriter) — the fan-out adapter and its first-error/short-write semantics.
- [`io.Writer`](https://pkg.go.dev/io#Writer) — the one-method contract every sink here satisfies.
- [`io.ErrShortWrite`](https://pkg.go.dev/io#pkg-variables) — the error MultiWriter returns when a sink accepts fewer bytes than offered.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-idle-timeout-conn.md](07-idle-timeout-conn.md) | Next: [09-buffered-readwritecloser.md](09-buffered-readwritecloser.md)
