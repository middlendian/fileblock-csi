// Package exec is a thin wrapper around os/exec that captures combined output,
// enforces a timeout, and surfaces exit codes in a way that's convenient for
// the rest of the driver. It is the single funnel through which the driver
// shells out — keeping it in one place makes mocking and audit easier.
package exec

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// Default timeout for any single shell-out. Mount and mkfs operations on slow
// backing stores can take a while; pick a generous default.
const DefaultTimeout = 2 * time.Minute

// Error wraps a failed command with the captured combined output and exit
// code so callers don't have to type-assert on *exec.ExitError.
type Error struct {
	Cmd      string
	Args     []string
	ExitCode int
	Output   string
	Err      error
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s %v: exit %d: %s: %v", e.Cmd, e.Args, e.ExitCode, e.Output, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Runner is the interface the rest of the driver depends on. Tests substitute
// a fake.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type osRunner struct{ timeout time.Duration }

// New returns a Runner that shells out via os/exec with the given default
// timeout. Pass 0 for DefaultTimeout.
func New(timeout time.Duration) Runner {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &osRunner{timeout: timeout}
}

func (r *osRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out := buf.String()
	if err != nil {
		return out, &Error{
			Cmd:      name,
			Args:     args,
			ExitCode: cmd.ProcessState.ExitCode(),
			Output:   out,
			Err:      err,
		}
	}
	return out, nil
}
