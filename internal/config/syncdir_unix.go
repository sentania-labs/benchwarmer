//go:build !windows

package config

import "os"

// syncDir makes a rename durable on Unix filesystems.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
