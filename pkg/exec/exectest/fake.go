// Package exectest provides a fake exec.Runner for unit tests. Other packages
// can wire it in place of the real os-level runner to drive deterministic
// behaviour without shelling out.
package exectest

import (
	"context"
	"fmt"
	"sync"
)

// Call records one Run invocation.
type Call struct {
	Name string
	Args []string
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
	mu      sync.Mutex
	rules   map[string]Response
	Func    func(ctx context.Context, name string, args ...string) (string, error)
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
	f.mu.Lock()
	f.Calls = append(f.Calls, Call{Name: name, Args: append([]string(nil), args...)})
	rule, ok := f.rules[name]
	useDefault := f.HasDef
	def := f.Default
	fn := f.Func
	f.mu.Unlock()

	if fn != nil {
		return fn(ctx, name, args...)
	}
	if ok {
		return rule.Out, rule.Err
	}
	if useDefault {
		return def.Out, def.Err
	}
	return "", fmt.Errorf("FakeRunner: unexpected call %s %v", name, args)
}

// Reset clears recorded calls. Rules and Func are preserved.
func (f *FakeRunner) Reset() {
	f.mu.Lock()
	f.Calls = nil
	f.mu.Unlock()
}
