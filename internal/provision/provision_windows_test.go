//go:build windows

package provision

import (
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

func TestEnsureACLs(t *testing.T) {
	requireElevated(t)
	dir := filepath.Join(t.TempDir(), "Benchwarmer")
	_, r, err := Ensure(dir, Options{Tokens: defaultFiles(), ApplyACLs: true})
	if err != nil {
		t.Fatal(err)
	}
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
		p, ex, in := acl(t, tt.path)
		if p != tt.protected || !slices.Equal(ex, canon(t, tt.explicit)) || !slices.Equal(in, tt.inherited) {
			t.Errorf("%s:\n got protected=%v explicit=%v inherited=%v\nwant protected=%v explicit=%v inherited=%v",
				tt.path, p, ex, in, tt.protected, canon(t, tt.explicit), tt.inherited)
		}
	}

	// A second run on a correct install changes nothing.
	_, r, err = Ensure(dir, Options{Tokens: defaultFiles(), ApplyACLs: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Created)+len(r.Repaired)+len(r.Problems) != 0 {
		t.Fatalf("second run report %+v", r)
	}
}

func TestEnsureRepairsDrift(t *testing.T) {
	requireElevated(t)
	dir := filepath.Join(t.TempDir(), "Benchwarmer")
	if _, _, err := Ensure(dir, Options{Tokens: defaultFiles(), ApplyACLs: true}); err != nil {
		t.Fatal(err)
	}
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

	_, r, err := Ensure(dir, Options{Tokens: defaultFiles(), ApplyACLs: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Problems) != 0 {
		t.Fatalf("problems %v", r.Problems)
	}
	var got []string
	for _, s := range r.Repaired {
		got = append(got, strings.TrimPrefix(s, "ACL on "))
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

	_, r, err = Ensure(dir, Options{Tokens: defaultFiles(), ApplyACLs: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Repaired)+len(r.Problems) != 0 {
		t.Fatalf("run after repair %+v", r)
	}
}
