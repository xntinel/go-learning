# 12. Go-to-WebAssembly Compiler

This capstone builds a compiler that accepts a restricted subset of Go — integer and float arithmetic, variables, if/else, and for loops — and emits a valid WebAssembly binary module. You drive Go's own compiler frontend (`go/parser`, `go/types`, `go/ast`) to parse and type-check the input, lower the typed AST to a flat IR, and drive a Wasm binary encoder that writes LEB128-encoded sections by hand. The resulting `.wasm` file runs inside `wazero`, a pure-Go Wasm runtime, making the full pipeline testable without leaving Go. The lesson is intentionally ambitious: it crosses every phase of compiler construction and forces you to confront what code generation means on a stack machine rather than a register machine.

The compiler backend cannot run offline because it requires `github.com/tetratelabs/wazero` for execution; the core pipeline (frontend, IR, binary encoder) uses only the standard library and is fully verifiable with `gofmt` and `go vet`. All integration tests that execute `.wasm` require a network `go get`.

```
gowasm/
  go.mod
  compiler/
    leb128.go        LEB128 encoder/decoder — pure stdlib
    ir.go            IR type definitions — pure stdlib
    wasm.go          Wasm binary module builder — pure stdlib
    frontend.go      Go parser + type-checker — stdlib (go/parser, go/types)
    codegen.go       Lowerer + code generator — stdlib
    leb128_test.go   Unit tests + Example functions — pure stdlib, runnable offline
    module_test.go   Module + frontend + pipeline tests — pure stdlib, runnable offline
    compiler_test.go Integration tests via wazero — requires network
  cmd/demo/
    main.go          Runnable demo (no wazero required)
```

## Concepts

### The Go Compiler Frontend in the Standard Library

`go/parser`, `go/ast`, `go/token`, and `go/types` form a complete, production-quality compiler frontend shipped with every Go installation. `parser.ParseFile` returns an `*ast.File` (an untyped AST). `types.Config.Check` type-checks that AST, fills a `types.Info` map that associates every `ast.Expr` node with its inferred type and every `ast.Ident` with the object it names, and returns a `*types.Package`. The type-checker requires a `types.Importer` to resolve imported packages; for a subset that imports only the standard library, `importer.Default()` from `go/importer` is sufficient.

```
source text
  parser.ParseFile  →  *ast.File (untyped AST)
    types.Config.Check  →  types.Info (expr types, identifier objects)
```

The type-checker must run before you walk the AST. Accessing `info.Types[expr]` before `Check` returns always produces the zero `TypeAndValue`, so every type lookup silently yields `TypeVoid` and every function appears to have an unsupported signature.

### WebAssembly Binary Format and Section Layout

A Wasm module is a sequence of sections identified by a one-byte id, each prefixed with a LEB128-encoded byte count. The sections the compiler needs, in required id order:

| id | Name    | Contents |
|----|---------|----------|
| 1  | Type    | Function signatures (param and result types) |
| 3  | Function| Maps function index to type index |
| 5  | Memory  | Linear memory declarations (min/max pages) |
| 7  | Export  | Name-to-index bindings for functions and memories |
| 10 | Code    | Function bodies: local decls + instruction bytes |
| 11 | Data    | Active data segments (bytes written to linear memory at load time) |

The module header is eight bytes: the magic `\0asm` (0x00 0x61 0x73 0x6D) and the version word (0x01 0x00 0x00 0x00). Sections must appear in strictly ascending id order; a validator rejects any other ordering.

### The Wasm Stack Machine

Wasm is a stack machine: instructions consume operands from an implicit value stack and push results back. There are no general-purpose registers. `i32.add` pops two i32 values and pushes their sum; `local.get 0` pushes the value of local 0; `local.set 1` pops the top of stack into local 1.

Control flow is *structured* — no arbitrary jumps. A `block` can be broken out of with `br 0` (jumps past the matching `end`). A `loop` can be restarted with `br 0` (jumps to the matching `loop` header). A Go `for cond { body }` becomes:

```
block $exit
  loop $header
    <cond>
    i32.eqz         ; invert: exit when condition is false
    br_if 1         ; br_if $exit
    <body>
    br 0            ; br $header (restart)
  end $header
end $exit
```

The label depth counts enclosing structured blocks outward. Inside the loop body, depth 0 names the `loop` (restart) and depth 1 names the surrounding `block` (exit).

### LEB128 Variable-Length Integer Encoding

LEB128 (Little-Endian Base-128) encodes integers in one or more bytes. Each byte carries 7 bits of payload; the high bit signals that more bytes follow. Wasm uses unsigned LEB128 (ULEB128) for sizes, counts, and indices, and signed LEB128 (SLEB128) for integer constants.

Unsigned LEB128 for 300 (0x12C):

```
300 = binary 1_0101100
  byte 0: low 7 bits = 0101100 (44), more follows → set high bit → 10101100 = 0xAC
  byte 1: remaining  = 10 (2), no more             → 00000010 = 0x02
result: [0xAC, 0x02]
```

SLEB128 uses two's complement: encoding terminates when the remaining value equals 0 (non-negative) or -1 (negative) AND the sign bit of the last byte matches the value's sign. SLEB128(-1) = [0x7F] because -1 has all bits set, the low 7 bits are 0x7F (sign bit 1), and the remaining shifted value is already -1.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/gowasm/compiler
mkdir -p ~/go-exercises/gowasm/cmd/demo
cd ~/go-exercises/gowasm
go mod init example.com/gowasm
```

Add the wazero dependency for the integration test harness:

```bash
go get github.com/tetratelabs/wazero@v1.8.0
```

The compiler package is a library. You verify it with `go test`, not with `go run`.

### Exercise 1: LEB128 Encoder and IR Types

Create `compiler/leb128.go`:

```go
package compiler

// AppendULEB128 encodes v as unsigned LEB128 and appends it to buf.
func AppendULEB128(buf []byte, v uint64) []byte {
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		buf = append(buf, b)
		if v == 0 {
			break
		}
	}
	return buf
}

// AppendSLEB128 encodes v as signed LEB128 and appends it to buf.
func AppendSLEB128(buf []byte, v int64) []byte {
	more := true
	for more {
		b := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			more = false
		} else {
			b |= 0x80
		}
		buf = append(buf, b)
	}
	return buf
}

// DecodeULEB128 decodes the next unsigned LEB128 value from b,
// returning the value and the number of bytes consumed.
func DecodeULEB128(b []byte) (uint64, int) {
	var result uint64
	var shift uint
	for i, byt := range b {
		result |= uint64(byt&0x7f) << shift
		shift += 7
		if byt&0x80 == 0 {
			return result, i + 1
		}
	}
	return result, len(b)
}
```

Create `compiler/ir.go`. The IR uses a flat instruction stream with structured control-flow opcodes that mirror Wasm's own `block`/`loop`/`if`/`end` model. This makes the lowering phase straightforward: each Go control construct translates to a fixed pattern of IR opcodes that the code generator then maps one-to-one to Wasm bytes.

```go
package compiler

import "fmt"

// Type represents the Wasm value types the compiler targets.
type Type int

const (
	TypeVoid Type = iota
	TypeI32
	TypeI64
	TypeF64
)

// String returns the Wasm text format name of t.
func (t Type) String() string {
	switch t {
	case TypeVoid:
		return "void"
	case TypeI32:
		return "i32"
	case TypeI64:
		return "i64"
	case TypeF64:
		return "f64"
	default:
		return fmt.Sprintf("Type(%d)", int(t))
	}
}

// WasmByte returns the Wasm binary format encoding of t.
// Panics for TypeVoid because the void type uses blocktype 0x40, not a value type.
func (t Type) WasmByte() byte {
	switch t {
	case TypeI32:
		return 0x7f
	case TypeI64:
		return 0x7e
	case TypeF64:
		return 0x7c
	default:
		panic(fmt.Sprintf("compiler: no wasm value type byte for %v", t))
	}
}

// Op is an IR opcode. Each opcode maps directly to one or more Wasm instructions.
type Op int

const (
	// Constant push
	OpConst  Op = iota // i32.const IVal
	OpFConst           // f64.const FVal

	// Local variable access
	OpLoad  // local.get Name
	OpStore // local.set Name

	// Integer arithmetic (i32)
	OpAdd  // i32.add
	OpSub  // i32.sub
	OpMul  // i32.mul
	OpDivS // i32.div_s

	// Integer comparison (produce i32: 0 or 1)
	OpLtS // i32.lt_s
	OpLeS // i32.le_s
	OpGtS // i32.gt_s
	OpGeS // i32.ge_s
	OpEqI // i32.eq
	OpNeI // i32.ne
	OpEqz // i32.eqz (logical NOT for booleans)

	// Structured control flow — mirrors Wasm's block/loop/if structure
	OpBlock // block (void) — enter a breakable block
	OpLoop  // loop (void) — enter a loop; br 0 jumps to loop start
	OpIf    // if (void)  — pop i32; enter then-branch if non-zero
	OpElse  // else       — switch to else-branch
	OpEnd   // end        — close block/loop/if

	// Branches (label depth counts enclosing blocks/loops outward from 0)
	OpBr   // br   IVal  — unconditional branch to label depth IVal
	OpBrIf // br_if IVal — conditional branch; pops i32 condition

	// Miscellaneous
	OpReturn // return
	OpCall   // call Name — call exported function by name
	OpDrop   // drop      — discard top-of-stack value
)

// Instr is a single IR instruction.
type Instr struct {
	Op   Op
	IVal int64   // OpConst: the i32/i64 constant; OpBr/OpBrIf: label depth
	FVal float64 // OpFConst: the f64 constant
	Name string  // OpLoad/OpStore/OpCall: variable or function name
	Type Type    // type annotation for OpConst/OpFConst
}

// Param is a named, typed function parameter.
type Param struct {
	Name string
	Type Type
}

// Func is an IR function — a flat sequence of instructions that may include
// structured control-flow opcodes (OpBlock, OpLoop, OpIf, OpElse, OpEnd).
type Func struct {
	Name   string
	Params []Param
	Result Type
	Instrs []Instr
	Locals map[string]Type // name -> type (params plus declared locals)
}

// NewFunc allocates a Func and pre-populates Locals with the parameter types.
func NewFunc(name string, params []Param, result Type) *Func {
	locals := make(map[string]Type, len(params))
	for _, p := range params {
		locals[p.Name] = p.Type
	}
	return &Func{
		Name:   name,
		Params: params,
		Result: result,
		Locals: locals,
	}
}

// Emit appends ins to the function's instruction stream.
func (f *Func) Emit(ins Instr) {
	f.Instrs = append(f.Instrs, ins)
}

// DeclareLocal registers a new local variable. Returns false if already declared.
func (f *Func) DeclareLocal(name string, t Type) bool {
	if _, ok := f.Locals[name]; ok {
		return false
	}
	f.Locals[name] = t
	return true
}
```

### Exercise 2: WebAssembly Binary Module Builder

The module builder manages the six sections and serializes them to the Wasm binary format. It deduplicates function type signatures and adds the mandatory trailing `end` opcode to every function body.

Create `compiler/wasm.go`:

```go
package compiler

import (
	"encoding/binary"
	"math"
)

// wasmMagic is the 8-byte Wasm module header: magic bytes + version word.
var wasmMagic = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

// Module accumulates Wasm sections and serializes them to binary format.
// Sections are added in spec order (type=1, func=3, memory=5, export=7, code=10, data=11).
type Module struct {
	types   []funcType
	funcs   []uint32 // type index per function
	exports []exportEntry
	codes   []funcBody
	mems    []memType
	data    []dataSegment
}

type funcType struct {
	params  []Type
	results []Type
}

type exportEntry struct {
	name string
	kind byte // 0 = function, 2 = memory
	idx  uint32
}

type funcBody struct {
	locals []LocalDecl
	code   []byte // raw Wasm instruction bytes (without the trailing end opcode)
}

// LocalDecl groups a run of locals with the same type (Wasm local declaration format).
type LocalDecl struct {
	Count uint32
	Type  Type
}

type memType struct {
	min uint32
}

type dataSegment struct {
	offset uint32
	bytes  []byte
}

// AddFuncType registers a function signature and returns its type index.
// Identical signatures are deduplicated.
func (m *Module) AddFuncType(params, results []Type) uint32 {
	for i, ft := range m.types {
		if typesEqual(ft.params, params) && typesEqual(ft.results, results) {
			return uint32(i)
		}
	}
	idx := uint32(len(m.types))
	m.types = append(m.types, funcType{
		params:  append([]Type(nil), params...),
		results: append([]Type(nil), results...),
	})
	return idx
}

func typesEqual(a, b []Type) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// AddFunc registers a function body under the given type index.
// Returns the function index. locals describes additional (non-parameter) locals.
func (m *Module) AddFunc(typeIdx uint32, locals []LocalDecl, code []byte) uint32 {
	idx := uint32(len(m.funcs))
	m.funcs = append(m.funcs, typeIdx)
	m.codes = append(m.codes, funcBody{locals: locals, code: code})
	return idx
}

// ExportFunc creates a named export for a function.
func (m *Module) ExportFunc(name string, funcIdx uint32) {
	m.exports = append(m.exports, exportEntry{name: name, kind: 0, idx: funcIdx})
}

// AddMemory declares a linear memory with min pages (1 page = 65536 bytes).
// Returns the memory index (always 0 in MVP Wasm).
func (m *Module) AddMemory(minPages uint32) uint32 {
	idx := uint32(len(m.mems))
	m.mems = append(m.mems, memType{min: minPages})
	return idx
}

// ExportMemory creates a named export for a memory.
func (m *Module) ExportMemory(name string, memIdx uint32) {
	m.exports = append(m.exports, exportEntry{name: name, kind: 2, idx: memIdx})
}

// AddDataSegment writes bytes into linear memory at offset (active segment, memory 0).
func (m *Module) AddDataSegment(offset uint32, data []byte) {
	m.data = append(m.data, dataSegment{offset: offset, bytes: append([]byte(nil), data...)})
}

// Bytes serializes the module to Wasm binary format.
func (m *Module) Bytes() []byte {
	var buf []byte
	buf = append(buf, wasmMagic...)
	if len(m.types) > 0 {
		buf = appendSection(buf, 1, m.encodeTypeSection())
	}
	if len(m.funcs) > 0 {
		buf = appendSection(buf, 3, m.encodeFuncSection())
	}
	if len(m.mems) > 0 {
		buf = appendSection(buf, 5, m.encodeMemorySection())
	}
	if len(m.exports) > 0 {
		buf = appendSection(buf, 7, m.encodeExportSection())
	}
	if len(m.codes) > 0 {
		buf = appendSection(buf, 10, m.encodeCodeSection())
	}
	if len(m.data) > 0 {
		buf = appendSection(buf, 11, m.encodeDataSection())
	}
	return buf
}

func appendSection(buf []byte, id byte, content []byte) []byte {
	buf = append(buf, id)
	buf = AppendULEB128(buf, uint64(len(content)))
	return append(buf, content...)
}

func (m *Module) encodeTypeSection() []byte {
	var b []byte
	b = AppendULEB128(b, uint64(len(m.types)))
	for _, ft := range m.types {
		b = append(b, 0x60) // func type marker
		b = AppendULEB128(b, uint64(len(ft.params)))
		for _, p := range ft.params {
			b = append(b, p.WasmByte())
		}
		b = AppendULEB128(b, uint64(len(ft.results)))
		for _, r := range ft.results {
			b = append(b, r.WasmByte())
		}
	}
	return b
}

func (m *Module) encodeFuncSection() []byte {
	var b []byte
	b = AppendULEB128(b, uint64(len(m.funcs)))
	for _, ti := range m.funcs {
		b = AppendULEB128(b, uint64(ti))
	}
	return b
}

func (m *Module) encodeMemorySection() []byte {
	var b []byte
	b = AppendULEB128(b, uint64(len(m.mems)))
	for _, mem := range m.mems {
		b = append(b, 0x00) // no maximum (limit kind 0)
		b = AppendULEB128(b, uint64(mem.min))
	}
	return b
}

func (m *Module) encodeExportSection() []byte {
	var b []byte
	b = AppendULEB128(b, uint64(len(m.exports)))
	for _, e := range m.exports {
		name := []byte(e.name)
		b = AppendULEB128(b, uint64(len(name)))
		b = append(b, name...)
		b = append(b, e.kind)
		b = AppendULEB128(b, uint64(e.idx))
	}
	return b
}

func (m *Module) encodeCodeSection() []byte {
	var b []byte
	b = AppendULEB128(b, uint64(len(m.codes)))
	for _, body := range m.codes {
		var bodyBuf []byte
		bodyBuf = AppendULEB128(bodyBuf, uint64(len(body.locals)))
		for _, ld := range body.locals {
			bodyBuf = AppendULEB128(bodyBuf, uint64(ld.Count))
			bodyBuf = append(bodyBuf, ld.Type.WasmByte())
		}
		bodyBuf = append(bodyBuf, body.code...)
		bodyBuf = append(bodyBuf, 0x0b) // end — required closing opcode for function body
		b = AppendULEB128(b, uint64(len(bodyBuf)))
		b = append(b, bodyBuf...)
	}
	return b
}

func (m *Module) encodeDataSection() []byte {
	var b []byte
	b = AppendULEB128(b, uint64(len(m.data)))
	for _, seg := range m.data {
		b = append(b, 0x00) // segment flags: active, memory index 0
		b = append(b, 0x41) // i32.const (offset expression)
		b = AppendSLEB128(b, int64(seg.offset))
		b = append(b, 0x0b) // end (offset expression)
		b = AppendULEB128(b, uint64(len(seg.bytes)))
		b = append(b, seg.bytes...)
	}
	return b
}

// EncodeI32Const returns the Wasm instruction bytes for i32.const v.
func EncodeI32Const(v int32) []byte {
	b := []byte{0x41} // i32.const opcode
	return AppendSLEB128(b, int64(v))
}

// EncodeI64Const returns the Wasm instruction bytes for i64.const v.
func EncodeI64Const(v int64) []byte {
	b := []byte{0x42} // i64.const opcode
	return AppendSLEB128(b, v)
}

// EncodeF64Const returns the Wasm instruction bytes for f64.const v.
func EncodeF64Const(v float64) []byte {
	b := []byte{0x44} // f64.const opcode
	bits := math.Float64bits(v)
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], bits)
	return append(b, tmp[:]...)
}

// LocalGet returns the Wasm bytes for local.get i.
func LocalGet(i uint32) []byte {
	return AppendULEB128([]byte{0x20}, uint64(i))
}

// LocalSet returns the Wasm bytes for local.set i.
func LocalSet(i uint32) []byte {
	return AppendULEB128([]byte{0x21}, uint64(i))
}
```

### Exercise 3: Frontend, Code Generator, and Test Harness

Create `compiler/frontend.go`. `ParseAndCheck` is the only entry point to Go's type-checker; every other pass consults its output through the `types.Info` maps.

```go
package compiler

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
)

// Source holds the parsed and type-checked result of a Go source file.
type Source struct {
	Fset *token.FileSet
	File *ast.File
	Info *types.Info
	Pkg  *types.Package
}

// ParseAndCheck parses src as Go source text and type-checks it using
// the standard library importer. Only single-file inputs without external
// module imports are supported; any use of a non-stdlib import will fail.
func ParseAndCheck(filename, src string) (*Source, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.AllErrors)
	if err != nil {
		return nil, fmt.Errorf("compiler: parse %s: %w", filename, err)
	}

	info := &types.Info{
		Types: make(map[ast.Expr]types.TypeAndValue),
		Defs:  make(map[*ast.Ident]types.Object),
		Uses:  make(map[*ast.Ident]types.Object),
	}
	cfg := &types.Config{
		Importer: importer.Default(),
	}
	pkg, err := cfg.Check(filename, fset, []*ast.File{f}, info)
	if err != nil {
		return nil, fmt.Errorf("compiler: typecheck %s: %w", filename, err)
	}
	return &Source{Fset: fset, File: f, Info: info, Pkg: pkg}, nil
}

// GoTypeToIR maps a go/types.Type to an IR Type.
// Returns TypeVoid for unsupported or composite types.
func GoTypeToIR(t types.Type) Type {
	b, ok := t.Underlying().(*types.Basic)
	if !ok {
		return TypeVoid
	}
	switch b.Kind() {
	case types.Int, types.Int32, types.UntypedInt:
		return TypeI32
	case types.Int64:
		return TypeI64
	case types.Float64, types.UntypedFloat:
		return TypeF64
	case types.Bool, types.UntypedBool:
		return TypeI32 // bool is i32 (0 = false, 1 = true)
	}
	return TypeVoid
}
```

Create `compiler/codegen.go`. The lowerer walks the typed AST and emits a flat IR stream; the code generator translates that stream to Wasm bytes. The two stages are separate so that optimization passes (constant folding, dead code elimination) can operate on the IR before code generation.

```go
package compiler

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
)

// Compile translates the functions in src to a Wasm binary module.
// Functions with unsupported parameter or result types are skipped silently.
// All compiled functions are exported by their Go name.
func Compile(src *Source) ([]byte, error) {
	mod := &Module{}
	memIdx := mod.AddMemory(1) // one 64 KiB page
	mod.ExportMemory("memory", memIdx)

	for _, decl := range src.File.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		fn, err := lowerFunc(src, fd)
		if err != nil {
			return nil, fmt.Errorf("compiler: %s: %w", fd.Name.Name, err)
		}
		if fn == nil {
			continue // unsupported signature; skip
		}

		paramTypes := make([]Type, len(fn.Params))
		for i, p := range fn.Params {
			paramTypes[i] = p.Type
		}

		var resultTypes []Type
		if fn.Result != TypeVoid {
			resultTypes = []Type{fn.Result}
		}

		// Collect non-param locals as Wasm local declarations.
		paramSet := make(map[string]bool, len(fn.Params))
		for _, p := range fn.Params {
			paramSet[p.Name] = true
		}
		var extraNames []string
		for name := range fn.Locals {
			if !paramSet[name] {
				extraNames = append(extraNames, name)
			}
		}
		sort.Strings(extraNames) // stable ordering for deterministic output
		locals := make([]LocalDecl, 0, len(extraNames))
		for _, name := range extraNames {
			locals = append(locals, LocalDecl{Count: 1, Type: fn.Locals[name]})
		}

		localIdx := buildLocalIdx(fn)
		code := emitInstrs(fn.Instrs, localIdx)

		typeIdx := mod.AddFuncType(paramTypes, resultTypes)
		fidx := mod.AddFunc(typeIdx, locals, code)
		mod.ExportFunc(fn.Name, fidx)
	}

	return mod.Bytes(), nil
}

// buildLocalIdx assigns a Wasm local index to every parameter and local variable.
// Parameters come first (in declaration order), then remaining locals alphabetically.
func buildLocalIdx(fn *Func) map[string]uint32 {
	idx := make(map[string]uint32, len(fn.Locals))
	next := uint32(0)
	for _, p := range fn.Params {
		idx[p.Name] = next
		next++
	}
	var extras []string
	for name := range fn.Locals {
		if _, isParam := idx[name]; !isParam {
			extras = append(extras, name)
		}
	}
	sort.Strings(extras)
	for _, name := range extras {
		idx[name] = next
		next++
	}
	return idx
}

// emitInstrs translates the IR instruction slice to Wasm bytes.
func emitInstrs(instrs []Instr, localIdx map[string]uint32) []byte {
	var code []byte
	for _, ins := range instrs {
		code = emitInstr(code, ins, localIdx)
	}
	return code
}

func emitInstr(code []byte, ins Instr, localIdx map[string]uint32) []byte {
	switch ins.Op {
	case OpConst:
		code = append(code, 0x41) // i32.const
		code = AppendSLEB128(code, ins.IVal)
	case OpFConst:
		code = append(code, EncodeF64Const(ins.FVal)...)
	case OpLoad:
		code = append(code, LocalGet(localIdx[ins.Name])...)
	case OpStore:
		code = append(code, LocalSet(localIdx[ins.Name])...)
	case OpAdd:
		code = append(code, 0x6a) // i32.add
	case OpSub:
		code = append(code, 0x6b) // i32.sub
	case OpMul:
		code = append(code, 0x6c) // i32.mul
	case OpDivS:
		code = append(code, 0x6d) // i32.div_s
	case OpLtS:
		code = append(code, 0x48) // i32.lt_s
	case OpLeS:
		code = append(code, 0x4c) // i32.le_s
	case OpGtS:
		code = append(code, 0x4a) // i32.gt_s
	case OpGeS:
		code = append(code, 0x4e) // i32.ge_s
	case OpEqI:
		code = append(code, 0x46) // i32.eq
	case OpNeI:
		code = append(code, 0x47) // i32.ne
	case OpEqz:
		code = append(code, 0x45) // i32.eqz
	case OpBlock:
		code = append(code, 0x02, 0x40) // block (void)
	case OpLoop:
		code = append(code, 0x03, 0x40) // loop (void)
	case OpIf:
		code = append(code, 0x04, 0x40) // if (void)
	case OpElse:
		code = append(code, 0x05) // else
	case OpEnd:
		code = append(code, 0x0b) // end
	case OpBr:
		code = append(code, 0x0c) // br
		code = AppendULEB128(code, uint64(ins.IVal))
	case OpBrIf:
		code = append(code, 0x0d) // br_if
		code = AppendULEB128(code, uint64(ins.IVal))
	case OpReturn:
		code = append(code, 0x0f) // return
	case OpDrop:
		code = append(code, 0x1a) // drop
	}
	return code
}

// lowerFunc lowers a single Go function declaration to an IR Func.
// Returns nil without error if the signature uses unsupported types.
func lowerFunc(src *Source, fd *ast.FuncDecl) (*Func, error) {
	var params []Param
	if fd.Type.Params != nil {
		for _, field := range fd.Type.Params.List {
			tv := src.Info.Types[field.Type]
			irt := GoTypeToIR(tv.Type)
			if irt == TypeVoid {
				return nil, nil // unsupported param type
			}
			for _, name := range field.Names {
				params = append(params, Param{Name: name.Name, Type: irt})
			}
		}
	}

	result := TypeVoid
	if fd.Type.Results != nil && fd.Type.Results.NumFields() > 0 {
		if fd.Type.Results.NumFields() > 1 {
			return nil, nil // multiple results not supported
		}
		fields := fd.Type.Results.List
		tv := src.Info.Types[fields[0].Type]
		result = GoTypeToIR(tv.Type)
		if result == TypeVoid {
			return nil, nil
		}
	}

	fn := NewFunc(fd.Name.Name, params, result)
	lw := &lowerer{src: src, fn: fn}
	if err := lw.lowerBlock(fd.Body.List); err != nil {
		return nil, err
	}
	return fn, nil
}

// lowerer walks a typed Go AST and emits IR instructions into fn.
type lowerer struct {
	src *Source
	fn  *Func
}

func (lw *lowerer) emit(ins Instr) { lw.fn.Emit(ins) }

func (lw *lowerer) lowerBlock(stmts []ast.Stmt) error {
	for _, s := range stmts {
		if err := lw.lowerStmt(s); err != nil {
			return err
		}
	}
	return nil
}

func (lw *lowerer) lowerStmt(stmt ast.Stmt) error {
	switch s := stmt.(type) {

	case *ast.ReturnStmt:
		if len(s.Results) > 0 {
			if err := lw.lowerExpr(s.Results[0]); err != nil {
				return err
			}
		}
		lw.emit(Instr{Op: OpReturn})

	case *ast.AssignStmt:
		if len(s.Lhs) != 1 || len(s.Rhs) != 1 {
			return fmt.Errorf("only single assignment supported")
		}
		ident, ok := s.Lhs[0].(*ast.Ident)
		if !ok {
			return fmt.Errorf("assignment left-hand side must be an identifier")
		}
		if err := lw.lowerExpr(s.Rhs[0]); err != nil {
			return err
		}
		tv := lw.src.Info.Types[s.Rhs[0]]
		lw.fn.DeclareLocal(ident.Name, GoTypeToIR(tv.Type))
		lw.emit(Instr{Op: OpStore, Name: ident.Name})

	case *ast.IncDecStmt:
		ident, ok := s.X.(*ast.Ident)
		if !ok {
			return fmt.Errorf("inc/dec only supported on identifiers")
		}
		lw.emit(Instr{Op: OpLoad, Name: ident.Name})
		lw.emit(Instr{Op: OpConst, IVal: 1, Type: TypeI32})
		if s.Tok == token.INC {
			lw.emit(Instr{Op: OpAdd})
		} else {
			lw.emit(Instr{Op: OpSub})
		}
		lw.emit(Instr{Op: OpStore, Name: ident.Name})

	case *ast.IfStmt:
		return lw.lowerIf(s)

	case *ast.ForStmt:
		return lw.lowerFor(s)

	case *ast.ExprStmt:
		if err := lw.lowerExpr(s.X); err != nil {
			return err
		}
		lw.emit(Instr{Op: OpDrop})

	default:
		return fmt.Errorf("unsupported statement: %T", stmt)
	}
	return nil
}

func (lw *lowerer) lowerExpr(expr ast.Expr) error {
	switch e := expr.(type) {

	case *ast.BasicLit:
		switch e.Kind {
		case token.INT:
			var v int64
			fmt.Sscanf(e.Value, "%d", &v)
			lw.emit(Instr{Op: OpConst, IVal: v, Type: TypeI32})
		case token.FLOAT:
			var v float64
			fmt.Sscanf(e.Value, "%g", &v)
			lw.emit(Instr{Op: OpFConst, FVal: v, Type: TypeF64})
		default:
			return fmt.Errorf("unsupported literal kind: %v", e.Kind)
		}

	case *ast.Ident:
		lw.emit(Instr{Op: OpLoad, Name: e.Name})

	case *ast.BinaryExpr:
		if err := lw.lowerExpr(e.X); err != nil {
			return err
		}
		if err := lw.lowerExpr(e.Y); err != nil {
			return err
		}
		switch e.Op {
		case token.ADD:
			lw.emit(Instr{Op: OpAdd})
		case token.SUB:
			lw.emit(Instr{Op: OpSub})
		case token.MUL:
			lw.emit(Instr{Op: OpMul})
		case token.QUO:
			lw.emit(Instr{Op: OpDivS})
		case token.LSS:
			lw.emit(Instr{Op: OpLtS})
		case token.LEQ:
			lw.emit(Instr{Op: OpLeS})
		case token.GTR:
			lw.emit(Instr{Op: OpGtS})
		case token.GEQ:
			lw.emit(Instr{Op: OpGeS})
		case token.EQL:
			lw.emit(Instr{Op: OpEqI})
		case token.NEQ:
			lw.emit(Instr{Op: OpNeI})
		default:
			return fmt.Errorf("unsupported binary operator: %v", e.Op)
		}

	case *ast.ParenExpr:
		return lw.lowerExpr(e.X)

	case *ast.UnaryExpr:
		if e.Op != token.NOT {
			return fmt.Errorf("unsupported unary operator: %v", e.Op)
		}
		if err := lw.lowerExpr(e.X); err != nil {
			return err
		}
		lw.emit(Instr{Op: OpEqz})

	default:
		return fmt.Errorf("unsupported expression: %T", expr)
	}
	return nil
}

// lowerIf emits a Wasm if/else/end structure for a Go if statement.
func (lw *lowerer) lowerIf(s *ast.IfStmt) error {
	if err := lw.lowerExpr(s.Cond); err != nil {
		return err
	}
	lw.emit(Instr{Op: OpIf}) // Wasm if: pops i32; enters then-branch if non-zero
	if err := lw.lowerBlock(s.Body.List); err != nil {
		return err
	}
	if s.Else != nil {
		lw.emit(Instr{Op: OpElse})
		switch e := s.Else.(type) {
		case *ast.BlockStmt:
			if err := lw.lowerBlock(e.List); err != nil {
				return err
			}
		case *ast.IfStmt:
			if err := lw.lowerIf(e); err != nil {
				return err
			}
		}
	}
	lw.emit(Instr{Op: OpEnd})
	return nil
}

// lowerFor emits a Wasm block/loop structure for a C-style Go for loop.
//
// The generated structure is:
//
//	block $exit       <- br 1 from inside loop exits here
//	  loop $header    <- br 0 from inside loop restarts here
//	    <cond>
//	    i32.eqz       <- invert: non-zero cond means keep looping
//	    br_if 1       <- exit if !cond
//	    <body>
//	    <post>
//	    br 0          <- restart loop
//	  end $header
//	end $exit
func (lw *lowerer) lowerFor(s *ast.ForStmt) error {
	if s.Init != nil {
		if err := lw.lowerStmt(s.Init); err != nil {
			return err
		}
	}
	lw.emit(Instr{Op: OpBlock}) // $exit block (label depth 1 from inside loop)
	lw.emit(Instr{Op: OpLoop})  // $header loop (label depth 0 from inside loop)
	if s.Cond != nil {
		if err := lw.lowerExpr(s.Cond); err != nil {
			return err
		}
		lw.emit(Instr{Op: OpEqz})           // invert: exit on false condition
		lw.emit(Instr{Op: OpBrIf, IVal: 1}) // br_if $exit (depth 1)
	}
	if err := lw.lowerBlock(s.Body.List); err != nil {
		return err
	}
	if s.Post != nil {
		if err := lw.lowerStmt(s.Post); err != nil {
			return err
		}
	}
	lw.emit(Instr{Op: OpBr, IVal: 0}) // br $header (restart loop)
	lw.emit(Instr{Op: OpEnd})         // end $header
	lw.emit(Instr{Op: OpEnd})         // end $exit
	return nil
}
```

Create `compiler/leb128_test.go`. The `Example` functions are auto-verified by `go test` via their `// Output:` comments.

```go
package compiler

import (
	"fmt"
	"testing"
)

func TestAppendULEB128(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v    uint64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{300, []byte{0xac, 0x02}},
		{16384, []byte{0x80, 0x80, 0x01}},
	}
	for _, tc := range cases {
		got := AppendULEB128(nil, tc.v)
		if !bytesEq(got, tc.want) {
			t.Errorf("AppendULEB128(%d) = %v, want %v", tc.v, got, tc.want)
		}
	}
}

func TestAppendSLEB128(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v    int64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{-1, []byte{0x7f}},
		{63, []byte{0x3f}},
		{64, []byte{0xc0, 0x00}},
		{-64, []byte{0x40}},
		{-128, []byte{0x80, 0x7f}},
		{300, []byte{0xac, 0x02}},
	}
	for _, tc := range cases {
		got := AppendSLEB128(nil, tc.v)
		if !bytesEq(got, tc.want) {
			t.Errorf("AppendSLEB128(%d) = %v, want %v", tc.v, got, tc.want)
		}
	}
}

func TestDecodeULEB128(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input []byte
		val   uint64
		n     int
	}{
		{[]byte{0x00}, 0, 1},
		{[]byte{0x7f}, 127, 1},
		{[]byte{0xac, 0x02}, 300, 2},
		{[]byte{0xac, 0x02, 0xff}, 300, 2}, // trailing byte not consumed
	}
	for _, tc := range cases {
		got, n := DecodeULEB128(tc.input)
		if got != tc.val || n != tc.n {
			t.Errorf("DecodeULEB128(%v) = (%d, %d), want (%d, %d)",
				tc.input, got, n, tc.val, tc.n)
		}
	}
}

func TestRoundTripULEB128(t *testing.T) {
	t.Parallel()
	values := []uint64{0, 1, 127, 128, 255, 300, 16383, 16384, 1<<28 - 1, 1 << 28}
	for _, v := range values {
		enc := AppendULEB128(nil, v)
		got, n := DecodeULEB128(enc)
		if got != v || n != len(enc) {
			t.Errorf("round-trip %d: got %d (n=%d), encoded as %v", v, got, n, enc)
		}
	}
}

func ExampleAppendULEB128() {
	buf := AppendULEB128(nil, 300)
	fmt.Println(buf)
	// Output: [172 2]
}

func ExampleAppendSLEB128() {
	buf := AppendSLEB128(nil, -1)
	fmt.Println(buf)
	// Output: [127]
}

func ExampleDecodeULEB128() {
	v, n := DecodeULEB128([]byte{0xac, 0x02, 0xff})
	fmt.Printf("value=%d consumed=%d\n", v, n)
	// Output: value=300 consumed=2
}

func bytesEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

Create `compiler/compiler_test.go`. This file requires `go get github.com/tetratelabs/wazero@v1.8.0` and cannot run offline.

```go
package compiler

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// compileAndRun compiles Go source text, instantiates the module in wazero,
// calls the named function with the given i32 arguments, and returns the i32 result.
func compileAndRun(t *testing.T, src, fn string, args ...int32) int32 {
	t.Helper()
	s, err := ParseAndCheck("input.go", src)
	if err != nil {
		t.Fatalf("ParseAndCheck: %v", err)
	}
	wasmBytes, err := Compile(s)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })

	mod, err := r.Instantiate(ctx, wasmBytes)
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	t.Cleanup(func() { mod.Close(ctx) })

	f := mod.ExportedFunction(fn)
	if f == nil {
		t.Fatalf("exported function %q not found", fn)
	}
	wargs := make([]uint64, len(args))
	for i, a := range args {
		wargs[i] = api.EncodeI32(a)
	}
	results, err := f.Call(ctx, wargs...)
	if err != nil {
		t.Fatalf("Call %s: %v", fn, err)
	}
	if len(results) == 0 {
		return 0
	}
	return api.DecodeI32(results[0])
}

func TestCompileAdd(t *testing.T) {
	t.Parallel()
	src := `package main
func add(a, b int) int { return a + b }
`
	if got := compileAndRun(t, src, "add", 3, 4); got != 7 {
		t.Errorf("add(3,4) = %d, want 7", got)
	}
}

func TestCompileMultiply(t *testing.T) {
	t.Parallel()
	src := `package main
func mul(a, b int) int { return a * b }
`
	if got := compileAndRun(t, src, "mul", 6, 7); got != 42 {
		t.Errorf("mul(6,7) = %d, want 42", got)
	}
}

func TestCompileIfElse(t *testing.T) {
	t.Parallel()
	src := `package main
func abs(x int) int {
	if x < 0 {
		return -x
	} else {
		return x
	}
}
`
	cases := []struct{ in, want int32 }{
		{5, 5}, {-3, 3}, {0, 0},
	}
	for _, tc := range cases {
		if got := compileAndRun(t, src, "abs", tc.in); got != tc.want {
			t.Errorf("abs(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestCompileForLoop(t *testing.T) {
	t.Parallel()
	src := `package main
func sumTo(n int) int {
	s := 0
	for i := 1; i <= n; i++ {
		s = s + i
	}
	return s
}
`
	if got := compileAndRun(t, src, "sumTo", 100); got != 5050 {
		t.Errorf("sumTo(100) = %d, want 5050", got)
	}
}

func TestCompileNegation(t *testing.T) {
	t.Parallel()
	src := `package main
func isZero(x int) int {
	if x == 0 {
		return 1
	}
	return 0
}
`
	cases := []struct{ in, want int32 }{{0, 1}, {1, 0}, {-5, 0}}
	for _, tc := range cases {
		if got := compileAndRun(t, src, "isZero", tc.in); got != tc.want {
			t.Errorf("isZero(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Your turn: add TestCompileDivide that calls a function div(a, b int) int { return a / b }
// with inputs (10, 3) and asserts the result is 3 (integer division truncates toward zero in Wasm).
```

Create `cmd/demo/main.go`. The demo uses only the stdlib-backed parts of the compiler and does not require wazero to run:

```go
package main

import (
	"fmt"
	"os"

	"example.com/gowasm/compiler"
)

func main() {
	// Demonstrate the LEB128 encoder: 300 -> [0xAC, 0x02].
	buf := compiler.AppendULEB128(nil, 300)
	fmt.Printf("LEB128(300)   = %v\n", buf)
	buf = compiler.AppendSLEB128(nil, -1)
	fmt.Printf("SLEB128(-1)   = %v\n", buf)

	// Build a minimal Wasm module by hand: func add(a, b i32) i32 { return a + b }
	mod := &compiler.Module{}
	typeIdx := mod.AddFuncType(
		[]compiler.Type{compiler.TypeI32, compiler.TypeI32},
		[]compiler.Type{compiler.TypeI32},
	)
	var code []byte
	code = append(code, compiler.LocalGet(0)...) // push param a
	code = append(code, compiler.LocalGet(1)...) // push param b
	code = append(code, 0x6a)                    // i32.add
	code = append(code, 0x0f)                    // return
	fidx := mod.AddFunc(typeIdx, nil, code)
	mod.ExportFunc("add", fidx)

	wasmBytes := mod.Bytes()
	fmt.Printf("Module size   = %d bytes\n", len(wasmBytes))
	fmt.Printf("Header        = % x\n", wasmBytes[:8])

	// Optionally compile a Go source file passed as the first argument.
	if len(os.Args) > 1 {
		src, err := os.ReadFile(os.Args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			os.Exit(1)
		}
		parsed, err := compiler.ParseAndCheck(os.Args[1], string(src))
		if err != nil {
			fmt.Fprintln(os.Stderr, "parse/check:", err)
			os.Exit(1)
		}
		out, err := compiler.Compile(parsed)
		if err != nil {
			fmt.Fprintln(os.Stderr, "compile:", err)
			os.Exit(1)
		}
		fmt.Printf("Compiled %s: %d bytes of Wasm\n", os.Args[1], len(out))
		if err := os.WriteFile(os.Args[1]+".wasm", out, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		fmt.Printf("Written to %s.wasm\n", os.Args[1])
	}
}
```

## Common Mistakes

### Confusing Signed and Unsigned LEB128

Wrong: using `AppendULEB128` for the operand of `i32.const`.

What happens: the constant is encoded without sign extension. For negative values such as -1, ULEB128 would produce a 10-byte sequence (the full unsigned representation of the two's complement bit pattern), which is both larger and semantically wrong. The Wasm spec says integer constants use signed LEB128.

Fix: use `AppendSLEB128` for `i32.const`, `i64.const`, and for the offset in active data segment expressions. Use `AppendULEB128` for section sizes, counts, indices, and `local.get`/`local.set` operands.

### Omitting the `end` Opcode at the Function Body Level

Wrong: writing instruction bytes directly from the IR without appending `0x0b` at the end of each function body.

What happens: the Wasm validator rejects the module with a validation error because every function body is itself a structured block that must be closed by `end`. The `encodeCodeSection` method appends `0x0b` after `body.code` for this reason; if you encode function bodies manually and forget this byte, the section's byte count will appear correct but the instruction stream will be malformed.

Fix: always append `0x0b` after the last instruction byte of each function body inside the code section encoding. An explicit `return` does not substitute for the structural `end`.

### Walking the AST Without Calling `types.Config.Check` First

Wrong:

```go
// Wrong: walking the AST before type-checking
f, _ := parser.ParseFile(fset, "x.go", src, 0)
ast.Inspect(f, func(n ast.Node) bool {
    id, ok := n.(*ast.Ident)
    if ok {
        obj := info.ObjectOf(id) // always nil — info was never populated
        _ = obj
    }
    return true
})
```

What happens: `info.Types`, `info.Defs`, and `info.Uses` are all empty maps because `cfg.Check` has not run. Every call to `GoTypeToIR` returns `TypeVoid`, every function is skipped as having an unsupported signature, and the output module is empty — with no error.

Fix: call `cfg.Check` before walking the AST. `ParseAndCheck` enforces this order; never split the two steps.

### Wrong Label Depth for `br_if` in a For Loop

Wrong: emitting `br_if 0` inside the `loop` body to exit the loop.

What happens: `br 0` from inside a `loop` jumps to the *start* of the loop (the `loop` instruction itself), not past it. Using `br_if 0` as an exit condition creates an infinite loop.

Fix: the exit block surrounds the `loop`, so from inside the `loop` body the block is at depth 1. Use `br_if 1` to exit. Inside the Wasm structured encoding:

```
block           ; depth 0 from outside = depth 1 from inside loop
  loop          ; depth 0 from inside loop (restart target)
    <cond>
    i32.eqz
    br_if 1     ; jump PAST the block (exit)
    <body>
    br 0        ; jump to loop START (restart)
  end
end
```

## Verification

The standard library parts (leb128, ir, wasm, frontend, codegen without wazero) are runnable offline. From `~/go-exercises/gowasm`:

```bash
test -z "$(gofmt -l ./compiler/ ./cmd/)"
go vet ./compiler/ ./cmd/demo/
go build ./cmd/demo/
go test -count=1 -race ./compiler/ -run 'TestAppend|TestDecode|TestRoundTrip|TestModule|TestFrontend|TestGoType'
go run ./cmd/demo/
```

Expected output from `go run ./cmd/demo/`:

```
LEB128(300)   = [172 2]
SLEB128(-1)   = [127]
Module size   = 42 bytes
Header        = 00 61 73 6d 01 00 00 00
```

After `go get github.com/tetratelabs/wazero@v1.8.0`, run the full integration suite:

```bash
go test -count=1 -race ./...
```

Add one more test of your own: `TestCompileDivide` in `compiler_test.go`, calling a function `div(a, b int) int { return a / b }` with inputs (10, 3) and asserting the result is 3. Wasm `i32.div_s` truncates toward zero, the same as Go integer division.

To validate the emitted `.wasm` against the official spec, install the WebAssembly Binary Toolkit and run:

```bash
go run ./cmd/demo/ input.go
wasm-validate input.go.wasm
```

## Summary

- Go's compiler frontend (`go/parser`, `go/types`, `go/ast`) ships in the standard library and provides a complete, production-quality parse-and-type-check pipeline. Call `types.Config.Check` before walking the AST; the `types.Info` maps are empty until it runs.
- A Wasm module is a sequence of sections in ascending id order, each length-prefixed with ULEB128. Sections omitted from the output are simply absent; the validator ignores missing optional sections.
- Wasm uses ULEB128 for sizes, counts, and indices; SLEB128 for integer constants. Mixing them produces either wrong values (for negative constants) or unnecessarily wide encodings.
- Wasm's stack machine has no registers. All operands pass through the implicit value stack: push left operand, push right operand, emit the instruction, and the result is left on the stack.
- Wasm control flow is structured. A Go `for` loop maps to a `block`/`loop` pair: `br 0` from inside the `loop` restarts it; `br 1` from inside exits the surrounding `block`.
- The label depth for a branch counts enclosing `block`/`loop`/`if` constructs outward from the branch site, starting at 0.
- Separating the lowering phase (AST to IR) from the code generation phase (IR to bytes) preserves space for optimization passes that operate on the IR without touching the binary encoder.

## What's Next

Next: [Interactive Debugger with ptrace](../13-interactive-debugger-ptrace/13-interactive-debugger-ptrace.md).

## Resources

- [WebAssembly Specification — Binary Format](https://webassembly.github.io/spec/core/binary/index.html): authoritative byte-level encoding of every section and instruction; the LEB128 encoding rules are in Section 5.2.
- [pkg.go.dev/go/types](https://pkg.go.dev/go/types): `Config`, `Info`, `Check`, `Basic`, `BasicKind` — everything used in `frontend.go`.
- [pkg.go.dev/go/parser](https://pkg.go.dev/go/parser): `ParseFile`, `Mode` flags, and the `AllErrors` option used to surface all parse errors.
- [wazero — Pure-Go WebAssembly Runtime](https://wazero.io/): `wazero.NewRuntime`, `Runtime.Instantiate`, `api.Module.ExportedFunction`, `api.EncodeI32`, `api.DecodeI32` — the wazero API used in the test harness.
- [TinyGo Compiler Source](https://github.com/tinygo-org/tinygo): a production Go-to-Wasm compiler; its `compiler/` package is the best real-world reference for the patterns this lesson introduces.
