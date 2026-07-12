# 1. Linux Namespaces: UTS and PID

Every container runtime -- Docker, containerd, Podman -- uses Linux namespaces to isolate processes from the host. A namespace is not a security boundary; it is a view-of-system-resources boundary: processes in a UTS namespace see their own hostname; processes in a PID namespace see their own process tree starting at PID 1. The hard part in Go is not the syscall itself but the mismatch between Go's M:N thread scheduler and the per-thread semantics of Linux namespace system calls.

```text
namespace-demo/
  go.mod
  namespace/
    namespace.go       (//go:build linux)
    namespace_test.go  (//go:build linux)
  cmd/namespace-demo/
    main.go            (//go:build linux)
```

The `namespace` package exports `Config`, two sentinel errors, `Run` (the parent side), and `Child` (the namespace interior side). The test exercises config validation paths that do not require root. The binary dispatches between parent and child roles based on `os.Args[1]`.

## Concepts

### What a Namespace Is

A Linux namespace wraps a global resource so that processes inside it see an isolated copy of that resource. The kernel tracks six resource classes as separate namespace types:

| Flag | Namespace | Isolated resource |
|------|-----------|-------------------|
| `CLONE_NEWUTS` | UTS | hostname, NIS domain name |
| `CLONE_NEWPID` | PID | process ID tree |
| `CLONE_NEWNS` | Mount | mount points |
| `CLONE_NEWNET` | Network | network interfaces, routing, ports |
| `CLONE_NEWUSER` | User | user and group IDs |
| `CLONE_NEWIPC` | IPC | POSIX message queues, SysV IPC |

Namespaces are created via `clone(2)` or `unshare(2)` with the appropriate `CLONE_NEW*` flag. This lesson covers UTS and PID; Mount appears as a required companion because PID namespace requires remounting `/proc`.

### UTS Namespace: Hostname Isolation

The UTS namespace (named after the UNIX Timesharing System `utsname` struct) isolates the hostname and NIS domain name. A process in a new UTS namespace starts with a copy of the host's values. It can change them with `sethostname(2)` -- `syscall.Sethostname` in Go -- without affecting the host or other UTS namespaces.

```go
if err := syscall.Sethostname([]byte("container-01")); err != nil {
	return fmt.Errorf("sethostname: %w", err)
}
```

`sethostname(2)` requires `CAP_SYS_ADMIN` in the UTS namespace that is being modified.

### PID Namespace: Independent Process Tree

A PID namespace gives processes their own PID number space. The first process spawned into a new PID namespace gets PID 1 from its own perspective. From the host kernel, that process still has a normal PID; the isolation is a view difference.

PID 1 inside a namespace carries special kernel responsibilities:

- All other processes in the namespace receive `SIGKILL` when PID 1 exits.
- PID 1 must reap zombie children; there is no implicit reaping above PID 1 within the namespace.
- Signals that PID 1 does not handle with an explicit handler are silently dropped.

The critical operational consequence: `/proc` reflects the PID namespace of the process that opened it. A container's `/proc` must be remounted inside the new PID namespace for tools like `ps` and `top` to see only the container's processes, not the full host tree.

### The Threading Problem and the Re-exec Pattern

`unshare(2)` moves the calling thread into a new namespace, not the calling process. Go's runtime multiplexes goroutines across OS threads (M:N scheduling). Calling `syscall.Unshare` from a goroutine affects only the OS thread running that goroutine at that moment; other goroutines may be on other threads still in the original namespace. The result is non-deterministic namespace membership.

The correct solution is `exec.Command` with `SysProcAttr.Cloneflags`. The kernel creates the new namespace at fork+exec time, before the child's Go runtime has started any goroutines. The entire child process runs inside the new namespace from the first instruction.

```go
child := exec.Command("/proc/self/exe", "child")
child.SysProcAttr = &syscall.SysProcAttr{
	Cloneflags: syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
}
```

`/proc/self/exe` is a Linux symlink to the currently running executable. Re-executing the same binary with `"child"` as the first argument lets the child detect that it is the namespace interior and run setup code (`Sethostname`, mount `/proc`, then `syscall.Exec` the user command) instead of the parent logic.

### Mount Propagation and /proc Safety

Adding `CLONE_NEWNS` gives the child its own mount namespace, but the new namespace starts with a copy of the host's mounts that may still have shared propagation mode (`MS_SHARED`). If any ancestor mount point is shared, a `mount("proc", "/proc", ...)` inside the child propagates back to the host -- destroying the host's `/proc`.

The fix is to make all mounts private before remounting `/proc`:

```go
// Prevent /proc remount from escaping to the host.
syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, "")
// Now mount proc; visible only in this namespace.
syscall.Mount("proc", "/proc", "proc", 0, "")
```

`MS_PRIVATE|MS_REC` recursively sets all mount points in the new namespace to private propagation mode. This is required even when `CLONE_NEWNS` is set.

## Exercises

All files use `//go:build linux`. The unit tests (validation-error paths) run on any Linux host without root. The integration smoke test requires root.

### Exercise 1: The namespace Package

Create `namespace/namespace.go`:

```go
//go:build linux

package namespace

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// ErrNotRoot is returned when the calling process is not running as root.
var ErrNotRoot = errors.New("namespace: must run as root (UID 0)")

// ErrEmptyHostname is returned when Config.Hostname is empty.
var ErrEmptyHostname = errors.New("namespace: hostname must not be empty")

// Config holds parameters for launching a child in new UTS, PID, and mount
// namespaces.
type Config struct {
	// Hostname is set inside the UTS namespace. Required.
	Hostname string
	// Args is the command and arguments to exec inside the namespace.
	// Defaults to ["/bin/sh"] when empty.
	Args []string
}

func (c Config) validate() error {
	if c.Hostname == "" {
		return ErrEmptyHostname
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("%w: current UID is %d", ErrNotRoot, os.Geteuid())
	}
	return nil
}

func (c Config) resolvedArgs() []string {
	if len(c.Args) > 0 {
		return c.Args
	}
	return []string{"/bin/sh"}
}

// Run creates a child process in new UTS, PID, and mount namespaces and waits
// for it to exit.
//
// The current binary is re-executed via /proc/self/exe with "child" prepended
// to os.Args so the child can detect that it is the namespace interior process.
// The child must call [Child] when it detects that argument.
//
// The hostname is passed through the environment variable CONTAINER_HOSTNAME so
// it survives the exec without requiring command-line parsing in the child.
func Run(cfg Config) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	args := append([]string{"child"}, cfg.resolvedArgs()...)
	child := exec.Command("/proc/self/exe", args...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.Env = append(os.Environ(), "CONTAINER_HOSTNAME="+cfg.Hostname)
	child.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
	}
	if err := child.Run(); err != nil {
		return fmt.Errorf("namespace: child exited: %w", err)
	}
	return nil
}

// Child configures the UTS and mount namespaces from inside the child process
// and replaces the process image with the user command via [syscall.Exec].
//
// On success, Child never returns: syscall.Exec replaces the current image.
// The caller becomes the user command and takes on PID 1 within the namespace.
func Child(hostname string, args []string) error {
	if hostname == "" {
		hostname = "container"
	}
	if len(args) == 0 {
		args = []string{"/bin/sh"}
	}
	if err := syscall.Sethostname([]byte(hostname)); err != nil {
		return fmt.Errorf("namespace: sethostname %q: %w", hostname, err)
	}
	// Make all mounts private so the /proc remount does not propagate back
	// to the host mount namespace through shared mount points.
	if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("namespace: make mounts private: %w", err)
	}
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		return fmt.Errorf("namespace: mount /proc: %w", err)
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		return fmt.Errorf("namespace: lookpath %s: %w", args[0], err)
	}
	// Exec replaces the current process image. This process becomes args[0]
	// as PID 1 within the namespace.
	return syscall.Exec(path, args, os.Environ())
}
```

`validate()` checks the empty-hostname invariant before the root check so that non-root users get a descriptive error for misconfigured calls, not only a permissions error. `Child` uses `syscall.Exec` (not `exec.Command`) to replace the current process image; the child becomes the user command without forking another process, so it is truly PID 1 in the namespace.

### Exercise 2: Tests

Create `namespace/namespace_test.go`:

```go
//go:build linux

package namespace

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestValidateEmptyHostname(t *testing.T) {
	t.Parallel()

	cfg := Config{}
	if err := cfg.validate(); !errors.Is(err, ErrEmptyHostname) {
		t.Fatalf("validate() = %v, want ErrEmptyHostname", err)
	}
}

func TestValidateNonRoot(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root; cannot test non-root rejection")
	}
	cfg := Config{Hostname: "mybox"}
	if err := cfg.validate(); !errors.Is(err, ErrNotRoot) {
		t.Fatalf("validate() = %v, want ErrNotRoot", err)
	}
}

func TestResolvedArgsReturnsDefault(t *testing.T) {
	t.Parallel()

	cfg := Config{Hostname: "x"}
	got := cfg.resolvedArgs()
	if len(got) != 1 || got[0] != "/bin/sh" {
		t.Fatalf("resolvedArgs() = %v, want [/bin/sh]", got)
	}
}

func TestResolvedArgsRespectsOverride(t *testing.T) {
	t.Parallel()

	cfg := Config{Hostname: "x", Args: []string{"/bin/echo", "hello"}}
	got := cfg.resolvedArgs()
	if len(got) != 2 || got[0] != "/bin/echo" || got[1] != "hello" {
		t.Fatalf("resolvedArgs() = %v, want [/bin/echo hello]", got)
	}
}

func TestRunRejectsEmptyHostname(t *testing.T) {
	t.Parallel()

	if err := Run(Config{}); !errors.Is(err, ErrEmptyHostname) {
		t.Fatalf("Run(Config{}) = %v, want ErrEmptyHostname", err)
	}
}

func TestRunRejectsNonRoot(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root; cannot test non-root rejection")
	}
	if err := Run(Config{Hostname: "box"}); !errors.Is(err, ErrNotRoot) {
		t.Fatalf("Run(Config{Hostname:box}) = %v, want ErrNotRoot", err)
	}
}

func ExampleConfig_hostname() {
	cfg := Config{
		Hostname: "container-01",
		Args:     []string{"/bin/sh"},
	}
	fmt.Println(cfg.Hostname)
	// Output:
	// container-01
}
```

Your turn: add `TestChildDefaultHostname` that calls `Child("", nil)` from a test helper that intercepts the `syscall.Sethostname` call. To keep the test hermetic without actually entering a namespace, factor out the hostname-resolution logic into an exported function `ResolveHostname(s string) string` and test that instead.

### Exercise 3: The CLI Binary

Create `cmd/namespace-demo/main.go`:

```go
//go:build linux

// namespace-demo creates a child process in new UTS and PID namespaces.
//
// Usage (as root):
//
//	sudo go run ./cmd/namespace-demo --hostname mybox /bin/sh
//
// Inside the shell, hostname(1) returns "mybox" and echo $$ returns 1.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"example.com/namespace-demo/namespace"
)

func main() {
	// Re-exec child path: Run calls /proc/self/exe with "child" as the first
	// argument. Detect that here and hand off to namespace.Child.
	if len(os.Args) > 1 && os.Args[1] == "child" {
		hostname := os.Getenv("CONTAINER_HOSTNAME")
		if err := namespace.Child(hostname, os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "namespace-demo child: %v\n", err)
			os.Exit(1)
		}
		return // unreachable: Child calls Exec and never returns on success
	}

	// Parent path: parse flags and launch the namespace.
	hostname := flag.String("hostname", "container", "hostname for the new UTS namespace")
	flag.Parse()

	cfg := namespace.Config{
		Hostname: *hostname,
		Args:     flag.Args(),
	}

	if err := namespace.Run(cfg); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				os.Exit(ws.ExitStatus())
			}
		}
		fmt.Fprintf(os.Stderr, "namespace-demo: %v\n", err)
		os.Exit(1)
	}
}
```

The exit-code block unwraps `*exec.ExitError` and extracts `WaitStatus.ExitStatus()` so that scripts checking the container exit code get the actual value, not a hardcoded 1.

## Common Mistakes

### Using unshare Instead of Cloneflags

Wrong: calling `syscall.Unshare(syscall.CLONE_NEWUTS)` from a goroutine.

What happens: `unshare(2)` is a per-thread syscall. The Go scheduler may run the goroutine on a different OS thread before or after the call. Other goroutines remain on their original threads in the original namespaces. Namespace membership is non-deterministic.

Fix: set `SysProcAttr.Cloneflags` on the child `exec.Command`. The kernel enters the new namespace at fork+exec time, before the child Go runtime has any goroutines.

### Omitting CLONE_NEWNS When Mounting /proc

Wrong: `Cloneflags: syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID` without `syscall.CLONE_NEWNS`, then calling `syscall.Mount("proc", "/proc", ...)`.

What happens: the child shares the host's mount namespace. The mount call modifies the host's `/proc`, breaking `ps`, `top`, `/proc/<pid>` lookups, and any kernel interface that reads `/proc` by PID on the host.

Fix: always include `syscall.CLONE_NEWNS` when remounting `/proc`.

### Skipping the Private Propagation Step

Wrong: including `CLONE_NEWNS` but mounting `/proc` immediately without the `MS_PRIVATE|MS_REC` step.

What happens: the new mount namespace inherits the host's propagation settings. If any parent mount point has `MS_SHARED` mode, the `/proc` remount propagates back to the host despite `CLONE_NEWNS`. The symptom is that the host's `/proc` shows only the container's processes after the child starts.

Fix: call `syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, "")` before any new mounts inside the child.

### Using exec.Command Inside Child Instead of syscall.Exec

Wrong: inside `Child`, creating `exec.Command(args[0], args[1:]...)` and running it.

What happens: `exec.Command` forks a new process. The child of the re-exec is no longer PID 1 in the namespace -- the grandchild is. If the child (now PID 1) exits while the grandchild is still running, the grandchild gets `SIGKILL` immediately.

Fix: use `syscall.Exec(path, args, os.Environ())` to replace the child process image in place. The child process becomes the user command and retains PID 1.

### Not Propagating the Child Exit Code

Wrong: `os.Exit(1)` unconditionally when `namespace.Run` returns an error.

What happens: the exit status of the container command is always 1 regardless of what the command returned. Shell scripts that check exit codes see wrong values.

Fix: unwrap `*exec.ExitError` with `errors.As`, extract `exitErr.Sys().(syscall.WaitStatus).ExitStatus()`, and pass that value to `os.Exit`.

## Verification

These commands run on a Linux host. Unit tests (validation paths) work as a non-root user; integration tests require root.

```bash
cd ~/go-exercises/namespace-demo

# Format check (no output means clean)
test -z "$(gofmt -l .)"

# Vet
go vet ./...

# Unit tests (non-root: skips root-required cases)
go test -count=1 -race ./namespace/...

# Build the binary
go build ./cmd/namespace-demo

# Integration smoke test -- requires root
sudo ./namespace-demo --hostname mybox /bin/sh -c '
  echo "hostname: $(hostname)"
  echo "pid: $$"
  ps aux
'
```

Expected output from the integration test:

```
hostname: mybox
pid: 1
USER       PID %CPU %MEM    VSZ   RSS TTY      STAT START   TIME COMMAND
root         1  0.0  0.0   2220   740 ?        Ss   00:00   0:00 /bin/sh -c ...
```

After the child exits, verify the host is unaffected:

```bash
hostname  # prints the host's original name, not mybox
```

## Summary

- A Linux namespace wraps a global system resource; processes inside it see an isolated copy. UTS isolates the hostname; PID isolates the process ID tree; Mount isolates mount points.
- Go's M:N thread scheduler makes `unshare(2)` unreliable inside a goroutine. The correct approach is `exec.Command` with `SysProcAttr.Cloneflags`, which moves the child into the new namespaces at fork+exec time before the Go runtime starts.
- The child is the same binary re-executed via `/proc/self/exe` with a sentinel argument. It calls `Child` to set the hostname, make mounts private, remount `/proc`, and exec the user command via `syscall.Exec`.
- `CLONE_NEWNS` must accompany `CLONE_NEWPID` to isolate the `/proc` remount. `MS_PRIVATE|MS_REC` prevents the remount from leaking back to the host even after `CLONE_NEWNS`.
- PID 1 in any namespace receives `SIGKILL` when it exits, terminating all other processes in the namespace. Using `syscall.Exec` (not `exec.Command`) keeps the re-exec child as PID 1 when the user command starts.

## What's Next

Next: [Mount Namespace and Root Filesystem](../02-mount-namespace-root-filesystem/02-mount-namespace-root-filesystem.md).

## Resources

- [namespaces(7) -- Linux man page](https://man7.org/linux/man-pages/man7/namespaces.7.html) -- complete catalog of namespace types and their semantics
- [pid_namespaces(7) -- Linux man page](https://man7.org/linux/man-pages/man7/pid_namespaces.7.html) -- PID 1 semantics, zombie reaping, /proc behavior
- [clone(2) -- Linux man page](https://man7.org/linux/man-pages/man2/clone.2.html) -- `CLONE_NEW*` flag reference and fork+exec semantics
- [syscall package -- pkg.go.dev](https://pkg.go.dev/syscall) -- `SysProcAttr`, `Cloneflags`, `Sethostname`, `Mount`, `Exec` signatures
- [Containers from Scratch -- Liz Rice (GitHub)](https://github.com/lizrice/containers-from-scratch) -- canonical Go reference implementation for the re-exec pattern
