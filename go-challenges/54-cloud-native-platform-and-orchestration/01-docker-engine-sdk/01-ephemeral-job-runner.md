# Exercise 1: An ephemeral container job runner

This is the whole-task deliverable: a run-to-completion job runner that mirrors
`docker run --rm`. It pulls an image if absent, creates a resource-limited
container, starts it, waits for it to exit under a deadline, and returns the exit
code plus a typed error that distinguishes a daemon failure from a container that
simply exited non-zero. The daemon calls sit behind a narrow port so the
orchestration logic is unit-tested with a fake, and a real-daemon test lives
behind a build tag.

## What you'll build

```text
jobrunner/                     independent module: example.com/jobrunner
  go.mod                       requires github.com/docker/docker
  jobrunner.go                 DockerAPI port; JobRunner; Run; ErrDaemon; ExitError; Spec
  cmd/
    demo/
      main.go                  runs busybox `true` and `sh -c 'exit 7'` against a real daemon
  jobrunner_test.go            fake DockerAPI; unit tests for ordering, exit mapping, cleanup
  integration_test.go          //go:build docker: real daemon, skipped when Ping fails
```

Files: `jobrunner.go`, `cmd/demo/main.go`, `jobrunner_test.go`, `integration_test.go`.
Implement: a `DockerAPI` interface (`ImagePull`/`ContainerCreate`/`ContainerStart`/`ContainerWait`/`ContainerRemove`), a `JobRunner` whose `Run` pulls-drains-creates-waits-starts-selects-cleans, and typed errors `ErrDaemon` (sentinel) and `*ExitError`.
Test: a hand-written fake asserting wait-before-start ordering, pull-stream drain+close, the three exit outcomes, and cleanup-on-cancel using a fresh context; an integration test behind `//go:build docker`.
Verify: `go test -race ./...` for the unit path; `go test -tags docker -race ./...` on a host with a daemon.

Set up the module:

```bash
mkdir -p go-solutions/54-cloud-native-platform-and-orchestration/01-docker-engine-sdk/01-ephemeral-job-runner/cmd/demo
cd go-solutions/54-cloud-native-platform-and-orchestration/01-docker-engine-sdk/01-ephemeral-job-runner
go get github.com/docker/docker@latest
```

### The port, and why the real client satisfies it for free

The lifecycle logic is the part with real bugs in it: the order of the wait and
the start, the two-channel select, the mapping of a status code to an error, the
cleanup that must survive cancellation. None of that should require a daemon to
test. So `JobRunner` depends on `DockerAPI`, an interface listing only the five
methods it calls. The signatures are copied verbatim from `*client.Client`, which
means the real client satisfies `DockerAPI` with no adapter type at all — `New(cli)`
just works — while a fake satisfies it in tests. This is the hexagonal split: a
narrow port, a real implementation you did not have to write, and a fake you did.

### Walking through Run

`Run` executes the sequence the concepts file argued for, in exactly this order.

First it pulls and **drains** the image stream. `ImagePull` returns immediately
with a progress reader; the image is not actually present until that reader hits
EOF. Draining with `io.Copy(io.Discard, rc)` and then closing it is what makes the
subsequent create safe. A pull error, a drain error, or a close error all wrap
`ErrDaemon` — they are infrastructure failures, not workload failures.

Then it creates the container with an **empty name** (so repeated runs never hit a
`409 Conflict`), `AutoRemove: true`, and a `Resources` block carrying the memory
and CPU limits. Immediately after a successful create it installs a deferred
cleanup: a best-effort `ContainerRemove(Force: true)` on `context.WithoutCancel(ctx)`.
`WithoutCancel` is the crux — it keeps the parent's values but strips its deadline
and cancellation, so cleanup still runs even when `Run` is returning *because* the
context expired. With `AutoRemove` already handling the normal exit, this explicit
remove usually finds nothing and returns a harmless not-found error, which is why
it is best-effort; it exists for the paths `AutoRemove` never reaches (a start
failure, or an abandoned wait).

Next comes the ordering that closes the fast-exit race: `ContainerWait` is called
**before** `ContainerStart`. `ContainerWait` only subscribes — it returns two
channels and does not block — so subscribing first guarantees the exit event is
captured no matter how quickly the container finishes. Only then does `Run` start
the container.

Finally the `select` reads all three possible outcomes on separate cases:
`ctx.Done()` (the deadline fired; the container keeps running, and the deferred
remove stops it), the wait error channel (a transport or daemon failure, wrapped
as `ErrDaemon`), and the status channel. On the status channel there are three
sub-outcomes the concepts file distinguished: a `WaitResponse.Error` set by the
daemon (wrapped `ErrDaemon`), a non-zero `StatusCode` (a clean `*ExitError`,
which explicitly must not also be an `ErrDaemon`), and a zero status (success).

Note `WaitResponse.StatusCode` is an `int64`; `Run` narrows it to `int` for its
return, which is the conventional exit-code width.

Create `jobrunner.go`:

```go
package jobrunner

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// DockerAPI is the narrow slice of the Docker Engine client that the job runner
// depends on. It mirrors the method set of *client.Client exactly, so a real
// *client.Client satisfies it with no adapter, while a hand-written fake
// satisfies it in unit tests with no daemon.
type DockerAPI interface {
	ImagePull(ctx context.Context, ref string, options image.PullOptions) (io.ReadCloser, error)
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
}

// ErrDaemon marks failures that come from the daemon or the transport, as
// opposed to a container that ran fine and simply exited non-zero.
var ErrDaemon = errors.New("docker daemon error")

// ExitError reports that the container ran to completion but exited non-zero.
// It is distinct from ErrDaemon: the infrastructure worked, the workload failed.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("container exited with code %d", e.Code)
}

// Spec describes one run-to-completion job.
type Spec struct {
	Image    string   // image reference, e.g. "busybox:latest"
	Cmd      []string // command + args; empty uses the image default
	Env      []string // KEY=VALUE strings
	Memory   int64    // hard memory limit in bytes; 0 means unlimited
	NanoCPUs int64    // CPU quota in billionths of a CPU; 1e9 is one core; 0 is unlimited
}

// JobRunner runs one-shot containers to completion behind the DockerAPI port.
type JobRunner struct {
	api DockerAPI
}

// New returns a JobRunner backed by api. Pass a *client.Client in production or
// a fake in tests.
func New(api DockerAPI) *JobRunner {
	return &JobRunner{api: api}
}

// Run pulls the image if needed, creates a resource-limited container, starts
// it, and waits for it to exit, returning the exit code. A non-zero exit is
// reported as *ExitError; a daemon or transport failure wraps ErrDaemon. The
// container is always removed before Run returns, even when ctx is cancelled.
func (r *JobRunner) Run(ctx context.Context, spec Spec) (int, error) {
	// The pull returns a progress stream; the image is not present until the
	// stream is drained to EOF, so drain and close it before creating.
	rc, err := r.api.ImagePull(ctx, spec.Image, image.PullOptions{})
	if err != nil {
		return -1, fmt.Errorf("pull %q: %w: %w", spec.Image, err, ErrDaemon)
	}
	if _, err := io.Copy(io.Discard, rc); err != nil {
		rc.Close()
		return -1, fmt.Errorf("drain pull stream: %w: %w", err, ErrDaemon)
	}
	if err := rc.Close(); err != nil {
		return -1, fmt.Errorf("close pull stream: %w: %w", err, ErrDaemon)
	}

	// Empty name: let the daemon assign an ID so repeated runs never 409.
	created, err := r.api.ContainerCreate(ctx,
		&container.Config{
			Image: spec.Image,
			Cmd:   spec.Cmd,
			Env:   spec.Env,
		},
		&container.HostConfig{
			AutoRemove: true,
			Resources: container.Resources{
				Memory:   spec.Memory,
				NanoCPUs: spec.NanoCPUs,
			},
		},
		nil, nil, "")
	if err != nil {
		return -1, fmt.Errorf("create container: %w: %w", err, ErrDaemon)
	}
	id := created.ID

	// Guaranteed cleanup. WithoutCancel drops ctx's deadline and cancellation so
	// the remove still runs on the timeout path; AutoRemove covers the happy
	// path, so a "no such container" here is expected and ignored.
	defer func() {
		_ = r.api.ContainerRemove(context.WithoutCancel(ctx), id, container.RemoveOptions{Force: true})
	}()

	// Subscribe to the wait BEFORE starting: a fast-exiting container can finish
	// before a wait registered after start would ever see it.
	statusCh, waitErrCh := r.api.ContainerWait(ctx, id, container.WaitConditionNotRunning)

	if err := r.api.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return -1, fmt.Errorf("start container: %w: %w", err, ErrDaemon)
	}

	select {
	case <-ctx.Done():
		// The container is still running on the host; the deferred remove stops it.
		return -1, fmt.Errorf("waiting for container: %w", ctx.Err())
	case werr := <-waitErrCh:
		if werr != nil {
			return -1, fmt.Errorf("wait: %w: %w", werr, ErrDaemon)
		}
		return -1, fmt.Errorf("wait channel closed without a status: %w", ErrDaemon)
	case resp := <-statusCh:
		code := int(resp.StatusCode)
		if resp.Error != nil {
			return code, fmt.Errorf("daemon wait error: %s: %w", resp.Error.Message, ErrDaemon)
		}
		if code != 0 {
			return code, &ExitError{Code: code}
		}
		return 0, nil
	}
}
```

### The demo

The demo constructs a real client with `FromEnv` and `WithAPIVersionNegotiation`
— the production construction — and runs two jobs against the daemon: `true`
(exit 0) and `sh -c 'exit 7'` (exit 7). It classifies the outcome with
`errors.As` for `*ExitError` and `errors.Is` for `ErrDaemon`, showing how a caller
tells a failed workload from failed infrastructure. It needs a running Docker
daemon and network access to pull `busybox`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/docker/docker/client"

	"example.com/jobrunner"
)

func main() {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		fmt.Fprintln(os.Stderr, "client:", err)
		os.Exit(1)
	}
	defer cli.Close()

	runner := jobrunner.New(cli)

	jobs := []struct {
		label string
		spec  jobrunner.Spec
	}{
		{"true", jobrunner.Spec{Image: "busybox:latest", Cmd: []string{"true"}, Memory: 64 << 20, NanoCPUs: 500_000_000}},
		{"exit 7", jobrunner.Spec{Image: "busybox:latest", Cmd: []string{"sh", "-c", "exit 7"}, Memory: 64 << 20, NanoCPUs: 500_000_000}},
	}

	for _, j := range jobs {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		code, err := runner.Run(ctx, j.spec)
		cancel()

		var exitErr *jobrunner.ExitError
		switch {
		case err == nil:
			fmt.Printf("job %q finished: exit code %d\n", j.label, code)
		case errors.As(err, &exitErr):
			fmt.Printf("job %q finished: exit code %d (ExitError)\n", j.label, exitErr.Code)
		case errors.Is(err, jobrunner.ErrDaemon):
			fmt.Printf("job %q daemon failure: %v\n", j.label, err)
		default:
			fmt.Printf("job %q error: %v\n", j.label, err)
		}
	}
}
```

Run it against a host with Docker:

```bash
go run ./cmd/demo
```

Expected output:

```
job "true" finished: exit code 0
job "exit 7" finished: exit code 7 (ExitError)
```

### The tests

The unit tests never touch a daemon. `fakeAPI` implements `DockerAPI`, records
the order of calls, and returns preconfigured channels and readers. `trackReader`
is an `io.ReadCloser` that flips a flag when it is read to EOF and another when it
is closed, which is how `TestRunSuccess` proves the pull stream is both drained
and closed. That same test uses `slices.Index` to assert `"wait"` appears before
`"start"` in the recorded call order — the fast-exit race, checked directly.

The three exit outcomes each get a test: `TestRunNonZeroExit` asserts a status of
7 becomes an `*ExitError` that is *not* an `ErrDaemon`; `TestRunWaitChannelError`
asserts an error on the wait error channel wraps `ErrDaemon` and is *not* an
`*ExitError`; `TestRunDaemonWaitError` asserts a `WaitResponse.Error` also wraps
`ErrDaemon`. `TestRunCleanupOnCancel` is the important one: it passes an
already-cancelled context, asserts `Run` returns `context.Canceled`, that
`ContainerRemove` still ran, and — critically — that the context the remove saw
was *not* cancelled, proving `WithoutCancel` did its job. `TestRunPullError`
asserts a pull failure short-circuits before create.

Create `jobrunner_test.go`:

```go
package jobrunner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// trackReader records whether it was read to EOF and whether it was closed, so
// tests can prove the runner drains and closes the pull stream.
type trackReader struct {
	r       *bytes.Reader
	drained bool
	closed  bool
}

func newTrackReader(s string) *trackReader {
	return &trackReader{r: bytes.NewReader([]byte(s))}
}

func (t *trackReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err == io.EOF {
		t.drained = true
	}
	return n, err
}

func (t *trackReader) Close() error {
	t.closed = true
	return nil
}

// fakeAPI is a hand-written DockerAPI that records call order and returns
// preconfigured results, so the runner's lifecycle logic is tested with no
// daemon.
type fakeAPI struct {
	mu    sync.Mutex
	calls []string

	pullReader *trackReader
	pullErr    error

	createErr error
	createID  string

	startErr error

	statusCh  chan container.WaitResponse
	waitErrCh chan error

	removeCalled bool
	removeCtxErr error
}

func (f *fakeAPI) record(name string) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
}

func (f *fakeAPI) ImagePull(_ context.Context, _ string, _ image.PullOptions) (io.ReadCloser, error) {
	f.record("pull")
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	return f.pullReader, nil
}

func (f *fakeAPI) ContainerCreate(_ context.Context, _ *container.Config, _ *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
	f.record("create")
	if f.createErr != nil {
		return container.CreateResponse{}, f.createErr
	}
	return container.CreateResponse{ID: f.createID}, nil
}

func (f *fakeAPI) ContainerWait(_ context.Context, _ string, _ container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
	f.record("wait")
	return f.statusCh, f.waitErrCh
}

func (f *fakeAPI) ContainerStart(_ context.Context, _ string, _ container.StartOptions) error {
	f.record("start")
	return f.startErr
}

func (f *fakeAPI) ContainerRemove(ctx context.Context, _ string, _ container.RemoveOptions) error {
	f.record("remove")
	f.mu.Lock()
	f.removeCalled = true
	f.removeCtxErr = ctx.Err()
	f.mu.Unlock()
	return nil
}

func newFake() *fakeAPI {
	return &fakeAPI{
		pullReader: newTrackReader("pull-progress"),
		createID:   "c1",
		statusCh:   make(chan container.WaitResponse, 1),
		waitErrCh:  make(chan error, 1),
	}
}

func TestRunSuccess(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.statusCh <- container.WaitResponse{StatusCode: 0}

	code, err := New(f).Run(context.Background(), Spec{Image: "busybox"})
	if err != nil || code != 0 {
		t.Fatalf("Run = %d, %v; want 0, nil", code, err)
	}
	if !f.pullReader.drained || !f.pullReader.closed {
		t.Errorf("pull reader drained=%v closed=%v; want both true", f.pullReader.drained, f.pullReader.closed)
	}
	wait := slices.Index(f.calls, "wait")
	start := slices.Index(f.calls, "start")
	if wait < 0 || start < 0 || wait > start {
		t.Errorf("calls = %v; want wait before start", f.calls)
	}
	if !f.removeCalled {
		t.Error("ContainerRemove was not called")
	}
}

func TestRunNonZeroExit(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.statusCh <- container.WaitResponse{StatusCode: 7}

	code, err := New(f).Run(context.Background(), Spec{Image: "busybox"})
	var ee *ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("Run err = %v; want *ExitError", err)
	}
	if ee.Code != 7 || code != 7 {
		t.Errorf("code = %d, ExitError.Code = %d; want 7, 7", code, ee.Code)
	}
	if errors.Is(err, ErrDaemon) {
		t.Error("a clean non-zero exit must not wrap ErrDaemon")
	}
}

func TestRunWaitChannelError(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.waitErrCh <- errors.New("connection reset")

	_, err := New(f).Run(context.Background(), Spec{Image: "busybox"})
	if !errors.Is(err, ErrDaemon) {
		t.Fatalf("Run err = %v; want ErrDaemon", err)
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		t.Error("a transport error must not be an *ExitError")
	}
}

func TestRunDaemonWaitError(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.statusCh <- container.WaitResponse{
		StatusCode: 137,
		Error:      &container.WaitExitError{Message: "OOM killed"},
	}

	_, err := New(f).Run(context.Background(), Spec{Image: "busybox"})
	if !errors.Is(err, ErrDaemon) {
		t.Fatalf("Run err = %v; want ErrDaemon", err)
	}
}

func TestRunCleanupOnCancel(t *testing.T) {
	t.Parallel()
	f := newFake() // statusCh and waitErrCh stay empty: the wait never resolves

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // ctx is already done when Run reaches the select

	_, err := New(f).Run(ctx, Spec{Image: "busybox"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v; want context.Canceled", err)
	}
	if !f.removeCalled {
		t.Fatal("cleanup did not run after cancellation")
	}
	if f.removeCtxErr != nil {
		t.Errorf("cleanup ran on a cancelled context (%v); it must use a fresh one", f.removeCtxErr)
	}
}

func TestRunPullError(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.pullErr = errors.New("socket closed")

	_, err := New(f).Run(context.Background(), Spec{Image: "busybox"})
	if !errors.Is(err, ErrDaemon) {
		t.Fatalf("Run err = %v; want ErrDaemon", err)
	}
	if slices.Contains(f.calls, "create") {
		t.Error("ContainerCreate must not run after a pull failure")
	}
}

func ExampleJobRunner_Run() {
	f := newFake()
	f.statusCh <- container.WaitResponse{StatusCode: 3}

	_, err := New(f).Run(context.Background(), Spec{Image: "busybox"})
	var ee *ExitError
	fmt.Println(errors.As(err, &ee), ee.Code)
	// Output: true 3
}
```

### The integration test

The integration test is the real-daemon proof, kept out of the default build by
`//go:build docker`. It constructs a real client, and if `Ping` fails it calls
`t.Skip` rather than failing — so `go test -tags docker` is safe on a machine
without Docker. It then runs `busybox true` and asserts exit 0, and `busybox sh -c
'exit 7'` and asserts an `*ExitError` with code 7.

Create `integration_test.go`:

```go
//go:build docker

package jobrunner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/docker/docker/client"
)

func newRealRunner(t *testing.T) *JobRunner {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	t.Cleanup(func() { cli.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := cli.Ping(ctx); err != nil {
		t.Skipf("no reachable docker daemon: %v", err)
	}
	return New(cli)
}

func TestIntegrationExitZero(t *testing.T) {
	r := newRealRunner(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	code, err := r.Run(ctx, Spec{Image: "busybox:latest", Cmd: []string{"true"}, Memory: 64 << 20})
	if err != nil || code != 0 {
		t.Fatalf("Run(true) = %d, %v; want 0, nil", code, err)
	}
}

func TestIntegrationExitSeven(t *testing.T) {
	r := newRealRunner(t)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	code, err := r.Run(ctx, Spec{Image: "busybox:latest", Cmd: []string{"sh", "-c", "exit 7"}, Memory: 64 << 20})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 7 || code != 7 {
		t.Fatalf("Run(exit 7) = %d, %v; want 7, *ExitError{Code:7}", code, err)
	}
}
```

## Review

The runner is correct when the three exit outcomes stay distinct: a zero status is
`(0, nil)`, a non-zero status is `(code, *ExitError)` and never an `ErrDaemon`, and
anything from the daemon or transport — a create failure, a start failure, a
`WaitResponse.Error`, or a value on the wait error channel — wraps `ErrDaemon` and
never an `*ExitError`. Confirm this by reading the four error tests together: they
partition the space and assert each partition with `errors.As`/`errors.Is`. The
most common way to get this wrong is to collapse "container exited non-zero" into a
generic error, which makes a caller unable to tell a failed job from a broken
daemon.

The two structural mistakes to watch for are ordering and cleanup. Starting before
subscribing to the wait loses the exit of a fast container; the `slices.Index`
assertion in `TestRunSuccess` is there to catch a refactor that reorders them.
Running cleanup on the request context means cleanup is dead on the timeout path;
`TestRunCleanupOnCancel` checks the remove both ran and saw an uncancelled context,
which only passes if you used `context.WithoutCancel`. Run `go test -race ./...`
for the unit path, and on a Docker host `go test -tags docker -race ./...` to
exercise the real daemon.

## Resources

- [Docker Engine Go SDK client](https://pkg.go.dev/github.com/docker/docker/client) — `NewClientWithOpts`, `FromEnv`, `WithAPIVersionNegotiation`, and the container methods.
- [`api/types/container`](https://pkg.go.dev/github.com/docker/docker/api/types/container) — `Config`, `HostConfig`, `Resources`, `WaitResponse`, `WaitCondition`, and the option structs.
- [Examples using the Docker Engine SDKs](https://docs.docker.com/reference/api/engine/sdk/examples/) — the canonical run/wait/logs flow in Go.
- [`context.WithoutCancel`](https://pkg.go.dev/context#WithoutCancel) — a derived context that keeps values but drops cancellation, for cleanup paths.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-log-stream-demux.md](02-log-stream-demux.md)
