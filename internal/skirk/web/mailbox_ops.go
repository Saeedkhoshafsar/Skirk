package web

import (
	"context"
	"regexp"
	"strings"

	"skirk/internal/skirk"
)

// labelRegex mirrors the validation used by `skirk mailbox add` so the web
// layer can reject obviously-bad input before sending it to the kit. It
// must stay in lock-step with looksLikeSafeLabel in cmd/skirk/mailbox.go.
var labelRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// LooksLikeSafeLabel is the web-side mirror of cmd/skirk's looksLikeSafeLabel.
// Exposed so the API layer can give a clean 400 error before mutating the kit.
func LooksLikeSafeLabel(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	return labelRegex.MatchString(value)
}

// MailboxInfo describes a single mailbox in a kit's exit.json. It mirrors the
// shape the UI needs: enough to render a card (label/folder/oauth mode) but
// without any secrets.
type MailboxInfo struct {
	Index     int    `json:"index"` // 0 = primary, 1.. = extras
	Label     string `json:"label"`
	IsPrimary bool   `json:"is_primary"`
	FolderID  string `json:"folder_id,omitempty"`
	Space     string `json:"space,omitempty"`
	// OAuthMode is "easy" when the auth block uses the bundled (shared)
	// Google OAuth client and "personal" when it carries its own
	// client_id/client_secret. Empty when the entry has no refresh token
	// (e.g. static access_token or token_command).
	OAuthMode string `json:"oauth_mode,omitempty"`
	// HasRefreshToken is true if the mailbox uses the standard refresh
	// token flow (vs. static access_token / token_command). UI uses this
	// to decide whether to offer a "re-login" affordance.
	HasRefreshToken bool `json:"has_refresh_token"`
}

// AddMailboxRequest is the public input shape for POST /api/mailboxes.
type AddMailboxRequest struct {
	Label     string `json:"label"`
	OAuthMode string `json:"oauth_mode"` // "easy" or "personal"
	// PersonalClientID / PersonalClientSecret are only consulted when
	// OAuthMode == "personal".
	PersonalClientID     string `json:"personal_client_id,omitempty"`
	PersonalClientSecret string `json:"personal_client_secret,omitempty"`
}

// AddMailboxResult mirrors the JSON returned by `skirk mailbox add --json`.
type AddMailboxResult struct {
	Label          string `json:"label"`
	FolderID       string `json:"folder_id"`
	GoogleAccount  string `json:"google_account"`
	TotalMailboxes int    `json:"total_mailboxes"`
	ExitConfig     string `json:"exit_config"`
	ClientUpdated  bool   `json:"client_updated"`
}

// RemoveMailboxResult mirrors the JSON returned by `skirk mailbox remove --json`.
type RemoveMailboxResult struct {
	Label          string `json:"label"`
	RemovedIndex   int    `json:"removed_index"`
	TotalMailboxes int    `json:"total_mailboxes"`
	ClientUpdated  bool   `json:"client_updated"`
}

// PromoteMailboxResult mirrors the JSON returned by `skirk mailbox promote --json`.
type PromoteMailboxResult struct {
	PromotedLabel string `json:"promoted_label"`
	ClientUpdated bool   `json:"client_updated"`
}

// MailboxOps is the surface the web package needs from the rest of the binary
// to mutate a kit. The cmd/skirk package provides the concrete implementation
// (see cmd/skirk/web_ops.go); tests provide a fake. This keeps internal/skirk/web
// free of the cmd/skirk interactive plumbing.
type MailboxOps interface {
	// List returns a snapshot of the kit's mailboxes (primary + extras),
	// derived from <kitDir>/exit.json.
	List(kitDir string) ([]MailboxInfo, error)

	// Add launches a Google OAuth login (currently device-code flow only in
	// the web UI) and appends a new mailbox. Implementations may block on
	// user interaction; the web layer drives this through a session-based
	// device-code handshake exposed via /api/oauth/device.
	Add(ctx context.Context, kitDir string, req AddMailboxRequest) (AddMailboxResult, error)

	// Remove deletes an extra mailbox by label. The primary mailbox cannot
	// be removed.
	Remove(ctx context.Context, kitDir, label string) (RemoveMailboxResult, error)

	// Promote swaps the named extra mailbox into the primary slot.
	Promote(ctx context.Context, kitDir, label string) (PromoteMailboxResult, error)

	// Rename changes the human-readable label of an existing mailbox
	// (primary or extra). The new label must satisfy LooksLikeSafeLabel
	// and must not collide with any existing label in the kit.
	Rename(ctx context.Context, kitDir, oldLabel, newLabel string) error

	// ClientProfile returns the current client.skirk text (the one-line
	// portable config). Empty string means the kit has no client.json yet.
	ClientProfile(kitDir string) (string, error)
}

// LabelOrDefault returns label if non-empty, otherwise a deterministic default
// like "mailbox-2" derived from the 1-based mailbox position. It mirrors
// labelOrDefault in cmd/skirk/mailbox.go so UI labels stay stable.
func LabelOrDefault(label string, oneBasedIndex int) string {
	if l := strings.TrimSpace(label); l != "" {
		return l
	}
	if oneBasedIndex <= 1 {
		return "primary"
	}
	// matches the cmd/skirk format: "mailbox-N"
	return autoNumberedLabel(oneBasedIndex)
}

func autoNumberedLabel(oneBasedIndex int) string {
	// Inline tiny itoa to avoid pulling in strconv just for one call.
	const digits = "0123456789"
	if oneBasedIndex < 10 {
		return "mailbox-" + string(digits[oneBasedIndex])
	}
	var out []byte
	n := oneBasedIndex
	for n > 0 {
		out = append([]byte{digits[n%10]}, out...)
		n /= 10
	}
	return "mailbox-" + string(out)
}

// MailboxOpsFromConfig produces a MailboxInfo list directly from a loaded
// skirk.Config. It is exposed so the cmd/skirk implementation can reuse the
// same projection logic the tests rely on.
func MailboxOpsFromConfig(cfg *skirk.Config) []MailboxInfo {
	out := make([]MailboxInfo, 0, 1+len(cfg.Drive.ExtraMailboxes))
	out = append(out, MailboxInfo{
		Index:           0,
		Label:           LabelOrDefault(primaryLabel(cfg), 1),
		IsPrimary:       true,
		FolderID:        cfg.Drive.FolderID,
		Space:           cfg.Drive.Space,
		OAuthMode:       oauthModeFor(cfg.Auth),
		HasRefreshToken: cfg.Auth.RefreshToken != "",
	})
	for i, mb := range cfg.Drive.ExtraMailboxes {
		out = append(out, MailboxInfo{
			Index:           i + 1,
			Label:           LabelOrDefault(mb.Label, i+2),
			IsPrimary:       false,
			FolderID:        mb.FolderID,
			Space:           mb.Space,
			OAuthMode:       oauthModeFor(mb.Auth),
			HasRefreshToken: mb.Auth.RefreshToken != "",
		})
	}
	return out
}

// primaryLabel is a small helper: the primary mailbox doesn't have a label
// field of its own, so we synthesise one from cfg.Client.ID when possible.
// Otherwise we fall back to "primary".
func primaryLabel(cfg *skirk.Config) string {
	if cfg == nil {
		return ""
	}
	// Today the primary mailbox does not carry an explicit label in
	// exit.json; future schema work may add cfg.Drive.PrimaryLabel. For
	// now we always render "primary" so the UI is unambiguous.
	return "primary"
}

func oauthModeFor(a skirk.AuthConfig) string {
	if a.ClientID != "" && a.ClientSecret != "" {
		return "personal"
	}
	if a.RefreshToken != "" {
		return "easy"
	}
	return ""
}
