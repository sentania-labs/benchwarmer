package config

import "testing"

// Token files are created as SYSTEM, so new references must stay inside
// the data folder. A file written by an older version still loads (the
// service refuses an outside path at run time, see internal/provision).
func TestTokenFileMustStayInDataFolder(t *testing.T) {
	for _, p := range []string{`secrets\management.token`, `secrets/x.token`, `keys\sub\agent.token`, `x.token`, `secrets\.\x.token`} {
		c := Clone(Default())
		c.Security.AgentTokenFile = p
		if err := ValidateChange(c); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}
	for _, p := range []string{
		`C:\Windows\x.token`, `C:x.token`, `\\server\share\x.token`, `\x.token`, `/etc/x.token`,
		`..\x.token`, `secrets\..\..\x.token`, `secrets/../../x.token`, `secrets\x.token:stream`, `.`, `secrets\..`,
	} {
		c := Clone(Default())
		c.Security.ManagementTokenFile = p
		if _, ok := fieldErrs(t, ValidateChange(c))["security.management_token_file"]; !ok {
			t.Errorf("%q accepted", p)
		}
		if err := Validate(c); err != nil {
			t.Errorf("%q: an existing file must still load: %v", p, err)
		}
	}
}
