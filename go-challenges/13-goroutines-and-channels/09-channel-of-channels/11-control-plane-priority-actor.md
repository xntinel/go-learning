# Exercise 11: Control-Plane Priority Request Actor

**Level: Intermediate**

A tenant coordinator serves two request classes over one serialized loop: high-priority control requests (pause a tenant, reload config, health probe) and ordinary data requests. The naive single-inbox actor serves whatever arrives next, so under load an operator's pause command queues behind a backlog of data work and takes effect minutes late. This exercise builds a two-inbox actor whose loop preferentially drains the control inbox before touching data, so control never starves.

This module is self-contained: its own module, a `priorityactor` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
priorityactor/               independent module: example.com/priorityactor
  go.mod                     go 1.26
  priorityactor.go           two-inbox actor: Control (priority) and Submit (data) over one loop
  cmd/demo/main.go           runnable demo: sequential control+data mix, print serviced order
  priorityactor_test.go      priority-ordering, backlog-jump, ctx-cancel, shutdown-reject tests
```

- Files: `priorityactor.go`, `cmd/demo/main.go`, `priorityactor_test.go`.
- Implement: `type Service`; `New() *Service`; `Run()`; `Control(ctx, ControlCmd) (ControlResp, error)`; `Submit(ctx, int) (int, error)`; `Shutdown()`; `var ErrShuttingDown`.
- Test: control is serviced before data even when data is enqueued first; a control request jumps a deep data backlog; both entry points honour a cancelled `ctx` via `context.Cause` on send and on receive; `Shutdown` joins `Run` and post-shutdown calls return `ErrShuttingDown`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/09-channel-of-channels/11-control-plane-priority-actor/cmd/demo
cd go-solutions/13-goroutines-and-channels/09-channel-of-channels/11-control-plane-priority-actor
```

### Two inboxes, one loop, a preferential drain

The actor owns two buffered request channels, `control` and `data`, and one goroutine reads both. Each request carries its own capacity-1 reply channel, so the serialized loop is the only writer of state and there is no mutex. The priority rule lives entirely in how the loop selects its next request:

1. At the top of every iteration, try a **non-blocking** receive on `control` with a `default`. If a control request is waiting, handle it and `continue` back to the top — re-checking control before anything else.
2. Only when that non-blocking receive falls through to `default` (control is empty) does the loop enter a **blocking** `select` over `control`, `data`, and `quit`.

That two-tier structure is the invariant: as long as control is non-empty, the loop services control and nothing else. A data request is handled only in the second `select`, which is reachable only when control is drained. So even a control request enqueued *after* a hundred data requests is serviced before the next data request — it waits at most for the one data item currently being handled, never for the whole backlog. This is the difference between a control plane that responds in microseconds and one that responds only after the queue empties.

The loop owns `paused`, `configVersion`, and `processed` as plain locals. It also appends the class of each request to a `serviced` slice in handling order; that slice is the ground truth the tests read (after `Shutdown`, which establishes happens-before) to prove control-before-data.

Both `Control` and `Submit` thread `ctx` through the inbox send and the reply receive, and a leading non-blocking `quit` check makes a completed shutdown win deterministically over an available buffer slot — otherwise a `select` between a ready send and a closed `quit` would pick at random.

Create `priorityactor.go`:

```go
package priorityactor

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// ErrShuttingDown is returned once the service has been shut down.
var ErrShuttingDown = errors.New("priorityactor: shutting down")

// ControlKind names the class of a control-plane request.
type ControlKind int

const (
	// Pause marks a tenant as paused; ControlCmd.Tenant selects it.
	Pause ControlKind = iota
	// Reload bumps the in-memory config version.
	Reload
	// Probe returns a health snapshot without mutating state.
	Probe
)

// ControlCmd is an operator command served on the high-priority inbox.
type ControlCmd struct {
	Kind   ControlKind
	Tenant string
}

// ControlResp is the answer to a ControlCmd.
type ControlResp struct {
	OK     bool
	Detail string
}

// controlReq and dataReq each carry their own capacity-1 reply channel, so a
// timed-out caller never wedges the run loop's send.
type controlReq struct {
	cmd   ControlCmd
	reply chan ControlResp
}

type dataReq struct {
	n     int
	reply chan int
}

// inboxCap bounds each inbox. Both classes are buffered so callers can enqueue
// without a live receiver, which is what lets a backlog build up ahead of the
// loop starting.
const inboxCap = 16

// Service is a single-goroutine actor serving two request classes over one
// serialized loop. The control inbox is drained preferentially so an operator
// command never starves behind queued data work.
type Service struct {
	control chan controlReq
	data    chan dataReq
	quit    chan struct{}
	done    chan struct{}

	// serviced records the class of each request in the order the loop handled
	// it. It is owned by Run and is only safe to read after Shutdown joins Run.
	serviced []string
}

// New returns a Service with buffered control and data inboxes. Start it with
// go s.Run().
func New() *Service {
	return &Service{
		control: make(chan controlReq, inboxCap),
		data:    make(chan dataReq, inboxCap),
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Run is the actor loop. It owns all mutable state, so no lock is needed. Each
// iteration first tries a non-blocking receive on the control inbox; only when
// control is empty does it fall through to a blocking select over both inboxes
// and the quit signal. That ordering is the whole point: control before data.
func (s *Service) Run() {
	defer close(s.done)

	var (
		paused        = map[string]bool{}
		configVersion int
		processed     int
	)

	handleControl := func(req controlReq) {
		var resp ControlResp
		switch req.cmd.Kind {
		case Pause:
			paused[req.cmd.Tenant] = true
			resp = ControlResp{OK: true, Detail: "paused " + req.cmd.Tenant}
		case Reload:
			configVersion++
			resp = ControlResp{OK: true, Detail: fmt.Sprintf("config v%d", configVersion)}
		case Probe:
			resp = ControlResp{OK: true, Detail: fmt.Sprintf("paused=%d version=%d processed=%d", len(paused), configVersion, processed)}
		}
		s.serviced = append(s.serviced, "control")
		req.reply <- resp
	}

	handleData := func(req dataReq) {
		processed++
		s.serviced = append(s.serviced, "data")
		req.reply <- req.n * 2
	}

	for {
		// Priority tier: drain one control request if any is waiting, then loop
		// back to re-check control before ever touching data.
		select {
		case req := <-s.control:
			handleControl(req)
			continue
		default:
		}
		// Control is empty: block until control refills, data arrives, or quit.
		select {
		case req := <-s.control:
			handleControl(req)
		case req := <-s.data:
			handleData(req)
		case <-s.quit:
			return
		}
	}
}

// Shutdown signals the loop to stop and waits for it to exit. It is synchronous:
// once it returns, Run has finished and callers can safely read Serviced.
func (s *Service) Shutdown() {
	close(s.quit)
	<-s.done
}

// Serviced returns a copy of the order in which requests were handled. Call it
// only after Shutdown, which establishes happens-before against the loop.
func (s *Service) Serviced() []string {
	return slices.Clone(s.serviced)
}

// Control submits a high-priority control command and returns its response. It
// threads ctx through both the inbox send and the reply receive, returning
// context.Cause(ctx) if the context is cancelled at either step.
func (s *Service) Control(ctx context.Context, cmd ControlCmd) (ControlResp, error) {
	// A closed quit wins deterministically over an available buffer slot.
	select {
	case <-s.quit:
		return ControlResp{}, ErrShuttingDown
	default:
	}
	reply := make(chan ControlResp, 1)
	select {
	case s.control <- controlReq{cmd: cmd, reply: reply}:
	case <-ctx.Done():
		return ControlResp{}, context.Cause(ctx)
	case <-s.quit:
		return ControlResp{}, ErrShuttingDown
	}
	select {
	case resp := <-reply:
		return resp, nil
	case <-ctx.Done():
		return ControlResp{}, context.Cause(ctx)
	case <-s.quit:
		return ControlResp{}, ErrShuttingDown
	}
}

// Submit submits an ordinary data request and returns the computed result. Like
// Control it honours ctx on both the send and the receive via context.Cause.
func (s *Service) Submit(ctx context.Context, n int) (int, error) {
	select {
	case <-s.quit:
		return 0, ErrShuttingDown
	default:
	}
	reply := make(chan int, 1)
	select {
	case s.data <- dataReq{n: n, reply: reply}:
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	case <-s.quit:
		return 0, ErrShuttingDown
	}
	select {
	case v := <-reply:
		return v, nil
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	case <-s.quit:
		return 0, ErrShuttingDown
	}
}
```

### The runnable demo

The demo issues a sequential mix of control and data requests. Because the loop serializes everything, the serviced order equals the call order and the output is fully deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/priorityactor"
)

func main() {
	ctx := context.Background()
	s := priorityactor.New()
	go s.Run()

	// A sequential mix of control and data requests. Because the loop serializes
	// every request, the outcomes are fully deterministic.
	probe, _ := s.Control(ctx, priorityactor.ControlCmd{Kind: priorityactor.Probe})
	fmt.Println("probe:", probe.Detail)

	v, _ := s.Submit(ctx, 21)
	fmt.Println("submit(21):", v)

	pause, _ := s.Control(ctx, priorityactor.ControlCmd{Kind: priorityactor.Pause, Tenant: "acme"})
	fmt.Println("pause:", pause.Detail)

	reload, _ := s.Control(ctx, priorityactor.ControlCmd{Kind: priorityactor.Reload})
	fmt.Println("reload:", reload.Detail)

	after, _ := s.Control(ctx, priorityactor.ControlCmd{Kind: priorityactor.Probe})
	fmt.Println("probe:", after.Detail)

	s.Shutdown()
	fmt.Println("serviced:", s.Serviced())

	if _, err := s.Submit(ctx, 1); err != nil {
		fmt.Println("post-shutdown submit:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
probe: paused=0 version=0 processed=0
submit(21): 42
pause: paused acme
reload: config v1
probe: paused=1 version=1 processed=1
serviced: [control data control control control]
post-shutdown submit: priorityactor: shutting down
```

### Tests

`TestControlDrainedBeforeDataWhenPreloaded` fills the data inbox first, then the control inbox — all before `Run` starts, verified by polling `len(s.data)` and `len(s.control)` (a channel's length read is race-free). It then starts the loop and asserts that in the `serviced` slice no control entry ever appears after a data entry, and that the counts match. `TestControlJumpsDataBacklog` enqueues a deep data backlog and then a single control request last, and pins that the control request is `serviced[0]` — it jumped the entire backlog. `TestSubmitHonorsCancel` and `TestControlHonorsCancel` each cover two sub-cases: a *send* blocked on a full inbox (only `ctx.Done()` is ready, so the return is deterministic) and a *receive* blocked with no reply, asserting `errors.Is(err, cause)` where `cause` came from `context.WithCancelCause`. `TestShutdownJoinsAndRejects` confirms `Shutdown` is synchronous and that both entry points return `ErrShuttingDown` afterward. No test uses `time.Sleep` to gate correctness; ordering falls out of the priority `select` and of channel lengths.

Create `priorityactor_test.go`:

```go
package priorityactor

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

// waitLen busy-waits (yielding, never sleeping) until get() reports want. The
// time bound is only a failsafe; correctness comes from the channel lengths, not
// from timing.
func waitLen(t *testing.T, get func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for get() != want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for length %d, got %d", want, get())
		}
		runtime.Gosched()
	}
}

// TestControlDrainedBeforeDataWhenPreloaded loads the data inbox first, then the
// control inbox, all before the loop starts. It pins that every control request
// is serviced before any data request, proving the priority ordering.
func TestControlDrainedBeforeDataWhenPreloaded(t *testing.T) {
	t.Parallel()
	const nData, nControl = 8, 4
	s := New()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := range nData {
		wg.Go(func() { s.Submit(ctx, i) })
	}
	waitLen(t, func() int { return len(s.data) }, nData)
	for range nControl {
		wg.Go(func() { s.Control(ctx, ControlCmd{Kind: Probe}) })
	}
	waitLen(t, func() int { return len(s.control) }, nControl)

	// Only now start the loop; both inboxes are fully backlogged.
	go s.Run()
	wg.Wait()
	s.Shutdown()

	order := s.Serviced()
	// Every control entry must precede every data entry: once the first data
	// request is serviced, no control may appear after it.
	seenData := false
	gotControl := 0
	for _, c := range order {
		switch c {
		case "control":
			if seenData {
				t.Fatalf("control serviced after data: %v", order)
			}
			gotControl++
		case "data":
			seenData = true
		}
	}
	if gotControl != nControl || len(order) != nControl+nData {
		t.Fatalf("serviced = %v, want %d control + %d data", order, nControl, nData)
	}
}

// TestControlJumpsDataBacklog enqueues a deep data backlog, then a single
// control request, then starts the loop. The control request must be serviced
// first even though it was enqueued last, without waiting out the backlog.
func TestControlJumpsDataBacklog(t *testing.T) {
	t.Parallel()
	const nData = 12
	s := New()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := range nData {
		wg.Go(func() { s.Submit(ctx, i) })
	}
	waitLen(t, func() int { return len(s.data) }, nData)

	done := make(chan struct{})
	wg.Go(func() {
		defer close(done)
		if _, err := s.Control(ctx, ControlCmd{Kind: Pause, Tenant: "acme"}); err != nil {
			t.Errorf("Control error = %v", err)
		}
	})
	waitLen(t, func() int { return len(s.control) }, 1)

	go s.Run()
	<-done // control is answered
	wg.Wait()
	s.Shutdown()

	order := s.Serviced()
	if len(order) == 0 || order[0] != "control" {
		t.Fatalf("control did not jump the data backlog: %v", order)
	}
}

// TestSubmitHonorsCancel checks that Submit returns context.Cause on both a
// blocked send (inbox full) and a blocked receive (no reply yet).
func TestSubmitHonorsCancel(t *testing.T) {
	t.Parallel()

	t.Run("send", func(t *testing.T) {
		t.Parallel()
		s := New()
		fillCtx, fillCancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for i := range inboxCap {
			wg.Go(func() { s.Submit(fillCtx, i) })
		}
		waitLen(t, func() int { return len(s.data) }, inboxCap)

		cause := errors.New("shed load")
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		if _, err := s.Submit(ctx, 999); !errors.Is(err, cause) {
			t.Fatalf("Submit send err = %v, want %v", err, cause)
		}
		fillCancel()
		wg.Wait()
	})

	t.Run("receive", func(t *testing.T) {
		t.Parallel()
		s := New()
		cause := errors.New("client gone")
		ctx, cancel := context.WithCancelCause(context.Background())
		errc := make(chan error, 1)
		go func() {
			_, err := s.Submit(ctx, 5)
			errc <- err
		}()
		waitLen(t, func() int { return len(s.data) }, 1) // send has landed
		cancel(cause)
		if err := <-errc; !errors.Is(err, cause) {
			t.Fatalf("Submit receive err = %v, want %v", err, cause)
		}
	})
}

// TestControlHonorsCancel mirrors TestSubmitHonorsCancel for the control path.
func TestControlHonorsCancel(t *testing.T) {
	t.Parallel()

	t.Run("send", func(t *testing.T) {
		t.Parallel()
		s := New()
		fillCtx, fillCancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for range inboxCap {
			wg.Go(func() { s.Control(fillCtx, ControlCmd{Kind: Probe}) })
		}
		waitLen(t, func() int { return len(s.control) }, inboxCap)

		cause := errors.New("operator abort")
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		if _, err := s.Control(ctx, ControlCmd{Kind: Reload}); !errors.Is(err, cause) {
			t.Fatalf("Control send err = %v, want %v", err, cause)
		}
		fillCancel()
		wg.Wait()
	})

	t.Run("receive", func(t *testing.T) {
		t.Parallel()
		s := New()
		cause := errors.New("deadline")
		ctx, cancel := context.WithCancelCause(context.Background())
		errc := make(chan error, 1)
		go func() {
			_, err := s.Control(ctx, ControlCmd{Kind: Probe})
			errc <- err
		}()
		waitLen(t, func() int { return len(s.control) }, 1)
		cancel(cause)
		if err := <-errc; !errors.Is(err, cause) {
			t.Fatalf("Control receive err = %v, want %v", err, cause)
		}
	})
}

// TestShutdownJoinsAndRejects confirms Shutdown is synchronous (it joins Run)
// and that every call afterward returns ErrShuttingDown.
func TestShutdownJoinsAndRejects(t *testing.T) {
	t.Parallel()
	s := New()
	go s.Run()
	ctx := context.Background()

	if v, err := s.Submit(ctx, 3); err != nil || v != 6 {
		t.Fatalf("Submit(3) = %d, %v; want 6, nil", v, err)
	}
	if resp, err := s.Control(ctx, ControlCmd{Kind: Probe}); err != nil || !resp.OK {
		t.Fatalf("Control probe = %+v, %v; want OK, nil", resp, err)
	}

	s.Shutdown()

	if _, err := s.Submit(ctx, 1); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("post-shutdown Submit err = %v, want %v", err, ErrShuttingDown)
	}
	if _, err := s.Control(ctx, ControlCmd{Kind: Probe}); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("post-shutdown Control err = %v, want %v", err, ErrShuttingDown)
	}
}
```

## Review

Correct here means one property: no data request is ever serviced while a control request waits. The two-tier `select` guarantees it — data is reachable only in the second `select`, entered only when the non-blocking control receive falls through to `default`, so control is drained first by construction, not by chance. `TestControlDrainedBeforeDataWhenPreloaded` proves it with both inboxes preloaded (data first) and asserts the serviced order never puts control after data; `TestControlJumpsDataBacklog` proves the live case where a control request enqueued last is serviced first. The reply channels are capacity-1 so a cancelled caller never wedges the loop, and `context.Cause` on both the send and the receive surfaces *why* a call was abandoned; the leading non-blocking `quit` check keeps a completed shutdown from losing a coin-flip against an open buffer slot. The production bug this prevents is the one every operator has hit: a pause or drain command that queues behind thousands of in-flight data requests and lands far too late to matter.

## Resources

- [Go statement: Select](https://go.dev/ref/spec#Select_statements) -- the non-blocking `default` and the blocking multi-way `select` that together encode the priority.
- [`context.Cause`](https://pkg.go.dev/context#Cause) -- returning the precise cancellation reason threaded through send and receive.
- [`context.WithCancelCause`](https://pkg.go.dev/context#WithCancelCause) -- attaching a custom cause the tests assert on with `errors.Is`.
- [Go Memory Model](https://go.dev/ref/mem) -- why single-goroutine ownership of the state and the `serviced` slice needs no lock, and why `Shutdown`'s join makes the post-run read safe.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-connection-pool-lease-checkout.md](10-connection-pool-lease-checkout.md) | Next: [12-singleflight-cache-coordinator.md](12-singleflight-cache-coordinator.md)
