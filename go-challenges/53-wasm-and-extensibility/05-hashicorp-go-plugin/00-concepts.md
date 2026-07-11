# Process Plugins with hashicorp/go-plugin — Concepts

The extensibility mechanisms earlier in this chapter kept the guest *inside* your
process: a Wasm module runs in a sandbox in your address space, and Go's own
`plugin` package (ELF `.so`) links a shared object into your running binary. Both
share one crash domain. A panic in linked third-party code, an out-of-bounds
write in a mis-compiled `.so`, or a memory leak in a plugin all land on *your*
process. `hashicorp/go-plugin` makes the opposite trade: it launches the plugin
as a **separate operating-system process** and talks to it over local gRPC. This
is the design behind Terraform providers, Vault, Nomad, and Packer — systems that
must run vendor binaries the host team did not compile, and survive those
binaries misbehaving. The senior question this raises is not "how do I call a
function in a plugin" but "what is the blast radius when the plugin is buggy,
malicious, or written in another language, and how do I supervise it". This file
is the conceptual foundation for the three independent exercises that follow.

## Concepts

### Why a whole process: isolation and crash domains

The core reason to pay for a second process is fault isolation. An in-process Go
`.so` plugin (or any linked C library) runs with your goroutines, your heap, and
your signal handlers; a segfault or a `panic` that unwinds past a `recover` takes
the host down with it, and a slow memory leak in the plugin is indistinguishable
from a leak in your own code. An out-of-process plugin has its own address space
and its own crash domain. It can panic, leak, deadlock, or be OOM-killed by the
kernel, and the worst the host sees is a broken connection on its next call. That
same boundary is where operating-system controls attach: you can wrap the child
in `ulimit`s, place it in a cgroup with a memory and CPU cap, or apply a seccomp
profile, none of which is possible for code linked into your own binary. You are
buying containment, and you pay for it in marshaling cost and process management.

### The transport is local gRPC, so the boundary must be coarse

Every call to the plugin is serialized to protobuf, written to a socket (a Unix
domain socket by default, or loopback TCP), read back, and deserialized. That is
cheap relative to a network hop but enormous relative to a Go function call. The
design consequence is non-negotiable: **the plugin interface must be
coarse-grained**. An interface with a per-record `Get`/`Set` invites a caller to
make ten thousand round trips where one batched call would do, and the
serialization plus context-switch cost will dominate the actual work. Design the
surface around batches and streams — `Process([]Record)`, not `Process(Record)`
in a loop — the same way you would design a network API, because that is exactly
what it is.

### The handshake is a compatibility and safety gate

Before any RPC, host and plugin perform a handshake defined by a
`plugin.HandshakeConfig`. It carries two things. First, a *magic cookie*
(`MagicCookieKey` / `MagicCookieValue`): the plugin refuses to start unless the
host sets the matching environment variable, so the plugin binary is not usefully
runnable as a standalone tool and is harder to trick into executing. It is an
anti-accident and mild anti-abuse guard, not encryption. Second, a
`ProtocolVersion` — the ABI contract. When you change the plugin interface or the
protobuf schema, you bump the version; an old plugin then fails the handshake with
a clear, human-readable version-mismatch error instead of connecting and
mis-marshaling at runtime. `VersionedPlugins` (a `map[int]plugin.PluginSet`) lets
one host speak several versions and negotiate the highest the plugin also
supports, which is how you roll out a new ABI without a flag day.

### The trust model: you are exec'ing an arbitrary binary

`plugin.NewClient` runs an executable that a third party may have produced. Two
mechanisms defend that surface. `SecureConfig{Checksum, Hash}` pins the plugin's
SHA-256: the host computes the digest of the file on disk and refuses to launch if
it does not match the pinned value, so a swapped or tampered binary is rejected
*before* it ever runs — a supply-chain defense. `AutoMTLS` (a single boolean on
`ClientConfig`) makes go-plugin generate a one-time certificate pair per launch
and require mutual TLS on the plugin socket, so a different local process cannot
connect to the plugin and impersonate the host, and the plugin cannot be talked to
by anything but its launcher. AutoMTLS is mutually exclusive with a manual
`TLSConfig` and with `Reattach` (there is no launch during which to mint certs).
In production you want both checksum pinning and AutoMTLS; shipping without them
means exec'ing an unpinned binary on an unauthenticated local socket.

### Lifecycle is process supervision

Treat the plugin the way an init system treats a service.
`plugin.NewClient(cfg)` builds the supervisor; `(*Client).Client()` launches the
process (if a `Cmd` was given), performs the handshake, and returns a
`ClientProtocol` connection; `Dispense(name)` hands back the typed interface for
one named plugin. `(*ClientProtocol).Ping()` is the liveness probe.
`(*Client).Kill()` terminates the child, and `plugin.CleanupClients()` — deferred
in `main` and ideally also run from a signal handler — reaps every client the
process started. `StartTimeout` bounds how long you wait for a plugin to complete
its handshake, so a binary that hangs on start fails fast instead of blocking the
host. If the host dies without calling `Kill`, plugins can be orphaned; `Managed`
clients and signal handling are what keep a long-running server from leaking child
processes across restarts.

### Directionality: the plugin can call back into the host

The default flow is host-calls-plugin, but real extensibility usually needs the
reverse: the plugin needs logging, storage, or a secret from the host. Giving the
plugin ambient access to those would defeat the isolation you just paid for.
`GRPCBroker` provides the disciplined alternative. The host stands up a small gRPC
service, allocates a stream id with `NextId()`, serves it with
`AcceptAndServe(id, ...)`, and passes the id to the plugin inside a request; the
plugin `Dial`s that id back and calls the host service. This is how you build a
*capability-scoped* host API — the plugin can do exactly the operations you chose
to expose, and nothing more.

### Failure modes: every call is a fallible RPC

A plugin crash mid-call does not arrive as a Go `panic` you can `recover`. It
arrives as a gRPC transport error on the host side. So the host must treat every
call the way it treats a network call: pass a `context` with a deadline, and have
a policy — retry, restart with backoff, or fail the request — for when the call
returns an error or the connection is gone. A *hung* plugin (one that accepts the
call and never answers) is caught only by the context deadline, and a plugin that
never finishes its handshake is caught only by `StartTimeout`. Error identity does
not survive the boundary either: a sentinel error the plugin returns becomes a
gRPC status on the host, so `errors.Is` against the plugin's sentinel will not
match unless you deliberately re-encode the error code. Validate and classify on
the host side of the wire.

### Testing without flaky subprocess builds

Spawning a freshly-compiled plugin binary in a unit test is slow and fragile: it
depends on build tags, `PATH`, and the compiler being present, and it flakes under
load. go-plugin ships an in-process harness for exactly this. Set
`ServeConfig.Test = &plugin.ServeTestConfig{ReattachConfigCh: ch, CloseCh: closeCh}`
and run `plugin.Serve` in a goroutine; it serves the plugin *in your test process*
and sends back a `*plugin.ReattachConfig`. The client then sets
`ClientConfig.Reattach = <-ch` and connects to that already-running server with no
`exec` at all. The round trip is deterministic and fast. Reserve real binary
spawning — and the AutoMTLS/SecureConfig end-to-end path, which cannot use
reattach — for integration tests behind a build tag.

### Cost/benefit versus the in-process options

Put the three approaches from this chapter side by side. Wasm is in-process and
sandboxed with a restricted syscall surface, near-native speed, but every call is
marshaling-bound and the guest is confined to what the host imports. go-plugin is
a full OS process: any language (the guest can be Python), the full syscall
surface unless you deliberately restrict it, real memory isolation, at the cost of
heavier startup and per-call latency. Go's stdlib `plugin` package is the
brittle middle — same process, same runtime, but requiring the exact same Go
compiler version and dependency versions as the host, which makes it unusable for
third-party binaries. Choose go-plugin when you need language independence, hard
memory isolation, or OS-level resource controls; choose Wasm when you need a tight
sandbox with lower per-call cost and control the guest toolchain.

### Observability

By default the plugin's stdout, stderr, and standard `log` output are piped back
to the host, so a plugin's diagnostics do not vanish into a detached terminal. If
you wire `hclog` on both the host (`ClientConfig.Logger`) and the plugin
(`ServeConfig.Logger`), the two log streams are structured and correlated, and you
can filter plugin logs by level and name alongside the host's. Losing plugin
output — letting it write to a tty nobody reads — is one of the most common
operability gaps when a third-party plugin misbehaves in production.

## Common Mistakes

### Treating the gRPC boundary as free

Wrong: exposing a fine-grained interface and calling it per record, so the caller
makes thousands of round trips. The serialization and context-switch cost swamps
the work. Fix: design a coarse interface around batches and streams and cross the
boundary as few times as possible.

### Leaking orphaned plugin processes

Wrong: forgetting `defer client.Kill()` and/or `plugin.CleanupClients()`. When the
host exits or is signaled, the child processes keep running. Fix: `defer
plugin.CleanupClients()` in `main`, `Kill` each client when done, and handle
signals in long-running servers.

### Not pinning the protocol to gRPC

Wrong: leaving `AllowedProtocols` unset, so the client may try net/rpc or refuse
to negotiate, producing confusing errors. Fix: set
`AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC}` on the client.

### Forgetting the net/rpc opt-out on the plugin type

Wrong: a gRPC-only plugin type that does not satisfy the base `plugin.Plugin`
interface, so it will not compile into a `PluginSet`. Fix: embed
`plugin.NetRPCUnsupportedPlugin` in the plugin struct so its `Server`/`Client`
methods are stubbed out.

### Expecting a plugin crash to be recoverable in-process

Wrong: wrapping a plugin call in `recover()` and expecting to catch its panic. The
crash arrives as a transport error, not a panic. Fix: use context deadlines and a
restart/backoff policy, and classify the returned error.

### Shipping without SecureConfig or AutoMTLS

Wrong: exec'ing an unpinned binary and exposing an unauthenticated local socket.
That is a supply-chain and lateral-movement risk. Fix: pin the SHA-256 with
`SecureConfig` and enable `AutoMTLS` in production.

### Combining AutoMTLS with Reattach or a manual TLSConfig

Wrong: setting `AutoMTLS: true` alongside `Reattach` or `TLSConfig`. These are
mutually exclusive and fail at connection time. Fix: use AutoMTLS only on the
launch path; use the reattach harness (no AutoMTLS) for deterministic tests.

### Changing the interface without bumping ProtocolVersion

Wrong: editing the proto or the Go interface but leaving `ProtocolVersion`
unchanged, so an old plugin connects and mis-marshals at runtime. Fix: bump the
version (or add a new entry to `VersionedPlugins`) so incompatible plugins fail
fast at the handshake with a readable error.

### Spawning real subprocesses in unit tests

Wrong: building and exec'ing a plugin binary in every unit test — slow, flaky,
`PATH`- and build-tag-dependent. Fix: use `ServeConfig.Test` with a
`ReattachConfigCh` for deterministic in-process tests; keep real spawning behind a
build tag.

### Dropping plugin logs

Wrong: letting plugin stdout/stderr go nowhere, so a misbehaving third-party
plugin gives you no signal. Fix: wire `hclog` on both sides so logs are structured
and correlated in the host's stream.

Next: [01-grpc-plugin-contract.md](01-grpc-plugin-contract.md)
