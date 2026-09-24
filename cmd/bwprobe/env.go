package main

import (
	"flag"
	"os"
	"os/user"
	"runtime"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/signals"
)

type envReport struct {
	Time       time.Time         `json:"time"`
	Host       string            `json:"host"`
	OS         string            `json:"os"`
	User       string            `json:"user"`
	PID        int               `json:"pid"`
	Identity   any               `json:"identity,omitempty"`
	Adapters   any               `json:"adapters,omitempty"`
	AdapterErr string            `json:"adapters_error,omitempty"`
	Sessions   []signals.Session `json:"sessions,omitempty"`

	// Process visibility answers E7: can this identity read the image paths
	// of processes in other sessions?
	ProcVisibility map[uint32]procVis `json:"process_visibility_by_session,omitempty"`
	ProcErr        string             `json:"process_error,omitempty"`

	// GPU counter sample answers E4 and E6 at a glance.
	GPUSample any    `json:"gpu_sample,omitempty"`
	GPUErr    string `json:"gpu_error,omitempty"`

	SessionSignals any    `json:"session_signals,omitempty"`
	SessionErr     string `json:"session_signals_error,omitempty"`
}

type procVis struct {
	Total    int      `json:"total"`
	WithPath int      `json:"with_path"`
	Missing  []string `json:"missing_examples,omitempty"`
}

func cmdEnv(args []string) error {
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	adapter := fs.String("adapter", "", "adapter LUID or name substring (default: largest discrete)")
	out := fs.String("out", "", "also append the report to this JSONL file")
	_ = fs.Parse(args)
	w, err := openJSONL(*out, true)
	if err != nil {
		return err
	}
	defer w.close()
	w.write(collectEnv(*adapter))
	return nil
}

func collectEnv(adapter string) envReport {
	r := envReport{Time: time.Now(), OS: runtime.GOOS + "/" + runtime.GOARCH, PID: os.Getpid()}
	r.Host, _ = os.Hostname()
	if u, err := user.Current(); err == nil {
		r.User = u.Username
	}
	r.Identity = identity()
	if a, err := listAdapters(); err == nil {
		r.Adapters = a
	} else {
		r.AdapterErr = err.Error()
	}
	r.Sessions, _ = signals.Sessions()

	if procs, err := signals.Snapshot(); err == nil {
		r.ProcVisibility = map[uint32]procVis{}
		for _, p := range procs {
			v := r.ProcVisibility[p.SessionID]
			v.Total++
			if p.Path != "" {
				v.WithPath++
			} else if len(v.Missing) < 8 && p.PID > 4 {
				v.Missing = append(v.Missing, p.Name)
			}
			r.ProcVisibility[p.SessionID] = v
		}
	} else {
		r.ProcErr = err.Error()
	}

	if g, err := newGPUSource(adapter); err == nil {
		g.Collect() // prime rate counters
		time.Sleep(time.Second)
		s := g.Collect()
		g.Close()
		r.GPUSample = struct {
			Sample    any `json:"sample"`
			Processes int `json:"process_instances"`
			Engines   int `json:"engine_instances"`
		}{s, len(s.Processes), len(s.Engines)}
	} else {
		r.GPUErr = err.Error()
	}

	if ss, err := signals.ReadSessionSignals(); err == nil {
		r.SessionSignals = ss
	} else {
		r.SessionErr = err.Error()
	}
	return r
}
