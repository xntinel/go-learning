# 6. Seccomp Filter Engine

Seccomp-BPF lets a process install a classic BPF program that the kernel runs on every system call to decide allow, deny, kill, or trap. The hard parts are three: (1) the BPF assembler works at the instruction level and has no error messages — a wrong jump offset silently produces the wrong policy; (2) Go's goroutine scheduler moves goroutines between OS threads, so a per-thread filter installation leaves other threads unprotected unless `SECCOMP_FILTER_FLAG_TSYNC` is used; (3) the filter runs before the kernel checks arguments, so argument filtering requires two 32-bit loads per 64-bit argument on both amd64 and arm64.

This lesson builds a pure-Go seccomp library: a policy DSL, a BPF compiler, and a Linux installation layer. The compiler and policy types are offline-testable; the installation layer requires Linux and `golang.org/x/sys/unix`.

```text
seccomp/
  go.mod
  policy.go           (Policy, Rule, Action, ArgFilter)
  bpf.go              (Insn, instruction helpers, seccomp_data offsets)
  compiler.go         (Compile: policy -> []Insn)
  filter_linux.go     (Install, InstallProcess — linux+amd64 only)
  seccomp_test.go     (unit tests + Example functions)
  cmd/demo/main.go    (runnable demo — linux+amd64 only)
```

## Concepts

### The seccomp_data Struct and BPF Accumulator

When a process makes a system call the kernel fills a `seccomp_data` struct and hands it to the BPF program. The BPF machine has a single accumulator register (A). The program loads fields from `seccomp_data` using `BPF_LD | BPF_W | BPF_ABS` with an absolute byte offset. Classic BPF is 32-bit: each load fills A with 32 bits. For 64-bit syscall arguments, two loads are required — the low 32 bits and the high 32 bits.

```
offset  field                       size
     0  nr (syscall number)          4 bytes
     4  arch                         4 bytes
     8  instruction_pointer          8 bytes
    16  args[0]                      8 bytes  (lo at 16, hi at 20)
    24  args[1]                      8 bytes  (lo at 24, hi at 28)
    32  args[2]                      8 bytes
    40  args[3]                      8 bytes
    48  args[4]                      8 bytes
    56  args[5]                      8 bytes
```

### BPF Instruction Encoding

Each BPF instruction is 8 bytes: `{Code uint16, Jt uint8, Jf uint8, K uint32}`. The Code field ORs together class, size modifier, and mode or op modifier. For conditional jumps, `Jt` is the number of instructions to skip when the condition is true, and `Jf` when false; a value of 0 means "fall through to the next instruction". The maximum BPF program length is 4096 instructions.

Common instruction patterns in a seccomp filter:

| Intent | Code | K |
|---|---|---|
| Load word at absolute offset | `BPF_LD\|BPF_W\|BPF_ABS` = 0x20 | byte offset into seccomp_data |
| Jump if A equals K | `BPF_JMP\|BPF_JEQ\|BPF_K` = 0x15 | comparison value |
| Jump if A > K | `BPF_JMP\|BPF_JGT\|BPF_K` = 0x25 | comparison value |
| Jump if A & K != 0 | `BPF_JMP\|BPF_JSET\|BPF_K` = 0x45 | bitmask |
| Return constant verdict | `BPF_RET\|BPF_K` = 0x06 | SECCOMP_RET_* value |

### Architecture Guard

Syscall numbers differ between personalities. A 32-bit process running on a 64-bit kernel presents different numbers than `seccomp_data.nr` would suggest under the 64-bit ABI. Always verify `seccomp_data.arch` as the first three instructions — if the arch does not match the expected value, kill the process immediately instead of applying policy with wrong syscall numbers.

### Verdict Encoding

The `K` field of a `BPF_RET` instruction is a 32-bit verdict. The high bits select the action; the low 16 bits carry optional data (the errno for `SECCOMP_RET_ERRNO`).

| Action | Value | Effect |
|---|---|---|
| SECCOMP_RET_ALLOW | 0x7fff0000 | Syscall proceeds normally |
| SECCOMP_RET_KILL_PROCESS | 0x80000000 | Whole process killed with SIGSYS |
| SECCOMP_RET_TRAP | 0x00030000 | SIGSYS delivered; process can handle it |
| SECCOMP_RET_ERRNO\|e | 0x00050000\|e | Syscall returns -e to the caller |
| SECCOMP_RET_LOG | 0x7ffc0000 | Allow and log (kernel 4.14+) |

### Go Threads and TSYNC

Go multiplexes goroutines onto OS threads. `prctl(PR_SET_SECCOMP)` installs the filter only on the calling OS thread. A goroutine that migrates to a different thread bypasses the filter. Two safe approaches:

1. `runtime.LockOSThread()` pins the goroutine to one thread; use this for a helper goroutine that does only restricted work.
2. The `seccomp(2)` syscall with `SECCOMP_FILTER_FLAG_TSYNC` (= 1) copies the filter to all threads atomically. This is the correct approach when sandboxing the whole process.

### Jump Offset Arithmetic

A forward jump of N in `Jt` or `Jf` skips N instructions after the current one, landing at index `(current + 1 + N)`. Planning the BPF program so each rule's jump can be calculated without fixups requires building each rule body independently and measuring its length before emitting the JEQ header.

## Exercises

Set up the module:

```bash
go get golang.org/x/sys
```

### Exercise 1: Policy DSL

Create `policy.go`:

```go
package seccomp

import "errors"

// Action is the seccomp verdict for a matching rule.
type Action uint32

const (
	// ActionAllow permits the syscall (SECCOMP_RET_ALLOW).
	ActionAllow Action = 0x7fff0000
	// ActionKill terminates the whole process (SECCOMP_RET_KILL_PROCESS, kernel 4.14+).
	ActionKill Action = 0x80000000
	// ActionTrap delivers SIGSYS without executing the syscall (SECCOMP_RET_TRAP).
	ActionTrap Action = 0x00030000
	// ActionLog allows the syscall and logs it (SECCOMP_RET_LOG, kernel 4.14+).
	ActionLog Action = 0x7ffc0000
)

// ActionErrno returns a verdict that makes the syscall fail with the given errno.
// errno must be a positive POSIX error number (e.g. unix.EPERM = 1).
func ActionErrno(errno uint16) Action {
	return Action(0x00050000 | uint32(errno))
}

// ArgOp is a comparison operator for an argument filter.
type ArgOp uint8

const (
	ArgOpEQ  ArgOp = iota // argument == value (64-bit, two 32-bit loads)
	ArgOpNE               // argument != value (low 32 bits only)
	ArgOpAnd              // (argument & value) != 0 (low 32 bits only)
)

// ArgFilter restricts a Rule to syscalls where the argument at Index
// satisfies the operator applied to Value.
//
// 64-bit argument values are compared as two 32-bit halves for ArgOpEQ.
// ArgOpNE and ArgOpAnd compare only the low 32 bits; this is sufficient for
// flag arguments such as open(2) flags and mmap(2) protections.
type ArgFilter struct {
	Index uint8 // argument index 0-5
	Op    ArgOp
	Value uint64
}

// Rule maps a syscall number to a verdict, with optional argument filters.
// If ArgFilters is non-empty, ALL filters must match (AND semantics) for
// the Rule action to apply; any mismatch falls through to the Policy default.
type Rule struct {
	SyscallNr  uint32
	Action     Action
	ArgFilters []ArgFilter
}

// Policy is the top-level description of the seccomp filter.
type Policy struct {
	// Default is the verdict for syscalls not matched by any Rule.
	Default Action
	// Rules are evaluated in order; the first matching rule wins.
	Rules []Rule
}

var (
	// ErrTooManyRules is returned when the compiled BPF program exceeds
	// the kernel's 4096-instruction limit.
	ErrTooManyRules = errors.New("seccomp: policy exceeds 4096 BPF instructions")
)
```

The `ArgFilter` comment documents the 32-bit limitation for ArgOpNE and ArgOpAnd — this is a real constraint of classic BPF's 32-bit accumulator. The 64-bit case for ArgOpEQ is handled with two loads.

### Exercise 2: BPF Assembler Helpers

Create `bpf.go`:

```go
package seccomp

// Insn is a single classic BPF instruction (mirrors unix.SockFilter).
// Code is formed by ORing the class, size, and mode/op constants below.
type Insn struct {
	Code uint16
	Jt   uint8 // jump-if-true: instructions to skip past next
	Jf   uint8 // jump-if-false: instructions to skip past next
	K    uint32
}

// BPF instruction code components. Values match linux/filter.h.
const (
	bpfLD  uint16 = 0x00 // class: load
	bpfJMP uint16 = 0x05 // class: jump
	bpfRET uint16 = 0x06 // class: return

	bpfW   uint16 = 0x00 // size modifier: 32-bit word
	bpfABS uint16 = 0x20 // mode modifier: absolute offset in seccomp_data

	bpfK    uint16 = 0x00 // src: constant K
	bpfJEQ  uint16 = 0x10 // jump op: equal
	bpfJGT  uint16 = 0x20 // jump op: greater-than (unsigned)
	bpfJSET uint16 = 0x40 // jump op: bitwise AND non-zero
)

// Byte offsets into struct seccomp_data (same on amd64 and arm64).
const (
	offNr   uint32 = 0 // syscall number (uint32)
	offArch uint32 = 4 // architecture (uint32, AUDIT_ARCH_*)
	// offArgs(i) returns the byte offset of args[i] in seccomp_data.
)

func offArgLo(i uint8) uint32 { return 16 + uint32(i)*8 }
func offArgHi(i uint8) uint32 { return 16 + uint32(i)*8 + 4 }

// archAMD64 is AUDIT_ARCH_X86_64 from <sys/audit.h>.
const archAMD64 uint32 = 0xC000003E

// maxInsn is the kernel hard limit on classic BPF program length.
const maxInsn = 4096

// instrLoad emits BPF_LD | BPF_W | BPF_ABS: load the 32-bit word at byteOffset
// from seccomp_data into the accumulator A.
func instrLoad(byteOffset uint32) Insn {
	return Insn{Code: bpfLD | bpfW | bpfABS, K: byteOffset}
}

// instrJEQ emits BPF_JMP | BPF_JEQ | BPF_K: if A == k, skip jt instructions;
// otherwise skip jf instructions. A value of 0 means fall through.
func instrJEQ(k uint32, jt, jf uint8) Insn {
	return Insn{Code: bpfJMP | bpfJEQ | bpfK, Jt: jt, Jf: jf, K: k}
}

// instrJGT emits BPF_JMP | BPF_JGT | BPF_K: if A > k (unsigned), skip jt; else jf.
func instrJGT(k uint32, jt, jf uint8) Insn {
	return Insn{Code: bpfJMP | bpfJGT | bpfK, Jt: jt, Jf: jf, K: k}
}

// instrJSET emits BPF_JMP | BPF_JSET | BPF_K: if A & k != 0, skip jt; else jf.
func instrJSET(k uint32, jt, jf uint8) Insn {
	return Insn{Code: bpfJMP | bpfJSET | bpfK, Jt: jt, Jf: jf, K: k}
}

// instrRet emits BPF_RET | BPF_K: return the verdict in k.
func instrRet(verdict uint32) Insn {
	return Insn{Code: bpfRET | bpfK, K: verdict}
}
```

### Exercise 3: Policy Compiler

Create `compiler.go`:

```go
package seccomp

import "fmt"

// Compile translates a Policy into a slice of BPF instructions.
//
// Program layout:
//  1. Architecture guard (3 instructions): kill if arch != AUDIT_ARCH_X86_64.
//  2. Load syscall number (1 instruction).
//  3. One block per Rule: JEQ header + body instructions.
//  4. Default verdict (1 instruction).
//
// Each Rule block has the structure:
//
//	JEQ(syscallNr, jt=0, jf=len(body))  -- mismatch: jump over body
//	[body: arg-filter checks + verdict]
//	[reload nr for next rule, if this is not the last rule]
//
// Arg-filter mismatch inside a body falls through to instrRet(p.Default),
// not to the Rule's verdict. This gives "AND semantics": all filters must pass.
func Compile(p *Policy) ([]Insn, error) {
	// Degenerate case: no rules — a single-instruction program.
	if len(p.Rules) == 0 {
		return []Insn{instrRet(uint32(p.Default))}, nil
	}

	var prog []Insn

	// 1. Architecture guard.
	// instrJEQ(archAMD64, 1, 0): if A==archAMD64 skip 1 (skip the kill); else fall to kill.
	prog = append(prog,
		instrLoad(offArch),
		instrJEQ(archAMD64, 1, 0),
		instrRet(uint32(ActionKill)),
	)

	// 2. Load syscall number into accumulator.
	prog = append(prog, instrLoad(offNr))

	// 3. Rule blocks.
	for i, rule := range p.Rules {
		body := compileBody(rule, p.Default)
		// JEQ: if nr matches, fall through (jt=0) into body;
		// if no match, jump over body (jf=len(body)).
		prog = append(prog, instrJEQ(rule.SyscallNr, 0, uint8(len(body))))
		prog = append(prog, body...)
		// After the body the accumulator holds whatever the last body load left.
		// Reload nr for the next rule's JEQ, except after the final rule (where
		// the next instruction is the default verdict that does not need A).
		if i < len(p.Rules)-1 {
			prog = append(prog, instrLoad(offNr))
		}
	}

	// 4. Default verdict.
	prog = append(prog, instrRet(uint32(p.Default)))

	if len(prog) > maxInsn {
		return nil, fmt.Errorf("%w: %d instructions (limit %d)",
			ErrTooManyRules, len(prog), maxInsn)
	}
	return prog, nil
}

// compileBody builds the instructions executed when a rule's syscall number
// matches. The body ends with instrRet(rule.Action). Each arg-filter check
// that fails emits instrRet(defaultAction) so the caller returns the policy
// default rather than the rule action on a filter mismatch.
//
// For ArgOpEQ the 64-bit value is split into two 32-bit halves:
//
//	load lo; JEQ(loVal, skip1, 0); ret(default)  // mismatch on lo
//	load hi; JEQ(hiVal, skip1, 0); ret(default)  // mismatch on hi
//
// jt=1 skips the immediately following ret(default) on a match.
func compileBody(rule Rule, defaultAction Action) []Insn {
	var body []Insn
	def := uint32(defaultAction)

	for _, af := range rule.ArgFilters {
		switch af.Op {
		case ArgOpEQ:
			loVal := uint32(af.Value)
			hiVal := uint32(af.Value >> 32)
			body = append(body,
				instrLoad(offArgLo(af.Index)),
				instrJEQ(loVal, 1, 0), // lo match: skip ret(def); mismatch: fall
				instrRet(def),
				instrLoad(offArgHi(af.Index)),
				instrJEQ(hiVal, 1, 0),
				instrRet(def),
			)
		case ArgOpNE:
			// Applies the rule action only when the low 32 bits differ from value.
			loVal := uint32(af.Value)
			body = append(body,
				instrLoad(offArgLo(af.Index)),
				instrJEQ(loVal, 0, 1), // lo match: fall (continue to ret(def)); differ: skip ret(def)
				instrRet(def),
			)
		case ArgOpAnd:
			// Applies the rule action only when (arg & value) != 0 (low 32 bits).
			loVal := uint32(af.Value)
			body = append(body,
				instrLoad(offArgLo(af.Index)),
				instrJSET(loVal, 1, 0), // bits set: skip ret(def); bits clear: fall
				instrRet(def),
			)
		}
	}

	body = append(body, instrRet(uint32(rule.Action)))
	return body
}
```

The `jt=1` trick — "skip 1 on match to jump over the immediately following default return" — keeps jump offsets computable before the program is assembled. No second pass is needed.

### Exercise 4: Installation Layer (Linux + amd64)

Create `filter_linux.go`:

```go
//go:build linux && amd64

package seccomp

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// syNrSeccomp is the seccomp(2) syscall number on amd64.
// Named constants from golang.org/x/sys/unix are used for prctl arguments;
// the raw number is required for the seccomp(2) syscall itself (available since Linux 3.17).
const syNrSeccomp uintptr = 317

const (
	seccompSetModeFilter   uintptr = 1
	seccompFilterFlagTsync uintptr = 1 // synchronize filter to all threads
)

// Install converts prog to a sock_fprog and installs it on the calling OS thread.
// The caller must hold runtime.LockOSThread() for the duration of the restricted
// work; use InstallProcess to sandbox all threads at once.
func Install(prog []Insn) error {
	return applyFilter(prog, 0)
}

// InstallProcess copies prog to every OS thread in the current process using
// SECCOMP_FILTER_FLAG_TSYNC. This is the correct approach for Go programs:
// without TSYNC, goroutines scheduled onto unfiltered threads bypass the policy.
// Requires Linux 3.17+. Returns an error if any thread rejects the filter.
func InstallProcess(prog []Insn) error {
	// LockOSThread is not needed for TSYNC (the kernel applies to all threads),
	// but it prevents the Go scheduler from adding new threads while the filter
	// is being installed. Unlock immediately after.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	return applyFilter(prog, seccompFilterFlagTsync)
}

func applyFilter(prog []Insn, flags uintptr) error {
	if len(prog) == 0 {
		return fmt.Errorf("seccomp: empty program")
	}
	filters := make([]unix.SockFilter, len(prog))
	for i, ins := range prog {
		filters[i] = unix.SockFilter{Code: ins.Code, Jt: ins.Jt, Jf: ins.Jf, K: ins.K}
	}
	fprog := unix.SockFprog{
		Len:    uint16(len(filters)),
		Filter: &filters[0],
	}
	// PR_SET_NO_NEW_PRIVS must be set before PR_SET_SECCOMP unless the process
	// holds CAP_SYS_ADMIN. Without it the kernel rejects the filter with EACCES.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("seccomp: PR_SET_NO_NEW_PRIVS: %w", err)
	}
	if flags == 0 {
		// Single-thread installation via prctl.
		if err := unix.Prctl(unix.PR_SET_SECCOMP,
			uintptr(unix.SECCOMP_MODE_FILTER),
			uintptr(unsafe.Pointer(&fprog)), 0, 0); err != nil {
			return fmt.Errorf("seccomp: prctl PR_SET_SECCOMP: %w", err)
		}
		return nil
	}
	// Process-wide installation via seccomp(2) with TSYNC flag.
	_, _, errno := unix.Syscall(syNrSeccomp,
		seccompSetModeFilter,
		flags,
		uintptr(unsafe.Pointer(&fprog)))
	if errno != 0 {
		return fmt.Errorf("seccomp: seccomp(TSYNC): %w", errno)
	}
	return nil
}
```

### Exercise 5: Tests and Example Functions

Create `seccomp_test.go`:

```go
package seccomp

import (
	"errors"
	"fmt"
	"testing"
)

// ExampleCompile_noRules shows the degenerate case: a policy with no rules
// compiles to a single return instruction.
func ExampleCompile_noRules() {
	p := &Policy{Default: ActionAllow}
	prog, _ := Compile(p)
	fmt.Println(len(prog))
	// Output:
	// 1
}

// ExampleCompile shows a policy with one rule. The program consists of the
// arch guard (3), the nr load (1), the rule JEQ (1), the rule body (1), and
// the default verdict (1) — 7 instructions total.
func ExampleCompile() {
	p := &Policy{
		Default: ActionAllow,
		Rules: []Rule{
			{SyscallNr: 87, Action: ActionKill}, // 87 = SYS_unlink on amd64
		},
	}
	prog, _ := Compile(p)
	fmt.Println(len(prog))
	// Output:
	// 7
}

func TestCompileEmptyPolicy(t *testing.T) {
	t.Parallel()

	p := &Policy{Default: ActionAllow}
	prog, err := Compile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(prog) != 1 {
		t.Fatalf("empty policy: got %d instructions, want 1", len(prog))
	}
	// The single instruction must return the default action.
	if prog[0].K != uint32(ActionAllow) {
		t.Fatalf("wrong verdict: got %#x, want %#x", prog[0].K, uint32(ActionAllow))
	}
}

func TestCompileArchGuard(t *testing.T) {
	t.Parallel()

	p := &Policy{
		Default: ActionAllow,
		Rules:   []Rule{{SyscallNr: 1, Action: ActionKill}},
	}
	prog, err := Compile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(prog) < 3 {
		t.Fatalf("program too short: %d instructions", len(prog))
	}
	// prog[0]: load arch.
	if prog[0].K != offArch {
		t.Fatalf("prog[0].K = %d, want offArch (%d)", prog[0].K, offArch)
	}
	// prog[1]: compare against archAMD64.
	if prog[1].K != archAMD64 {
		t.Fatalf("prog[1].K = %#x, want archAMD64 (%#x)", prog[1].K, archAMD64)
	}
	// prog[2]: kill on arch mismatch.
	if prog[2].K != uint32(ActionKill) {
		t.Fatalf("prog[2].K = %#x, want ActionKill (%#x)", prog[2].K, uint32(ActionKill))
	}
}

func TestCompileSingleRule(t *testing.T) {
	t.Parallel()

	p := &Policy{
		Default: ActionAllow,
		Rules:   []Rule{{SyscallNr: 87, Action: ActionKill}},
	}
	prog, err := Compile(p)
	if err != nil {
		t.Fatal(err)
	}
	// arch guard(3) + load_nr(1) + rule_jeq(1) + rule_body(1) + default(1) = 7
	const want = 7
	if len(prog) != want {
		t.Fatalf("got %d instructions, want %d", len(prog), want)
	}
}

func TestCompileMultipleRules(t *testing.T) {
	t.Parallel()

	p := &Policy{
		Default: ActionAllow,
		Rules: []Rule{
			{SyscallNr: 87, Action: ActionKill},  // unlink
			{SyscallNr: 263, Action: ActionKill}, // unlinkat
		},
	}
	prog, err := Compile(p)
	if err != nil {
		t.Fatal(err)
	}
	// arch guard(3) + load_nr(1)
	// rule0: jeq(1)+body(1)+reload(1) = 3
	// rule1: jeq(1)+body(1) = 2           (no reload after last rule)
	// default(1)
	// total: 3+1+3+2+1 = 10
	const want = 10
	if len(prog) != want {
		t.Fatalf("got %d instructions, want %d", len(prog), want)
	}
}

func TestCompileArgFilterEQ(t *testing.T) {
	t.Parallel()

	// Allow openat only when flags arg (index 1) is 0 (O_RDONLY).
	// Any other flag value returns the default action.
	p := &Policy{
		Default: ActionErrno(1), // EPERM
		Rules: []Rule{{
			SyscallNr:  257, // SYS_openat on amd64
			Action:     ActionAllow,
			ArgFilters: []ArgFilter{{Index: 1, Op: ArgOpEQ, Value: 0}},
		}},
	}
	prog, err := Compile(p)
	if err != nil {
		t.Fatal(err)
	}
	// arch guard(3) + load_nr(1) + rule_jeq(1)
	// body for ArgOpEQ(64-bit): 6 filter insns + 1 ret(rule action) = 7
	// default(1)
	// total: 3+1+1+7+1 = 13
	const want = 13
	if len(prog) != want {
		t.Fatalf("got %d instructions, want %d", len(prog), want)
	}
}

func TestCompileArgFilterAnd(t *testing.T) {
	t.Parallel()

	// Kill mmap if PROT_EXEC (0x04) is requested.
	const PROT_EXEC = 0x04
	p := &Policy{
		Default: ActionAllow,
		Rules: []Rule{{
			SyscallNr:  9, // SYS_mmap on amd64
			Action:     ActionKill,
			ArgFilters: []ArgFilter{{Index: 2, Op: ArgOpAnd, Value: PROT_EXEC}},
		}},
	}
	prog, err := Compile(p)
	if err != nil {
		t.Fatal(err)
	}
	// arch guard(3) + load_nr(1) + rule_jeq(1)
	// body for ArgOpAnd: 3 filter insns + 1 ret = 4
	// default(1)
	// total: 3+1+1+4+1 = 10
	const want = 10
	if len(prog) != want {
		t.Fatalf("got %d instructions, want %d", len(prog), want)
	}
}

func TestCompileActionErrno(t *testing.T) {
	t.Parallel()

	action := ActionErrno(1) // EPERM
	const wantK = uint32(0x00050001)
	if uint32(action) != wantK {
		t.Fatalf("ActionErrno(1) = %#x, want %#x", uint32(action), wantK)
	}
}

func TestCompileTooManyRules(t *testing.T) {
	t.Parallel()

	// 2000 rules * ~3 instructions each exceeds the 4096-instruction limit.
	rules := make([]Rule, 2000)
	for i := range rules {
		rules[i] = Rule{SyscallNr: uint32(i), Action: ActionKill}
	}
	p := &Policy{Default: ActionAllow, Rules: rules}
	_, err := Compile(p)
	if !errors.Is(err, ErrTooManyRules) {
		t.Fatalf("got %v, want ErrTooManyRules", err)
	}
}

// Your turn: add TestCompileDefaultKill that builds a Policy with Default:ActionKill
// and no rules, compiles it, and asserts that the single instruction returns
// uint32(ActionKill). Then add a Rule that allows SYS_exit_group (231) and verify
// the program has 7 instructions.
```

### Exercise 6: CLI Demo (Linux + amd64)

Create `cmd/demo/main.go`:

```go
//go:build linux && amd64

package main

import (
	"fmt"
	"os"
	"runtime"

	"example.com/seccomp"
)

func main() {
	// Policy: allow everything except unlink (87) and unlinkat (263).
	// Attempting either syscall will receive EPERM.
	p := &seccomp.Policy{
		Default: seccomp.ActionAllow,
		Rules: []seccomp.Rule{
			{SyscallNr: 87, Action: seccomp.ActionErrno(1)},  // unlink  -> EPERM
			{SyscallNr: 263, Action: seccomp.ActionErrno(1)}, // unlinkat -> EPERM
		},
	}

	prog, err := seccomp.Compile(p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "compile: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("compiled policy: %d BPF instructions\n", len(prog))

	// Pin this goroutine to its OS thread and install the filter on that thread only.
	// For whole-process sandboxing use seccomp.InstallProcess(prog) instead.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := seccomp.Install(prog); err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("filter installed on current OS thread")

	// Attempt to remove a non-existent file; under the filter this must fail with EPERM.
	err = os.Remove("/tmp/seccomp-test-file-that-does-not-exist")
	if err != nil {
		fmt.Printf("os.Remove result (expected EPERM): %v\n", err)
	} else {
		fmt.Println("FAIL: os.Remove succeeded; filter did not apply")
		os.Exit(1)
	}

	fmt.Println("OK: unlink blocked by seccomp filter")
}
```

Run the demo on a Linux/amd64 host:

```bash
go run ./cmd/demo
```

Expected output (the exact errno text varies by libc):

```
compiled policy: 10 BPF instructions
filter installed on current OS thread
os.Remove result (expected EPERM): remove /tmp/seccomp-test-file-that-does-not-exist: operation not permitted
OK: unlink blocked by seccomp filter
```

## Common Mistakes

### Wrong Jump Offset Direction

Wrong: emitting `instrJEQ(nr, 1, 0)` intending "jump over the rule body on mismatch". The `jt` field is the true branch (match) and `jf` is the false branch (mismatch). Swapping them lets mismatching syscalls fall into the wrong rule body.

Fix: `instrJEQ(nr, 0, uint8(len(body)))` — on match (`jt=0`) fall through into the body; on mismatch (`jf=len(body)`) jump over the body to the next rule.

### Installing Without PR_SET_NO_NEW_PRIVS

Wrong: calling `prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, &prog)` without first setting `PR_SET_NO_NEW_PRIVS`. The kernel returns `EACCES` unless the process holds `CAP_SYS_ADMIN`.

Fix: always call `prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)` immediately before the seccomp prctl. This is idempotent — calling it multiple times is safe.

### Per-Thread Filter Leaving Goroutines Unprotected

Wrong: calling `Install(prog)` from a goroutine without `runtime.LockOSThread()`. Go may schedule the goroutine onto a different OS thread between the LockOSThread and Install calls, or another goroutine may run on the filtered thread and bypass the filter.

Fix: use `InstallProcess` with `SECCOMP_FILTER_FLAG_TSYNC` to apply the filter to all OS threads atomically, or call `runtime.LockOSThread()` immediately before `Install` in the same goroutine.

### Forgetting the Architecture Guard

Wrong: emitting rules that compare `seccomp_data.nr` without first checking `seccomp_data.arch`. A process running under the 32-bit personality (`ARCH_I386`) presents different syscall numbers; without the arch guard, a 32-bit `read` (syscall 3) could collide with a completely different 64-bit syscall.

Fix: always open the BPF program with the three-instruction arch check shown in `compiler.go`. Kill the process on arch mismatch rather than defaulting to allow.

### Overly Restrictive Default Killing the Go Runtime

Wrong: setting `Default: ActionKill` and adding only a few allow rules. The Go runtime uses `futex`, `sigaltstack`, `mmap`, `munmap`, `clone`, `epoll_*`, `read`, `write`, and many more. An incomplete allowlist silently kills the process mid-execution.

Fix: start with `Default: ActionAllow` and deny specific syscalls. Move to a restrictive default only after profiling the exact syscalls the process needs (use `strace -e trace=all ./program` to enumerate them).

## Verification

The pure-Go parts (policy, BPF assembler, compiler) are testable without Linux or external packages:

```bash
cd ~/go-exercises/seccomp
test -z "$(gofmt -l .)"
go vet ./...
go test -count=1 -race ./...
```

The installation and demo require a Linux/amd64 host with kernel 3.17+:

```bash
go build ./...
go run ./cmd/demo
```

`go test -count=1 -race ./...` is the primary verification — the Example functions are auto-verified by the test runner. Add at least one test of your own (see the "your turn" note in `seccomp_test.go`).

## Summary

- Classic BPF programs for seccomp operate on a `seccomp_data` struct via a 32-bit accumulator; 64-bit arguments require two sequential loads.
- The arch guard (check `seccomp_data.arch` before `seccomp_data.nr`) prevents syscall-number confusion under personality changes.
- Each rule block is: `JEQ(nr, jt=0, jf=len(body))` followed by body instructions that end with `RET(action)`. Jump offsets are computable forward without fixups when each body is built before the header is emitted.
- Go goroutines migrate between OS threads; `SECCOMP_FILTER_FLAG_TSYNC` via the `seccomp(2)` syscall is the correct way to sandbox a Go process.
- Start with a permissive default and deny specific syscalls; an overly restrictive default that blocks Go runtime syscalls kills the process without diagnostic output.

## What's Next

Next: [ptrace Syscall Tracer](../07-ptrace-syscall-tracer/07-ptrace-syscall-tracer.md).

## Resources

- [Linux kernel seccomp documentation](https://www.kernel.org/doc/html/latest/userspace-api/seccomp_filter.html) — authoritative reference for seccomp_data layout, return value encoding, and TSYNC semantics.
- [linux/filter.h BPF constants](https://elixir.bootlin.com/linux/latest/source/include/uapi/linux/filter.h) — canonical source for BPF_LD, BPF_JMP, BPF_RET, and all modifier values.
- [pkg.go.dev: golang.org/x/sys/unix SockFilter](https://pkg.go.dev/golang.org/x/sys/unix#SockFilter) — Go type definitions for SockFilter and SockFprog; Prctl signature.
- [Docker default seccomp profile](https://docs.docker.com/engine/security/seccomp/) — production example of an allowlist policy with 300+ rules; shows the practical set of syscalls a containerized process needs.
- [man 2 seccomp](https://man7.org/linux/man-pages/man2/seccomp.2.html) — SECCOMP_FILTER_FLAG_TSYNC, SECCOMP_SET_MODE_FILTER, and the complete return-value precedence table.
