package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// handleMailboxList serves GET /api/mailboxes — returns the full list of
// mailboxes (primary + extras) in the kit.
func (s *Server) handleMailboxList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ops == nil {
		writeError(w, http.StatusServiceUnavailable, "mailbox operations not available")
		return
	}
	list, err := s.ops.List(s.kitDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kit_dir":   s.kitDir,
		"mailboxes": list,
		"count":     len(list),
	})
}

// handleMailboxAdd serves POST /api/mailboxes. PR #1 ships a stub that
// returns 501; the real implementation lands together with the OAuth
// device-code session machinery in step 2.
func (s *Server) handleMailboxAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ops == nil {
		writeError(w, http.StatusServiceUnavailable, "mailbox operations not available")
		return
	}

	var req AddMailboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Label = strings.TrimSpace(req.Label)
	req.OAuthMode = strings.ToLower(strings.TrimSpace(req.OAuthMode))
	if req.OAuthMode == "" {
		req.OAuthMode = "easy"
	}
	if req.OAuthMode != "easy" && req.OAuthMode != "personal" {
		writeError(w, http.StatusBadRequest, "oauth_mode must be \"easy\" or \"personal\"")
		return
	}
	if req.Label != "" && !LooksLikeSafeLabel(req.Label) {
		writeError(w, http.StatusBadRequest,
			"label must be 1-64 chars, start with [A-Za-z0-9], and contain only letters, digits, '.', '_' or '-'")
		return
	}
	if req.OAuthMode == "personal" {
		if strings.TrimSpace(req.PersonalClientID) == "" || strings.TrimSpace(req.PersonalClientSecret) == "" {
			writeError(w, http.StatusBadRequest,
				"personal_client_id and personal_client_secret are required when oauth_mode=\"personal\"")
			return
		}
	}

	result, err := s.ops.Add(r.Context(), s.kitDir, req)
	if err != nil {
		// Map a few common kit-level errors to friendlier HTTP codes.
		status := http.StatusInternalServerError
		msg := err.Error()
		switch {
		case strings.Contains(msg, "already has 15 entries"),
			strings.Contains(msg, "max 15"):
			status = http.StatusUnprocessableEntity
		case strings.Contains(msg, "already configured"):
			status = http.StatusConflict
		case errors.Is(err, errNotImplemented),
			strings.Contains(strings.ToLower(msg), "not implemented"):
			status = http.StatusNotImplemented
		}
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// handleMailboxRemove serves DELETE /api/mailboxes/{label}.
func (s *Server) handleMailboxRemove(w http.ResponseWriter, r *http.Request, label string) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ops == nil {
		writeError(w, http.StatusServiceUnavailable, "mailbox operations not available")
		return
	}
	if label == "" {
		writeError(w, http.StatusBadRequest, "label path parameter is required")
		return
	}
	result, err := s.ops.Remove(r.Context(), s.kitDir, label)
	if err != nil {
		status := http.StatusInternalServerError
		msg := err.Error()
		switch {
		case strings.Contains(msg, "no extra mailboxes"),
			strings.Contains(msg, "not found"),
			strings.Contains(msg, "no mailbox with label"):
			status = http.StatusNotFound
		case strings.Contains(msg, "primary mailbox cannot be removed"):
			status = http.StatusForbidden
		}
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleMailboxPromote serves POST /api/mailboxes/{label}/promote.
func (s *Server) handleMailboxPromote(w http.ResponseWriter, r *http.Request, label string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ops == nil {
		writeError(w, http.StatusServiceUnavailable, "mailbox operations not available")
		return
	}
	if label == "" {
		writeError(w, http.StatusBadRequest, "label path parameter is required")
		return
	}
	result, err := s.ops.Promote(r.Context(), s.kitDir, label)
	if err != nil {
		status := http.StatusInternalServerError
		msg := err.Error()
		switch {
		case strings.Contains(msg, "no extra mailboxes"),
			strings.Contains(msg, "not found"),
			strings.Contains(msg, "no mailbox with label"):
			status = http.StatusNotFound
		case strings.Contains(msg, "already primary"):
			status = http.StatusConflict
		}
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// renameRequest is the JSON body of PATCH /api/mailboxes/{label}.
type renameRequest struct {
	Label string `json:"label"`
}

// handleMailboxRename serves PATCH /api/mailboxes/{label} — currently only
// supports updating the label.
func (s *Server) handleMailboxRename(w http.ResponseWriter, r *http.Request, label string) {
	if r.Method != http.MethodPatch {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ops == nil {
		writeError(w, http.StatusServiceUnavailable, "mailbox operations not available")
		return
	}
	if label == "" {
		writeError(w, http.StatusBadRequest, "label path parameter is required")
		return
	}
	var req renameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	newLabel := strings.TrimSpace(req.Label)
	if !LooksLikeSafeLabel(newLabel) {
		writeError(w, http.StatusBadRequest,
			"label must be 1-64 chars, start with [A-Za-z0-9], and contain only letters, digits, '.', '_' or '-'")
		return
	}
	if newLabel == label {
		writeError(w, http.StatusBadRequest, "new label is identical to the current label")
		return
	}
	if err := s.ops.Rename(r.Context(), s.kitDir, label, newLabel); err != nil {
		status := http.StatusInternalServerError
		msg := err.Error()
		switch {
		case strings.Contains(msg, "not found"),
			strings.Contains(msg, "no mailbox with label"):
			status = http.StatusNotFound
		case strings.Contains(msg, "duplicate"),
			strings.Contains(msg, "already in use"):
			status = http.StatusConflict
		}
		writeError(w, status, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"old_label": label,
		"new_label": newLabel,
	})
}

// handleClientProfile serves GET /api/client-profile — returns the current
// client.skirk one-line text. It is intentionally a separate endpoint from
// /api/status so the (potentially large) profile string doesn't bloat status
// polls.
func (s *Server) handleClientProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ops == nil {
		writeError(w, http.StatusServiceUnavailable, "mailbox operations not available")
		return
	}
	profile, err := s.ops.ClientProfile(s.kitDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kit_dir":                s.kitDir,
		"client_profile":         profile,
		"client_profile_present": profile != "",
	})
}

// errNotImplemented is returned by MailboxOps implementations that have not
// yet wired up a feature (e.g. the OAuth device-code Add flow in PR #1).
var errNotImplemented = errors.New("not implemented yet")
