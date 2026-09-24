//go:build windows

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
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
		account := fs.String("account", "system", "service identity: system is the only choice (ADR 0006, 0012); kept for compatibility")
		data := fs.String("data", service.DefaultDataDir(), "data directory")
		_ = fs.Parse(args[1:])
		acct, err := accountName(*account)
		if err != nil {
			return err
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		return winsvc.Install(winsvc.InstallConfig{
			Name: serviceName, DisplayName: "Benchwarmer",
			Description: "Runs a local LLM on the GPU only while it is otherwise idle; yields to games and interactive use.",
			ExePath:     exe, Args: service.ServiceArgs(*data), Account: acct,
		})
	case "remove":
		return winsvc.Remove(serviceName, winsvc.DefaultStopTimeout)
	case "restart":
		fs := flag.NewFlagSet("restart", flag.ExitOnError)
		delay := fs.Duration("delay", 0, "wait this long first (lets the API answer the request that asked for the restart)")
		_ = fs.Parse(args[1:])
		time.Sleep(*delay)
		return winsvc.Restart(serviceName, winsvc.DefaultStopTimeout)
	}
	return fmt.Errorf("unknown service command %q", args[0])
}

// accountName accepts the installer's spellings of LocalSystem, the only
// supported identity. A virtual account is refused: the ACLs the service
// applies to its data folder grant SYSTEM and Administrators only.
func accountName(a string) (string, error) {
	switch strings.ToLower(a) {
	case "system", "localsystem":
		return winsvc.AccountLocalSystem, nil
	}
	return "", fmt.Errorf("unsupported --account %q: the service runs as LocalSystem only (use --account system or omit it)", a)
}
