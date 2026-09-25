//go:build windows

package provision

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// These tests apply real ACLs to a temporary folder. They need an elevated
// token (CI's windows-latest runner is): the result is readable only by
// SYSTEM and Administrators, so a non-elevated run could not clean up.
func requireElevated(t *testing.T) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("needs an elevated (administrator) token")
	}
}

var aceRE = regexp.MustCompile(`\([^)]*\)`)

// acl reads path's DACL as SDDL and splits it into protection, explicit ACEs,
// and inherited ACEs (sorted), independently of the code under test.
func acl(t *testing.T, path string) (protected bool, explicit, inherited []string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	s := sd.String()
	head, _, _ := strings.Cut(strings.TrimPrefix(s, "D:"), "(")
	protected = strings.Contains(head, "P")
	for _, a := range aceRE.FindAllString(s, -1) {
		flags := strings.Split(a, ";")[1]
		if strings.Contains(flags, "ID") {
			inherited = append(inherited, a)
		} else {
			explicit = append(explicit, a)
		}
	}
	sort.Strings(explicit)
	sort.Strings(inherited)
	return protected, explicit, inherited
}

// canon renders SDDL ACEs the way Windows prints them back (masks such as
// 0x20 come back as aliases), sorted.
func canon(t *testing.T, sddl string) []string {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	out := aceRE.FindAllString(sd.String(), -1)
	sort.Strings(out)
	return out
}

func TestProvisionACLs(t *testing.T) {
	requireElevated(t)
	dir := filepath.Join(t.TempDir(), "Benchwarmer")
	_, r := provision(t, dir, Options{ApplyACLs: true})
	if len(r.Problems) != 0 || r.SecretsUnprotected {
		t.Fatalf("report %+v", r)
	}

	fromData := canon(t, "D:(A;OICIID;FA;;;SY)(A;OICIID;FA;;;BA)")
	j := func(p ...string) string { return filepath.Join(append([]string{dir}, p...)...) }
	tests := []struct {
		path      string
		protected bool
		explicit  string
		inherited []string
	}{
		{dir, true, DataSDDL, nil},
		{j("secrets"), false, SecretsSDDL, fromData},
		{j("tls"), false, InheritOnlySDDL, fromData},
		{j("logs"), false, UsersReadSDDL, fromData},
		{j("models"), false, UsersReadSDDL, fromData},
		{j("secrets", "management.token"), true, TokenSDDL, nil},
		{j("secrets", "inference.token"), true, TokenSDDL, nil},
		{j("secrets", "agent.token"), true, AgentTokenSDDL, nil},
	}
	for _, tt := range tests {
		if o := owner(t, tt.path); o != SIDAdministrators {
			t.Errorf("%s: owner %s", tt.path, o)
		}
		p, ex, in := acl(t, tt.path)
		if p != tt.protected || !slices.Equal(ex, canon(t, tt.explicit)) || !slices.Equal(in, tt.inherited) {
			t.Errorf("%s:\n got protected=%v explicit=%v inherited=%v\nwant protected=%v explicit=%v inherited=%v",
				tt.path, p, ex, in, tt.protected, canon(t, tt.explicit), tt.inherited)
		}
	}

	// A second run on a correct install changes nothing.
	_, r = provision(t, dir, Options{ApplyACLs: true})
	if len(r.Created)+len(r.Repaired)+len(r.Problems) != 0 {
		t.Fatalf("second run report %+v", r)
	}
}

func TestProvisionRepairsDrift(t *testing.T) {
	requireElevated(t)
	dir := filepath.Join(t.TempDir(), "Benchwarmer")
	provision(t, dir, Options{ApplyACLs: true})
	tamper := map[string]struct {
		sddl string
		info windows.SECURITY_INFORMATION
	}{
		// Everyone may read the data folder (and so config and database).
		dir: {DataSDDL + "(A;OICI;FR;;;WD)", windows.PROTECTED_DACL_SECURITY_INFORMATION},
		// Secrets cut off from the data folder with users granted read.
		filepath.Join(dir, "secrets"): {"D:P(A;OICI;FA;;;SY)(A;OICI;FR;;;BU)", windows.PROTECTED_DACL_SECURITY_INFORMATION},
		// Users may read the management token.
		filepath.Join(dir, "secrets", "management.token"): {TokenSDDL + "(A;;FR;;;BU)", windows.PROTECTED_DACL_SECURITY_INFORMATION},
		// Logs no longer readable by users.
		filepath.Join(dir, "logs"): {"D:", windows.UNPROTECTED_DACL_SECURITY_INFORMATION},
	}
	for p, x := range tamper {
		sd, err := windows.SecurityDescriptorFromString(x.sddl)
		if err != nil {
			t.Fatal(err)
		}
		d, _, err := sd.DACL()
		if err != nil {
			t.Fatal(err)
		}
		if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|x.info, nil, nil, d, nil); err != nil {
			t.Fatal(err)
		}
	}

	_, r := provision(t, dir, Options{ApplyACLs: true})
	if len(r.Problems) != 0 {
		t.Fatalf("problems %v", r.Problems)
	}
	// A tampered token can be reported twice: Start resets a file it does
	// not recognize, then Finish applies the token ACL.
	var got []string
	for _, s := range r.Repaired {
		if p := strings.TrimPrefix(s, "owner and ACL of "); !slices.Contains(got, p) {
			got = append(got, p)
		}
	}
	var want []string
	for p := range tamper {
		want = append(want, p)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("repaired %v, want %v", got, want)
	}
	if p, ex, _ := acl(t, filepath.Join(dir, "secrets")); p || !slices.Equal(ex, canon(t, SecretsSDDL)) {
		t.Fatalf("secrets not repaired: protected=%v explicit=%v", p, ex)
	}
	// The data folder repair reaches children only by inheritance
	// propagation: Everyone must be gone from tls, which was not touched.
	if _, _, in := acl(t, filepath.Join(dir, "tls")); !slices.Equal(in, canon(t, "D:(A;OICIID;FA;;;SY)(A;OICIID;FA;;;BA)")) {
		t.Fatalf("tls inherited ACEs not refreshed: %v", in)
	}

	_, r = provision(t, dir, Options{ApplyACLs: true})
	if len(r.Repaired)+len(r.Problems) != 0 {
		t.Fatalf("run after repair %+v", r)
	}
}

func owner(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	o, _, err := sd.Owner()
	if err != nil {
		t.Fatal(err)
	}
	return o.String()
}

// plant sets path's owner and DACL the way a standard user who created it
// would leave them. Assigning another user as owner needs the restore
// privilege, which an elevated administrator holds.
func plant(t *testing.T, path, ownerSID, sddl string) {
	t.Helper()
	undo, _ := winOps{}.privileges()
	defer undo()
	sid, err := windows.StringToSid(ownerSID)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, sid, nil, nil, nil); err != nil {
		t.Skipf("cannot assign %s as owner (restore privilege unavailable?): %v", ownerSID, err)
	}
	d, err := parseDACL(sddl)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, d, nil); err != nil {
		t.Fatal(err)
	}
}

const sidUsersWin = "S-1-5-32-545"

// A standard user pre-created the data folder before the install, with a
// known management token, a config, and an Everyone ACE on a file.
func TestPreCreatedFolderIsTakenBack(t *testing.T) {
	requireElevated(t)
	dir := filepath.Join(t.TempDir(), "Benchwarmer")
	planted := strings.Repeat("k", 43)
	tok := filepath.Join(dir, "secrets", "management.token")
	cfg := filepath.Join(dir, "config.json")
	notes := filepath.Join(dir, "notes.txt")
	write(t, tok, planted)
	write(t, cfg, "{}")
	write(t, notes, "x")
	userOwned := "D:(A;OICI;FA;;;BU)(A;OICI;FA;;;WD)"
	for _, p := range []string{dir, filepath.Join(dir, "secrets"), tok, cfg} {
		plant(t, p, sidUsersWin, userOwned)
	}
	plant(t, notes, sidUsersWin, "D:(A;;FA;;;WD)")

	toks, r := provision(t, dir, Options{ApplyACLs: true})
	if toks.Management == planted {
		t.Fatal("planted management token kept")
	}
	if r.SecretsUnprotected || len(r.Problems) != 1 || !strings.Contains(r.Problems[0], "config.json") {
		t.Fatalf("report %+v", r)
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatal("untrusted config.json still in place")
	}
	for _, p := range []string{dir, filepath.Join(dir, "secrets"), tok, notes} {
		if o := owner(t, p); o != SIDAdministrators {
			t.Errorf("%s: owner %s", p, o)
		}
	}
	if _, ex, in := acl(t, notes); len(ex) != 0 || slices.ContainsFunc(in, func(a string) bool { return strings.HasSuffix(a, ";WD)") || strings.HasSuffix(a, ";BU)") }) {
		t.Errorf("notes.txt keeps planted access: explicit=%v inherited=%v", ex, in)
	}
	if p, ex, _ := acl(t, dir); !p || !slices.Equal(ex, canon(t, DataSDDL)) {
		t.Errorf("data folder: protected=%v explicit=%v", p, ex)
	}

	// Fixed, so the next start changes nothing and blocks nothing.
	_, r = provision(t, dir, Options{ApplyACLs: true})
	if len(r.Repaired)+len(r.Problems) != 0 || r.SecretsUnprotected {
		t.Fatalf("second run %+v", r)
	}
}

// A junction for secrets is refused: not followed, not written through,
// and loading is blocked.
func TestJunctionIsRefused(t *testing.T) {
	requireElevated(t)
	dir := filepath.Join(t.TempDir(), "Benchwarmer")
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(dir, "secrets"), target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v: %s", err, out)
	}
	before := sddlOf(t, target)
	beforeOwner := owner(t, target)

	toks, r := provision(t, dir, Options{ApplyACLs: true})
	if !r.SecretsUnprotected || toks.Management == "" {
		t.Fatalf("report %+v", r)
	}
	if list, _ := os.ReadDir(target); len(list) != 0 {
		t.Fatalf("wrote through the junction: %v", list)
	}
	if after := sddlOf(t, target); after != before {
		t.Fatalf("junction target ACL changed:\n before %s\n after  %s", before, after)
	}
	if o := owner(t, target); o != beforeOwner {
		t.Fatalf("junction target owner changed to %s", o)
	}
}

func sddlOf(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}
