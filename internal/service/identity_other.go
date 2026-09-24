//go:build !windows

package service

// Identity switching is ignored off Windows (development builds).
const requiresSystemForLocalService = false

func isLocalSystem() bool { return false }
