package provision

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/secrets"
)

func defaultFiles() secrets.Files { return secrets.FilesFrom(config.Default().Security) }

func TestEnsureCreatesFoldersAndTokens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Benchwarmer")
	toks, r, err := Ensure(dir, Options{Tokens: defaultFiles()})
	if err != nil {
		t.Fatal(err)
	}
	if toks.Management == "" || toks.Inference == "" || toks.Agent == "" {
		t.Fatal("missing token")
	}
	want := append([]string{dir}, subdirPaths(dir)...)
	if !slices.Equal(r.Created, want) {
		t.Fatalf("created %v, want %v", r.Created, want)
	}
	if len(r.Problems) != 0 || len(r.Repaired) != 0 || r.SecretsUnprotected {
		t.Fatalf("report %+v", r)
	}
	for _, f := range []string{"management", "inference", "agent"} {
		if _, err := os.Stat(filepath.Join(dir, "secrets", f+".token")); err != nil {
			t.Fatal(err)
		}
	}

	// Second run: nothing to do, same tokens.
	toks2, r2, err := Ensure(dir, Options{Tokens: defaultFiles()})
	if err != nil {
		t.Fatal(err)
	}
	if toks2 != toks {
		t.Fatal("tokens changed on the second run")
	}
	if len(r2.Created)+len(r2.Repaired)+len(r2.Problems) != 0 {
		t.Fatalf("second run report %+v", r2)
	}
}

// A folder that cannot be created is a problem, not a failure to start.
func TestEnsureFolderProblemIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "logs"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, r, err := Ensure(dir, Options{Tokens: defaultFiles()})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Problems) != 1 || !strings.Contains(r.Problems[0], "logs") {
		t.Fatalf("problems %v", r.Problems)
	}
}

// Tokens are the one thing the service cannot run without.
func TestEnsureTokenFailureIsFatal(t *testing.T) {
	f := secrets.Files{Management: `secrets\same.token`, Inference: `secrets\same.token`, Agent: `secrets\same.token`}
	if _, _, err := Ensure(t.TempDir(), Options{Tokens: f}); err == nil {
		t.Fatal("identical token files accepted")
	}
}

func TestDirTargets(t *testing.T) {
	dir := filepath.Join("C:", "ProgramData", "Benchwarmer")
	ts := DirTargets(dir)
	if ts[0].Path != dir || !ts[0].Protected || !ts[0].Secret {
		t.Fatalf("data folder must come first, protected and secret: %+v", ts[0])
	}
	byName := map[string]Target{}
	for _, x := range ts[1:] {
		if x.Protected {
			t.Errorf("%s: subfolders inherit from the data folder", x.Path)
		}
		byName[filepath.Base(x.Path)] = x
	}
	for _, n := range Subdirs {
		if _, ok := byName[n]; !ok {
			t.Errorf("no ACL for %s", n)
		}
	}
	if !byName["secrets"].Secret || !byName["tls"].Secret || byName["logs"].Secret || byName["models"].Secret {
		t.Error("secret flags")
	}
	if byName["logs"].SDDL != UsersReadSDDL || byName["models"].SDDL != UsersReadSDDL {
		t.Error("logs and models must be readable by users")
	}
	for _, x := range []Target{byName["secrets"], byName["tls"]} {
		if strings.Contains(x.SDDL, ";BU)") {
			t.Errorf("%s readable by users", x.Path)
		}
	}
}

func TestTokenTargets(t *testing.T) {
	dir := filepath.Join("C:", "ProgramData", "Benchwarmer")
	ts := TokenTargets(dir, defaultFiles())
	if len(ts) != 3 {
		t.Fatal(ts)
	}
	for i, x := range ts {
		if !x.Protected || !x.Secret {
			t.Errorf("%s: token files must be protected and secret", x.Path)
		}
		if got := strings.Contains(x.SDDL, ";IU)"); got != (i == 2) {
			t.Errorf("%s: interactive access %v", x.Path, got)
		}
	}
	if ts[2].Path != filepath.Join(dir, "secrets", "agent.token") {
		t.Fatalf("agent token path %s", ts[2].Path)
	}
}

func TestSameACEs(t *testing.T) {
	sy := ace{Type: 0, Flags: 3, Mask: 0x1f01ff, SID: "S-1-5-18"}
	ba := ace{Type: 0, Flags: 3, Mask: 0x1f01ff, SID: "S-1-5-32-544"}
	if !sameACEs([]ace{sy, ba}, []ace{ba, sy}) {
		t.Error("order must not matter")
	}
	if !sameACEs(nil, []ace{}) {
		t.Error("empty sets differ")
	}
	weaker := ba
	weaker.Mask = 0x1200a9
	noInherit := ba
	noInherit.Flags = 0
	deny := ba
	deny.Type = 1
	for _, b := range [][]ace{{sy}, {sy, weaker}, {sy, noInherit}, {sy, deny}, {sy, ba, ba}} {
		if sameACEs([]ace{sy, ba}, b) {
			t.Errorf("%v reported equal", b)
		}
	}
}
