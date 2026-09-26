// Package exectest provides a fake exec.Runner for unit tests. Other packages
// can wire it in place of the real os-level runner to drive deterministic
// behaviour without shelling out.
package exectest

import (
	"context"
	"fmt"
	"sync"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
)

// Call records one Run or RunCmd invocation.
type Call struct {
	Name    string
	Args    []string
	Env     []string
	Secrets [][]byte
}

// Response pairs an output with an error returned for a matched call.
type Response struct {
	Out string
	Err error
}

// FakeRunner is an in-memory exec.Runner. Match callers by command name (the
// first arg) via Set, or supply a custom Func that sees the full args for
// richer matching. Calls are recorded in order.
type FakeRunner struct {
	mu    sync.Mutex
	rules map[string]Response
	Func  func(ctx context.Context, name string, args ...string) (string, error)
	// CmdFunc, when set, handles RunCmd calls and sees Env and Secrets.
	// RunCmd falls back to Func and the rules when it is nil.
	CmdFunc func(ctx context.Context, c fbexec.Cmd) (string, error)
	Calls   []Call
	Default Response
	HasDef  bool
}

// New returns a FakeRunner with no rules. Until Set or Func is used, every
// Run returns ("", error("unexpected call <name>")).
func New() *FakeRunner {
	return &FakeRunner{rules: map[string]Response{}}
}

// Set registers a canned response for a given command name.
func (f *FakeRunner) Set(name, out string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules[name] = Response{Out: out, Err: err}
}

// SetDefault returns the given response for any unmatched call.
func (f *FakeRunner) SetDefault(out string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Default = Response{Out: out, Err: err}
	f.HasDef = true
}

// Run implements exec.Runner.
func (f *FakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	return f.dispatch(ctx, fbexec.Cmd{Name: name, Args: args}, false)
}

// RunCmd implements exec.Runner.
func (f *FakeRunner) RunCmd(ctx context.Context, c fbexec.Cmd) (string, error) {
	return f.dispatch(ctx, c, true)
}

func (f *FakeRunner) dispatch(ctx context.Context, c fbexec.Cmd, viaCmd bool) (string, error) {
	f.mu.Lock()
	call := Call{Name: c.Name, Args: append([]string(nil), c.Args...), Env: append([]string(nil), c.Env...)}
	for _, s := range c.Secrets {
		call.Secrets = append(call.Secrets, append([]byte(nil), s...))
	}
	f.Calls = append(f.Calls, call)
	rule, ok := f.rules[c.Name]
	useDefault := f.HasDef
	def := f.Default
	fn := f.Func
	cmdFn := f.CmdFunc
	f.mu.Unlock()

	if viaCmd && cmdFn != nil {
		return cmdFn(ctx, c)
	}
	if fn != nil {
		return fn(ctx, c.Name, c.Args...)
	}
	if ok {
		return rule.Out, rule.Err
	}
	if useDefault {
		return def.Out, def.Err
	}
	return "", fmt.Errorf("FakeRunner: unexpected call %s %v", c.Name, c.Args)
}

// Reset clears recorded calls. Rules and Func are preserved.
func (f *FakeRunner) Reset() {
	f.mu.Lock()
	f.Calls = nil
	f.mu.Unlock()
}
