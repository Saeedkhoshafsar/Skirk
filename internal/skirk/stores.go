package skirk

import (
	"context"
	"log"
)

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
