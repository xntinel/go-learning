# 2. Inspecting Struct Fields and Tags

Build a documentation helper that reads exported struct fields and their tags. The useful part of struct reflection is not printing fields; it is turning `reflect.StructField` metadata into a stable contract that other code can test.

## Concepts

### StructField Is Metadata, Not the Field Value

`reflect.Type.Field(i)` returns a `reflect.StructField`: name, type, tag, index path, and export status. It does not read the value stored in a particular struct instance. Tag-driven tools usually start from `reflect.Type`, not `reflect.Value`, because the mapping is type metadata.

### Tags Are Conventional Strings

A struct tag is a raw string interpreted by packages such as `encoding/json`. `StructTag.Get("json")` returns the value for one key. Go does not validate the meaning of `json:"name,omitempty"`; your tool must decide what to accept.

### Unexported Fields Are Usually Skipped

Reflection can see unexported field metadata, but packages outside the declaring package cannot safely treat those fields as public API. A documentation generator should skip unexported fields unless it is intentionally documenting internals.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/tagdoc/cmd/demo
cd ~/go-exercises/tagdoc
go mod init example.com/tagdoc
```

### Exercise 1: Extract Field Metadata

Create `tagdoc.go`:

```go
package tagdoc

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

var ErrNotStruct = errors.New("value must be a struct or pointer to struct")

type FieldDoc struct {
	Name     string
	Type     string
	JSONName string
	DBName   string
}

func Document(v any) ([]FieldDoc, error) {
	t := reflect.TypeOf(v)
	if t == nil {
		return nil, fmt.Errorf("document: %w", ErrNotStruct)
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("document %s: %w", t.Kind(), ErrNotStruct)
	}

	var docs []FieldDoc
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		jsonName := tagName(field.Tag.Get("json"), field.Name)
		if jsonName == "-" {
			continue
		}
		docs = append(docs, FieldDoc{Name: field.Name, Type: field.Type.String(), JSONName: jsonName, DBName: tagName(field.Tag.Get("db"), field.Name)})
	}
	return docs, nil
}

func tagName(tag string, fallback string) string {
	if tag == "" {
		return fallback
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return fallback
	}
	return name
}
```

### Exercise 2: Add an Example

Create `tagdoc_example_test.go`:

```go
package tagdoc

import "fmt"

type Account struct {
	ID    int    `json:"id" db:"account_id"`
	Email string `json:"email" db:"email"`
	hash  string
}

func ExampleDocument() {
	docs, _ := Document(Account{})
	for _, doc := range docs {
		fmt.Printf("%s %s %s\n", doc.Name, doc.JSONName, doc.DBName)
	}
	// Output:
	// ID id account_id
	// Email email email
}
```

### Exercise 3: Test the Contract

Create `tagdoc_test.go`:

```go
package tagdoc

import (
	"errors"
	"testing"
)

type User struct {
	ID     int    `json:"id" db:"user_id"`
	Name   string `json:"name,omitempty" db:"name"`
	Secret string `json:"-" db:"secret"`
	state  string
}

func TestDocumentReadsTags(t *testing.T) {
	t.Parallel()

	docs, err := Document(&User{})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("len(docs) = %d, want 2", len(docs))
	}
	if docs[0].Name != "ID" || docs[0].JSONName != "id" || docs[0].DBName != "user_id" {
		t.Fatalf("first doc = %+v", docs[0])
	}
	if docs[1].Name != "Name" || docs[1].JSONName != "name" {
		t.Fatalf("second doc = %+v", docs[1])
	}
}

func TestDocumentRejectsNonStruct(t *testing.T) {
	t.Parallel()

	tests := []any{nil, 42, []string{"x"}}
	for _, in := range tests {
		_, err := Document(in)
		if !errors.Is(err, ErrNotStruct) {
			t.Fatalf("Document(%T) err = %v, want ErrNotStruct", in, err)
		}
	}
}
```

### Exercise 4: Run a Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/tagdoc"
)

type Product struct {
	SKU   string `json:"sku" db:"sku"`
	Price int    `json:"price" db:"price_cents"`
}

func main() {
	docs, err := tagdoc.Document(Product{})
	if err != nil {
		log.Fatal(err)
	}
	for _, doc := range docs {
		fmt.Printf("%s maps to json=%s db=%s\n", doc.Name, doc.JSONName, doc.DBName)
	}
}
```

## Common Mistakes

### Reading Tags From Values Instead of Types

Wrong: trying to get tags from `reflect.Value.Field(i)`. Tags live on `reflect.StructField`, which comes from the type.

Fix: inspect `reflect.Type` for metadata and use `reflect.Value` only when you need field contents.

### Treating `omitempty` as Part of the Name

Wrong: storing `name,omitempty` as the JSON field name.

Fix: split the tag at the first comma. `Document` uses `strings.Cut` for that.

### Forgetting Pointer Inputs

Wrong: accepting `User{}` but rejecting `&User{}` even though callers commonly pass pointers.

Fix: if the kind is `Pointer`, call `Elem` before checking for `Struct`.

## Verification

Run this from `~/go-exercises/tagdoc`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add a field with `json:",omitempty"` and prove the fallback name is the Go field name.

## Summary

- `reflect.StructField` contains field metadata, including tags.
- Tags are strings interpreted by convention; your code must parse the options it supports.
- Export status matters when a reflection utility defines public API behavior.
- Pointer-to-struct inputs should usually be accepted by calling `Elem` on the type.

## What's Next

Next: [Dynamic Method Invocation](../03-dynamic-method-invocation/03-dynamic-method-invocation.md).

## Resources

- [reflect.StructField](https://pkg.go.dev/reflect#StructField)
- [reflect.StructTag](https://pkg.go.dev/reflect#StructTag)
- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types)
