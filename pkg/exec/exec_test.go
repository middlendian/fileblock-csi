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
