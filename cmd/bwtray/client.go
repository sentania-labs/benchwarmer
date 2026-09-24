// Command bwtray is Benchwarmer's system-tray app and session agent. It shows
// the worker's condition, switches manual modes, opens the dashboard, and
// reports foreground, fullscreen, and idle facts from the user's session,
// which the service cannot observe from session 0 (ADR 0005).
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/api"
	"github.com/sentania-labs/benchwarmer/internal/policy"
)

// client talks to the management API with the agent token.
type client struct {
	base      string
	tokenFile string
	http      *http.Client
}

func newClient(base, tokenFile string) *client {
	return &client{base: strings.TrimRight(base, "/"), tokenFile: tokenFile, http: &http.Client{Timeout: 5 * time.Second}}
}

// token is re-read each call so a token rotated by the installer is picked up.
func (c *client) token() string {
	b, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (c *client) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if t := c.token(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e api.Error
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *client) status() (api.Status, error) {
	var s api.Status
	err := c.do(http.MethodGet, "/api/v1/status", nil, &s)
	return s, err
}

func (c *client) setMode(m policy.Mode, duration string) error {
	return c.do(http.MethodPut, "/api/v1/mode", api.ModeRequest{Mode: m, Duration: duration, SetBy: "tray"}, nil)
}

func (c *client) report(r api.AgentReport) error {
	return c.do(http.MethodPost, "/api/v1/agent/report", r, nil)
}

// tooltip renders a short status line for the tray icon.
func tooltip(s api.Status, err error) string {
	if err != nil {
		return "Benchwarmer: service not reachable"
	}
	t := "Benchwarmer: " + s.Summary
	if s.Mode.Mode != policy.ModeAuto {
		t += " [" + modeLabel(s.Mode.Mode) + untilLabel(s.Mode.Until) + "]"
	}
	// Windows truncates tray tooltips at 127 characters.
	if len(t) > 120 {
		t = t[:117] + "..."
	}
	return t
}

func modeLabel(m policy.Mode) string {
	switch m {
	case policy.ModePause:
		return "Paused"
	case policy.ModeAIPriority:
		return "AI Priority"
	}
	return "Auto"
}

func untilLabel(u *time.Time) string {
	if u == nil {
		return ""
	}
	l := u.Local()
	if y, m, d := l.Date(); y == time.Now().Year() && m == time.Now().Month() && d == time.Now().Day() {
		return " until " + l.Format("3:04 PM")
	}
	return fmt.Sprintf(" until %s", l.Format("Mon 3:04 PM"))
}
