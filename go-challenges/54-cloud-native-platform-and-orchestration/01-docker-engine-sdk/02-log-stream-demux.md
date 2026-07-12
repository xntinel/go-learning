# Exercise 2: Streaming and demultiplexing container logs

A container's log stream is not clean stdout. For a container created without a
TTY, the daemon interleaves stdout and stderr into one stream with an 8-byte
frame header on every chunk; copying that straight to your terminal produces
garbage. This exercise builds a log tailer that inspects the container to learn
whether it has a TTY and then either copies the raw stream (TTY) or
demultiplexes the framed stream with `stdcopy.StdCopy` (non-TTY) — and the demux
is a pure function you test with no daemon at all.

## What you'll build

```text
logtail/                       independent module: example.com/logtail
  go.mod                       requires github.com/docker/docker
  logtail.go                   LogAPI port; Tailer; Tail; Demux (raw vs stdcopy split)
  cmd/
    demo/
      main.go                  runs a container that writes to both fds, then tails its logs
  logtail_test.go              pure demux tests built from stdcopy.NewStdWriter; close assertion
  integration_test.go          //go:build docker: real daemon, skipped when Ping fails
```

Files: `logtail.go`, `cmd/demo/main.go`, `logtail_test.go`, `integration_test.go`.
Implement: a `LogAPI` interface (`ContainerInspect`/`ContainerLogs`), a `Tailer` whose `Tail` inspects, opens the log stream, and closes it, and a `Demux(tty, src, stdout, stderr)` that copies raw for TTY and runs `stdcopy.StdCopy` for non-TTY.
Test: build a multiplexed stream with `stdcopy.NewStdWriter` and assert `Demux` routes each side correctly; assert the TTY path is a raw passthrough; assert `Tail` closes the stream and propagates an inspect error.
Verify: `go test -race ./...` for the unit path; `go test -tags docker -race ./...` on a Docker host.

Set up the module:

```bash
mkdir -p go-solutions/54-cloud-native-platform-and-orchestration/01-docker-engine-sdk/02-log-stream-demux/cmd/demo
cd go-solutions/54-cloud-native-platform-and-orchestration/01-docker-engine-sdk/02-log-stream-demux
go get github.com/docker/docker@latest
```

### Why the demux is a branch, and why it is testable alone

The daemon frames a non-TTY log stream because stdout and stderr are two separate
file descriptors that must travel over one connection: each chunk is prefixed
with an 8-byte header whose first byte identifies the stream (1 for stdout, 2 for
stderr) and whose last four bytes hold the chunk length.
`stdcopy.StdCopy(dstout, dsterr, src)` reads that framing and writes the two
destreams to two writers. A TTY container has no such framing — a TTY is a single
character device, so stdout and stderr are already merged into one raw byte
stream, and running it through `StdCopy` would misread ordinary bytes as frame
headers. Hence the branch: `Tail` reads `Config.Tty` from `ContainerInspect` and
picks raw copy or `StdCopy`.

The important design move is that `Demux` takes a `bool` and three plain
`io.Reader`/`io.Writer` values, not a client or a container ID. That makes it a
pure function of its inputs, and it makes it testable with zero infrastructure:
`stdcopy.NewStdWriter(w, stdcopy.Stdout)` returns a writer that applies the exact
framing the daemon uses, so a test can synthesize a realistic multiplexed stream
in memory and assert that `Demux` splits it back apart correctly. The tailer's
one daemon-dependent responsibility — inspect, open, close — is tested separately
with a fake `LogAPI`.

Note the `defer rc.Close()` in `Tail`: the `io.ReadCloser` from `ContainerLogs`
owns a live connection, and leaking it leaks a file descriptor per call.

Create `logtail.go`:

```go
package logtail

import (
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// LogAPI is the narrow slice of the Docker Engine client that the tailer needs.
// *client.Client satisfies it directly; a fake satisfies it in tests.
type LogAPI interface {
	ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error)
	ContainerLogs(ctx context.Context, id string, options container.LogsOptions) (io.ReadCloser, error)
}

// Tailer streams a container's logs, demultiplexing stdout and stderr.
type Tailer struct {
	api LogAPI
}

// New returns a Tailer backed by api.
func New(api LogAPI) *Tailer {
	return &Tailer{api: api}
}

// Tail streams container id's logs into stdout and stderr according to opts. It
// first inspects the container to learn whether it was created with a TTY, then
// picks the raw or the demultiplexed path, and always closes the log stream.
func (t *Tailer) Tail(ctx context.Context, id string, opts container.LogsOptions, stdout, stderr io.Writer) error {
	insp, err := t.api.ContainerInspect(ctx, id)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", id, err)
	}
	tty := insp.Config != nil && insp.Config.Tty

	rc, err := t.api.ContainerLogs(ctx, id, opts)
	if err != nil {
		return fmt.Errorf("logs %s: %w", id, err)
	}
	defer rc.Close()

	return Demux(tty, rc, stdout, stderr)
}

// Demux splits a container output stream into stdout and stderr. For a TTY
// stream (tty true) the bytes are raw and copied straight to stdout; for a
// non-TTY stream they carry 8-byte frame headers and are split with
// stdcopy.StdCopy.
func Demux(tty bool, src io.Reader, stdout, stderr io.Writer) error {
	if tty {
		if _, err := io.Copy(stdout, src); err != nil {
			return fmt.Errorf("copy tty stream: %w", err)
		}
		return nil
	}
	if _, err := stdcopy.StdCopy(stdout, stderr, src); err != nil {
		return fmt.Errorf("demux stream: %w", err)
	}
	return nil
}
```

### The demo

The demo runs a container that writes one line to stdout and one to stderr, then
tails its logs after it exits. It deliberately creates the container **without**
`AutoRemove`: because it reads the logs after the container stops, the container
must survive its own exit — this is exactly the `AutoRemove`-versus-post-mortem
trade-off from the concepts file. It removes the container explicitly at the end
with a `WithoutCancel` context. It needs a running daemon and network access.

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

	"example.com/logtail"
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

	// No AutoRemove: we read the logs after the container exits, so it must
	// survive the exit rather than being deleted the instant it stops.
	created, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image: "busybox:latest",
			Cmd:   []string{"sh", "-c", "echo to-stdout; echo to-stderr 1>&2"},
		},
		nil, nil, nil, "")
	if err != nil {
		return err
	}
	id := created.ID
	defer cli.ContainerRemove(context.WithoutCancel(ctx), id, container.RemoveOptions{Force: true})

	statusCh, errCh := cli.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return err
	}
	select {
	case err := <-errCh:
		if err != nil {
			return err
		}
	case <-statusCh:
	}

	tailer := logtail.New(cli)
	return tailer.Tail(ctx, id,
		container.LogsOptions{ShowStdout: true, ShowStderr: true}, os.Stdout, os.Stderr)
}
```

Run it against a host with Docker:

```bash
go run ./cmd/demo
```

Expected output (stdout and stderr both reach the terminal):

```
to-stdout
to-stderr
```

### The tests

`muxStream` is the key helper: it writes through `stdcopy.NewStdWriter(&buf,
stdcopy.Stdout)` and `stdcopy.NewStdWriter(&buf, stdcopy.Stderr)` to produce
exactly the framed bytes a real non-TTY daemon would emit, so `TestDemuxNonTTY`
can feed that buffer to `Demux` and assert stdout and stderr land on the right
writers with the headers stripped. `TestDemuxTTY` asserts the TTY path is a raw
passthrough — everything goes to stdout, stderr stays empty. `TestTailDemuxesAndCloses`
uses a fake `LogAPI` and a `closeReader` to assert both that `Tail` demultiplexes
and that it closes the stream. `TestTailInspectError` asserts an inspect failure
is wrapped and surfaced (checked with `errors.Is`) rather than swallowed.

Create `logtail_test.go`:

```go
package logtail

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
)

// muxStream builds the daemon's non-TTY multiplexed framing by writing through
// stdcopy's frame writers, so the demultiplexer can be tested with no daemon.
func muxStream(stdout, stderr string) *bytes.Buffer {
	var buf bytes.Buffer
	ow := stdcopy.NewStdWriter(&buf, stdcopy.Stdout)
	ew := stdcopy.NewStdWriter(&buf, stdcopy.Stderr)
	io.WriteString(ow, stdout)
	io.WriteString(ew, stderr)
	return &buf
}

// closeReader wraps a reader to record that Close was called.
type closeReader struct {
	io.Reader
	closed bool
}

func (c *closeReader) Close() error {
	c.closed = true
	return nil
}

type fakeLogAPI struct {
	tty        bool
	inspectErr error
	logsErr    error
	logs       *closeReader
}

func (f *fakeLogAPI) ContainerInspect(_ context.Context, _ string) (container.InspectResponse, error) {
	if f.inspectErr != nil {
		return container.InspectResponse{}, f.inspectErr
	}
	return container.InspectResponse{Config: &container.Config{Tty: f.tty}}, nil
}

func (f *fakeLogAPI) ContainerLogs(_ context.Context, _ string, _ container.LogsOptions) (io.ReadCloser, error) {
	if f.logsErr != nil {
		return nil, f.logsErr
	}
	return f.logs, nil
}

func TestDemuxNonTTY(t *testing.T) {
	t.Parallel()
	src := muxStream("out-line\n", "err-line\n")

	var out, errb bytes.Buffer
	if err := Demux(false, src, &out, &errb); err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if out.String() != "out-line\n" {
		t.Errorf("stdout = %q; want %q", out.String(), "out-line\n")
	}
	if errb.String() != "err-line\n" {
		t.Errorf("stderr = %q; want %q", errb.String(), "err-line\n")
	}
}

func TestDemuxTTY(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	if err := Demux(true, strings.NewReader("raw tty bytes\n"), &out, &errb); err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if out.String() != "raw tty bytes\n" {
		t.Errorf("stdout = %q; want raw passthrough", out.String())
	}
	if errb.Len() != 0 {
		t.Errorf("stderr = %q; want empty on the TTY path", errb.String())
	}
}

func TestTailDemuxesAndCloses(t *testing.T) {
	t.Parallel()
	cr := &closeReader{Reader: muxStream("hello\n", "oops\n")}
	f := &fakeLogAPI{tty: false, logs: cr}

	var out, errb bytes.Buffer
	err := New(f).Tail(context.Background(), "c1",
		container.LogsOptions{ShowStdout: true, ShowStderr: true}, &out, &errb)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if out.String() != "hello\n" || errb.String() != "oops\n" {
		t.Errorf("out=%q err=%q; want hello/oops", out.String(), errb.String())
	}
	if !cr.closed {
		t.Error("log stream was not closed")
	}
}

func TestTailInspectError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("inspect boom")
	f := &fakeLogAPI{inspectErr: sentinel}

	err := New(f).Tail(context.Background(), "c1", container.LogsOptions{}, io.Discard, io.Discard)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Tail err = %v; want it to wrap the inspect error", err)
	}
}

func ExampleDemux() {
	src := muxStream("to stdout\n", "to stderr\n")

	var out, errb bytes.Buffer
	_ = Demux(false, src, &out, &errb)
	fmt.Printf("stdout=%q stderr=%q\n", out.String(), errb.String())
	// Output: stdout="to stdout\n" stderr="to stderr\n"
}
```

### The integration test

Behind `//go:build docker`, the integration test runs a real `busybox` container
that writes `OUT` to stdout and `ERR` to stderr, tails its logs through the real
demux path, and asserts each string landed on the correct side. It skips when
`Ping` fails, so it is safe to run on a machine without Docker.

Create `integration_test.go`:

```go
//go:build docker

package logtail

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

func TestIntegrationTailDemux(t *testing.T) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("no reachable docker daemon: %v", err)
	}

	rc, err := cli.ImagePull(ctx, "busybox:latest", image.PullOptions{})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	io.Copy(io.Discard, rc)
	rc.Close()

	created, err := cli.ContainerCreate(ctx,
		&container.Config{Image: "busybox:latest", Cmd: []string{"sh", "-c", "echo OUT; echo ERR 1>&2"}},
		nil, nil, nil, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := created.ID
	defer cli.ContainerRemove(context.WithoutCancel(ctx), id, container.RemoveOptions{Force: true})

	statusCh, errCh := cli.ContainerWait(ctx, id, container.WaitConditionNotRunning)
	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
	case <-statusCh:
	}

	var out, errb bytes.Buffer
	if err := New(cli).Tail(ctx, id,
		container.LogsOptions{ShowStdout: true, ShowStderr: true}, &out, &errb); err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if !strings.Contains(out.String(), "OUT") {
		t.Errorf("stdout = %q; want it to contain OUT", out.String())
	}
	if !strings.Contains(errb.String(), "ERR") {
		t.Errorf("stderr = %q; want it to contain ERR", errb.String())
	}
}
```

## Review

The demux is correct when a non-TTY stream is split byte-for-byte back into the
two writers it was framed from, and a TTY stream passes through untouched to
stdout only. The `stdcopy.NewStdWriter` helper is what makes that assertion honest
without a daemon — it produces the identical framing the daemon uses, so a passing
`TestDemuxNonTTY` genuinely exercises the header-parsing path. The most common
real-world bug this guards against is copying a non-TTY stream straight to
`os.Stdout`, which corrupts the output with header bytes; the second is failing to
close the log stream, which `TestTailDemuxesAndCloses` checks explicitly.

The subtle mistake is inverting the TTY branch or forgetting it entirely: running
`StdCopy` on a TTY stream mangles it, and running a raw copy on a non-TTY stream
leaves the headers in. Confirm the branch by reading `Config.Tty` from
`ContainerInspect`, never guessing. Run `go test -race ./...` for the unit path
and `go test -tags docker -race ./...` on a Docker host.

## Resources

- [`pkg/stdcopy`](https://pkg.go.dev/github.com/docker/docker/pkg/stdcopy) — `StdCopy`, `NewStdWriter`, and the `Stdout`/`Stderr` stream types.
- [`api/types/container` — LogsOptions](https://pkg.go.dev/github.com/docker/docker/api/types/container#LogsOptions) — `ShowStdout`, `ShowStderr`, `Follow`, `Timestamps`, `Tail`, `Since`.
- [`(*Client).ContainerLogs`](https://pkg.go.dev/github.com/docker/docker/client#Client.ContainerLogs) — the streaming log endpoint and its `io.ReadCloser`.
- [Docker Engine SDK examples — run and get output](https://docs.docker.com/reference/api/engine/sdk/examples/) — the raw-versus-multiplexed distinction in the official Go sample.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-ephemeral-job-runner.md](01-ephemeral-job-runner.md) | Next: [03-exec-readiness-probe.md](03-exec-readiness-probe.md)
