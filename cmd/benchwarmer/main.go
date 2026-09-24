// Command benchwarmer is the Benchwarmer Windows service: it runs a local LLM
// on the GPU only while the GPU is otherwise idle and yields to the human.
//
// Usage:
//
//	benchwarmer run [--data DIR] [--service] [--simulate-gpu [--sim-control FILE]]
//	benchwarmer service install [--account system] [--data DIR]
//	benchwarmer service remove
//	benchwarmer service restart [--delay D]
//	benchwarmer config default
//	benchwarmer config validate FILE
//	benchwarmer login [--code CODE] [--data DIR]
//	benchwarmer version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/logfile"
	"github.com/sentania-labs/benchwarmer/internal/procgroup"
	"github.com/sentania-labs/benchwarmer/internal/service"
	"github.com/sentania-labs/benchwarmer/internal/version"
	"github.com/sentania-labs/benchwarmer/internal/webui"
	"github.com/sentania-labs/benchwarmer/internal/winsvc"
)

const serviceName = "Benchwarmer"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "benchwarmer:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: benchwarmer run|service|config|login|version (see package documentation)")
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "service":
		return cmdService(args[1:])
	case "config":
		return cmdConfig(args[1:])
	case "login":
		return cmdLogin(args[1:])
	case "version":
		fmt.Println(version.Version)
		return nil
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	data := fs.String("data", service.DefaultDataDir(), "data directory (config, database, secrets, logs)")
	asService := fs.Bool("service", false, "run under the Windows service control manager")
	sim := fs.Bool("simulate-gpu", false, "development only: simulate the GPU instead of reading Windows telemetry")
	simCtl := fs.String("sim-control", "", "development only: JSON file injecting simulated competing load")
	_ = fs.Parse(args)

	// The service logs to a file, but the file is opened only after
	// service.New has provisioned the data folder (ADR 0012): before that
	// the config and the logs folder may have been planted by a non-admin,
	// and SYSTEM must not write where they point. Early lines are held in
	// memory and written once the file is open.
	var out io.Writer = os.Stderr
	early := &earlyLog{}
	if *asService {
		out = early
	}
	cfg, _ := peekConfig(*data) // log level only
	lv := new(slog.LevelVar)
	lv.Set(level(cfg.Logging.Level))
	log := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: lv}))
	slog.SetDefault(log)

	if *asService {
		// A service has no console; give it one so the runtime can be
		// stopped with Ctrl+C rather than killed (ADR 0002).
		if err := procgroup.EnsureConsole(); err != nil {
			log.Warn("no console for graceful runtime stop; runtime stops will hard-kill", "err", err)
		}
	}
	so := service.Options{DataDir: *data, SimulateGPU: *sim, SimControl: *simCtl, Log: log, UI: webui.Handler()}
	if *asService {
		so.ServiceName = serviceName
	}
	svc, err := service.New(so)
	if err != nil {
		return err
	}
	if *asService {
		c, _ := svc.Controller().Config() // provisioned, trusted config
		lv.Set(level(c.Logging.Level))
		dir := c.Logging.Dir
		if dir == "" {
			dir = filepath.Join(*data, "logs")
		}
		if fi, lerr := os.Lstat(dir); lerr == nil && fi.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			log.Error("log folder is a link or junction; not writing a log file there", "dir", dir)
		} else if lf, err := logfile.Open(filepath.Join(dir, "benchwarmer.log"), int64(c.Retention.LogMaxMB)<<20, c.Retention.LogFiles); err != nil {
			log.Error("cannot open the log file", "err", err)
		} else {
			defer lf.Close()
			early.switchTo(lf)
		}
	}
	opts := winsvc.Options{Name: serviceName}
	h := winsvc.HandlerFunc(func(ctx context.Context, evs <-chan winsvc.Event) error { return svc.Run(ctx, evs) })
	if *asService {
		return winsvc.Run(opts, h)
	}
	err = winsvc.RunConsole(opts, h)
	if errors.Is(err, winsvc.ErrForced) {
		return nil
	}
	return err
}

// peekConfig reads the config for logging settings before the service is
// built; failures fall back to defaults (the service reports them).
func peekConfig(dataDir string) (config.Config, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, "config.json"))
	if err != nil {
		return config.Default(), err
	}
	c, err := config.Parse(b)
	if err != nil {
		return config.Default(), err
	}
	return c, nil
}

func level(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func cmdConfig(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: benchwarmer config default|validate FILE")
	}
	switch args[0] {
	case "default":
		b, err := config.Marshal(config.Default())
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(b)
		return err
	case "validate":
		if len(args) < 2 {
			return errors.New("usage: benchwarmer config validate FILE")
		}
		b, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		if _, err := config.Parse(b); err != nil {
			return err
		}
		fmt.Println("valid")
		return nil
	}
	return fmt.Errorf("unknown config command %q", args[0])
}

// earlyLog buffers log output until the log file can be opened safely, then
// writes the buffer to it and passes everything through.
type earlyLog struct {
	mu  sync.Mutex
	buf []byte
	w   io.Writer
}

func (e *earlyLog) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.w != nil {
		return e.w.Write(p)
	}
	if len(e.buf) < 1<<20 {
		e.buf = append(e.buf, p...)
	}
	return len(p), nil
}

func (e *earlyLog) switchTo(w io.Writer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = w.Write(e.buf)
	e.buf, e.w = nil, w
}
