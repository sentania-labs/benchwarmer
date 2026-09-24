//go:build windows

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/service"
	"github.com/sentania-labs/benchwarmer/internal/winsvc"
)

func cmdService(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: benchwarmer service install|remove|restart")
	}
	switch args[0] {
	case "install":
		fs := flag.NewFlagSet("install", flag.ExitOnError)
		account := fs.String("account", "system", "service identity: system (default, ADR 0006) or virtual")
		data := fs.String("data", service.DefaultDataDir(), "data directory")
		_ = fs.Parse(args[1:])
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		return winsvc.Install(winsvc.InstallConfig{
			Name: serviceName, DisplayName: "Benchwarmer",
			Description: "Runs a local LLM on the GPU only while it is otherwise idle; yields to games and interactive use.",
			ExePath:     exe, Args: service.ServiceArgs(*data), Account: accountName(*account),
		})
	case "remove":
		return winsvc.Remove(serviceName, winsvc.DefaultStopTimeout)
	case "restart":
		fs := flag.NewFlagSet("restart", flag.ExitOnError)
		delay := fs.Duration("delay", 0, "wait this long first (lets the API answer the request that asked for the restart)")
		data := fs.String("data", service.DefaultDataDir(), "data directory (a failure is written to logs\\restart.log)")
		_ = fs.Parse(args[1:])
		time.Sleep(*delay)
		// The stop can take the service's whole shutdown budget (drain plus
		// a graceful runtime stop), so wait well past it. Started detached
		// from the dashboard, nobody sees stderr: record failures.
		err := winsvc.Restart(serviceName, 90*time.Second)
		if err != nil {
			if f, ferr := os.OpenFile(filepath.Join(*data, "logs", "restart.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); ferr == nil {
				fmt.Fprintf(f, "%s restart failed: %v\n", time.Now().Format(time.RFC3339), err)
				f.Close()
			}
		}
		return err
	}
	return fmt.Errorf("unknown service command %q", args[0])
}

// accountName maps the installer's spelling onto winsvc's account names.
func accountName(a string) string {
	switch strings.ToLower(a) {
	case "system", "localsystem":
		return winsvc.AccountLocalSystem
	case "virtual":
		return winsvc.AccountVirtual
	}
	return a
}
