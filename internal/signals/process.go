// Package signals collects host facts other than GPU telemetry: running
// processes, the interactive session's foreground window and input idle
// time, and session presence.
package signals

import "time"

// Process is one running process.
type Process struct {
	PID        uint32    `json:"pid"`
	PPID       uint32    `json:"ppid"`
	Name       string    `json:"name"`
	Path       string    `json:"path,omitempty"` // empty when not readable
	SessionID  uint32    `json:"session_id"`
	CreateTime time.Time `json:"create_time,omitzero"`
}

// Snapshot lists running processes. Paths are best effort: the caller may
// lack rights to query some processes, which is itself a Phase 0 finding.
func Snapshot() ([]Process, error) { return snapshot() }

// SessionSignals are facts only observable from inside the interactive user
// session (ADR 0005). The session agent collects them and reports them to the
// service.
type SessionSignals struct {
	Time           time.Time `json:"time"`
	SessionID      uint32    `json:"session_id"`
	ForegroundPID  uint32    `json:"foreground_pid,omitempty"`
	ForegroundName string    `json:"foreground_name,omitempty"`
	ForegroundPath string    `json:"foreground_path,omitempty"`
	// Fullscreen is true when the foreground window covers its whole monitor
	// (exclusive or borderless) and is not the desktop or shell.
	Fullscreen bool `json:"fullscreen"`
	// NotificationState is SHQueryUserNotificationState's verdict, e.g.
	// "d3d_fullscreen", "busy", "presentation", "accepts_notifications".
	NotificationState string  `json:"notification_state,omitempty"`
	IdleSeconds       float64 `json:"idle_seconds"`
}

// Session describes a Windows logon session.
type Session struct {
	ID      uint32 `json:"id"`
	Name    string `json:"name"`
	State   string `json:"state"` // "active", "connected", "disconnected", ...
	Console bool   `json:"console"`
}
