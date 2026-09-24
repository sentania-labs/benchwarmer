package llamacpp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
)

var fakeExe string

// TestMain builds cmd/fakellama once into a temp dir; every test runs it as
// the "llama-server" executable.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "llamacpp-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	name := "fakellama"
	if goruntime.GOOS == "windows" {
		name += ".exe"
	}
	fakeExe = filepath.Join(dir, name)
	build := exec.Command("go", "build", "-o", fakeExe, "github.com/sentania-labs/benchwarmer/cmd/fakellama")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build fakellama:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func testConfig(t *testing.T, extra ...string) config.Runtime {
	t.Helper()
	model := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(model, []byte("GGUF"), 0o644); err != nil {
		t.Fatal(err)
	}
	return config.Runtime{
		Executable:           fakeExe,
		ModelPath:            model,
		Args:                 extra,
		ContextSize:          512,
		GPULayers:            0,
		Host:                 "127.0.0.1",
		Port:                 freePort(t),
		LoadTimeout:          config.Duration(10 * time.Second),
		DiagnosticsTailBytes: 4096,
	}
}

func start(t *testing.T, a *Adapter, cfg config.Runtime) *Instance {
	t.Helper()
	inst, err := a.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	i := inst.(*Instance)
	t.Cleanup(func() { _, _ = i.Stop(5 * time.Second) })
	return i
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func TestValidate(t *testing.T) {
	a := New()
	good := testConfig(t)
	if err := a.Validate(good); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty.gguf")
	_ = os.WriteFile(empty, nil, 0o644)
	tests := []struct {
		name   string
		mutate func(*config.Runtime)
		want   string
	}{
		{"missing executable", func(c *config.Runtime) { c.Executable = filepath.Join(t.TempDir(), "nope") }, "executable"},
		{"executable is a directory", func(c *config.Runtime) { c.Executable = t.TempDir() }, "not a regular file"},
		{"missing model", func(c *config.Runtime) { c.ModelPath = filepath.Join(t.TempDir(), "nope.gguf") }, "model"},
		{"empty model", func(c *config.Runtime) { c.ModelPath = empty }, "empty"},
		{"non-loopback host", func(c *config.Runtime) { c.Host = "0.0.0.0" }, "loopback"},
		{"lan host", func(c *config.Runtime) { c.Host = "192.168.1.5" }, "loopback"},
		{"bad port", func(c *config.Runtime) { c.Port = 0 }, "port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := good
			tt.mutate(&c)
			err := a.Validate(c)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err %v, want mention of %q", err, tt.want)
			}
		})
	}
	for _, h := range []string{"localhost", "::1", "127.0.0.2"} {
		c := good
		c.Host = h
		if err := a.Validate(c); err != nil {
			t.Errorf("host %s rejected: %v", h, err)
		}
	}
}

func TestArgs(t *testing.T) {
	c := config.Runtime{ModelPath: "m.gguf", Host: "127.0.0.1", Port: 9, ContextSize: 8192, GPULayers: 999, Args: []string{"-fa", "on"}}
	want := []string{"-m", "m.gguf", "--host", "127.0.0.1", "--port", "9", "-c", "8192", "-ngl", "999", "-fa", "on"}
	if got := Args(c); !slices.Equal(got, want) {
		t.Fatalf("args %v", got)
	}
}

func TestStartReadyStop(t *testing.T) {
	a := New()
	cfg := testConfig(t, "-load-delay", "100ms")
	i := start(t, a, cfg)
	if err := i.WaitReady(context.Background()); err != nil {
		t.Fatalf("WaitReady: %v\n%s", err, i.Diagnostics())
	}
	resp, err := http.Get(i.BaseURL().JoinPath("health").String())
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("health: %v %v", resp, err)
	}
	resp.Body.Close()
	if !slices.Contains(i.Members(), i.PID()) || i.Started().IsZero() || i.ExitErr() != nil {
		t.Fatalf("members %v, started %v, exitErr %v", i.Members(), i.Started(), i.ExitErr())
	}
	if !strings.Contains(i.Diagnostics(), "listening") {
		t.Fatalf("diagnostics missing startup line: %q", i.Diagnostics())
	}

	res, err := i.Stop(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.AlreadyExited || res.RootExit <= 0 || res.TreeEmpty < res.RootExit {
		t.Fatalf("stop result %+v", res)
	}
	select {
	case <-i.Exited():
	default:
		t.Fatal("Exited not closed after Stop")
	}
	if i.ExitErr() == nil || len(i.Members()) != 0 {
		t.Fatalf("after stop: exitErr %v members %v", i.ExitErr(), i.Members())
	}
	// Idempotent: same result, no error.
	res2, err := i.Stop(time.Second)
	if err != nil || res2 != res {
		t.Fatalf("second stop: %+v %v", res2, err)
	}
	if len(a.ownedPIDs()) != 0 {
		t.Fatal("stopped instance still owned")
	}
}

func TestReadyFailsFastOnCrash(t *testing.T) {
	cfg := testConfig(t, "-load-delay", "1h", "-crash-after", "200ms")
	i := start(t, New(), cfg)
	t0 := time.Now()
	err := i.WaitReady(context.Background())
	if !errors.Is(err, ErrExited) {
		t.Fatalf("err %v, want ErrExited", err)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("crash noticed after %v", d)
	}
	if !strings.Contains(i.Diagnostics(), "simulated crash") {
		t.Fatalf("diagnostics: %q", i.Diagnostics())
	}
	res, err := i.Stop(5 * time.Second)
	if err != nil || !res.AlreadyExited || res.RootExit != 0 {
		t.Fatalf("stop after crash: %+v %v", res, err)
	}
}

func TestLoadTimeout(t *testing.T) {
	cfg := testConfig(t, "-load-delay", "1h")
	cfg.LoadTimeout = config.Duration(300 * time.Millisecond)
	i := start(t, New(), cfg)
	t0 := time.Now()
	if err := i.WaitReady(context.Background()); !errors.Is(err, ErrLoadTimeout) {
		t.Fatalf("err %v, want ErrLoadTimeout", err)
	}
	if d := time.Since(t0); d > 3*time.Second {
		t.Fatalf("timeout took %v", d)
	}
}

func TestWaitReadyContext(t *testing.T) {
	i := start(t, New(), testConfig(t, "-load-delay", "1h"))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := i.WaitReady(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
}

func TestStopKillsTree(t *testing.T) {
	i := start(t, New(), testConfig(t, "-load-delay", "50ms", "-spawn-child"))
	if err := i.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	var child int
	waitFor(t, func() bool {
		for _, p := range i.Members() {
			if p != i.PID() {
				child = p
				return true
			}
		}
		return false
	}, "child in boundary")
	res, err := i.Stop(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if processAlive(uint32(child)) || processAlive(uint32(i.PID())) {
		t.Fatalf("leftover process after stop (%+v)", res)
	}
}

func TestPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := testConfig(t)
	cfg.Port = ln.Addr().(*net.TCPAddr).Port
	if _, err := New().Start(context.Background(), cfg); !errors.Is(err, ErrPortInUse) {
		t.Fatalf("err %v, want ErrPortInUse", err)
	}
}

func TestReconcileKillsOrphanOnly(t *testing.T) {
	a := New()
	owned := start(t, a, testConfig(t, "-load-delay", "50ms", "-spawn-child"))
	if err := owned.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	// On Windows the job also holds the hidden conhost.exe of the console
	// process, so count at least root plus child.
	waitFor(t, func() bool { return len(owned.Members()) >= 2 }, "owned child")

	// An orphan: same executable, started outside the adapter.
	orphan := exec.Command(fakeExe, "-port", "0", "-load-delay", "1h")
	stderr, _ := orphan.StderrPipe()
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = orphan.Process.Kill(); _ = orphan.Wait() })
	if !waitLine(stderr, "listening", 10*time.Second) {
		t.Fatal("orphan did not start")
	}
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

	found, err := a.Reconcile(owned.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var hit bool
	for _, o := range found {
		for _, m := range owned.Members() {
			if int(o.PID) == m {
				t.Fatalf("reconcile touched owned process %d", m)
			}
		}
		if int(o.PID) == orphan.Process.Pid {
			hit = true
			if !o.Killed || o.Err != "" || !strings.EqualFold(filepath.Clean(o.Path), filepath.Clean(fakeExe)) {
				t.Fatalf("orphan record %+v", o)
			}
		}
	}
	if !hit {
		t.Fatalf("orphan %d not found in %+v", orphan.Process.Pid, found)
	}
	_ = orphan.Wait()
	select {
	case <-owned.Exited():
		t.Fatal("owned instance was killed")
	default:
	}
	if err := owned.WaitReady(context.Background()); err != nil {
		t.Fatalf("owned instance unhealthy after reconcile: %v", err)
	}

	// Once stopped, nothing of the owned instance is left to reconcile.
	if _, err := owned.Stop(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	if again, err := a.Reconcile(owned.cfg); err != nil || len(again) != 0 {
		t.Fatalf("second reconcile: %+v %v", again, err)
	}
}

func waitLine(r io.Reader, substr string, timeout time.Duration) bool {
	ch := make(chan bool, 1)
	go func() {
		s := bufio.NewScanner(r)
		for s.Scan() {
			if strings.Contains(s.Text(), substr) {
				ch <- true
				return
			}
		}
		ch <- false
	}()
	select {
	case ok := <-ch:
		return ok
	case <-time.After(timeout):
		return false
	}
}

func TestDiagnosticsRedaction(t *testing.T) {
	args := []string{"--api-key", "sk-live-123456", "--hf-token=hf_abcdef", "--api-key-file", `C:\secrets\key.txt`, "-fa", "on"}
	tl := newTail(4096)
	fmt.Fprintf(tl, "main: args --api-key sk-live-123456 --hf-token=hf_abcdef\n")
	fmt.Fprintf(tl, "request: Authorization: Bearer eyJhbGciOi.payload.sig\n")
	fmt.Fprintf(tl, "echo api key again: sk-live-123456\n")
	fmt.Fprintf(tl, "config: {\"password\": \"hunter22\", \"client_secret\":\"s3cr3t!\"}\n")
	fmt.Fprintf(tl, "reading --api-key-file C:\\secrets\\key.txt\n")
	fmt.Fprintf(tl, "llm_load_print_meta: tokenizer.ggml.model = gpt2\n")
	fmt.Fprintf(tl, "kv cache: 512 MiB\n")
	got := redact(tl.String(), secretValues(args))
	for _, leak := range []string{"sk-live-123456", "hf_abcdef", "eyJhbGciOi", "hunter22", "s3cr3t!"} {
		if strings.Contains(got, leak) {
			t.Errorf("leaked %q in:\n%s", leak, got)
		}
	}
	for _, keep := range []string{`C:\secrets\key.txt`, "tokenizer.ggml.model = gpt2", "kv cache: 512 MiB", "Bearer " + config.Redacted} {
		if !strings.Contains(got, keep) {
			t.Errorf("over-redacted, missing %q in:\n%s", keep, got)
		}
	}
}

func TestTailBounded(t *testing.T) {
	tl := newTail(64)
	for i := 0; i < 100; i++ {
		fmt.Fprintf(tl, "line %03d token=secretvalue%03d\n", i, i)
	}
	s := tl.String()
	if len(s) > 64 {
		t.Fatalf("tail %d bytes", len(s))
	}
	if !strings.HasPrefix(s, "line ") || !strings.HasSuffix(s, "099\n") {
		t.Fatalf("partial first line not dropped or last line missing: %q", s)
	}
	tl.Write([]byte(strings.Repeat("x", 200)))
	if s := tl.String(); s != "" {
		t.Fatalf("oversized write without newline should yield empty tail, got %d bytes", len(s))
	}
}
