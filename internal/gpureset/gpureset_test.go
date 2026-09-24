package gpureset

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReportsOnlyNewWatchdogDumps(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "WATCHDOG")
	_ = os.MkdirAll(sub, 0o755)
	_ = os.WriteFile(filepath.Join(sub, "WATCHDOG-20260924-0536.dmp"), []byte("old"), 0o644)
	w := New([]string{sub, dir})
	if got := w.Poll(); len(got) != 0 {
		t.Fatalf("baseline dump reported: %v", got)
	}
	_ = os.WriteFile(filepath.Join(sub, "WATCHDOG-20260924-0921.dmp"), []byte("new"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "WATCHDOG-20260924-0921.dmp"), []byte("full"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "USBHUB3-20260924.dmp"), []byte("other"), 0o644)
	got := w.Poll()
	if len(got) != 2 {
		t.Fatalf("want 2 new watchdog dumps, got %v", got)
	}
	if again := w.Poll(); len(again) != 0 {
		t.Fatalf("reported twice: %v", again)
	}
	// A rewritten dump (same name, newer time) counts again.
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(filepath.Join(sub, "WATCHDOG-20260924-0921.dmp"), later, later)
	if got := w.Poll(); len(got) != 1 {
		t.Fatalf("rewritten dump not reported: %v", got)
	}
}

func TestMissingDirsAreHarmless(t *testing.T) {
	w := New([]string{filepath.Join(t.TempDir(), "nope")})
	if got := w.Poll(); len(got) != 0 {
		t.Fatal(got)
	}
}
