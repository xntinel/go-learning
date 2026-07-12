# 3. Custom JSON Marshaler

Struct tags are a fine answer for "rename this field" and "omit when empty", but they cannot change the fundamental shape of a value. The `json.Marshaler` and `json.Unmarshaler` interfaces exist for cases where the JSON representation is genuinely different from the Go representation: enums that should be strings instead of integers, dates that should be `YYYY-MM-DD` without time or timezone, and durations that should be human strings like `"2h30m"` instead of nanoseconds. Implementing these interfaces on a type is the only way to take full control; the trade-off is that you become responsible for producing valid JSON and for matching the encoding on the way back in.

## Concepts

### The Two Interfaces

`encoding/json` calls the following methods when present:

```go
type Marshaler interface {
    MarshalJSON() ([]byte, error)
}

type Unmarshaler interface {
    UnmarshalJSON([]byte) error
}
```

`MarshalJSON` returns the JSON representation of the value. `UnmarshalJSON` parses the incoming bytes into the receiver. When the type has both methods, the encoder and decoder use them in place of the default rules. A few details from the spec matter:

- The receiver of `MarshalJSON` may be a value or a pointer. The encoder prefers a value method over a pointer method, but it will use a pointer method if the value is addressable.
- The receiver of `UnmarshalJSON` is almost always a pointer. The decoder must mutate the destination, and the only way to mutate through `json.Unmarshal(data, &v)` is with a pointer method.
- `MarshalJSON` must return valid JSON. If the value is a string, the bytes returned must include the surrounding quotes. The cheapest way to get this right is to delegate to `json.Marshal(value)` rather than build the bytes by hand.

### Enums As Strings

The most common use of `MarshalJSON` is to encode a typed integer as its string name. The standard pattern is two maps: a name-to-value map for decoding, and a value-to-name map for encoding. The sentinel-error pattern (`errors.Is(err, errUnknownPriority)`) is the test-friendly way to surface "you sent me a string I do not recognize" without leaking the internal constant set.

The alternative — encoding the integer directly with the `string` tag option — works, but a value like `3` in the wire format gives the reader no clue that it means "High". A string is self-describing and survives schema changes that add new constants in the middle of the range.

### Date And Time In Custom Formats

`time.Time` has a default JSON encoding of RFC 3339 (e.g. `2026-06-15T09:30:00Z`). For dates without time, a wrapper type with `MarshalJSON` and `UnmarshalJSON` is the only way to drop the time and timezone. The pattern:

- `MarshalJSON` returns `json.Marshal(t.Format("2006-01-02"))`. The `json.Marshal` call adds the surrounding quotes.
- `UnmarshalJSON` first unmarshals into a `string` (this strips the quotes), then calls `time.Parse("2006-01-02", s)`.
- The wrapper stores the parsed time as UTC to make the round-trip stable across machines in different timezones.

Always normalize on the way in (`.UTC()`) and on the way out (`.Format("2006-01-02")`). Without normalization, a value parsed from `"2026-06-15"` in a non-UTC location will serialize back as something different, and the round-trip test will fail mysteriously.

### Duration In Human-Readable Form

`time.Duration` is `int64` nanoseconds. Its default JSON encoding is the raw integer, which is unreadable and not portable to non-Go systems. The `time.Duration.String()` method produces values like `"2h30m0s"` and `time.ParseDuration` is the inverse. A `Duration` wrapper that delegates to both is the standard solution.

The lesson here is that wrapping a stdlib type to override its JSON behavior is a common and powerful technique. The wrapper has the same zero-value semantics (`Duration(0)` is `0s`), the same arithmetic (just convert with `time.Duration(d)`), and a wire format that is independent of the Go-specific representation.

### Composing Custom Types

Custom marshaler types compose naturally inside structs with no extra wiring. A field of type `Priority` inside `Task` uses `Priority.MarshalJSON` automatically; a field of type `DateOnly` uses `DateOnly.MarshalJSON`. The only constraint is that the type itself implements the interface, not the field's struct. This is why a constructor (`NewDateOnly`, `NewDuration`) is useful: it can take a `time.Time` or `time.Duration` argument and return the wrapper, hiding the conversion from the caller.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/18-encoding-json-xml-protobuf/03-custom-json-marshaler/03-custom-json-marshaler/internal/schedule
mkdir -p go-solutions/18-encoding-json-xml-protobuf/03-custom-json-marshaler/03-custom-json-marshaler/cmd/demo
cd go-solutions/18-encoding-json-xml-protobuf/03-custom-json-marshaler/03-custom-json-marshaler
```

This is a library plus a small demo. Verification is `go test -race`.

### Exercise 1: Priority As A String Enum

Create `internal/schedule/schedule.go`:

```go
package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Priority int

const (
	PriorityUnknown Priority = iota
	PriorityLow
	PriorityMedium
	PriorityHigh
	PriorityCritical
)

var (
	errUnknownPriority = errors.New("unknown priority")
)

var priorityNames = map[Priority]string{
	PriorityLow:      "Low",
	PriorityMedium:   "Medium",
	PriorityHigh:     "High",
	PriorityCritical: "Critical",
}

var priorityValues = map[string]Priority{
	"Low":      PriorityLow,
	"Medium":   PriorityMedium,
	"High":     PriorityHigh,
	"Critical": PriorityCritical,
}

func (p Priority) MarshalJSON() ([]byte, error) {
	name, ok := priorityNames[p]
	if !ok {
		return nil, fmt.Errorf("priority: %w: %d", errUnknownPriority, p)
	}
	return json.Marshal(name)
}

func (p *Priority) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	v, ok := priorityValues[s]
	if !ok {
		return fmt.Errorf("priority: %w: %q", errUnknownPriority, s)
	}
	*p = v
	return nil
}
```

`Priority` is `int`-typed, so it stays cheap to compare and switch on. The two maps live next to the type. The error wraps the sentinel with `%w` so tests can use `errors.Is`.

### Exercise 2: DateOnly And Duration Wrappers

Append to the same file:

```go
type DateOnly struct {
	time.Time
}

func NewDateOnly(t time.Time) DateOnly {
	return DateOnly{Time: t.UTC()}
}

func (d DateOnly) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Time.Format("2006-01-02"))
}

func (d *DateOnly) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return err
	}
	d.Time = t.UTC()
	return nil
}

type Duration time.Duration

func NewDuration(d time.Duration) Duration {
	return Duration(d)
}

func (d Duration) Standard() time.Duration {
	return time.Duration(d)
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}
```

`DateOnly` embeds `time.Time` so it inherits its methods (e.g. `After`, `Before`) without writing them by hand. `Duration` is a defined type over `time.Duration` so the two cannot be mixed without an explicit conversion.

### Exercise 3: The Task Type And A Constructor

Append:

```go
type Task struct {
	ID       int      `json:"id"`
	Title    string   `json:"title"`
	Priority Priority `json:"priority"`
	Deadline DateOnly `json:"deadline"`
	Estimate Duration `json:"estimate"`
}

func NewTask(id int, title string, pri Priority, deadline time.Time, estimate time.Duration) Task {
	return Task{
		ID:       id,
		Title:    title,
		Priority: pri,
		Deadline: NewDateOnly(deadline),
		Estimate: NewDuration(estimate),
	}
}
```

`NewTask` is the only place that needs to know about the conversion between `time.Time` and `DateOnly`, and between `time.Duration` and `Duration`. The rest of the program works with the wrapper types.

### Exercise 4: Test The Round-Trip And The Failure Modes

Create `internal/schedule/schedule_test.go`:

```go
package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestPriorityMarshalAsString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   Priority
		want string
	}{
		{PriorityLow, `"Low"`},
		{PriorityMedium, `"Medium"`},
		{PriorityHigh, `"High"`},
		{PriorityCritical, `"Critical"`},
	}
	for _, c := range cases {
		data, err := json.Marshal(c.in)
		if err != nil {
			t.Errorf("Marshal(%v) error: %v", c.in, err)
			continue
		}
		if string(data) != c.want {
			t.Errorf("Marshal(%v) = %s, want %s", c.in, data, c.want)
		}
	}
}

func TestPriorityUnmarshalFromString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want Priority
	}{
		{`"Low"`, PriorityLow},
		{`"Medium"`, PriorityMedium},
		{`"High"`, PriorityHigh},
		{`"Critical"`, PriorityCritical},
	}
	for _, c := range cases {
		var p Priority
		if err := json.Unmarshal([]byte(c.in), &p); err != nil {
			t.Errorf("Unmarshal(%s) error: %v", c.in, err)
			continue
		}
		if p != c.want {
			t.Errorf("Unmarshal(%s) = %v, want %v", c.in, p, c.want)
		}
	}
}

func TestPriorityRejectsUnknownString(t *testing.T) {
	t.Parallel()

	var p Priority
	if err := json.Unmarshal([]byte(`"bogus"`), &p); err == nil {
		t.Fatal("expected error, got nil")
	} else if !errors.Is(err, errUnknownPriority) {
		t.Fatalf("err = %v, want errUnknownPriority", err)
	}
}

func TestPriorityRejectsUnknownInt(t *testing.T) {
	t.Parallel()

	_, err := json.Marshal(Priority(99))
	if err == nil {
		t.Fatal("expected error, got nil")
	} else if !errors.Is(err, errUnknownPriority) {
		t.Fatalf("err = %v, want errUnknownPriority", err)
	}
}

func TestDateOnlyMarshalAsISO(t *testing.T) {
	t.Parallel()

	d := NewDateOnly(time.Date(2026, time.June, 15, 9, 30, 0, 0, time.UTC))
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	if string(data) != `"2026-06-15"` {
		t.Fatalf("got %s, want \"2026-06-15\"", data)
	}
}

func TestDateOnlyUnmarshalFromISO(t *testing.T) {
	t.Parallel()

	var d DateOnly
	if err := json.Unmarshal([]byte(`"2026-06-15"`), &d); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if y, m, day := d.Time.Date(); y != 2026 || m != time.June || day != 15 {
		t.Fatalf("got %v, want 2026-06-15", d.Time)
	}
}

func TestDurationMarshalAsString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   time.Duration
		want string
	}{
		{2*time.Hour + 30*time.Minute, `"2h30m0s"`},
		{45 * time.Minute, `"45m0s"`},
		{0, `"0s"`},
		{-1 * time.Second, `"-1s"`},
	}
	for _, c := range cases {
		d := NewDuration(c.in)
		data, err := json.Marshal(d)
		if err != nil {
			t.Errorf("Marshal(%v) error: %v", c.in, err)
			continue
		}
		if string(data) != c.want {
			t.Errorf("Marshal(%v) = %s, want %s", c.in, data, c.want)
		}
	}
}

func TestDurationUnmarshalFromString(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want time.Duration
	}{
		{`"2h30m"`, 2*time.Hour + 30*time.Minute},
		{`"45m"`, 45 * time.Minute},
		{`"500ms"`, 500 * time.Millisecond},
	}
	for _, c := range cases {
		var d Duration
		if err := json.Unmarshal([]byte(c.in), &d); err != nil {
			t.Errorf("Unmarshal(%s) error: %v", c.in, err)
			continue
		}
		if d.Standard() != c.want {
			t.Errorf("Unmarshal(%s) = %v, want %v", c.in, d.Standard(), c.want)
		}
	}
}

func TestTaskRoundTrip(t *testing.T) {
	t.Parallel()

	deadline := time.Date(2026, time.June, 15, 0, 0, 0, 0, time.UTC)
	in := NewTask(1, "Write docs", PriorityHigh, deadline, 2*time.Hour+30*time.Minute)

	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent error: %v", err)
	}

	want := "{\n" +
		"  \"id\": 1,\n" +
		"  \"title\": \"Write docs\",\n" +
		"  \"priority\": \"High\",\n" +
		"  \"deadline\": \"2026-06-15\",\n" +
		"  \"estimate\": \"2h30m0s\"\n" +
		"}"
	if string(data) != want {
		t.Fatalf("MarshalIndent mismatch\n got: %s\nwant: %s", data, want)
	}

	var out Task
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if out.ID != in.ID {
		t.Errorf("ID = %d, want %d", out.ID, in.ID)
	}
	if out.Title != in.Title {
		t.Errorf("Title = %q, want %q", out.Title, in.Title)
	}
	if out.Priority != in.Priority {
		t.Errorf("Priority = %v, want %v", out.Priority, in.Priority)
	}
	if !out.Deadline.Time.Equal(in.Deadline.Time) {
		t.Errorf("Deadline = %v, want %v", out.Deadline.Time, in.Deadline.Time)
	}
	if out.Estimate.Standard() != in.Estimate.Standard() {
		t.Errorf("Estimate = %v, want %v", out.Estimate.Standard(), in.Estimate.Standard())
	}
}

func TestTasksRoundTrip(t *testing.T) {
	t.Parallel()

	in := []Task{
		NewTask(1, "Write docs", PriorityHigh, time.Date(2026, time.June, 15, 0, 0, 0, 0, time.UTC), 2*time.Hour+30*time.Minute),
		NewTask(2, "Fix bug", PriorityCritical, time.Date(2026, time.March, 20, 0, 0, 0, 0, time.UTC), 45*time.Minute),
	}

	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent error: %v", err)
	}

	want := "[\n" +
		"  {\n" +
		"    \"id\": 1,\n" +
		"    \"title\": \"Write docs\",\n" +
		"    \"priority\": \"High\",\n" +
		"    \"deadline\": \"2026-06-15\",\n" +
		"    \"estimate\": \"2h30m0s\"\n" +
		"  },\n" +
		"  {\n" +
		"    \"id\": 2,\n" +
		"    \"title\": \"Fix bug\",\n" +
		"    \"priority\": \"Critical\",\n" +
		"    \"deadline\": \"2026-03-20\",\n" +
		"    \"estimate\": \"45m0s\"\n" +
		"  }\n" +
		"]"
	if string(data) != want {
		t.Fatalf("MarshalIndent mismatch\n got: %s\nwant: %s", data, want)
	}

	var out []Task
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}
	for i := range in {
		if out[i].ID != in[i].ID || out[i].Title != in[i].Title || out[i].Priority != in[i].Priority {
			t.Errorf("index %d: in=%+v out=%+v", i, in[i], out[i])
		}
	}
}

func ExampleNewTask() {
	task := NewTask(
		1,
		"Write docs",
		PriorityHigh,
		time.Date(2026, time.June, 15, 0, 0, 0, 0, time.UTC),
		2*time.Hour+30*time.Minute,
	)
	data, _ := json.MarshalIndent(task, "", "  ")
	fmt.Println(string(data))
	// Output:
	// {
	//   "id": 1,
	//   "title": "Write docs",
	//   "priority": "High",
	//   "deadline": "2026-06-15",
	//   "estimate": "2h30m0s"
	// }
}
```

`TestPriorityRejectsUnknownString` and `TestPriorityRejectsUnknownInt` are the most important tests in the suite. They pin the contract that an unknown value is a real error, not a silent zero. The `errors.Is` check makes the assertion exact, not just "an error occurred".

### Exercise 5: A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"example.com/schedule/internal/schedule"
)

func main() {
	tasks := []schedule.Task{
		schedule.NewTask(1, "Write docs", schedule.PriorityHigh, time.Date(2026, time.June, 15, 0, 0, 0, 0, time.UTC), 2*time.Hour+30*time.Minute),
		schedule.NewTask(2, "Fix bug", schedule.PriorityCritical, time.Date(2026, time.March, 20, 0, 0, 0, 0, time.UTC), 45*time.Minute),
	}

	data, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(data))

	fmt.Println()
	fmt.Println("Decoded tasks:")
	var decoded []schedule.Task
	if err := json.Unmarshal(data, &decoded); err != nil {
		log.Fatal(err)
	}
	for _, t := range decoded {
		fmt.Printf("  %+v\n", t)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Your turn: add a `TestDateOnlyRejectsBadFormat` test that unmarshals `"2026/06/15"` and `"not-a-date"` and asserts that both return a non-nil error. The test pins the contract that the date format is strict.

## Common Mistakes

### Forgetting To Include Quotes When Returning A String

Wrong: a developer writes `return []byte("hello"), nil` from `MarshalJSON`. The encoder writes the unquoted string into the document; the result is invalid JSON, and any subsequent `Unmarshal` on the same data fails to parse.

Fix: delegate to `json.Marshal`. `return json.Marshal("hello")` produces `[]byte("\"hello\""), nil` with the quotes included. Only build bytes by hand when the format is genuinely custom (e.g. a number-shaped string, a custom date format, an encoded byte sequence).

### Using A Value Receiver On `UnmarshalJSON`

Wrong: `func (p Priority) UnmarshalJSON(data []byte) error`. The decoder gets a copy of the receiver, mutates the copy, and discards it. The original value in the parent struct stays at zero.

Fix: `func (p *Priority) UnmarshalJSON(data []byte) error`. The pointer receiver is mandatory for any unmarshaler that needs to mutate state.

### Encoding An Empty Map As `null` When The Receiver Expects `{}`

Wrong: a `MarshalJSON` method returns `null` for the zero value of a wrapper type. The receiver expects an object, and `null` does not parse back into a `map[string]int`.

Fix: in the wrapper's `MarshalJSON`, distinguish "the value is the zero value" from "the value is a non-zero but legitimately empty structure". For maps, an empty map encodes as `{}` automatically; for pointers, decide whether `nil` means "absent" (use `omitempty` and a pointer) or "empty" (use a non-pointer).

### Forgetting To Normalize Timezones On Parse

Wrong: `UnmarshalJSON` calls `time.Parse("2006-01-02", s)` and stores the result. The parsed time is in UTC, but tests on machines in other timezones compare the result with a `time.Date(2026, time.June, 15, 0, 0, 0, 0, time.Local)`, and the comparison fails because the locations differ.

Fix: normalize on parse: `d.Time = t.UTC()`. This makes the in-memory representation timezone-stable and the round-trip deterministic.

## Verification

From `~/go-exercises/schedule`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must pass. The `ExampleNewTask` test in the suite is auto-verified under `go test` because it carries an `// Output:` comment.

## Summary

- `json.Marshaler` and `json.Unmarshaler` are the only way to take full control of a type's JSON representation; struct tags are not enough.
- `MarshalJSON` must return valid JSON; the cheapest way to get a quoted string right is to delegate to `json.Marshal`.
- `UnmarshalJSON` must use a pointer receiver so it can mutate the destination.
- The two-map pattern (name-to-value and value-to-name) is the standard way to encode a typed integer as a string enum.
- Wrapper types over `time.Time` and `time.Duration` are the right way to encode a custom date or duration format; normalize timezones on parse for a stable round-trip.
- Sentinel errors wrapped with `%w` let tests assert exact failure modes with `errors.Is`.

## What's Next

Next: [Streaming JSON](../04-streaming-json/04-streaming-json.md).

## Resources

- [encoding/json package](https://pkg.go.dev/encoding/json)
- [JSON and Go (Go blog)](https://go.dev/blog/json)
- [time package](https://pkg.go.dev/time)
