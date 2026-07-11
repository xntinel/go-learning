# Exercise 8: Remote Debugging — Headless Server plus dlv connect

Debugging a process inside a Docker container or on a remote host uses a
client/server split: the target runs under a headless Delve backend that listens
on a TCP socket, and a separate `dlv connect` client drives it over the wire. This
is also the exact backend editors speak to. This module builds a small target and
walks the headless-plus-connect workflow.

This module is fully self-contained: its own `go mod init`, demo, and test.

## What you'll build

```text
remotedbg/                 independent module: example.com/remotedbg
  go.mod                   go 1.24
  seq/
    seq.go                 Fib(n) int  (iterative; a loop to break in)
  cmd/
    demo/
      main.go              prints a couple of Fibonacci values
  seq/seq_test.go          table-driven test + Example
```

- Files: `seq/seq.go`, `cmd/demo/main.go`, `seq/seq_test.go`.
- Implement: `Fib(n int) int`, iterative, so there is a loop and accumulators to inspect remotely.
- Test: table-driven cases (0, 1, 10, 20) plus an `Example`.
- Verify: `go test -count=1 -race ./...`, then a headless backend + `dlv connect` script asserting `Fib(10)` state over the socket.

Set up the module:

```bash
mkdir -p ~/go-exercises/remotedbg/seq ~/go-exercises/remotedbg/cmd/demo
cd ~/go-exercises/remotedbg
go mod init example.com/remotedbg
go mod edit -go=1.24
```

### The headless/connect model, and why it is the editor path

`dlv debug --headless --listen=:2345 --api-version=2` starts Delve as a backend
server: it launches the target and exposes the debugger over a TCP socket instead
of a local REPL. A separate client, `dlv connect host:port`, attaches to that
socket and drives it with the same commands you already know. The split is what
makes remote debugging work — the backend runs inside the container (where the
binary and its `-N -l` build live) and the client runs on your laptop, connected
across the container boundary or an SSH tunnel. `--api-version=2` selects the
current wire protocol; `--accept-multiclient` lets more than one client share the
backend (and keeps it alive across client disconnects); `--continue` starts the
target running immediately instead of waiting for the first client.

The same backend also speaks the Debug Adapter Protocol: `dlv dap` runs a DAP
server that VS Code, Neovim, and other editors drive under the hood. So the editor
"Debug" button and your `dlv connect` session are the same engine — learning the
CLI is learning what the editor does for you.

A blunt security note: a Delve backend grants full code execution to anyone who
connects. Bind `--listen` to `127.0.0.1` (or a private network you control, or an
SSH tunnel) and never expose it on a public interface.

Create `seq/seq.go`:

```go
package seq

// Fib returns the nth Fibonacci number (Fib(0)=0, Fib(1)=1), computed
// iteratively so a debugger can watch the accumulators advance.
func Fib(n int) int {
	a, b := 0, 1
	for range n {
		a, b = b, a+b
	}
	return a
}
```

### The headless workflow

Build with debug info, start the backend in the background bound to localhost, and
wait until the port accepts connections before connecting:

```bash
go build -gcflags='all=-N -l' -o /tmp/remotedbg ./cmd/demo

dlv debug --headless --listen=127.0.0.1:2345 --api-version=2 --accept-multiclient ./cmd/demo &
DLV=$!

# poll until the backend is listening
until nc -z 127.0.0.1 2345 2>/dev/null; do sleep 0.1; done

dlv connect 127.0.0.1:2345
```

The connected client drives the remote target exactly like a local session:

```text
(dlv) break example.com/remotedbg/seq.Fib
Breakpoint 1 set at 0x... for example.com/remotedbg/seq.Fib() ./seq/seq.go:6
(dlv) condition 1 n == 10
(dlv) continue
> example.com/remotedbg/seq.Fib() ./seq/seq.go:6 (hits goroutine(1):1 total:1)
(dlv) print n
10
(dlv) next
(dlv) next
(dlv) print a
0
(dlv) print b
1
(dlv) continue
(dlv) quit
```

The commands run over the socket against the process on the other end; the
condition `n == 10` lands on the `Fib(10)` call so `print n` shows `10`, and after
two `next` steps you are on the first loop-body line with the accumulators at
their initial `a=0, b=1` before the first `a, b = b, a+b` runs. Quitting the client leaves the
`--accept-multiclient` backend alive; kill it explicitly when done.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/remotedbg/seq"
)

func main() {
	for _, n := range []int{10, 20} {
		fmt.Printf("fib(%d) = %d\n", n, seq.Fib(n))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
fib(10) = 55
fib(20) = 6765
```

### The test verifies the sequence

Create `seq/seq_test.go`:

```go
package seq

import (
	"fmt"
	"testing"
)

func TestFib(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		n    int
		want int
	}{
		{name: "zero", n: 0, want: 0},
		{name: "one", n: 1, want: 1},
		{name: "ten", n: 10, want: 55},
		{name: "twenty", n: 20, want: 6765},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Fib(tc.n); got != tc.want {
				t.Fatalf("Fib(%d) = %d; want %d", tc.n, got, tc.want)
			}
		})
	}
}

func ExampleFib() {
	fmt.Println(Fib(10))
	// Output: 55
}
```

### Scripted: drive the backend over the wire

`dlv connect` takes `--init` too, so the whole remote session is scriptable:

```bash
go build -gcflags='all=-N -l' -o /tmp/remotedbg ./cmd/demo

dlv debug --headless --listen=127.0.0.1:2345 --api-version=2 ./cmd/demo &
DLV=$!
until nc -z 127.0.0.1 2345 2>/dev/null; do sleep 0.1; done

cat > /tmp/remote.dlv <<'EOF'
break example.com/remotedbg/seq.Fib
condition 1 n == 10
continue
print n
quit
EOF

dlv connect 127.0.0.1:2345 --init /tmp/remote.dlv 2>&1 | tee /tmp/remote.out
kill "$DLV" 2>/dev/null

grep -q 'n = 10' /tmp/remote.out && echo "remote break confirmed"
```

The client connects to the backend, breaks on `Fib` with the condition, prints
`n = 10` over the socket, and quits; killing `$DLV` tears down the backend. A CI
job asserts the marker with the `grep`. For an editor, point its launch config at
`dlv dap` instead — the same backend, DAP wire format.

## Review

The target is correct when `Fib` produces the standard sequence, which the table
pins at 0, 1, 55, and 6765. The remote-debugging proof is that a `dlv connect`
client breaks and prints `n` against a process it launched only as a headless
backend — no local REPL involved. The mistakes to avoid are binding the listener
to a public interface (a Delve backend is remote code execution; keep it on
`127.0.0.1` or a tunnel), connecting before the port is up (poll it), and
forgetting `--api-version=2` on older setups where the client expects it. `dlv dap`
is the same backend in the protocol editors drive.

## Resources

- [`dlv debug` usage](https://github.com/go-delve/delve/blob/master/Documentation/usage/dlv_debug.md) — `--headless`, `--listen`, `--api-version`, `--accept-multiclient`, `--continue`.
- [`dlv connect` usage](https://github.com/go-delve/delve/blob/master/Documentation/usage/dlv_connect.md) — attaching a client to a headless backend.
- [`dlv dap` usage](https://github.com/go-delve/delve/blob/master/Documentation/usage/dlv_dap.md) — the Debug Adapter Protocol server editors use.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-data-watchpoints.md](09-data-watchpoints.md)
