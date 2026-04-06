package metadata

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// APKMetadata holds the parsed fields from AndroidManifest.xml.
type APKMetadata struct {
	PackageName string
	Version     string
	VersionCode int
	MinSDK      int
	TargetSDK   int
	Permissions []string
	Activities  []string
	Services    []string
	Receivers   []string
}

// manifest is the top-level XML element of AndroidManifest.xml.
type manifest struct {
	XMLName     xml.Name    `xml:"manifest"`
	Package     string      `xml:"package,attr"`
	VersionName string      `xml:"versionName,attr"`
	VersionCode string      `xml:"versionCode,attr"`
	UsesSdk     usesSdk     `xml:"uses-sdk"`
	Application application `xml:"application"`
	Permissions []permission `xml:"uses-permission"`
}

type usesSdk struct {
	MinSdkVersion    string `xml:"minSdkVersion,attr"`
	TargetSdkVersion string `xml:"targetSdkVersion,attr"`
}

type application struct {
	Activities []activity `xml:"activity"`
	Services   []service  `xml:"service"`
	Receivers  []receiver `xml:"receiver"`
}

type activity struct {
	Name string `xml:"name,attr"`
}

type service struct {
	Name string `xml:"name,attr"`
}

type receiver struct {
	Name string `xml:"name,attr"`
}

type permission struct {
	Name string `xml:"name,attr"`
}

// ParseFromApktoolOutput reads AndroidManifest.xml from the apktool output
// directory and extracts APK metadata. Partial results are returned when
// optional fields are missing — parsing only fails on fatal XML errors.
func ParseFromApktoolOutput(outputDir string) (*APKMetadata, error) {
	manifestPath := filepath.Join(outputDir, "AndroidManifest.xml")

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read AndroidManifest.xml: %w", err)
	}

	var m manifest
	if err := xml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse AndroidManifest.xml: %w", err)
	}

	meta := &APKMetadata{
		PackageName: m.Package,
		Version:     m.VersionName,
	}

	if m.VersionCode != "" {
		if vc, err := strconv.Atoi(m.VersionCode); err == nil {
			meta.VersionCode = vc
		}
	}

	if m.UsesSdk.MinSdkVersion != "" {
		if v, err := strconv.Atoi(m.UsesSdk.MinSdkVersion); err == nil {
			meta.MinSDK = v
		}
	}

	if m.UsesSdk.TargetSdkVersion != "" {
		if v, err := strconv.Atoi(m.UsesSdk.TargetSdkVersion); err == nil {
			meta.TargetSDK = v
		}
	}

	for _, p := range m.Permissions {
		if p.Name != "" {
			meta.Permissions = append(meta.Permissions, p.Name)
		}
	}

	for _, a := range m.Application.Activities {
		if a.Name != "" {
			meta.Activities = append(meta.Activities, a.Name)
		}
	}

	for _, svc := range m.Application.Services {
		if svc.Name != "" {
			meta.Services = append(meta.Services, svc.Name)
		}
	}

	for _, r := range m.Application.Receivers {
		if r.Name != "" {
			meta.Receivers = append(meta.Receivers, r.Name)
		}
	}

	return meta, nil
}
