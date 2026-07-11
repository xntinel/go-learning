# Driving Docker with the Engine SDK — Concepts

The Docker Engine Go SDK (`github.com/docker/docker/client`) is the substrate
under CI job runners, ephemeral task executors, integration-test harnesses (the
testcontainers pattern), build systems, and sandboxes for untrusted code. For a
senior backend engineer the interesting question is not "how do I call
`docker run` from Go" — it is how to treat the daemon as what it actually is: an
unreliable, privileged, remote service reached over a socket. Every method is an
HTTP request; the socket is a root-equivalent trust boundary; and cancelling a
context cancels the request, not the container it launched. Getting those three
facts wrong is how you end up with orphaned containers, garbled logs, missed exit
codes, and a service that is one crafted input away from running arbitrary code
as root on the host.

This file is the conceptual foundation for the three independent exercises that
follow: an ephemeral job runner, a log demultiplexer, and an exec-based readiness
probe. Read it once and you have the model you need for all three.

## The daemon is HTTP over a socket

The Docker Engine API is a REST API served over a Unix domain socket
(`/var/run/docker.sock`) by default, or over TCP when `DOCKER_HOST` points at a
`tcp://` address. The Go SDK is a thin, typed client over that HTTP surface:
`(*client.Client).ContainerCreate` is a `POST /containers/create`,
`ContainerWait` is a `POST /containers/{id}/wait` whose response streams back
when the container stops, and so on. Nothing about the client is magic; it
marshals a struct to JSON, sends a request, and decodes the reply.

Internalizing that one fact explains almost every design constraint in this
lesson. Every method takes a `context.Context` because every method is a network
call that can hang, and you need deadlines and cancellation. Errors can be
transport errors (socket gone, daemon restarting) as easily as they are
application errors (no such image). And because it is HTTP, cancellation follows
HTTP semantics — which is the single most misunderstood point below.

## API version negotiation is not optional

The client is compiled against a specific Engine API version and sends that
version in the request path (`/v1.51/containers/create`). The daemon on the
other end has its own supported range. If the client's pinned version is newer
than the daemon supports, or older than the daemon's minimum, calls fail with a
version-mismatch error before your logic ever runs. This bites in production
constantly: your binary is built once and then runs against whatever daemon a
given host, CI runner, or developer laptop happens to have.

The fix is to construct the client with `client.WithAPIVersionNegotiation()`,
which makes the client issue a `Ping` on its first call and downgrade its pinned
version to what the daemon actually supports. Alternatively you can pin
explicitly with the `DOCKER_API_VERSION` environment variable. A production
client is almost always built as:

```
cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
```

`client.FromEnv` reads `DOCKER_HOST`, `DOCKER_TLS_VERIFY`, and `DOCKER_CERT_PATH`
so the same binary talks to a local socket or a remote daemon depending on the
environment — which, note, is a security-relevant configuration surface.

## Context cancellation cancels the request, not the workload

This is the trap that produces orphaned containers. When you pass a context to
`ContainerWait` and that context is cancelled or its deadline expires, the SDK
aborts the in-flight HTTP request. It does **not** stop, kill, or remove the
container. The workload keeps running on the host; you have simply stopped
listening for its result. A job runner that treats "my `ContainerWait` returned
`context.DeadlineExceeded`" as "the job is dead" leaks a live container every
time it times out, and those accumulate until the host runs out of resources.

Two consequences follow. First, stopping a workload is an explicit act:
`ContainerStop` or `ContainerRemove` with `Force: true`. Second — and this is the
subtle one — cleanup must run on a context that is *not* the one that just
expired. If you `defer` a `ContainerRemove(ctx, ...)` using the same `ctx` whose
deadline blew, the remove call is born already cancelled and fails, so cleanup
silently does nothing on exactly the path where it matters most. Use a fresh
context, or `context.WithoutCancel(ctx)` (Go 1.21+), which keeps the parent's
values but drops its cancellation and deadline, so the cleanup call still has
time to complete.

## ContainerWait returns two channels, and order matters

`ContainerWait` has an unusual signature: it returns
`(<-chan container.WaitResponse, <-chan error)` — two receive-only channels, not
a single result. The daemon streams the exit status on the first channel; a
transport or daemon-side failure arrives on the second. You must `select` on
both. Reading only the status channel means a daemon failure is silently dropped
and you block forever; reading only the error channel means you never see the
exit code. Also `select` on `ctx.Done()`, so a deadline unblocks you (remembering
the point above: that unblock abandons the wait, it does not stop the container).

The other half of using `ContainerWait` correctly is *ordering*. You must
subscribe to the wait **before** you start the container. A container that runs
`true` and exits in a few milliseconds can finish before your wait request even
reaches the daemon; if you start first and wait second, the exit event is gone
and your wait hangs until it times out. The correct sequence is: create, then
`ContainerWait(ctx, id, container.WaitConditionNotRunning)` to register the
subscription, then `ContainerStart`, then `select` on the channels. Registering
the wait first is what closes the fast-exit race.

## Streams are multiplexed; you must demultiplex them

`ContainerLogs`, and the attached stream from an exec, do not hand you clean
stdout. For a container created **without** a TTY, the daemon frames the combined
output: each chunk is prefixed by an 8-byte header whose first byte says whether
the chunk is stdout or stderr and whose last four bytes are the chunk length.
Copying that byte stream straight to `os.Stdout` produces output corrupted by the
header bytes interleaved with your text. The `github.com/docker/docker/pkg/stdcopy`
package exists precisely for this: `stdcopy.StdCopy(dstout, dsterr, src)` reads
the framed stream and writes the destreamed stdout and stderr to two separate
writers.

For a container created **with** a TTY (`Config.Tty == true`), there is no
framing at all — stdout and stderr are merged into one raw stream, because a TTY
is a single character device. For that case you must copy the stream directly and
must *not* run it through `StdCopy`, which would misinterpret the raw bytes as
frame headers. So the demux path is a branch: inspect `Config.Tty` (via
`ContainerInspect`) and choose raw copy or `StdCopy` accordingly. Building the
multiplexed form yourself for tests is easy: `stdcopy.NewStdWriter(w, stdcopy.Stdout)`
returns a writer that frames whatever you write to it as stdout, which lets you
test the demultiplexer with no daemon at all.

## Everything streaming must be closed

Several methods return something you are responsible for closing, and leaking
them leaks file descriptors and goroutines in a long-running service:

- `ImagePull`, `ContainerLogs`, and `CopyFromContainer` return an
  `io.ReadCloser`. Close it.
- `ContainerExecAttach` returns a `types.HijackedResponse` that owns a hijacked
  connection; call its `Close()` method.
- The `*client.Client` itself holds a connection pool; `Close()` it on shutdown.

`defer rc.Close()` immediately after checking the error from the call that
returned it is the habit that keeps a service from slowly exhausting its fd
budget.

## ImagePull returns a progress stream you must drain

`ImagePull` does not block until the image is present. It returns immediately with
a reader that carries the pull's progress as a JSON stream, and the pull is not
complete until that reader reaches EOF. The classic bug is to fire `ImagePull`,
ignore the reader, and call `ContainerCreate` right away — which races the pull
and fails with "no such image" because the layers have not finished downloading.
You must read the reader to completion (`io.Copy(io.Discard, rc)` if you do not
care about progress, or decode it if you want to show a progress bar) and then
close it before you assume the image exists.

## Exit-code semantics: three distinct outcomes

A robust runner distinguishes three things that a naive one conflates:

- A **successful daemon call with a non-zero exit code**. This is
  `WaitResponse.StatusCode != 0` with `WaitResponse.Error == nil`. The daemon did
  its job; the container's process exited non-zero. This is a normal, expected
  result, not an infrastructure failure — map it to a typed exit error carrying
  the code, not to a generic "docker failed".
- A **daemon-side wait error**: `WaitResponse.Error != nil` (a
  `*container.WaitExitError` whose `Message` explains it). The daemon accepted the
  wait but reports it could not determine the status.
- A **transport/daemon failure**: a value arriving on the *error* channel of
  `ContainerWait`. The call itself failed.

For an exec, the exit code is not carried by the attached stream at all. The
attach stream reaching EOF only tells you the process finished; to read the code
you call `ContainerExecInspect` and read `ExitCode` once `Running` is false.
Reading the exit status "from the EOF" is impossible; you must poll the inspect
endpoint.

## AutoRemove trades post-mortem data for convenience

`HostConfig.AutoRemove` makes the daemon delete the container the instant it
exits — the `--rm` of `docker run`. It is convenient for fire-and-forget jobs,
but it races anything that reads the container after it exits: a later
`ContainerLogs` or `ContainerInspect` can fail because the container (and its
logs) are already gone. Note the nuance that the job runner exercise depends on:
a `ContainerWait` *subscribed before start* still reliably delivers the exit code
even with `AutoRemove`, because the daemon sends the status as part of the stop
event. It is the post-exit reads — logs, inspect — that lose the race. So the
rule is: if you need post-mortem data, do an explicit remove instead of
`AutoRemove` and read what you need first; if you only need the exit code from a
pre-subscribed wait, `AutoRemove` is fine. Either way, keep an explicit
best-effort remove on the cleanup path for the cases `AutoRemove` never covers
(create succeeded but start failed; you abandoned the wait on a deadline).

## Trust boundary and blast radius

Access to the Docker socket is effectively root on the host. Anyone who can talk
to the daemon can bind-mount `/` into a container, run a privileged container, or
otherwise escape to the host. A service that launches containers on behalf of
user input is therefore running attacker-influenced code with root-equivalent
authority, and the socket is a security boundary you are choosing to sit on top
of. The mitigations belong in the `HostConfig` you send with every create:
resource limits (`Resources.Memory`, `Resources.NanoCPUs`, `Resources.PidsLimit`)
to bound a runaway or a fork bomb, `ReadonlyRootfs`, dropped capabilities
(`CapDrop`), no privileged mode, and no bind mounts of host paths. None of these
are defaults; the SDK will happily create an unbounded, privileged container if
you leave the fields zero. `NanoCPUs` is expressed in billionths of a CPU, so one
core is `1_000_000_000`; `Memory` is bytes.

## Naming and idempotency

Creating a container with a name that already exists returns a `409 Conflict`. A
runner that retries by name will fail the second time. The two idempotent options
are: pass an empty name and let the daemon assign an ID (simplest for
run-to-completion jobs), or handle the conflict explicitly (remove-then-create,
or reuse the existing container). Empty-name-plus-generated-ID is the right
default for ephemeral jobs.

## The package layout has moved

Tutorials older than a couple of years put option and response types under the
top-level `types` package (`types.ContainerListOptions`, `types.ImagePullOptions`),
and that code no longer compiles against current `github.com/docker/docker`. The
types now live in capability-scoped packages:

- `api/types/container` — `Config`, `HostConfig`, `Resources`, `CreateResponse`,
  `WaitResponse`, `WaitCondition`, `StartOptions`, `RemoveOptions`,
  `LogsOptions`, `ExecOptions`, `ExecInspect`, `InspectResponse`.
- `api/types/image` — `PullOptions`.
- `api/types/network` — `NetworkingConfig`.
- `api/types` — `HijackedResponse`, `Ping`.

The `ocispec.Platform` argument to `ContainerCreate` comes from
`github.com/opencontainers/image-spec/specs-go/v1`. Reach for the scoped package
first; if an import looks like the stale top-level form, it is from an outdated
source.

## Hide the daemon behind a narrow port

The reason all of this is testable is architectural, and it mirrors this repo's
hexagonal layout. If your lifecycle logic — wait-before-start ordering, the
dual-channel select, exit-code mapping, cleanup-on-cancel — calls
`*client.Client` directly, you cannot unit-test any of it without a live daemon.
Instead, define a narrow interface with only the methods you use (`ImagePull`,
`ContainerCreate`, `ContainerStart`, `ContainerWait`, `ContainerRemove`) and
depend on that. Because the interface mirrors the client's method set exactly,
`*client.Client` satisfies it with no adapter, and a hand-written fake satisfies
it in tests. The business logic becomes unit-testable against the fake; a thin
integration test against a real daemon sits behind a `//go:build docker` build
tag and is skipped when a `Ping` fails. That split — pure orchestration in front,
a build-tagged integration test behind — is the whole discipline.

## Common Mistakes

### Constructing the client without version negotiation

Wrong: `client.NewClientWithOpts(client.FromEnv)` and shipping it. The moment the
binary meets a daemon whose API version differs from the compiled-in pin, calls
fail with a version-mismatch error. Fix: add `client.WithAPIVersionNegotiation()`
(or set `DOCKER_API_VERSION`), so the client pings and downgrades on first use.

### Copying non-TTY logs straight to stdout

Wrong: `io.Copy(os.Stdout, logsReader)` for a container without a TTY. The 8-byte
frame headers are copied verbatim and corrupt the output. Fix: run the stream
through `stdcopy.StdCopy(os.Stdout, os.Stderr, logsReader)`, and take the raw-copy
path only when `Config.Tty` is true.

### Starting before subscribing to Wait

Wrong: `ContainerStart` then `ContainerWait`. A fast-exiting container finishes
before the wait registers and the exit event is lost. Fix: `ContainerWait` first
(it only subscribes), then `ContainerStart`, then `select` on the channels.

### Reading only one Wait channel

Wrong: `resp := <-statusCh` with no case for the error channel. A daemon failure
is dropped and the goroutine blocks forever. Fix: `select` on both the status
channel and the error channel, plus `ctx.Done()`.

### Not draining the pull stream

Wrong: calling `ImagePull` and immediately `ContainerCreate`. The pull has not
finished, so create fails with "no such image". Fix: `io.Copy(io.Discard, rc)`
(or decode progress) to EOF, then `rc.Close()`, before creating.

### Leaking readers and hijacked connections

Wrong: ignoring the `io.ReadCloser` from logs/pull/copy or the
`types.HijackedResponse` from exec attach. In a long-lived service this leaks file
descriptors and goroutines until it falls over. Fix: `defer` the `Close()`
immediately after the error check.

### Assuming cancellation kills the container

Wrong: treating a cancelled `ContainerWait` as "the workload stopped". It only
aborts the HTTP request; the container runs on. Fix: stop or remove the container
explicitly, and do it on a fresh context (`context.WithoutCancel`) so cleanup
still runs on the timeout path.

### Reusing the expired context for cleanup

Wrong: `defer cli.ContainerRemove(ctx, id, ...)` with the same `ctx` whose
deadline just fired. The remove is cancelled before it starts. Fix: derive a
fresh context for cleanup.

### AutoRemove plus post-mortem reads

Wrong: setting `AutoRemove: true` and then calling `ContainerLogs` or
`ContainerInspect` after the container exits. The daemon may already have deleted
it. Fix: use explicit removal when you need post-mortem data and read it before
removing; reserve `AutoRemove` for fire-and-forget or for a pre-subscribed wait.

### Reading an exec exit code from the stream EOF

Wrong: assuming the attached exec stream carries the exit status. It does not; EOF
only means the process finished. Fix: `ContainerExecInspect` and read `ExitCode`
once `Running` is false, polling until then within the deadline.

### Using moved/deprecated type names

Wrong: `types.ContainerListOptions`, `types.ImagePullOptions` from an old blog
post. They no longer exist at that path. Fix: use the scoped packages —
`container.LogsOptions`, `image.PullOptions`, and so on.

### Depending on *client.Client throughout

Wrong: threading `*client.Client` into every function, so nothing is unit-testable
without a daemon. Fix: define a narrow interface of only the methods you call and
depend on that; the real client satisfies it directly and a fake satisfies it in
tests.

Next: [01-ephemeral-job-runner.md](01-ephemeral-job-runner.md)
