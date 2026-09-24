//go:build !windows

package main

import "os"

func identity() any {
	return map[string]int{"uid": os.Getuid(), "euid": os.Geteuid()}
}
