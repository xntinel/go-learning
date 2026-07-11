# Exercise 4: Turning SIGTERM Into A Context Cancellation In main()

This is the top of the stack: the `main()` that a container runs. It uses
`signal.NotifyContext` so that SIGTERM (or Ctrl+C's SIGINT) cancels the one root
context every component already watches, and it keeps the signal wiring a thin
shell around a testable `Run(ctx)` core — because signal delivery is awkward to
unit-test, but a cancelled context is the exact substitute for a signal.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
signalshutdown/            module example.com/signalshutdown
  go.mod                   go 1.26
  service/
    service.go             Run(ctx, out, ln): HTTP + metrics worker, drain on cancel
  cmd/
    demo/
      main.go              signal.NotifyContext shell wiring SIGTERM/SIGINT to Run
  service/service_test.go  cancel-returns, worker-logs-stop, SIGTERM-cancels-context
```

Files: `service/service.go`, `cmd/demo/main.go`, `service/service_test.go`.
Implement: `Run(ctx context.Context, out io.Writer, ln net.Listener) error` that serves HTTP and a metrics worker, blocks on `ctx.Done()`, then drains in reverse order.
Test: cancelling `ctx` makes `Run` return `nil` and the worker log its stop; an integration test sends SIGTERM to the process and asserts a `NotifyContext` context becomes `Done`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/signalshutdown/service ~/go-exercises/signalshutdown/cmd/demo
cd ~/go-exercises/signalshutdown
go mod init example.com/signalshutdown
```

## Why the shell/core split

`signal.NotifyContext(parent, sig...)` returns `(ctx, stop)`: the first matching
signal cancels `ctx`, and `stop()` unregisters the handler so a *second* signal
gets the default disposition (a fatal kill) — the double-Ctrl+C escape hatch. That
is three lines of wiring that are genuinely hard to exercise in a unit test,
because a unit test cannot easily deliver an OS signal to just one goroutine. So
the design keeps `main()` a five-line shell: it builds the signal-derived context,
listens, and hands the context to `Run`. Everything worth testing lives in `Run`,
which knows nothing about signals — it only knows `ctx.Done()`. A test cancels the
context directly, which is behaviorally identical to a signal arriving.

`Run` is a compact real lifecycle: it serves an HTTP health endpoint and runs a
metrics worker on a ticker, blocks on `ctx.Done()`, then drains in reverse order —
HTTP first, worker second — with a fresh, bounded shutdown context. The metrics
worker logs "metrics worker stopping" on its way out, which is the observable the
test asserts to prove the cancellation propagated all the way into the worker.

The demo shows the other half — the signal shell — and where `stop()` goes. A
goroutine blocks on `ctx.Done()` and calls `stop()` the instant the first signal
arrives, restoring default handling so an impatient second Ctrl+C force-kills a
wedged drain. `defer stop()` covers the clean path.

Create `service/service.go`:

```go
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Run serves an HTTP health endpoint and a metrics worker under ctx, blocks
// until ctx is cancelled, then drains in reverse order: HTTP first so no new
// requests arrive, the worker second. In production ctx comes from
// signal.NotifyContext; a test cancels it directly, which is the exact
// behavioral substitute for a signal.
func Run(ctx context.Context, out io.Writer, ln net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	server := &http.Server{Handler: mux}

	serveErr := make(chan error, 1)
	go func() {
		err := server.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				fmt.Fprintln(out, "metrics worker stopping")
				return
			case <-t.C:
				fmt.Fprintln(out, "metrics tick")
			}
		}
	}()

	<-ctx.Done()

	var errs []error

	// Phase 1: stop ingress with a fresh, bounded context.
	shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shCtx); err != nil {
		errs = append(errs, fmt.Errorf("http shutdown: %w", err))
	}
	if err := <-serveErr; err != nil {
		errs = append(errs, fmt.Errorf("serve: %w", err))
	}

	// Phase 2: drain the worker, bounded so a wedged worker cannot hang exit.
	select {
	case <-workerDone:
	case <-time.After(3 * time.Second):
		errs = append(errs, fmt.Errorf("metrics worker did not stop in time"))
	}

	return errors.Join(errs...)
}
```

## The runnable demo

The demo is the signal shell. It builds a `NotifyContext`, serves on `:8080`, and
hands the context to `Run`. A watcher goroutine calls `stop()` when the first
signal lands so a second is fatal.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"example.com/signalshutdown/service"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		stop() // first signal arrived: restore default so a second is fatal
	}()

	ln, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	log.Println("service starting on 127.0.0.1:8080; press Ctrl+C to stop")
	if err := service.Run(ctx, os.Stdout, ln); err != nil {
		log.Fatalf("shutdown incomplete: %v", err)
	}
	log.Println("clean shutdown")
}
```

Run with `go run ./cmd/demo`, then press Ctrl+C. Expected output (the log
timestamps will differ):

```
service starting on 127.0.0.1:8080; press Ctrl+C to stop
metrics worker stopping
clean shutdown
```

## Tests

`TestRunReturnsOnCancel` starts `Run` against a loopback listener, cancels, and
asserts it returns `nil` and the buffer contains the worker's stop log — proving
the cancellation reached the worker. `TestSignalCancelsNotifyContext` is the
integration proof: it builds a `NotifyContext` for SIGTERM, sends SIGTERM to the
current process, and asserts the context becomes `Done` within a deadline —
exercising the real signal-to-cancellation bridge that the shell relies on.
`ExampleRun` locks the observable worker-stop line.

Create `service/service_test.go`:

```go
package service

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

func TestRunReturnsOnCancel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, &buf, listen(t)) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after cancel")
	}
	if !strings.Contains(buf.String(), "metrics worker stopping") {
		t.Fatalf("worker did not log its stop; got %q", buf.String())
	}
}

func TestSignalCancelsNotifyContext(t *testing.T) {
	// Not parallel: it delivers a real signal to the whole process.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}

	select {
	case <-ctx.Done():
		// The signal became a cancellation, exactly as the shell relies on.
	case <-time.After(2 * time.Second):
		t.Fatal("NotifyContext context did not cancel after SIGTERM")
	}
}

func ExampleRun() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println(err)
		return
	}
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, &buf, ln) }()

	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	fmt.Print(buf.String())
	// Output: metrics worker stopping
}
```

## Review

The split is the point: `main()` is a shell you validate by reading it, and `Run`
is a core you validate with tests, because a cancelled context is a faithful
stand-in for a signal. `TestRunReturnsOnCancel` proves cancellation propagates
into the worker; `TestSignalCancelsNotifyContext` proves the one thing the shell
adds — that a real SIGTERM becomes that cancellation. The mistakes to avoid:
never calling `stop()` (a second Ctrl+C then does nothing, so a wedged drain
cannot be force-killed); deriving the shutdown context from the cancelled root
instead of `Background` (zero drain budget); and burying untestable logic inside
`main()` where no test can reach it. Run `go test -race`; the signal test is not
parallel because it perturbs process-wide signal state.

## Resources

- [os/signal.NotifyContext](https://pkg.go.dev/os/signal#NotifyContext) — the signal-to-cancellation bridge and the role of the returned stop function.
- [os.Process.Signal](https://pkg.go.dev/os#Process.Signal) — delivering a signal to a process, used by the integration test.
- [Go Blog: Context](https://go.dev/blog/context) — the cancellation model the whole shutdown path is built on.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-app-reverse-order-orchestrator.md](03-app-reverse-order-orchestrator.md) | Next: [05-errgroup-supervised-lifecycle.md](05-errgroup-supervised-lifecycle.md)
