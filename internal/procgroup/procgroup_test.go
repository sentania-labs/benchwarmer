package procgroup

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The test binary re-executes itself in helper roles selected by env var.
func TestMain(m *testing.M) {
	switch os.Getenv("PROCGROUP_HELPER") {
	case "parent":
		// Spawn a grandchild, print its PID, then sleep.
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "PROCGROUP_HELPER=sleeper")
		if err := c.Start(); err != nil {
			fmt.Println("ERR", err)
			os.Exit(2)
		}
		fmt.Println("GRANDCHILD", c.Process.Pid)
		time.Sleep(time.Hour)
		os.Exit(0)
	case "sleeper":
		time.Sleep(time.Hour)
		os.Exit(0)
	case "owner":
		// Own a group containing a parent+grandchild, report PIDs, then block
		// until killed from outside. Used for kill-on-close testing.
		g, err := Start(Spec{Path: os.Args[0], Env: append(os.Environ(), "PROCGROUP_HELPER=parent"), Stdout: os.Stdout})
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(2)
		}
		fmt.Println("ROOT", g.PID())
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func startParent(t *testing.T) (*Group, int) {
	t.Helper()
	pr, pw := io.Pipe()
	g, err := Start(Spec{Path: os.Args[0], Env: append(os.Environ(), "PROCGROUP_HELPER=parent"), Stdout: pw})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(); _ = pw.Close() })
	line, err := readPrefixed(pr, "GRANDCHILD", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	gc, _ := strconv.Atoi(line)
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	return g, gc
}

func readPrefixed(r io.Reader, prefix string, timeout time.Duration) (string, error) {
	ch := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(r)
		for s.Scan() {
			if v, ok := strings.CutPrefix(s.Text(), prefix+" "); ok {
				ch <- v
				return
			}
		}
		close(ch)
	}()
	select {
	case v, ok := <-ch:
		if !ok {
			return "", fmt.Errorf("stream ended before %s", prefix)
		}
		return v, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("timeout waiting for %s", prefix)
	}
}

func TestKillTerminatesWholeTree(t *testing.T) {
	g, gc := startParent(t)
	waitFor(t, func() bool { m, _ := g.Members(); return contains(m, gc) && contains(m, g.PID()) }, "both processes in boundary")

	d, err := g.KillAndWait(10 * time.Second)
	if err != nil {
		t.Fatalf("KillAndWait: %v", err)
	}
	t.Logf("tree terminated in %v", d)
	waitFor(t, func() bool { return !alive(gc) }, "grandchild gone")
}

func TestDoneReportsUnexpectedExit(t *testing.T) {
	g, err := Start(Spec{Path: os.Args[0], Args: []string{"-test.run=^$"}})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	select {
	case <-g.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("process did not exit")
	}
	if g.Exited().IsZero() {
		t.Fatal("exit time not recorded")
	}
}

func TestOwnerDeathKillsTree(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("kill-on-owner-death is a Job Object guarantee; the Unix dev implementation only covers the direct child")
	}
	pr, pw := io.Pipe()
	owner := exec.Command(os.Args[0])
	owner.Env = append(os.Environ(), "PROCGROUP_HELPER=owner")
	owner.Stdout = pw
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	gcs, err := readPrefixed(pr, "GRANDCHILD", 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	gc, _ := strconv.Atoi(gcs)
	if !alive(gc) {
		t.Fatal("grandchild not running before owner kill")
	}
	_ = owner.Process.Kill() // abrupt: no cleanup code in the owner runs
	_ = owner.Wait()
	waitFor(t, func() bool { return !alive(gc) }, "grandchild killed by job close")
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func contains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
