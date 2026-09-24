package telemetry

import (
	"errors"
	"sync"
)

// Source produces telemetry samples for the target adapter. The Windows
// implementation is *WindowsCollector; tests use Fake.
type Source interface {
	// Collect takes one sample. It never fails outright: failures are
	// reported through Sample.Complete and Sample.Errors.
	Collect() Sample
	// Rebuild re-enumerates the adapter and recreates collection state,
	// e.g. after resume or a driver reset.
	Rebuild() error
	// Close releases resources.
	Close()
}

var _ Source = (*Fake)(nil)

// ErrClosed is returned by Fake.Rebuild after Close.
var ErrClosed = errors.New("telemetry: source closed")

// Fake is a scripted Source. Collect returns Samples in order and then keeps
// returning the last one; with no samples it returns an incomplete Sample.
// It is safe for concurrent use.
type Fake struct {
	mu      sync.Mutex
	samples []Sample
	next    int

	// RebuildErr, when set, is returned by Rebuild.
	RebuildErr error
	rebuilds   int
	closed     bool
}

// NewFake returns a Fake that plays samples in order.
func NewFake(samples ...Sample) *Fake { return &Fake{samples: samples} }

// Push appends samples to the script.
func (f *Fake) Push(samples ...Sample) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.samples = append(f.samples, samples...)
}

// Collect implements Source.
func (f *Fake) Collect() Sample {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.samples) == 0 {
		return Sample{Errors: []string{"fake: no samples scripted"}}
	}
	i := min(f.next, len(f.samples)-1)
	if f.next < len(f.samples) {
		f.next++
	}
	return f.samples[i]
}

// Rebuild implements Source.
func (f *Fake) Rebuild() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return ErrClosed
	}
	f.rebuilds++
	return f.RebuildErr
}

// Close implements Source.
func (f *Fake) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

// Rebuilds reports how many times Rebuild was called.
func (f *Fake) Rebuilds() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rebuilds
}

// Closed reports whether Close was called.
func (f *Fake) Closed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}
