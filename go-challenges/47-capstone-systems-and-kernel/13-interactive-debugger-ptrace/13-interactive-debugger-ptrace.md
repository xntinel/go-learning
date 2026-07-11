# 13. Interactive Debugger with ptrace

Building a source-level debugger requires integrating four separate domains into a single coherent tool: Linux process control via `ptrace`, ELF binary parsing with `debug/elf`, DWARF debug information with `debug/dwarf`, and a command-line REPL. The hard part is that these layers couple tightly: a breakpoint requires knowing both the machine address (from DWARF line tables) and the ability to write to the tracee's memory (from ptrace). The breakpoint re-arm cycle, goroutine inspection, and the OS-thread pinning requirement are the most common failure points.

```text
debugger/
  go.mod
  breakpoint.go        -- breakpoint table (cross-platform)
  elf_info.go          -- ELF/DWARF parsing (cross-platform)
  debugger_linux.go    -- ptrace tracer (Linux only, //go:build linux)
  debugger_test.go     -- tests for breakpoint table and BinaryInfo (cross-platform)
  cmd/demo/main.go     -- interactive REPL (Linux only, //go:build linux)
```

This lesson is Linux-only at runtime; the breakpoint table and ELF-parsing code are cross-platform and fully testable on any OS.

## Concepts

### Process Control with ptrace

`ptrace(2)` is a Linux system call that lets one process (the tracer) observe and control another (the tracee). In Go, the `syscall` package exposes ptrace through typed wrappers: `syscall.PtraceAttach`, `syscall.PtraceCont`, `syscall.PtraceSingleStep`, `syscall.PtraceGetRegs`, `syscall.PtraceSetRegs`, `syscall.PtracePeekText`, and `syscall.PtracePokeText`.

The critical constraint: all ptrace calls to a given PID must come from the same OS thread. Go's goroutine scheduler moves goroutines between OS threads freely, so any goroutine issuing ptrace calls must first call `runtime.LockOSThread()`. Forgetting this produces `ptrace: operation not permitted` errors that are silent and hard to diagnose.

To launch a process under ptrace, set `exec.Cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}`. The child then executes `PTRACE_TRACEME` and stops immediately after `exec`; the parent calls `Wait4` to consume that initial `SIGTRAP`.

### ELF Format and Symbol Tables

ELF (Executable and Linkable Format) is the binary format on Linux. Go's `debug/elf` package (`pkg.go.dev/debug/elf`) parses ELF files without running them, so it is cross-platform. The two most useful methods are:

- `f.Symbols() ([]elf.Symbol, error)` reads the `.symtab` section. Stripped binaries return an error; handle it gracefully.
- `f.DWARF() (*dwarf.Data, error)` returns the parsed DWARF block covering all `.debug_*` sections.

`elf.Symbol.Value` is the virtual address of the symbol; `elf.Symbol.Name` is its mangled name (e.g. `main.main`, `runtime.mallocgc`).

### DWARF Debug Information and Line Tables

DWARF is the debug format embedded in most ELF binaries. Go's `debug/dwarf` package (`pkg.go.dev/debug/dwarf`) exposes it through two key primitives:

1. `dwarf.Data.Reader()` iterates Debugging Information Entries (DIEs). Each DIE has a `Tag`; `dwarf.TagCompileUnit` marks the start of a source file's compilation unit.
2. `dwarf.Data.LineReader(cu *Entry) (*LineReader, error)` returns a line reader for one compile unit. `lr.Next(&le)` fills a `dwarf.LineEntry` with fields including `Address` (machine address), `File` (source file), and `Line` (1-based line number).

The line table maps machine addresses to source locations bidirectionally: given an address, find the source line; given a source file:line, find the first machine address.

### Software Breakpoints on x86-64

On x86-64, `INT3` is the single-byte instruction `0xCC`. Writing it at any address causes the CPU to deliver `SIGTRAP` to the process on that instruction. The breakpoint cycle is:

1. Save the byte currently at the target address (the displaced byte).
2. Write `0xCC` to that address with `PTRACE_POKETEXT`.
3. When `SIGTRAP` fires, the instruction pointer (`RIP`) points one byte past the `INT3`.
4. Decrement `RIP` to the breakpoint address.
5. Restore the original byte with `PTRACE_POKETEXT`.
6. Set `RIP` via `PTRACE_SETREGS`.
7. To keep the breakpoint active: single-step the restored instruction with `PTRACE_SINGLESTEP`, then re-write `0xCC`.

Step 7 (re-arm) is the step most implementations omit on first attempt. Skipping it means the breakpoint fires only once.

`PTRACE_POKETEXT` operates on machine-word units (8 bytes on amd64). To modify a single byte, read the surrounding 8-byte word first, patch the target byte, and write the word back.

### Goroutine Inspection

Go goroutines are not OS threads; the runtime scheduler maps them dynamically onto OS threads. To list goroutines from a debugger, parse the runtime's internal data structures:

1. Find the symbol `runtime.allgs` in the symbol table; this is a `[]g` slice header.
2. Read the slice header (ptr, len, cap) from the tracee's memory.
3. Read each `g` (goroutine) struct. Relevant fields at Go 1.26: `goid int64` and `atomicstatus atomic.Uint32`.
4. The `atomicstatus` values: 1=runnable, 2=running, 3=syscall, 4=waiting, 6=dead.

Field offsets are version-specific; a robust implementation reads them from the DWARF `.debug_info` entries for the `runtime.g` type rather than hard-coding offsets.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/debugger/cmd/demo
cd ~/go-exercises/debugger
go mod init example.com/debugger
```

This is a library used by the `cmd/demo` REPL, not a standalone program. Verify with `go test ./...` (the cross-platform tests) and `go build ./...` (on Linux, builds the full package including `cmd/demo`).

### Exercise 1: Breakpoint Table

Create `breakpoint.go`. The breakpoint table is pure Go with no OS dependency.

```go
package debugger

import (
	"errors"
	"fmt"
	"sort"
)

// Sentinel errors for breakpoint operations.
var (
	ErrBreakpointExists   = errors.New("breakpoint already exists at address")
	ErrBreakpointNotFound = errors.New("no breakpoint found")
)

// Breakpoint records a single software breakpoint.
// On x86-64, inserting a breakpoint overwrites the byte at Addr
// with 0xCC (INT3) and saves the displaced byte in Original.
type Breakpoint struct {
	ID       int
	Addr     uintptr
	Original byte // byte displaced by INT3
	Enabled  bool
	File     string // source file (empty if unknown)
	Line     int    // source line (0 if unknown)
}

// Table manages all breakpoints for a traced process.
type Table struct {
	next   int
	byAddr map[uintptr]*Breakpoint
	byID   map[int]*Breakpoint
}

// NewTable returns an empty breakpoint table.
func NewTable() *Table {
	return &Table{
		byAddr: make(map[uintptr]*Breakpoint),
		byID:   make(map[int]*Breakpoint),
	}
}

// Add records a breakpoint at addr with the displaced byte orig.
// Returns the breakpoint's numeric ID.
// Returns ErrBreakpointExists if addr is already occupied.
func (t *Table) Add(addr uintptr, orig byte, file string, line int) (int, error) {
	if _, ok := t.byAddr[addr]; ok {
		return 0, fmt.Errorf("%w: 0x%x", ErrBreakpointExists, addr)
	}
	t.next++
	bp := &Breakpoint{
		ID:       t.next,
		Addr:     addr,
		Original: orig,
		Enabled:  true,
		File:     file,
		Line:     line,
	}
	t.byAddr[addr] = bp
	t.byID[t.next] = bp
	return t.next, nil
}

// Get returns the breakpoint at addr, or false if none.
func (t *Table) Get(addr uintptr) (*Breakpoint, bool) {
	bp, ok := t.byAddr[addr]
	return bp, ok
}

// GetByID returns the breakpoint with the given ID, or false if none.
func (t *Table) GetByID(id int) (*Breakpoint, bool) {
	bp, ok := t.byID[id]
	return bp, ok
}

// Remove deletes the breakpoint with the given ID.
// Returns ErrBreakpointNotFound if the ID is unknown.
func (t *Table) Remove(id int) error {
	bp, ok := t.byID[id]
	if !ok {
		return fmt.Errorf("%w: id %d", ErrBreakpointNotFound, id)
	}
	delete(t.byAddr, bp.Addr)
	delete(t.byID, id)
	return nil
}

// Len returns the number of active breakpoints.
func (t *Table) Len() int {
	return len(t.byID)
}

// All returns all breakpoints sorted by address.
func (t *Table) All() []*Breakpoint {
	bps := make([]*Breakpoint, 0, len(t.byAddr))
	for _, bp := range t.byAddr {
		bps = append(bps, bp)
	}
	sort.Slice(bps, func(i, j int) bool {
		return bps[i].Addr < bps[j].Addr
	})
	return bps
}
```

### Exercise 2: ELF and DWARF Binary Parsing

Create `elf_info.go`. This file compiles on any OS; it reads ELF files as data without executing them.

```go
package debugger

import (
	"debug/dwarf"
	"debug/elf"
	"fmt"
	"io"
)

// Symbol is one entry from the ELF symbol table.
type Symbol struct {
	Name string
	Addr uint64
	Size uint64
}

// LineEntry maps a source location to a machine address.
type LineEntry struct {
	File string
	Line int
	Addr uint64
}

// BinaryInfo holds all symbol and source-line data extracted from an ELF binary.
type BinaryInfo struct {
	symbols    []Symbol
	addrToLine map[uint64]LineEntry
	lineToAddr map[string][]uint64 // "file:line" -> []addr (multiple for inlining)
}

// ParseBinary reads the ELF binary at path and extracts symbol and line tables.
// It succeeds even if the symbol table is stripped; it then uses only DWARF line info.
func ParseBinary(path string) (*BinaryInfo, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("debugger: open ELF %q: %w", path, err)
	}
	defer f.Close()

	if f.Machine != elf.EM_X86_64 {
		return nil, fmt.Errorf("debugger: %q is not an x86-64 ELF binary (machine: %s)", path, f.Machine)
	}

	bi := &BinaryInfo{
		addrToLine: make(map[uint64]LineEntry),
		lineToAddr: make(map[string][]uint64),
	}

	// Symbol table: stripped binaries return an error; treat as non-fatal.
	if syms, err := f.Symbols(); err == nil {
		for _, s := range syms {
			bi.symbols = append(bi.symbols, Symbol{
				Name: s.Name,
				Addr: s.Value,
				Size: s.Size,
			})
		}
	}

	dw, err := f.DWARF()
	if err != nil {
		return nil, fmt.Errorf("debugger: read DWARF from %q: %w", path, err)
	}
	if err := bi.parseLineTable(dw); err != nil {
		return nil, err
	}

	return bi, nil
}

func (bi *BinaryInfo) parseLineTable(dw *dwarf.Data) error {
	r := dw.Reader()
	for {
		entry, err := r.Next()
		if err != nil {
			return fmt.Errorf("debugger: DWARF reader: %w", err)
		}
		if entry == nil {
			break
		}
		if entry.Tag != dwarf.TagCompileUnit {
			continue
		}

		lr, err := dw.LineReader(entry)
		if err != nil {
			return fmt.Errorf("debugger: line reader: %w", err)
		}
		if lr == nil {
			r.SkipChildren()
			continue
		}

		var le dwarf.LineEntry
		for {
			if err := lr.Next(&le); err != nil {
				if err == io.EOF {
					break
				}
				return fmt.Errorf("debugger: line entry: %w", err)
			}
			if le.File == nil || le.EndSequence {
				continue
			}
			lEntry := LineEntry{
				File: le.File.Name,
				Line: le.Line,
				Addr: le.Address,
			}
			bi.addrToLine[le.Address] = lEntry
			key := fmt.Sprintf("%s:%d", le.File.Name, le.Line)
			bi.lineToAddr[key] = append(bi.lineToAddr[key], le.Address)
		}

		r.SkipChildren()
	}
	return nil
}

// LookupSymbol returns the virtual address of the named symbol, or false if not found.
func (bi *BinaryInfo) LookupSymbol(name string) (uint64, bool) {
	for _, s := range bi.symbols {
		if s.Name == name {
			return s.Addr, true
		}
	}
	return 0, false
}

// LookupAddr returns the source location for the given machine address.
func (bi *BinaryInfo) LookupAddr(addr uint64) (LineEntry, bool) {
	le, ok := bi.addrToLine[addr]
	return le, ok
}

// LookupLine returns all machine addresses for the given source file:line.
// Multiple addresses arise from inlining.
func (bi *BinaryInfo) LookupLine(file string, line int) ([]uint64, bool) {
	key := fmt.Sprintf("%s:%d", file, line)
	addrs, ok := bi.lineToAddr[key]
	return addrs, ok
}

// Symbols returns a copy of the symbol table.
func (bi *BinaryInfo) Symbols() []Symbol {
	out := make([]Symbol, len(bi.symbols))
	copy(out, bi.symbols)
	return out
}
```

### Exercise 3: ptrace Tracer (Linux only)

Create `debugger_linux.go`. The build constraint restricts this file to Linux; all ptrace calls must come from an OS-thread-pinned goroutine.

```go
//go:build linux

package debugger

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

// EventKind categorizes why the tracee stopped.
type EventKind int

const (
	EventStopped  EventKind = iota // stopped by a signal (breakpoint, step, etc.)
	EventExited                    // process exited normally
	EventSignaled                  // process killed by a signal
)

// Event is the result of waiting for the tracee to stop or exit.
type Event struct {
	Kind     EventKind
	Signal   syscall.Signal
	ExitCode int
}

// Tracer controls a Linux process via ptrace.
// All exported methods must be called from an OS-thread-pinned goroutine
// (see LockOSThread).
type Tracer struct {
	pid          int
	cmd          *exec.Cmd // non-nil if we launched the process
	bps          *Table
	binary       *BinaryInfo
	pendingRearm *Breakpoint // set when a bp was hit and must be re-armed before next continue
}

// LockOSThread pins the current goroutine to its OS thread.
// Call this once at the start of the goroutine that will issue all ptrace calls.
// Forgetting this causes "ptrace: operation not permitted" because ptrace requires
// every call to a given PID to come from the same OS thread.
func LockOSThread() {
	runtime.LockOSThread()
}

// Launch starts the binary at path under ptrace and returns a Tracer.
// The tracee is stopped immediately after exec; the caller issues Continue
// or Step to resume it.
func Launch(path string, args []string) (*Tracer, error) {
	bi, err := ParseBinary(path)
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(path, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("debugger: launch %q: %w", path, err)
	}

	// The child calls PTRACE_TRACEME before exec, so it receives SIGTRAP
	// immediately after exec. Consume that stop.
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(cmd.Process.Pid, &ws, 0, nil); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("debugger: wait after exec: %w", err)
	}

	return &Tracer{
		pid:    cmd.Process.Pid,
		cmd:    cmd,
		bps:    NewTable(),
		binary: bi,
	}, nil
}

// Attach attaches to a running process by PID.
func Attach(pid int, binaryPath string) (*Tracer, error) {
	bi, err := ParseBinary(binaryPath)
	if err != nil {
		return nil, err
	}
	if err := syscall.PtraceAttach(pid); err != nil {
		return nil, fmt.Errorf("debugger: attach pid %d: %w", pid, err)
	}
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		return nil, fmt.Errorf("debugger: wait after attach: %w", err)
	}
	return &Tracer{
		pid:    pid,
		bps:    NewTable(),
		binary: bi,
	}, nil
}

// SetBreakpoint inserts a software breakpoint at the first address for file:line.
// Returns the breakpoint ID.
func (t *Tracer) SetBreakpoint(file string, line int) (int, error) {
	addrs, ok := t.binary.LookupLine(file, line)
	if !ok || len(addrs) == 0 {
		return 0, fmt.Errorf("debugger: no address for %s:%d", file, line)
	}
	addr := uintptr(addrs[0])

	orig, err := t.readByte(addr)
	if err != nil {
		return 0, fmt.Errorf("debugger: read byte at 0x%x: %w", addr, err)
	}
	if err := t.writeByte(addr, 0xCC); err != nil {
		return 0, fmt.Errorf("debugger: write INT3 at 0x%x: %w", addr, err)
	}
	return t.bps.Add(addr, orig, file, line)
}

// RemoveBreakpoint removes the breakpoint with the given ID, restoring the original byte.
func (t *Tracer) RemoveBreakpoint(id int) error {
	bp, ok := t.bps.GetByID(id)
	if !ok {
		return fmt.Errorf("%w: id %d", ErrBreakpointNotFound, id)
	}
	if err := t.writeByte(bp.Addr, bp.Original); err != nil {
		return fmt.Errorf("debugger: restore byte at 0x%x: %w", bp.Addr, err)
	}
	return t.bps.Remove(id)
}

// Continue resumes the tracee until the next stop event.
// If a breakpoint was just hit, Continue re-arms it first: single-steps past the
// restored original instruction, then re-writes 0xCC, before issuing PTRACE_CONT.
func (t *Tracer) Continue() (Event, error) {
	if bp := t.pendingRearm; bp != nil {
		t.pendingRearm = nil
		// Single-step the restored original instruction.
		if err := syscall.PtraceSingleStep(t.pid); err != nil {
			return Event{}, fmt.Errorf("debugger: rearm single step: %w", err)
		}
		// Consume the SIGTRAP from the single step without going through handleTrap
		// (the SIGTRAP here is from the step, not an INT3).
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(t.pid, &ws, 0, nil); err != nil {
			return Event{}, fmt.Errorf("debugger: rearm wait: %w", err)
		}
		// Re-insert INT3.
		if err := t.writeByte(bp.Addr, 0xCC); err != nil {
			return Event{}, fmt.Errorf("debugger: rearm INT3 at 0x%x: %w", bp.Addr, err)
		}
	}

	if err := syscall.PtraceCont(t.pid, 0); err != nil {
		return Event{}, fmt.Errorf("debugger: ptrace cont: %w", err)
	}
	return t.waitEvent()
}

// Step executes a single machine instruction.
func (t *Tracer) Step() (Event, error) {
	if err := syscall.PtraceSingleStep(t.pid); err != nil {
		return Event{}, fmt.Errorf("debugger: single step: %w", err)
	}
	return t.waitEvent()
}

// Registers returns the current CPU register set.
func (t *Tracer) Registers() (syscall.PtraceRegs, error) {
	var regs syscall.PtraceRegs
	if err := syscall.PtraceGetRegs(t.pid, &regs); err != nil {
		return regs, fmt.Errorf("debugger: get regs: %w", err)
	}
	return regs, nil
}

// PC returns the current instruction pointer (RIP on x86-64).
func (t *Tracer) PC() (uint64, error) {
	regs, err := t.Registers()
	if err != nil {
		return 0, err
	}
	return regs.Rip, nil
}

// CurrentLocation returns the source location for the current PC.
// Returns an empty LineEntry if the address has no DWARF line entry.
func (t *Tracer) CurrentLocation() (LineEntry, error) {
	pc, err := t.PC()
	if err != nil {
		return LineEntry{}, err
	}
	le, _ := t.binary.LookupAddr(pc)
	return le, nil
}

// Detach stops tracing and lets the process run freely.
func (t *Tracer) Detach() error {
	if err := syscall.PtraceDetach(t.pid); err != nil {
		return fmt.Errorf("debugger: detach: %w", err)
	}
	return nil
}

// Breakpoints returns the breakpoint table.
func (t *Tracer) Breakpoints() *Table { return t.bps }

// Binary returns the parsed ELF/DWARF info.
func (t *Tracer) Binary() *BinaryInfo { return t.binary }

// -- internal --

// readByte reads a single byte from the tracee's memory at addr.
// PtracePeekText reads one machine word (8 bytes); we take byte 0.
func (t *Tracer) readByte(addr uintptr) (byte, error) {
	buf := make([]byte, 8)
	if _, err := syscall.PtracePeekText(t.pid, addr, buf); err != nil {
		return 0, fmt.Errorf("peek at 0x%x: %w", addr, err)
	}
	return buf[0], nil
}

// writeByte writes a single byte to the tracee's memory at addr.
// PtracePokeText operates on machine-word units: read the surrounding word,
// patch the target byte, write the word back.
func (t *Tracer) writeByte(addr uintptr, b byte) error {
	buf := make([]byte, 8)
	if _, err := syscall.PtracePeekText(t.pid, addr, buf); err != nil {
		return fmt.Errorf("peek for patch at 0x%x: %w", addr, err)
	}
	buf[0] = b
	if _, err := syscall.PtracePokeText(t.pid, addr, buf); err != nil {
		return fmt.Errorf("poke at 0x%x: %w", addr, err)
	}
	return nil
}

// waitEvent calls Wait4 and converts the WaitStatus to an Event.
// If the tracee stopped due to SIGTRAP from an INT3 breakpoint, handleTrap
// adjusts RIP and restores the original byte so execution can resume correctly.
func (t *Tracer) waitEvent() (Event, error) {
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(t.pid, &ws, 0, nil); err != nil {
		return Event{}, fmt.Errorf("debugger: wait4: %w", err)
	}

	ev := Event{}
	switch {
	case ws.Exited():
		ev.Kind = EventExited
		ev.ExitCode = ws.ExitStatus()
	case ws.Signaled():
		ev.Kind = EventSignaled
		ev.Signal = ws.Signal()
	default:
		ev.Kind = EventStopped
		ev.Signal = ws.StopSignal()
		if ev.Signal == syscall.SIGTRAP {
			if err := t.handleTrap(); err != nil {
				return ev, err
			}
		}
	}
	return ev, nil
}

// handleTrap is called when the tracee stops with SIGTRAP.
// On x86-64 the INT3 instruction leaves RIP one byte past the 0xCC byte;
// this function rewinds RIP and restores the original byte, then sets
// t.pendingRearm so the next Continue re-inserts INT3 after single-stepping.
func (t *Tracer) handleTrap() error {
	regs, err := t.Registers()
	if err != nil {
		return err
	}

	// INT3 is at RIP-1.
	bpAddr := uintptr(regs.Rip - 1)
	bp, ok := t.bps.Get(bpAddr)
	if !ok {
		// Not one of our breakpoints (e.g. a runtime SIGTRAP); leave RIP as is.
		return nil
	}

	// Restore the original byte.
	if err := t.writeByte(bpAddr, bp.Original); err != nil {
		return fmt.Errorf("debugger: restore at 0x%x: %w", bpAddr, err)
	}

	// Rewind RIP to the breakpoint address.
	regs.Rip = uint64(bpAddr)
	if err := syscall.PtraceSetRegs(t.pid, &regs); err != nil {
		return fmt.Errorf("debugger: set regs: %w", err)
	}

	// Mark for re-arm: Continue will single-step the restored instruction
	// and then re-insert INT3 before resuming the tracee.
	t.pendingRearm = bp
	return nil
}

// StatusString returns a human-readable name for a goroutine's atomicstatus value.
// Constants match runtime/runtime2.go in Go 1.26; verify against the source for
// other versions at https://github.com/golang/go/blob/master/src/runtime/runtime2.go.
func StatusString(s uint32) string {
	// The high bits of atomicstatus encode a "scan" flag; mask them off.
	const scanBit = 0x1000
	switch s &^ uint32(scanBit) {
	case 0:
		return "idle"
	case 1:
		return "runnable"
	case 2:
		return "running"
	case 3:
		return "syscall"
	case 4:
		return "waiting"
	case 6:
		return "dead"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}
```

### Exercise 4: Tests and Interactive REPL

Create `debugger_test.go`. The tests cover the cross-platform breakpoint table and `BinaryInfo` lookup logic; they run on any OS without a real binary.

```go
package debugger

import (
	"errors"
	"fmt"
	"testing"
)

// -- Breakpoint table tests --

func TestTableAddSetsFields(t *testing.T) {
	t.Parallel()

	tbl := NewTable()
	id, err := tbl.Add(0x400100, 0x55, "main.go", 10)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if id != 1 {
		t.Fatalf("id = %d, want 1", id)
	}
	bp, ok := tbl.Get(0x400100)
	if !ok {
		t.Fatal("Get: breakpoint not found after Add")
	}
	if bp.Addr != 0x400100 || bp.Original != 0x55 || bp.File != "main.go" || bp.Line != 10 {
		t.Fatalf("bp = %+v", bp)
	}
	if !bp.Enabled {
		t.Fatal("new breakpoint should be enabled")
	}
}

func TestTableDuplicateReturnsError(t *testing.T) {
	t.Parallel()

	tbl := NewTable()
	if _, err := tbl.Add(0x400100, 0x55, "main.go", 10); err != nil {
		t.Fatal(err)
	}
	_, err := tbl.Add(0x400100, 0x90, "main.go", 10)
	if !errors.Is(err, ErrBreakpointExists) {
		t.Fatalf("err = %v, want ErrBreakpointExists", err)
	}
}

func TestTableRemoveDeletesEntry(t *testing.T) {
	t.Parallel()

	tbl := NewTable()
	id, _ := tbl.Add(0x400100, 0x55, "main.go", 10)
	if err := tbl.Remove(id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := tbl.Get(0x400100); ok {
		t.Fatal("breakpoint should be gone after Remove")
	}
	if tbl.Len() != 0 {
		t.Fatalf("Len = %d, want 0", tbl.Len())
	}
}

func TestTableRemoveNotFoundReturnsError(t *testing.T) {
	t.Parallel()

	tbl := NewTable()
	if err := tbl.Remove(99); !errors.Is(err, ErrBreakpointNotFound) {
		t.Fatalf("err = %v, want ErrBreakpointNotFound", err)
	}
}

func TestTableLenTracksCount(t *testing.T) {
	t.Parallel()

	tbl := NewTable()
	if tbl.Len() != 0 {
		t.Fatalf("Len = %d, want 0", tbl.Len())
	}
	if _, err := tbl.Add(0x400100, 0x55, "a.go", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := tbl.Add(0x400200, 0x48, "a.go", 2); err != nil {
		t.Fatal(err)
	}
	if tbl.Len() != 2 {
		t.Fatalf("Len = %d, want 2", tbl.Len())
	}
}

func TestTableAllSortedByAddr(t *testing.T) {
	t.Parallel()

	tbl := NewTable()
	addrs := []uintptr{0x400300, 0x400100, 0x400200}
	for i, a := range addrs {
		if _, err := tbl.Add(a, 0x55, "a.go", i+1); err != nil {
			t.Fatal(err)
		}
	}
	all := tbl.All()
	if len(all) != 3 {
		t.Fatalf("All() len = %d, want 3", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Addr <= all[i-1].Addr {
			t.Fatalf("All() not sorted: [%d]=0x%x >= [%d]=0x%x",
				i-1, all[i-1].Addr, i, all[i].Addr)
		}
	}
}

func TestTableIDsAreSequential(t *testing.T) {
	t.Parallel()

	tbl := NewTable()
	for i := 0; i < 5; i++ {
		id, err := tbl.Add(uintptr(0x400000+i*0x10), 0x55, "a.go", i+1)
		if err != nil {
			t.Fatalf("Add[%d]: %v", i, err)
		}
		if id != i+1 {
			t.Fatalf("id = %d, want %d", id, i+1)
		}
	}
}

func TestTableGetByIDRoundTrip(t *testing.T) {
	t.Parallel()

	tbl := NewTable()
	id, err := tbl.Add(0x401000, 0xCC, "lib.go", 42)
	if err != nil {
		t.Fatal(err)
	}
	bp, ok := tbl.GetByID(id)
	if !ok {
		t.Fatal("GetByID: not found")
	}
	if bp.ID != id || bp.Addr != 0x401000 {
		t.Fatalf("bp = %+v", bp)
	}
}

// -- BinaryInfo lookup tests (no real ELF binary needed) --

func makeBinaryInfo() *BinaryInfo {
	return &BinaryInfo{
		symbols: []Symbol{
			{Name: "main.main", Addr: 0x401000, Size: 256},
			{Name: "runtime.main", Addr: 0x402000, Size: 512},
		},
		addrToLine: map[uint64]LineEntry{
			0x401000: {File: "main.go", Line: 10, Addr: 0x401000},
			0x401010: {File: "main.go", Line: 11, Addr: 0x401010},
			0x401020: {File: "main.go", Line: 12, Addr: 0x401020},
		},
		lineToAddr: map[string][]uint64{
			"main.go:10": {0x401000},
			"main.go:11": {0x401010},
			"main.go:12": {0x401020},
		},
	}
}

func TestBinaryInfoLookupSymbol(t *testing.T) {
	t.Parallel()

	bi := makeBinaryInfo()
	addr, ok := bi.LookupSymbol("main.main")
	if !ok {
		t.Fatal("LookupSymbol: main.main not found")
	}
	if addr != 0x401000 {
		t.Fatalf("addr = 0x%x, want 0x401000", addr)
	}
	if _, ok := bi.LookupSymbol("nonexistent"); ok {
		t.Fatal("LookupSymbol: expected false for unknown symbol")
	}
}

func TestBinaryInfoLookupLine(t *testing.T) {
	t.Parallel()

	bi := makeBinaryInfo()
	addrs, ok := bi.LookupLine("main.go", 10)
	if !ok {
		t.Fatal("LookupLine: main.go:10 not found")
	}
	if len(addrs) != 1 || addrs[0] != 0x401000 {
		t.Fatalf("addrs = %v, want [0x401000]", addrs)
	}
	if _, ok := bi.LookupLine("main.go", 99); ok {
		t.Fatal("LookupLine: expected false for unknown line")
	}
}

func TestBinaryInfoLookupAddr(t *testing.T) {
	t.Parallel()

	bi := makeBinaryInfo()
	le, ok := bi.LookupAddr(0x401010)
	if !ok {
		t.Fatal("LookupAddr: 0x401010 not found")
	}
	if le.File != "main.go" || le.Line != 11 {
		t.Fatalf("le = %+v", le)
	}
	if _, ok := bi.LookupAddr(0xdeadbeef); ok {
		t.Fatal("LookupAddr: expected false for unknown address")
	}
}

func TestBinaryInfoSymbolsCopied(t *testing.T) {
	t.Parallel()

	bi := makeBinaryInfo()
	s1 := bi.Symbols()
	s2 := bi.Symbols()
	if len(s1) != len(s2) {
		t.Fatalf("Symbols() len mismatch: %d vs %d", len(s1), len(s2))
	}
	// Mutate the copy and confirm the original is unchanged.
	s1[0].Name = "mutated"
	if bi.symbols[0].Name == "mutated" {
		t.Fatal("Symbols() returned the internal slice, not a copy")
	}
}

// -- Example functions (auto-verified by go test) --

func ExampleTable_Add() {
	tbl := NewTable()
	id, err := tbl.Add(0x400100, 0x55, "main.go", 10)
	if err != nil {
		panic(err)
	}
	fmt.Printf("breakpoint %d at %s:%d\n", id, "main.go", 10)
	// Output: breakpoint 1 at main.go:10
}

func ExampleTable_Remove() {
	tbl := NewTable()
	id, _ := tbl.Add(0x400100, 0x55, "main.go", 10)
	_ = tbl.Remove(id)
	fmt.Printf("len after remove: %d\n", tbl.Len())
	// Output: len after remove: 0
}

// Your turn: add TestTableAddThenRemoveThenAddSameAddr to verify that after
// removing a breakpoint the same address can be registered again without
// getting ErrBreakpointExists. The new ID must be 2 (the sequence counter
// never resets), and Get(addr) must succeed.
```

Create `cmd/demo/main.go`. The REPL is Linux-only because `debugger.Launch` and `debugger.Tracer` are only compiled on Linux.

```go
//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"example.com/debugger"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: demo <binary> [args...]")
		os.Exit(1)
	}

	// Pin to an OS thread: all ptrace calls must originate from the same thread.
	runtime.LockOSThread()

	t, err := debugger.Launch(os.Args[1], os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "launch: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("launched %s\n", os.Args[1])
	fmt.Println("commands: break <file>:<line>, continue, step, regs, where, info breakpoints, delete <id>, quit")

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("(dbg) ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		switch parts[0] {

		case "break", "b":
			if len(parts) < 2 {
				fmt.Println("usage: break <file>:<line>")
				continue
			}
			col := strings.LastIndex(parts[1], ":")
			if col < 0 {
				fmt.Printf("invalid location %q: expected file:line\n", parts[1])
				continue
			}
			file := parts[1][:col]
			lineNum, err := strconv.Atoi(parts[1][col+1:])
			if err != nil {
				fmt.Printf("invalid line number: %v\n", err)
				continue
			}
			id, err := t.SetBreakpoint(file, lineNum)
			if err != nil {
				fmt.Printf("set breakpoint: %v\n", err)
				continue
			}
			fmt.Printf("breakpoint %d set at %s:%d\n", id, file, lineNum)

		case "continue", "c":
			ev, err := t.Continue()
			if err != nil {
				fmt.Printf("continue: %v\n", err)
				continue
			}
			printEvent(t, ev)

		case "step", "s":
			ev, err := t.Step()
			if err != nil {
				fmt.Printf("step: %v\n", err)
				continue
			}
			printEvent(t, ev)

		case "regs":
			regs, err := t.Registers()
			if err != nil {
				fmt.Printf("registers: %v\n", err)
				continue
			}
			fmt.Printf("  RIP=0x%016x  RSP=0x%016x  RBP=0x%016x\n",
				regs.Rip, regs.Rsp, regs.Rbp)
			fmt.Printf("  RAX=0x%016x  RBX=0x%016x  RCX=0x%016x\n",
				regs.Rax, regs.Rbx, regs.Rcx)

		case "where", "backtrace", "bt":
			loc, err := t.CurrentLocation()
			if err != nil {
				fmt.Printf("location: %v\n", err)
				continue
			}
			if loc.File != "" {
				fmt.Printf("  %s:%d (0x%x)\n", loc.File, loc.Line, loc.Addr)
			} else {
				pc, _ := t.PC()
				fmt.Printf("  0x%x (no source info)\n", pc)
			}

		case "info":
			if len(parts) < 2 || parts[1] != "breakpoints" {
				fmt.Println("usage: info breakpoints")
				continue
			}
			bps := t.Breakpoints().All()
			if len(bps) == 0 {
				fmt.Println("no breakpoints")
				continue
			}
			for _, bp := range bps {
				state := "enabled"
				if !bp.Enabled {
					state = "disabled"
				}
				fmt.Printf("  %d  0x%x  %s:%d  %s\n",
					bp.ID, bp.Addr, bp.File, bp.Line, state)
			}

		case "delete", "d":
			if len(parts) < 2 {
				fmt.Println("usage: delete <id>")
				continue
			}
			id, err := strconv.Atoi(parts[1])
			if err != nil {
				fmt.Printf("invalid id: %v\n", err)
				continue
			}
			if err := t.RemoveBreakpoint(id); err != nil {
				fmt.Printf("delete: %v\n", err)
				continue
			}
			fmt.Printf("breakpoint %d removed\n", id)

		case "quit", "q":
			_ = t.Detach()
			return

		default:
			fmt.Printf("unknown command %q\n", parts[0])
		}
	}
}

func printEvent(t *debugger.Tracer, ev debugger.Event) {
	switch ev.Kind {
	case debugger.EventExited:
		fmt.Printf("process exited with code %d\n", ev.ExitCode)
	case debugger.EventSignaled:
		fmt.Printf("process killed by signal %s\n", ev.Signal)
	case debugger.EventStopped:
		loc, _ := t.CurrentLocation()
		if loc.File != "" {
			fmt.Printf("stopped at %s:%d\n", loc.File, loc.Line)
		} else {
			pc, _ := t.PC()
			fmt.Printf("stopped at 0x%x\n", pc)
		}
	}
}
```

## Common Mistakes

### Forgetting runtime.LockOSThread

Wrong: starting a goroutine and issuing ptrace calls from it without `runtime.LockOSThread()`.

What happens: `ptrace: operation not permitted` on any call after the scheduler migrates the goroutine to a different OS thread. The error is intermittent and hard to reproduce.

Fix: call `runtime.LockOSThread()` at the very start of the goroutine that owns the tracer, before calling `Launch` or `Attach`. In `cmd/demo`, this is done in `main()` because `main` itself is the ptrace goroutine.

### Not Re-arming the Breakpoint After It Fires

Wrong: restore the original byte and resume with `Continue` without single-stepping past the instruction first.

What happens: the breakpoint fires exactly once; on the next `Continue`, execution resumes at the restored original instruction and the breakpoint is gone.

Fix: after restoring the original byte and rewinding `RIP`, single-step past the restored instruction before re-inserting `0xCC`. In the `Tracer`, the `pendingRearm` field tracks this state; `Continue` checks it before every `PTRACE_CONT`.

### Writing Only One Byte with PtracePokeText

Wrong:

```go
// This clobbers bytes 1-7 with zeros, corrupting the instruction stream.
buf := []byte{0xCC}
syscall.PtracePokeText(t.pid, addr, buf)
```

What happens: `PTRACE_POKETEXT` writes a full machine word. Passing a 1-byte slice on a 64-bit kernel pads the remaining 7 bytes to zero, overwriting valid instructions after the breakpoint address.

Fix: read the 8-byte word first, replace byte 0, write the whole word back:

```go
buf := make([]byte, 8)
syscall.PtracePeekText(t.pid, addr, buf)
buf[0] = 0xCC
syscall.PtracePokeText(t.pid, addr, buf)
```

### Matching RIP Against the Breakpoint Address Before Adjusting

Wrong:

```go
// RIP already points one byte past INT3; this lookup misses.
bpAddr := uintptr(regs.Rip)
bp, ok := t.bps.Get(bpAddr)
```

What happens: `Get` returns false for every breakpoint; the debugger does not recognize that it stopped at one of its own breakpoints, so neither RIP nor the original byte is restored.

Fix: on x86-64, subtract 1 from `RIP` to get the INT3 address:

```go
bpAddr := uintptr(regs.Rip - 1)
bp, ok := t.bps.Get(bpAddr)
```

### Hardcoding Goroutine Struct Offsets

Wrong: hardcoding the byte offset of `goid` in the `g` struct based on one Go version.

What happens: offsets change between minor versions; the debugger silently reads garbage goroutine IDs on a different toolchain.

Fix: read the offsets from the target binary's DWARF `.debug_info` for the `runtime.g` type at runtime. `debug/dwarf` exposes the byte offset of each field via `DW_AT_data_member_location`; parse it from the compile-unit DIE for the `runtime` package.

## Verification

On Linux, with a compiled Go target binary at `./target`:

```bash
cd ~/go-exercises/debugger

# 1. Format check (all files, including linux-specific ones).
test -z "$(gofmt -l .)"

# 2. Vet the cross-platform package (elf_info.go + breakpoint.go).
#    On Linux, go vet also covers debugger_linux.go.
go vet ./...

# 3. Cross-platform tests (breakpoint table + BinaryInfo lookups).
go test -count=1 -race ./...

# 4. On Linux: build the full package and cmd/demo.
GOOS=linux go build ./...
GOOS=linux go build ./cmd/demo

# 5. Integration: launch a Go binary and exercise the REPL.
go run ./cmd/demo ./target
# (dbg) break main.go:10
# (dbg) continue
# (dbg) where
# (dbg) regs
# (dbg) step
# (dbg) info breakpoints
# (dbg) delete 1
# (dbg) quit
```

The cross-platform tests (`go test -count=1 -race ./...`) must pass on any OS. The integration test requires Linux. A test for `TestTableAddThenRemoveThenAddSameAddr` is left for you to write as described at the end of Exercise 4.

## Summary

- `ptrace` provides process control; all ptrace calls to a PID must come from the same OS thread. Call `runtime.LockOSThread()` before the first ptrace call.
- An INT3 breakpoint (`0xCC`) fires `SIGTRAP` and leaves `RIP` one byte past the instruction. The debugger must rewind `RIP`, restore the original byte, single-step past it, and re-insert `0xCC` to keep the breakpoint active.
- `PTRACE_POKETEXT` operates on 8-byte machine words; read the surrounding word, patch one byte, write the word back. Passing a shorter slice to `PtracePokeText` silently zero-pads.
- `debug/elf` parses ELF symbol tables; `debug/dwarf` line readers map machine addresses to source file:line positions. Both packages are cross-platform (they parse file formats, not run binaries).
- Goroutine inspection reads the `runtime.allgs` slice from the tracee's memory. Struct field offsets are Go-version-specific; read them from the target binary's DWARF info rather than hard-coding them.
- The breakpoint table is the only cross-platform, fully testable component; gate correctness there first before debugging on Linux.

## What's Next

Next: [Directory-Confined Filesystem with os.Root](../../48-modern-go-language-and-stdlib/01-os-root-directory-sandboxing/00-concepts.md) — the first lesson of the modern-Go / trending-topics wave (chapters 48-54).

## Resources

- [pkg.go.dev/syscall — PtraceAttach, PtraceCont, PtraceGetRegs and related](https://pkg.go.dev/syscall#PtraceAttach)
- [pkg.go.dev/debug/elf — ELF file parsing, Symbols, DWARF](https://pkg.go.dev/debug/elf)
- [pkg.go.dev/debug/dwarf — DWARF data, LineReader, LineEntry](https://pkg.go.dev/debug/dwarf)
- [Delve debugger source — reference Go ptrace implementation](https://github.com/go-delve/delve)
- [DWARF 5 Standard — line table and location expression specification](https://dwarfstd.org/dwarf5std.html)
