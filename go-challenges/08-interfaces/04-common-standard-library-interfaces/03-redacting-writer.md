# Exercise 3: A Secret-Redacting io.Writer Wrapper for Log Sinks

Logs leak secrets: an API key printed in a request dump, a password in an error
message. A robust defence is a writer that sits in front of your log sink and
masks known secret tokens before they hit disk. Building it teaches the one
non-obvious rule of `io.Writer`: a transforming writer must still report the
number of bytes it *consumed* (`len(p)`), not the number it forwarded.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
redact/                     independent module: example.com/redact
  go.mod
  redact.go                 RedactingWriter wrapping io.Writer; masks secret tokens
  cmd/
    demo/
      main.go               logs a line with a password and an API key, both masked
  redact_test.go            masking, len(p) contract, pass-through, MultiWriter, error propagation
```

- Files: `redact.go`, `cmd/demo/main.go`, `redact_test.go`.
- Implement: `RedactingWriter` with `Write(p []byte) (int, error)` that replaces each configured secret with a mask before forwarding, plus `NewRedactingWriter(w io.Writer, secrets ...string) *RedactingWriter`.
- Test: secrets masked in output; `Write` returns `len(p)` not the post-redaction length; clean input passes through unchanged; composition with `io.MultiWriter` fans out to two sinks; an error from the underlying writer is propagated.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/redact/cmd/demo
cd ~/go-exercises/redact
go mod init example.com/redact
```

### The len(p) contract is the whole lesson

`io.Copy`, `fmt.Fprintf`, and `log.Logger` all interpret `Write` through the
short-write rule: if `Write` returns `n < len(p)` with a nil error, the caller
concludes the sink accepted only part of the data and reports `io.ErrShortWrite`.
A redacting writer replaces `hunter2` (7 bytes) with `[REDACTED]` (10 bytes), so
the number of bytes it hands to the downstream sink differs from the number it
received. If it naively returned the downstream count, every `fmt.Fprintf`
through it would look like a short (or over-) write and fail. The contract is
resolved by reporting what you *consumed from the caller*: on success, return
`len(p)`. The downstream byte count is an internal detail the caller must never
see.

Two buffer-safety points from the concepts file apply directly. First, do not
mutate the caller's slice `p` in place; `bytes.ReplaceAll` returns a fresh slice,
so redaction never touches `p`, and when there is nothing to redact the writer
forwards `p` unchanged without copying. Second, on the error path — when the
underlying writer fails — propagate that error; the caller needs to know the sink
broke. The implementation captures the downstream error and returns it alongside
`len(p)` semantics: on any downstream failure it returns the error so the caller
stops.

Create `redact.go`:

```go
package redact

import (
	"bytes"
	"io"
)

var mask = []byte("[REDACTED]")

// RedactingWriter forwards writes to an underlying sink after replacing every
// configured secret with a fixed mask. It satisfies io.Writer and always
// reports len(p) on success, per the Writer contract, even though the number of
// bytes forwarded downstream differs after redaction.
type RedactingWriter struct {
	w       io.Writer
	secrets [][]byte
}

// NewRedactingWriter wraps w, masking each secret token before forwarding.
func NewRedactingWriter(w io.Writer, secrets ...string) *RedactingWriter {
	tokens := make([][]byte, 0, len(secrets))
	for _, s := range secrets {
		if s != "" {
			tokens = append(tokens, []byte(s))
		}
	}
	return &RedactingWriter{w: w, secrets: tokens}
}

func (rw *RedactingWriter) Write(p []byte) (int, error) {
	out := p
	for _, secret := range rw.secrets {
		if bytes.Contains(out, secret) {
			// ReplaceAll allocates a fresh slice, so the caller's p is
			// never mutated. Only copy when there is something to redact.
			out = bytes.ReplaceAll(out, secret, mask)
		}
	}

	if _, err := rw.w.Write(out); err != nil {
		return 0, err
	}
	// Report bytes consumed from the caller, not bytes forwarded downstream.
	return len(p), nil
}

var _ io.Writer = (*RedactingWriter)(nil)
```

### The runnable demo

The demo logs a line containing a password and an API key through the redacting
writer into a buffer, then prints the buffer so you can see both secrets masked.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"

	"example.com/redact"
)

func main() {
	var sink bytes.Buffer
	w := redact.NewRedactingWriter(&sink, "hunter2", "sk-live-abc123")

	fmt.Fprintf(w, "user=bob password=hunter2 token=sk-live-abc123\n")

	fmt.Print(sink.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
user=bob password=[REDACTED] token=[REDACTED]
```

### Tests

`TestMasksSecrets` asserts the output has the secrets replaced.
`TestWriteReportsInputLength` is the contract test: it calls `Write` directly with
a slice that shrinks under redaction and asserts `n == len(input)`, never the
shorter forwarded length. `TestCleanInputPassesThrough` proves input without
secrets is forwarded byte-for-byte. `TestMultiWriterFanOut` composes the redactor
with `io.MultiWriter` to two buffers and asserts both receive the masked output —
the redactor is just an `io.Writer`, so it slots into any composition.
`TestUnderlyingErrorPropagates` uses a failing writer and asserts the error comes
back.

Create `redact_test.go`:

```go
package redact

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestMasksSecrets(t *testing.T) {
	t.Parallel()

	var sink bytes.Buffer
	w := NewRedactingWriter(&sink, "hunter2")
	if _, err := io.WriteString(w, "pw=hunter2 done"); err != nil {
		t.Fatal(err)
	}
	if got := sink.String(); got != "pw=[REDACTED] done" {
		t.Fatalf("output = %q, want %q", got, "pw=[REDACTED] done")
	}
}

func TestWriteReportsInputLength(t *testing.T) {
	t.Parallel()

	var sink bytes.Buffer
	w := NewRedactingWriter(&sink, "secret")
	input := []byte("token=secret")

	n, err := w.Write(input)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(input) {
		t.Fatalf("Write returned n=%d, want len(input)=%d (must report bytes consumed, not forwarded)", n, len(input))
	}
	if sink.Len() == len(input) {
		t.Fatal("sanity: redaction should have changed the forwarded length")
	}
}

func TestCleanInputPassesThrough(t *testing.T) {
	t.Parallel()

	var sink bytes.Buffer
	w := NewRedactingWriter(&sink, "secret")
	const clean = "nothing to hide here"
	n, err := io.WriteString(w, clean)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(clean) {
		t.Fatalf("n = %d, want %d", n, len(clean))
	}
	if sink.String() != clean {
		t.Fatalf("output = %q, want %q", sink.String(), clean)
	}
}

func TestMultiWriterFanOut(t *testing.T) {
	t.Parallel()

	var a, b bytes.Buffer
	w := NewRedactingWriter(io.MultiWriter(&a, &b), "key123")
	if _, err := io.WriteString(w, "apikey=key123"); err != nil {
		t.Fatal(err)
	}
	const want = "apikey=[REDACTED]"
	if a.String() != want || b.String() != want {
		t.Fatalf("fan-out: a=%q b=%q, want both %q", a.String(), b.String(), want)
	}
}

type failWriter struct{ err error }

func (f failWriter) Write([]byte) (int, error) { return 0, f.err }

func TestUnderlyingErrorPropagates(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("disk full")
	w := NewRedactingWriter(failWriter{err: sentinel}, "secret")
	_, err := io.WriteString(w, "x=secret")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func ExampleRedactingWriter() {
	var sink strings.Builder
	w := NewRedactingWriter(&sink, "p@ss")
	fmt.Fprint(w, "login p@ss ok")
	fmt.Println(sink.String())
	// Output: login [REDACTED] ok
}
```

## Review

The writer is correct when secrets never reach the sink, clean input is forwarded
verbatim, and `Write` reports `len(p)` on success so `fmt.Fprintf` and
`io.MultiWriter` treat every write as complete. The trap is returning the
post-redaction length; the contract test exists precisely to catch that. Note the
redactor is a plain `io.Writer`, so it composes: put it in front of a file sink,
a `io.MultiWriter`, or a `log.New` destination without any of them knowing it is
there. A production version would guard against a secret split across two `Write`
calls (buffering across writes) and use a constant-width mask to avoid leaking the
secret's length; both are natural extensions of this core. Run `go test -race`.

## Resources

- [io.Writer](https://pkg.go.dev/io#Writer) — the `Write` contract and the short-write rule.
- [io.MultiWriter](https://pkg.go.dev/io#MultiWriter) — fanning one write out to several sinks.
- [bytes.ReplaceAll](https://pkg.go.dev/bytes#ReplaceAll) — non-mutating replacement returning a fresh slice.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-capped-body-reader.md](02-capped-body-reader.md) | Next: [04-domain-stringer.md](04-domain-stringer.md)
