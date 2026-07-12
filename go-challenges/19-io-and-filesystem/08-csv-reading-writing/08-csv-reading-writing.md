# 8. CSV Reading and Writing

Build a small employee CSV package that parses records, validates required fields, and writes sorted output. The lesson uses `encoding/csv` so quoting, commas inside fields, and newline handling are delegated to the standard library.

## Concepts

### CSV Is A Streaming Format

`csv.Reader` reads records from an `io.Reader`; `csv.Writer` writes records to an `io.Writer`. The package handles quoting and escaping. Your code should validate the meaning of fields, not split lines by comma.

### Headers Are A Contract

A parser that expects specific columns should validate the header before reading data rows. That catches mismatched exports early and gives callers a typed failure instead of corrupt data.

### Writer Errors Are Buffered

`csv.Writer` buffers output. Always call `Flush` and then check `Error`; otherwise write failures can be missed.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/19-io-and-filesystem/08-csv-reading-writing/08-csv-reading-writing/cmd/demo
cd go-solutions/19-io-and-filesystem/08-csv-reading-writing/08-csv-reading-writing
```

### Exercise 1: Parse Employees

Create `csv.go`:

```go
package employeecsv

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

type Employee struct {
	Name       string
	Department string
	Salary     int
}

func ReadEmployees(r io.Reader) ([]Employee, error) {
	if r == nil {
		return nil, fmt.Errorf("read employees: %w", ErrNilReader)
	}
	cr := csv.NewReader(r)
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	if !sameHeader(header, []string{"name", "department", "salary"}) {
		return nil, fmt.Errorf("read employees: %w", ErrBadHeader)
	}

	var employees []Employee
	for row := 2; ; row++ {
		record, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read row %d: %w", row, err)
		}
		emp, err := parseEmployee(record)
		if err != nil {
			return nil, fmt.Errorf("read row %d: %w", row, err)
		}
		employees = append(employees, emp)
	}
	return employees, nil
}

func WriteEmployees(w io.Writer, employees []Employee) error {
	if w == nil {
		return fmt.Errorf("write employees: %w", ErrNilWriter)
	}
	rows := append([]Employee(nil), employees...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"name", "department", "salary"}); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	for _, emp := range rows {
		if err := validateEmployee(emp); err != nil {
			return fmt.Errorf("write employee %q: %w", emp.Name, err)
		}
		if err := cw.Write([]string{emp.Name, emp.Department, strconv.Itoa(emp.Salary)}); err != nil {
			return fmt.Errorf("write employee %q: %w", emp.Name, err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("flush employees: %w", err)
	}
	return nil
}

func parseEmployee(record []string) (Employee, error) {
	if len(record) != 3 {
		return Employee{}, fmt.Errorf("%w: got %d fields", ErrBadFieldCount, len(record))
	}
	salary, err := strconv.Atoi(strings.TrimSpace(record[2]))
	if err != nil {
		return Employee{}, fmt.Errorf("%w: %v", ErrBadSalary, err)
	}
	emp := Employee{Name: strings.TrimSpace(record[0]), Department: strings.TrimSpace(record[1]), Salary: salary}
	if err := validateEmployee(emp); err != nil {
		return Employee{}, err
	}
	return emp, nil
}

func validateEmployee(emp Employee) error {
	if emp.Name == "" {
		return ErrEmptyName
	}
	if emp.Department == "" {
		return ErrEmptyDepartment
	}
	if emp.Salary < 0 {
		return fmt.Errorf("%w: %d", ErrBadSalary, emp.Salary)
	}
	return nil
}

func sameHeader(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if strings.TrimSpace(strings.ToLower(got[i])) != want[i] {
			return false
		}
	}
	return true
}
```

Create `errors.go`:

```go
package employeecsv

import "errors"

var (
	ErrNilReader       = errors.New("reader must not be nil")
	ErrNilWriter       = errors.New("writer must not be nil")
	ErrBadHeader       = errors.New("csv header must be name,department,salary")
	ErrBadFieldCount   = errors.New("record has wrong field count")
	ErrEmptyName       = errors.New("employee name is required")
	ErrEmptyDepartment = errors.New("employee department is required")
	ErrBadSalary       = errors.New("salary must be a non-negative integer")
)
```

### Exercise 2: Test Parsing And Writing

Create `csv_test.go`:

```go
package employeecsv

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestReadEmployees(t *testing.T) {
	t.Parallel()

	input := "name,department,salary\nAlice,Engineering,95000\nBob,Marketing,72000\n"
	got, err := ReadEmployees(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "Alice" || got[1].Salary != 72000 {
		t.Fatalf("employees = %+v", got)
	}
}

func TestWriteEmployeesSortsAndQuotes(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := WriteEmployees(&out, []Employee{{Name: "Zoe", Department: "Research, Lab", Salary: 3}, {Name: "Ada", Department: "Engineering", Salary: 5}})
	if err != nil {
		t.Fatal(err)
	}
	want := "name,department,salary\nAda,Engineering,5\nZoe,\"Research, Lab\",3\n"
	if out.String() != want {
		t.Fatalf("csv = %q, want %q", out.String(), want)
	}
}

func TestReadEmployeesValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  error
	}{
		{name: "bad header", input: "x,y,z\n", want: ErrBadHeader},
		{name: "empty name", input: "name,department,salary\n,Engineering,1\n", want: ErrEmptyName},
		{name: "bad salary", input: "name,department,salary\nAda,Engineering,nope\n", want: ErrBadSalary},
	}
	for _, tt := range tests {
		_, err := ReadEmployees(strings.NewReader(tt.input))
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
}

func TestReadEmployeesRejectsNilReader(t *testing.T) {
	t.Parallel()

	_, err := ReadEmployees(nil)
	if !errors.Is(err, ErrNilReader) {
		t.Fatalf("err = %v, want ErrNilReader", err)
	}
}

func ExampleWriteEmployees() {
	var out bytes.Buffer
	_ = WriteEmployees(&out, []Employee{{Name: "Ada", Department: "Engineering", Salary: 5}})
	fmt.Print(out.String())
	// Output:
	// name,department,salary
	// Ada,Engineering,5
}
```

### Exercise 3: Add A Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"log"

	"example.com/employeecsv"
)

func main() {
	var out bytes.Buffer
	err := employeecsv.WriteEmployees(&out, []employeecsv.Employee{
		{Name: "Bob", Department: "Marketing", Salary: 72000},
		{Name: "Alice", Department: "Engineering", Salary: 95000},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(out.String())
}
```

## Common Mistakes

### Splitting Lines By Comma

Wrong: use `strings.Split(line, ",")` and break fields containing commas or quotes.

Fix: use `encoding/csv`, which implements CSV quoting rules.

### Forgetting Flush Errors

Wrong: call `cw.Flush()` and return nil.

Fix: call `cw.Flush()` and then `cw.Error()`.

### Trusting Column Order Without Checking

Wrong: parse row fields as name, department, and salary without validating the header.

Fix: reject unexpected headers with a sentinel error.

## Verification

Run this from `~/go-exercises/employeecsv`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

Add one more test for `WriteEmployees(nil, nil)` and assert `errors.Is(err, ErrNilWriter)`.

## Summary

- Use `encoding/csv` instead of splitting strings by comma.
- Validate headers before parsing rows.
- Check `csv.Writer.Error()` after `Flush`.
- Sort output when tests or downstream systems need deterministic CSV.

## What's Next

Next: [YAML Parsing](../09-yaml-parsing/09-yaml-parsing.md).

## Resources

- [encoding/csv package](https://pkg.go.dev/encoding/csv)
- [io.Reader interface](https://pkg.go.dev/io#Reader)
- [strconv.Atoi](https://pkg.go.dev/strconv#Atoi)
