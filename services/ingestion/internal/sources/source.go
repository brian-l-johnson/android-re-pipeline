package sources

import (
	"context"
	"io"
)

// APKMetadata holds metadata about an APK, partially populated at download time
// and fully populated after the APK is parsed.
type APKMetadata struct {
	PackageName string
	Version     string
	VersionCode int
	Source      string
	DownloadURL string
	SHA256      string // hex-encoded, computed after download
}

// APKSource is the interface implemented by every APK acquisition source.
// identifier is a URL for DirectURL, or a package name for scraping-based sources.
type APKSource interface {
	Name() string
	Download(ctx context.Context, identifier string) (io.ReadCloser, *APKMetadata, error)
}
