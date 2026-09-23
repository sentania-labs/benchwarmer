package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/procgroup"
	"github.com/sentania-labs/benchwarmer/internal/signals"
)

type runtimeOpts struct {
	exe, model, extra, prompt, host string
	port, cycles, maxTokens         int
	loadTimeout, settle             time.Duration
	releaseTolerance                uint64
	adapter                         string
	logDir                          string
}

type cycleResult struct {
	Kind  string    `json:"kind"` // "cycle"
	Cycle int       `json:"cycle"`
	Mode  string    `json:"kill_mode"` // "idle" or "busy"
	Start time.Time `json:"start"`
	PID   int       `json:"pid"`

	StartErr string `json:"start_error,omitempty"`

	BaselineDedicatedMiB int64    `json:"baseline_dedicated_mib"`
	LoadSeconds          float64  `json:"load_seconds"`
	Ready                bool     `json:"ready"`
	LoadedDedicatedMiB   int64    `json:"loaded_dedicated_mib"`
	OwnDedicatedMiB      int64    `json:"own_dedicated_mib"`
	LoadedOwnUtilPeakPct float64  `json:"inference_own_util_peak_pct"`
	InferenceEngineTypes []string `json:"inference_engine_types,omitempty"`

	Completion *reqResult `json:"completion,omitempty"`
	Stream     *reqResult `json:"stream,omitempty"`
	BusyStream *reqResult `json:"busy_stream,omitempty"`

	KillToRootExitMs    int64    `json:"kill_to_root_exit_ms"`
	KillToTreeEmptyMs   int64    `json:"kill_to_tree_empty_ms"`
	KillErr             string   `json:"kill_error,omitempty"`
	KillToVRAMReleaseMs int64    `json:"kill_to_vram_release_ms"` // -1 = not confirmed
	VRAMAfterMiB        int64    `json:"vram_after_mib"`
	OwnCounterGoneMs    int64    `json:"own_counter_instances_gone_ms"` // -1 = not confirmed
	LeftoverProcesses   []string `json:"leftover_processes,omitempty"`
	StderrTail          string   `json:"stderr_tail,omitempty"`
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
	fs.IntVar(&o.port, "port", 18081, "private port for the runtime")
	fs.IntVar(&o.cycles, "cycles", 4, "load/serve/kill cycles; kill mode alternates idle, busy")
	fs.IntVar(&o.maxTokens, "max-tokens", 128, "max tokens per test completion")
	fs.StringVar(&o.prompt, "prompt", "Write a short paragraph about lighthouses.", "test prompt (not recorded)")
	fs.DurationVar(&o.loadTimeout, "load-timeout", 5*time.Minute, "max time to wait for readiness")
	fs.DurationVar(&o.settle, "settle", 10*time.Second, "pause between cycles")
	fs.Uint64Var(&o.releaseTolerance, "release-tolerance-mib", 256, "VRAM counts as released within this many MiB of baseline")
	fs.StringVar(&o.adapter, "adapter", "", "adapter LUID or name substring")
	out := fs.String("out", "", "JSONL output (default runtime-<time>.jsonl)")
	fs.StringVar(&o.logDir, "log-dir", "", "directory for per-cycle runtime stdout/stderr logs (default: next to -out)")
	_ = fs.Parse(args)
	if o.exe == "" || o.model == "" {
		return errors.New("-exe and -model are required")
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
	g, gerr := newGPUSource(o.adapter)
	if gerr != nil {
		fmt.Fprintln(os.Stderr, "note: GPU telemetry unavailable, VRAM measurements skipped:", gerr)
	} else {
		defer g.Close()
		g.Collect() // prime
	}
	w.write(eventRecord{Kind: "start", Label: "runtime", Time: time.Now(), Data: map[string]any{
		"exe": o.exe, "model": filepath.Base(o.model), "args": o.extra, "cycles": o.cycles}})
	for i := 1; i <= o.cycles; i++ {
		mode := "idle"
		if i%2 == 0 {
			mode = "busy"
		}
		r := runCycle(o, g, i, mode, w)
		w.write(r)
		fmt.Fprintf(os.Stderr, "cycle %d (%s): ready=%v load=%.1fs kill->exit=%dms kill->tree-empty=%dms kill->vram=%dms leftovers=%v %s\n",
			i, mode, r.Ready, r.LoadSeconds, r.KillToRootExitMs, r.KillToTreeEmptyMs, r.KillToVRAMReleaseMs, r.LeftoverProcesses, r.StartErr)
		if i < o.cycles {
			time.Sleep(o.settle)
		}
	}
	return nil
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

func runCycle(o runtimeOpts, g gpuSource, n int, mode string, w *jsonl) cycleResult {
	r := cycleResult{Kind: "cycle", Cycle: n, Mode: mode, Start: time.Now(), KillToVRAMReleaseMs: -1, OwnCounterGoneMs: -1}
	var baseline uint64
	if g != nil {
		baseline = g.Collect().DedicatedUsedBytes
		r.BaselineDedicatedMiB = int64(baseline >> 20)
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
	pg, err := procgroup.Start(procgroup.Spec{Path: o.exe, Args: args, Stdout: sink, Stderr: sink})
	if err != nil {
		r.StartErr = err.Error()
		return r
	}
	defer pg.Close()
	r.PID = pg.PID()
	base := fmt.Sprintf("http://%s:%d", o.host, o.port)

	// Background VRAM/util timeline for the whole cycle.
	stopTL := make(chan struct{})
	var tlMu sync.Mutex
	var peakOwnUtil float64
	engTypes := map[string]bool{}
	tlDone := make(chan struct{})
	go func() {
		defer close(tlDone)
		if g == nil {
			return
		}
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stopTL:
				return
			case <-t.C:
				s := g.Collect()
				members, _ := pg.Members()
				own := map[uint32]bool{}
				for _, m := range members {
					own[uint32(m)] = true
				}
				var ownUtil float64
				var ownMem uint64
				tlMu.Lock()
				for _, p := range s.Processes {
					if own[p.PID] {
						ownMem += p.DedicatedBytes
						for typ, u := range p.EngineUtil {
							ownUtil = max(ownUtil, u)
							if u > 1 {
								engTypes[typ] = true
							}
						}
					}
				}
				peakOwnUtil = max(peakOwnUtil, ownUtil)
				tlMu.Unlock()
				w.write(map[string]any{"kind": "vram", "cycle": n, "time": s.Time,
					"adapter_dedicated_mib": s.DedicatedUsedBytes >> 20, "own_dedicated_mib": ownMem >> 20,
					"own_util_pct": ownUtil, "temperature_c": s.TemperatureC, "collect_ms": s.CollectDuration.Milliseconds()})
			}
		}
	}()

	t0 := time.Now()
	r.Ready = waitReady(base, o.loadTimeout, pg)
	r.LoadSeconds = time.Since(t0).Seconds()
	if r.Ready {
		if g != nil {
			time.Sleep(time.Second)
			s := g.Collect()
			r.LoadedDedicatedMiB = int64(s.DedicatedUsedBytes >> 20)
			members, _ := pg.Members()
			for _, p := range s.Processes {
				for _, m := range members {
					if uint32(m) == p.PID {
						r.OwnDedicatedMiB += int64(p.DedicatedBytes >> 20)
					}
				}
			}
		}
		r.Completion = doChat(base, o.prompt, o.maxTokens, false, 0)
		r.Stream = doChat(base, o.prompt, o.maxTokens, true, 0)
	}

	var killAt time.Time
	if mode == "busy" && r.Ready {
		// Kill one second into a streaming response, as a forced preemption would.
		res := make(chan *reqResult, 1)
		go func() { res <- doChat(base, o.prompt, 4*o.maxTokens, true, 0) }()
		time.Sleep(time.Second)
		killAt = time.Now()
		if err := pg.Kill(); err != nil {
			r.KillErr = err.Error()
		}
		select {
		case br := <-res:
			r.BusyStream = br
		case <-time.After(10 * time.Second):
			r.BusyStream = &reqResult{Err: "client did not observe termination within 10s"}
		}
	} else {
		killAt = time.Now()
		if err := pg.Kill(); err != nil && !errors.Is(err, procgroup.ErrNotRunning) {
			r.KillErr = err.Error()
		}
	}
	select {
	case <-pg.Done():
		r.KillToRootExitMs = time.Since(killAt).Milliseconds()
	case <-time.After(30 * time.Second):
		r.KillToRootExitMs = -1
	}
	for time.Since(killAt) < 30*time.Second {
		if m, _ := pg.Members(); len(m) == 0 {
			r.KillToTreeEmptyMs = time.Since(killAt).Milliseconds()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	close(stopTL)
	<-tlDone
	tlMu.Lock()
	r.LoadedOwnUtilPeakPct = peakOwnUtil
	for t := range engTypes {
		r.InferenceEngineTypes = append(r.InferenceEngineTypes, t)
	}
	tlMu.Unlock()

	if g != nil {
		tol := o.releaseTolerance << 20
		for time.Since(killAt) < 60*time.Second {
			s := g.Collect()
			r.VRAMAfterMiB = int64(s.DedicatedUsedBytes >> 20)
			if r.OwnCounterGoneMs < 0 {
				gone := true
				for _, p := range s.Processes {
					if int(p.PID) == r.PID {
						gone = false
					}
				}
				if gone {
					r.OwnCounterGoneMs = time.Since(killAt).Milliseconds()
				}
			}
			if r.KillToVRAMReleaseMs < 0 && s.DedicatedUsedBytes <= baseline+tol {
				r.KillToVRAMReleaseMs = time.Since(killAt).Milliseconds()
			}
			if r.KillToVRAMReleaseMs >= 0 && r.OwnCounterGoneMs >= 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	exeName := filepath.Base(o.exe)
	if procs, err := signals.Snapshot(); err == nil {
		for _, p := range procs {
			if strings.EqualFold(p.Name, exeName) {
				r.LeftoverProcesses = append(r.LeftoverProcesses, fmt.Sprintf("%s(pid %d)", p.Name, p.PID))
			}
		}
	}
	if !r.Ready {
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

func doChat(base, prompt string, maxTokens int, stream bool, timeout time.Duration) *reqResult {
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
		if res.Chunks == 0 {
			res.TTFTms = time.Since(t0).Milliseconds()
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
