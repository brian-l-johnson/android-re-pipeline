// Package apkinfo extracts basic metadata from an APK file by parsing its
// binary AndroidManifest.xml. It reads only the root <manifest> element and
// returns immediately, so it is fast regardless of APK size.
package apkinfo

import (
	"encoding/xml"
	"errors"

	"github.com/avast/apkparser"
)

// Info holds the fields extracted from AndroidManifest.xml.
type Info struct {
	PackageName string
	VersionName string
	VersionCode string
}

// ParseAPK opens the APK at path, decodes the binary AndroidManifest.xml, and
// returns the package name and version. Non-fatal parse errors (e.g. unknown
// resource references) are silently ignored; only a complete failure to open
// or parse the manifest returns an error.
func ParseAPK(path string) (*Info, error) {
	info := &Info{}
	enc := &manifestEncoder{info: info}

	_, _, manifestErr := apkparser.ParseApk(path, enc)

	// ErrEndParsing is our own sentinel returned after we've captured what we
	// need — it is not a real error.
	if manifestErr != nil && !errors.Is(manifestErr, apkparser.ErrEndParsing) {
		return nil, manifestErr
	}
	return info, nil
}

// manifestEncoder implements apkparser.ManifestEncoder. It captures attributes
// from the root <manifest> element and then signals the parser to stop via
// apkparser.ErrEndParsing.
type manifestEncoder struct {
	info *Info
	done bool
}

func (e *manifestEncoder) EncodeToken(t xml.Token) error {
	if e.done {
		return nil
	}
	start, ok := t.(xml.StartElement)
	if !ok || start.Name.Local != "manifest" {
		return nil
	}

	for _, attr := range start.Attr {
		switch attr.Name.Local {
		case "package":
			e.info.PackageName = attr.Value
		case "versionName":
			e.info.VersionName = attr.Value
		case "versionCode":
			e.info.VersionCode = attr.Value
		}
	}

	e.done = true
	// Stop parsing — we have everything we need.
	return apkparser.ErrEndParsing
}

func (e *manifestEncoder) Flush() error { return nil }
