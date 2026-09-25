//go:build !windows

package provision

import (
	"io/fs"
	"os"
)

// Off Windows there are no ACLs or owners to manage: folders and tokens get
// owner-only POSIX modes instead. Symbolic links are still refused.
var platform fsOps = posixOps{}

type posixOps struct{}

func (posixOps) inspect(path string) (entry, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return entry{}, err
	}
	return entry{Dir: fi.IsDir(), Reparse: fi.Mode()&fs.ModeSymlink != 0, Links: 1, Owner: SIDAdministrators}, nil
}

func (posixOps) secure(Target) (bool, error) { return false, nil }

func (posixOps) privileges() (func(), error) { return func() {}, nil }
