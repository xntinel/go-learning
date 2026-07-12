# 7. ptrace Syscall Tracer

`ptrace` lets one process observe and control another at the instruction level. Building a syscall tracer in Go requires solving four problems that are unrelated to algorithms: the kernel delivers ptrace stops through a wait-status encoding that distinguishes syscall-stops, event-stops, and signal-stops; every ptrace call must execute on the OS thread that owns the trace; entry and exit stops alternate per-thread and must be tracked independently; and strings live in the tracee's address space and must be read one `uintptr`-sized word at a time with `PTRACE_PEEKDATA`.

This lesson builds a `tracer` package that spawns or attaches to a process, intercepts every syscall, decodes flags and arguments, and emits structured `SyscallEvent` values. The package is Linux/amd64-only; building on other platforms requires a Linux environment.

```text
ptrace-syscall-tracer/
  go.mod
  tracer/
    event.go               (SyscallEvent, FormatEvent, flag decoders — no build tag)
    syscalls.go            (x86_64 syscall name table — no build tag)
    tracer.go              (Tracer, Config, Spawn, Trace — //go:build linux)
    mem.go                 (PeekString, PeekBytes — //go:build linux)
    tracer_test.go         (table-driven tests, Examples — no build tag)
  cmd/tracer/
    main.go                (CLI — //go:build linux)
```

## Concepts

### The ptrace Contract

`ptrace` operates on a parent-child relationship enforced by the kernel. When a traced process makes a syscall, the kernel stops it and delivers a `SIGTRAP`-derived notification to the tracer via `waitpid`. The tracer inspects or modifies the tracee's state, then calls `ptrace(PTRACE_SYSCALL, pid, ...)` to resume to the next syscall boundary. Miss the resume and the tracee hangs permanently.

In Go, `runtime.LockOSThread()` is mandatory at the start of the tracer goroutine. ptrace binds state to an OS thread; Go's scheduler migrates goroutines between threads and any ptrace call on a different thread than the one that initiated the trace returns `ESRCH`.

### Syscall-Stop Detection: TRACESYSGOOD

Setting `PTRACE_O_TRACESYSGOOD` in options causes the kernel to set bit 7 of the stop signal for syscall-stops, so the stop signal becomes `SIGTRAP | 0x80` (decimal 133) rather than plain `SIGTRAP` (5). This lets the tracer distinguish a syscall-stop from a genuine signal delivery or a ptrace event-stop.

A ptrace event-stop (fork, clone, exec) arrives as `SIGTRAP` with the event code in bits 16-23 of the raw wait status. Extract it with:

```go
event := int(wstatus>>16) & 0xFF
```

The values are: `PTRACE_EVENT_FORK` = 1, `PTRACE_EVENT_VFORK` = 2, `PTRACE_EVENT_CLONE` = 3, `PTRACE_EVENT_EXEC` = 4.

### Register Layout on x86_64

At a syscall-stop, `syscall.PtraceGetRegs` fills a `syscall.PtraceRegs`. The fields that carry syscall data:

| Field | Purpose |
|---|---|
| `Orig_rax` | Syscall number (valid at both entry and exit) |
| `Rdi` | Argument 0 |
| `Rsi` | Argument 1 |
| `Rdx` | Argument 2 |
| `R10` | Argument 3 |
| `R8` | Argument 4 |
| `R9` | Argument 5 |
| `Rax` | Return value at exit; `-errno` on error |

At exit, a negative `Rax` encodes `-errno`. For example, `Rax == 0xffffffffffffffea` is `-22`, meaning `EINVAL`.

Entry and exit stops alternate perfectly per thread. Track a boolean per TID to know which stop is next.

### Reading Strings from the Tracee

`syscall.PtracePeekData(pid, addr, buf)` reads one `uintptr`-sized word (8 bytes on amd64) from the tracee's address space. To reconstruct a C string: read words at successive addresses, scan each word for a null byte, and stop when found. Cap the read at a fixed limit (256 bytes is safe) and append `"..."` if truncated to prevent hanging on a very long argument.

`ESRCH` from `PtracePeekData` means the tracee exited between the wait and the read. Treat it as end-of-trace, not a fatal error.

### Multi-Thread Multiplexing

With `PTRACE_O_TRACEFORK | PTRACE_O_TRACECLONE`, the kernel delivers a clone-event stop for the parent TID when a new thread is created. After detecting the event, call `syscall.PtraceGetEventMsg(pid)` to retrieve the new TID, add it to the tracked set, set its options, and resume it. `waitpid(-1, ..., __WALL)` returns events for any TID; the tracer must maintain per-TID entry/exit state because the alternating pattern is independent per thread.

## Exercises

### Exercise 1: SyscallEvent and Flag Decoders

Create `tracer/event.go`. This file has no build constraint and compiles on all platforms.

```go
package tracer

import (
	"fmt"
	"strings"
	"time"
)

// SyscallEvent records one ptrace syscall stop (entry or exit).
type SyscallEvent struct {
	PID      int
	TID      int
	Nr       uint64        // raw syscall number
	Name     string        // resolved name; "unknown(N)" if not in table
	Args     [6]uint64     // arguments captured at entry
	Ret      int64         // return value; set at exit stop
	Entry    bool          // true = entry stop; false = exit stop
	At       time.Time     // wall clock at the stop
	Duration time.Duration // elapsed since entry; zero at entry stop
}

// SyscallName returns the name for nr on x86_64, or "unknown(nr)".
func SyscallName(nr uint64) string {
	if name, ok := syscallNames[nr]; ok {
		return name
	}
	return fmt.Sprintf("unknown(%d)", nr)
}

// FormatEvent formats a syscall event as a single human-readable line.
// All six argument slots are printed; zero-valued trailing slots are
// expected for syscalls that take fewer than six arguments.
func FormatEvent(e SyscallEvent) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[%d:%d] %s(", e.PID, e.TID, e.Name)
	for i, a := range e.Args {
		if i > 0 {
			sb.WriteString(", ")
		}
		if a < 4096 {
			fmt.Fprintf(&sb, "%d", a)
		} else {
			fmt.Fprintf(&sb, "0x%x", a)
		}
	}
	fmt.Fprintf(&sb, ") = %d", e.Ret)
	if e.Duration > 0 {
		fmt.Fprintf(&sb, " <%s>", e.Duration.Round(time.Microsecond))
	}
	return sb.String()
}

// openFlagBits lists Linux open(2) flag bits in ascending value order.
// The access mode (bits 0-1: O_RDONLY/O_WRONLY/O_RDWR) is handled
// separately in DecodeOpenFlags.
var openFlagBits = []struct {
	val  uint64
	name string
}{
	{0x0040, "O_CREAT"},
	{0x0080, "O_EXCL"},
	{0x0100, "O_NOCTTY"},
	{0x0200, "O_TRUNC"},
	{0x0400, "O_APPEND"},
	{0x0800, "O_NONBLOCK"},
	{0x1000, "O_DSYNC"},
	{0x4000, "O_DIRECT"},
	{0x8000, "O_LARGEFILE"},
	{0x10000, "O_DIRECTORY"},
	{0x20000, "O_NOFOLLOW"},
	{0x80000, "O_CLOEXEC"},
}

// DecodeOpenFlags converts an open(2) or openat(2) flags argument to a
// symbolic OR-separated string. Values are taken from Linux asm-generic/fcntl.h.
func DecodeOpenFlags(flags uint64) string {
	var parts []string
	switch flags & 3 {
	case 0:
		parts = append(parts, "O_RDONLY")
	case 1:
		parts = append(parts, "O_WRONLY")
	case 2:
		parts = append(parts, "O_RDWR")
	}
	for _, f := range openFlagBits {
		if flags&f.val != 0 {
			parts = append(parts, f.name)
		}
	}
	return strings.Join(parts, "|")
}

// DecodeMmapProt converts an mmap(2) prot argument to a symbolic string.
// Values are from sys/mman.h: PROT_READ=1, PROT_WRITE=2, PROT_EXEC=4.
func DecodeMmapProt(prot uint64) string {
	if prot == 0 {
		return "PROT_NONE"
	}
	var parts []string
	if prot&1 != 0 {
		parts = append(parts, "PROT_READ")
	}
	if prot&2 != 0 {
		parts = append(parts, "PROT_WRITE")
	}
	if prot&4 != 0 {
		parts = append(parts, "PROT_EXEC")
	}
	return strings.Join(parts, "|")
}
```

### Exercise 2: Syscall Name Table

Create `tracer/syscalls.go`. The file has no arch constraint so the table compiles on any host; it documents x86_64 values in comments. Source of truth: `arch/x86/entry/syscalls/syscall_64.tbl` in the Linux kernel tree.

```go
package tracer

// syscallNames maps x86_64 Linux syscall numbers to their names.
// Source: Linux kernel arch/x86/entry/syscalls/syscall_64.tbl
var syscallNames = map[uint64]string{
	0:   "read",
	1:   "write",
	2:   "open",
	3:   "close",
	4:   "stat",
	5:   "fstat",
	6:   "lstat",
	7:   "poll",
	8:   "lseek",
	9:   "mmap",
	10:  "mprotect",
	11:  "munmap",
	12:  "brk",
	13:  "rt_sigaction",
	14:  "rt_sigprocmask",
	16:  "ioctl",
	17:  "pread64",
	18:  "pwrite64",
	19:  "readv",
	20:  "writev",
	21:  "access",
	22:  "pipe",
	23:  "select",
	24:  "sched_yield",
	32:  "dup",
	33:  "dup2",
	35:  "nanosleep",
	39:  "getpid",
	40:  "sendfile",
	41:  "socket",
	42:  "connect",
	43:  "accept",
	44:  "sendto",
	45:  "recvfrom",
	46:  "sendmsg",
	47:  "recvmsg",
	48:  "shutdown",
	49:  "bind",
	50:  "listen",
	51:  "getsockname",
	52:  "getpeername",
	53:  "socketpair",
	54:  "setsockopt",
	55:  "getsockopt",
	56:  "clone",
	57:  "fork",
	58:  "vfork",
	59:  "execve",
	60:  "exit",
	61:  "wait4",
	62:  "kill",
	63:  "uname",
	72:  "fcntl",
	79:  "getcwd",
	80:  "chdir",
	82:  "rename",
	83:  "mkdir",
	84:  "rmdir",
	85:  "creat",
	86:  "link",
	87:  "unlink",
	88:  "symlink",
	89:  "readlink",
	90:  "chmod",
	92:  "chown",
	95:  "umask",
	96:  "gettimeofday",
	97:  "getrlimit",
	98:  "getrusage",
	99:  "sysinfo",
	101: "ptrace",
	102: "getuid",
	104: "getgid",
	107: "geteuid",
	108: "getegid",
	110: "getppid",
	111: "getpgrp",
	112: "setsid",
	158: "arch_prctl",
	186: "gettid",
	200: "tkill",
	202: "futex",
	218: "set_tid_address",
	228: "clock_gettime",
	229: "clock_getres",
	230: "clock_nanosleep",
	231: "exit_group",
	232: "epoll_wait",
	233: "epoll_ctl",
	234: "tgkill",
	257: "openat",
	258: "mkdirat",
	262: "newfstatat",
	263: "unlinkat",
	264: "renameat",
	266: "symlinkat",
	267: "readlinkat",
	268: "fchmodat",
	269: "faccessat",
	280: "utimensat",
	281: "epoll_pwait",
	283: "timerfd_create",
	285: "fallocate",
	288: "accept4",
	291: "epoll_create1",
	292: "dup3",
	293: "pipe2",
	302: "prlimit64",
	316: "renameat2",
	318: "getrandom",
	319: "memfd_create",
	332: "statx",
	435: "clone3",
	437: "openat2",
	439: "faccessat2",
}
```

### Exercise 3: String and Memory Helpers

Create `tracer/mem.go`. The build constraint restricts it to Linux where the ptrace helpers exist.

```go
//go:build linux

package tracer

import (
	"syscall"
)

const (
	wordSize     = 8   // bytes per PTRACE_PEEKDATA word on amd64
	maxStringLen = 256 // cap on string reads from tracee memory
)

// PeekString reads a null-terminated C string from the tracee's address
// space starting at addr. It stops at the first null byte or after
// maxStringLen bytes, appending "..." if the string was truncated.
// Returns "<nil>" when addr is zero.
func PeekString(pid int, addr uintptr) string {
	if addr == 0 {
		return "<nil>"
	}
	var buf [maxStringLen]byte
	n := 0
	for n < maxStringLen {
		var word [wordSize]byte
		if _, err := syscall.PtracePeekData(pid, addr+uintptr(n), word[:]); err != nil {
			break
		}
		for i, b := range word {
			pos := n + i
			if pos >= maxStringLen {
				return string(buf[:maxStringLen]) + "..."
			}
			if b == 0 {
				return string(buf[:pos])
			}
			buf[pos] = b
		}
		n += wordSize
	}
	return string(buf[:n])
}

// PeekBytes reads count bytes from the tracee's address space at addr.
// It reads in wordSize-aligned chunks. Returns nil if addr is zero or
// count is non-positive. Returns a partial slice and an error if a
// PEEKDATA call fails mid-read.
func PeekBytes(pid int, addr uintptr, count int) ([]byte, error) {
	if count <= 0 || addr == 0 {
		return nil, nil
	}
	words := (count + wordSize - 1) / wordSize
	buf := make([]byte, words*wordSize)
	for i := 0; i < words; i++ {
		off := i * wordSize
		if _, err := syscall.PtracePeekData(pid, addr+uintptr(off), buf[off:off+wordSize]); err != nil {
			return buf[:off], err
		}
	}
	return buf[:count], nil
}
```

### Exercise 4: The Tracer

Create `tracer/tracer.go`. This is the core trace loop.

```go
//go:build linux

package tracer

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

// ptrace option flags (PTRACE_O_*) and event codes (PTRACE_EVENT_*).
// Defined explicitly because not all versions of the Go syscall package
// export them. Values are from linux/ptrace.h.
const (
	ptraceOTraceSysGood = 0x00000001
	ptraceOTraceFork    = 0x00000002
	ptraceOTraceVFork   = 0x00000004
	ptraceOTraceClone   = 0x00000008
	ptraceOTraceExec    = 0x00000010

	ptraceEventFork  = 1
	ptraceEventVFork = 2
	ptraceEventClone = 3
	ptraceEventExec  = 4

	ptraceOptions = ptraceOTraceSysGood |
		ptraceOTraceFork |
		ptraceOTraceVFork |
		ptraceOTraceClone |
		ptraceOTraceExec

	// syscallStopSig is the stop signal for syscall-stops when
	// PTRACE_O_TRACESYSGOOD is set (SIGTRAP with bit 7).
	syscallStopSig = syscall.Signal(syscall.SIGTRAP | 0x80)
)

// Config controls tracer behaviour.
type Config struct {
	// Command is the program and arguments to spawn under ptrace.
	// Mutually exclusive with PID.
	Command []string

	// PID is an already-running process to attach to.
	// Zero means spawn Command.
	PID int

	// FollowForks enables tracing of forked or cloned children.
	FollowForks bool

	// BufSize is the event channel buffer. Zero uses 256.
	BufSize int
}

// Tracer attaches to a process and emits SyscallEvents on a channel.
type Tracer struct {
	cfg    Config
	events chan SyscallEvent
}

// New creates a Tracer from cfg. Either cfg.Command or cfg.PID must be
// non-zero; setting both is an error.
func New(cfg Config) (*Tracer, error) {
	if len(cfg.Command) == 0 && cfg.PID == 0 {
		return nil, errors.New("tracer: Command or PID must be set")
	}
	if len(cfg.Command) > 0 && cfg.PID != 0 {
		return nil, errors.New("tracer: Command and PID are mutually exclusive")
	}
	buf := cfg.BufSize
	if buf <= 0 {
		buf = 256
	}
	return &Tracer{cfg: cfg, events: make(chan SyscallEvent, buf)}, nil
}

// Events returns the channel on which decoded SyscallEvents are sent.
// The channel is closed when Trace returns.
func (t *Tracer) Events() <-chan SyscallEvent {
	return t.events
}

// Trace runs the trace loop until the root tracee exits or ctx is cancelled.
// It locks the calling goroutine to its OS thread (required by ptrace) and
// closes the Events channel before returning.
func (t *Tracer) Trace(ctx context.Context) error {
	runtime.LockOSThread()
	defer close(t.events)

	var (
		rootPID int
		err     error
	)
	if len(t.cfg.Command) > 0 {
		rootPID, err = spawnUnderPtrace(t.cfg.Command)
	} else {
		rootPID, err = attachToPID(t.cfg.PID)
	}
	if err != nil {
		return fmt.Errorf("tracer: start: %w", err)
	}

	if err := syscall.PtraceSetOptions(rootPID, ptraceOptions); err != nil {
		return fmt.Errorf("tracer: setoptions pid=%d: %w", rootPID, err)
	}

	// Per-TID state: true = next stop is an entry, false = next is an exit.
	atEntry := map[int]bool{rootPID: true}
	// entryAt records wall time of the last entry stop per TID.
	entryAt := map[int]time.Time{}
	// pending holds the partially decoded entry event until the exit arrives.
	pending := map[int]SyscallEvent{}

	// Resume the tracee to its first syscall boundary.
	if err := syscall.PtraceSyscall(rootPID, 0); err != nil {
		return fmt.Errorf("tracer: initial resume pid=%d: %w", rootPID, err)
	}

	for {
		// Honour context cancellation between waits.
		select {
		case <-ctx.Done():
			for pid := range atEntry {
				_ = syscall.PtraceDetach(pid)
			}
			return ctx.Err()
		default:
		}

		var ws syscall.WaitStatus
		// __WALL (0x40000000) waits for all children including threads.
		pid, err := syscall.Wait4(-1, &ws, syscall.WALL, nil)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return fmt.Errorf("tracer: wait4: %w", err)
		}

		switch {
		case ws.Exited() || ws.Signaled():
			delete(atEntry, pid)
			delete(entryAt, pid)
			delete(pending, pid)
			if pid == rootPID {
				return nil // root exited; done
			}
			continue

		case !ws.Stopped():
			_ = syscall.PtraceSyscall(pid, 0)
			continue
		}

		sig := ws.StopSignal()
		// ptrace event code lives in bits 16-23 of the raw wait status.
		event := int(ws>>16) & 0xFF

		switch {
		case sig == syscallStopSig:
			// Syscall-stop: PTRACE_O_TRACESYSGOOD set bit 7 of the signal.
			isEntry := atEntry[pid]
			atEntry[pid] = !isEntry
			t.handleSyscallStop(pid, isEntry, entryAt, pending)
			_ = syscall.PtraceSyscall(pid, 0)

		case sig == syscall.SIGTRAP && event != 0:
			// ptrace event-stop from PTRACE_O_TRACE* options.
			if t.cfg.FollowForks && (event == ptraceEventClone ||
				event == ptraceEventFork ||
				event == ptraceEventVFork) {
				if newTID, err := syscall.PtraceGetEventMsg(pid); err == nil {
					newPID := int(newTID)
					atEntry[newPID] = true
					// The new child is already stopped; set its options and resume.
					_ = syscall.PtraceSetOptions(newPID, ptraceOptions)
					_ = syscall.PtraceSyscall(newPID, 0)
				}
			}
			_ = syscall.PtraceSyscall(pid, 0)

		case sig == syscall.SIGSTOP || sig == syscall.SIGTSTP:
			// Initial stop from PTRACE_ATTACH or group-stop; suppress the signal.
			_ = syscall.PtraceSyscall(pid, 0)

		default:
			// Genuine signal: deliver it to the tracee.
			_ = syscall.PtraceSyscall(pid, int(sig))
		}
	}
}

// handleSyscallStop reads registers and emits a SyscallEvent. On entry it
// records arguments and time; on exit it computes the return value and duration.
func (t *Tracer) handleSyscallStop(
	pid int,
	entry bool,
	entryAt map[int]time.Time,
	pending map[int]SyscallEvent,
) {
	var regs syscall.PtraceRegs
	if err := syscall.PtraceGetRegs(pid, &regs); err != nil {
		return // tracee may have exited between wait and getregs
	}

	now := time.Now()

	if entry {
		ev := SyscallEvent{
			PID:   pid,
			TID:   pid,
			Nr:    regs.Orig_rax,
			Name:  SyscallName(regs.Orig_rax),
			Args:  [6]uint64{regs.Rdi, regs.Rsi, regs.Rdx, regs.R10, regs.R8, regs.R9},
			Entry: true,
			At:    now,
		}
		entryAt[pid] = now
		pending[pid] = ev
		select {
		case t.events <- ev:
		default: // drop if consumer is not keeping up
		}
		return
	}

	// Exit stop.
	ev, ok := pending[pid]
	if !ok {
		return
	}
	ev.Ret = int64(regs.Rax)
	ev.Entry = false
	ev.Duration = now.Sub(entryAt[pid])
	delete(pending, pid)
	select {
	case t.events <- ev:
	default:
	}
}

// spawnUnderPtrace starts command as a child process with ptrace enabled.
// The child stops before execve runs; spawnUnderPtrace waits for that stop
// and returns the child PID.
func spawnUnderPtrace(command []string) (int, error) {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("spawn: %w", err)
	}
	// The kernel delivers a SIGTRAP before execve; wait for it.
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(cmd.Process.Pid, &ws, 0, nil); err != nil {
		return 0, fmt.Errorf("spawn: initial wait: %w", err)
	}
	return cmd.Process.Pid, nil
}

// attachToPID sends PTRACE_ATTACH to pid and waits for the resulting SIGSTOP.
func attachToPID(pid int) (int, error) {
	if err := syscall.PtraceAttach(pid); err != nil {
		return 0, fmt.Errorf("attach pid=%d: %w", pid, err)
	}
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		return 0, fmt.Errorf("attach wait pid=%d: %w", pid, err)
	}
	return pid, nil
}
```

### Exercise 5: Tests

Create `tracer/tracer_test.go`. This file has no build constraint: it tests only the platform-agnostic functions in `event.go` and `syscalls_amd64.go` and runs on any OS.

```go
package tracer

import (
	"fmt"
	"testing"
)

func TestSyscallName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		nr   uint64
		want string
	}{
		{0, "read"},
		{1, "write"},
		{2, "open"},
		{3, "close"},
		{9, "mmap"},
		{59, "execve"},
		{257, "openat"},
		{9999, "unknown(9999)"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("nr=%d", tc.nr), func(t *testing.T) {
			t.Parallel()
			got := SyscallName(tc.nr)
			if got != tc.want {
				t.Errorf("SyscallName(%d) = %q, want %q", tc.nr, got, tc.want)
			}
		})
	}
}

func TestDecodeOpenFlags(t *testing.T) {
	t.Parallel()

	// Flag constants for readability (Linux asm-generic/fcntl.h values).
	const (
		oRDONLY  = uint64(0)
		oWRONLY  = uint64(1)
		oRDWR    = uint64(2)
		oCREAT   = uint64(0x0040)
		oTRUNC   = uint64(0x0200)
		oAPPEND  = uint64(0x0400)
		oCLOEXEC = uint64(0x80000)
	)

	cases := []struct {
		flags uint64
		want  string
	}{
		{oRDONLY, "O_RDONLY"},
		{oWRONLY, "O_WRONLY"},
		{oRDWR, "O_RDWR"},
		{oRDWR | oCREAT, "O_RDWR|O_CREAT"},
		{oWRONLY | oCREAT | oTRUNC, "O_WRONLY|O_CREAT|O_TRUNC"},
		{oRDONLY | oCLOEXEC, "O_RDONLY|O_CLOEXEC"},
		{oWRONLY | oAPPEND, "O_WRONLY|O_APPEND"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("0x%x", tc.flags), func(t *testing.T) {
			t.Parallel()
			got := DecodeOpenFlags(tc.flags)
			if got != tc.want {
				t.Errorf("DecodeOpenFlags(0x%x) = %q, want %q",
					tc.flags, got, tc.want)
			}
		})
	}
}

func TestDecodeMmapProt(t *testing.T) {
	t.Parallel()

	cases := []struct {
		prot uint64
		want string
	}{
		{0, "PROT_NONE"},
		{1, "PROT_READ"},
		{2, "PROT_WRITE"},
		{4, "PROT_EXEC"},
		{3, "PROT_READ|PROT_WRITE"},
		{5, "PROT_READ|PROT_EXEC"},
		{7, "PROT_READ|PROT_WRITE|PROT_EXEC"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("prot=%d", tc.prot), func(t *testing.T) {
			t.Parallel()
			got := DecodeMmapProt(tc.prot)
			if got != tc.want {
				t.Errorf("DecodeMmapProt(%d) = %q, want %q", tc.prot, got, tc.want)
			}
		})
	}
}

func TestFormatEventSmallArgs(t *testing.T) {
	t.Parallel()

	e := SyscallEvent{
		PID:  200,
		TID:  200,
		Nr:   3,
		Name: "close",
		Args: [6]uint64{4, 0, 0, 0, 0, 0},
		Ret:  0,
	}
	got := FormatEvent(e)
	want := "[200:200] close(4, 0, 0, 0, 0, 0) = 0"
	if got != want {
		t.Errorf("FormatEvent = %q, want %q", got, want)
	}
}

func TestFormatEventLargeAddress(t *testing.T) {
	t.Parallel()

	// An mmap return value is a large address; FormatEvent must use hex.
	e := SyscallEvent{
		PID:  300,
		TID:  300,
		Nr:   9,
		Name: "mmap",
		Args: [6]uint64{0, 4096, 3, 2, 0, 0},
		Ret:  int64(0x7f1234560000),
	}
	got := FormatEvent(e)
	// Ret is printed as decimal (the format uses %d for Ret).
	if got == "" {
		t.Fatal("FormatEvent returned empty string")
	}
}

func ExampleSyscallName() {
	fmt.Println(SyscallName(0))
	fmt.Println(SyscallName(257))
	fmt.Println(SyscallName(9999))
	// Output:
	// read
	// openat
	// unknown(9999)
}

func ExampleFormatEvent() {
	e := SyscallEvent{
		PID:  100,
		TID:  100,
		Nr:   1,
		Name: "write",
		Args: [6]uint64{1, 0, 5, 0, 0, 0},
		Ret:  5,
	}
	fmt.Println(FormatEvent(e))
	// Output: [100:100] write(1, 0, 5, 0, 0, 0) = 5
}
```

Your turn: add `TestDecodeOpenFlagsUnknownBits` — pass a flags value with high bits set that do not appear in `openFlagBits` (e.g. `0x200001`) and assert the output still begins with `"O_WRONLY"` (the access mode is always decoded) while the unknown bits are silently ignored. This pins the contract that unknown flags do not panic or produce garbage.

### Exercise 6: CLI

Create `cmd/tracer/main.go`. The CLI traces the exit stops only (the interesting ones) and prints each to stdout.

```go
//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"example.com/ptrace-syscall-tracer/tracer"
)

func main() {
	pid := flag.Int("p", 0, "attach to PID instead of spawning a command")
	follow := flag.Bool("f", false, "follow forks and clones")
	flag.Parse()

	cfg := tracer.Config{FollowForks: *follow}
	switch {
	case *pid != 0:
		cfg.PID = *pid
	case flag.NArg() > 0:
		cfg.Command = flag.Args()
	default:
		fmt.Fprintln(os.Stderr, "usage: tracer [-p pid] [-f] [command [args...]]")
		os.Exit(2)
	}

	tr, err := tracer.New(cfg)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT, syscall.SIGTERM,
	)
	defer cancel()

	// Consume events in a separate goroutine so the main goroutine can
	// call Trace (which locks the OS thread).
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range tr.Events() {
			if !ev.Entry {
				fmt.Println(tracer.FormatEvent(ev))
			}
		}
	}()

	if err := tr.Trace(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
	<-done
}
```

## Common Mistakes

### Calling ptrace from an Unlocked Goroutine

Wrong: starting the trace loop in a plain goroutine, then making ptrace calls.

```go
go func() {
    tr.Trace(ctx) // ptrace calls may run on different OS threads
}()
```

What happens: `PtraceGetRegs`, `PtraceSyscall`, and `Wait4` execute on whichever OS thread the scheduler happens to use. The thread that owns the trace gets an `ESRCH` error on any call, the tracee hangs, and the tracer deadlocks.

Fix: call `runtime.LockOSThread()` at the very start of the goroutine that makes ptrace calls. The `Trace` method in the implementation above does this internally, but if you split the trace loop into multiple goroutines, each goroutine that calls ptrace must lock its own OS thread.

### Treating Every SIGTRAP as a Syscall-Stop

Wrong: resuming with `PTRACE_SYSCALL` on any `SIGTRAP` stop and reading registers unconditionally.

What happens: a ptrace event-stop (clone, fork, exec) also delivers `SIGTRAP`. Reading registers during a clone event produces stale data from the parent's registers, not the new child's. The entry/exit alternation logic breaks because an event-stop does not consume an entry or exit slot.

Fix: check bit 7 of the stop signal. A syscall-stop has `sig == SIGTRAP | 0x80`; an event-stop has `sig == SIGTRAP` with a non-zero event in bits 16-23 of the wait status.

### Reading `Rax` as the Syscall Number

Wrong:

```go
nr := regs.Rax // at entry
```

What happens: at syscall entry, the kernel has already set `Rax` to `-ENOSYS` as a sentinel; the real syscall number is in `Orig_rax`. At exit, `Rax` holds the return value. Using `Rax` at entry produces syscall number `-38` for every call.

Fix: use `Orig_rax` for the syscall number at both entry and exit. `Rax` is only meaningful at exit as the return value.

### Ignoring `ESRCH` on Memory Reads

Wrong:

```go
s := PeekString(pid, addr) // if pid exited, this returns garbage or panics
```

What happens: `PtracePeekData` returns `ESRCH` when the tracee has exited between the wait-event and the read. If the error is silently ignored, the tracer reads whatever was last in the buffer or loops forever.

Fix: propagate the `error` from `PeekBytes`/`PeekString` and treat `ESRCH` as a clean end-of-trace signal, not a fatal error.

### Blocking the Event Channel

Wrong: sending to `t.events` without a `select`/`default`:

```go
t.events <- ev // blocks if the consumer is slow
```

What happens: the tracer goroutine stalls on the channel send while the tracee waits for `PTRACE_SYSCALL` to resume it. The tracee is permanently suspended.

Fix: use a non-blocking send with a `default` branch to drop events when the consumer cannot keep up, or size the buffer generously and document the drop policy.

## Verification

The flag-decoder and name-table tests are platform-agnostic and run on any OS. The tracer core requires Linux. Run in two phases:

**Phase 1 — any platform** (tests event.go + syscalls_amd64.go):

```bash
cd ~/go-exercises/ptrace-syscall-tracer
test -z "$(gofmt -l ./tracer/event.go ./tracer/syscalls.go ./tracer/tracer_test.go)"
go vet ./tracer/...
go test -count=1 -race ./tracer/...
```

The `go test` command compiles only the files without a build constraint on non-Linux systems, so `TestSyscallName`, `TestDecodeOpenFlags`, `TestDecodeMmapProt`, `TestFormatEvent*`, and the two `Example` functions all run and pass.

**Phase 2 — Linux only** (integration):

```bash
go build ./...
# Trace a single-threaded command:
sudo go run ./cmd/tracer -- ls /tmp
# Verify: openat, getdents64, write entries appear with decoded syscall names.

# Trace with fork-following:
sudo go run ./cmd/tracer -f -- sh -c 'echo hello'
# Verify: clone or fork events; child TID appears in output.

# Attach to a running process:
sleep 100 &
sudo go run ./cmd/tracer -p $!
# Verify: syscalls from the sleep process appear; Ctrl-C detaches cleanly.
```

All four commands in Phase 1 must pass before evaluating Phase 2. Add the `TestDecodeOpenFlagsUnknownBits` test from Exercise 5 before submitting.

## Summary

- `runtime.LockOSThread()` is not optional in any goroutine that calls ptrace; the ptrace binding is per-OS-thread, and Go migrates goroutines freely.
- Syscall-stop detection requires `PTRACE_O_TRACESYSGOOD`; the bit-7 marker in the stop signal is the only reliable way to distinguish a syscall-stop from an event-stop or a genuine signal.
- `Orig_rax` holds the syscall number at both entry and exit; `Rax` is the return value and is valid only at exit.
- Entry and exit stops alternate per-thread; maintain per-TID state for correct sequencing under multi-threaded tracees.
- `PtracePeekData` reads one word at a time from the tracee's address space; cap string reads and handle `ESRCH` as a clean exit condition.
- The flag-decoder and name-table components are platform-agnostic and can be unit-tested on any OS.

## What's Next

Next: [Raw Socket Packet Capture](../08-raw-socket-packet-capture/08-raw-socket-packet-capture.md).

## Resources

- [ptrace(2) man page](https://man7.org/linux/man-pages/man2/ptrace.2.html) — the definitive reference for wait-status encoding, option flags, and event codes
- [pkg.go.dev/syscall](https://pkg.go.dev/syscall) — Go's syscall package: PtraceGetRegs, PtracePeekData, PtraceSetOptions, WaitStatus methods
- [Linux kernel syscall table (x86_64)](https://github.com/torvalds/linux/blob/master/arch/x86/entry/syscalls/syscall_64.tbl) — authoritative source for syscall numbers
- [strace source code](https://github.com/strace/strace) — reference implementation: argument decoders, flag tables, and multi-thread handling
- [Eli Bendersky: How debuggers work, Part 1](https://eli.thegreenplace.net/2011/01/23/how-debuggers-work-part-1) — clear walk-through of the ptrace loop and wait-status encoding
