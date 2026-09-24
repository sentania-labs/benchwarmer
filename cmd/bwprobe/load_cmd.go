package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

// cmdLoad sends back-to-back streaming completions to a running runtime, to
// generate steady inference load during a telemetry recording.
func cmdLoad(args []string) error {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	url := fs.String("url", "http://127.0.0.1:18081", "runtime base URL")
	duration := fs.Duration("duration", 3*time.Minute, "how long to generate load")
	maxTokens := fs.Int("max-tokens", 512, "tokens per request")
	_ = fs.Parse(args)
	end := time.Now().Add(*duration)
	for i := 1; time.Now().Before(end); i++ {
		r := doChat(*url, "Write a long story about a lighthouse keeper.", *maxTokens, true, 5*time.Minute, nil)
		fmt.Fprintf(os.Stderr, "request %d: status=%d chunks=%d ttft=%dms total=%dms %s\n", i, r.Status, r.Chunks, r.TTFTms, r.TotalMs, r.Err)
		if r.Status != 200 {
			time.Sleep(time.Second)
		}
	}
	return nil
}
