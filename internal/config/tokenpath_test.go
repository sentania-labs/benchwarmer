package config

import "testing"

// Token files are created as SYSTEM, so their references must stay inside
// the data folder.
func TestTokenFileMustStayInDataFolder(t *testing.T) {
	for _, p := range []string{`secrets\management.token`, `secrets/x.token`, `keys\sub\agent.token`, `x.token`, `secrets\.\x.token`} {
		c := Clone(Default())
		c.Security.AgentTokenFile = p
		if err := Validate(c); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}
	for _, p := range []string{
		`C:\Windows\x.token`, `C:x.token`, `\\server\share\x.token`, `\x.token`, `/etc/x.token`,
		`..\x.token`, `secrets\..\..\x.token`, `secrets/../../x.token`, `secrets\x.token:stream`, `.`, `secrets\..`,
	} {
		c := Clone(Default())
		c.Security.ManagementTokenFile = p
		if _, ok := fieldErrs(t, Validate(c))["security.management_token_file"]; !ok {
			t.Errorf("%q accepted", p)
		}
	}
}
