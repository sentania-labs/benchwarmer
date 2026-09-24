//go:build !windows

package procgroup

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i < 0 || !strings.HasPrefix(strings.TrimSpace(s[i+1:]), "Z")
}
