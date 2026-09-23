package main

import (
	"log"
	"os"
	"os/exec"
)

// spawnSleeper starts a long-lived child so tests can verify that tree
// termination reaches descendants.
func spawnSleeper() {
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	c := exec.Command(exe, "-port", "0", "-load-delay", "876000h")
	if err := c.Start(); err != nil {
		log.Fatalf("spawn child: %v", err)
	}
	log.Printf("fakellama: spawned child pid %d", c.Process.Pid)
}
