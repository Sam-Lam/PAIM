package services

import (
	"context"
	"errors"

	"github.com/Sam-Lam/PAIM/internal/library"
	"github.com/Sam-Lam/PAIM/internal/repo"
	"github.com/Sam-Lam/PAIM/internal/thumbs"
)

// thumbAssetResolver adapts the AssetRepo and the portable-library path resolver
// to thumbs.AssetResolver, so the thumbnail HTTP handler can map an asset ID to
// its absolute source path and quick hash without importing repo/library itself.
// Soft-deleted assets are excluded by the repo's default scope, so a deleted
// asset resolves to thumbs.ErrAssetNotFound and the handler returns 404.
type thumbAssetResolver struct {
	assets *repo.AssetRepo
	root   string
}

// Resolve implements thumbs.AssetResolver.
func (t thumbAssetResolver) Resolve(ctx context.Context, assetID string) (string, string, error) {
	a, err := t.assets.GetByID(ctx, assetID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return "", "", thumbs.ErrAssetNotFound
		}
		return "", "", err
	}
	return library.ResolvePath(t.root, a.CurrentArchivePath), a.QuickHash, nil
}

// ThumbResolver returns the AssetResolver the thumbnail handler uses for this
// open library.
func (c *AppCore) ThumbResolver() thumbs.AssetResolver {
	return thumbAssetResolver{assets: c.Assets, root: c.Root}
}
