// Package web implements the Skirk Web UI: a small, single-binary HTTP
// dashboard for managing a Skirk kit (mailboxes, status, OAuth, etc.). It is
// served by the `skirk web` subcommand and intentionally has no third-party
// HTTP framework dependencies — only the Go standard library plus the rest of
// the skirk module.
//
// PR #1 (this file is part of) implements the backend skeleton:
//
//   - `skirk web` subcommand with bind/port/auth flags.
//   - Bearer-token + session-cookie auth with CSRF protection on mutations.
//   - Non-interactive wrappers around the existing `mailbox` CLI helpers so
//     the same logic can be reused over HTTP.
//   - REST endpoints for status, mailbox list/add/remove/promote/rename, and
//     stubs for the OAuth device-code flow.
//   - An embed.FS bundle that ships the (currently minimal) static UI inside
//     the binary so deployments stay single-file.
//
// The frontend (Alpine.js dashboard, i18n, RTL styling) lands in PR #1 step 2.
package web

import "embed"

// staticFS is the embedded directory containing the static frontend assets
// (index.html, app.js, styles.css, i18n/*.json, ...). It is exposed via the
// `/` route by Server.
//
//go:embed all:static
var staticFS embed.FS
