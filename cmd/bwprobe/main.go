// Command bwprobe is the Phase 0 measurement tool. It records GPU telemetry,
// host signals, and runtime start/stop/VRAM-release behavior on the target
// machine so thresholds and telemetry sources are chosen from evidence.
// See docs/phase0/runbook.md.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

const usage = `bwprobe: Benchwarmer Phase 0 probe

Usage:
  bwprobe env        [-adapter SEL] [-out FILE]
  bwprobe telemetry  -label NAME [-adapter SEL] [-interval 1s] [-duration 5m] [-own NAME] [-out FILE]
  bwprobe runtime    -exe PATH -model PATH [-cycles 3] [-port 18081] [-args "..."] [-out FILE]
  bwprobe load       [-url http://127.0.0.1:18081] [-duration 3m]
  bwprobe svctest    -exe PATH -model PATH [-account virtual|system|localservice] [-out DIR]   (Windows, admin)
  bwprobe summarize  FILE.jsonl [FILE.jsonl ...]

Run "bwprobe <command> -h" for flags.`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "env":
		err = cmdEnv(os.Args[2:])
	case "telemetry":
		err = cmdTelemetry(os.Args[2:])
	case "runtime":
		err = cmdRuntime(os.Args[2:])
	case "load":
		err = cmdLoad(os.Args[2:])
	case "svctest":
		err = cmdSvcTest(os.Args[2:])
	case "svcrun":
		err = cmdSvcRun(os.Args[2:])
	case "summarize":
		err = cmdSummarize(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Println(usage)
		return
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bwprobe:", err)
		os.Exit(1)
	}
}

// jsonl writes one JSON record per line, to a file and optionally stdout.
type jsonl struct {
	mu   sync.Mutex
	f    *os.File
	echo bool
}

func openJSONL(path string, echo bool) (*jsonl, error) {
	j := &jsonl{echo: echo}
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		j.f = f
	}
	return j, nil
}

func (j *jsonl) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"marshal_error": err.Error()})
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f != nil {
		_, _ = j.f.Write(append(b, '\n'))
	}
	if j.echo {
		fmt.Println(string(b))
	}
}

func (j *jsonl) close() {
	if j.f != nil {
		_ = j.f.Close()
	}
}
