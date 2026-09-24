//go:build !windows

package winsvc

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// TestRunSIGTERM sends a real SIGTERM to the test process; RunConsole must
// turn it into a Stop event instead of letting it kill the process.
func TestRunSIGTERM(t *testing.T) {
	started := make(chan struct{})
	got := make(chan Kind, 1)
	h := HandlerFunc(func(ctx context.Context, ev <-chan Event) error {
		close(started)
		<-ctx.Done()
		got <- (<-ev).Kind
		return nil
	})
	errc := make(chan error, 1)
	go func() { errc <- Run(Options{StopTimeout: 5 * time.Second}, h) }()
	<-started
	// signal.Notify is installed before the handler starts.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after SIGTERM")
	}
	if k := <-got; k != Stop {
		t.Fatalf("kind %v", k)
	}
}
