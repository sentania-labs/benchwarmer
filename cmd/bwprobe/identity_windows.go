//go:build windows

package main

import (
	"golang.org/x/sys/windows"
)

func identity() any {
	type id struct {
		SessionID uint32   `json:"session_id"`
		Elevated  bool     `json:"elevated"`
		Service   bool     `json:"session0"`
		Groups    []string `json:"groups,omitempty"`
	}
	var r id
	_ = windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &r.SessionID)
	r.Service = r.SessionID == 0
	tok := windows.GetCurrentProcessToken()
	r.Elevated = tok.IsElevated()
	if gs, err := tok.GetTokenGroups(); err == nil {
		for _, g := range gs.AllGroups() {
			if acct, dom, _, err := g.Sid.LookupAccount(""); err == nil {
				name := acct
				if dom != "" {
					name = dom + `\` + acct
				}
				r.Groups = append(r.Groups, name)
			}
		}
	}
	return r
}
