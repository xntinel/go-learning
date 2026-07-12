# Exercise 3: Exec-based readiness probe

Integration tests routinely need to wait for a dependency inside a container to
become ready — Postgres accepting connections, Redis answering `PING` — before
they run. The standard mechanism is `docker exec`: run a small command inside an
already-running container and treat exit code 0 as "ready". This exercise builds
that probe on the Engine SDK's exec endpoints, and the subtle part is that the
attach stream's EOF does not carry the exit code — you have to poll
`ContainerExecInspect` for it.

## What you'll build

```text
readyprobe/                    independent module: example.com/readyprobe
  go.mod                       requires github.com/docker/docker
  readyprobe.go                ExecAPI port; Prober; Probe; Result; ErrDaemon
  cmd/
    demo/
      main.go                  starts a long-lived container, execs `true` then `exit 1`
  readyprobe_test.go           fake ExecAPI; ready/not-ready/poll/timeout/close unit tests
  integration_test.go          //go:build docker: real daemon, skipped when Ping fails
```

Files: `readyprobe.go`, `cmd/demo/main.go`, `readyprobe_test.go`, `integration_test.go`.
Implement: an `ExecAPI` interface (`ContainerExecCreate`/`ContainerExecAttach`/`ContainerExecInspect`), a `Prober` whose `Probe` creates an exec, attaches and demultiplexes the output, and polls inspect for the exit code, plus a `Result` and the `ErrDaemon` sentinel.
Test: a hand-written fake asserting exit 0 is ready, non-zero is a not-ready `Result` with captured stderr, a still-`Running` exec is polled until it finishes, a deadline yields `context.DeadlineExceeded`, and the hijacked connection is always closed.
Verify: `go test -race ./...` for the unit path; `go test -tags docker -race ./...` on a Docker host.

Set up the module:

```bash
mkdir -p go-solutions/54-cloud-native-platform-and-orchestration/01-docker-engine-sdk/03-exec-readiness-probe/cmd/demo
cd go-solutions/54-cloud-native-platform-and-orchestration/01-docker-engine-sdk/03-exec-readiness-probe
go get github.com/docker/docker@latest
```

### Why the exit code is not in the stream

A `docker exec` is a three-call dance, not one call. `ContainerExecCreate`
registers the command and returns an exec ID. `ContainerExecAttach` opens a
hijacked connection carrying the command's output. `ContainerExecInspect`
reports the exec's live state — `Running` and, once it stops, `ExitCode`. The
attach stream is where the classic bug lives: reading it to EOF tells you the
process finished writing its output, but the stream carries no exit status. If
you infer "ready" from a clean EOF you will call a container ready that just
exited non-zero. The exit code lives only in the inspect response, and only once
`Running` has flipped to false — so the correct shape is: drain the attach
stream, then poll inspect until `Running` is false, then read `ExitCode`.

The attach stream is also multiplexed exactly like a container's logs. The exec
here is created without a TTY, so stdout and stderr arrive framed with 8-byte
headers and must be split with `stdcopy.StdCopy` — the same demultiplexing the
log exercise covered — into two buffers, so a not-ready result can surface the
stderr that explains why (`connection refused`, say).

### Two deadlines, two shapes

A readiness probe is deadline-bounded by definition; you probe "within N
seconds". There are two distinct ways the deadline can fire, and `Probe`
distinguishes them. If the command hangs producing output, `stdcopy.StdCopy` is
blocked reading the hijacked connection; when `ctx` is cancelled the SDK aborts
that read, `StdCopy` returns an error, and `Probe` reports it as a timeout by
consulting `ctx.Err()` rather than mislabeling it a daemon failure. If instead
the output has finished but the exec is still marked `Running`, the inspect poll
loop is where the deadline lands, and `ctx.Done()` in its `select` returns
`context.DeadlineExceeded`. A non-zero exit is neither of those: it is a normal
`Result` with `Ready` false, not an error at all — the caller decides whether to
retry.

### The hijacked connection must always close

`ContainerExecAttach` returns a `types.HijackedResponse` that owns a hijacked
TCP/socket connection. Its `Close()` method (which returns nothing) tears that
down; skipping it leaks a file descriptor per probe, and a readiness loop that
probes every second will exhaust the process's fd budget within the hour. A
`defer attach.Close()` immediately after the attach succeeds is the discipline,
and the unit test asserts it fires.

Create `readyprobe.go`:

```go
package readyprobe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// ExecAPI is the narrow slice of the Docker Engine client the probe depends on.
// It mirrors the method set of *client.Client, so a real *client.Client
// satisfies it with no adapter, while a hand-written fake satisfies it in unit
// tests with no daemon.
type ExecAPI interface {
	ContainerExecCreate(ctx context.Context, containerID string, options container.ExecOptions) (container.ExecCreateResponse, error)
	ContainerExecAttach(ctx context.Context, execID string, options container.ExecAttachOptions) (types.HijackedResponse, error)
	ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error)
}

// ErrDaemon marks failures from the daemon or the transport, as opposed to a
// probe command that ran fine and simply reported a non-zero (not-ready) status.
var ErrDaemon = errors.New("docker daemon error")

// Result is the outcome of one readiness probe.
type Result struct {
	Ready    bool   // true only when the command exited 0
	ExitCode int    // the command's exit code
	Stdout   string // captured standard output
	Stderr   string // captured standard error
}

// Prober runs a command inside an already-running container (the docker exec
// equivalent) and reports readiness from the command's exit code.
type Prober struct {
	api          ExecAPI
	PollInterval time.Duration // how often to poll ContainerExecInspect for the exit code
}

// New returns a Prober backed by api with a sane default poll interval. Pass a
// *client.Client in production or a fake in tests.
func New(api ExecAPI) *Prober {
	return &Prober{api: api, PollInterval: 200 * time.Millisecond}
}

// Probe runs cmd inside containerID once and reports whether it exited 0 within
// ctx's deadline. A non-zero exit is a normal not-ready Result, not an error. A
// daemon or transport failure wraps ErrDaemon; a deadline that fires before the
// exec finishes returns ctx.Err(). The hijacked attach connection is always
// closed.
func (p *Prober) Probe(ctx context.Context, containerID string, cmd []string) (Result, error) {
	created, err := p.api.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return Result{}, fmt.Errorf("exec create: %w: %w", err, ErrDaemon)
	}
	execID := created.ID

	attach, err := p.api.ContainerExecAttach(ctx, execID, container.ExecAttachOptions{})
	if err != nil {
		return Result{}, fmt.Errorf("exec attach: %w: %w", err, ErrDaemon)
	}
	defer attach.Close()

	// The exec has no TTY, so the attach stream is multiplexed: stdout and stderr
	// are framed with 8-byte headers and must be split with StdCopy. This blocks
	// until the process closes its output (EOF).
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, attach.Reader); err != nil {
		// A cancelled ctx aborts the underlying read; classify that as a timeout,
		// not a daemon failure.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, fmt.Errorf("probe deadline while streaming exec output: %w", ctxErr)
		}
		return Result{}, fmt.Errorf("drain exec stream: %w: %w", err, ErrDaemon)
	}

	// The attach EOF means the process finished writing; it does NOT carry the
	// exit code. Poll ContainerExecInspect until Running is false, then read
	// ExitCode.
	for {
		insp, err := p.api.ContainerExecInspect(ctx, execID)
		if err != nil {
			return Result{}, fmt.Errorf("exec inspect: %w: %w", err, ErrDaemon)
		}
		if !insp.Running {
			return Result{
				Ready:    insp.ExitCode == 0,
				ExitCode: insp.ExitCode,
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
			}, nil
		}

		select {
		case <-ctx.Done():
			return Result{}, fmt.Errorf("probe deadline before exec finished: %w", ctx.Err())
		case <-time.After(p.PollInterval):
		}
	}
}
```

### The demo

The demo starts a long-lived `busybox sleep` container (something to exec into),
then probes it twice: once with `true` (ready) and once with a command that
writes to stderr and exits 1 (not ready). It classifies the outcome from the
`Result`, showing how a caller tells a ready dependency from an unready one. It
needs a running Docker daemon and network access to pull `busybox`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"

	"example.com/readyprobe"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()

	rc, err := cli.ImagePull(ctx, "busybox:latest", image.PullOptions{})
	if err != nil {
		return err
	}
	io.Copy(io.Discard, rc)
	rc.Close()

	// A long-lived container to exec into.
	created, err := cli.ContainerCreate(ctx,
		&container.Config{Image: "busybox:latest", Cmd: []string{"sleep", "30"}},
		nil, nil, nil, "")
	if err != nil {
		return err
	}
	id := created.ID
	defer cli.ContainerRemove(context.WithoutCancel(ctx), id, container.RemoveOptions{Force: true})

	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return err
	}

	prober := readyprobe.New(cli)

	ready, err := prober.Probe(ctx, id, []string{"true"})
	if err != nil {
		return err
	}
	fmt.Printf("probe #1 ready=%v exit=%d\n", ready.Ready, ready.ExitCode)

	notReady, err := prober.Probe(ctx, id, []string{"sh", "-c", "echo not-ready 1>&2; exit 1"})
	if err != nil {
		return err
	}
	fmt.Printf("probe #2 ready=%v exit=%d stderr=%q\n", notReady.Ready, notReady.ExitCode, notReady.Stderr)

	return nil
}
```

Run it against a host with Docker:

```bash
go run ./cmd/demo
```

Expected output:

```
probe #1 ready=true exit=0
probe #2 ready=false exit=1 stderr="not-ready\n"
```

### The tests

The unit tests never touch a daemon. `fakeExecAPI` implements `ExecAPI` and
returns preconfigured results, including a slice of `ExecInspect` values handed
out in sequence so a test can model an exec that reports `Running` for two polls
before finishing. `muxStream` reuses the `stdcopy.NewStdWriter` trick from the
log exercise to synthesize the exact multiplexed framing the daemon emits, so
the probe's stream handling is exercised for real without a container.
`fakeConn` is a minimal `net.Conn` that records `Close`, which is how
`TestProbeReady` proves the hijacked connection is torn down.
`blockingReader` returns only when its context is cancelled, modelling an exec
whose output outlives the deadline, so `TestProbeStreamTimeout` drives the
read-abort timeout path.

`TestProbeReady` asserts exit 0 is `Ready` with stdout captured;
`TestProbeNotReady` asserts a non-zero exit is a `Result` (no error) carrying the
stderr; `TestProbePollsUntilNotRunning` asserts the inspect loop keeps polling
while `Running` is true; `TestProbeTimeoutWhileRunning` asserts a never-finishing
exec yields `context.DeadlineExceeded`; `TestProbeCreateError` and
`TestProbeInspectError` assert daemon failures wrap `ErrDaemon`.

Create `readyprobe_test.go`:

```go
package readyprobe

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// muxStream builds the daemon's multiplexed exec framing by writing through
// stdcopy's frame writers, so the probe's stream handling is tested with no
// daemon.
func muxStream(stdout, stderr string) *bytes.Buffer {
	var buf bytes.Buffer
	ow := stdcopy.NewStdWriter(&buf, stdcopy.Stdout)
	ew := stdcopy.NewStdWriter(&buf, stdcopy.Stderr)
	io.WriteString(ow, stdout)
	io.WriteString(ew, stderr)
	return &buf
}

// fakeConn is a net.Conn that records whether it was closed, so a test can prove
// HijackedResponse.Close ran. The probe never actually reads or writes it; it
// reads the HijackedResponse.Reader instead.
type fakeConn struct{ closed bool }

func (c *fakeConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *fakeConn) Write(b []byte) (int, error)      { return len(b), nil }
func (c *fakeConn) Close() error                     { c.closed = true; return nil }
func (c *fakeConn) LocalAddr() net.Addr              { return nil }
func (c *fakeConn) RemoteAddr() net.Addr             { return nil }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

// blockingReader blocks until ctx is done, then returns ctx.Err() as a read
// error, modelling an attach stream whose process outlives the deadline.
type blockingReader struct{ ctx context.Context }

func (b blockingReader) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

type fakeExecAPI struct {
	createErr error
	execID    string

	attachErr    error
	attachConn   *fakeConn
	attachStream io.Reader

	inspectResults []container.ExecInspect
	inspectIdx     int
	inspectErr     error
}

func (f *fakeExecAPI) ContainerExecCreate(_ context.Context, _ string, _ container.ExecOptions) (container.ExecCreateResponse, error) {
	if f.createErr != nil {
		return container.ExecCreateResponse{}, f.createErr
	}
	return container.ExecCreateResponse{ID: f.execID}, nil
}

func (f *fakeExecAPI) ContainerExecAttach(_ context.Context, _ string, _ container.ExecAttachOptions) (types.HijackedResponse, error) {
	if f.attachErr != nil {
		return types.HijackedResponse{}, f.attachErr
	}
	return types.HijackedResponse{Conn: f.attachConn, Reader: bufio.NewReader(f.attachStream)}, nil
}

func (f *fakeExecAPI) ContainerExecInspect(_ context.Context, _ string) (container.ExecInspect, error) {
	if f.inspectErr != nil {
		return container.ExecInspect{}, f.inspectErr
	}
	r := f.inspectResults[f.inspectIdx]
	if f.inspectIdx < len(f.inspectResults)-1 {
		f.inspectIdx++
	}
	return r, nil
}

// newFast returns a Prober with a tiny poll interval so poll-loop tests run
// quickly.
func newFast(f *fakeExecAPI) *Prober {
	p := New(f)
	p.PollInterval = time.Millisecond
	return p
}

func TestProbeReady(t *testing.T) {
	t.Parallel()
	conn := &fakeConn{}
	f := &fakeExecAPI{
		execID:         "e1",
		attachConn:     conn,
		attachStream:   muxStream("pong\n", ""),
		inspectResults: []container.ExecInspect{{Running: false, ExitCode: 0}},
	}
	res, err := newFast(f).Probe(context.Background(), "c1", []string{"true"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Ready || res.ExitCode != 0 {
		t.Errorf("Result = %+v; want Ready=true ExitCode=0", res)
	}
	if res.Stdout != "pong\n" {
		t.Errorf("Stdout = %q; want %q", res.Stdout, "pong\n")
	}
	if !conn.closed {
		t.Error("attach connection was not closed")
	}
}

func TestProbeNotReady(t *testing.T) {
	t.Parallel()
	f := &fakeExecAPI{
		execID:         "e1",
		attachConn:     &fakeConn{},
		attachStream:   muxStream("", "connection refused\n"),
		inspectResults: []container.ExecInspect{{Running: false, ExitCode: 1}},
	}
	res, err := newFast(f).Probe(context.Background(), "c1", []string{"pg_isready"})
	if err != nil {
		t.Fatalf("Probe returned an error for a non-zero exit: %v", err)
	}
	if res.Ready || res.ExitCode != 1 {
		t.Errorf("Result = %+v; want Ready=false ExitCode=1", res)
	}
	if res.Stderr != "connection refused\n" {
		t.Errorf("Stderr = %q; want the captured stderr", res.Stderr)
	}
}

func TestProbePollsUntilNotRunning(t *testing.T) {
	t.Parallel()
	f := &fakeExecAPI{
		execID:       "e1",
		attachConn:   &fakeConn{},
		attachStream: muxStream("", ""),
		inspectResults: []container.ExecInspect{
			{Running: true},
			{Running: true},
			{Running: false, ExitCode: 0},
		},
	}
	res, err := newFast(f).Probe(context.Background(), "c1", []string{"true"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !res.Ready {
		t.Errorf("Result = %+v; want Ready=true after polling", res)
	}
	if f.inspectIdx != 2 {
		t.Errorf("inspectIdx = %d; want 2 (three inspect calls)", f.inspectIdx)
	}
}

func TestProbeTimeoutWhileRunning(t *testing.T) {
	t.Parallel()
	f := &fakeExecAPI{
		execID:         "e1",
		attachConn:     &fakeConn{},
		attachStream:   muxStream("", ""),
		inspectResults: []container.ExecInspect{{Running: true}}, // never finishes
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := newFast(f).Probe(ctx, "c1", []string{"sleep"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Probe err = %v; want context.DeadlineExceeded", err)
	}
}

func TestProbeStreamTimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	f := &fakeExecAPI{
		execID:         "e1",
		attachConn:     &fakeConn{},
		attachStream:   blockingReader{ctx: ctx},
		inspectResults: []container.ExecInspect{{Running: false}},
	}
	_, err := newFast(f).Probe(ctx, "c1", []string{"sleep"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Probe err = %v; want context.DeadlineExceeded from the aborted read", err)
	}
}

func TestProbeCreateError(t *testing.T) {
	t.Parallel()
	f := &fakeExecAPI{createErr: errors.New("socket closed")}
	_, err := newFast(f).Probe(context.Background(), "c1", []string{"true"})
	if !errors.Is(err, ErrDaemon) {
		t.Fatalf("Probe err = %v; want ErrDaemon", err)
	}
}

func TestProbeInspectError(t *testing.T) {
	t.Parallel()
	f := &fakeExecAPI{
		execID:       "e1",
		attachConn:   &fakeConn{},
		attachStream: muxStream("", ""),
		inspectErr:   errors.New("inspect boom"),
	}
	_, err := newFast(f).Probe(context.Background(), "c1", []string{"true"})
	if !errors.Is(err, ErrDaemon) {
		t.Fatalf("Probe err = %v; want ErrDaemon", err)
	}
}

func ExampleProber_Probe() {
	f := &fakeExecAPI{
		execID:         "e1",
		attachConn:     &fakeConn{},
		attachStream:   muxStream("", "not ready\n"),
		inspectResults: []container.ExecInspect{{Running: false, ExitCode: 1}},
	}
	res, _ := New(f).Probe(context.Background(), "db", []string{"pg_isready"})
	fmt.Printf("ready=%v exit=%d stderr=%q\n", res.Ready, res.ExitCode, res.Stderr)
	// Output: ready=false exit=1 stderr="not ready\n"
}
```

### The integration test

Behind `//go:build docker`, the integration test starts a real long-lived
`busybox` container, execs `true` and asserts ready, then execs `sh -c 'exit 1'`
and asserts not-ready with exit code 1. It skips when `Ping` fails, so
`go test -tags docker` is safe on a machine without Docker.

Create `integration_test.go`:

```go
//go:build docker

package readyprobe

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

func startBusybox(ctx context.Context, t *testing.T, cli *client.Client) string {
	t.Helper()
	rc, err := cli.ImagePull(ctx, "busybox:latest", image.PullOptions{})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	io.Copy(io.Discard, rc)
	rc.Close()

	created, err := cli.ContainerCreate(ctx,
		&container.Config{Image: "busybox:latest", Cmd: []string{"sleep", "60"}},
		nil, nil, nil, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := created.ID
	t.Cleanup(func() {
		cli.ContainerRemove(context.WithoutCancel(ctx), id, container.RemoveOptions{Force: true})
	})
	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	return id
}

func TestIntegrationProbe(t *testing.T) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("no reachable docker daemon: %v", err)
	}

	id := startBusybox(ctx, t, cli)
	p := New(cli)

	ready, err := p.Probe(ctx, id, []string{"true"})
	if err != nil {
		t.Fatalf("Probe(true): %v", err)
	}
	if !ready.Ready || ready.ExitCode != 0 {
		t.Errorf("Probe(true) = %+v; want Ready=true ExitCode=0", ready)
	}

	notReady, err := p.Probe(ctx, id, []string{"sh", "-c", "exit 1"})
	if err != nil {
		t.Fatalf("Probe(exit 1): %v", err)
	}
	if notReady.Ready || notReady.ExitCode != 1 {
		t.Errorf("Probe(exit 1) = %+v; want Ready=false ExitCode=1", notReady)
	}
}
```

## Review

The probe is correct when readiness is a pure function of the exit code and the
exit code comes from `ContainerExecInspect`, never from the attach stream's EOF.
Read the tests together: `TestProbeReady` and `TestProbeNotReady` pin the two
normal outcomes (exit 0 is ready, non-zero is a not-ready `Result` and not an
error), `TestProbePollsUntilNotRunning` proves the loop waits for `Running` to
clear before trusting `ExitCode`, and the two timeout tests prove both deadline
paths — a blocked read and a stuck-`Running` poll — surface as
`context.DeadlineExceeded` rather than as a spurious daemon error or a false
"ready".

The mistakes to avoid are the ones the concepts file flagged. Inferring the exit
code from EOF makes a non-zero exit look ready; the poll loop exists precisely to
avoid that. Copying the non-TTY attach stream raw instead of through
`stdcopy.StdCopy` would leave frame headers in the captured stderr, so a
not-ready diagnostic would be garbage. And leaking the `HijackedResponse` on
every probe drains the fd budget of a long-running probe loop; the
`defer attach.Close()` and the `fakeConn.closed` assertion guard against a
refactor that drops it. Run `go test -race ./...` for the unit path and, on a
Docker host, `go test -tags docker -race ./...` for the real exec.

## Resources

- [`(*Client).ContainerExecCreate`](https://pkg.go.dev/github.com/docker/docker/client#Client.ContainerExecCreate) — creating an exec and the returned exec ID.
- [`(*Client).ContainerExecAttach`](https://pkg.go.dev/github.com/docker/docker/client#Client.ContainerExecAttach) — the hijacked connection and `types.HijackedResponse`.
- [`api/types/container` — ExecOptions and ExecInspect](https://pkg.go.dev/github.com/docker/docker/api/types/container#ExecOptions) — `Cmd`, `AttachStdout`/`AttachStderr`, and the `Running`/`ExitCode` fields.
- [`pkg/stdcopy`](https://pkg.go.dev/github.com/docker/docker/pkg/stdcopy) — demultiplexing the multiplexed exec stream with `StdCopy`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-log-stream-demux.md](02-log-stream-demux.md) | Next: [../02-kubebuilder-operator-crd/00-concepts.md](../02-kubebuilder-operator-crd/00-concepts.md)