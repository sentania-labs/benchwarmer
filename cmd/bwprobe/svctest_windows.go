//go:build windows

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const probeService = "BenchwarmerProbe"

// cmdSvcTest answers E2 and E7: it registers a temporary Windows service
// under the chosen identity, runs the env report and runtime cycles from
// session 0, collects the results, and removes the service.
func cmdSvcTest(args []string) error {
	fs := flag.NewFlagSet("svctest", flag.ExitOnError)
	exe := fs.String("exe", "", "path to llama-server executable (required)")
	model := fs.String("model", "", "path to GGUF model (required)")
	extra := fs.String("args", "-ngl 999 -c 8192", "extra llama-server arguments")
	account := fs.String("account", "virtual", "service identity: virtual, system, or localservice")
	outDir := fs.String("out", `C:\ProgramData\BenchwarmerProbe`, "result directory")
	cycles := fs.Int("cycles", 2, "runtime cycles to run inside the service")
	perfmon := fs.Bool("perfmon-group", false, "add the virtual account to Performance Monitor Users for this run")
	timeout := fs.Duration("timeout", 15*time.Minute, "max time to wait for the service run")
	_ = fs.Parse(args)
	if *exe == "" || *model == "" {
		return errors.New("-exe and -model are required")
	}

	var startName string
	switch *account {
	case "virtual":
		startName = `NT SERVICE\` + probeService
	case "system":
		startName = "LocalSystem"
	case "localservice":
		startName = `NT AUTHORITY\LocalService`
	default:
		return fmt.Errorf("unknown account %q", *account)
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	// Run the probe from where it is installed. Copying it elsewhere (for
	// example into the output folder under ProgramData) puts it outside the
	// Defender ASR path exclusion, and the service fails to start.
	probeExe, err := os.Executable()
	if err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as administrator): %w", err)
	}
	defer m.Disconnect()
	if old, err := m.OpenService(probeService); err == nil {
		_, _ = old.Control(svc.Stop)
		_ = old.Delete()
		old.Close()
		time.Sleep(2 * time.Second)
	}

	label := "svc-" + *account
	if *perfmon {
		label += "-perfmon"
	}
	resultFile := filepath.Join(*outDir, fmt.Sprintf("%s-%s.jsonl", label, time.Now().Format("20060102-150405")))
	s, err := m.CreateService(probeService, probeExe, mgr.Config{
		DisplayName:      "Benchwarmer Phase 0 probe (temporary)",
		StartType:        mgr.StartManual,
		ServiceStartName: startName,
	}, "svcrun", "-exe", *exe, "-model", *model, "-args", *extra, "-out", resultFile, "-cycles", fmt.Sprint(*cycles))
	if err != nil {
		return fmt.Errorf("create service: %w", err)
	}
	defer func() {
		_ = s.Delete()
		s.Close()
	}()

	var grants []string
	if *account == "virtual" {
		// A virtual account has no rights to these paths by default. Grants
		// are removed again at the end of the run.
		for _, g := range []struct{ path, perm string }{
			{*outDir, "(OI)(CI)M"},
			{filepath.Dir(*exe), "(OI)(CI)RX"},
			{filepath.Dir(probeExe), "(OI)(CI)RX"},
			{filepath.Dir(*model), "(OI)(CI)RX"},
		} {
			if out, err := exec.Command("icacls", g.path, "/grant", startName+":"+g.perm).CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: icacls grant on %s failed: %v %s\n", g.path, err, out)
			} else {
				grants = append(grants, g.path)
			}
		}
		defer func() {
			for _, p := range grants {
				_ = exec.Command("icacls", p, "/remove:g", startName).Run()
			}
		}()
		if *perfmon {
			ps := fmt.Sprintf(`Add-LocalGroupMember -SID S-1-5-32-558 -Member '%s'`, startName)
			if out, err := exec.Command("powershell", "-NoProfile", "-Command", ps).CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "warning: add to Performance Monitor Users failed: %v %s\n", err, out)
			}
			defer func() {
				ps := fmt.Sprintf(`Remove-LocalGroupMember -SID S-1-5-32-558 -Member '%s'`, startName)
				_ = exec.Command("powershell", "-NoProfile", "-Command", ps).Run()
			}()
		}
	}

	fmt.Fprintf(os.Stderr, "starting %s as %s; results -> %s\n", probeService, startName, resultFile)
	if err := s.Start(); err != nil {
		return fmt.Errorf("start service: %w", err)
	}
	deadline := time.Now().Add(*timeout)
	for {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == svc.Stopped {
			fmt.Fprintf(os.Stderr, "service finished (exit code %d, service-specific %d)\n", st.Win32ExitCode, st.ServiceSpecificExitCode)
			break
		}
		if time.Now().After(deadline) {
			_, _ = s.Control(svc.Stop)
			return errors.New("timed out waiting for service run")
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Println(resultFile)
	return nil
}

// cmdSvcRun is the service body used by svctest.
func cmdSvcRun(args []string) error {
	fs := flag.NewFlagSet("svcrun", flag.ExitOnError)
	var o runtimeOpts
	fs.StringVar(&o.exe, "exe", "", "")
	fs.StringVar(&o.model, "model", "", "")
	fs.StringVar(&o.extra, "args", "", "")
	out := fs.String("out", "", "")
	fs.IntVar(&o.cycles, "cycles", 2, "")
	_ = fs.Parse(args)
	o.host, o.port, o.maxTokens = "127.0.0.1", 18081, 64
	o.prompt = "Write a short paragraph about lighthouses."
	o.loadTimeout, o.settle, o.releaseTolerance = 5*time.Minute, 5*time.Second, 256
	o.logDir = filepath.Dir(*out)
	// No console in a service, so no graceful (Ctrl+C) mode.
	o.modes = []string{modeIdle, modeBusy}

	isSvc, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	body := func() error {
		w, err := openJSONL(*out, false)
		if err != nil {
			return err
		}
		defer w.close()
		w.write(map[string]any{"kind": "env", "report": collectEnv("")})
		return runCycles(o, w)
	}
	if !isSvc {
		return body()
	}
	return svc.Run(probeService, &svcHandler{body: body})
}

type svcHandler struct{ body func() error }

func (h *svcHandler) Execute(_ []string, req <-chan svc.ChangeRequest, st chan<- svc.Status) (bool, uint32) {
	st <- svc.Status{State: svc.StartPending}
	done := make(chan error, 1)
	go func() { done <- h.body() }()
	st <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop}
	for {
		select {
		case err := <-done:
			st <- svc.Status{State: svc.StopPending}
			if err != nil {
				return true, 1
			}
			return false, 0
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				st <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				// Exiting closes the job handles, which kills any runtime.
				st <- svc.Status{State: svc.StopPending}
				return false, 0
			}
		}
	}
}
