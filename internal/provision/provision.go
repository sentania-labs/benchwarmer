// Package provision brings the data folder, its ACLs, the API tokens, and
// the service's own SCM settings to the installed state at every start, so
// an MSI install, a hand install, and an upgrade converge (ADR 0012). It is
// idempotent: a second run on a correct install changes nothing.
//
// Only token creation is fatal. Every other failure is reported and the
// service keeps running; a failure to protect secret-bearing paths sets
// Report.SecretsUnprotected so the service refuses to load a model.
package provision

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/sentania-labs/benchwarmer/internal/secrets"
	"github.com/sentania-labs/benchwarmer/internal/winsvc"
)

// Subdirs are the folders created under the data folder.
var Subdirs = []string{"secrets", "models", "logs", "tls"}

// Options selects what Ensure manages beyond the folders and tokens.
type Options struct {
	// Tokens are the token file references from the config.
	Tokens secrets.Files
	// ApplyACLs sets and repairs the data folder ACLs. Only the service
	// sets it: a console run by a developer would otherwise lock the
	// developer out of a data folder under their own profile.
	ApplyACLs bool
	// ServiceName, when set, is the service whose own SCM settings are
	// ensured (the running service itself).
	ServiceName string
}

// Report says what Ensure changed and what it could not do. It never holds
// token values.
type Report struct {
	Created  []string
	Repaired []string
	Problems []string
	// SecretsUnprotected is set when an ACL on a secret-bearing path (the
	// data folder, secrets, tls, or a token file) could not be applied or
	// verified. The service must not load a model while it is set.
	SecretsUnprotected bool
}

// Target is one path and the ACL it must carry.
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
	// TLS (self-signed key): the data folder's ACL only.
	InheritOnlySDDL = "D:"
	// Management and inference tokens: SYSTEM and Administrators only,
	// set explicitly so the token is safe wherever the config puts it.
	TokenSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	// Agent token: also readable by the interactive user (the tray).
	AgentTokenSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x120089;;;IU)"
)

// DirTargets are the folder ACLs, parent first: a folder's inherited ACEs
// are only right once its parent is.
func DirTargets(dataDir string) []Target {
	j := func(s string) string { return filepath.Join(dataDir, s) }
	return []Target{
		{Path: dataDir, SDDL: DataSDDL, Protected: true, Secret: true},
		{Path: j("secrets"), SDDL: SecretsSDDL, Secret: true},
		{Path: j("tls"), SDDL: InheritOnlySDDL, Secret: true},
		{Path: j("logs"), SDDL: UsersReadSDDL},
		{Path: j("models"), SDDL: UsersReadSDDL},
	}
}

// TokenTargets are the token file ACLs.
func TokenTargets(dataDir string, f secrets.Files) []Target {
	return []Target{
		{Path: secrets.Resolve(dataDir, f.Management), SDDL: TokenSDDL, Protected: true, Secret: true},
		{Path: secrets.Resolve(dataDir, f.Inference), SDDL: TokenSDDL, Protected: true, Secret: true},
		{Path: secrets.Resolve(dataDir, f.Agent), SDDL: AgentTokenSDDL, Protected: true, Secret: true},
	}
}

// Ensure creates the folders, applies their ACLs, loads or creates the
// tokens, applies the token ACLs, and ensures the service's SCM settings,
// in that order: a token file created by EnsureTokens gets its ACL in the
// same run. The error is non-nil only when the tokens cannot be loaded or
// created, which the service cannot run without.
func Ensure(dataDir string, o Options) (secrets.Tokens, Report, error) {
	var r Report
	for _, d := range append([]string{dataDir}, subdirPaths(dataDir)...) {
		created, err := mkdir(d)
		if err != nil {
			r.Problems = append(r.Problems, fmt.Sprintf("create %s: %v", d, err))
			continue
		}
		if created {
			r.Created = append(r.Created, d)
		}
	}
	if o.ApplyACLs {
		r.applyAll(DirTargets(dataDir))
	}
	toks, err := secrets.EnsureTokens(dataDir, o.Tokens)
	if err != nil {
		return secrets.Tokens{}, r, fmt.Errorf("tokens: %w", err)
	}
	if o.ApplyACLs {
		r.applyAll(TokenTargets(dataDir, o.Tokens))
	}
	if o.ServiceName != "" {
		changed, err := winsvc.EnsureSettings(o.ServiceName, 0)
		for _, c := range changed {
			r.Repaired = append(r.Repaired, "service "+c)
		}
		if err != nil {
			r.Problems = append(r.Problems, "service settings: "+err.Error())
		}
	}
	return toks, r, nil
}

func (r *Report) applyAll(ts []Target) {
	for _, t := range ts {
		repaired, err := applyACL(t)
		switch {
		case err != nil:
			r.Problems = append(r.Problems, fmt.Sprintf("ACL on %s: %v", t.Path, err))
			if t.Secret {
				r.SecretsUnprotected = true
			}
		case repaired:
			r.Repaired = append(r.Repaired, "ACL on "+t.Path)
		}
	}
}

func subdirPaths(dataDir string) []string {
	out := make([]string, len(Subdirs))
	for i, s := range Subdirs {
		out[i] = filepath.Join(dataDir, s)
	}
	return out
}

func mkdir(p string) (bool, error) {
	if fi, err := os.Stat(p); err == nil {
		if !fi.IsDir() {
			return false, errors.New("exists and is not a folder")
		}
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	// 0o700: meaningful off Windows; on Windows the ACLs above apply.
	if err := os.MkdirAll(p, 0o700); err != nil {
		return false, err
	}
	return true, nil
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
