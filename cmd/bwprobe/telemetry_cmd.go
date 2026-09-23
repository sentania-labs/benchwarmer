package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/signals"
	"github.com/sentania-labs/benchwarmer/internal/telemetry"
)

type sampleRecord struct {
	Kind             string                  `json:"kind"` // "sample"
	Label            string                  `json:"label"`
	Time             time.Time               `json:"time"`
	GPU              telemetry.Sample        `json:"gpu"`
	Demand           telemetry.Demand        `json:"demand"`
	OwnPIDs          []uint32                `json:"own_pids,omitempty"`
	Session          *signals.SessionSignals `json:"session,omitempty"`
	SnapshotDuration time.Duration           `json:"snapshot_duration_ns"`
}

type eventRecord struct {
	Kind  string    `json:"kind"` // "start", "mark", "gap", "rebuild", "processes", "end"
	Label string    `json:"label"`
	Time  time.Time `json:"time"`
	Text  string    `json:"text,omitempty"`
	Data  any       `json:"data,omitempty"`
}

type rebuilder interface{ Rebuild() error }

func cmdTelemetry(args []string) error {
	fs := flag.NewFlagSet("telemetry", flag.ExitOnError)
	label := fs.String("label", "", "scenario label, e.g. idle-desktop, game-active (required)")
	adapter := fs.String("adapter", "", "adapter LUID or name substring")
	interval := fs.Duration("interval", time.Second, "sample interval")
	duration := fs.Duration("duration", 5*time.Minute, "how long to record (0 = until Ctrl+C)")
	own := fs.String("own", "llama-server.exe", "process name treated as Benchwarmer's own runtime")
	out := fs.String("out", "", "JSONL output file (default: telemetry-<label>-<time>.jsonl)")
	procEvery := fs.Duration("process-list-every", 30*time.Second, "how often to record the full process list")
	quiet := fs.Bool("quiet", false, "do not print a status line per sample")
	_ = fs.Parse(args)
	if *label == "" {
		return errors.New("-label is required")
	}
	if *out == "" {
		*out = fmt.Sprintf("telemetry-%s-%s.jsonl", *label, time.Now().Format("20060102-150405"))
	}
	w, err := openJSONL(*out, false)
	if err != nil {
		return err
	}
	defer w.close()

	g, err := newGPUSource(*adapter)
	if err != nil {
		return err
	}
	defer g.Close()

	w.write(eventRecord{Kind: "start", Label: *label, Time: time.Now(), Data: collectEnv(*adapter)})
	fmt.Fprintf(os.Stderr, "recording %q to %s; type a note and press Enter to add a marker, Ctrl+C to stop\n", *label, *out)

	marks := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			marks <- sc.Text()
		}
	}()
	intr := make(chan os.Signal, 1)
	signal.Notify(intr, os.Interrupt)

	tick := time.NewTicker(*interval)
	defer tick.Stop()
	var end <-chan time.Time
	if *duration > 0 {
		end = time.After(*duration)
	}
	last := time.Now()
	lastProcs := time.Time{}
	failures := 0
	for {
		select {
		case <-intr:
			w.write(eventRecord{Kind: "end", Label: *label, Time: time.Now(), Text: "interrupted"})
			return nil
		case <-end:
			w.write(eventRecord{Kind: "end", Label: *label, Time: time.Now(), Text: "duration reached"})
			return nil
		case m := <-marks:
			w.write(eventRecord{Kind: "mark", Label: *label, Time: time.Now(), Text: m})
			fmt.Fprintf(os.Stderr, "marker recorded: %s\n", m)
		case now := <-tick.C:
			// A long gap between ticks means the machine slept or stalled.
			if gap := now.Sub(last); gap > 3**interval {
				w.write(eventRecord{Kind: "gap", Label: *label, Time: now, Text: gap.String()})
				if r, ok := g.(rebuilder); ok {
					err := r.Rebuild()
					w.write(eventRecord{Kind: "rebuild", Label: *label, Time: time.Now(), Text: errText(err, "after gap")})
				}
			}
			last = now

			t0 := time.Now()
			procs, _ := signals.Snapshot()
			snapDur := time.Since(t0)
			if now.Sub(lastProcs) >= *procEvery {
				lastProcs = now
				w.write(eventRecord{Kind: "processes", Label: *label, Time: now, Data: userProcesses(procs)})
			}

			s := g.Collect()
			names := map[uint32]string{}
			ownSet := map[uint32]bool{}
			var ownPIDs []uint32
			for _, p := range procs {
				names[p.PID] = p.Name
				if strings.EqualFold(p.Name, *own) {
					ownSet[p.PID] = true
					ownPIDs = append(ownPIDs, p.PID)
				}
			}
			for i := range s.Processes {
				s.Processes[i].Name = names[s.Processes[i].PID]
			}
			d := telemetry.Split(s, ownSet)
			rec := sampleRecord{Kind: "sample", Label: *label, Time: now, GPU: s, Demand: d, OwnPIDs: ownPIDs, SnapshotDuration: snapDur}
			if ss, err := signals.ReadSessionSignals(); err == nil {
				rec.Session = &ss
			}
			w.write(rec)

			if len(s.Errors) > 0 {
				failures++
			} else {
				failures = 0
			}
			if failures == 3 {
				if r, ok := g.(rebuilder); ok {
					err := r.Rebuild()
					w.write(eventRecord{Kind: "rebuild", Label: *label, Time: time.Now(), Text: errText(err, "after 3 failed samples")})
				}
			}
			if !*quiet {
				fmt.Fprintln(os.Stderr, statusLine(s, d, rec.Session))
			}
		}
	}
}

func errText(err error, ctx string) string {
	if err != nil {
		return ctx + ": " + err.Error()
	}
	return ctx + ": ok"
}

func userProcesses(procs []signals.Process) []signals.Process {
	var out []signals.Process
	for _, p := range procs {
		if p.SessionID != 0 {
			out = append(out, p)
		}
	}
	return out
}

func statusLine(s telemetry.Sample, d telemetry.Demand, ss *signals.SessionSignals) string {
	temp := "n/a"
	if s.TemperatureC != nil {
		temp = fmt.Sprintf("%.0fC", *s.TemperatureC)
	}
	top := ""
	if len(d.TopExternal) > 0 {
		p := d.TopExternal[0]
		top = fmt.Sprintf(" top=%s(%.0f%%,%dMiB)", p.Name, p.MaxUtil(), p.DedicatedBytes>>20)
	}
	fg := ""
	if ss != nil {
		fg = fmt.Sprintf(" fg=%s fs=%v idle=%.0fs", ss.ForegroundName, ss.Fullscreen, ss.IdleSeconds)
	}
	return fmt.Sprintf("%s util=%.0f%% own=%.0f%% ext=%.0f%% vram=%d/%dMiB ownvram=%dMiB temp=%s collect=%s%s%s",
		s.Time.Format("15:04:05"), d.TotalUtilPct, d.OwnUtilPct, d.ExternalUtilPct,
		s.DedicatedUsedBytes>>20, s.DedicatedTotalBytes>>20, d.OwnDedicatedBytes>>20, temp,
		s.CollectDuration.Round(time.Millisecond), top, fg)
}
