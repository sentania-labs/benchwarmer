package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotationBoundsDiskUse(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "bw.log"), 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 30) + "\n"
	for i := 0; i < 100; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	ents, _ := os.ReadDir(dir)
	var total int64
	for _, e := range ents {
		fi, _ := e.Info()
		total += fi.Size()
	}
	if len(ents) != 3 || total > 300 {
		t.Fatalf("files=%d total=%d", len(ents), total)
	}
}
