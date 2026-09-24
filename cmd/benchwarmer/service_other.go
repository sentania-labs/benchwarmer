//go:build !windows

package main

import "errors"

func cmdService([]string) error {
	return errors.New("service install and remove are Windows-only; use \"benchwarmer run\" to run in the foreground")
}
