//go:build !windows

package procgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The Unix implementation exists for development and CI on Linux. It uses a
// process group for tree termination. It is not a production target.
type platform struct {
	mu     sync.Mutex
	pgid   int
	closed bool
}

func start(spec Spec) (*Group, error) {
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	cmd.SysProcAttr = sysProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("procgroup: start: %w", err)
	}
	g := &Group{pid: cmd.Process.Pid, started: time.Now(), done: make(chan struct{})}
	g.plat.pgid = cmd.Process.Pid
	go func() { g.setExit(cmd.Wait()) }()
	return g, nil
}

func (g *Group) members() ([]int, error) {
	g.plat.mu.Lock()
	pgid := g.plat.pgid
	g.plat.mu.Unlock()
	// Probe the group: kill(-pgid, 0) fails with ESRCH once it is empty.
	if err := syscall.Kill(-pgid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil, nil
		}
		if !errors.Is(err, syscall.EPERM) {
			return nil, err
		}
	}
	return scanGroup(pgid), nil
}

// scanGroup lists live members of a process group via /proc when available.
// Zombies are excluded since they hold no resources.
func scanGroup(pgid int) []int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return []int{pgid}
	}
	var out []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// Fields after the parenthesised comm: state ppid pgrp ...
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 3 || f[0] == "Z" {
			continue
		}
		if pg, _ := strconv.Atoi(f[2]); pg == pgid {
			out = append(out, pid)
		}
	}
	return out
}

func (g *Group) kill() error {
	g.plat.mu.Lock()
	defer g.plat.mu.Unlock()
	if err := syscall.Kill(-g.plat.pgid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return ErrNotRunning
		}
		return err
	}
	return nil
}

func (g *Group) close() error {
	g.plat.mu.Lock()
	defer g.plat.mu.Unlock()
	if g.plat.closed {
		return nil
	}
	g.plat.closed = true
	// Mirror Windows kill-on-close semantics.
	_ = syscall.Kill(-g.plat.pgid, syscall.SIGKILL)
	return nil
}
