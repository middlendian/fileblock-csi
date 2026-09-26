// Package exec is a thin wrapper around os/exec that captures combined output,
// enforces a timeout, and surfaces exit codes in a way that's convenient for
// the rest of the driver. It is the single funnel through which the driver
// shells out — keeping it in one place makes mocking and audit easier.
package exec

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
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

// Cmd is a command with inputs Run cannot express.
type Cmd struct {
	Name string
	Args []string
	// Env is appended to the parent's environment.
	Env []string
	// Secrets[i] is readable by the child, to EOF, at SecretFD(i). They
	// travel over pipes so they never touch argv or disk.
	Secrets [][]byte
}

// SecretFD is the path at which the child reads Cmd.Secrets[i].
func SecretFD(i int) string { return "/dev/fd/" + strconv.Itoa(3+i) }

// Runner is the interface the rest of the driver depends on. Tests substitute
// a fake.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
	RunCmd(ctx context.Context, c Cmd) (string, error)
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
	return r.RunCmd(ctx, Cmd{Name: name, Args: args})
}

func (r *osRunner) RunCmd(ctx context.Context, c Cmd) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	writers := make([]*os.File, 0, len(c.Secrets))
	closeAll := func(fs []*os.File) {
		for _, f := range fs {
			_ = f.Close()
		}
	}
	for range c.Secrets {
		pr, pw, err := os.Pipe()
		if err != nil {
			closeAll(cmd.ExtraFiles)
			closeAll(writers)
			return "", fmt.Errorf("pipe for %s: %w", c.Name, err)
		}
		cmd.ExtraFiles = append(cmd.ExtraFiles, pr)
		writers = append(writers, pw)
	}
	startErr := cmd.Start()
	// The child holds its own copies of the read ends.
	closeAll(cmd.ExtraFiles)
	if startErr != nil {
		closeAll(writers)
		return "", &Error{Cmd: c.Name, Args: c.Args, ExitCode: -1, Err: startErr}
	}
	var wg sync.WaitGroup
	for i, w := range writers {
		wg.Add(1)
		go func(w *os.File, s []byte) {
			defer wg.Done()
			_, _ = w.Write(s)
			_ = w.Close()
		}(w, c.Secrets[i])
	}
	err := cmd.Wait()
	wg.Wait()
	out := buf.String()
	if err != nil {
		return out, &Error{
			Cmd:      c.Name,
			Args:     c.Args,
			ExitCode: cmd.ProcessState.ExitCode(),
			Output:   out,
			Err:      err,
		}
	}
	return out, nil
}
