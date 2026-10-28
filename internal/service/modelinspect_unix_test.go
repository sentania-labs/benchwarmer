//go:build !windows

package service

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/modelplan"
)

func TestInspectModelFileRefusesNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	real := ggufFixture(t, 0)
	link := filepath.Join(dir, "link.gguf")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "pipe.gguf")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"symlink": link, "fifo": fifo} {
		start := time.Now()
		got := inspectModelFile(p, 0, modelplan.GPU{}, []string{p})
		if got.Status != api.InspectInvalid || got.Metadata != nil || time.Since(start) > 2*time.Second {
			t.Errorf("%s: %+v after %s", name, got, time.Since(start))
		}
	}
}
