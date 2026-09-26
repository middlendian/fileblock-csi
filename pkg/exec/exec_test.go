package exec

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunSuccess(t *testing.T) {
	r := New(0)
	out, err := r.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("Run(true): %v", err)
	}
	if out != "" {
		t.Fatalf("expected empty output, got %q", out)
	}
}

func TestRunCapturesStdout(t *testing.T) {
	r := New(0)
	out, err := r.Run(context.Background(), "sh", "-c", "echo hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(out) != "hello" {
		t.Fatalf("got %q want hello", strings.TrimSpace(out))
	}
}

func TestRunCapturesStderr(t *testing.T) {
	r := New(0)
	out, err := r.Run(context.Background(), "sh", "-c", "echo bad >&2; exit 3")
	if err == nil {
		t.Fatal("expected error from non-zero exit")
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error is %T, want *Error", err)
	}
	if e.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", e.ExitCode)
	}
	if !strings.Contains(out, "bad") {
		t.Fatalf("stderr not captured: %q", out)
	}
	// Ensure Error implements Unwrap and stringifies usefully.
	if e.Unwrap() == nil {
		t.Fatal("Unwrap returned nil")
	}
	if !strings.Contains(e.Error(), "exit 3") {
		t.Fatalf("Error() = %q, want substring 'exit 3'", e.Error())
	}
}

func TestRunDefaultTimeoutApplied(t *testing.T) {
	r := New(50 * time.Millisecond)
	start := time.Now()
	_, err := r.Run(context.Background(), "sleep", "5")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("timeout not honored, elapsed=%v", elapsed)
	}
}

func TestRunRespectsCallerDeadline(t *testing.T) {
	// New(0) -> DefaultTimeout (2 minutes). The caller's much shorter deadline
	// should win because Run only sets a default when the ctx has no deadline.
	r := New(0)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Run(ctx, "sleep", "5")
	if err == nil {
		t.Fatal("expected error from cancelled ctx")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("caller deadline ignored, elapsed=%v", time.Since(start))
	}
}

func TestNewZeroUsesDefault(t *testing.T) {
	r := New(0).(*osRunner)
	if r.timeout != DefaultTimeout {
		t.Fatalf("timeout=%v want %v", r.timeout, DefaultTimeout)
	}
}

func TestRunCmdDeliversSecretsOnFDs(t *testing.T) {
	r := New(0)
	out, err := r.RunCmd(context.Background(), Cmd{
		Name:    "sh",
		Args:    []string{"-c", `cat "$0"; printf '|'; cat "$1"`, SecretFD(0), SecretFD(1)},
		Secrets: [][]byte{[]byte("alpha\n"), []byte("beta")},
	})
	if err != nil {
		t.Fatalf("RunCmd: %v", err)
	}
	if out != "alpha\n|beta" {
		t.Fatalf("out = %q, want %q", out, "alpha\n|beta")
	}
}

func TestRunCmdAppendsEnv(t *testing.T) {
	out, err := New(0).RunCmd(context.Background(), Cmd{
		Name: "sh",
		Args: []string{"-c", `printf %s "$FB_TEST_ENV"`},
		Env:  []string{"FB_TEST_ENV=set"},
	})
	if err != nil || out != "set" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestRunCmdErrorOmitsSecret(t *testing.T) {
	_, err := New(0).RunCmd(context.Background(), Cmd{
		Name:    "sh",
		Args:    []string{"-c", `cat "$0" >/dev/null; exit 3`, SecretFD(0)},
		Secrets: [][]byte{[]byte("s3cr3t-value")},
	})
	var e *Error
	if !errors.As(err, &e) || e.ExitCode != 3 {
		t.Fatalf("err = %v, want *Error exit 3", err)
	}
	if strings.Contains(err.Error(), "s3cr3t-value") {
		t.Fatalf("error leaks the secret: %v", err)
	}
}

func TestSecretFD(t *testing.T) {
	if SecretFD(0) != "/dev/fd/3" || SecretFD(2) != "/dev/fd/5" {
		t.Fatalf("SecretFD: %s %s", SecretFD(0), SecretFD(2))
	}
}
