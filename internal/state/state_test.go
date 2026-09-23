package state

import "testing"

func TestConditionMapping(t *testing.T) {
	want := map[State]Condition{
		Stopped: Unavailable, Cooldown: Unavailable, Loading: Available, Ready: Available, Busy: Available,
		Draining: Yielding, Preempting: Yielding, Suppressed: Unavailable, Disabled: Unavailable, Error: Unavailable,
	}
	for _, s := range All {
		if got := s.Condition(); got != want[s] {
			t.Errorf("%s: got %s want %s", s, got, want[s])
		}
	}
}

func TestAdmissionOnlyWhenLoaded(t *testing.T) {
	for _, s := range All {
		if s.Admitting() != (s == Ready || s == Busy) {
			t.Errorf("%s admitting=%v", s, s.Admitting())
		}
	}
}

func TestTransitionInvariants(t *testing.T) {
	cases := []struct {
		from, to State
		ok       bool
	}{
		{Stopped, Loading, true},
		{Cooldown, Loading, false}, // cooldown must expire to Stopped first
		{Suppressed, Loading, false},
		{Disabled, Loading, false},
		{Error, Loading, false},
		{Loading, Ready, true},
		{Ready, Busy, true},
		{Busy, Draining, true},
		{Draining, Ready, false}, // a drain never re-opens admission
		{Draining, Busy, false},
		{Draining, Preempting, true},
		{Ready, Preempting, true},
		{Preempting, Cooldown, true},
		{Preempting, Suppressed, true},
		{Preempting, Ready, false},
		{Ready, Stopped, false}, // the runtime must be terminated via Preempting
		{Busy, Cooldown, false},
		{Ready, Ready, false},
	}
	for _, c := range cases {
		if got := CanTransition(c.from, c.to); got != c.ok {
			t.Errorf("%s -> %s: got %v want %v", c.from, c.to, got, c.ok)
		}
	}
}

func TestEveryRunningStateCanReachError(t *testing.T) {
	for _, s := range All {
		if s == Preempting || s == Error {
			continue
		}
		if !CanTransition(s, Error) {
			t.Errorf("%s cannot reach Error", s)
		}
	}
}

func TestNoPathFromRunningToIdleSkipsPreempting(t *testing.T) {
	for _, from := range All {
		if !from.RuntimeRunning() || from == Preempting {
			continue
		}
		for _, to := range idle {
			if to == Error {
				continue // crash: runtime already gone
			}
			if CanTransition(from, to) {
				t.Errorf("%s -> %s skips termination", from, to)
			}
		}
	}
}
