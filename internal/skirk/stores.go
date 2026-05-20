package skirk

import (
	"context"
	"fmt"
	"log"
)

// StoresFromConfig builds the primary DriveStore for a config. Multi-mailbox
// pools are constructed by MailboxPoolFromConfig instead; this helper kept
// its single-store return type so existing single-mailbox callers stay
// source-compatible.
func StoresFromConfig(ctx context.Context, cfg *Config) (*DriveStore, error) {
	tokenSource := NewAccessTokenSource(cfg.Auth, cfg.Route)
	tokenSource.Logger = log.Default()
	if _, err := tokenSource.Token(ctx); err != nil {
		// The token source owns a cancellable context; cancel it on the
		// failure path so the cancel goroutine attached to it does not
		// leak when callers receive a nil DriveStore.
		tokenSource.Close()
		return nil, err
	}
	httpClient := NewGoogleHTTPClient(cfg.Route)
	drive := NewDriveStoreWithTokenSource(httpClient, tokenSource, cfg.Drive)
	drive.Logger = log.Default()
	return drive, nil
}

// BlobStoreFromConfig returns the appropriate BlobStore for the supplied
// config: a single DriveStore when only the primary mailbox is configured,
// or a MailboxPool that stripes traffic when ExtraMailboxes is set.
//
// The closer return value lets callers run a single defer that cleans up
// every underlying token source on shutdown, regardless of whether the
// pool was actually used.
//
// Note: the multi-mailbox path opens one OAuth token source per mailbox.
// Each token source is initialized eagerly (just like the single-mailbox
// path) so a misconfigured extra mailbox fails fast at startup rather
// than silently dropping every Nth lane's traffic later.
func BlobStoreFromConfig(ctx context.Context, cfg *Config) (BlobStore, func(), error) {
	store, _, closer, err := BlobStoreWithPrimaryFromConfig(ctx, cfg)
	return store, closer, err
}

// BlobStoreWithPrimaryFromConfig is the canonical multi-mailbox factory:
// it always returns the primary DriveStore alongside the BlobStore that
// callers should feed into NewTunnel. When ExtraMailboxes is empty, the
// returned BlobStore IS the primary DriveStore (single allocation); when
// ExtraMailboxes is set, the BlobStore is a MailboxPool whose mailbox 0
// is that same primary.
//
// Exposing the primary lets callers run admin-only operations
// (DriveCleanup, QuotaSnapshot, ResetTelemetry) that the BlobStore
// interface intentionally does not surface, without forking two separate
// token sources for the same primary mailbox.
//
// The closer cleans up every underlying token source (primary + extras)
// in a single call; callers should defer it on every exit path.
func BlobStoreWithPrimaryFromConfig(ctx context.Context, cfg *Config) (BlobStore, *DriveStore, func(), error) {
	primary, err := StoresFromConfig(ctx, cfg)
	if err != nil {
		return nil, nil, func() {}, err
	}
	if len(cfg.Drive.ExtraMailboxes) == 0 {
		return primary, primary, primary.Close, nil
	}
	extras := make([]*DriveStore, 0, len(cfg.Drive.ExtraMailboxes))
	cleanup := func() {
		primary.Close()
		for _, extra := range extras {
			if extra != nil {
				extra.Close()
			}
		}
	}
	for i, mb := range cfg.Drive.ExtraMailboxes {
		ts := NewAccessTokenSource(mb.Auth, cfg.Route)
		ts.Logger = log.Default()
		if _, err := ts.Token(ctx); err != nil {
			ts.Close()
			cleanup()
			return nil, nil, func() {}, fmt.Errorf("extra mailbox[%d] token init failed: %w", i, err)
		}
		// Reuse the primary HTTP client per mailbox; it is route-scoped, not
		// auth-scoped, and reusing it preserves the warmed connection pool
		// across mailboxes that share the same Google-fronted route.
		httpClient := NewGoogleHTTPClient(cfg.Route)
		store := NewDriveStoreWithTokenSource(httpClient, ts, DriveConfig{
			FolderID: mb.FolderID,
			Space:    mb.Space,
		})
		store.Logger = log.Default()
		extras = append(extras, store)
	}
	pool, err := NewMailboxPool(primary, extras...)
	if err != nil {
		cleanup()
		return nil, nil, func() {}, err
	}
	pool.SetLogger(log.Default())
	log.Default().Printf("mailbox pool active mailboxes=%d primary_folder=%s extras=%d", pool.Size(), cfg.Drive.FolderID, len(extras))
	return pool, primary, cleanup, nil
}
