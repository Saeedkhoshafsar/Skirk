package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"skirk/internal/skirk"
)

// mailboxCommand is the top-level dispatcher for `skirk mailbox ...`. It
// implements an opinionated wrapper around the existing
// `drive.extra_mailboxes` field so operators can manage multi-mailbox
// striping without hand-editing exit.json/client.json, re-running
// `setup init`, or regenerating client.skirk manually.
//
// All subcommands operate on a "kit directory" (default: skirk-kit) which
// must contain at least exit.json. client.json/client.skirk are updated in
// lockstep when they are present so that client and exit always agree on
// mailbox ordering (lane routing is positional and deterministic).
func mailboxCommand(ctx context.Context, args []string) error {
	if len(args) < 1 {
		mailboxUsage()
		return fmt.Errorf("mailbox needs a subcommand: list, add, remove, promote, or regenerate-client")
	}
	switch args[0] {
	case "help", "--help", "-h":
		mailboxUsage()
		return nil
	case "list":
		return mailboxList(ctx, args[1:])
	case "add":
		return mailboxAdd(ctx, args[1:])
	case "remove", "rm", "delete":
		return mailboxRemove(ctx, args[1:])
	case "promote":
		return mailboxPromote(ctx, args[1:])
	case "regenerate-client", "regenerate", "regen":
		return mailboxRegenerateClient(ctx, args[1:])
	default:
		mailboxUsage()
		return fmt.Errorf("unknown mailbox subcommand %q", args[0])
	}
}

func mailboxUsage() {
	fmt.Println(`skirk mailbox commands:
  list      [--kit skirk-kit] [--json]
  add       [--kit skirk-kit] [--label NAME] [--oauth-mode easy|personal]
            [--oauth-client-file path] [--restart-exit=true|false]
            [--exit-service-name skirk-exit]
  remove    --kit skirk-kit (--label NAME | --index N) [--restart-exit=true|false]
            [--exit-service-name skirk-exit] [--yes]
  promote   --kit skirk-kit (--label NAME | --index N) [--restart-exit=true|false]
            [--exit-service-name skirk-exit] [--yes]
  regenerate-client [--kit skirk-kit]

The kit directory must contain exit.json. When client.json is present it is
kept in lockstep with exit.json and client.skirk is regenerated automatically.
Mailbox order is significant: lane i is served by mailbox i mod N on both
sides, so client and exit must list the same mailboxes in the same order.`)
}

// mailboxEntry is a small projection over DriveConfig + MailboxConfig used
// only for listing/menu rendering. It deliberately omits secrets so it can be
// safely printed or serialized to stdout.
type mailboxEntry struct {
	Index    int    `json:"index"`
	Role     string `json:"role"` // "primary" or "extra"
	Label    string `json:"label,omitempty"`
	FolderID string `json:"folder_id,omitempty"`
	Space    string `json:"space,omitempty"`
	HasAuth  bool   `json:"has_auth"`
}

func mailboxEntries(cfg *skirk.Config) []mailboxEntry {
	out := []mailboxEntry{{
		Index:    0,
		Role:     "primary",
		Label:    "primary",
		FolderID: cfg.Drive.FolderID,
		Space:    cfg.Drive.Space,
		HasAuth:  cfg.Auth.RefreshToken != "" || cfg.Auth.AccessToken != "" || cfg.Auth.TokenCommand != "",
	}}
	for i, mb := range cfg.Drive.ExtraMailboxes {
		label := strings.TrimSpace(mb.Label)
		if label == "" {
			label = fmt.Sprintf("alt%d", i+1)
		}
		out = append(out, mailboxEntry{
			Index:    i + 1,
			Role:     "extra",
			Label:    label,
			FolderID: mb.FolderID,
			Space:    mb.Space,
			HasAuth:  mb.Auth.RefreshToken != "" || mb.Auth.AccessToken != "" || mb.Auth.TokenCommand != "",
		})
	}
	return out
}

func mailboxList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mailbox list", flag.ExitOnError)
	kitDir := fs.String("kit", "skirk-kit", "kit directory containing exit.json")
	configPath := fs.String("config", "", "exit config path; defaults to <kit>/exit.json")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = ctx
	exitPath := resolveKitExitPath(*kitDir, *configPath)
	cfg, err := skirk.LoadConfig(exitPath)
	if err != nil {
		return err
	}
	entries := mailboxEntries(cfg)
	if *jsonOut {
		return printJSON(map[string]any{
			"result":    "ok",
			"kit":       *kitDir,
			"config":    exitPath,
			"mailboxes": entries,
			"total":     len(entries),
		})
	}
	fmt.Printf("Kit: %s\n", *kitDir)
	fmt.Printf("Config: %s\n", exitPath)
	fmt.Printf("Total mailboxes: %d (1 primary + %d extra)\n", len(entries), len(entries)-1)
	fmt.Println()
	fmt.Printf("%-6s %-8s %-20s %-20s %s\n", "INDEX", "ROLE", "LABEL", "FOLDER", "SPACE")
	for _, e := range entries {
		folder := e.FolderID
		if folder == "" {
			folder = "(appDataFolder)"
		}
		space := e.Space
		if space == "" {
			space = "drive_folder"
		}
		fmt.Printf("%-6d %-8s %-20s %-20s %s\n", e.Index, e.Role, e.Label, truncateLabel(folder, 20), space)
	}
	fmt.Println()
	fmt.Println("Lane assignment is deterministic: lane i is served by mailbox i mod N.")
	fmt.Println("Client and exit MUST list the same mailboxes in the same order.")
	return nil
}

func truncateLabel(value string, max int) string {
	if len(value) <= max {
		return value
	}
	if max <= 3 {
		return value[:max]
	}
	return value[:max-3] + "..."
}

func mailboxAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mailbox add", flag.ExitOnError)
	kitDir := fs.String("kit", "skirk-kit", "kit directory containing exit.json (and optionally client.json)")
	configPath := fs.String("config", "", "exit config path; defaults to <kit>/exit.json")
	label := fs.String("label", "", "human-readable label for the new mailbox (letters, digits, _-.); auto-generated when empty")
	oauthMode := fs.String("oauth-mode", "auto", "Google OAuth mode: auto, easy, or personal")
	oauthFlow := fs.String("oauth-flow", "auto", "Google OAuth flow: auto, device, or desktop")
	oauthClientFile := fs.String("oauth-client-file", "", "override Google OAuth client JSON for personal mode")
	oauthScopes := fs.String("oauth-scopes", defaultCustomOAuthScopes, "comma- or space-separated scopes used for Google OAuth login")
	restartExit := fs.Bool("restart-exit", runtime.GOOS == "linux", "restart the exit service after the kit is updated")
	exitServiceName := fs.String("exit-service-name", defaultServiceName, "systemd service name used with --restart-exit")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON instead of the human summary")
	if err := fs.Parse(args); err != nil {
		return err
	}

	exitPath := resolveKitExitPath(*kitDir, *configPath)
	exitCfg, err := skirk.LoadConfig(exitPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", exitPath, err)
	}
	if len(exitCfg.Drive.ExtraMailboxes) >= 15 {
		return fmt.Errorf("mailbox add refused: extra_mailboxes already has %d entries (max 15 + 1 primary = 16)", len(exitCfg.Drive.ExtraMailboxes))
	}

	resolvedLabel, err := resolveNewMailboxLabel(*label, exitCfg)
	if err != nil {
		return err
	}

	selectedOAuthMode, err := normalizeOAuthMode(*oauthMode)
	if err != nil {
		return err
	}
	selectedOAuthFlow, err := normalizeOAuthFlow(*oauthFlow)
	if err != nil {
		return err
	}
	var addReader *bufio.Reader
	getReader := func() *bufio.Reader {
		if addReader == nil {
			addReader = bufio.NewReader(os.Stdin)
		}
		return addReader
	}

	oauthClientFileValue := strings.TrimSpace(*oauthClientFile)
	if shouldPromptOAuthMode("", false, *jsonOut, selectedOAuthMode, oauthClientFileValue) {
		fmt.Println()
		fmt.Printf("Adding a new Drive mailbox to kit %q with label %q.\n", *kitDir, resolvedLabel)
		fmt.Println("Sign in with a DIFFERENT Google account than the existing mailboxes,")
		fmt.Println("otherwise you will not actually multiply your Drive API quota.")
		fmt.Println()
		selectedOAuthMode, oauthClientFileValue, err = promptSetupOAuthMode(ctx, getReader())
		if err != nil {
			return err
		}
	}
	if selectedOAuthMode == "auto" {
		// Non-interactive default: easy mode for parity with `setup init`.
		selectedOAuthMode = "easy"
	}
	if selectedOAuthMode == "personal" && oauthClientFileValue == "" && os.Getenv("SKIRK_OAUTH_CLIENT_ID") == "" && isInteractiveTerminal() && !*jsonOut {
		oauthClientFileValue, err = promptPersonalOAuthClientFile(ctx, getReader(), filepath.Join(*kitDir, "oauth-client.json"))
		if err != nil {
			return err
		}
	}

	oauthClient, oauthSource, oauthErr := resolveOAuthClientCredentialsForMode(oauthClientFileValue, selectedOAuthMode != "personal")
	if oauthErr != nil {
		return oauthErr
	}
	if oauthSource == "" {
		return errors.New("mailbox add could not resolve a Google OAuth client; pass --oauth-client-file, set SKIRK_OAUTH_CLIENT_ID, or use --oauth-mode personal in an interactive terminal")
	}
	selectedOAuthFlow, err = resolveSetupOAuthFlow(selectedOAuthMode, selectedOAuthFlow, oauthClient)
	if err != nil {
		return err
	}
	if selectedOAuthMode == "personal" && isInteractiveTerminal() && !*jsonOut {
		if err := confirmPersonalOAuthConsentReady(ctx, getReader()); err != nil {
			return err
		}
	}

	fmt.Printf("Google login for new mailbox %q via %s (%s OAuth flow).\n\n", resolvedLabel, oauthSource, selectedOAuthFlow)
	creds, err := runGoogleOAuth(ctx, oauthClient, *oauthScopes, selectedOAuthFlow, getReader())
	if err != nil {
		return fmt.Errorf("Google login for new mailbox failed: %w", err)
	}
	auth := creds.AuthConfig()

	// Refuse to add a mailbox whose refresh token matches one we already
	// hold. Striping across the same Google account multiplies Drive
	// metadata calls without unlocking new quota and just makes the
	// operator's life harder.
	if duplicateMailboxAuth(exitCfg, auth) {
		return errors.New("mailbox add refused: this Google account is already configured as the primary or an extra mailbox; sign in with a different Google account")
	}

	googleIP := exitCfg.Route.GoogleIP
	if strings.TrimSpace(googleIP) == "" {
		googleIP = "216.239.38.120"
	}
	driveCfg, folderID, err := setupDriveMailbox(ctx, auth, googleIP, exitCfg.SessionID)
	if err != nil {
		return fmt.Errorf("Drive mailbox setup for new mailbox failed: %w", err)
	}

	newEntry := skirk.MailboxConfig{
		Auth:     auth,
		FolderID: driveCfg.FolderID,
		Space:    driveCfg.Space,
		Label:    resolvedLabel,
	}
	exitCfg.Drive.ExtraMailboxes = append(exitCfg.Drive.ExtraMailboxes, newEntry)
	if err := exitCfg.Validate(); err != nil {
		return fmt.Errorf("kit refused new mailbox: %w", err)
	}
	if err := writeJSONFile(exitPath, *exitCfg); err != nil {
		return err
	}

	clientUpdated, clientTextPath, err := syncClientFromExit(*kitDir, exitCfg)
	if err != nil {
		return err
	}

	runtimeStatus := ""
	if *restartExit {
		if err := serviceCommand(ctx, []string{"restart", "--name", *exitServiceName}); err != nil {
			fmt.Fprintf(os.Stderr, "warn: exit service restart failed: %v\n", err)
		} else {
			runtimeStatus = *exitServiceName + " restarted"
		}
	}

	if *jsonOut {
		return printJSON(map[string]any{
			"result":           "ok",
			"action":           "added",
			"label":            resolvedLabel,
			"folder_id":        folderID,
			"google_account":   creds.Account,
			"total_mailboxes":  1 + len(exitCfg.Drive.ExtraMailboxes),
			"exit_config":      exitPath,
			"client_updated":   clientUpdated,
			"client_text_path": clientTextPath,
			"exit_runtime":     runtimeStatus,
		})
	}
	fmt.Println()
	fmt.Printf("Added mailbox %q (account %s) to %s\n", resolvedLabel, creds.Account, exitPath)
	if folderID != "" {
		fmt.Printf("Data folder: %s\n", folderID)
	}
	fmt.Printf("Total mailboxes now: %d (1 primary + %d extra)\n", 1+len(exitCfg.Drive.ExtraMailboxes), len(exitCfg.Drive.ExtraMailboxes))
	if clientUpdated {
		fmt.Printf("Updated client.json and client.skirk in %s\n", *kitDir)
		fmt.Println("Send the new client.skirk to every client device so they list the same mailboxes in the same order.")
	} else {
		fmt.Println("No client.json found in the kit; only exit.json was updated.")
	}
	if runtimeStatus != "" {
		fmt.Printf("Exit runtime: %s\n", runtimeStatus)
	} else if *restartExit {
		fmt.Println("Exit service was NOT restarted automatically. Run `skirk service restart --name " + *exitServiceName + "` after verifying the kit.")
	}
	return nil
}

func mailboxRemove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mailbox remove", flag.ExitOnError)
	kitDir := fs.String("kit", "skirk-kit", "kit directory containing exit.json")
	configPath := fs.String("config", "", "exit config path; defaults to <kit>/exit.json")
	label := fs.String("label", "", "label of the mailbox to remove (mutually exclusive with --index)")
	index := fs.Int("index", -1, "1-based index of the mailbox to remove (1 = first extra; 0 = primary is not removable)")
	restartExit := fs.Bool("restart-exit", runtime.GOOS == "linux", "restart the exit service after the kit is updated")
	exitServiceName := fs.String("exit-service-name", defaultServiceName, "systemd service name used with --restart-exit")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	exitPath := resolveKitExitPath(*kitDir, *configPath)
	exitCfg, err := skirk.LoadConfig(exitPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", exitPath, err)
	}
	if len(exitCfg.Drive.ExtraMailboxes) == 0 {
		return errors.New("mailbox remove refused: kit has no extra mailboxes; the primary mailbox cannot be removed")
	}

	target, err := resolveExtraMailboxIndex(exitCfg, *label, *index)
	if err != nil {
		return err
	}
	removed := exitCfg.Drive.ExtraMailboxes[target]

	if !*yes && isInteractiveTerminal() && !*jsonOut {
		reader := bufio.NewReader(os.Stdin)
		confirm, err := promptYesNo(ctx, reader,
			fmt.Sprintf("Remove extra mailbox %q (index %d). Removing reorders lane routing; old client.skirk profiles will stop working. Continue", labelOrDefault(removed.Label, target+1), target+1),
			false)
		if err != nil {
			return err
		}
		if !confirm {
			return errors.New("mailbox remove cancelled")
		}
	}

	exitCfg.Drive.ExtraMailboxes = append(exitCfg.Drive.ExtraMailboxes[:target], exitCfg.Drive.ExtraMailboxes[target+1:]...)
	if err := exitCfg.Validate(); err != nil {
		return fmt.Errorf("kit refused mailbox removal: %w", err)
	}
	if err := writeJSONFile(exitPath, *exitCfg); err != nil {
		return err
	}
	clientUpdated, clientTextPath, err := syncClientFromExit(*kitDir, exitCfg)
	if err != nil {
		return err
	}

	runtimeStatus := ""
	if *restartExit {
		if err := serviceCommand(ctx, []string{"restart", "--name", *exitServiceName}); err != nil {
			fmt.Fprintf(os.Stderr, "warn: exit service restart failed: %v\n", err)
		} else {
			runtimeStatus = *exitServiceName + " restarted"
		}
	}

	if *jsonOut {
		return printJSON(map[string]any{
			"result":           "ok",
			"action":           "removed",
			"label":            labelOrDefault(removed.Label, target+1),
			"removed_index":    target + 1,
			"total_mailboxes":  1 + len(exitCfg.Drive.ExtraMailboxes),
			"exit_config":      exitPath,
			"client_updated":   clientUpdated,
			"client_text_path": clientTextPath,
			"exit_runtime":     runtimeStatus,
		})
	}
	fmt.Printf("Removed mailbox %q (was extra index %d).\n", labelOrDefault(removed.Label, target+1), target+1)
	fmt.Printf("Total mailboxes now: %d (1 primary + %d extra)\n", 1+len(exitCfg.Drive.ExtraMailboxes), len(exitCfg.Drive.ExtraMailboxes))
	if clientUpdated {
		fmt.Printf("Updated client.json and client.skirk in %s\n", *kitDir)
		fmt.Println("Send the new client.skirk to every client device; old profiles route lanes to a mailbox that no longer exists.")
	}
	if runtimeStatus != "" {
		fmt.Printf("Exit runtime: %s\n", runtimeStatus)
	}
	fmt.Println()
	fmt.Println("Tip: stale Drive objects belonging to the removed account remain in its")
	fmt.Println("Drive. Sign in to that Google account and delete the skirk-mailbox-* folder")
	fmt.Println("manually, or run `skirk revoke --revoke-oauth` against a kit that still")
	fmt.Println("points at it before deleting.")
	return nil
}

// mailboxPromote swaps an extra mailbox with the primary so that the chosen
// mailbox becomes the new top-level Auth/Drive pair. The previous primary
// becomes an extra at the position the promoted mailbox vacated. Lane
// assignment is positional, so this rearranges lane 0 — clients with the old
// client.skirk will not route lane 0 to the new primary until they receive
// the regenerated client.skirk.
func mailboxPromote(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mailbox promote", flag.ExitOnError)
	kitDir := fs.String("kit", "skirk-kit", "kit directory containing exit.json")
	configPath := fs.String("config", "", "exit config path; defaults to <kit>/exit.json")
	label := fs.String("label", "", "label of the extra mailbox to promote (mutually exclusive with --index)")
	index := fs.Int("index", -1, "1-based index of the extra mailbox to promote")
	restartExit := fs.Bool("restart-exit", runtime.GOOS == "linux", "restart the exit service after the kit is updated")
	exitServiceName := fs.String("exit-service-name", defaultServiceName, "systemd service name used with --restart-exit")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	exitPath := resolveKitExitPath(*kitDir, *configPath)
	exitCfg, err := skirk.LoadConfig(exitPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", exitPath, err)
	}
	if len(exitCfg.Drive.ExtraMailboxes) == 0 {
		return errors.New("mailbox promote refused: kit has no extra mailboxes to promote")
	}
	target, err := resolveExtraMailboxIndex(exitCfg, *label, *index)
	if err != nil {
		return err
	}
	if !*yes && isInteractiveTerminal() && !*jsonOut {
		reader := bufio.NewReader(os.Stdin)
		confirm, err := promptYesNo(ctx, reader,
			fmt.Sprintf("Promote extra mailbox %q to primary. Lane 0 will switch Google accounts; old client.skirk profiles must be reissued. Continue",
				labelOrDefault(exitCfg.Drive.ExtraMailboxes[target].Label, target+1)),
			false)
		if err != nil {
			return err
		}
		if !confirm {
			return errors.New("mailbox promote cancelled")
		}
	}

	oldPrimary := skirk.MailboxConfig{
		Auth:     exitCfg.Auth,
		FolderID: exitCfg.Drive.FolderID,
		Space:    exitCfg.Drive.Space,
		Label:    "primary",
	}
	promoted := exitCfg.Drive.ExtraMailboxes[target]
	exitCfg.Auth = promoted.Auth
	exitCfg.Drive.FolderID = promoted.FolderID
	exitCfg.Drive.Space = promoted.Space
	exitCfg.Drive.ExtraMailboxes[target] = oldPrimary
	if err := exitCfg.Validate(); err != nil {
		return fmt.Errorf("kit refused mailbox promote: %w", err)
	}
	if err := writeJSONFile(exitPath, *exitCfg); err != nil {
		return err
	}
	clientUpdated, clientTextPath, err := syncClientFromExit(*kitDir, exitCfg)
	if err != nil {
		return err
	}
	runtimeStatus := ""
	if *restartExit {
		if err := serviceCommand(ctx, []string{"restart", "--name", *exitServiceName}); err != nil {
			fmt.Fprintf(os.Stderr, "warn: exit service restart failed: %v\n", err)
		} else {
			runtimeStatus = *exitServiceName + " restarted"
		}
	}
	if *jsonOut {
		return printJSON(map[string]any{
			"result":           "ok",
			"action":           "promoted",
			"promoted_label":   labelOrDefault(promoted.Label, target+1),
			"exit_config":      exitPath,
			"client_updated":   clientUpdated,
			"client_text_path": clientTextPath,
			"exit_runtime":     runtimeStatus,
		})
	}
	fmt.Printf("Promoted mailbox %q to primary.\n", labelOrDefault(promoted.Label, target+1))
	if clientUpdated {
		fmt.Printf("Updated client.json and client.skirk in %s\n", *kitDir)
	}
	if runtimeStatus != "" {
		fmt.Printf("Exit runtime: %s\n", runtimeStatus)
	}
	return nil
}

// mailboxRegenerateClient is the explicit equivalent of the
// "regenerate client.skirk from client.json" step in the manual workflow.
// It is exposed as its own subcommand so operators can recover from a
// scenario where exit.json and client.json are in sync but client.skirk is
// stale (for example after hand-editing labels).
func mailboxRegenerateClient(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mailbox regenerate-client", flag.ExitOnError)
	kitDir := fs.String("kit", "skirk-kit", "kit directory containing client.json")
	jsonOut := fs.Bool("json", false, "print machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = ctx
	kit := strings.TrimSpace(*kitDir)
	if kit == "" {
		kit = "skirk-kit"
	}
	clientPath := filepath.Join(kit, "client.json")
	cfg, err := skirk.LoadConfig(clientPath)
	if err != nil {
		return fmt.Errorf("load %s: %w", clientPath, err)
	}
	clientText, err := skirk.EncodeConfigText(cfg)
	if err != nil {
		return err
	}
	clientTextPath := filepath.Join(kit, "client.skirk")
	if err := writeTextFile(clientTextPath, clientText+"\n"); err != nil {
		return err
	}
	listen := strings.TrimSpace(cfg.Tunnel.Listen)
	if listen == "" {
		listen = "127.0.0.1:18080"
	}
	clientCommand := fmt.Sprintf("skirk serve-client --config '%s' --listen %s\n", clientText, listen)
	clientCommandPath := filepath.Join(kit, "client-command.txt")
	if err := writeTextFile(clientCommandPath, clientCommand); err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(map[string]any{
			"result":              "ok",
			"client_config":       clientPath,
			"client_text":         clientTextPath,
			"client_command_file": clientCommandPath,
			"total_mailboxes":     1 + len(cfg.Drive.ExtraMailboxes),
		})
	}
	fmt.Printf("Regenerated %s from %s\n", clientTextPath, clientPath)
	fmt.Printf("Total mailboxes: %d (1 primary + %d extra)\n", 1+len(cfg.Drive.ExtraMailboxes), len(cfg.Drive.ExtraMailboxes))
	return nil
}

// --- helpers ---------------------------------------------------------------

func resolveKitExitPath(kitDir, configPath string) string {
	configPath = strings.TrimSpace(configPath)
	if configPath != "" {
		return configPath
	}
	kit := strings.TrimSpace(kitDir)
	if kit == "" {
		kit = "skirk-kit"
	}
	return filepath.Join(kit, "exit.json")
}

// syncClientFromExit copies the Auth/Drive blocks from the (already-updated)
// exit config into client.json, regenerates client.skirk, and rewrites
// client-command.txt. It is a no-op (returning false) when client.json is
// absent — multi-mailbox striping is just as valid for an exit that doesn't
// ship a paired client profile.
func syncClientFromExit(kitDir string, exitCfg *skirk.Config) (bool, string, error) {
	kit := strings.TrimSpace(kitDir)
	if kit == "" {
		kit = "skirk-kit"
	}
	clientPath := filepath.Join(kit, "client.json")
	info, err := os.Stat(clientPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		return false, "", err
	}
	if info.IsDir() {
		return false, "", fmt.Errorf("%s is a directory, not a client config file", clientPath)
	}
	clientCfg, err := skirk.LoadConfig(clientPath)
	if err != nil {
		return false, "", fmt.Errorf("load %s: %w", clientPath, err)
	}
	if clientCfg.SessionID != exitCfg.SessionID || clientCfg.Secret != exitCfg.Secret {
		return false, "", fmt.Errorf("client config %s does not match exit session/secret; refusing to rewrite", clientPath)
	}
	clientCfg.Auth = exitCfg.Auth
	clientCfg.Drive = exitCfg.Drive
	if err := clientCfg.Validate(); err != nil {
		return false, "", fmt.Errorf("client config refused multi-mailbox update: %w", err)
	}
	if err := writeJSONFile(clientPath, *clientCfg); err != nil {
		return false, "", err
	}
	clientText, err := skirk.EncodeConfigText(clientCfg)
	if err != nil {
		return false, "", err
	}
	clientTextPath := filepath.Join(kit, "client.skirk")
	if err := writeTextFile(clientTextPath, clientText+"\n"); err != nil {
		return false, "", err
	}
	listen := strings.TrimSpace(clientCfg.Tunnel.Listen)
	if listen == "" {
		listen = "127.0.0.1:18080"
	}
	clientCommand := fmt.Sprintf("skirk serve-client --config '%s' --listen %s\n", clientText, listen)
	clientCommandPath := filepath.Join(kit, "client-command.txt")
	if err := writeTextFile(clientCommandPath, clientCommand); err != nil {
		return false, "", err
	}
	return true, clientTextPath, nil
}

func duplicateMailboxAuth(cfg *skirk.Config, candidate skirk.AuthConfig) bool {
	if candidate.RefreshToken == "" {
		// Only refresh tokens give a meaningful uniqueness check. Static
		// access tokens and token_command-driven auth blocks are
		// operator-managed; let them through.
		return false
	}
	if cfg.Auth.RefreshToken == candidate.RefreshToken {
		return true
	}
	for _, mb := range cfg.Drive.ExtraMailboxes {
		if mb.Auth.RefreshToken == candidate.RefreshToken {
			return true
		}
	}
	return false
}

func resolveExtraMailboxIndex(cfg *skirk.Config, label string, index int) (int, error) {
	label = strings.TrimSpace(label)
	if label != "" && index >= 0 {
		return 0, errors.New("specify either --label or --index, not both")
	}
	if label == "" && index < 0 {
		return 0, errors.New("specify --label or --index to identify the extra mailbox")
	}
	if label != "" {
		if strings.EqualFold(label, "primary") {
			return 0, errors.New("the primary mailbox cannot be addressed by extra mailbox operations; use `mailbox promote` to change which mailbox is primary")
		}
		matches := []int{}
		for i, mb := range cfg.Drive.ExtraMailboxes {
			if strings.EqualFold(strings.TrimSpace(mb.Label), label) {
				matches = append(matches, i)
			}
		}
		if len(matches) == 0 {
			return 0, fmt.Errorf("no extra mailbox with label %q (use `skirk mailbox list` to see labels)", label)
		}
		if len(matches) > 1 {
			return 0, fmt.Errorf("label %q matches %d extra mailboxes; use --index to disambiguate", label, len(matches))
		}
		return matches[0], nil
	}
	if index == 0 {
		return 0, errors.New("--index 0 refers to the primary mailbox, which cannot be removed; use `mailbox promote` instead")
	}
	if index < 1 || index > len(cfg.Drive.ExtraMailboxes) {
		return 0, fmt.Errorf("--index %d out of range; kit has %d extra mailboxes (use 1..%d)", index, len(cfg.Drive.ExtraMailboxes), len(cfg.Drive.ExtraMailboxes))
	}
	return index - 1, nil
}

func resolveNewMailboxLabel(requested string, cfg *skirk.Config) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return autoMailboxLabel(cfg), nil
	}
	if strings.EqualFold(requested, "primary") {
		return "", errors.New("label \"primary\" is reserved for the top-level mailbox")
	}
	if !looksLikeSafeLabel(requested) {
		return "", fmt.Errorf("label %q is invalid; use 3-96 chars of letters, digits, underscore, hyphen, or dot", requested)
	}
	existing := collectMailboxLabels(cfg)
	if _, taken := existing[strings.ToLower(requested)]; taken {
		return "", fmt.Errorf("label %q is already used in this kit", requested)
	}
	return requested, nil
}

// autoMailboxLabel picks the lowest "altN" name not already used in the
// kit. Operators can override this with --label or by editing the JSON
// later. We keep the naming convention from the README example
// (alt1/alt2/alt3) so existing docs stay valid.
func autoMailboxLabel(cfg *skirk.Config) string {
	used := collectMailboxLabels(cfg)
	for n := 1; n < 1000; n++ {
		candidate := "alt" + strconv.Itoa(n)
		if _, taken := used[strings.ToLower(candidate)]; !taken {
			return candidate
		}
	}
	return "alt"
}

func collectMailboxLabels(cfg *skirk.Config) map[string]struct{} {
	out := map[string]struct{}{"primary": {}}
	for _, mb := range cfg.Drive.ExtraMailboxes {
		label := strings.TrimSpace(mb.Label)
		if label == "" {
			continue
		}
		out[strings.ToLower(label)] = struct{}{}
	}
	return out
}

func looksLikeSafeLabel(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 3 || len(value) > 96 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
			continue
		default:
			return false
		}
	}
	return true
}

func labelOrDefault(label string, idx int) string {
	label = strings.TrimSpace(label)
	if label != "" {
		return label
	}
	return fmt.Sprintf("alt%d", idx)
}

// --- interactive menu wrapper ---------------------------------------------

// mailboxMenu drives `skirk` -> "Manage Drive mailboxes" from the operator
// menu. It is a thin wrapper around mailboxList/mailboxAdd/mailboxRemove/
// mailboxPromote that asks for the kit directory once and then loops until
// the operator picks "Back".
func mailboxMenu(ctx context.Context, reader *bufio.Reader) error {
	kitDir, err := prompt(ctx, reader, "Kit directory", "skirk-kit")
	if err != nil {
		return err
	}
	kitDir = strings.TrimSpace(kitDir)
	if kitDir == "" {
		kitDir = "skirk-kit"
	}
	exitPath := filepath.Join(kitDir, "exit.json")
	if _, err := os.Stat(exitPath); err != nil {
		return fmt.Errorf("kit not found: %s does not exist; run `skirk setup init` first or pass a different kit directory", exitPath)
	}
	for {
		if err := mailboxList(ctx, []string{"--kit", kitDir}); err != nil {
			return err
		}
		fmt.Println()
		fmt.Println("1. Add a new mailbox (sign in with another Google account)")
		fmt.Println("2. Remove an extra mailbox")
		fmt.Println("3. Promote an extra mailbox to primary")
		fmt.Println("4. Regenerate client.skirk from client.json")
		fmt.Println("0. Back")
		choice, err := prompt(ctx, reader, "Mailbox action", "1")
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "0", "b", "back", "q", "quit", "exit":
			return nil
		case "1", "add":
			if err := mailboxAddInteractive(ctx, reader, kitDir); err != nil {
				fmt.Fprintf(os.Stderr, "mailbox add: %v\n", err)
			}
		case "2", "remove", "rm", "delete":
			if err := mailboxRemoveInteractive(ctx, reader, kitDir); err != nil {
				fmt.Fprintf(os.Stderr, "mailbox remove: %v\n", err)
			}
		case "3", "promote":
			if err := mailboxPromoteInteractive(ctx, reader, kitDir); err != nil {
				fmt.Fprintf(os.Stderr, "mailbox promote: %v\n", err)
			}
		case "4", "regen", "regenerate":
			if err := mailboxRegenerateClient(ctx, []string{"--kit", kitDir}); err != nil {
				fmt.Fprintf(os.Stderr, "mailbox regenerate-client: %v\n", err)
			}
		default:
			fmt.Println("Unknown selection")
		}
	}
}

func mailboxAddInteractive(ctx context.Context, reader *bufio.Reader, kitDir string) error {
	label, err := prompt(ctx, reader, "Label for the new mailbox (blank = auto altN)", "")
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("OAuth source for the new mailbox:")
	fmt.Println("1. Easy Skirk OAuth client (quick, shared Drive API quota)")
	fmt.Println("2. Personal Google OAuth project (recommended for sustained use)")
	mode, err := prompt(ctx, reader, "OAuth mode", "1")
	if err != nil {
		return err
	}
	oauthMode := "easy"
	if strings.TrimSpace(mode) == "2" {
		oauthMode = "personal"
	}
	restart := runtime.GOOS == "linux"
	if runtime.GOOS == "linux" {
		restart, err = promptYesNo(ctx, reader, "Restart exit service after adding the mailbox", true)
		if err != nil {
			return err
		}
	}
	args := []string{"--kit", kitDir, "--oauth-mode", oauthMode}
	if strings.TrimSpace(label) != "" {
		args = append(args, "--label", strings.TrimSpace(label))
	}
	if !restart {
		args = append(args, "--restart-exit=false")
	}
	return mailboxAdd(ctx, args)
}

func mailboxRemoveInteractive(ctx context.Context, reader *bufio.Reader, kitDir string) error {
	cfg, err := skirk.LoadConfig(filepath.Join(kitDir, "exit.json"))
	if err != nil {
		return err
	}
	if len(cfg.Drive.ExtraMailboxes) == 0 {
		fmt.Println("This kit has no extra mailboxes to remove. The primary mailbox cannot be removed.")
		return nil
	}
	labels := availableExtraLabels(cfg)
	fmt.Printf("Extra mailbox labels: %s\n", strings.Join(labels, ", "))
	identifier, err := prompt(ctx, reader, "Label OR 1-based index to remove", "")
	if err != nil {
		return err
	}
	args := []string{"--kit", kitDir}
	if idx, convErr := strconv.Atoi(strings.TrimSpace(identifier)); convErr == nil {
		args = append(args, "--index", strconv.Itoa(idx))
	} else if strings.TrimSpace(identifier) != "" {
		args = append(args, "--label", strings.TrimSpace(identifier))
	} else {
		return errors.New("mailbox remove cancelled: empty identifier")
	}
	restart := runtime.GOOS == "linux"
	if runtime.GOOS == "linux" {
		restart, err = promptYesNo(ctx, reader, "Restart exit service after removal", true)
		if err != nil {
			return err
		}
	}
	if !restart {
		args = append(args, "--restart-exit=false")
	}
	return mailboxRemove(ctx, args)
}

func mailboxPromoteInteractive(ctx context.Context, reader *bufio.Reader, kitDir string) error {
	cfg, err := skirk.LoadConfig(filepath.Join(kitDir, "exit.json"))
	if err != nil {
		return err
	}
	if len(cfg.Drive.ExtraMailboxes) == 0 {
		fmt.Println("This kit has no extra mailboxes to promote.")
		return nil
	}
	labels := availableExtraLabels(cfg)
	fmt.Printf("Extra mailbox labels: %s\n", strings.Join(labels, ", "))
	identifier, err := prompt(ctx, reader, "Label OR 1-based index to promote to primary", "")
	if err != nil {
		return err
	}
	args := []string{"--kit", kitDir}
	if idx, convErr := strconv.Atoi(strings.TrimSpace(identifier)); convErr == nil {
		args = append(args, "--index", strconv.Itoa(idx))
	} else if strings.TrimSpace(identifier) != "" {
		args = append(args, "--label", strings.TrimSpace(identifier))
	} else {
		return errors.New("mailbox promote cancelled: empty identifier")
	}
	restart := runtime.GOOS == "linux"
	if runtime.GOOS == "linux" {
		restart, err = promptYesNo(ctx, reader, "Restart exit service after promote", true)
		if err != nil {
			return err
		}
	}
	if !restart {
		args = append(args, "--restart-exit=false")
	}
	return mailboxPromote(ctx, args)
}

func availableExtraLabels(cfg *skirk.Config) []string {
	out := make([]string, 0, len(cfg.Drive.ExtraMailboxes))
	for i, mb := range cfg.Drive.ExtraMailboxes {
		out = append(out, fmt.Sprintf("%d=%s", i+1, labelOrDefault(mb.Label, i+1)))
	}
	sort.Strings(out)
	return out
}
