//go:build !windows

package provision

// applyACL is a no-op off Windows: the folders and tokens are created with
// owner-only POSIX modes instead, and there is no target ACL to maintain.
func applyACL(Target) (bool, error) { return false, nil }
