// Package provision brings the data folder, its owner and ACLs, the API
// tokens, and the service's own SCM settings to the installed state at
// every start, so an MSI install, a hand install, and an upgrade converge
// (ADR 0012). It is idempotent: a second run on a correct install changes
// nothing.
//
// It runs in two steps around loading the config: Start secures the data
// folder before anything in it is read, and Finish, given the token file
// references from the (now trusted) config, creates and protects the
// tokens. Only a token that cannot be created is fatal, plus a data folder
// that is itself a junction or link. Every other failure is reported and
// the service keeps running; a failure that could leave secrets exposed
// sets Report.SecretsUnprotected so the service refuses to load a model.
//
// Pre-created folder takeover: ProgramData lets any user create a folder,
// and the creator owns it. A user could create the data folder before the
// install and plant a token with a known value, a config, extra ACEs, or a
// junction. So, as the service, provisioning never follows a junction or
// link; resets the owner of the data folder and everything in it (except
// model files) to Administrators; strips planted ACEs; and, before resetting
// owners, distrusts files a standard user could have planted:
//   - a token not owned by SYSTEM, Administrators, or TrustedInstaller is
//     deleted and regenerated;
//   - a config.json or config.last-good.json not owned by SYSTEM or
//     Administrators is renamed aside and the service starts on defaults.
//
// Legitimate pre-placed files stay trusted: an elevated administrator's
// files are owned by Administrators by default, and a GPO file preference
// writes as SYSTEM.
package provision

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/secrets"
	"github.com/sentania-labs/benchwarmer/internal/winsvc"
)

// Subdirs are the folders created under the data folder.
var Subdirs = []string{"secrets", "models", "logs", "tls"}

// Well-known owner SIDs.
const (
	SIDSystem           = "S-1-5-18"
	SIDAdministrators   = "S-1-5-32-544"
	SIDTrustedInstaller = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
)

// Options selects what provisioning manages beyond folders and tokens.
type Options struct {
	// ApplyACLs secures the data folder: owners, ACLs, junction refusal,
	// and distrust of planted files. Only the service sets it: a console
	// run by a developer would otherwise lock the developer out of a data
	// folder under their own profile.
	ApplyACLs bool
	// ServiceName, when set, is the service whose own SCM settings are
	// ensured (the running service itself).
	ServiceName string
}

// Report says what provisioning changed and what it could not do. It never
// holds token values.
type Report struct {
	Created  []string
	Repaired []string
	Problems []string
	// SecretsUnprotected is set when a secret-bearing path (the data
	// folder, secrets, tls, a token file) could not be secured or is a
	// junction or link. The service must not load a model while it is set.
	SecretsUnprotected bool
}

// Target is one path and the security it must carry: owner Administrators
// and this DACL.
type Target struct {
	Path string
	// SDDL is the DACL, written with well-known SID aliases (SY, BA, BU,
	// IU) so it does not depend on the display language. Only explicit
	// ACEs are listed; inherited ones come from the parent unless
	// Protected.
	SDDL string
	// Protected blocks inheritance from the parent.
	Protected bool
	// Secret marks paths whose failure must block loading.
	Secret bool
	// Accept lists other protected DACLs left alone when found. Unmanaged
	// files accept the token ACLs, so a token keeps the ACL Finish gave it
	// (Start runs before the config names the token files).
	Accept []string
}

// Access masks and ACL strings. They reproduce what install.ps1 applied
// with icacls: (X) is FILE_TRAVERSE alone, (R) is FILE_GENERIC_READ, and
// (RX) is FILE_GENERIC_READ|FILE_GENERIC_EXECUTE. Masks are written in hex
// because SDDL renders 0x20 as "WP", a directory-service alias.
const (
	// Data folder: SYSTEM and Administrators only, not inherited from
	// ProgramData (config and database can carry secrets); INTERACTIVE
	// may traverse it, on this folder only, to reach the agent token.
	DataSDDL = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;;0x20;;;IU)"
	// Logs and models: local users can also read (no secrets or prompts).
	UsersReadSDDL = "D:(A;OICI;0x1200a9;;;BU)"
	// Secrets: inherits the data folder's ACL, plus traverse for the tray.
	SecretsSDDL = "D:(A;;0x20;;;IU)"
	// Everything else: the parent's ACL only.
	InheritOnlySDDL = "D:"
	// Management and inference tokens: SYSTEM and Administrators only.
	TokenSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	// Agent token: also readable by the interactive user (the tray).
	AgentTokenSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x120089;;;IU)"
)

// DirTargets are the managed folder ACLs, parent first.
func DirTargets(dataDir string) []Target {
	out := []Target{{Path: dataDir, SDDL: DataSDDL, Protected: true, Secret: true}}
	for _, s := range Subdirs {
		t, _ := policyFor(s, true)
		t.Path = filepath.Join(dataDir, s)
		out = append(out, t)
	}
	return out
}

// TokenTargets are the token file ACLs.
func TokenTargets(dataDir string, f secrets.Files) []Target {
	return []Target{
		{Path: secrets.Resolve(dataDir, f.Management), SDDL: TokenSDDL, Protected: true, Secret: true},
		{Path: secrets.Resolve(dataDir, f.Inference), SDDL: TokenSDDL, Protected: true, Secret: true},
		{Path: secrets.Resolve(dataDir, f.Agent), SDDL: AgentTokenSDDL, Protected: true, Secret: true},
	}
}

// policyFor is how Start treats an entry, by its path relative to the data
// folder: the security it must carry and whether to walk into it. Model
// files are left alone (large, and not ours to re-own).
func policyFor(rel string, dir bool) (Target, bool) {
	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	t := Target{SDDL: InheritOnlySDDL, Secret: first != "logs" && first != "models"}
	if !dir {
		t.Accept = []string{TokenSDDL, AgentTokenSDDL}
		return t, false
	}
	switch filepath.ToSlash(rel) {
	case "secrets":
		t.SDDL = SecretsSDDL
	case "logs":
		t.SDDL = UsersReadSDDL
	case "models":
		return Target{SDDL: UsersReadSDDL}, false
	}
	return t, true
}

func isConfigFile(rel string) bool { return rel == "config.json" || rel == "config.last-good.json" }

func trustedConfigOwner(sid string) bool { return sid == SIDSystem || sid == SIDAdministrators }

func trustedTokenOwner(sid string) bool {
	return trustedConfigOwner(sid) || sid == SIDTrustedInstaller
}

// entry is what the platform reports about a path, without following a
// junction or link at it.
type entry struct {
	Dir     bool
	Reparse bool   // junction, symbolic link, or mount point
	Links   uint32 // hard links to the file
	Owner   string // owner SID
}

// fsOps is the platform part: inspecting and securing one path, never
// through a junction or link.
type fsOps interface {
	inspect(path string) (entry, error)
	// secure sets owner Administrators and t's DACL where they differ and
	// reports whether it wrote.
	secure(t Target) (bool, error)
	// privileges enables what securing needs (taking ownership, bypassing
	// a planted DACL) and returns the undo.
	privileges() (func(), error)
}

// Run is one provisioning pass: Start, then Finish.
type Run struct {
	dataDir string
	o       Options
	ops     fsOps
	now     func() time.Time
	r       Report
	// untrusted maps paths to the owner SID they had before Start reset
	// it, for files a standard user could have planted.
	untrusted map[string]string
	// reparse lists the junctions and links found; nothing under them is
	// written.
	reparse []string
}

// Start secures the data folder, before the config or anything else in it
// is read. The error is non-nil only when the data folder cannot be used
// at all.
func Start(dataDir string, o Options) (*Run, error) {
	p := &Run{dataDir: dataDir, o: o, ops: platform, now: time.Now, untrusted: map[string]string{}}
	return p, p.start()
}

// Report returns what the run changed and what it could not do.
func (p *Run) Report() Report { return p.r }

func (p *Run) problem(secret bool, format string, a ...any) {
	p.r.Problems = append(p.r.Problems, fmt.Sprintf(format, a...))
	if secret {
		p.r.SecretsUnprotected = true
	}
}

func (p *Run) start() error {
	if !p.o.ApplyACLs {
		p.mkdirs(true)
		return nil
	}
	undo, err := p.ops.privileges()
	if err != nil {
		// Not fatal: files carrying a planted DACL then fail to secure,
		// which is reported (and blocks loading) below.
		p.problem(false, "enable privileges: %v", err)
	}
	defer undo()

	if _, err := os.Lstat(p.dataDir); errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(p.dataDir, 0o700); err != nil {
			return fmt.Errorf("create data folder: %w", err)
		}
		p.r.Created = append(p.r.Created, p.dataDir)
	}
	e, err := p.ops.inspect(p.dataDir)
	if err != nil {
		return fmt.Errorf("data folder: %w", err)
	}
	if e.Reparse {
		return fmt.Errorf("data folder %s is a junction or link; refusing to use it (remove it and restart the service)", p.dataDir)
	}
	if !e.Dir {
		return fmt.Errorf("data folder %s is not a folder", p.dataDir)
	}
	p.record(p.dataDir, e)
	p.secure(Target{Path: p.dataDir, SDDL: DataSDDL, Protected: true, Secret: true})
	p.mkdirs(false)
	p.walk(p.dataDir)
	return nil
}

// mkdirs creates missing managed folders. An existing entry, including a
// junction, is left for the walk to judge.
func (p *Run) mkdirs(withData bool) {
	dirs := subdirPaths(p.dataDir)
	if withData {
		dirs = append([]string{p.dataDir}, dirs...)
	}
	for _, d := range dirs {
		if _, err := os.Lstat(d); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			p.problem(false, "create %s: %v", d, err)
			continue
		}
		// 0o700: meaningful off Windows; on Windows the ACLs apply.
		if err := os.MkdirAll(d, 0o700); err != nil {
			p.problem(false, "create %s: %v", d, err)
			continue
		}
		p.r.Created = append(p.r.Created, d)
	}
}

// walk secures dir's entries top-down: each folder is secured before its
// entries are listed, so a standard user can no longer add to it.
func (p *Run) walk(dir string) {
	list, err := os.ReadDir(dir)
	if err != nil {
		p.problem(p.secretRel(dir), "list %s: %v", dir, err)
		return
	}
	for _, de := range list {
		path := filepath.Join(dir, de.Name())
		rel, _ := filepath.Rel(p.dataDir, path)
		secret := p.secretRel(path)
		e, err := p.ops.inspect(path)
		if err != nil {
			p.problem(secret, "inspect %s: %v", path, err)
			continue
		}
		if e.Reparse {
			// Any junction or link blocks loading, models included: the
			// runtime would read whatever it points to.
			p.reparse = append(p.reparse, path)
			p.problem(true, "%s is a junction or link; not following it (remove it and restart the service)", path)
			continue
		}
		p.record(path, e)
		if !e.Dir && isConfigFile(rel) && (!trustedConfigOwner(e.Owner) || e.Links > 1) {
			p.setAsideConfig(path, e)
			continue
		}
		if !e.Dir && e.Links > 1 {
			// Securing a hard link would change the security of the file
			// it shares, which may be outside the data folder.
			p.problem(secret, "%s has other hard links; not changing its security (remove it and restart the service)", path)
			continue
		}
		t, descend := policyFor(rel, e.Dir)
		t.Path = path
		p.secure(t)
		if descend {
			p.walk(path)
		}
	}
}

func (p *Run) secretRel(path string) bool {
	rel, err := filepath.Rel(p.dataDir, path)
	if err != nil || rel == "." {
		return true
	}
	t, _ := policyFor(rel, false)
	return t.Secret
}

func (p *Run) record(path string, e entry) {
	if !trustedTokenOwner(e.Owner) {
		p.untrusted[path] = e.Owner
	}
}

func (p *Run) secure(t Target) {
	wrote, err := p.ops.secure(t)
	switch {
	case err != nil:
		p.problem(t.Secret, "secure %s: %v", t.Path, err)
	case wrote:
		p.r.Repaired = append(p.r.Repaired, "owner and ACL of "+t.Path)
	}
}

// setAsideConfig renames a config a standard user could have written, so
// the service starts on defaults instead of trusting it. Not blocking: the
// config holds no secret the service would expose by loading a model.
func (p *Run) setAsideConfig(path string, e entry) {
	why := "owned by " + e.Owner + ", not SYSTEM or Administrators"
	if e.Links > 1 {
		why = "hard-linked to another file"
	}
	aside := path + ".untrusted-" + p.now().Format("20060102-150405")
	if err := os.Rename(path, aside); err != nil {
		if rerr := os.Remove(path); rerr != nil {
			p.problem(true, "%s is %s and could not be set aside: %v", path, why, err)
			return
		}
		p.problem(false, "%s was %s: deleted (rename failed: %v); running on defaults", path, why, err)
		return
	}
	p.problem(false, "%s was %s: renamed to %s; running on defaults", path, why, filepath.Base(aside))
	if e.Links <= 1 {
		p.secure(Target{Path: aside, SDDL: InheritOnlySDDL, Secret: true})
	}
}

// Finish loads or creates the tokens named by the config, distrusting
// planted ones, protects them, and ensures the service's SCM settings. It
// returns an error only when the tokens cannot be created.
func (p *Run) Finish(f secrets.Files) (secrets.Tokens, error) {
	unsafe := false
	if p.o.ApplyACLs {
		undo, _ := p.ops.privileges()
		defer undo()
		for _, path := range tokenPaths(p.dataDir, f) {
			if !p.tokenSafe(path) {
				unsafe = true
			}
		}
	}
	var toks secrets.Tokens
	if unsafe {
		// Writing a token here could write through a junction or outside
		// the data folder. Run with tokens held in memory only, so the
		// dashboard still explains the problem; loading stays blocked.
		var err error
		if toks, err = ephemeralTokens(); err != nil {
			return secrets.Tokens{}, err
		}
		p.problem(true, "tokens not written to disk; using temporary tokens until the service restarts with the problem fixed")
	} else {
		var err error
		if toks, err = secrets.EnsureTokens(p.dataDir, f); err != nil {
			return secrets.Tokens{}, fmt.Errorf("tokens: %w", err)
		}
		if p.o.ApplyACLs {
			for _, t := range TokenTargets(p.dataDir, f) {
				p.secure(t)
			}
		}
	}
	if p.o.ServiceName != "" {
		changed, err := winsvc.EnsureSettings(p.o.ServiceName, 0)
		for _, c := range changed {
			p.r.Repaired = append(p.r.Repaired, "service "+c)
		}
		if err != nil {
			p.problem(false, "service settings: %v", err)
		}
	}
	return toks, nil
}

// tokenSafe reports whether the token at path can be read and written
// safely, deleting a planted one so EnsureTokens creates a fresh token.
func (p *Run) tokenSafe(path string) bool {
	if !insideDir(p.dataDir, path) {
		p.problem(true, "token file %s is outside the data folder", path)
		return false
	}
	for _, rp := range p.reparse {
		if path == rp || insideDir(rp, path) {
			return false // already reported
		}
	}
	e, err := p.ops.inspect(path)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		p.problem(true, "inspect %s: %v", path, err)
		return false
	}
	why := ""
	switch owner, planted := p.untrusted[path]; {
	case e.Reparse:
		why = "a junction or link"
	case e.Dir:
		p.problem(true, "token file %s is a folder", path)
		return false
	case e.Links > 1:
		why = "hard-linked to another file"
	case planted:
		why = "owned by " + owner
	case !trustedTokenOwner(e.Owner):
		why = "owned by " + e.Owner
	}
	if why == "" {
		return true
	}
	if err := os.Remove(path); err != nil {
		p.problem(true, "token file %s is %s and could not be deleted: %v", path, why, err)
		return false
	}
	p.r.Repaired = append(p.r.Repaired, fmt.Sprintf("regenerated untrusted token %s (was %s)", path, why))
	return true
}

func tokenPaths(dataDir string, f secrets.Files) []string {
	var out []string
	for _, t := range TokenTargets(dataDir, f) {
		out = append(out, t.Path)
	}
	return out
}

func ephemeralTokens() (secrets.Tokens, error) {
	var t secrets.Tokens
	for _, p := range []*string{&t.Management, &t.Inference, &t.Agent} {
		v, err := secrets.Generate()
		if err != nil {
			return secrets.Tokens{}, err
		}
		*p = v
	}
	return t, nil
}

// insideDir reports whether path is strictly inside dir, lexically.
func insideDir(dir, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func subdirPaths(dataDir string) []string {
	out := make([]string, len(Subdirs))
	for i, s := range Subdirs {
		out[i] = filepath.Join(dataDir, s)
	}
	return out
}

// ace is one access control entry in comparable form.
type ace struct {
	Type  uint8
	Flags uint8
	Mask  uint32
	SID   string
}

func (a ace) key() string { return fmt.Sprintf("%d/%#x/%#x/%s", a.Type, a.Flags, a.Mask, a.SID) }

// sameACEs compares explicit ACE sets. Order is ignored: tools such as
// Explorer reorder ACEs into canonical order, which is not drift.
func sameACEs(a, b []ace) bool {
	if len(a) != len(b) {
		return false
	}
	ka, kb := make([]string, len(a)), make([]string, len(b))
	for i := range a {
		ka[i], kb[i] = a[i].key(), b[i].key()
	}
	sort.Strings(ka)
	sort.Strings(kb)
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}
