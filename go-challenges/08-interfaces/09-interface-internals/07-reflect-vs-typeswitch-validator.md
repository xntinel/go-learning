# Exercise 7: Validate Struct Fields Generically With reflect Versus A Type Switch

Request validation off a decoded payload is where reflection earns its keep — or
does not. This module builds a `validate`-tag validator two ways: a reflect-based
pass that works on any struct, and a hand-rolled type switch for one known type,
then reasons (and benchmarks) when the reflection cost is worth paying.

This module is fully self-contained: its own module, code, demo, and tests.

## What you'll build

```text
validate/                   independent module: example.com/validate
  go.mod                    go 1.26
  validate.go               Validate (reflect + tags) and ValidateUserFast (type switch)
  cmd/
    demo/
      main.go               validates a good and a bad struct both ways
  validate_test.go          tag rules pass/fail, non-struct errors, reflect vs fast benchmark
```

- Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: `Validate(any) error` walking fields via reflect and enforcing `required` and `min=N` tags; `ValidateUserFast(User) error` doing the same checks by hand; `ErrNotStruct` sentinel.
- Test: structs with required/min tags pass or fail as expected; non-struct input errors cleanly with `ErrNotStruct`; a benchmark contrasts the reflect pass with the type-switch fast path.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Reflection reads the same metadata the interface header points at

`reflect.ValueOf(v)` and `reflect.TypeOf(v)` open up the runtime type the `any`
header already carries. From there `rt.NumField()`, `rt.Field(i).Tag.Get("validate")`,
and `rv.Field(i)` walk the struct's fields, read each field's struct tag, and read
its value — none of which the compiler knows statically, which is exactly why this
works on an *arbitrary* struct and exactly why it costs more. Each step chases
pointers through metadata and cannot be inlined; reflection also boxes values as
it goes. That is the price of being open-ended.

`Validate` accepts `any`, dereferences a pointer if given one, and requires a
struct — a non-struct input is a programming error, reported as `ErrNotStruct`
(wrapped `%w`). For each field it reads the `validate` tag and enforces two rules:
`required` (a string must be non-empty; a number must be non-zero) and `min=N`
(a string's length, or a numeric value, must be at least N). Violations accumulate
into `errors.Join` so the caller sees every problem at once, which is what a real
API validator does rather than failing on the first field.

`ValidateUserFast` is the closed-world counterpart: for the single known `User`
type, the same checks are three `if` statements on concrete fields. No metadata
walk, no boxing, fully inlinable. It cannot validate anything but a `User` — that
is the trade. The benchmark quantifies the gap: the type-switch/hand-rolled path
is markedly faster and allocation-free, so the rule of thumb is to reach for
reflection only when the set of input types is genuinely open (a generic
middleware validating any registered request type), and to hand-roll the check
when the type is known and the path is hot.

Create `validate.go`:

```go
package validate

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// ErrNotStruct is returned when Validate is given a non-struct value.
var ErrNotStruct = errors.New("not a struct")

// Validate enforces `validate` struct tags on an arbitrary struct using
// reflection. Supported rules: "required" and "min=N". It reports every
// violation via errors.Join.
func Validate(v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return fmt.Errorf("%w: got %s", ErrNotStruct, rv.Kind())
	}

	rt := rv.Type()
	var errs []error
	for i := range rt.NumField() {
		field := rt.Field(i)
		tag := field.Tag.Get("validate")
		if tag == "" {
			continue
		}
		value := rv.Field(i)
		for _, rule := range strings.Split(tag, ",") {
			if err := applyRule(field.Name, rule, value); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func applyRule(name, rule string, value reflect.Value) error {
	switch {
	case rule == "required":
		if isZero(value) {
			return fmt.Errorf("%s is required", name)
		}
	case strings.HasPrefix(rule, "min="):
		n, err := strconv.Atoi(strings.TrimPrefix(rule, "min="))
		if err != nil {
			return fmt.Errorf("%s: bad min rule %q", name, rule)
		}
		if !meetsMin(value, n) {
			return fmt.Errorf("%s must be at least %d", name, n)
		}
	}
	return nil
}

func isZero(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		return v.Len() == 0
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	default:
		return v.IsZero()
	}
}

func meetsMin(v reflect.Value, n int) bool {
	switch v.Kind() {
	case reflect.String:
		return v.Len() >= n
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() >= int64(n)
	default:
		return true
	}
}

// User is the known request type used for the type-switch fast path.
type User struct {
	Name string `validate:"required,min=2"`
	Age  int    `validate:"min=18"`
}

// ValidateUserFast validates a User with hand-rolled checks and no reflection.
// It is the closed-world counterpart to Validate for one known type.
func ValidateUserFast(u User) error {
	var errs []error
	if u.Name == "" {
		errs = append(errs, fmt.Errorf("Name is required"))
	}
	if len(u.Name) < 2 {
		errs = append(errs, fmt.Errorf("Name must be at least 2"))
	}
	if u.Age < 18 {
		errs = append(errs, fmt.Errorf("Age must be at least 18"))
	}
	return errors.Join(errs...)
}
```

### The runnable demo

The demo validates a good `User` and a bad one both ways, printing the joined
errors. The reflect path and the fast path agree on the verdict.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/validate"
)

func main() {
	good := validate.User{Name: "alice", Age: 30}
	bad := validate.User{Name: "", Age: 15}

	fmt.Println("reflect good:", validate.Validate(good))
	fmt.Println("reflect bad: ", validate.Validate(bad))
	fmt.Println("fast good:   ", validate.ValidateUserFast(good))
	fmt.Println("fast bad:    ", validate.ValidateUserFast(bad))
	fmt.Println("non-struct:  ", validate.Validate(42))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
reflect good: <nil>
reflect bad:  Name is required
Name must be at least 2
Age must be at least 18
fast good:    <nil>
fast bad:     Name is required
Name must be at least 2
Age must be at least 18
non-struct:   not a struct: got int
```

(`errors.Join` renders its members one per line, so the bad cases print three
lines.)

### Tests

The table drives structs through `Validate` and asserts pass/fail and, on failure,
that the joined error mentions the offending field. `TestValidateNonStruct` proves
a non-struct input returns `ErrNotStruct` via `errors.Is`. `TestReflectMatchesFast`
cross-checks that the reflect verdict and the fast-path verdict agree on the same
inputs. `BenchmarkValidate` contrasts the two paths so the reflection overhead is
visible as ns/op and allocs/op.

Create `validate_test.go`:

```go
package validate

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateStructTags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		user       User
		wantOK     bool
		wantSubstr string
	}{
		{"valid", User{Name: "alice", Age: 30}, true, ""},
		{"missing name", User{Name: "", Age: 30}, false, "Name is required"},
		{"short name", User{Name: "a", Age: 30}, false, "Name must be at least 2"},
		{"underage", User{Name: "bob", Age: 15}, false, "Age must be at least 18"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tc.user)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("Validate(%+v) = %v, want nil", tc.user, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate(%+v) = nil, want an error", tc.user)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestValidateNonStruct(t *testing.T) {
	t.Parallel()

	for _, v := range []any{42, "hello", []int{1}} {
		if err := Validate(v); !errors.Is(err, ErrNotStruct) {
			t.Fatalf("Validate(%#v) = %v, want ErrNotStruct", v, err)
		}
	}
}

func TestValidatePointerToStruct(t *testing.T) {
	t.Parallel()

	// A pointer to a struct is dereferenced and validated.
	if err := Validate(&User{Name: "alice", Age: 30}); err != nil {
		t.Fatalf("Validate(&User{...}) = %v, want nil", err)
	}
}

func TestReflectMatchesFast(t *testing.T) {
	t.Parallel()

	users := []User{
		{Name: "alice", Age: 30},
		{Name: "", Age: 15},
		{Name: "a", Age: 20},
	}
	for _, u := range users {
		reflectErr := Validate(u) != nil
		fastErr := ValidateUserFast(u) != nil
		if reflectErr != fastErr {
			t.Fatalf("verdict mismatch for %+v: reflect=%v fast=%v", u, reflectErr, fastErr)
		}
	}
}

func BenchmarkValidate(b *testing.B) {
	u := User{Name: "alice", Age: 30}

	b.Run("reflect", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = Validate(u)
		}
	})

	b.Run("fast", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = ValidateUserFast(u)
		}
	})
}
```

## Review

The validator is correct when both paths agree on every verdict — that is what
`TestReflectMatchesFast` guarantees — and when a non-struct input is a clean
`ErrNotStruct` rather than a panic. The design lesson is the trade the benchmark
makes concrete: reflection walks the runtime type metadata the interface already
points at, which lets one function validate any tagged struct but costs pointer
chases, boxing, and lost inlining; the hand-rolled type switch is faster and
allocation-free but only knows the one type. Prefer the closed type switch when the
set of types is known and the path is hot; reserve reflection for genuinely
open-ended input. When you do use reflect, read tags with `StructField.Tag.Get`
rather than parsing the raw tag string, and accumulate violations with
`errors.Join` so the API returns every problem at once.

## Resources

- [reflect package](https://pkg.go.dev/reflect) — `TypeOf`, `ValueOf`, `Kind`, `NumField`, and `StructField.Tag`.
- [StructTag.Get](https://pkg.go.dev/reflect#StructTag.Get) — the conventional tag-parsing entry point the validator uses.
- [The Laws of Reflection](https://go.dev/blog/laws-of-reflection) — how reflection relates to the interface's type and value words.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-errors-as-itab-taxonomy.md](06-errors-as-itab-taxonomy.md) | Next: [08-json-marshaler-receiver-itab.md](08-json-marshaler-receiver-itab.md)
