//go:build !windows

package main

import "errors"

var errWindowsOnly = errors.New("svctest is Windows-only")

func cmdSvcTest([]string) error { return errWindowsOnly }
func cmdSvcRun([]string) error  { return errWindowsOnly }
