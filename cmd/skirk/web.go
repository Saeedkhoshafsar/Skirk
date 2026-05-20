package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"skirk/internal/skirk"
	"skirk/internal/skirk/web"
)

// webCommand handles the `skirk web` subcommand. It boots the Web UI HTTP
// server with bind/auth flags, persists or generates a bearer token, and
// blocks until ctx is cancelled.
func webCommand(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	port := fs.Int("port", 8787, "HTTP port to listen on")
	bind := fs.String("bind", "127.0.0.1", "address to bind the HTTP server to")
	addr := fs.String("addr", "", "full host:port to bind (overrides --bind/--port when set)")
	kitDir := fs.String("kit", "skirk-kit", "kit directory containing exit.json / client.json")
	tokenFlag := fs.String("token", "", "bearer token to require (empty = load or generate <kit>/web-token)")
	tokenFile := fs.String("token-file", "", "path to read/write the bearer token (default: <kit>/web-token)")
	tlsCert := fs.String("tls-cert", "", "optional TLS certificate path (enables HTTPS when paired with --tls-key)")
	tlsKey := fs.String("tls-key", "", "optional TLS private key path")
	noAuth := fs.Bool("no-auth", false, "disable authentication (loopback bind only unless --allow-public-no-auth is set)")
	allowPublicNoAuth := fs.Bool("allow-public-no-auth", false, "allow --no-auth on non-loopback binds (DANGEROUS)")
	printToken := fs.Bool("print-token", true, "print the active bearer token to stdout on startup")
	if err := fs.Parse(args); err != nil {
		return err
	}

	bindAddr := strings.TrimSpace(*addr)
	if bindAddr == "" {
		bindAddr = fmt.Sprintf("%s:%d", *bind, *port)
	}

	resolvedKit := strings.TrimSpace(*kitDir)
	if resolvedKit == "" {
		resolvedKit = "skirk-kit"
	}
	if abs, err := filepath.Abs(resolvedKit); err == nil {
		resolvedKit = abs
	}

	// Resolve the bearer token: explicit --token > --token-file > kit default.
	resolvedTokenPath := strings.TrimSpace(*tokenFile)
	if resolvedTokenPath == "" {
		resolvedTokenPath = filepath.Join(resolvedKit, "web-token")
	}

	var (
		token   string
		created bool
		err     error
	)
	switch {
	case *noAuth:
		// No token needed.
	case strings.TrimSpace(*tokenFlag) != "":
		token = strings.TrimSpace(*tokenFlag)
	default:
		token, created, err = web.LoadOrCreateToken(resolvedTokenPath)
		if err != nil {
			return fmt.Errorf("resolve web token: %w", err)
		}
	}

	cfg := web.Config{
		Addr:              bindAddr,
		KitDir:            resolvedKit,
		Auth:              web.AuthConfig{Token: token, NoAuth: *noAuth},
		AllowPublicNoAuth: *allowPublicNoAuth,
		TLSCertFile:       strings.TrimSpace(*tlsCert),
		TLSKeyFile:        strings.TrimSpace(*tlsKey),
		Ops:               newCLIMailboxOps(),
		Version: web.VersionInfo{
			Version: version,
			Commit:  commit,
			Date:    date,
		},
	}

	server, err := web.New(cfg)
	if err != nil {
		return err
	}

	// Print startup banner and token (if applicable) AFTER successful
	// constructor validation. This avoids leaking a freshly-generated
	// token on stdout when the config is bogus.
	scheme := "http"
	if cfg.TLSCertFile != "" {
		scheme = "https"
	}
	fmt.Printf("Skirk Web UI starting on %s://%s\n", scheme, bindAddr)
	fmt.Printf("  kit:           %s\n", resolvedKit)
	if *noAuth {
		fmt.Println("  auth:          DISABLED (--no-auth)")
	} else {
		fmt.Printf("  auth:          bearer-token (file=%s%s)\n",
			resolvedTokenPath, ternary(created, " [newly generated]", ""))
		if *printToken {
			fmt.Printf("  token:         %s\n", token)
			fmt.Println("  (export as SKIRK_WEB_TOKEN or pass `Authorization: Bearer <token>` for API calls)")
		} else {
			fmt.Println("  token:         (suppressed; --print-token=false)")
		}
	}
	fmt.Println("Press Ctrl+C to stop.")

	return server.Serve(ctx)
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// cliMailboxOps is the cmd/skirk-side implementation of web.MailboxOps. It
// reads exit.json directly for read-only endpoints; mutating endpoints
// currently return ErrNotImplemented because the existing CLI helpers expect
// an interactive terminal. The full implementation lands in PR #1 step 2.
type cliMailboxOps struct{}

func newCLIMailboxOps() web.MailboxOps {
	return &cliMailboxOps{}
}

func (cliMailboxOps) List(kitDir string) ([]web.MailboxInfo, error) {
	exitPath := resolveKitExitPath(kitDir, "")
	// A brand-new kit directory does not have an exit.json yet (the operator
	// may have just pointed `skirk web` at an empty folder). Returning a
	// clean empty list lets the UI render its "no mailboxes yet" hint
	// instead of bubbling up a 500 from a missing-file error.
	if _, statErr := os.Stat(exitPath); errors.Is(statErr, os.ErrNotExist) {
		return []web.MailboxInfo{}, nil
	}
	cfg, err := skirk.LoadConfig(exitPath)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", exitPath, err)
	}
	return web.MailboxOpsFromConfig(cfg), nil
}

func (cliMailboxOps) Add(ctx context.Context, kitDir string, req web.AddMailboxRequest) (web.AddMailboxResult, error) {
	// The CLI `mailbox add` flow is interactive (it launches a Google OAuth
	// device-code or desktop flow). PR #1 ships the API surface but the
	// actual orchestration lives in PR #1 step 2.
	return web.AddMailboxResult{}, errNotImplementedWeb
}

func (cliMailboxOps) Remove(ctx context.Context, kitDir, label string) (web.RemoveMailboxResult, error) {
	exitPath := resolveKitExitPath(kitDir, "")
	cfg, err := skirk.LoadConfig(exitPath)
	if err != nil {
		return web.RemoveMailboxResult{}, fmt.Errorf("load %s: %w", exitPath, err)
	}
	if len(cfg.Drive.ExtraMailboxes) == 0 {
		return web.RemoveMailboxResult{}, errors.New("kit has no extra mailboxes; the primary mailbox cannot be removed")
	}
	target, err := findExtraByLabel(cfg, label)
	if err != nil {
		return web.RemoveMailboxResult{}, err
	}
	removed := cfg.Drive.ExtraMailboxes[target]
	cfg.Drive.ExtraMailboxes = append(cfg.Drive.ExtraMailboxes[:target], cfg.Drive.ExtraMailboxes[target+1:]...)
	if err := cfg.Validate(); err != nil {
		return web.RemoveMailboxResult{}, fmt.Errorf("kit refused mailbox removal: %w", err)
	}
	if err := writeJSONFile(exitPath, *cfg); err != nil {
		return web.RemoveMailboxResult{}, err
	}
	clientUpdated, _, err := syncClientFromExit(kitDir, cfg)
	if err != nil {
		return web.RemoveMailboxResult{}, err
	}
	return web.RemoveMailboxResult{
		Label:          web.LabelOrDefault(removed.Label, target+2),
		RemovedIndex:   target + 1,
		TotalMailboxes: 1 + len(cfg.Drive.ExtraMailboxes),
		ClientUpdated:  clientUpdated,
	}, nil
}

func (cliMailboxOps) Promote(ctx context.Context, kitDir, label string) (web.PromoteMailboxResult, error) {
	exitPath := resolveKitExitPath(kitDir, "")
	cfg, err := skirk.LoadConfig(exitPath)
	if err != nil {
		return web.PromoteMailboxResult{}, fmt.Errorf("load %s: %w", exitPath, err)
	}
	if len(cfg.Drive.ExtraMailboxes) == 0 {
		return web.PromoteMailboxResult{}, errors.New("kit has no extra mailboxes to promote")
	}
	target, err := findExtraByLabel(cfg, label)
	if err != nil {
		return web.PromoteMailboxResult{}, err
	}
	oldPrimary := skirk.MailboxConfig{
		Auth:     cfg.Auth,
		FolderID: cfg.Drive.FolderID,
		Space:    cfg.Drive.Space,
		Label:    "primary",
	}
	promoted := cfg.Drive.ExtraMailboxes[target]
	cfg.Auth = promoted.Auth
	cfg.Drive.FolderID = promoted.FolderID
	cfg.Drive.Space = promoted.Space
	cfg.Drive.ExtraMailboxes[target] = oldPrimary
	if err := cfg.Validate(); err != nil {
		return web.PromoteMailboxResult{}, fmt.Errorf("kit refused mailbox promote: %w", err)
	}
	if err := writeJSONFile(exitPath, *cfg); err != nil {
		return web.PromoteMailboxResult{}, err
	}
	clientUpdated, _, err := syncClientFromExit(kitDir, cfg)
	if err != nil {
		return web.PromoteMailboxResult{}, err
	}
	return web.PromoteMailboxResult{
		PromotedLabel: web.LabelOrDefault(promoted.Label, target+2),
		ClientUpdated: clientUpdated,
	}, nil
}

func (cliMailboxOps) Rename(ctx context.Context, kitDir, oldLabel, newLabel string) error {
	if !web.LooksLikeSafeLabel(newLabel) {
		return errors.New("new label fails label validation")
	}
	exitPath := resolveKitExitPath(kitDir, "")
	cfg, err := skirk.LoadConfig(exitPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", exitPath, err)
	}
	// Reject collisions against the *other* mailboxes' labels.
	for i, mb := range cfg.Drive.ExtraMailboxes {
		if mb.Label == newLabel && mb.Label != oldLabel {
			return fmt.Errorf("label %q is already in use by extra mailbox at index %d", newLabel, i+1)
		}
	}
	// Locate the target. The primary mailbox is identified by the synthetic
	// label "primary" produced by web.LabelOrDefault.
	if oldLabel == "primary" {
		// No explicit primary label field exists today; treat rename as
		// a no-op so the API stays idempotent without lying.
		return errors.New("renaming the primary mailbox is not supported yet")
	}
	target, err := findExtraByLabel(cfg, oldLabel)
	if err != nil {
		return err
	}
	cfg.Drive.ExtraMailboxes[target].Label = newLabel
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("kit refused label change: %w", err)
	}
	if err := writeJSONFile(exitPath, *cfg); err != nil {
		return err
	}
	_, _, err = syncClientFromExit(kitDir, cfg)
	return err
}

func (cliMailboxOps) ClientProfile(kitDir string) (string, error) {
	clientPath := filepath.Join(kitDir, "client.json")
	info, err := os.Stat(clientPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory, not a client config file", clientPath)
	}
	cfg, err := skirk.LoadConfig(clientPath)
	if err != nil {
		return "", err
	}
	return skirk.EncodeConfigText(cfg)
}

// findExtraByLabel walks cfg.Drive.ExtraMailboxes returning the 0-based index
// whose label (or auto-generated label) matches. It mirrors the behaviour of
// resolveExtraMailboxIndex but operates purely on labels — the web API never
// exposes raw indices to the client.
func findExtraByLabel(cfg *skirk.Config, label string) (int, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return -1, errors.New("label is required")
	}
	for i, mb := range cfg.Drive.ExtraMailboxes {
		auto := web.LabelOrDefault(mb.Label, i+2)
		if mb.Label == label || auto == label {
			return i, nil
		}
	}
	return -1, fmt.Errorf("no mailbox with label %q", label)
}

// errNotImplementedWeb is the cmd/skirk-side mirror of the web package's
// internal errNotImplemented; the web layer detects this string to map it to
// HTTP 501.
var errNotImplementedWeb = errors.New("OAuth-driven mailbox add over HTTP is not implemented yet (PR #1 step 2); use `skirk mailbox add` for now")
