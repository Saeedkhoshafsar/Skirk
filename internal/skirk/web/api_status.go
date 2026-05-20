package web

import (
	"net/http"
	"runtime"
	"time"
)

// StatusInfo is the JSON payload returned by GET /api/status.
type StatusInfo struct {
	Version        string `json:"version"`
	Commit         string `json:"commit,omitempty"`
	BuildDate      string `json:"build_date,omitempty"`
	GoVersion      string `json:"go_version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
	KitDir         string `json:"kit_dir"`
	StartedAt      string `json:"started_at"`
	UptimeSeconds  int64  `json:"uptime_seconds"`
	MailboxCount   int    `json:"mailbox_count"` // includes primary
	PrimaryAccount string `json:"primary_account,omitempty"`
	AuthRequired   bool   `json:"auth_required"`
}

// handleStatus serves GET /api/status. It is always available behind auth
// (or unconditionally when --no-auth is set on a loopback bind).
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	uptime := time.Since(s.startedAt)
	info := StatusInfo{
		Version:       s.versionInfo.Version,
		Commit:        s.versionInfo.Commit,
		BuildDate:     s.versionInfo.Date,
		GoVersion:     runtime.Version(),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		KitDir:        s.kitDir,
		StartedAt:     s.startedAt.UTC().Format(time.RFC3339),
		UptimeSeconds: int64(uptime.Seconds()),
		AuthRequired:  !s.auth.cfg.NoAuth,
	}
	// MailboxCount is a best-effort read of the current kit; we tolerate
	// a missing/invalid exit.json by leaving the count at zero.
	if s.ops != nil {
		if list, err := s.ops.List(s.kitDir); err == nil {
			info.MailboxCount = len(list)
		}
	}
	writeJSON(w, http.StatusOK, info)
}
