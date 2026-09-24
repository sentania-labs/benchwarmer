//go:build windows

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sentania-labs/benchwarmer/internal/service"
	"github.com/sentania-labs/benchwarmer/internal/winsvc"
)

func cmdService(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: benchwarmer service install|remove")
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
