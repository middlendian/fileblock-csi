package loop

import (
	"context"
	"errors"
	"testing"

	fbexec "github.com/middlendian/fileblock-csi/pkg/exec"
	"github.com/middlendian/fileblock-csi/pkg/exec/exectest"
)

func TestAttachReturnsDevice(t *testing.T) {
	fake := exectest.New()
	fake.Set("losetup", "/dev/loop7\n", nil)
	dev, err := NewLosetup(fake).Attach(context.Background(), "/img/a.img")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if dev != "/dev/loop7" {
		t.Fatalf("dev=%q want /dev/loop7", dev)
	}
}

func TestAttachPoolExhausted(t *testing.T) {
	fake := exectest.New()
	fake.Set("losetup", "losetup: cannot find an unused loop device: No such device", &fbexec.Error{
		Cmd:      "losetup",
		Args:     []string{"--find", "--show", "x"},
		ExitCode: 1,
		Output:   "losetup: cannot find an unused loop device: No such device",
		Err:      errors.New("exit status 1"),
	})
	_, err := NewLosetup(fake).Attach(context.Background(), "x")
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("got %v, want ErrPoolExhausted", err)
	}
}

func TestAttachUnexpectedDevice(t *testing.T) {
	fake := exectest.New()
	fake.Set("losetup", "garbage\n", nil)
	_, err := NewLosetup(fake).Attach(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error on unexpected device output")
	}
}

func TestDetachIdempotent(t *testing.T) {
	fake := exectest.New()
	fake.Set("losetup", "losetup: /dev/loop9: detach failed: No such device or address", &fbexec.Error{
		Cmd:      "losetup",
		Args:     []string{"--detach", "/dev/loop9"},
		ExitCode: 1,
		Output:   "losetup: /dev/loop9: detach failed: No such device or address",
		Err:      errors.New("exit status 1"),
	})
	if err := NewLosetup(fake).Detach(context.Background(), "/dev/loop9"); err != nil {
		t.Fatalf("Detach should swallow ENXIO-like output, got %v", err)
	}
}

func TestDetachSuccess(t *testing.T) {
	fake := exectest.New()
	fake.Set("losetup", "", nil)
	if err := NewLosetup(fake).Detach(context.Background(), "/dev/loop3"); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if got := fake.Calls[0].Args; got[0] != "--detach" || got[1] != "/dev/loop3" {
		t.Fatalf("Detach args=%v", got)
	}
}

func TestListParsesJSON(t *testing.T) {
	fake := exectest.New()
	fake.Set("losetup", `{"loopdevices":[
        {"name":"/dev/loop0","back-file":"/srv/a.img"},
        {"name":"/dev/loop1","back-file":"/srv/b.img"}
    ]}`, nil)
	out, err := NewLosetup(fake).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 || out[0].Device != "/dev/loop0" || out[1].BackFile != "/srv/b.img" {
		t.Fatalf("List: %+v", out)
	}
}

func TestListEmpty(t *testing.T) {
	fake := exectest.New()
	fake.Set("losetup", "   ", nil)
	out, err := NewLosetup(fake).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty, got %+v", out)
	}
}

func TestSetCapacity(t *testing.T) {
	fake := exectest.New()
	fake.Set("losetup", "", nil)
	if err := NewLosetup(fake).SetCapacity(context.Background(), "/dev/loop2"); err != nil {
		t.Fatalf("SetCapacity: %v", err)
	}
	if fake.Calls[0].Args[0] != "--set-capacity" {
		t.Fatalf("expected --set-capacity, got %v", fake.Calls[0].Args)
	}
}
