# Exercise 3: Arrays and Indexing

Arrays add the interpreter's first composite value and its first subscript operator. Two evaluation rules carry the design. An array literal evaluates its elements left to right, short-circuiting on the first error, exactly like a call's arguments. The index operator distinguishes three outcomes that are easy to conflate: a valid index returns the element, an out-of-range index returns `null` (not an error), and a wrong-typed operand — indexing a non-array, or indexing with a non-integer — is a runtime error. Choosing `null` for out-of-range and an error for type misuse is a deliberate language decision, and it is what this module pins down.

This module is fully self-contained. It depends on nothing but the standard library, reproduces the small AST it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
ast.go            AST nodes: array literal, index expression, infix, literals
object.go         Integer, String, Null, Error, Array
env.go            Environment: the scope chain
eval.go           Eval, evalExpressions, evalIndexExpression
eval_test.go      construction, indexing, out-of-range null, type errors
cmd/
  demo/
    main.go       build an array, index it, go out of range, concatenate
```

- Files: `ast.go`, `object.go`, `env.go`, `eval.go`, `cmd/demo/main.go`, `eval_test.go`.
- Implement: an `Array` object with an `Inspect` that prints `[a, b, c]`, an `evalExpressions` that builds the element slice with error short-circuiting, and an `evalIndexExpression` with the three-outcome rule.
- Test: array construction, in-range indexing, out-of-range yields `null`, indexing a non-array errors, indexing with a non-integer errors, and a string-element array.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/40-capstone-language-interpreter/04-tree-walking-evaluator/03-arrays-and-indexing/cmd/demo && cd go-solutions/40-capstone-language-interpreter/04-tree-walking-evaluator/03-arrays-and-indexing
```

### Evaluating an array literal

An `*ArrayLiteral` holds a slice of element expressions. `Eval` walks them left to right through `evalExpressions`, the same helper a call expression uses for its arguments, and the same error discipline applies: the instant an element evaluates to an error, evaluation stops and that error becomes the result of the whole literal. There is no partial array — either every element evaluated cleanly and you get an `*Array`, or you get the first error. This keeps `[1, 2, 1/0]` from producing a half-built array with a garbage third slot.

### The three outcomes of indexing

`evalIndexExpression` receives the already-evaluated left and index objects and must separate three cases. First, the left operand has to be an `*Array`; anything else — indexing an integer, a string, `null` — is a type error, because the subscript operator is only defined on arrays here. Second, the index has to be an `*Integer`; a string or boolean index is a type error. Third, with a valid array and integer index, the index is range-checked: in range returns the element, out of range returns `NULL`.

The out-of-range-returns-null choice is the subtle one. It mirrors how Monkey and several scripting languages treat a missing element as absence rather than as a fault, so `arr[10]` on a three-element array is a lookup that found nothing, not a crash. That is a different category from `arr["x"]`, which is a misuse of the operator and is reported as an error. Keeping "absent element" and "wrong type" as distinct outcomes is the point: collapsing them — erroring on out-of-range, or returning null on a type mismatch — would either make ordinary boundary checks throw or hide real bugs.

Create `ast.go`:

```go
package arrays

// Node is the base interface every AST element implements.
type Node interface{ nodeTag() }

// Statement nodes produce effects.
type Statement interface {
	Node
	stmtTag()
}

// Expression nodes produce values.
type Expression interface {
	Node
	exprTag()
}

// Program is the root node.
type Program struct{ Statements []Statement }

func (p *Program) nodeTag() {}

// LetStatement binds Name to the result of Value.
type LetStatement struct {
	Name  string
	Value Expression
}

func (l *LetStatement) nodeTag() {}
func (l *LetStatement) stmtTag() {}

// ExpressionStatement wraps an expression used in statement position.
type ExpressionStatement struct{ Expr Expression }

func (e *ExpressionStatement) nodeTag() {}
func (e *ExpressionStatement) stmtTag() {}

// IntegerLiteral holds a parsed int64.
type IntegerLiteral struct{ Value int64 }

func (i *IntegerLiteral) nodeTag() {}
func (i *IntegerLiteral) exprTag() {}

// StringLiteral holds a quoted string value.
type StringLiteral struct{ Value string }

func (s *StringLiteral) nodeTag() {}
func (s *StringLiteral) exprTag() {}

// Identifier is a variable reference.
type Identifier struct{ Name string }

func (i *Identifier) nodeTag() {}
func (i *Identifier) exprTag() {}

// InfixExpression applies Operator between Left and Right.
type InfixExpression struct {
	Left     Expression
	Operator string
	Right    Expression
}

func (i *InfixExpression) nodeTag() {}
func (i *InfixExpression) exprTag() {}

// ArrayLiteral constructs an array from its element expressions.
type ArrayLiteral struct{ Elements []Expression }

func (a *ArrayLiteral) nodeTag() {}
func (a *ArrayLiteral) exprTag() {}

// IndexExpression subscripts Left with Index.
type IndexExpression struct {
	Left  Expression
	Index Expression
}

func (i *IndexExpression) nodeTag() {}
func (i *IndexExpression) exprTag() {}
```

Create `object.go`:

```go
package arrays

import (
	"fmt"
	"strings"
)

// ObjectType is the string tag identifying a runtime value's kind.
type ObjectType string

const (
	IntegerType ObjectType = "INTEGER"
	StringType  ObjectType = "STRING"
	NullType    ObjectType = "NULL"
	ErrorType   ObjectType = "ERROR"
	ArrayType   ObjectType = "ARRAY"
)

// Object is implemented by every runtime value.
type Object interface {
	Type() ObjectType
	Inspect() string
}

// Integer wraps int64.
type Integer struct{ Value int64 }

func (i *Integer) Type() ObjectType { return IntegerType }
func (i *Integer) Inspect() string  { return fmt.Sprintf("%d", i.Value) }

// String wraps a Go string.
type String struct{ Value string }

func (s *String) Type() ObjectType { return StringType }
func (s *String) Inspect() string  { return s.Value }

// Null is the singleton null value, returned for an out-of-range index.
type Null struct{}

func (n *Null) Type() ObjectType { return NullType }
func (n *Null) Inspect() string  { return "null" }

// Error represents a runtime error, returned for a type-misused index.
type Error struct{ Message string }

func (e *Error) Type() ObjectType { return ErrorType }
func (e *Error) Inspect() string  { return "ERROR: " + e.Message }

// Array holds an ordered list of Objects.
type Array struct{ Elements []Object }

func (a *Array) Type() ObjectType { return ArrayType }
func (a *Array) Inspect() string {
	elems := make([]string, len(a.Elements))
	for i, e := range a.Elements {
		elems[i] = e.Inspect()
	}
	return "[" + strings.Join(elems, ", ") + "]"
}
```

Create `env.go`:

```go
package arrays

// Environment maps variable names to Objects.
type Environment struct {
	store map[string]Object
}

// NewEnvironment returns a fresh environment.
func NewEnvironment() *Environment {
	return &Environment{store: make(map[string]Object)}
}

// Get looks up name in the environment.
func (e *Environment) Get(name string) (Object, bool) {
	obj, ok := e.store[name]
	return obj, ok
}

// Set creates or overwrites name.
func (e *Environment) Set(name string, val Object) Object {
	e.store[name] = val
	return val
}
```

Create `eval.go`:

```go
package arrays

import "fmt"

// NULL is the singleton returned for an out-of-range index.
var NULL = &Null{}

func isError(obj Object) bool {
	return obj != nil && obj.Type() == ErrorType
}

func newError(format string, args ...any) *Error {
	return &Error{Message: fmt.Sprintf(format, args...)}
}

// Eval recursively evaluates node in env and never panics.
func Eval(node Node, env *Environment) Object {
	switch n := node.(type) {
	case *Program:
		return evalProgram(n, env)
	case *ExpressionStatement:
		return Eval(n.Expr, env)
	case *LetStatement:
		val := Eval(n.Value, env)
		if isError(val) {
			return val
		}
		env.Set(n.Name, val)
		return NULL
	case *IntegerLiteral:
		return &Integer{Value: n.Value}
	case *StringLiteral:
		return &String{Value: n.Value}
	case *Identifier:
		return evalIdentifier(n, env)
	case *InfixExpression:
		left := Eval(n.Left, env)
		if isError(left) {
			return left
		}
		right := Eval(n.Right, env)
		if isError(right) {
			return right
		}
		return evalInfixExpression(n.Operator, left, right)
	case *ArrayLiteral:
		elems, err := evalExpressions(n.Elements, env)
		if err != nil {
			return err
		}
		return &Array{Elements: elems}
	case *IndexExpression:
		left := Eval(n.Left, env)
		if isError(left) {
			return left
		}
		index := Eval(n.Index, env)
		if isError(index) {
			return index
		}
		return evalIndexExpression(left, index)
	}
	return newError("unknown node type: %T", node)
}

func evalProgram(prog *Program, env *Environment) Object {
	var result Object
	for _, stmt := range prog.Statements {
		result = Eval(stmt, env)
		if isError(result) {
			return result
		}
	}
	return result
}

func evalIdentifier(node *Identifier, env *Environment) Object {
	if val, ok := env.Get(node.Name); ok {
		return val
	}
	return newError("identifier not found: %s", node.Name)
}

// evalExpressions evaluates a list left-to-right and short-circuits on the first
// error, so an array literal never half-builds.
func evalExpressions(exprs []Expression, env *Environment) ([]Object, *Error) {
	result := make([]Object, 0, len(exprs))
	for _, e := range exprs {
		val := Eval(e, env)
		if isError(val) {
			return nil, val.(*Error)
		}
		result = append(result, val)
	}
	return result, nil
}

func evalInfixExpression(op string, left, right Object) Object {
	switch {
	case left.Type() == IntegerType && right.Type() == IntegerType:
		l, r := left.(*Integer).Value, right.(*Integer).Value
		switch op {
		case "+":
			return &Integer{Value: l + r}
		case "-":
			return &Integer{Value: l - r}
		case "*":
			return &Integer{Value: l * r}
		}
		return newError("unknown operator: INTEGER %s INTEGER", op)
	case left.Type() == StringType && right.Type() == StringType:
		if op == "+" {
			return &String{Value: left.(*String).Value + right.(*String).Value}
		}
		return newError("unknown operator: STRING %s STRING", op)
	}
	return newError("type mismatch: %s %s %s", left.Type(), op, right.Type())
}

// evalIndexExpression implements the three-outcome rule: in-range returns the
// element, out-of-range returns NULL, and a wrong-typed operand returns an error.
func evalIndexExpression(left, index Object) Object {
	arr, ok := left.(*Array)
	if !ok {
		return newError("index operator not supported for %s", left.Type())
	}
	idx, ok := index.(*Integer)
	if !ok {
		return newError("array index must be INTEGER, got %s", index.Type())
	}
	i := idx.Value
	if i < 0 || i >= int64(len(arr.Elements)) {
		return NULL // out-of-range is absence, not an error
	}
	return arr.Elements[i]
}
```

### The runnable demo

The demo builds a three-element array and prints it, indexes in range, indexes out of range to show the `null`, and concatenates two string elements pulled out by index. Small local constructors keep the hand-built ASTs readable.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/arrays"
)

func intLit(v int64) *arrays.IntegerLiteral { return &arrays.IntegerLiteral{Value: v} }
func strLit(v string) *arrays.StringLiteral { return &arrays.StringLiteral{Value: v} }
func ident(n string) *arrays.Identifier     { return &arrays.Identifier{Name: n} }

func arrayLit(elems ...arrays.Expression) *arrays.ArrayLiteral {
	return &arrays.ArrayLiteral{Elements: elems}
}

func index(left, idx arrays.Expression) *arrays.IndexExpression {
	return &arrays.IndexExpression{Left: left, Index: idx}
}

func exprStmt(e arrays.Expression) *arrays.ExpressionStatement {
	return &arrays.ExpressionStatement{Expr: e}
}

func letStmt(name string, v arrays.Expression) *arrays.LetStatement {
	return &arrays.LetStatement{Name: name, Value: v}
}

func run(stmts ...arrays.Statement) string {
	env := arrays.NewEnvironment()
	return arrays.Eval(&arrays.Program{Statements: stmts}, env).Inspect()
}

func main() {
	// 1. Build and print an array.
	fmt.Println("array:", run(exprStmt(arrayLit(intLit(1), intLit(2), intLit(3)))))

	// 2. Index in range.
	fmt.Println("arr[1]:", run(exprStmt(index(arrayLit(intLit(1), intLit(2), intLit(3)), intLit(1)))))

	// 3. Index out of range returns null.
	fmt.Println("arr[10]:", run(exprStmt(index(arrayLit(intLit(1), intLit(2), intLit(3)), intLit(10)))))

	// 4. Concatenate two string elements pulled out by index.
	concat := run(
		letStmt("words", arrayLit(strLit("go"), strLit("lang"))),
		exprStmt(&arrays.InfixExpression{
			Left:     index(ident("words"), intLit(0)),
			Operator: "+",
			Right:    index(ident("words"), intLit(1)),
		}),
	)
	fmt.Println("concat:", concat)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
array: [1, 2, 3]
arr[1]: 2
arr[10]: null
concat: golang
```

### Tests

The tests pin construction and all three index outcomes. `TestArrayIndexing` checks an in-range read and the out-of-range `null` in one place. `TestIndexNonArray` and `TestIndexNonInteger` pin the two type-error paths so they cannot drift into returning `null`, which would hide misuse.

Create `eval_test.go`:

```go
package arrays

import (
	"fmt"
	"testing"
)

func intLit(v int64) *IntegerLiteral { return &IntegerLiteral{Value: v} }
func strLit(v string) *StringLiteral { return &StringLiteral{Value: v} }

func arrayLit(elems ...Expression) *ArrayLiteral {
	return &ArrayLiteral{Elements: elems}
}

func index(left, idx Expression) *IndexExpression {
	return &IndexExpression{Left: left, Index: idx}
}

func eval(node Node) Object {
	return Eval(node, NewEnvironment())
}

func TestArrayConstruction(t *testing.T) {
	t.Parallel()

	got := eval(arrayLit(intLit(1), intLit(2), intLit(3)))
	arr, ok := got.(*Array)
	if !ok {
		t.Fatalf("Eval = %T, want *Array", got)
	}
	if len(arr.Elements) != 3 {
		t.Fatalf("len = %d, want 3", len(arr.Elements))
	}
	if arr.Inspect() != "[1, 2, 3]" {
		t.Errorf("Inspect = %q, want %q", arr.Inspect(), "[1, 2, 3]")
	}
}

func TestArrayIndexing(t *testing.T) {
	t.Parallel()

	arr := arrayLit(intLit(1), intLit(2), intLit(3))

	// in-range: arr[1] == 2
	got := eval(index(arr, intLit(1)))
	if i, ok := got.(*Integer); !ok || i.Value != 2 {
		t.Errorf("arr[1] = %v, want 2", got)
	}

	// out-of-range returns null, not an error
	got = eval(index(arr, intLit(10)))
	if got != NULL {
		t.Errorf("arr[10] = %v, want null", got)
	}
}

func TestStringArray(t *testing.T) {
	t.Parallel()

	got := eval(index(arrayLit(strLit("a"), strLit("b")), intLit(1)))
	s, ok := got.(*String)
	if !ok {
		t.Fatalf("Eval = %T, want *String", got)
	}
	if s.Value != "b" {
		t.Errorf("got %q, want %q", s.Value, "b")
	}
}

// TestIndexNonArray verifies indexing a non-array is a type error.
func TestIndexNonArray(t *testing.T) {
	t.Parallel()

	got := eval(index(intLit(5), intLit(0)))
	if _, ok := got.(*Error); !ok {
		t.Fatalf("Eval = %T, want *Error", got)
	}
}

// TestIndexNonInteger verifies a non-integer index is a type error.
func TestIndexNonInteger(t *testing.T) {
	t.Parallel()

	got := eval(index(arrayLit(intLit(1)), strLit("x")))
	if _, ok := got.(*Error); !ok {
		t.Fatalf("Eval = %T, want *Error", got)
	}
}

func ExampleArray_inspect() {
	got := eval(arrayLit(intLit(1), intLit(2), intLit(3)))
	fmt.Println(got.Inspect())
	// Output: [1, 2, 3]
}

func ExampleEval_outOfRange() {
	got := eval(index(arrayLit(intLit(1), intLit(2)), intLit(9)))
	fmt.Println(got.Inspect())
	// Output: null
}
```

## Review

The module is correct when construction is all-or-nothing and indexing keeps its three outcomes distinct. `evalExpressions` either returns a fully built element slice or the first element error, so no array is ever half-constructed. `evalIndexExpression` returns the element for an in-range integer index, `NULL` for an out-of-range integer index, and an `*Error` for a non-array left operand or a non-integer index — `TestArrayIndexing`, `TestIndexNonArray`, and `TestIndexNonInteger` pin all three so none collapses into another.

The mistakes to avoid are conflations. Returning an error for an out-of-range index turns ordinary boundary lookups into failures; returning `null` for a non-array or non-integer operand hides a genuine type misuse behind a value that looks like absence. Building the element slice without the `isError` short-circuit lets `[1, 2, 1/0]` produce a partially filled array with a meaningless slot instead of surfacing the division error.

## Resources

- [Writing An Interpreter In Go, Thorsten Ball](https://interpreterbook.com/) — the array object and index-operator design this module follows.
- [Crafting Interpreters, Robert Nystrom](https://craftinginterpreters.com/) — tree-walking evaluation, including composite values and subscripting.
- [Go Specification: Index expressions](https://go.dev/ref/spec#Index_expressions) — how Go itself defines indexing and bounds, the model behind the range check.

---

Back to [02-loops-and-control-flow.md](02-loops-and-control-flow.md) | Next: [../05-builtin-functions/00-concepts.md](../05-builtin-functions/00-concepts.md)
