package winsvc

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 23, 21, 0, 0, 0, time.UTC)

func TestTranslate(t *testing.T) {
	o := Options{StopTimeout: 10 * time.Second}
	tests := []struct {
		name          string
		cmd, evt, sid uint32
		want          Event
		ok            bool
	}{
		{"stop", ctlStop, 0, 0, Event{Kind: Stop, Deadline: now.Add(10 * time.Second)}, true},
		{"preshutdown", ctlPreShutdown, 0, 0, Event{Kind: Shutdown, Deadline: now.Add(DefaultPreshutdownTimeout)}, true},
		{"shutdown", ctlShutdown, 0, 0, Event{Kind: Shutdown, Deadline: now.Add(DefaultShutdownTimeout)}, true},
		{"suspend", ctlPowerEvent, 4, 0, Event{Kind: Suspend}, true},
		{"resume automatic", ctlPowerEvent, 0x12, 0, Event{Kind: Resume}, true},
		{"resume suspend", ctlPowerEvent, 7, 0, Event{Kind: Resume}, true},
		{"power status change ignored", ctlPowerEvent, 0xA, 0, Event{}, false},
		{"logon", ctlSessionChange, 5, 2, Event{Kind: SessionChange, Session: SessionLogon, SessionID: 2}, true},
		{"logoff", ctlSessionChange, 6, 2, Event{Kind: SessionChange, Session: SessionLogoff, SessionID: 2}, true},
		{"lock", ctlSessionChange, 7, 3, Event{Kind: SessionChange, Session: SessionLock, SessionID: 3}, true},
		{"unlock", ctlSessionChange, 8, 3, Event{Kind: SessionChange, Session: SessionUnlock, SessionID: 3}, true},
		{"console connect", ctlSessionChange, 1, 1, Event{Kind: SessionChange, Session: SessionConsoleConnect, SessionID: 1}, true},
		{"console disconnect", ctlSessionChange, 2, 1, Event{Kind: SessionChange, Session: SessionConsoleDisconnect, SessionID: 1}, true},
		{"remote connect", ctlSessionChange, 3, 4, Event{Kind: SessionChange, Session: SessionRemoteConnect, SessionID: 4}, true},
		{"session remote control ignored", ctlSessionChange, 9, 1, Event{}, false},
		{"pause ignored", 0x2, 0, 0, Event{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := translate(tt.cmd, tt.evt, tt.sid, now, o)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("got %+v %v, want %+v %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestWaitHint(t *testing.T) {
	if got := waitHint(now.Add(15*time.Second), now); got != 15000 {
		t.Fatalf("hint %d", got)
	}
	if got := waitHint(now.Add(-time.Second), now); got != 2000 {
		t.Fatalf("past deadline hint %d", got)
	}
}

func TestStartName(t *testing.T) {
	tests := []struct {
		account, want string
		sid, err      bool
	}{
		{"", `NT SERVICE\Benchwarmer`, true, false},
		{AccountVirtual, `NT SERVICE\Benchwarmer`, true, false},
		{AccountLocalSystem, "LocalSystem", false, false},
		{"NT AUTHORITY\\NetworkService", "", false, true},
	}
	for _, tt := range tests {
		name, sid, err := startName(tt.account, "Benchwarmer")
		if name != tt.want || sid != tt.sid || (err != nil) != tt.err {
			t.Errorf("%q: got %q %v %v", tt.account, name, sid, err)
		}
	}
	if _, _, err := startName(AccountVirtual, ""); err == nil {
		t.Error("virtual account without a service name accepted")
	}
}

func TestRecoveryPlan(t *testing.T) {
	p := RecoveryPlan()
	want := []RecoveryStep{{true, 30 * time.Second}, {true, 2 * time.Minute}, {false, 0}}
	if len(p) != len(want) {
		t.Fatalf("plan %+v", p)
	}
	for i := range want {
		if p[i] != want[i] {
			t.Fatalf("step %d: %+v", i, p[i])
		}
	}
	if RecoveryResetPeriod != 24*time.Hour {
		t.Fatal("reset period")
	}
}

func TestConsoleStop(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	got := make(chan Event, 1)
	h := HandlerFunc(func(ctx context.Context, ev <-chan Event) error {
		<-ctx.Done()
		got <- <-ev
		return nil
	})
	errc := make(chan error, 1)
	go func() {
		errc <- runConsole(Options{StopTimeout: 5 * time.Second}, h, sigs, func() time.Time { return now })
	}()
	sigs <- os.Interrupt
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if e := <-got; e.Kind != Stop || !e.Deadline.Equal(now.Add(5*time.Second)) {
		t.Fatalf("event %+v", e)
	}
}

func TestConsoleHandlerExitsOnItsOwn(t *testing.T) {
	boom := errors.New("boom")
	h := HandlerFunc(func(context.Context, <-chan Event) error { return boom })
	if err := runConsole(Options{}, h, make(chan os.Signal), time.Now); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
}

func TestConsoleForcedAndTimeout(t *testing.T) {
	stuck := HandlerFunc(func(context.Context, <-chan Event) error { select {} })

	sigs := make(chan os.Signal, 2)
	sigs <- os.Interrupt
	sigs <- os.Interrupt
	if err := runConsole(Options{StopTimeout: 5 * time.Second}, stuck, sigs, time.Now); !errors.Is(err, ErrForced) {
		t.Fatalf("second interrupt: %v", err)
	}

	sigs = make(chan os.Signal, 1)
	sigs <- os.Interrupt
	err := runConsole(Options{StopTimeout: 50 * time.Millisecond}, stuck, sigs, time.Now)
	if err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("timeout: %v", err)
	}
}
