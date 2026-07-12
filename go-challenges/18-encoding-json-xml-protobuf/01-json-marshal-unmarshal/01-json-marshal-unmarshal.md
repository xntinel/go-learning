# 1. JSON Marshal and Unmarshal

Go's `encoding/json` is the standard for converting Go values to and from JSON. The two operations are `Marshal` (Go to bytes) and `Unmarshal` (bytes to Go), and the model they share is recursive: a struct becomes an object, a slice or array becomes a JSON array, a map with string keys becomes an object, and a primitive becomes the matching JSON type. The library has a few rules worth understanding before any project: only exported fields are visible to the encoder, `Unmarshal` needs a pointer because it must mutate the destination, and unknown JSON fields are silently ignored by default. The hard parts of using this package well are all about matching the rules of `encoding/json` to the shape of real data.

## Concepts

### Marshaling: Go to JSON Bytes

`json.Marshal(v any) ([]byte, error)` walks `v` recursively and produces a JSON document. For each value it visits, the encoder picks an encoding based on the Go type. Structs become objects (one exported field per member), maps with string keys become objects, slices and arrays become arrays, `[]byte` becomes a base64-encoded string, pointers follow the pointee, and a `nil` interface or `nil` pointer becomes `null`. Three details matter:

1. **Unexported fields are invisible.** `json.Marshal` only walks exported fields, so a struct with `name` (unexported) will not contain a `name` key in the output. The encoder honors the field name, not the tag, for visibility.
2. **HTML escaping is on by default.** `Marshal` and `Encoder.Encode` escape `<`, `>`, `&`, U+2028, and U+2029 to their `\u00xx` form so the output is safe to embed in a `<script>` tag. `Encoder.SetEscapeHTML(false)` turns this off when the output is not going into HTML.
3. **A non-nil error is a real failure.** The `error` return is not just for cyclic data and channels (which `Marshal` rejects with `UnsupportedTypeError`); some legitimate values fail too — `NaN` and `±Inf` floats return `UnsupportedValueError` because JSON has no representation for them.

### Unmarshaling: JSON Bytes to Go

`json.Unmarshal(data []byte, v any) error` parses JSON and writes results into the value pointed at by `v`. The pointer is mandatory: `Unmarshal` must mutate the destination, and passing a non-pointer returns an `InvalidUnmarshalError` without reading the data. Five rules govern the decode:

1. **Object keys match struct fields by tag, then by case-insensitive name.** A tag of `json:"name"` forces the match; absent a tag, `Name` matches `"Name"`, `"name"`, `"NAME"`, and so on.
2. **Extra fields in the JSON are silently dropped** unless you opt in with `Decoder.DisallowUnknownFields()`. This is the most common source of "the API returns X but I see zero" bugs: a typo in the JSON tag means the field is treated as unknown.
3. **A missing field leaves the Go field at its zero value.** `Unmarshal` does not touch fields that have no matching key in the input.
4. **Type mismatches return an `UnmarshalTypeError`** that names the offending key, the JSON type, and the Go type. Catch the error, don't log-and-continue, in production code.
5. **Numbers lose precision when decoded into a `float64`** if the integer part exceeds 53 bits. For arbitrary-size numbers, decode into `json.Number` (via `Decoder.UseNumber()`) and parse with `Int64` or `Float64` yourself.

### MarshalIndent And The Cost Of Pretty Printing

`json.MarshalIndent(v, prefix, indent)` is `Marshal` plus formatting: each element begins on a new line prefixed with `prefix` and one or more copies of `indent`. Use it for debugging and for human-readable config files. Avoid it on the hot path: the indentation adds bytes and time. For production logs and protocols, use `json.Marshal` or `json.Encoder` with no indent.

### Maps, Slices, And `interface{}`

You can `Marshal` and `Unmarshal` maps and slices directly. `map[string]int` becomes a JSON object with integer values; `[]int` becomes an array; `map[string]any` becomes an object whose values are encoded by the dynamic type of each value. The pattern `json.Unmarshal(bytes, &result)` where `result` is a `map[string]any` is the right way to ask "what is in this JSON without committing to a struct shape" — you can then type-assert each value to handle it. The trade-off is that numbers come back as `float64` by default; use `Decoder.UseNumber()` if you need the original integer precision.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/18-encoding-json-xml-protobuf/01-json-marshal-unmarshal/01-json-marshal-unmarshal/internal/book
mkdir -p go-solutions/18-encoding-json-xml-protobuf/01-json-marshal-unmarshal/01-json-marshal-unmarshal/cmd/demo
cd go-solutions/18-encoding-json-xml-protobuf/01-json-marshal-unmarshal/01-json-marshal-unmarshal
```

This is a library plus a tiny demo. You verify the library with `go test`; you run the demo with `go run ./cmd/demo`.

### Exercise 1: The `Book` Type And Constructor

Create `internal/book/book.go`:

```go
package book

import "errors"

var (
	ErrEmptyTitle  = errors.New("book title must not be empty")
	ErrInvalidYear = errors.New("book year must be greater than zero")
)

type Book struct {
	Title     string
	Author    string
	Pages     int
	Published bool
}

func New(title, author string, pages int, published bool) (Book, error) {
	if title == "" {
		return Book{}, ErrEmptyTitle
	}
	if pages < 0 {
		return Book{}, ErrInvalidYear
	}
	return Book{
		Title:     title,
		Author:    author,
		Pages:     pages,
		Published: published,
	}, nil
}

func (b Book) TitleAuthor() string {
	return b.Title + " by " + b.Author
}
```

The constructor validates inputs and returns sentinel errors. `TitleAuthor` is a small exported accessor the demo will use so the demo can stay in `package main` and only touch exported API.

### Exercise 2: Marshal A Slice, Round-Trip It

Create `internal/book/book_test.go`:

```go
package book

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestNewRejectsEmptyTitle(t *testing.T) {
	t.Parallel()

	if _, err := New("", "Anonymous", 100, true); !errors.Is(err, ErrEmptyTitle) {
		t.Fatalf("err = %v, want ErrEmptyTitle", err)
	}
}

func TestNewRejectsNegativePages(t *testing.T) {
	t.Parallel()

	if _, err := New("a", "b", -1, false); !errors.Is(err, ErrInvalidYear) {
		t.Fatalf("err = %v, want ErrInvalidYear", err)
	}
}

func TestMarshalProducesExpectedJSON(t *testing.T) {
	t.Parallel()

	books := []Book{
		{Title: "The Go Programming Language", Author: "Donovan and Kernighan", Pages: 380, Published: true},
		{Title: "Concurrency in Go", Author: "Katherine Cox-Buday", Pages: 238, Published: true},
		{Title: "Untitled Draft", Author: "Unknown", Pages: 0, Published: false},
	}

	data, err := json.MarshalIndent(books, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent error: %v", err)
	}

	want := "[\n" +
		"  {\n" +
		"    \"Title\": \"The Go Programming Language\",\n" +
		"    \"Author\": \"Donovan and Kernighan\",\n" +
		"    \"Pages\": 380,\n" +
		"    \"Published\": true\n" +
		"  },\n" +
		"  {\n" +
		"    \"Title\": \"Concurrency in Go\",\n" +
		"    \"Author\": \"Katherine Cox-Buday\",\n" +
		"    \"Pages\": 238,\n" +
		"    \"Published\": true\n" +
		"  },\n" +
		"  {\n" +
		"    \"Title\": \"Untitled Draft\",\n" +
		"    \"Author\": \"Unknown\",\n" +
		"    \"Pages\": 0,\n" +
		"    \"Published\": false\n" +
		"  }\n" +
		"]"
	if string(data) != want {
		t.Fatalf("MarshalIndent mismatch\n got: %s\nwant: %s", data, want)
	}
}

func TestUnmarshalRoundTrip(t *testing.T) {
	t.Parallel()

	in := []Book{
		{Title: "The Go Programming Language", Author: "Donovan and Kernighan", Pages: 380, Published: true},
		{Title: "Concurrency in Go", Author: "Katherine Cox-Buday", Pages: 238, Published: true},
		{Title: "Untitled Draft", Author: "Unknown", Pages: 0, Published: false},
	}

	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var out []Book
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if len(in) != len(out) {
		t.Fatalf("len mismatch: in=%d out=%d", len(in), len(out))
	}
	for i := range in {
		if in[i] != out[i] {
			t.Errorf("index %d: in=%+v out=%+v", i, in[i], out[i])
		}
	}
}

func TestMarshalUnexportedFieldIgnored(t *testing.T) {
	t.Parallel()

	type withUnexported struct {
		Visible string
		hidden  string
	}
	v := withUnexported{Visible: "yes", hidden: "secret"}

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if _, ok := got["hidden"]; ok {
		t.Fatalf("unexported field 'hidden' must not appear in JSON: %s", data)
	}
	if got["Visible"] != "yes" {
		t.Fatalf("Visible = %v, want yes", got["Visible"])
	}
}

func TestUnmarshalIgnoresUnknownFields(t *testing.T) {
	t.Parallel()

	type point struct {
		X int
		Y int
	}
	in := []byte(`{"X":1,"Y":2,"Extra":"ignored","Another":99}`)

	var p point
	if err := json.Unmarshal(in, &p); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if p.X != 1 || p.Y != 2 {
		t.Fatalf("p = %+v, want {1 2}", p)
	}
}

func TestMarshalMapAndSlice(t *testing.T) {
	t.Parallel()

	m := map[string]any{
		"name":   "Charlie",
		"scores": []int{95, 87, 92},
	}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if out["name"] != "Charlie" {
		t.Fatalf("name = %v, want Charlie", out["name"])
	}
	scores, ok := out["scores"].([]any)
	if !ok {
		t.Fatalf("scores type = %T, want []any", out["scores"])
	}
	if len(scores) != 3 {
		t.Fatalf("len(scores) = %d, want 3", len(scores))
	}
}

func ExampleNew() {
	b, _ := New("The Go Programming Language", "Donovan and Kernighan", 380, true)
	data, _ := json.MarshalIndent(b, "", "  ")
	fmt.Println(string(data))
	// Output:
	// {
	//   "Title": "The Go Programming Language",
	//   "Author": "Donovan and Kernighan",
	//   "Pages": 380,
	//   "Published": true
	// }
}
```

The tests pin three contracts: the constructor rejects bad inputs with the right sentinel, `MarshalIndent` produces the exact indented document for a known input, and a round-trip `Marshal` then `Unmarshal` is the identity for a slice of `Book`. `TestMarshalUnexportedFieldIgnored` and `TestUnmarshalIgnoresUnknownFields` enforce the visibility rules: lowercase fields are absent from output, unknown JSON keys are absent from input.

### Exercise 3: A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"

	"example.com/bookjson/internal/book"
)

func main() {
	books := []book.Book{
		{Title: "The Go Programming Language", Author: "Donovan and Kernighan", Pages: 380, Published: true},
		{Title: "Concurrency in Go", Author: "Katherine Cox-Buday", Pages: 238, Published: true},
		{Title: "Untitled Draft", Author: "Unknown", Pages: 0, Published: false},
	}

	data, err := json.MarshalIndent(books, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(data))

	fmt.Println()
	fmt.Println("Decoded books:")
	var decoded []book.Book
	if err := json.Unmarshal(data, &decoded); err != nil {
		log.Fatal(err)
	}
	for _, b := range decoded {
		fmt.Printf("  %+v\n", b)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

The first block is the JSON output; the second is the round-tripped slice printed with `%+v`.

Your turn: add a `TestUnmarshalTypeMismatchReturnsError` test that decodes `{"Pages":"oops"}` into `Book` and asserts that `Unmarshal` returns a non-nil error. This pins the contract that type mismatches fail loudly instead of silently corrupting data.

## Common Mistakes

### Forgetting To Pass A Pointer To `Unmarshal`

Wrong: `json.Unmarshal(data, book)` where `book` is a `Book` (value). `Unmarshal` panics-or-returns-`InvalidUnmarshalError` because it cannot write to a value that has no address.

Fix: pass `&book` (pointer to the value). The compiler will not catch this for variables stored in `interface{}`, so always write `&` at the call site.

### Treating `nil` Error Return As A Bug

Wrong: `data, _ := json.Marshal(v)` and then using `data` as if `Marshal` cannot fail. For arbitrary `any` values, `Marshal` rejects channels, functions, complex numbers, `NaN`, and `±Inf` with a real error. The `_, _` discards the most useful information you have when the encoder fails.

Fix: handle the error. Either return it, log it, or assert it deliberately in a test. The verification gate at the end of this lesson runs `go test -race`, which is the cheapest way to catch a swallowed error.

### Assuming `Unmarshal` Rejects Unknown Fields

Wrong: the API contract says "the JSON must contain exactly these keys" but the code uses `json.Unmarshal` and the wrong key is silently dropped. The application proceeds with zero values and only fails far from the cause.

Fix: when the contract requires strict matching, decode with `json.NewDecoder(r).Decode(&v)` after `dec.DisallowUnknownFields()`. The lesson on `Decoder` covers this in detail.

### Misreading `json.MarshalIndent` Output As Stable Across Versions

Wrong: copying the indented output of `MarshalIndent` into a golden test and expecting it to never change. `Marshal` and `MarshalIndent` use the same encoding rules, but the indentation and field order are deterministic, so a stable comparison is fine. The mistake is comparing a `Marshal` output to a `MarshalIndent` golden, or comparing after running through a third-party tool that reformats.

Fix: capture the canonical output with the exact same call you use in code, and pin the test to that. `TestMarshalProducesExpectedJSON` in this lesson does this for `MarshalIndent`.

## Verification

From `~/go-exercises/bookjson`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All four gates must pass, and the demo must print the JSON block followed by the round-tripped slice. The `ExampleNew` test in the suite is also auto-verified by `go test` because it carries an `// Output:` comment.

## Summary

- `json.Marshal` encodes a Go value to JSON bytes; `json.Unmarshal` decodes JSON bytes into a Go value.
- Only exported struct fields are visible to the encoder; lowercase fields are absent from output.
- `Unmarshal` requires a non-nil pointer; passing a value returns an `InvalidUnmarshalError`.
- Unknown JSON fields are silently dropped unless the caller enables `Decoder.DisallowUnknownFields()`.
- `MarshalIndent` adds line breaks and indentation for human-readable output; use it for debugging, not hot paths.

## What's Next

Next: [Struct Tags for JSON](../02-struct-tags-for-json/02-struct-tags-for-json.md).

## Resources

- [encoding/json package](https://pkg.go.dev/encoding/json)
- [JSON and Go (Go blog)](https://go.dev/blog/json)
- [RFC 8259: The JavaScript Object Notation Data Interchange Format](https://datatracker.ietf.org/doc/html/rfc8259)
- [Go Specification: JSON tags - struct tags](https://go.dev/ref/spec#Struct_types)
