package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"skirk/internal/skirk"
	"skirk/internal/skirk/web"
)

// TestWebCommand_StartsAndServesStatus drives `skirk web` end-to-end against
// a temporary kit and verifies that /api/status reports the kit, the bearer
// token gets persisted with mode 0600, and a valid token unlocks the API.
func TestWebCommand_StartsAndServesStatus(t *testing.T) {
	kit := t.TempDir()

	// Write a minimal exit.json so the List call inside /api/status has
	// something to count.
	secret, err := skirk.RandomSecret()
	if err != nil {
		t.Fatalf("RandomSecret: %v", err)
	}
	cfg := skirk.Config{
		Role:   "exit",
		Secret: secret,
		Auth: skirk.AuthConfig{
			RefreshToken: "rt-primary",
			ClientID:     "cid",
			ClientSecret: "csec",
			TokenURL:     "https://oauth2.googleapis.com/token",
		},
		Drive: skirk.DriveConfig{FolderID: "primary-folder", Space: "drive"},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("seed config invalid: %v", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kit, "exit.json"), data, 0o600); err != nil {
		t.Fatalf("write exit.json: %v", err)
	}

	// Find a free port so the test doesn't collide with anything else.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() {
		done <- webCommand(ctx, []string{
			"--bind", "127.0.0.1",
			"--port", itoa(port),
			"--kit", kit,
			"--print-token=false",
		})
	}()

	// Wait for the server to come up.
	url := "http://127.0.0.1:" + itoa(port)
	if !waitFor(url+"/api/status", 3*time.Second) {
		cancel()
		<-done
		t.Fatal("server did not come up in time")
	}

	// Read the freshly-persisted token.
	tokenPath := filepath.Join(kit, "web-token")
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read web-token: %v", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		t.Fatal("web-token file is empty")
	}
	stat, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat web-token: %v", err)
	}
	if mode := stat.Mode().Perm(); mode != 0o600 {
		t.Errorf("expected web-token mode 0600, got %#o", mode)
	}

	// /api/status without token must be 401.
	resp, err := http.Get(url + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}

	// /api/status with the token should be 200 and report mailbox_count >= 1.
	req, _ := http.NewRequest(http.MethodGet, url+"/api/status", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/status w/ token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with token, got %d", resp.StatusCode)
	}
	var info web.StatusInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if info.MailboxCount < 1 {
		t.Errorf("expected mailbox_count >= 1, got %d", info.MailboxCount)
	}
	if info.KitDir == "" {
		t.Error("kit_dir should be reported")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("webCommand returned error: %v", err)
	}
}

// TestWebCommand_RejectsPublicNoAuth ensures the CLI refuses to start when
// --no-auth is paired with a non-loopback bind and no override flag.
func TestWebCommand_RejectsPublicNoAuth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	err := webCommand(ctx, []string{
		"--bind", "0.0.0.0",
		"--port", "0",
		"--no-auth",
		"--kit", t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error for --no-auth on 0.0.0.0 without --allow-public-no-auth")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "no-auth") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// waitFor probes url with HTTP GET until it returns any response (even 401)
// or the timeout elapses. Returns true on success.
func waitFor(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// TestCLIMailboxOps_ListEmptyKit verifies that pointing `skirk web` at a
// freshly-created (empty) kit directory does not surface a 500 error from
// the missing exit.json — the UI's "no mailboxes yet" state needs a clean
// empty slice instead. Regression test for the step 2-a fix.
func TestCLIMailboxOps_ListEmptyKit(t *testing.T) {
	dir := t.TempDir()
	ops := newCLIMailboxOps()
	got, err := ops.List(dir)
	if err != nil {
		t.Fatalf("unexpected error listing empty kit: %v", err)
	}
	if got == nil {
		t.Fatalf("expected non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 mailboxes from empty kit, got %d", len(got))
	}
	// And the type-asserted concrete return shouldn't be web.MailboxInfo's
	// zero-value singleton — we want a real empty slice serialised as [].
	var _ []web.MailboxInfo = got
}

func itoa(i int) string {
	// Tiny stdlib-free int-to-string so the test file doesn't need strconv.
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
