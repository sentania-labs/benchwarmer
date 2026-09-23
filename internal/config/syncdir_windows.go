//go:build windows

package config

// syncDir is a no-op on Windows: NTFS journals the rename, and directory
// handles cannot be fsynced through os.File.
func syncDir(string) {}
