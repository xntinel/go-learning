# 6. Progress Bars and Spinners

Progress output is useful only when it does not break scripts or hide errors. A CLI should make progress optional, render it to stderr, and keep the work itself testable without timers or terminal animation.

## Concepts

### Stderr Is for Progress

Progress indicators are diagnostics. Send them to stderr so stdout remains machine-readable data that can be piped to another command.

### Separate Rendering From Work

Do not sleep in tests to prove a progress bar works. Return progress events from deterministic work and test the renderer with fixed inputs.

### Carriage Return Is a Terminal Detail

`\r` rewrites one terminal line. It is useful interactively, but logs and CI often prefer one line per update. A `-progress` flag lets users decide.

### Clamp Percentages

Real progress can receive duplicate, late, or over-total updates. Clamp current values to `[0,total]` so output stays sane.

## Exercises

Set up the module:

```bash
go mod edit -go=1.26
```

Confirm `go.mod` contains `go 1.26` before writing code.

### Exercise 1: Render Deterministic Progress

Create `main.go`:

```go
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

var ErrInvalidTotal = errors.New("total must be at least 1")

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("copy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	total := fs.Int("total", 3, "number of files")
	showProgress := fs.Bool("progress", true, "write progress to stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *total < 1 {
		return fmt.Errorf("%w: got %d", ErrInvalidTotal, *total)
	}
	for i := 1; i <= *total; i++ {
		if *showProgress {
			fmt.Fprintln(stderr, renderProgress(i, *total, 20))
		}
	}
	fmt.Fprintf(stdout, "copied %d files\n", *total)
	return nil
}

func renderProgress(current, total, width int) string {
	if current < 0 {
		current = 0
	}
	if current > total {
		current = total
	}
	filled := width * current / total
	bar := strings.Repeat("#", filled) + strings.Repeat("-", width-filled)
	percent := 100 * current / total
	return fmt.Sprintf("[%s] %3d%% (%d/%d)", bar, percent, current, total)
}
```

### Exercise 2: Test Output and Validation

Create `main_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestRenderProgress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		current int
		total   int
		want    string
	}{
		{"start", 0, 4, "[----------]   0% (0/4)"},
		{"half", 2, 4, "[#####-----]  50% (2/4)"},
		{"clamped", 8, 4, "[##########] 100% (4/4)"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := renderProgress(tc.current, tc.total, 10); got != tc.want {
				t.Fatalf("renderProgress() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunWritesProgressToStderr(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if err := run([]string{"-total=2"}, &stdout, &stderr); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if got, want := stdout.String(), "copied 2 files\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() == 0 {
		t.Fatal("stderr is empty, want progress output")
	}
}

func TestRunRejectsBadTotal(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	err := run([]string{"-total=0"}, &stdout, &stderr)
	if !errors.Is(err, ErrInvalidTotal) {
		t.Fatalf("err = %v, want ErrInvalidTotal", err)
	}
}

func Example_renderProgress() {
	fmt.Print(renderProgress(3, 4, 8))
	// Output: [######--]  75% (3/4)
}
```

### Exercise 3: Add Quiet Mode

Add `-quiet` so the command prints neither progress nor success text, while still returning errors. Test that stdout and stderr stay empty on success.

## Common Mistakes

### Writing Progress to Stdout

Wrong: progress frames go to stdout before JSON or data output.

What happens: pipelines receive invalid data.

Fix: progress and status messages go to stderr.

### Testing With Sleeps

Wrong: tests wait for animation timing.

What happens: tests are slow and flaky.

Fix: test deterministic render functions and event handling.

### Forgetting Bounds

Wrong: rendering `125%` after a duplicate final event.

What happens: users see impossible progress.

Fix: clamp current progress before computing the bar.

## Verification

From `~/go-exercises/progress-cli`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add quiet mode and rerun the same commands.

## Summary

- Write progress to stderr, not stdout.
- Keep progress rendering deterministic and testable.
- Clamp out-of-range updates.
- Use flags to disable progress in automation.

## What's Next

Next: [Output Formatting](../07-output-formatting/07-output-formatting.md).

## Resources

- [Package fmt](https://pkg.go.dev/fmt)
- [Package strings](https://pkg.go.dev/strings)
- [Package flag](https://pkg.go.dev/flag)
