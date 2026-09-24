package telemetry

import (
	"errors"
	"testing"
	"time"
)

func TestFakeScript(t *testing.T) {
	f := NewFake()
	if s := f.Collect(); s.Complete || len(s.Errors) == 0 {
		t.Fatalf("empty fake must return an incomplete sample: %+v", s)
	}
	t0 := time.Unix(1000, 0)
	f.Push(Sample{Time: t0, Complete: true}, Sample{Time: t0.Add(time.Second), Complete: true})
	for i, want := range []time.Time{t0, t0.Add(time.Second), t0.Add(time.Second)} {
		if got := f.Collect().Time; !got.Equal(want) {
			t.Fatalf("collect %d: %v, want %v", i, got, want)
		}
	}
	f.RebuildErr = errors.New("boom")
	if err := f.Rebuild(); err == nil || f.Rebuilds() != 1 {
		t.Fatalf("rebuild: %v, count %d", err, f.Rebuilds())
	}
	f.Close()
	if !f.Closed() || !errors.Is(f.Rebuild(), ErrClosed) {
		t.Fatal("close not recorded")
	}
}
