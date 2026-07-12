# Exercise 5: A Retry-On-Transient-Timeout Read Wrapper

A read from a flaky network source can stall transiently: a slow peer, a
momentary congestion, a proxy hiccup. The resilient response is to retry a bounded
number of times on a *transient* timeout while surfacing a *permanent* error
immediately. This exercise builds that wrapper and proves it with
`iotest.TimeoutReader`, which returns `iotest.ErrTimeout` on the second read and
then succeeds — an exact model of a single transient stall.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
retryread/                  independent module: example.com/retryread
  go.mod                    module example.com/retryread
  retry.go                  retryReader: retry on iotest.ErrTimeout, bounded attempts, pass others through
  cmd/
    demo/
      main.go               recovers a full payload despite an injected stall
  retry_test.go             full payload recovered; non-timeout surfaced with no extra reads; max-attempts cap
```

Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
Implement: an `io.Reader` wrapper that retries a read up to `maxRetry` times when it returns `iotest.ErrTimeout` with no data, and forwards any other error immediately.
Test: `io.ReadAll` recovers the full payload despite an injected `ErrTimeout`; a distinct sentinel is surfaced with no extra reads (counted by a spy); an always-timeout source hits the cap and returns the timeout deterministically.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/10-testing-readers-with-iotest/05-timeout-reader-retry-loop/cmd/demo
cd go-solutions/12-testing-ecosystem/10-testing-readers-with-iotest/05-timeout-reader-retry-loop
```

### Retry only what is retryable, and only bounded

The wrapper's whole value is discrimination. `iotest.TimeoutReader` returns
`(0, iotest.ErrTimeout)` on its second `Read` and reads normally otherwise, which
is the shape of a transient stall: no data lost, just a hiccup that a retry clears.
A retry loop that classifies with `errors.Is(err, iotest.ErrTimeout)` and re-reads
recovers the full stream. The two failure modes it must avoid are symmetric: retry
*every* error and a permanent fault becomes an infinite loop; retry *nothing* and a
transient stall fails a request that would have succeeded on the next read.

Two guards make it correct. First, only retry when `n == 0`: if a timeout ever
arrives with data (`n > 0`), the contract says process those bytes, so the wrapper
returns them and lets the caller drive the next read rather than discarding data.
Second, bound the retries with a counter; once `maxRetry` retries are spent, return
the timeout error so a persistently stalled source cannot spin forever. A permanent
error (any non-timeout) is returned on the first occurrence with no extra reads —
verified in the test with a spy reader that counts `Read` calls.

Create `retry.go`:

```go
package retryread

import (
	"errors"
	"io"
	"testing/iotest"
)

// retryReader wraps an io.Reader and retries a read up to maxRetry times when it
// returns iotest.ErrTimeout with no data. Any other error (including io.EOF) is
// forwarded unchanged on the first occurrence.
type retryReader struct {
	r        io.Reader
	maxRetry int
}

// NewRetryReader returns a reader that tolerates up to maxRetry transient timeouts
// per Read call.
func NewRetryReader(r io.Reader, maxRetry int) io.Reader {
	return &retryReader{r: r, maxRetry: maxRetry}
}

func (rr *retryReader) Read(p []byte) (int, error) {
	attempts := 0
	for {
		n, err := rr.r.Read(p)
		if n == 0 && errors.Is(err, iotest.ErrTimeout) {
			if attempts >= rr.maxRetry {
				return 0, err
			}
			attempts++
			continue
		}
		return n, err
	}
}
```

### The runnable demo

The demo wraps a one-byte-at-a-time source in `iotest.TimeoutReader` (injecting a
stall on the second read) and shows the retry wrapper still recovers the whole
payload through `io.ReadAll`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log"
	"strings"
	"testing/iotest"

	"example.com/retryread"
)

func main() {
	const payload = "resilient-payload"
	// One byte per read, with a transient timeout injected on the second read.
	flaky := iotest.TimeoutReader(iotest.OneByteReader(strings.NewReader(payload)))

	out, err := io.ReadAll(retryread.NewRetryReader(flaky, 3))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("recovered: %s\n", out)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
recovered: resilient-payload
```

### Tests

`TestRecoversFullPayload` wraps a one-byte source in `iotest.TimeoutReader` and
asserts `io.ReadAll` through the wrapper returns the whole payload despite the
injected timeout. `TestNonTimeoutSurfacedImmediately` uses a spy reader that
returns a distinct sentinel and counts reads, asserting the wrapper forwards it on
the first read with no retry. `TestMaxAttemptsCap` uses an always-timeout reader
and asserts the wrapper gives up after exactly `maxRetry + 1` reads and returns the
timeout.

Create `retry_test.go`:

```go
package retryread

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestRecoversFullPayload(t *testing.T) {
	t.Parallel()
	const payload = "the full recovered stream"
	flaky := iotest.TimeoutReader(iotest.OneByteReader(strings.NewReader(payload)))

	got, err := io.ReadAll(NewRetryReader(flaky, 3))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

// spyReader records how many times Read is called.
type spyReader struct {
	err   error
	calls int
}

func (s *spyReader) Read(p []byte) (int, error) {
	s.calls++
	return 0, s.err
}

var errPermanent = errors.New("connection refused")

func TestNonTimeoutSurfacedImmediately(t *testing.T) {
	t.Parallel()
	spy := &spyReader{err: errPermanent}
	rr := NewRetryReader(spy, 5)

	_, err := rr.Read(make([]byte, 8))
	if !errors.Is(err, errPermanent) {
		t.Fatalf("err = %v, want errPermanent", err)
	}
	if spy.calls != 1 {
		t.Fatalf("Read called %d times, want 1 (no retry on permanent error)", spy.calls)
	}
}

func TestMaxAttemptsCap(t *testing.T) {
	t.Parallel()
	spy := &spyReader{err: iotest.ErrTimeout}
	const maxRetry = 2
	rr := NewRetryReader(spy, maxRetry)

	_, err := rr.Read(make([]byte, 8))
	if !errors.Is(err, iotest.ErrTimeout) {
		t.Fatalf("err = %v, want iotest.ErrTimeout", err)
	}
	if spy.calls != maxRetry+1 {
		t.Fatalf("Read called %d times, want %d", spy.calls, maxRetry+1)
	}
}
```

## Review

The wrapper is correct when retry is gated on classification, not on the mere
presence of an error: `errors.Is(err, iotest.ErrTimeout)` with `n == 0` drives a
bounded retry, and everything else passes straight through. The three tests pin the
three behaviors that matter in an incident — recovery from a transient stall, no
wasted reads on a permanent fault, and a hard cap so a dead source cannot loop.
`iotest.TimeoutReader` is the honest simulator here because it stalls exactly once
and then succeeds, which is what a transient network hiccup looks like; the
always-timeout spy models the permanent-stall boundary the cap defends. Run
`go test -race` to confirm.

## Resources

- [`testing/iotest#TimeoutReader`](https://pkg.go.dev/testing/iotest#TimeoutReader) — returns `ErrTimeout` on the second read, then succeeds.
- [`testing/iotest#ErrTimeout`](https://pkg.go.dev/testing/iotest#pkg-variables) — the transient-stall sentinel.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — classification against a sentinel.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-checksum-tee-reader-fault-injection.md](04-checksum-tee-reader-fault-injection.md) | Next: [06-bounded-body-reader-oom-guard.md](06-bounded-body-reader-oom-guard.md)
