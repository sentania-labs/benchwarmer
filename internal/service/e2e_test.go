package service

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/events"
	"github.com/sentania-labs/benchwarmer/internal/signals"
	"github.com/sentania-labs/benchwarmer/internal/state"
	"github.com/sentania-labs/benchwarmer/internal/winsvc"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func exe(name string) string {
	if goruntime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func buildFakeLlama(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(dir, exe("fakellama"))
	cmd := exec.Command("go", "build", "-o", out, "github.com/sentania-labs/benchwarmer/cmd/fakellama")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fakellama: %v\n%s", err, b)
	}
	return out
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestEndToEndLifecycle runs the assembled service with a real runtime
// process (fakellama), real process detection, and the GPU simulator. It is
// the automated form of the first milestone: load, serve, detect a
// competing workload, stop admitting, drain, unload, release, cool down,
// reload; then hard contention, suppression, and clean shutdown.
func TestEndToEndLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	dir := t.TempDir()
	fake := buildFakeLlama(t, dir)
	model := filepath.Join(dir, "model.gguf")
	_ = os.WriteFile(model, []byte("fake"), 0o644)
	simCtl := filepath.Join(dir, "sim.json")
	_ = os.WriteFile(simCtl, []byte("{}"), 0o644)
	data := filepath.Join(dir, "data")
	_ = os.MkdirAll(data, 0o755)

	c := config.Default()
	c.Runtime.Executable, c.Runtime.ModelPath = fake, model
	c.Runtime.Args = []string{"-token-delay", "30ms", "-tokens", "120"}
	c.Runtime.Port = freePort(t)
	inf, mgmt := freePort(t), freePort(t)
	c.Listen.Inference = "127.0.0.1:" + strconv.Itoa(inf)
	c.Listen.Management = "127.0.0.1:" + strconv.Itoa(mgmt)
	c.Recovery.StartupCooldown = config.Duration(time.Second)
	for _, n := range []string{"normal", "school_hours"} {
		p := c.Profiles[n]
		p.Cooldown = config.Duration(3 * time.Second)
		c.Profiles[n] = p
	}
	c.Schedules[0].Enabled = false
	c.Signals.ProcessInterval = config.Duration(250 * time.Millisecond)
	b, _ := config.Marshal(c)
	if err := os.WriteFile(filepath.Join(data, "config.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	svc, err := New(Options{DataDir: data, SimulateGPU: true, SimControl: simCtl, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	evs := make(chan winsvc.Event, 1)
	runDone := make(chan error, 1)
	go func() { runDone <- svc.Run(context.Background(), evs) }()
	ctl := svc.Controller()

	waitState := func(want state.State, within time.Duration) {
		t.Helper()
		deadline := time.Now().Add(within)
		for time.Now().Before(deadline) {
			if ctl.Status().State == want {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		st := ctl.Status()
		t.Fatalf("state %s, want %s (%s)", st.State, want, st.Summary)
	}
	proxyURL := "http://127.0.0.1:" + strconv.Itoa(inf) + "/v1/chat/completions"
	stream := func() chan [2]int {
		resp, err := http.Post(proxyURL, "application/json", strings.NewReader(`{"stream":true}`))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("stream status %d", resp.StatusCode)
		}
		out := make(chan [2]int, 1)
		go func() {
			defer resp.Body.Close()
			sc := bufio.NewScanner(resp.Body)
			chunks, done := 0, 0
			for sc.Scan() {
				switch l := sc.Text(); {
				case l == "data: [DONE]":
					done = 1
				case strings.HasPrefix(l, "data: "):
					chunks++
				}
			}
			out <- [2]int{chunks, done}
		}()
		return out
	}

	waitState(state.Ready, 15*time.Second)

	// A game starts while a request is streaming.
	s1 := stream()
	time.Sleep(300 * time.Millisecond)
	gamePath := filepath.Join(dir, "steamapps", "common", "Elden Ring", exe("eldenring"))
	copyFile(t, fake, gamePath)
	game := exec.Command(gamePath, "-port", strconv.Itoa(freePort(t)), "-load-delay", "1h")
	if err := game.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = game.Process.Kill(); _, _ = game.Process.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && ctl.Status().Condition == state.Available {
		time.Sleep(25 * time.Millisecond)
	}
	resp, err := http.Post(proxyURL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	resp.Body.Close()
	if resp.StatusCode != 503 || body["code"] != "worker_unavailable" {
		t.Fatalf("new request after yield: %d %v", resp.StatusCode, body)
	}
	if r := <-s1; r[1] != 1 || r[0] != 120 {
		t.Fatalf("active request should finish within grace: chunks=%d done=%d", r[0], r[1])
	}
	waitState(state.Cooldown, 10*time.Second)
	if st := ctl.Status(); !strings.Contains(st.Summary, "after it stops") {
		t.Fatalf("summary while game runs: %q", st.Summary)
	}

	// The game exits; reload after the cooldown.
	_ = game.Process.Kill()
	_, _ = game.Process.Wait()
	waitState(state.Ready, 15*time.Second)

	// Hard contention during a request cuts it immediately.
	s2 := stream()
	time.Sleep(300 * time.Millisecond)
	_ = os.WriteFile(simCtl, []byte(`{"external_vram_mib": 4000}`), 0o644)
	if r := <-s2; r[1] == 1 {
		t.Fatalf("request should be cut by hard contention: %v", r)
	}
	// Second preemption within the window: suppressed.
	waitState(state.Suppressed, 10*time.Second)
	if st := ctl.Status(); st.Timers.SuppressedUntil == nil {
		t.Fatal("suppression expiry not reported")
	}

	evList, err := ctl.Events(apiQuery(200))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[events.Type]bool{}
	for _, e := range evList {
		seen[e.Type] = true
	}
	for _, want := range []events.Type{events.LoadCompleted, events.DrainStarted, events.DrainCompleted, events.RuntimeStopped,
		events.CooldownStarted, events.VRAMReleased, events.Preempted, events.SuppressionStarted} {
		if !seen[want] {
			t.Errorf("missing event %s", want)
		}
	}

	// Clean shutdown leaves no runtime behind.
	evs <- winsvc.Event{Kind: winsvc.Stop, Deadline: time.Now().Add(10 * time.Second)}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("service did not stop")
	}
	procs, _ := signals.Snapshot()
	for _, p := range procs {
		if p.Path != "" && filepath.Clean(p.Path) == filepath.Clean(fake) {
			t.Fatalf("runtime process %d survived shutdown", p.PID)
		}
	}
}

func apiQuery(limit int) api.EventQuery { return api.EventQuery{Limit: limit} }
