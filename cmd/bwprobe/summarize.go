package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// cmdSummarize turns probe JSONL files into a Markdown summary suitable for
// pasting into docs/phase0/findings.md.
func cmdSummarize(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: bwprobe summarize FILE.jsonl [FILE.jsonl]")
	}
	agg := newAggregator()
	for _, p := range args {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		err = agg.read(f)
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	agg.write(os.Stdout)
	return nil
}

type labelStats struct {
	n, complete, fullscreen, withSession int
	first, last                          time.Time
	total, own, ext, ext3D, extCompute   []float64
	vramUsed, vramExt, vramOwn, temp     []float64
	collectMs, snapMs                    []float64
	topExt, fg                           map[string]int
	errors                               map[string]int
}

type aggregator struct {
	labels map[string]*labelStats
	order  []string
	cycles []cycleResult
	marks  []eventRecord
	gaps   []eventRecord
}

func newAggregator() *aggregator { return &aggregator{labels: map[string]*labelStats{}} }

func (a *aggregator) stats(label string) *labelStats {
	s := a.labels[label]
	if s == nil {
		s = &labelStats{topExt: map[string]int{}, fg: map[string]int{}, errors: map[string]int{}}
		a.labels[label] = s
		a.order = append(a.order, label)
	}
	return s
}

func (a *aggregator) read(r io.Reader) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var k struct{ Kind string }
		if json.Unmarshal(line, &k) != nil {
			continue
		}
		switch k.Kind {
		case "sample":
			var rec sampleRecord
			if err := json.Unmarshal(line, &rec); err != nil {
				return err
			}
			a.addSample(rec)
		case "cycle":
			var c cycleResult
			if err := json.Unmarshal(line, &c); err != nil {
				return err
			}
			a.cycles = append(a.cycles, c)
		case "mark":
			var e eventRecord
			_ = json.Unmarshal(line, &e)
			a.marks = append(a.marks, e)
		case "gap":
			var e eventRecord
			_ = json.Unmarshal(line, &e)
			a.gaps = append(a.gaps, e)
		}
	}
	return sc.Err()
}

func (a *aggregator) addSample(r sampleRecord) {
	s := a.stats(r.Label)
	s.n++
	if s.first.IsZero() || r.Time.Before(s.first) {
		s.first = r.Time
	}
	if r.Time.After(s.last) {
		s.last = r.Time
	}
	if r.GPU.Complete {
		s.complete++
	}
	for _, e := range r.GPU.Errors {
		s.errors[e]++
	}
	d := r.Demand
	s.total = append(s.total, d.TotalUtilPct)
	s.own = append(s.own, d.OwnUtilPct)
	s.ext = append(s.ext, d.ExternalUtilPct)
	s.ext3D = append(s.ext3D, d.ExternalUtilByType["3D"])
	s.extCompute = append(s.extCompute, d.ExternalUtilByType["Compute"])
	s.vramUsed = append(s.vramUsed, mib(r.GPU.DedicatedUsedBytes))
	s.vramExt = append(s.vramExt, mib(d.ExternalDedicatedBytes))
	s.vramOwn = append(s.vramOwn, mib(d.OwnDedicatedBytes))
	if r.GPU.TemperatureC != nil {
		s.temp = append(s.temp, *r.GPU.TemperatureC)
	}
	s.collectMs = append(s.collectMs, float64(r.GPU.CollectDuration)/1e6)
	s.snapMs = append(s.snapMs, float64(r.SnapshotDuration)/1e6)
	if len(d.TopExternal) > 0 && d.TopExternal[0].MaxUtil() >= 5 {
		name := d.TopExternal[0].Name
		if name == "" {
			name = fmt.Sprintf("pid %d", d.TopExternal[0].PID)
		}
		s.topExt[name]++
	}
	if r.Session != nil {
		s.withSession++
		if r.Session.Fullscreen {
			s.fullscreen++
		}
		if r.Session.ForegroundName != "" {
			s.fg[r.Session.ForegroundName]++
		}
	}
}

func mib(b uint64) float64 { return float64(b) / (1 << 20) }

func pct(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	i := int(p * float64(len(c)-1))
	return c[i]
}

func dist(v []float64, unit string) string {
	if len(v) == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f / %.0f / %.0f%s", pct(v, 0.5), pct(v, 0.95), pct(v, 1), unit)
}

func topN(m map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	var s []kv
	for k, v := range m {
		s = append(s, kv{k, v})
	}
	sort.Slice(s, func(i, j int) bool { return s[i].v > s[j].v || (s[i].v == s[j].v && s[i].k < s[j].k) })
	var parts []string
	for i := 0; i < len(s) && i < n; i++ {
		parts = append(parts, fmt.Sprintf("%s (%d)", s[i].k, s[i].v))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func (a *aggregator) write(w io.Writer) {
	if len(a.order) > 0 {
		fmt.Fprintln(w, "## Telemetry scenarios")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Values are median / p95 / max.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "| Scenario | Samples | Total util | Own util | External util | Ext 3D | Ext Compute | VRAM used | External VRAM | Own VRAM | Temp | Collect cost |")
		fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|---|---|---|---|")
		for _, l := range a.order {
			s := a.labels[l]
			fmt.Fprintf(w, "| %s | %d (%d complete) | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
				l, s.n, s.complete, dist(s.total, "%"), dist(s.own, "%"), dist(s.ext, "%"), dist(s.ext3D, "%"), dist(s.extCompute, "%"),
				dist(s.vramUsed, " MiB"), dist(s.vramExt, " MiB"), dist(s.vramOwn, " MiB"), dist(s.temp, " C"), dist(s.collectMs, " ms"))
		}
		fmt.Fprintln(w)
		for _, l := range a.order {
			s := a.labels[l]
			fmt.Fprintf(w, "**%s** (%s to %s): top external GPU users: %s. Foreground: %s. Fullscreen in %d of %d session samples. Process snapshot cost %s.",
				l, s.first.Local().Format("15:04:05"), s.last.Local().Format("15:04:05"), topN(s.topExt, 5), topN(s.fg, 3), s.fullscreen, s.withSession, dist(s.snapMs, " ms"))
			if len(s.errors) > 0 {
				fmt.Fprintf(w, " Errors: %s.", topN(s.errors, 3))
			}
			fmt.Fprintln(w)
			fmt.Fprintln(w)
		}
	}
	if len(a.marks) > 0 || len(a.gaps) > 0 {
		fmt.Fprintln(w, "## Markers and gaps")
		fmt.Fprintln(w)
		for _, m := range a.marks {
			fmt.Fprintf(w, "- %s [%s] mark: %s\n", m.Time.Local().Format("2006-01-02 15:04:05"), m.Label, m.Text)
		}
		for _, g := range a.gaps {
			fmt.Fprintf(w, "- %s [%s] sampling gap of %s (sleep or stall)\n", g.Time.Local().Format("2006-01-02 15:04:05"), g.Label, g.Text)
		}
		fmt.Fprintln(w)
	}
	if len(a.cycles) > 0 {
		fmt.Fprintln(w, "## Runtime cycles")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "-1 means not measured (no data, or the event never happened within the timeout).")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "| # | Stop mode | Ready | Load s | Own VRAM MiB | Seen in counters | Engine types | Stream TTFT ms | Crashed first | Graceful exit | Stop->exit ms | Stop->tree empty ms | Stop->VRAM released ms | Counter gone ms | Leftovers | Busy client saw | Exit status |")
		fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
		for _, c := range a.cycles {
			ttft, busy := "", ""
			if c.Stream != nil {
				ttft = fmt.Sprint(c.Stream.TTFTms)
			}
			if c.BusyStream != nil {
				busy = c.BusyStream.Err
				if busy == "" {
					busy = "clean end"
				}
			}
			fmt.Fprintf(w, "| %d | %s | %v | %.1f | %d | %v | %s | %s | %v | %v | %d | %d | %d | %d | %s | %s | %s |\n",
				c.Cycle, c.Mode, c.Ready, c.LoadSeconds, c.OwnDedicatedMiB, c.OwnSeenInCounters, strings.Join(c.InferenceEngineTypes, ","), ttft,
				c.ExitedBeforeStop, c.GracefulExited, c.StopToRootExitMs, c.StopToTreeEmptyMs, c.StopToVRAMReleaseMs, c.StopToCounterGoneMs,
				strings.Join(c.LeftoverProcesses, ","), busy, c.ExitStatus)
		}
		fmt.Fprintln(w)
		for _, c := range a.cycles {
			if c.StartErr != "" || !c.Ready || c.ExitedBeforeStop || c.StopErr != "" {
				fmt.Fprintf(w, "Cycle %d: ready=%v crashed_first=%v start error: %q stop error: %q. stderr tail:\n\n```\n%s\n```\n\n",
					c.Cycle, c.Ready, c.ExitedBeforeStop, c.StartErr, c.StopErr, c.StderrTail)
			}
		}
	}
}
