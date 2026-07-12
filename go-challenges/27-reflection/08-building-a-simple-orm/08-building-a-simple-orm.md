# 8. Building a Simple ORM

A reflection-based ORM bridges Go's static type system with SQL's dynamic nature. When you call `orm.Find(&users, "age > ?", 25)`, the library must inspect the type `*[]User` at runtime, derive SELECT columns from struct tags, execute the query, iterate over rows, and scan each row back into a new `User` value — all without knowing the type at compile time. This lesson builds a minimal but complete ORM that does exactly that, using `database/sql` as the transport and `sync.Map` to cache per-type metadata.

```text
orm/
  go.mod
  schema.go
  orm.go
  orm_test.go
  orm_online_test.go
  cmd/demo/main.go
```

The default test path (`orm_test.go`) is hermetic: it exercises schema extraction with no external modules. Tests that require a real SQL driver live in `orm_online_test.go` behind `//go:build online` and are run separately when network access is available.

## Concepts

### Schema Extraction as the Central Cache

Every ORM operation starts the same way: "what are the columns, which is the primary key, what is the table name?" Computing this from struct tags on every query is wasteful. The right approach is a `sync.Map` keyed by `reflect.Type`. The first call for a given type pays the reflection cost; every subsequent call returns the cached result in O(1).

```
getSchema(model any) -> *TableSchema
  if schemaCache[reflect.TypeOf(model)] exists -> return it
  else -> buildSchema(reflect.TypeOf(model)) -> cache and return
```

### Struct Tag Parsing

The `db` tag drives the column name and constraints:

```
db:"id,pk,autoincrement"   -> primary key, auto-increment
db:"user_name"             -> column name override
db:"bio,nullable"          -> nullable column
db:"-"                     -> skip this field
```

The first comma-separated element is the column name (or the field name if absent). The remaining elements are option flags.

### Row Scanning with Reflection

`sql.Rows.Scan` accepts `...any` where each argument must be a pointer to the destination variable. For a reflected struct, you build this slice with:

```go
ptrs[i] = structValue.Field(col.FieldIndex).Addr().Interface()
```

`Addr()` gives you a `reflect.Value` holding a pointer to the field; `Interface()` extracts the `*T` as `any`. This is the key insight that makes generic row scanning possible without code generation.

### Slice Destination for Find

`Find` receives `*[]User`. To iterate and append:

```go
slicePtr  := reflect.ValueOf(dest)   // *[]User
sliceVal  := slicePtr.Elem()          // []User
elemType  := sliceVal.Type().Elem()   // User
elem      := reflect.New(elemType).Elem()  // new User (settable)
// ... scan row into elem ...
sliceVal   = reflect.Append(sliceVal, elem)
slicePtr.Elem().Set(sliceVal)          // write back
```

### Go Type to SQL Type Mapping

The mapping is kind-based, with a special case for `time.Time`:

| Go type | SQL type |
| --- | --- |
| `string` | TEXT |
| `int`, `int32`, `int64` | INTEGER |
| `float32`, `float64` | REAL |
| `bool` | BOOLEAN |
| `time.Time` | TIMESTAMP |

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/27-reflection/08-building-a-simple-orm/08-building-a-simple-orm/cmd/demo
cd go-solutions/27-reflection/08-building-a-simple-orm/08-building-a-simple-orm
```

The schema and ORM logic compile with no external modules. Tests that require a SQL driver are gated behind `//go:build online`.

### Exercise 1: Schema Types and Cache

Create `schema.go`:

```go
package orm

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ColumnInfo describes a single struct field's mapping to a SQL column.
type ColumnInfo struct {
	FieldIndex    int
	ColumnName    string
	SQLType       string
	IsPK          bool
	AutoIncrement bool
	Nullable      bool
	Skip          bool
}

// TableSchema holds the complete reflected metadata for one struct type.
type TableSchema struct {
	TableName string
	Columns   []ColumnInfo
	PKIndex   int // index into Columns; -1 if no PK defined
}

var schemaCache sync.Map // reflect.Type -> *TableSchema

// GetSchema returns the cached schema for the type of model, building it on
// first access. model may be a value or a pointer to a struct.
func GetSchema(model any) (*TableSchema, error) {
	t := reflect.TypeOf(model)
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil, fmt.Errorf("orm: expected struct, got %v", t.Kind())
	}
	if v, ok := schemaCache.Load(t); ok {
		return v.(*TableSchema), nil
	}
	schema := buildSchema(t)
	schemaCache.Store(t, schema)
	return schema, nil
}

func buildSchema(t reflect.Type) *TableSchema {
	schema := &TableSchema{
		TableName: toSnake(t.Name()) + "s",
		PKIndex:   -1,
	}
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		col := parseColumn(sf, i)
		if col.Skip {
			continue
		}
		if col.IsPK {
			schema.PKIndex = len(schema.Columns)
		}
		schema.Columns = append(schema.Columns, col)
	}
	return schema
}

func parseColumn(sf reflect.StructField, idx int) ColumnInfo {
	col := ColumnInfo{
		FieldIndex: idx,
		ColumnName: toSnake(sf.Name),
		SQLType:    goTypeToSQL(sf.Type),
	}
	tag := sf.Tag.Get("db")
	if tag == "-" {
		col.Skip = true
		return col
	}
	parts := strings.Split(tag, ",")
	if parts[0] != "" {
		col.ColumnName = parts[0]
	}
	for _, opt := range parts[1:] {
		switch strings.TrimSpace(opt) {
		case "pk":
			col.IsPK = true
		case "autoincrement":
			col.AutoIncrement = true
		case "nullable":
			col.Nullable = true
		}
	}
	return col
}

func goTypeToSQL(t reflect.Type) string {
	if t == reflect.TypeOf(time.Time{}) {
		return "TIMESTAMP"
	}
	switch t.Kind() {
	case reflect.String:
		return "TEXT"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "INTEGER"
	case reflect.Float32, reflect.Float64:
		return "REAL"
	case reflect.Bool:
		return "BOOLEAN"
	default:
		return "BLOB"
	}
}

// toSnake converts CamelCase to snake_case.
func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) && i > 0 {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
```

### Exercise 2: ORM Operations

Create `orm.go`:

```go
package orm

import (
	"database/sql"
	"fmt"
	"reflect"
	"strings"
)

// CreateTable generates and executes a CREATE TABLE IF NOT EXISTS statement
// derived from the struct tags of model.
func CreateTable(db *sql.DB, model any) error {
	schema, err := GetSchema(model)
	if err != nil {
		return err
	}

	var cols []string
	for _, col := range schema.Columns {
		def := fmt.Sprintf("%s %s", col.ColumnName, col.SQLType)
		if col.IsPK {
			def += " PRIMARY KEY"
		}
		if col.AutoIncrement {
			def += " AUTOINCREMENT"
		}
		if !col.Nullable && !col.IsPK {
			def += " NOT NULL DEFAULT ''"
		}
		cols = append(cols, def)
	}

	q := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s (%s)",
		schema.TableName, strings.Join(cols, ", "))
	_, err = db.Exec(q)
	return err
}

// Insert inserts model into its table. If the PK column is autoincrement,
// the generated ID is written back into the struct via reflection.
// model must be a pointer to a struct.
func Insert(db *sql.DB, model any) error {
	schema, err := GetSchema(model)
	if err != nil {
		return err
	}
	rv := reflect.ValueOf(model)
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}

	var colNames []string
	var placeholders []string
	var args []any
	for _, col := range schema.Columns {
		if col.AutoIncrement {
			continue // let the DB assign the PK
		}
		colNames = append(colNames, col.ColumnName)
		placeholders = append(placeholders, "?")
		args = append(args, rv.Field(col.FieldIndex).Interface())
	}

	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		schema.TableName,
		strings.Join(colNames, ", "),
		strings.Join(placeholders, ", "))

	res, err := db.Exec(q, args...)
	if err != nil {
		return err
	}

	// Write back the generated PK if the column is autoincrement.
	if schema.PKIndex >= 0 && schema.Columns[schema.PKIndex].AutoIncrement {
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		pkField := rv.Field(schema.Columns[schema.PKIndex].FieldIndex)
		if pkField.CanSet() {
			pkField.SetInt(id)
		}
	}
	return nil
}

// Find queries the table for rows matching the optional WHERE clause and scans
// them into dest, which must be a pointer to a slice of structs.
func Find(db *sql.DB, dest any, where string, args ...any) error {
	slicePtr := reflect.ValueOf(dest)
	if slicePtr.Kind() != reflect.Ptr || slicePtr.Elem().Kind() != reflect.Slice {
		return fmt.Errorf("orm: Find: dest must be a pointer to a slice")
	}
	sliceVal := slicePtr.Elem()
	elemType := sliceVal.Type().Elem()

	schema, err := GetSchema(reflect.New(elemType).Interface())
	if err != nil {
		return err
	}

	colNames := make([]string, len(schema.Columns))
	for i, col := range schema.Columns {
		colNames[i] = col.ColumnName
	}

	q := fmt.Sprintf("SELECT %s FROM %s", strings.Join(colNames, ", "), schema.TableName)
	if where != "" {
		q += " WHERE " + where
	}

	rows, err := db.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		elem := reflect.New(elemType).Elem()
		if err := scanRow(rows, schema, elem); err != nil {
			return err
		}
		sliceVal = reflect.Append(sliceVal, elem)
	}
	slicePtr.Elem().Set(sliceVal)
	return rows.Err()
}

// FindOne queries the table for a single row matching where and scans it into dest,
// which must be a pointer to a struct.
func FindOne(db *sql.DB, dest any, where string, args ...any) error {
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Ptr || rv.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("orm: FindOne: dest must be a pointer to a struct")
	}

	schema, err := GetSchema(dest)
	if err != nil {
		return err
	}

	colNames := make([]string, len(schema.Columns))
	for i, col := range schema.Columns {
		colNames[i] = col.ColumnName
	}

	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s LIMIT 1",
		strings.Join(colNames, ", "), schema.TableName, where)

	rows, err := db.Query(q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	if !rows.Next() {
		return sql.ErrNoRows
	}
	return scanRow(rows, schema, rv.Elem())
}

// scanRow reads the current row into structVal using the column metadata in schema.
func scanRow(rows *sql.Rows, schema *TableSchema, structVal reflect.Value) error {
	ptrs := make([]any, len(schema.Columns))
	for i, col := range schema.Columns {
		ptrs[i] = structVal.Field(col.FieldIndex).Addr().Interface()
	}
	return rows.Scan(ptrs...)
}
```

### Exercise 3: Hermetic Tests

Create `orm_test.go`. These tests exercise schema extraction with no external modules:

```go
package orm_test

import (
	"fmt"
	"testing"
	"time"

	"example.com/orm"
)

type Product struct {
	ID    int64  `db:"id,pk,autoincrement"`
	Name  string `db:"name"`
	Price int    `db:"price"`
	Stock int    `db:"stock"`
}

type WithSkip struct {
	ID     int64  `db:"id,pk,autoincrement"`
	Name   string `db:"name"`
	Secret string `db:"-"`
}

type WithTime struct {
	ID        int64     `db:"id,pk,autoincrement"`
	CreatedAt time.Time `db:"created_at"`
}

func TestGetSchemaTableName(t *testing.T) {
	t.Parallel()

	s, err := orm.GetSchema(Product{})
	if err != nil {
		t.Fatal(err)
	}
	if s.TableName != "products" {
		t.Fatalf("TableName = %q, want %q", s.TableName, "products")
	}
}

func TestGetSchemaColumns(t *testing.T) {
	t.Parallel()

	s, err := orm.GetSchema(Product{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Columns) != 4 {
		t.Fatalf("got %d columns, want 4", len(s.Columns))
	}
	if s.Columns[0].ColumnName != "id" || !s.Columns[0].IsPK {
		t.Errorf("column[0] = %+v, want id/pk", s.Columns[0])
	}
	if s.Columns[1].ColumnName != "name" || s.Columns[1].SQLType != "TEXT" {
		t.Errorf("column[1] = %+v, want name/TEXT", s.Columns[1])
	}
}

func TestGetSchemaSkipsField(t *testing.T) {
	t.Parallel()

	s, err := orm.GetSchema(WithSkip{})
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range s.Columns {
		if col.ColumnName == "secret" {
			t.Error("field tagged db:\"-\" must be skipped")
		}
	}
	if len(s.Columns) != 2 {
		t.Fatalf("got %d columns, want 2", len(s.Columns))
	}
}

func TestGetSchemaTimestampType(t *testing.T) {
	t.Parallel()

	s, err := orm.GetSchema(WithTime{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, col := range s.Columns {
		if col.ColumnName == "created_at" {
			found = true
			if col.SQLType != "TIMESTAMP" {
				t.Errorf("created_at SQLType = %q, want TIMESTAMP", col.SQLType)
			}
		}
	}
	if !found {
		t.Error("column created_at not found")
	}
}

func TestGetSchemaErrorOnNonStruct(t *testing.T) {
	t.Parallel()

	_, err := orm.GetSchema(42)
	if err == nil {
		t.Fatal("expected error for non-struct input, got nil")
	}
}

func TestGetSchemaCaching(t *testing.T) {
	t.Parallel()

	s1, err := orm.GetSchema(Product{})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := orm.GetSchema(Product{})
	if err != nil {
		t.Fatal(err)
	}
	// Same pointer means the cache was used.
	if s1 != s2 {
		t.Error("GetSchema should return the same *TableSchema on repeated calls")
	}
}

func TestGetSchemaAutoincrement(t *testing.T) {
	t.Parallel()

	s, err := orm.GetSchema(Product{})
	if err != nil {
		t.Fatal(err)
	}
	if s.PKIndex < 0 {
		t.Fatal("PKIndex should be >= 0")
	}
	pk := s.Columns[s.PKIndex]
	if !pk.AutoIncrement {
		t.Errorf("PK column AutoIncrement = false, want true")
	}
}

func ExampleGetSchema() {
	type Item struct {
		ID    int64  `db:"id,pk,autoincrement"`
		Label string `db:"label"`
	}
	s, err := orm.GetSchema(Item{})
	if err != nil {
		panic(err)
	}
	fmt.Println(s.TableName)
	fmt.Println(len(s.Columns), "columns")
	fmt.Println(s.Columns[0].ColumnName, s.Columns[0].IsPK)
	// Output:
	// items
	// 2 columns
	// id true
}
```

### Exercise 4: Online Tests (SQL driver required)

Create `orm_online_test.go`. These tests require a registered SQL driver and are excluded from the default `go test` run:

```go
//go:build online

package orm_test

import (
	"database/sql"
	"testing"

	"example.com/orm"
	_ "modernc.org/sqlite"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestCreateTableAndInsert(t *testing.T) {
	t.Parallel()

	db := openDB(t)
	if err := orm.CreateTable(db, Product{}); err != nil {
		t.Fatal("CreateTable:", err)
	}

	p := Product{Name: "Widget", Price: 999, Stock: 50}
	if err := orm.Insert(db, &p); err != nil {
		t.Fatal("Insert:", err)
	}
	if p.ID == 0 {
		t.Fatal("expected autoincrement ID to be written back")
	}
}

func TestFindAll(t *testing.T) {
	t.Parallel()

	db := openDB(t)
	orm.CreateTable(db, Product{})

	for _, p := range []Product{
		{Name: "A", Price: 100, Stock: 10},
		{Name: "B", Price: 200, Stock: 5},
		{Name: "C", Price: 300, Stock: 0},
	} {
		if err := orm.Insert(db, &p); err != nil {
			t.Fatal(err)
		}
	}

	var products []Product
	if err := orm.Find(db, &products, ""); err != nil {
		t.Fatal(err)
	}
	if len(products) != 3 {
		t.Fatalf("Find returned %d products, want 3", len(products))
	}
}

func TestFindWithWhere(t *testing.T) {
	t.Parallel()

	db := openDB(t)
	orm.CreateTable(db, Product{})

	for _, p := range []Product{
		{Name: "Cheap", Price: 10, Stock: 5},
		{Name: "Expensive", Price: 5000, Stock: 2},
	} {
		orm.Insert(db, &p)
	}

	var expensive []Product
	if err := orm.Find(db, &expensive, "price > ?", 100); err != nil {
		t.Fatal(err)
	}
	if len(expensive) != 1 || expensive[0].Name != "Expensive" {
		t.Fatalf("Find with WHERE returned %+v", expensive)
	}
}

func TestFindOneNotFound(t *testing.T) {
	t.Parallel()

	db := openDB(t)
	orm.CreateTable(db, Product{})

	var p Product
	err := orm.FindOne(db, &p, "id = ?", 9999)
	if err != sql.ErrNoRows {
		t.Fatalf("err = %v, want sql.ErrNoRows", err)
	}
}
```

To run the online tests after `go get modernc.org/sqlite`:

```bash
go test -tags online -count=1 -race ./...
```

### Exercise 5: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"time"

	"example.com/orm"
)

type User struct {
	ID        int64     `db:"id,pk,autoincrement"`
	Name      string    `db:"name"`
	Email     string    `db:"email"`
	Age       int       `db:"age"`
	CreatedAt time.Time `db:"created_at"`
}

func main() {
	schema, err := orm.GetSchema(User{})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("table: %s\n", schema.TableName)
	fmt.Printf("columns (%d):\n", len(schema.Columns))
	for _, col := range schema.Columns {
		pk := ""
		if col.IsPK {
			pk = " [PK]"
		}
		fmt.Printf("  %-15s %-12s%s\n", col.ColumnName, col.SQLType, pk)
	}
}
```

Run with `go run ./cmd/demo`.

## Common Mistakes

### Building the Column Pointer Slice Wrong

Wrong: passing `structVal.Field(i).Interface()` to `rows.Scan` instead of a pointer.

What happens: `Scan` panics or returns an error because it cannot write into a non-pointer destination.

Fix: use `structVal.Field(i).Addr().Interface()` to pass a `*T` for each column.

### Not Caching Schema Per Type

Wrong: calling `reflect.TypeOf(model)` and iterating all fields on every query.

What happens: for a service handling 10,000 requests per second with 20-field structs, you burn CPU re-deriving the same schema information on every call.

Fix: use a `sync.Map` keyed by `reflect.Type`; pay the reflection cost once and retrieve the `*TableSchema` in O(1) on every subsequent call.

### String-Interpolating Values Into SQL

Wrong: building `"SELECT * FROM users WHERE name = '" + name + "'"`.

What happens: SQL injection; the query breaks on names containing apostrophes.

Fix: always use parameterized queries with `?` placeholders and pass values as `args...` to `db.Query` or `db.Exec`.

## Verification

From `~/go-exercises/orm`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass with no external modules. The default test run (`orm_test.go`) exercises schema extraction entirely in-process. To run the SQL-driver tests after fetching `modernc.org/sqlite`, add `-tags online`.

## Summary

- Cache struct metadata in a `sync.Map` keyed by `reflect.Type`; pay the reflection cost once per type.
- Build SELECT column lists and INSERT placeholders from `TableSchema.Columns`.
- Scan rows into structs with `rows.Scan(ptrs...)` where each `ptr` is `field.Addr().Interface()`.
- Populate a `*[]T` destination in `Find` by reflecting on the element type and appending with `reflect.Append`.
- Gate any external-driver tests behind `//go:build online` so the default path stays hermetic.

## What's Next

Next: [Code Generation vs Reflection](../09-code-generation-vs-reflection/09-code-generation-vs-reflection.md).

## Resources

- [database/sql package](https://pkg.go.dev/database/sql)
- [database/sql tutorial](https://go.dev/doc/database/)
- [reflect.Value.Addr](https://pkg.go.dev/reflect#Value.Addr)
- [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)
- [GORM schema parser source](https://github.com/go-gorm/gorm/tree/master/schema)
