package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"skirk/internal/skirk"
)

// writeTestKit creates a minimal but valid skirk-kit directory layout the
// mailbox.go commands can operate on. It returns the kit directory path so
// individual tests can pass --kit to the command flag parsers.
func writeTestKit(t *testing.T, primary skirk.MailboxConfig, extras []skirk.MailboxConfig) string {
	t.Helper()
	dir := t.TempDir()
	secret, err := skirk.RandomSecret()
	if err != nil {
		t.Fatal(err)
	}
	session, err := skirk.NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	sessionID := skirk.SessionString(session)

	tunnel := skirk.TunnelConfig{
		Listen:           "127.0.0.1:18080",
		Profile:          "auto",
		ChunkSize:        8 * 1024 * 1024,
		PollIntervalMS:   1000,
		Concurrency:      32,
		CleanupProcessed: true,
	}
	drive := skirk.DriveConfig{FolderID: primary.FolderID, Space: primary.Space}
	if len(extras) > 0 {
		drive.ExtraMailboxes = append(drive.ExtraMailboxes, extras...)
	}
	exitCfg := skirk.Config{
		Secret:    secret,
		SessionID: sessionID,
		Auth:      primary.Auth,
		Route:     skirk.RouteConfig{Mode: "direct", GoogleIP: "216.239.38.120", TimeoutSeconds: 240},
		Drive:     drive,
		Tunnel:    tunnel,
	}
	clientCfg := exitCfg
	clientCfg.Route = skirk.RouteConfig{Mode: "google_front_pinned", GoogleIP: "216.239.38.120", TimeoutSeconds: 240}

	if err := writeJSONFile(filepath.Join(dir, "exit.json"), exitCfg); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(filepath.Join(dir, "client.json"), clientCfg); err != nil {
		t.Fatal(err)
	}
	clientText, err := skirk.EncodeConfigText(&clientCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTextFile(filepath.Join(dir, "client.skirk"), clientText+"\n"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func samplePrimary() skirk.MailboxConfig {
	return skirk.MailboxConfig{
		Auth: skirk.AuthConfig{
			ClientID:     "primary-client",
			ClientSecret: "primary-secret",
			RefreshToken: "primary-refresh",
			TokenURL:     "https://oauth2.googleapis.com/token",
		},
		FolderID: "primary-folder",
	}
}

func sampleExtra(label, refresh, folder string) skirk.MailboxConfig {
	return skirk.MailboxConfig{
		Auth: skirk.AuthConfig{
			ClientID:     "extra-client",
			ClientSecret: "extra-secret",
			RefreshToken: refresh,
			TokenURL:     "https://oauth2.googleapis.com/token",
		},
		FolderID: folder,
		Label:    label,
	}
}

func TestMailboxEntriesPrimaryAlwaysIndexZero(t *testing.T) {
	cfg := &skirk.Config{
		Auth:  skirk.AuthConfig{RefreshToken: "p"},
		Drive: skirk.DriveConfig{FolderID: "primary", ExtraMailboxes: []skirk.MailboxConfig{sampleExtra("alpha", "a", "f1")}},
	}
	got := mailboxEntries(cfg)
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].Role != "primary" || got[0].Index != 0 || got[0].Label != "primary" {
		t.Fatalf("primary entry wrong: %+v", got[0])
	}
	if got[1].Role != "extra" || got[1].Index != 1 || got[1].Label != "alpha" {
		t.Fatalf("extra entry wrong: %+v", got[1])
	}
}

func TestMailboxEntriesGeneratesLabelForEmpty(t *testing.T) {
	cfg := &skirk.Config{
		Auth:  skirk.AuthConfig{RefreshToken: "p"},
		Drive: skirk.DriveConfig{ExtraMailboxes: []skirk.MailboxConfig{{Auth: skirk.AuthConfig{RefreshToken: "x"}, FolderID: "f"}}},
	}
	got := mailboxEntries(cfg)
	if got[1].Label != "alt1" {
		t.Fatalf("expected fallback label alt1, got %q", got[1].Label)
	}
}

func TestResolveExtraMailboxIndexByLabel(t *testing.T) {
	cfg := &skirk.Config{Drive: skirk.DriveConfig{ExtraMailboxes: []skirk.MailboxConfig{
		sampleExtra("alpha", "r1", "f1"),
		sampleExtra("beta", "r2", "f2"),
	}}}
	idx, err := resolveExtraMailboxIndex(cfg, "beta", -1)
	if err != nil || idx != 1 {
		t.Fatalf("expected idx=1 err=nil, got %d %v", idx, err)
	}
}

func TestResolveExtraMailboxIndexByIndex(t *testing.T) {
	cfg := &skirk.Config{Drive: skirk.DriveConfig{ExtraMailboxes: []skirk.MailboxConfig{
		sampleExtra("alpha", "r1", "f1"),
		sampleExtra("beta", "r2", "f2"),
	}}}
	idx, err := resolveExtraMailboxIndex(cfg, "", 2)
	if err != nil || idx != 1 {
		t.Fatalf("expected idx=1 err=nil, got %d %v", idx, err)
	}
}

func TestResolveExtraMailboxIndexRefusesPrimary(t *testing.T) {
	cfg := &skirk.Config{Drive: skirk.DriveConfig{ExtraMailboxes: []skirk.MailboxConfig{sampleExtra("alpha", "r", "f")}}}
	if _, err := resolveExtraMailboxIndex(cfg, "primary", -1); err == nil {
		t.Fatal("expected error for label=primary, got nil")
	}
	if _, err := resolveExtraMailboxIndex(cfg, "", 0); err == nil {
		t.Fatal("expected error for index=0, got nil")
	}
}

func TestResolveExtraMailboxIndexAmbiguousLabel(t *testing.T) {
	cfg := &skirk.Config{Drive: skirk.DriveConfig{ExtraMailboxes: []skirk.MailboxConfig{
		sampleExtra("dup", "r1", "f1"),
		sampleExtra("dup", "r2", "f2"),
	}}}
	if _, err := resolveExtraMailboxIndex(cfg, "dup", -1); err == nil {
		t.Fatal("expected ambiguity error for duplicate label, got nil")
	}
}

func TestResolveNewMailboxLabelAutogenerates(t *testing.T) {
	cfg := &skirk.Config{Drive: skirk.DriveConfig{ExtraMailboxes: []skirk.MailboxConfig{
		sampleExtra("alt1", "r1", "f1"),
		sampleExtra("custom", "r2", "f2"),
	}}}
	got, err := resolveNewMailboxLabel("", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got != "alt2" {
		t.Fatalf("expected alt2, got %q", got)
	}
}

func TestResolveNewMailboxLabelRejectsReserved(t *testing.T) {
	cfg := &skirk.Config{}
	if _, err := resolveNewMailboxLabel("primary", cfg); err == nil {
		t.Fatal("expected error for reserved label primary")
	}
	if _, err := resolveNewMailboxLabel("a", cfg); err == nil {
		t.Fatal("expected error for too-short label")
	}
	if _, err := resolveNewMailboxLabel("bad label!", cfg); err == nil {
		t.Fatal("expected error for unsafe characters")
	}
}

func TestResolveNewMailboxLabelDetectsDuplicate(t *testing.T) {
	cfg := &skirk.Config{Drive: skirk.DriveConfig{ExtraMailboxes: []skirk.MailboxConfig{sampleExtra("Alpha", "r", "f")}}}
	if _, err := resolveNewMailboxLabel("alpha", cfg); err == nil {
		t.Fatal("expected duplicate label detection to be case-insensitive")
	}
}

func TestDuplicateMailboxAuthDetectsPrimaryAndExtra(t *testing.T) {
	cfg := &skirk.Config{
		Auth:  skirk.AuthConfig{RefreshToken: "primary"},
		Drive: skirk.DriveConfig{ExtraMailboxes: []skirk.MailboxConfig{{Auth: skirk.AuthConfig{RefreshToken: "extra-1"}}}},
	}
	if !duplicateMailboxAuth(cfg, skirk.AuthConfig{RefreshToken: "primary"}) {
		t.Fatal("expected match against primary refresh token")
	}
	if !duplicateMailboxAuth(cfg, skirk.AuthConfig{RefreshToken: "extra-1"}) {
		t.Fatal("expected match against extra refresh token")
	}
	if duplicateMailboxAuth(cfg, skirk.AuthConfig{RefreshToken: "fresh"}) {
		t.Fatal("expected no match for fresh refresh token")
	}
	if duplicateMailboxAuth(cfg, skirk.AuthConfig{AccessToken: "anything"}) {
		t.Fatal("static access tokens should not be treated as duplicates")
	}
}

func TestMailboxListJSONOutputsAllMailboxes(t *testing.T) {
	dir := writeTestKit(t, samplePrimary(), []skirk.MailboxConfig{
		sampleExtra("alpha", "alpha-refresh", "alpha-folder"),
		sampleExtra("beta", "beta-refresh", "beta-folder"),
	})

	// Capture stdout while running `mailbox list --json`.
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = old })

	if err := mailboxList(t.Context(), []string{"--kit", dir, "--json"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out := make([]byte, 4096)
	n, _ := r.Read(out)
	var parsed struct {
		Result    string         `json:"result"`
		Total     int            `json:"total"`
		Mailboxes []mailboxEntry `json:"mailboxes"`
	}
	if err := json.Unmarshal(out[:n], &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out[:n])
	}
	if parsed.Result != "ok" {
		t.Fatalf("unexpected result %q", parsed.Result)
	}
	if parsed.Total != 3 {
		t.Fatalf("expected 3 mailboxes, got %d", parsed.Total)
	}
	if parsed.Mailboxes[0].Role != "primary" || parsed.Mailboxes[1].Label != "alpha" || parsed.Mailboxes[2].Label != "beta" {
		t.Fatalf("unexpected mailbox order: %+v", parsed.Mailboxes)
	}
}

// TestMailboxRemoveByLabelUpdatesExitAndClient verifies the "happy path"
// of the simplified removal flow: exit.json shrinks, client.json shrinks
// in lockstep, and client.skirk decodes back to the new extras list.
func TestMailboxRemoveByLabelUpdatesExitAndClient(t *testing.T) {
	dir := writeTestKit(t, samplePrimary(), []skirk.MailboxConfig{
		sampleExtra("alpha", "alpha-refresh", "alpha-folder"),
		sampleExtra("beta", "beta-refresh", "beta-folder"),
	})
	args := []string{"--kit", dir, "--label", "alpha", "--restart-exit=false", "--yes"}
	if err := mailboxRemove(t.Context(), args); err != nil {
		t.Fatalf("mailbox remove failed: %v", err)
	}

	exitCfg, err := skirk.LoadConfig(filepath.Join(dir, "exit.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(exitCfg.Drive.ExtraMailboxes) != 1 || exitCfg.Drive.ExtraMailboxes[0].Label != "beta" {
		t.Fatalf("exit extras after remove unexpected: %+v", exitCfg.Drive.ExtraMailboxes)
	}
	clientCfg, err := skirk.LoadConfig(filepath.Join(dir, "client.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(clientCfg.Drive.ExtraMailboxes) != 1 || clientCfg.Drive.ExtraMailboxes[0].Label != "beta" {
		t.Fatalf("client extras after remove unexpected: %+v", clientCfg.Drive.ExtraMailboxes)
	}
	// client.skirk must round-trip to the same shape.
	textBytes, err := os.ReadFile(filepath.Join(dir, "client.skirk"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := skirk.LoadConfig(strings.TrimSpace(string(textBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Drive.ExtraMailboxes) != 1 || decoded.Drive.ExtraMailboxes[0].Label != "beta" {
		t.Fatalf("client.skirk did not regenerate cleanly: %+v", decoded.Drive.ExtraMailboxes)
	}
}

func TestMailboxRemoveRefusesEmptyExtras(t *testing.T) {
	dir := writeTestKit(t, samplePrimary(), nil)
	err := mailboxRemove(t.Context(), []string{"--kit", dir, "--label", "anything", "--restart-exit=false", "--yes"})
	if err == nil {
		t.Fatal("expected error when removing from kit with no extras")
	}
	if !strings.Contains(err.Error(), "no extra mailboxes") {
		t.Fatalf("expected friendly error about empty extras, got: %v", err)
	}
}

func TestMailboxPromoteSwapsPrimaryAndExtra(t *testing.T) {
	dir := writeTestKit(t, samplePrimary(), []skirk.MailboxConfig{
		sampleExtra("alpha", "alpha-refresh", "alpha-folder"),
		sampleExtra("beta", "beta-refresh", "beta-folder"),
	})
	if err := mailboxPromote(t.Context(), []string{"--kit", dir, "--label", "alpha", "--restart-exit=false", "--yes"}); err != nil {
		t.Fatalf("mailbox promote failed: %v", err)
	}
	exitCfg, err := skirk.LoadConfig(filepath.Join(dir, "exit.json"))
	if err != nil {
		t.Fatal(err)
	}
	if exitCfg.Auth.RefreshToken != "alpha-refresh" {
		t.Fatalf("primary auth not promoted, got refresh=%q", exitCfg.Auth.RefreshToken)
	}
	if exitCfg.Drive.FolderID != "alpha-folder" {
		t.Fatalf("primary folder not promoted, got %q", exitCfg.Drive.FolderID)
	}
	if len(exitCfg.Drive.ExtraMailboxes) != 2 {
		t.Fatalf("expected 2 extras after swap, got %d", len(exitCfg.Drive.ExtraMailboxes))
	}
	// The slot that used to hold alpha must now carry the old primary so
	// no lane drops between mailbox layouts.
	if exitCfg.Drive.ExtraMailboxes[0].Auth.RefreshToken != "primary-refresh" {
		t.Fatalf("old primary not demoted into alpha's slot, got %+v", exitCfg.Drive.ExtraMailboxes[0])
	}
	if exitCfg.Drive.ExtraMailboxes[1].Label != "beta" {
		t.Fatalf("beta moved unexpectedly: %+v", exitCfg.Drive.ExtraMailboxes[1])
	}
}

func TestMailboxRegenerateClientRecreatesText(t *testing.T) {
	dir := writeTestKit(t, samplePrimary(), []skirk.MailboxConfig{sampleExtra("alpha", "a", "f")})
	// Corrupt client.skirk on purpose to prove regenerate fixes it.
	if err := os.WriteFile(filepath.Join(dir, "client.skirk"), []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := mailboxRegenerateClient(t.Context(), []string{"--kit", dir}); err != nil {
		t.Fatal(err)
	}
	textBytes, err := os.ReadFile(filepath.Join(dir, "client.skirk"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(textBytes)), skirk.ConfigTextPrefix) {
		t.Fatalf("client.skirk does not start with skirk: prefix: %q", textBytes)
	}
	if _, err := skirk.LoadConfig(strings.TrimSpace(string(textBytes))); err != nil {
		t.Fatalf("regenerated client.skirk did not parse: %v", err)
	}
}

func TestSyncClientFromExitNoClientFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	// Only write exit.json; no client.json should be a clean no-op.
	cfg := skirk.Config{
		Secret: "test-secret",
		Auth:   skirk.AuthConfig{RefreshToken: "r"},
		Route:  skirk.RouteConfig{Mode: "direct", GoogleIP: "1.2.3.4", TimeoutSeconds: 240},
		Drive:  skirk.DriveConfig{FolderID: "f"},
		Tunnel: skirk.TunnelConfig{Listen: "127.0.0.1:0", Profile: "auto", ChunkSize: 4096, PollIntervalMS: 1000, Concurrency: 1, CleanupProcessed: true},
	}
	if err := writeJSONFile(filepath.Join(dir, "exit.json"), cfg); err != nil {
		t.Fatal(err)
	}
	updated, _, err := syncClientFromExit(dir, &cfg)
	if err != nil {
		t.Fatalf("syncClientFromExit returned err on missing client.json: %v", err)
	}
	if updated {
		t.Fatal("expected no client update when client.json is absent")
	}
}
