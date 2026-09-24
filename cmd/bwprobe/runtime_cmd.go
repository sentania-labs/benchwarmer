package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/gpureset"
	"github.com/sentania-labs/benchwarmer/internal/procgroup"
	"github.com/sentania-labs/benchwarmer/internal/signals"
	"github.com/sentania-labs/benchwarmer/internal/telemetry"
)

type runtimeOpts struct {
	exe, model, extra, prompt, host string
	port, cycles, maxTokens         int
	loadTimeout, settle             time.Duration
	releaseTolerance                uint64
	adapter                         string
	logDir                          string
	modes                           []string
	runAs                           string
	serveFor                        time.Duration
}

// Kill modes. "graceful" sends Ctrl+C (SIGINT on Unix) and falls back to a
// job kill; it answers the spec's question of whether a graceful stop
// releases VRAM differently from a hard kill.
const (
	modeIdle     = "idle"
	modeBusy     = "busy"
	modeGraceful = "graceful"
)

// Fields are -1 when the probe could not measure them. Zero is a measurement.
type cycleResult struct {
	Kind  string    `json:"kind"` // "cycle"
	Cycle int       `json:"cycle"`
	Mode  string    `json:"kill_mode"`
	Start time.Time `json:"start"`
	PID   int       `json:"pid"`

	StartErr string `json:"start_error,omitempty"`

	BaselineDedicatedMiB int64    `json:"baseline_dedicated_mib"`
	LoadSeconds          float64  `json:"load_seconds"`
	Ready                bool     `json:"ready"`
	LoadedDedicatedMiB   int64    `json:"loaded_dedicated_mib"`
	OwnDedicatedMiB      int64    `json:"own_dedicated_mib"`
	OwnSeenInCounters    bool     `json:"own_seen_in_counters"`
	OwnUtilPeakPct       float64  `json:"inference_own_util_peak_pct"`
	InferenceEngineTypes []string `json:"inference_engine_types,omitempty"`

	Completion *reqResult `json:"completion,omitempty"`
	Stream     *reqResult `json:"stream,omitempty"`
	BusyStream *reqResult `json:"busy_stream,omitempty"`

	// ExitedBeforeStop is true when the runtime died on its own (crash)
	// before the probe stopped it; ExitStatus records how it ended.
	ExitedBeforeStop bool   `json:"exited_before_stop"`
	ExitStatus       string `json:"exit_status,omitempty"`

	// GracefulExited: the runtime exited after Ctrl+C without a job kill.
	GracefulExited      bool  `json:"graceful_exited,omitempty"`
	StopToRootExitMs    int64 `json:"stop_to_root_exit_ms"`
	StopToTreeEmptyMs   int64 `json:"stop_to_tree_empty_ms"`
	StopToVRAMReleaseMs int64 `json:"stop_to_vram_release_ms"`
	// StopToCounterGoneMs: until no owned PID has a GPU counter instance.
	// Only measured when an owned PID was seen in the counters after load.
	StopToCounterGoneMs int64    `json:"stop_to_counter_instances_gone_ms"`
	VRAMAfterMiB        int64    `json:"vram_after_mib"`
	StopErr             string   `json:"stop_error,omitempty"`
	LeftoverProcesses   []string `json:"leftover_processes,omitempty"`
	StderrTail          string   `json:"stderr_tail,omitempty"`
	// RuntimeUser and RuntimePrivileges are read from the running process's
	// token: evidence of the identity it actually ran under.
	RuntimeUser       string   `json:"runtime_user,omitempty"`
	RuntimePrivileges []string `json:"runtime_privileges,omitempty"`
	RuntimeIntegrity  string   `json:"runtime_integrity,omitempty"`
	SoakRequests      int      `json:"soak_requests,omitempty"`
	SoakFailures      int      `json:"soak_failures,omitempty"`
}

type reqResult struct {
	Status    int    `json:"status"`
	Err       string `json:"error,omitempty"`
	TTFTms    int64  `json:"ttft_ms,omitempty"`
	TotalMs   int64  `json:"total_ms"`
	Chunks    int    `json:"chunks,omitempty"`
	SawDone   bool   `json:"saw_done,omitempty"`
	BytesRead int    `json:"bytes_read"`
}

func cmdRuntime(args []string) error {
	fs := flag.NewFlagSet("runtime", flag.ExitOnError)
	var o runtimeOpts
	fs.StringVar(&o.exe, "exe", "", "path to llama-server executable (required)")
	fs.StringVar(&o.model, "model", "", "path to GGUF model (required)")
	fs.StringVar(&o.extra, "args", "-ngl 999 -c 8192", "extra llama-server arguments (space separated)")
	fs.StringVar(&o.host, "host", "127.0.0.1", "loopback host for the runtime")
	fs.IntVar(&o.port, "port", 18081, "private port for the runtime (must be free)")
	fs.IntVar(&o.cycles, "cycles", 6, "load/serve/stop cycles; stop mode rotates through -modes")
	modes := fs.String("modes", "idle,busy,graceful", "stop modes to rotate through: idle, busy, graceful")
	fs.IntVar(&o.maxTokens, "max-tokens", 128, "max tokens per test completion")
	fs.StringVar(&o.prompt, "prompt", "Write a short paragraph about lighthouses.", "test prompt (not recorded)")
	fs.DurationVar(&o.loadTimeout, "load-timeout", 5*time.Minute, "max time to wait for readiness")
	fs.DurationVar(&o.settle, "settle", 10*time.Second, "pause between cycles")
	fs.Uint64Var(&o.releaseTolerance, "release-tolerance-mib", 256, "VRAM counts as released within this many MiB of baseline")
	fs.StringVar(&o.adapter, "adapter", "", "adapter LUID or name substring")
	fs.DurationVar(&o.serveFor, "serve-for", 0, "before stopping, keep sending completions for this long (long-lived runtime test)")
	fs.StringVar(&o.runAs, "runtime-as", "", "runtime identity: empty (same as probe) or localservice (probe must run as LocalSystem)")
	out := fs.String("out", "", "JSONL output (default runtime-<time>.jsonl)")
	fs.StringVar(&o.logDir, "log-dir", "", "directory for per-cycle runtime stdout/stderr logs (default: next to -out)")
	_ = fs.Parse(args)
	if o.exe == "" || o.model == "" {
		return errors.New("-exe and -model are required")
	}
	for _, m := range strings.Split(*modes, ",") {
		switch m = strings.TrimSpace(m); m {
		case modeIdle, modeBusy, modeGraceful:
			o.modes = append(o.modes, m)
		default:
			return fmt.Errorf("unknown stop mode %q", m)
		}
	}
	for _, m := range o.modes {
		if m == modeGraceful && runtime.GOOS == "windows" && !hasConsole() {
			return errors.New("graceful mode needs a console (run from a terminal or a scheduled task), otherwise it would fall back to a hard kill")
		}
	}
	if *out == "" {
		*out = "runtime-" + time.Now().Format("20060102-150405") + ".jsonl"
	}
	if o.logDir == "" {
		o.logDir = filepath.Dir(*out)
	}
	w, err := openJSONL(*out, false)
	if err != nil {
		return err
	}
	defer w.close()
	return runCycles(o, w)
}

func runCycles(o runtimeOpts, w *jsonl) error {
	if len(o.modes) == 0 {
		o.modes = []string{modeIdle, modeBusy}
	}
	g, gerr := newGPUSource(o.adapter)
	if gerr != nil {
		fmt.Fprintln(os.Stderr, "note: GPU telemetry unavailable, VRAM measurements skipped:", gerr)
		w.write(eventRecord{Kind: "note", Label: "runtime", Time: time.Now(), Text: "GPU telemetry unavailable: " + gerr.Error()})
	} else {
		defer g.Close()
		g.Collect() // prime rate counters
	}
	w.write(eventRecord{Kind: "start", Label: "runtime", Time: time.Now(), Data: map[string]any{
		"exe": o.exe, "model": filepath.Base(o.model), "args": o.extra, "cycles": o.cycles, "modes": o.modes}})
	// Tripwire: on the first GPU driver reset, kill the runtime and stop
	// the whole run. On the target a second hang a minute after the first
	// escalated to a blue screen.
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		watcher := gpureset.New(gpureset.DefaultDirs())
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-t.C:
			}
			if resets := watcher.Poll(); len(resets) > 0 {
				tripped.Store(true)
				killCurrent()
				w.write(map[string]any{"kind": "gpu_reset", "time": time.Now(), "resets": resets})
				fmt.Fprintf(os.Stderr, "GPU driver reset detected (%v); runtime killed, run aborted\n", resets)
				return
			}
		}
	}()
	for i := 1; i <= o.cycles && !tripped.Load(); i++ {
		mode := o.modes[(i-1)%len(o.modes)]
		r := runCycle(o, g, i, mode, w)
		w.write(r)
		fmt.Fprintf(os.Stderr, "cycle %d (%s): ready=%v load=%.1fs stop->exit=%dms stop->tree-empty=%dms stop->vram=%dms graceful=%v crashed=%v leftovers=%v %s\n",
			i, mode, r.Ready, r.LoadSeconds, r.StopToRootExitMs, r.StopToTreeEmptyMs, r.StopToVRAMReleaseMs, r.GracefulExited, r.ExitedBeforeStop, r.LeftoverProcesses, r.StartErr)
		if i < o.cycles && !tripped.Load() {
			time.Sleep(o.settle)
		}
	}
	if tripped.Load() {
		return errors.New("aborted after a GPU driver reset")
	}
	return nil
}

var (
	tripped   atomic.Bool
	currentMu sync.Mutex
	current   *procgroup.Group
)

func setCurrent(g *procgroup.Group) { currentMu.Lock(); current = g; currentMu.Unlock() }

func killCurrent() {
	currentMu.Lock()
	defer currentMu.Unlock()
	if current != nil {
		_ = current.Kill()
	}
}

// ring keeps the tail of a stream.
type ring struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
	return len(p), nil
}

func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

// timeline samples GPU telemetry in the background for one cycle. It is the
// only caller of Collect while running, so rate intervals stay even.
type timeline struct {
	mu        sync.Mutex
	latest    *telemetry.Sample
	seenOwn   map[uint32]bool // owned PIDs seen with a counter instance
	peakUtil  float64
	engTypes  map[string]bool
	stop      chan struct{}
	done      chan struct{}
	available bool
}

func startTimeline(g gpuSource, pg *procgroup.Group, cycle int, w *jsonl) *timeline {
	tl := &timeline{seenOwn: map[uint32]bool{}, engTypes: map[string]bool{}, stop: make(chan struct{}), done: make(chan struct{}), available: g != nil}
	go func() {
		defer close(tl.done)
		if g == nil {
			return
		}
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-tl.stop:
				return
			case <-t.C:
			}
			s := g.Collect()
			members, _ := pg.Members()
			own := map[uint32]bool{}
			for _, m := range members {
				own[uint32(m)] = true
			}
			var ownUtil float64
			var ownMem uint64
			tl.mu.Lock()
			tl.latest = &s
			if s.Complete {
				for _, p := range s.Processes {
					if !own[p.PID] {
						continue
					}
					tl.seenOwn[p.PID] = true
					ownMem += p.DedicatedBytes
					for typ, u := range p.EngineUtil {
						ownUtil = max(ownUtil, u)
						if u > 1 {
							tl.engTypes[typ] = true
						}
					}
				}
				tl.peakUtil = max(tl.peakUtil, ownUtil)
			}
			tl.mu.Unlock()
			w.write(map[string]any{"kind": "vram", "cycle": cycle, "time": s.Time, "complete": s.Complete,
				"adapter_dedicated_mib": s.DedicatedUsedBytes >> 20, "own_dedicated_mib": ownMem >> 20,
				"own_util_pct": ownUtil, "temperature_c": s.TemperatureC, "collect_ms": s.CollectDuration.Milliseconds()})
		}
	}()
	return tl
}

// waitComplete returns the next complete sample newer than after.
func (tl *timeline) waitComplete(after time.Time, timeout time.Duration) (telemetry.Sample, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		tl.mu.Lock()
		s := tl.latest
		tl.mu.Unlock()
		if s != nil && s.Complete && s.Time.After(after) {
			return *s, true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return telemetry.Sample{}, false
}

func (tl *timeline) close() {
	close(tl.stop)
	<-tl.done
}

// runtimePath returns the full path used to recognise leftover runtimes.
func runtimePath(exe string) string {
	if p, err := filepath.Abs(exe); err == nil {
		return p
	}
	return exe
}

func sameRuntime(p signals.Process, full, base string) bool {
	if p.Path != "" {
		return strings.EqualFold(filepath.Clean(p.Path), filepath.Clean(full))
	}
	return strings.EqualFold(p.Name, base)
}

func runCycle(o runtimeOpts, g gpuSource, n int, mode string, w *jsonl) cycleResult {
	r := cycleResult{Kind: "cycle", Cycle: n, Mode: mode, Start: time.Now(), PID: -1,
		BaselineDedicatedMiB: -1, LoadedDedicatedMiB: -1, OwnDedicatedMiB: -1,
		StopToRootExitMs: -1, StopToTreeEmptyMs: -1, StopToVRAMReleaseMs: -1, StopToCounterGoneMs: -1, VRAMAfterMiB: -1}

	// Refuse to start if something already listens on the port: readiness
	// would be measured against the wrong process.
	addr := net.JoinHostPort(o.host, strconv.Itoa(o.port))
	if ln, err := net.Listen("tcp", addr); err != nil {
		r.StartErr = fmt.Sprintf("port %s is in use (another llama-server running?): %v", addr, err)
		return r
	} else {
		ln.Close()
	}

	full, base := runtimePath(o.exe), filepath.Base(o.exe)
	preexisting := map[uint32]bool{}
	if procs, err := signals.Snapshot(); err == nil {
		for _, p := range procs {
			if sameRuntime(p, full, base) {
				preexisting[p.PID] = true
			}
		}
	}

	logf, _ := os.Create(filepath.Join(o.logDir, fmt.Sprintf("runtime-cycle%02d.log", n)))
	if logf != nil {
		defer logf.Close()
	}
	tail := &ring{max: 4096}
	var sink io.Writer = tail
	if logf != nil {
		sink = io.MultiWriter(tail, logf)
	}
	args := append([]string{"-m", o.model, "--host", o.host, "--port", strconv.Itoa(o.port)}, strings.Fields(o.extra)...)
	// Graceful mode on Windows needs the child on our console for Ctrl+C.
	share := mode == modeGraceful && runtime.GOOS == "windows"
	pg, err := procgroup.Start(procgroup.Spec{Path: o.exe, Args: args, Stdout: sink, Stderr: sink, ShareConsole: share, RunAs: o.runAs})
	if err != nil {
		r.StartErr = err.Error()
		return r
	}
	defer pg.Close()
	setCurrent(pg)
	defer setCurrent(nil)
	if tripped.Load() {
		_ = pg.Kill()
	}
	r.PID = pg.PID()
	baseURL := fmt.Sprintf("http://%s", addr)

	// Baseline VRAM: the first complete sample after start, before the model
	// has had time to allocate. Taken from a complete sample or not at all.
	tl := startTimeline(g, pg, n, w)
	var baseline uint64
	haveBaseline := false
	if tl.available {
		if s, ok := tl.waitComplete(r.Start, 3*time.Second); ok {
			baseline, haveBaseline = s.DedicatedUsedBytes, true
			r.BaselineDedicatedMiB = int64(baseline >> 20)
		}
	}

	if u, privs, err := signals.ProcessIdentity(uint32(pg.PID())); err == nil {
		r.RuntimeUser, r.RuntimePrivileges = u, privs
	} else {
		r.RuntimeUser = "unknown: " + err.Error()
	}
	r.RuntimeIntegrity, _ = signals.ProcessIntegrity(uint32(pg.PID()))
	t0 := time.Now()
	r.Ready = waitReady(baseURL, o.loadTimeout, pg)
	r.LoadSeconds = time.Since(t0).Seconds()
	if r.Ready {
		if s, ok := tl.waitComplete(time.Now().Add(500*time.Millisecond), 5*time.Second); ok {
			r.LoadedDedicatedMiB = int64(s.DedicatedUsedBytes >> 20)
			members, _ := pg.Members()
			var own int64
			for _, p := range s.Processes {
				for _, m := range members {
					if uint32(m) == p.PID {
						own += int64(p.DedicatedBytes >> 20)
					}
				}
			}
			r.OwnDedicatedMiB = own
		}
		r.Completion = doChat(baseURL, o.prompt, o.maxTokens, false, 0, nil)
		r.Stream = doChat(baseURL, o.prompt, o.maxTokens, true, 0, nil)
		if o.serveFor > 0 {
			end := time.Now().Add(o.serveFor)
			for time.Now().Before(end) && !tripped.Load() {
				res := doChat(baseURL, o.prompt, o.maxTokens, false, 2*time.Minute, nil)
				r.SoakRequests++
				if res.Status != 200 {
					r.SoakFailures++
				}
			}
		}
	}

	// Was the runtime still alive when we went to stop it?
	select {
	case <-pg.Done():
		r.ExitedBeforeStop = true
	default:
	}

	var stopAt time.Time
	switch {
	case r.ExitedBeforeStop:
		stopAt = time.Now()
	case mode == modeBusy && r.Ready:
		// Kill mid-stream, as a forced preemption would: after the first
		// token arrives, not at a fixed delay that prompt processing might
		// outlast.
		first := make(chan struct{})
		res := make(chan *reqResult, 1)
		go func() { res <- doChat(baseURL, o.prompt, 4*o.maxTokens, true, 0, first) }()
		select {
		case <-first:
			time.Sleep(500 * time.Millisecond)
		case <-time.After(2 * time.Minute):
		}
		stopAt = time.Now()
		if err := pg.Kill(); err != nil && !errors.Is(err, procgroup.ErrNotRunning) {
			r.StopErr = err.Error()
		}
		select {
		case br := <-res:
			r.BusyStream = br
		case <-time.After(10 * time.Second):
			r.BusyStream = &reqResult{Err: "client did not observe termination within 10s"}
		}
	case mode == modeGraceful:
		stopAt = time.Now()
		if err := pg.Interrupt(); err != nil {
			r.StopErr = "interrupt: " + err.Error()
		}
		select {
		case <-pg.Done():
			r.GracefulExited = true
		case <-time.After(60 * time.Second):
			r.StopErr = strings.TrimPrefix(r.StopErr+"; no exit 60s after Ctrl+C, job kill", "; ")
			_ = pg.Kill()
		}
	default:
		stopAt = time.Now()
		if err := pg.Kill(); err != nil && !errors.Is(err, procgroup.ErrNotRunning) {
			r.StopErr = err.Error()
		}
	}

	select {
	case <-pg.Done():
		if !r.ExitedBeforeStop {
			r.StopToRootExitMs = time.Since(stopAt).Milliseconds()
		}
	case <-time.After(30 * time.Second):
		r.StopErr = strings.TrimPrefix(r.StopErr+"; root did not exit within 30s", "; ")
	}
	if e := pg.ExitErr(); e != nil {
		r.ExitStatus = e.Error()
	} else {
		r.ExitStatus = "exit 0"
	}
	for time.Since(stopAt) < 30*time.Second {
		if m, err := pg.Members(); err == nil && len(m) == 0 {
			r.StopToTreeEmptyMs = time.Since(stopAt).Milliseconds()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// VRAM release: only complete samples count, and only against a real
	// baseline. Counter instances: only if an owned PID was ever seen.
	if tl.available {
		tl.mu.Lock()
		seen := make(map[uint32]bool, len(tl.seenOwn))
		for p := range tl.seenOwn {
			seen[p] = true
		}
		tl.mu.Unlock()
		r.OwnSeenInCounters = len(seen) > 0
		tol := o.releaseTolerance << 20
		last := stopAt
		for time.Since(stopAt) < 60*time.Second {
			s, ok := tl.waitComplete(last, 2*time.Second)
			if !ok {
				continue
			}
			last = s.Time
			r.VRAMAfterMiB = int64(s.DedicatedUsedBytes >> 20)
			if r.OwnSeenInCounters && r.StopToCounterGoneMs < 0 {
				gone := true
				for _, p := range s.Processes {
					if seen[p.PID] {
						gone = false
					}
				}
				if gone {
					r.StopToCounterGoneMs = s.Time.Sub(stopAt).Milliseconds()
				}
			}
			if haveBaseline && r.StopToVRAMReleaseMs < 0 && s.DedicatedUsedBytes <= baseline+tol {
				r.StopToVRAMReleaseMs = s.Time.Sub(stopAt).Milliseconds()
			}
			if (!haveBaseline || r.StopToVRAMReleaseMs >= 0) && (!r.OwnSeenInCounters || r.StopToCounterGoneMs >= 0) {
				break
			}
		}
		tl.mu.Lock()
		r.OwnUtilPeakPct = tl.peakUtil
		for t := range tl.engTypes {
			r.InferenceEngineTypes = append(r.InferenceEngineTypes, t)
		}
		tl.mu.Unlock()
	}
	tl.close()

	// Leftovers: runtime processes (same executable path) that did not exist
	// before this cycle. Checked while the job handle is still open, so this
	// tests TerminateJobObject, not kill-on-close.
	if procs, err := signals.Snapshot(); err == nil {
		for _, p := range procs {
			if sameRuntime(p, full, base) && !preexisting[p.PID] {
				r.LeftoverProcesses = append(r.LeftoverProcesses, fmt.Sprintf("%s(pid %d)", p.Name, p.PID))
			}
		}
	}
	if !r.Ready || r.ExitedBeforeStop {
		r.StderrTail = tail.String()
	}
	return r
}

func waitReady(base string, timeout time.Duration, pg *procgroup.Group) bool {
	deadline := time.Now().Add(timeout)
	c := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		select {
		case <-pg.Done():
			return false
		default:
		}
		resp, err := c.Get(base + "/health")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// doChat sends one chat completion. For streams, firstChunk (if non-nil) is
// closed when the first data line arrives.
func doChat(base, prompt string, maxTokens int, stream bool, timeout time.Duration, firstChunk chan struct{}) *reqResult {
	body := fmt.Sprintf(`{"messages":[{"role":"user","content":%q}],"max_tokens":%d,"stream":%v}`, prompt, maxTokens, stream)
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	t0 := time.Now()
	res := &reqResult{}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		res.Err = err.Error()
		res.TotalMs = time.Since(t0).Milliseconds()
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	if !stream {
		b, err := io.ReadAll(resp.Body)
		res.BytesRead = len(b)
		if err != nil {
			res.Err = err.Error()
		}
		res.TotalMs = time.Since(t0).Milliseconds()
		return res
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		res.BytesRead += len(line) + 1
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if res.Chunks == 0 && !res.SawDone {
			res.TTFTms = time.Since(t0).Milliseconds()
			if firstChunk != nil {
				close(firstChunk)
				firstChunk = nil
			}
		}
		if line == "data: [DONE]" {
			res.SawDone = true
			continue
		}
		res.Chunks++
	}
	if err := sc.Err(); err != nil {
		res.Err = err.Error()
	} else if !res.SawDone {
		res.Err = "stream ended without [DONE]"
	}
	res.TotalMs = time.Since(t0).Milliseconds()
	return res
}
