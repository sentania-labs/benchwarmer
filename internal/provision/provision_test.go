package provision

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/config"
	"github.com/sentania-labs/benchwarmer/internal/secrets"
)

func defaultFiles() secrets.Files { return secrets.FilesFrom(config.Default().Security) }

func provision(t *testing.T, dir string, o Options) (secrets.Tokens, Report) {
	t.Helper()
	p, err := Start(dir, o)
	if err != nil {
		t.Fatal(err)
	}
	toks, err := p.Finish(defaultFiles())
	if err != nil {
		t.Fatal(err)
	}
	return toks, p.Report()
}

func TestProvisionCreatesFoldersAndTokens(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Benchwarmer")
	toks, r := provision(t, dir, Options{})
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
	toks2, r2 := provision(t, dir, Options{})
	if toks2 != toks {
		t.Fatal("tokens changed on the second run")
	}
	if len(r2.Created)+len(r2.Repaired)+len(r2.Problems) != 0 {
		t.Fatalf("second run report %+v", r2)
	}
}

// A folder that cannot be created is a problem, not a failure to start.
func TestFolderProblemIsNotFatal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a read-only folder attribute does not stop creating entries on Windows")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "logs", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if os.Getuid() == 0 {
		t.Skip("root ignores the read-only folder")
	}
	p, err := Start(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r := p.Report(); len(r.Problems) == 0 || !strings.Contains(strings.Join(r.Problems, " "), "secrets") {
		t.Fatalf("problems %v", r.Problems)
	}
}

// Tokens are the one thing the service cannot run without.
func TestTokenFailureIsFatal(t *testing.T) {
	p, err := Start(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	f := secrets.Files{Management: `secrets\same.token`, Inference: `secrets\same.token`, Agent: `secrets\same.token`}
	if _, err := p.Finish(f); err == nil {
		t.Fatal("identical token files accepted")
	}
}

// fakeOps reports owners and hard links from a table, on top of the real
// filesystem, and records what was secured.
type fakeOps struct {
	owner   map[string]string
	links   map[string]uint32
	secured []string
}

func (f *fakeOps) inspect(path string) (entry, error) {
	e, err := posix(path)
	if err != nil {
		return e, err
	}
	e.Owner = SIDAdministrators
	if o, ok := f.owner[path]; ok {
		e.Owner = o
	}
	if n, ok := f.links[path]; ok {
		e.Links = n
	}
	return e, nil
}

func (f *fakeOps) secure(t Target) (bool, error) {
	f.secured = append(f.secured, t.Path)
	f.owner[t.Path] = SIDAdministrators
	return false, nil
}

func (f *fakeOps) privileges() (func(), error) { return func() {}, nil }

// posix inspects without following a symbolic link, on any platform.
func posix(path string) (entry, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return entry{}, err
	}
	return entry{Dir: fi.IsDir(), Reparse: fi.Mode()&fs.ModeSymlink != 0, Links: 1}, nil
}

func fakeRun(t *testing.T, dir string, ops *fakeOps) (*Run, secrets.Tokens) {
	t.Helper()
	p := &Run{dataDir: dir, o: Options{ApplyACLs: true}, ops: ops, untrusted: map[string]string{},
		now: func() time.Time { return time.Date(2026, 9, 24, 15, 30, 0, 0, time.Local) }}
	if err := p.start(); err != nil {
		t.Fatal(err)
	}
	toks, err := p.Finish(defaultFiles())
	if err != nil {
		t.Fatal(err)
	}
	return p, toks
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

const sidUsers = "S-1-5-32-545"

// A standard user pre-created the data folder and planted a token and a
// config: the token is regenerated, the config set aside, and loading is
// not blocked (both are fixed).
func TestPlantedFilesAreDistrusted(t *testing.T) {
	dir := t.TempDir()
	planted := strings.Repeat("k", 43)
	tok := filepath.Join(dir, "secrets", "management.token")
	cfg := filepath.Join(dir, "config.json")
	lastGood := filepath.Join(dir, "config.last-good.json")
	write(t, tok, planted)
	write(t, cfg, "{}")
	write(t, lastGood, "{}")
	write(t, filepath.Join(dir, "models", "m.gguf"), "x")
	ops := &fakeOps{owner: map[string]string{dir: sidUsers, tok: sidUsers, cfg: sidUsers}, links: map[string]uint32{lastGood: 2}}

	p, toks := fakeRun(t, dir, ops)
	r := p.Report()
	if toks.Management == planted {
		t.Fatal("planted management token kept")
	}
	if !slices.ContainsFunc(r.Repaired, func(s string) bool { return strings.HasPrefix(s, "regenerated untrusted token "+tok) }) {
		t.Fatalf("repaired %v", r.Repaired)
	}
	for _, f := range []string{cfg, lastGood} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s not set aside", f)
		}
		if _, err := os.Stat(f + ".untrusted-20260924-153000"); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.Problems) != 2 || r.SecretsUnprotected {
		t.Fatalf("problems %v, secrets unprotected %v", r.Problems, r.SecretsUnprotected)
	}
	if slices.Contains(ops.secured, filepath.Join(dir, "models", "m.gguf")) {
		t.Fatal("model file re-owned")
	}
	if !slices.Contains(ops.secured, filepath.Join(dir, "models")) {
		t.Fatal("models folder not secured")
	}
	// The data folder is secured first, before anything in it.
	if ops.secured[0] != dir {
		t.Fatalf("first secured %s", ops.secured[0])
	}
}

// A junction or link for secrets is refused: nothing is written through
// it, tokens stay in memory, and loading is blocked.
func TestLinkedSecretsAreRefused(t *testing.T) {
	dir, elsewhere := t.TempDir(), t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(dir, "secrets")); err != nil {
		t.Skip("cannot create a symbolic link:", err)
	}
	ops := &fakeOps{owner: map[string]string{}, links: map[string]uint32{}}
	p, toks := fakeRun(t, dir, ops)
	r := p.Report()
	if !r.SecretsUnprotected || toks.Management == "" {
		t.Fatalf("report %+v", r)
	}
	if list, _ := os.ReadDir(elsewhere); len(list) != 0 {
		t.Fatalf("wrote through the link: %v", list)
	}
	for _, s := range ops.secured {
		if strings.HasPrefix(s, filepath.Join(dir, "secrets")) {
			t.Fatalf("secured through the link: %s", s)
		}
	}
}

// A data folder that is itself a link cannot be used at all.
func TestLinkedDataFolderIsFatal(t *testing.T) {
	root, elsewhere := t.TempDir(), t.TempDir()
	dir := filepath.Join(root, "Benchwarmer")
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Skip("cannot create a symbolic link:", err)
	}
	p := &Run{dataDir: dir, o: Options{ApplyACLs: true}, ops: &fakeOps{owner: map[string]string{}}, untrusted: map[string]string{}, now: time.Now}
	if err := p.start(); err == nil {
		t.Fatal("linked data folder accepted")
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
	if !byName["secrets"].Secret || !byName["tls"].Secret || byName["logs"].Secret || byName["models"].Secret {
		t.Error("secret flags")
	}
	if byName["logs"].SDDL != UsersReadSDDL || byName["models"].SDDL != UsersReadSDDL {
		t.Error("logs and models must be readable by users")
	}
	if byName["secrets"].SDDL != SecretsSDDL || byName["tls"].SDDL != InheritOnlySDDL {
		t.Error("secrets and tls ACLs")
	}
	if tf, descend := policyFor(filepath.Join("secrets", "x"), false); tf.SDDL != InheritOnlySDDL || !tf.Secret || descend || len(tf.Accept) != 2 {
		t.Errorf("unmanaged file policy %+v", tf)
	}
	if _, descend := policyFor("models", true); descend {
		t.Error("models contents must be left alone")
	}
}

func TestTokenTargets(t *testing.T) {
	dir := filepath.Join("C:", "ProgramData", "Benchwarmer")
	ts := TokenTargets(dir, defaultFiles())
	for i, x := range ts {
		if !x.Protected || !x.Secret {
			t.Errorf("%s: token files must be protected and secret", x.Path)
		}
		if got := strings.Contains(x.SDDL, ";IU)"); got != (i == 2) {
			t.Errorf("%s: interactive access %v", x.Path, got)
		}
	}
}

func TestOwnerTrust(t *testing.T) {
	for _, s := range []string{SIDSystem, SIDAdministrators} {
		if !trustedConfigOwner(s) || !trustedTokenOwner(s) {
			t.Errorf("%s untrusted", s)
		}
	}
	if trustedConfigOwner(SIDTrustedInstaller) || !trustedTokenOwner(SIDTrustedInstaller) {
		t.Error("TrustedInstaller")
	}
	for _, s := range []string{sidUsers, "S-1-5-21-1-2-3-1001", ""} {
		if trustedConfigOwner(s) || trustedTokenOwner(s) {
			t.Errorf("%q trusted", s)
		}
	}
}

func TestInsideDir(t *testing.T) {
	d := filepath.Join(string(filepath.Separator)+"pd", "Benchwarmer")
	for p, want := range map[string]bool{
		filepath.Join(d, "secrets", "a.token"): true,
		filepath.Join(d, "a.token"):            true,
		d:                                      false,
		filepath.Join(d, "..", "x"):            false,
		filepath.Join(d+"2", "x"):              false,
	} {
		if got := insideDir(d, p); got != want {
			t.Errorf("%s: %v", p, got)
		}
	}
}

func TestSameACEs(t *testing.T) {
	sy := ace{Type: 0, Flags: 3, Mask: 0x1f01ff, SID: SIDSystem}
	ba := ace{Type: 0, Flags: 3, Mask: 0x1f01ff, SID: SIDAdministrators}
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
