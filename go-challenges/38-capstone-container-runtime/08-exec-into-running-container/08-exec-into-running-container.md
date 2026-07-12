# 8. Exec into Running Container

Entering a running container's namespaces is the mechanism behind `docker exec`. A new process is spawned and joined to every namespace the container already owns, sharing its filesystem view, network stack, PID tree, and hostname — without disturbing the running container process. The core system call is `setns(2)`, but Go's multi-threaded runtime creates a fundamental constraint: `setns` affects only the calling OS thread while other goroutines continue executing in the original namespaces. This lesson implements the **re-exec pattern** — the binary re-invokes itself via `/proc/self/exe` so that namespace entry and `syscall.Exec` happen before the Go scheduler proliferates threads — and adds pseudo-terminal (PTY) support for interactive sessions.

```text
containerexec/
  go.mod
  exec.go
  nsenter.go
  pty.go
  exec_test.go
  cmd/demo/main.go
```

## Concepts

### How `setns(2)` Enters a Namespace

`setns(2)` reassociates the calling thread with a namespace identified by an open file descriptor. The file descriptors come from `/proc/<pid>/ns/` entries — one symlink per namespace type. When the `nstype` argument is a `CLONE_NEW*` constant, the kernel validates that the fd references the correct namespace type before making the switch. Passing `nstype = 0` accepts any type.

The namespaces entered for `exec`, and the order that matters:

| File       | Constant         | What it isolates                          |
|------------|------------------|-------------------------------------------|
| `ns/mnt`   | `CLONE_NEWNS`    | Filesystem mount points                   |
| `ns/uts`   | `CLONE_NEWUTS`   | Hostname and domain name                  |
| `ns/ipc`   | `CLONE_NEWIPC`   | System V IPC, POSIX message queues        |
| `ns/net`   | `CLONE_NEWNET`   | Network interfaces, routing, iptables     |
| `ns/pid`   | `CLONE_NEWPID`   | PID numbering (new process is a child)    |

The user namespace is excluded deliberately: the kernel forbids a multithreaded process from calling `setns` into a different user namespace, and a privileged runtime running as root does not need to enter the container's user namespace.

### The Go Threading Constraint

When a Go binary starts, the runtime initializes a scheduler, sets `GOMAXPROCS` OS threads, and starts at least one background thread for garbage collection. `setns(2)` changes only the thread that calls it. If the main goroutine calls `unix.Setns(fd, unix.CLONE_NEWNET)` and a second goroutine runs on a different OS thread, that goroutine still sees the original network namespace.

The consequence: calling `Setns` in a goroutine and then spawning a process via `exec.Command` does not guarantee the child inherits the switched namespaces — `exec.Command.Start` may schedule the fork on a thread that was never switched.

### The Re-exec Pattern

The standard pure-Go solution is to have the binary re-invoke itself with a magic environment variable that triggers namespace entry in an `init()` function, then call `syscall.Exec` to replace the entire process image. After `syscall.Exec` succeeds, no Go runtime threads remain — the kernel loads a fresh binary.

Steps:

1. Parent opens namespace FDs from `/proc/<containerPID>/ns/{mnt,uts,ipc,net,pid}`.
2. Parent forks a child via `exec.Command("/proc/self/exe")`, passing FDs through `cmd.ExtraFiles` (they arrive as FDs 3–7 in the child) and the FD numbers in env vars.
3. Child's `init()` detects `_RUNTIME_NSENTER=1`, calls `runtime.LockOSThread()` to pin the main goroutine to its OS thread, calls `unix.Setns` for each namespace FD in order, then calls `syscall.Exec` to replace itself with the target command.

`runtime.LockOSThread()` in `init()` is sufficient for non-user namespaces because `syscall.Exec` replaces the entire process — all threads are gone after it returns. The runc project uses a cgo `__attribute__((constructor))` to run namespace entry code before the Go runtime starts at all; this is the production-grade approach for user namespaces and for runtimes that do not re-exec.

### PTY Allocation for Interactive Sessions

A pseudo-terminal pair consists of a master device (`/dev/ptmx`) and a dynamically allocated slave (`/dev/pts/<n>`). The master is held by the runtime; the slave is the terminal the container process sees as its stdin/stdout/stderr.

Allocation sequence on Linux:

1. `os.OpenFile("/dev/ptmx", os.O_RDWR, 0)` — obtain the master fd.
2. `unix.IoctlRetInt(masterFd, unix.TIOCGPTN)` — get the PTY index `n`.
3. `unix.IoctlSetInt(masterFd, unix.TIOCSPTLCK, 0)` — unlock the slave.
4. Open `/dev/pts/<n>` as the slave.
5. `unix.IoctlSetWinsize(masterFd, unix.TIOCSWINSZ, ws)` — set initial terminal size.
6. Pass slave as stdin/stdout/stderr for the child with `Setsid: true, Setctty: true, Ctty: 0`.

In the parent, a `SIGWINCH` signal handler calls `IoctlGetWinsize` on the caller's terminal and `IoctlSetWinsize` on the master when the user resizes their terminal window.

### Exit Code Propagation

`exec.Command.Run` returns a `*exec.ExitError` when the child exits with a non-zero status. The exit code must be extracted and returned to the caller so that shell scripts using `exit $?` after `runtime exec` see the correct value. The pattern is `errors.As(err, &exitErr)` followed by `exitErr.ExitCode()`.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/38-capstone-container-runtime/08-exec-into-running-container/08-exec-into-running-container/cmd/demo
cd go-solutions/38-capstone-container-runtime/08-exec-into-running-container/08-exec-into-running-container
go get golang.org/x/sys/unix
```

The state directory format is `/var/run/myruntime/<container-id>/state.json`, matching the lifecycle manager from lesson 7.

### Exercise 1: Container State, Sentinel Errors, and the Exec Entry Point

Create `exec.go`. This file owns the public API, the state-loading logic, and the non-interactive exec path:

```go
//go:build linux

package containerexec

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

// Sentinel errors returned by Exec.
var (
	ErrContainerNotFound   = errors.New("container not found")
	ErrContainerNotRunning = errors.New("container is not running")
	ErrEmptyCommand        = errors.New("command must not be empty")
)

// ContainerState mirrors the JSON written by the lifecycle manager (lesson 7).
type ContainerState struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	PID    int    `json:"pid"`
	RootFS string `json:"rootfs"`
}

// ExecConfig holds the parameters for exec'ing a new process into a running container.
type ExecConfig struct {
	ContainerID string
	Command     []string
	Env         []string
	Tty         bool
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
	StateDir    string // default: "/var/run/myruntime"
}

// Exec runs Command inside the container identified by cfg.ContainerID.
// It returns the exit code of the executed command on success. A non-zero exit
// code is not itself an error — callers must inspect both return values.
func Exec(cfg ExecConfig) (int, error) {
	if len(cfg.Command) == 0 {
		return 0, ErrEmptyCommand
	}
	cs, err := loadContainerState(cfg.StateDir, cfg.ContainerID)
	if err != nil {
		return 0, err
	}
	if cs.Status != "running" {
		return 0, fmt.Errorf("%w: %s (status=%s)", ErrContainerNotRunning, cfg.ContainerID, cs.Status)
	}
	if cfg.Tty {
		return execWithTTY(cfg, cs.PID)
	}
	return execNoTTY(cfg, cs.PID)
}

// loadContainerState reads and parses the state.json for the given container.
func loadContainerState(stateDir, containerID string) (*ContainerState, error) {
	if stateDir == "" {
		stateDir = "/var/run/myruntime"
	}
	path := filepath.Join(stateDir, containerID, "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrContainerNotFound, containerID)
		}
		return nil, fmt.Errorf("containerexec: read state: %w", err)
	}
	var cs ContainerState
	if err := json.Unmarshal(data, &cs); err != nil {
		return nil, fmt.Errorf("containerexec: parse state: %w", err)
	}
	return &cs, nil
}

// closeAll closes every file in the slice, discarding errors.
func closeAll(files []*os.File) {
	for _, f := range files {
		f.Close()
	}
}

// execNoTTY forks a re-exec child that enters the container's namespaces via
// the init() hook in nsenter.go and then replaces itself with cfg.Command.
func execNoTTY(cfg ExecConfig, containerPID int) (int, error) {
	nsFDs, err := openNsFDs(containerPID)
	if err != nil {
		return 0, err
	}
	defer closeAll(nsFDs)

	// ExtraFiles passes [mnt, uts, ipc, net, pid] as FDs [3, 4, 5, 6, 7] in the child.
	cmdJSON, _ := json.Marshal(cfg.Command)
	envJSON, _ := json.Marshal(cfg.Env)

	child := exec.Command("/proc/self/exe")
	child.ExtraFiles = nsFDs
	child.Env = buildNsenterEnv(cmdJSON, envJSON)
	child.Stdin = cfg.Stdin
	child.Stdout = cfg.Stdout
	child.Stderr = cfg.Stderr

	if err := child.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("containerexec: nsenter child: %w", err)
	}
	return 0, nil
}

// buildNsenterEnv returns the environment slice for the re-exec child.
// FD numbers 3-7 correspond to nsEntriesOrder: mnt, uts, ipc, net, pid.
func buildNsenterEnv(cmdJSON, envJSON []byte) []string {
	return []string{
		"_RUNTIME_NSENTER=1",
		"_RUNTIME_NS_MNT=3",
		"_RUNTIME_NS_UTS=4",
		"_RUNTIME_NS_IPC=5",
		"_RUNTIME_NS_NET=6",
		"_RUNTIME_NS_PID=7",
		fmt.Sprintf("_RUNTIME_CMD=%s", cmdJSON),
		fmt.Sprintf("_RUNTIME_ENV=%s", envJSON),
	}
}
```

The `loadContainerState` uses `errors.Is(err, os.ErrNotExist)` so that tests can create fake state directories without touching `/var/run/`. `closeAll` is a simple helper shared by the PTY and non-PTY paths.

### Exercise 2: Namespace FDs and the Re-exec Init Hook

Create `nsenter.go`. This file has two responsibilities: opening namespace file descriptors (called by the parent) and the `init()` hook that runs in the re-exec child:

```go
//go:build linux

package containerexec

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// nsEntriesOrder defines the namespace files and their CLONE_NEW* constants.
// The order determines ExtraFiles indices: mnt=FD3, uts=FD4, ipc=FD5, net=FD6, pid=FD7.
var nsEntriesOrder = []struct {
	name   string
	nstype int
}{
	{"mnt", unix.CLONE_NEWNS},
	{"uts", unix.CLONE_NEWUTS},
	{"ipc", unix.CLONE_NEWIPC},
	{"net", unix.CLONE_NEWNET},
	{"pid", unix.CLONE_NEWPID},
}

// openNsFDs opens /proc/<containerPID>/ns/{mnt,uts,ipc,net,pid}.
// Files are returned in nsEntriesOrder. The caller must call closeAll when done.
func openNsFDs(containerPID int) ([]*os.File, error) {
	base := fmt.Sprintf("/proc/%d/ns", containerPID)
	var files []*os.File
	for _, e := range nsEntriesOrder {
		f, err := os.Open(filepath.Join(base, e.name))
		if err != nil {
			closeAll(files)
			return nil, fmt.Errorf("open ns/%s for PID %d: %w", e.name, containerPID, err)
		}
		files = append(files, f)
	}
	return files, nil
}

// init is the re-exec hook for namespace entry. When the binary is invoked with
// _RUNTIME_NSENTER=1, this init function pins the main goroutine to its OS thread,
// enters each container namespace, and replaces the process image with the target
// command via syscall.Exec.
//
// Constraint: init() runs after the Go runtime starts. runtime.LockOSThread()
// ensures Setns and the subsequent syscall.Exec happen on the same OS thread,
// which is sufficient for mnt/uts/ipc/net/pid namespaces in a privileged runtime.
// For user namespaces, use the cgo constructor approach (runc/libcontainer/nsenter).
func init() {
	if os.Getenv("_RUNTIME_NSENTER") != "1" {
		return
	}
	runtime.LockOSThread()
	if err := enterNamespacesFromEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "nsenter: %v\n", err)
		os.Exit(127)
	}
	replaceWithCommand()
}

// enterNamespacesFromEnv reads namespace FD numbers from environment variables
// and calls unix.Setns for each one on the current locked OS thread.
func enterNamespacesFromEnv() error {
	nsVars := []struct {
		envKey string
		nstype int
	}{
		{"_RUNTIME_NS_MNT", unix.CLONE_NEWNS},
		{"_RUNTIME_NS_UTS", unix.CLONE_NEWUTS},
		{"_RUNTIME_NS_IPC", unix.CLONE_NEWIPC},
		{"_RUNTIME_NS_NET", unix.CLONE_NEWNET},
		{"_RUNTIME_NS_PID", unix.CLONE_NEWPID},
	}
	for _, v := range nsVars {
		val := os.Getenv(v.envKey)
		if val == "" {
			continue
		}
		fd, err := strconv.Atoi(val)
		if err != nil {
			return fmt.Errorf("parse %s=%q: %w", v.envKey, val, err)
		}
		if err := unix.Setns(fd, v.nstype); err != nil {
			return fmt.Errorf("setns %s (fd=%d): %w", v.envKey, fd, err)
		}
	}
	return nil
}

// replaceWithCommand reads the target command from _RUNTIME_CMD and calls
// syscall.Exec to replace the current process image. Does not return on success.
func replaceWithCommand() {
	var argv []string
	if err := json.Unmarshal([]byte(os.Getenv("_RUNTIME_CMD")), &argv); err != nil || len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "nsenter: missing or invalid _RUNTIME_CMD")
		os.Exit(127)
	}
	var envv []string
	if raw := os.Getenv("_RUNTIME_ENV"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &envv)
	}
	if len(envv) == 0 {
		envv = os.Environ()
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "nsenter: %v\n", err)
		os.Exit(127)
	}
	if err := syscall.Exec(path, argv, envv); err != nil {
		fmt.Fprintf(os.Stderr, "nsenter: exec %s: %v\n", path, err)
		os.Exit(127)
	}
}
```

The `init()` function is a no-op for every normal invocation of the binary because `_RUNTIME_NSENTER` is unset. Only the re-exec child sees `_RUNTIME_NSENTER=1`. After `syscall.Exec`, the Go runtime is replaced entirely — no goroutines survive from the pre-exec process.

### Exercise 3: PTY Allocation, Interactive Exec, Tests, and the CLI Demo

Create `pty.go`. The PTY path uses `golang.org/x/sys/unix` for the ioctl calls that the stdlib does not expose:

```go
//go:build linux

package containerexec

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// openPTY allocates a pseudo-terminal pair and returns the open master file
// and the slave device path (e.g. "/dev/pts/3"). The caller must close the master.
func openPTY() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /dev/ptmx: %w", err)
	}
	// TIOCGPTN returns the PTY slave index n; slave lives at /dev/pts/<n>.
	n, err := unix.IoctlRetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, "", fmt.Errorf("TIOCGPTN: %w", err)
	}
	// TIOCSPTLCK with 0 unlocks the slave so it can be opened.
	if err := unix.IoctlSetInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("TIOCSPTLCK: %w", err)
	}
	return master, fmt.Sprintf("/dev/pts/%d", n), nil
}

// syncTermSize copies the window size from src to dst via TIOCGWINSZ / TIOCSWINSZ.
func syncTermSize(src, dst *os.File) error {
	ws, err := unix.IoctlGetWinsize(int(src.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return fmt.Errorf("get winsize: %w", err)
	}
	if err := unix.IoctlSetWinsize(int(dst.Fd()), unix.TIOCSWINSZ, ws); err != nil {
		return fmt.Errorf("set winsize: %w", err)
	}
	return nil
}

// execWithTTY allocates a PTY, sets the slave as the child's controlling terminal,
// and forwards I/O bidirectionally between the caller's terminal and the PTY master.
func execWithTTY(cfg ExecConfig, containerPID int) (int, error) {
	nsFDs, err := openNsFDs(containerPID)
	if err != nil {
		return 0, err
	}
	defer closeAll(nsFDs)

	master, slavePath, err := openPTY()
	if err != nil {
		return 0, err
	}
	defer master.Close()

	// Mirror the caller's terminal size onto the PTY master before the child opens it.
	if termFile, ok := cfg.Stdout.(*os.File); ok {
		_ = syncTermSize(termFile, master)
	}

	// Open the slave without making it a controlling terminal yet (O_NOCTTY);
	// SysProcAttr.Setctty does that after setsid() in the child.
	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return 0, fmt.Errorf("open slave %s: %w", slavePath, err)
	}
	defer slave.Close()

	cmdJSON, _ := json.Marshal(cfg.Command)
	envJSON, _ := json.Marshal(cfg.Env)

	child := exec.Command("/proc/self/exe")
	child.ExtraFiles = nsFDs // slave is FD 0 (stdin), not in ExtraFiles
	child.Env = buildNsenterEnv(cmdJSON, envJSON)
	// Setsid creates a new session; Setctty + Ctty:0 sets the slave (FD 0 = stdin)
	// as the controlling terminal of the new session.
	child.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}
	child.Stdin = slave
	child.Stdout = slave
	child.Stderr = slave

	if err := child.Start(); err != nil {
		return 1, fmt.Errorf("containerexec: start nsenter child: %w", err)
	}
	slave.Close() // parent releases the slave; the child holds it

	// Forward SIGWINCH: resize the PTY master when the caller's terminal changes.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer func() {
		signal.Stop(winch)
		close(winch)
	}()
	go func() {
		for range winch {
			if termFile, ok := cfg.Stdout.(*os.File); ok {
				_ = syncTermSize(termFile, master)
			}
		}
	}()

	// Bidirectional I/O: caller stdin -> PTY master, PTY master -> caller stdout.
	go func() { io.Copy(master, cfg.Stdin) }()  //nolint:errcheck
	go func() { io.Copy(cfg.Stdout, master) }() //nolint:errcheck

	if err := child.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("containerexec: wait: %w", err)
	}
	return 0, nil
}
```

Create `exec_test.go`. The tests cover the extractable pure-Go logic — state loading, sentinel error propagation, and env construction — without requiring live Linux namespaces:

```go
//go:build linux

package containerexec

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestExecRejectsEmptyCommand(t *testing.T) {
	t.Parallel()

	_, err := Exec(ExecConfig{
		ContainerID: "test123",
		Command:     nil,
		StateDir:    t.TempDir(),
	})
	if !errors.Is(err, ErrEmptyCommand) {
		t.Fatalf("err = %v, want ErrEmptyCommand", err)
	}
}

func TestLoadContainerStateNotFound(t *testing.T) {
	t.Parallel()

	_, err := loadContainerState(t.TempDir(), "nonexistent-id")
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("err = %v, want ErrContainerNotFound", err)
	}
}

func TestLoadContainerStateRunning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const containerID = "abc123"
	containerDir := filepath.Join(dir, containerID)
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := ContainerState{
		ID:     containerID,
		Status: "running",
		PID:    12345,
		RootFS: "/var/lib/myruntime/abc123/rootfs",
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(containerDir, "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := loadContainerState(dir, containerID)
	if err != nil {
		t.Fatalf("loadContainerState: %v", err)
	}
	if *got != want {
		t.Fatalf("got %+v, want %+v", *got, want)
	}
}

func TestExecContainerNotFound(t *testing.T) {
	t.Parallel()

	_, err := Exec(ExecConfig{
		ContainerID: "does-not-exist",
		Command:     []string{"/bin/sh"},
		StateDir:    t.TempDir(),
	})
	if !errors.Is(err, ErrContainerNotFound) {
		t.Fatalf("err = %v, want ErrContainerNotFound", err)
	}
}

func TestExecContainerNotRunning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const containerID = "stopped123"
	containerDir := filepath.Join(dir, containerID)
	if err := os.MkdirAll(containerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := ContainerState{ID: containerID, Status: "stopped", PID: 0}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(containerDir, "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Exec(ExecConfig{
		ContainerID: containerID,
		Command:     []string{"/bin/sh"},
		StateDir:    dir,
	})
	if !errors.Is(err, ErrContainerNotRunning) {
		t.Fatalf("err = %v, want ErrContainerNotRunning", err)
	}
}

func TestBuildNsenterEnv(t *testing.T) {
	t.Parallel()

	cases := []struct {
		key  string
		want string
	}{
		{"_RUNTIME_NSENTER", "1"},
		{"_RUNTIME_NS_MNT", "3"},
		{"_RUNTIME_NS_UTS", "4"},
		{"_RUNTIME_NS_IPC", "5"},
		{"_RUNTIME_NS_NET", "6"},
		{"_RUNTIME_NS_PID", "7"},
	}
	env := buildNsenterEnv([]byte(`["/bin/sh"]`), []byte(`["PATH=/usr/bin"]`))

	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()
			want := tc.key + "=" + tc.want
			for _, e := range env {
				if e == want {
					return
				}
			}
			t.Errorf("env missing %q; env = %v", want, env)
		})
	}
}

func TestContainerStateJSONRoundtrip(t *testing.T) {
	t.Parallel()

	want := ContainerState{
		ID:     "abc123",
		Status: "running",
		PID:    9999,
		RootFS: "/var/lib/myruntime/abc123/rootfs",
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ContainerState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("roundtrip mismatch: got %+v, want %+v", got, want)
	}
}

func ExampleContainerState() {
	cs := ContainerState{
		ID:     "c1d2e3f4",
		Status: "running",
		PID:    42,
		RootFS: "/var/lib/myruntime/c1d2e3f4/rootfs",
	}
	data, _ := json.Marshal(cs)
	fmt.Println(string(data))
	// Output:
	// {"id":"c1d2e3f4","status":"running","pid":42,"rootfs":"/var/lib/myruntime/c1d2e3f4/rootfs"}
}
```

Create `cmd/demo/main.go`. The demo exercises the exported API; it only touches exported identifiers:

```go
//go:build linux

package main

import (
	"fmt"
	"os"

	"example.com/containerexec"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: demo [-it] <container-id> <command> [args...]")
		os.Exit(1)
	}

	tty := false
	args := os.Args[1:]
	if args[0] == "-it" {
		tty = true
		args = args[1:]
	}
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: demo [-it] <container-id> <command> [args...]")
		os.Exit(1)
	}

	containerID := args[0]
	command := args[1:]

	code, err := containerexec.Exec(containerexec.ExecConfig{
		ContainerID: containerID,
		Command:     command,
		Env: []string{
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"TERM=xterm-256color",
		},
		Tty:    tty,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "exec: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}
```

Your turn: add `TestLoadContainerStateCorruptJSON` that writes a non-JSON `state.json`, calls `loadContainerState`, and asserts the returned error wraps a parse failure (not `ErrContainerNotFound`).

## Common Mistakes

### Calling `Setns` Without Locking the OS Thread

Wrong: calling `unix.Setns(fd, unix.CLONE_NEWNET)` from a plain goroutine, then spawning a process with `exec.Command(...).Run()`. The fork may happen on a different OS thread that was never switched.

What happens: the child process sees the original namespace of the unmodified thread, not the container's namespace. The `ip addr` output inside the "container" shows the host's network interfaces.

Fix: use the re-exec pattern — fork the binary itself with the namespace FDs in `ExtraFiles`, call `runtime.LockOSThread()` in `init()`, call `Setns`, then `syscall.Exec`. After `syscall.Exec`, the entire process is replaced and the namespace context is correct.

### Entering Namespaces After the Go Runtime Has Started Multiple Threads (User Namespace)

Wrong: attempting to enter a user namespace from `init()` or a goroutine. The kernel returns `EINVAL` because the process is already multithreaded.

What happens: `unix.Setns(fd, unix.CLONE_NEWUSER)` returns `invalid argument` even though the file descriptor is valid.

Fix: use the cgo constructor approach. In runc, `libcontainer/nsenter/nsenter.go` contains a `//export nsexec` function and a `__attribute__((constructor))` C stub that runs before the Go runtime starts. This lesson skips user namespaces; privileged runtimes running as root do not need to enter the container's user namespace.

### Not Propagating the Exit Code

Wrong: ignoring the error from `child.Run()` or always returning `os.Exit(0)`.

```go
// Wrong
child.Run()
return 0, nil
```

What happens: `runtime exec /bin/false` exits 0, breaking shell scripts that check exit codes.

Fix: check for `*exec.ExitError` and return `exitErr.ExitCode()`. A non-zero exit from the exec'd command is not a runtime error — it is the correct result to propagate:

```go
// Fix
if err := child.Run(); err != nil {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 1, fmt.Errorf("nsenter child: %w", err)
}
return 0, nil
```

## Verification

This package requires Linux namespaces and `golang.org/x/sys/unix`; it cannot run or build offline. The extractable pure-Go logic (state loading, sentinel errors, env construction) was validated with `gofmt` and compiles with standard library packages only.

On a Linux host with the module available:

```bash
cd ~/go-exercises/containerexec
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

End-to-end verification requires a running container from lesson 7. Start one, note its container ID, then:

```bash
# Non-interactive: run a command and capture its exit code
sudo go run ./cmd/demo <container-id> /bin/hostname
echo "exit code: $?"

# Interactive: open a shell with a TTY
sudo go run ./cmd/demo -it <container-id> /bin/sh

# Inside the shell, verify namespace membership:
hostname          # should match the container's hostname
ip addr           # should show only the container's veth interface
cat /proc/self/cgroup  # should show the container's cgroup
```

The exec'd process must appear in the container's PID namespace: inside the container's init shell, `ps aux` should list the exec'd command with a low PID, not the host's PID.

## Summary

- `setns(2)` enters a running namespace by opening a `/proc/<pid>/ns/<type>` file descriptor; the kernel validates the namespace type against the `CLONE_NEW*` constant.
- `setns` affects only the calling OS thread; other goroutines continue in the original namespaces.
- The re-exec pattern solves the threading constraint: fork `/proc/self/exe`, call `runtime.LockOSThread()` and `unix.Setns` in `init()`, then call `syscall.Exec` to replace the process image entirely.
- User namespaces require the cgo constructor approach (runc's `libcontainer/nsenter`) because a multithreaded process cannot call `setns` into a user namespace.
- PTY allocation opens `/dev/ptmx`, reads the slave index via `TIOCGPTN`, unlocks it with `TIOCSPTLCK`, and sets the slave as the child's controlling terminal via `SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}`.
- `SIGWINCH` forwarding keeps the PTY slave terminal size synchronized with the caller's terminal.
- Exit code propagation requires extracting `exitErr.ExitCode()` from `*exec.ExitError`; a non-zero exit code is a result to propagate, not a runtime error.

## What's Next

Next: [Container Networking: Bridge and NAT](../09-container-networking-bridge-nat/09-container-networking-bridge-nat.md).

## Resources

- [setns(2) man page](https://man7.org/linux/man-pages/man2/setns.2.html) — system call semantics, `nstype` values, threading constraints
- [runc libcontainer/nsenter](https://github.com/opencontainers/runc/tree/main/libcontainer/nsenter) — reference cgo constructor implementation for user namespace entry
- [golang.org/x/sys/unix](https://pkg.go.dev/golang.org/x/sys/unix) — `Setns`, `IoctlRetInt`, `IoctlSetInt`, `IoctlGetWinsize`, `IoctlSetWinsize`, and `CLONE_NEW*` constants
- [pty(7) man page](https://man7.org/linux/man-pages/man7/pty.7.html) — pseudo-terminal architecture, master/slave pair lifecycle
- [syscall.SysProcAttr](https://pkg.go.dev/syscall#SysProcAttr) — `Setsid`, `Setctty`, `Ctty` for controlling terminal setup in child processes
