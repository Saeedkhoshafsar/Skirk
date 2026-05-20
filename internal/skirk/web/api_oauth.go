package web

import (
	"net/http"
)

// OAuth device-code endpoints. PR #1 ships these as 501 stubs that document
// the wire format the UI will eventually consume; the actual device-code
// orchestration is implemented in PR #1 step 2 together with the Alpine.js
// frontend that drives the polling loop.
//
// Wire format (final shape, NOT yet implemented):
//
//   POST /api/oauth/device
//     request:  { "oauth_mode": "easy"|"personal", "personal_client_id"?, "personal_client_secret"? }
//     response: { "session_id": "...", "verification_url": "...", "user_code": "...",
//                 "expires_in_seconds": 600, "interval_seconds": 5 }
//
//   GET /api/oauth/poll?session=<id>
//     response: { "status": "pending"|"authorized"|"denied"|"expired",
//                 "account"?: "user@example.com" }
//
// On status=="authorized" the UI calls POST /api/mailboxes with the session id
// to finalise mailbox creation server-side (no token ever crosses the wire to
// the browser).

func (s *Server) handleOAuthDevice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeError(w, http.StatusNotImplemented,
		"OAuth device-code flow over the web UI is not implemented in PR #1; use `skirk mailbox add` on the host CLI for now")
}

func (s *Server) handleOAuthPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeError(w, http.StatusNotImplemented,
		"OAuth device-code flow over the web UI is not implemented in PR #1")
}
