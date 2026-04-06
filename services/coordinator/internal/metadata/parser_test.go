package metadata

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const sampleManifest = `<?xml version="1.0" encoding="utf-8"?>
<manifest xmlns:android="http://schemas.android.com/apk/res/android"
    package="com.example.testapp"
    android:versionCode="42"
    android:versionName="1.2.3">

    <uses-sdk
        android:minSdkVersion="21"
        android:targetSdkVersion="34" />

    <uses-permission android:name="android.permission.INTERNET" />
    <uses-permission android:name="android.permission.CAMERA" />

    <application>
        <activity android:name=".MainActivity" />
        <activity android:name=".SettingsActivity" />
        <service android:name=".SyncService" />
        <receiver android:name=".BootReceiver" />
    </application>
</manifest>`

func writeTempManifest(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "AndroidManifest.xml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}
	return dir
}

func TestParseFromApktoolOutput_FullManifest(t *testing.T) {
	dir := writeTempManifest(t, sampleManifest)

	meta, err := ParseFromApktoolOutput(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if meta.PackageName != "com.example.testapp" {
		t.Errorf("PackageName = %q, want %q", meta.PackageName, "com.example.testapp")
	}
	if meta.Version != "1.2.3" {
		t.Errorf("Version = %q, want %q", meta.Version, "1.2.3")
	}
	if meta.VersionCode != 42 {
		t.Errorf("VersionCode = %d, want 42", meta.VersionCode)
	}
	if meta.MinSDK != 21 {
		t.Errorf("MinSDK = %d, want 21", meta.MinSDK)
	}
	if meta.TargetSDK != 34 {
		t.Errorf("TargetSDK = %d, want 34", meta.TargetSDK)
	}

	wantPerms := []string{
		"android.permission.INTERNET",
		"android.permission.CAMERA",
	}
	if !reflect.DeepEqual(meta.Permissions, wantPerms) {
		t.Errorf("Permissions = %v, want %v", meta.Permissions, wantPerms)
	}

	wantActivities := []string{".MainActivity", ".SettingsActivity"}
	if !reflect.DeepEqual(meta.Activities, wantActivities) {
		t.Errorf("Activities = %v, want %v", meta.Activities, wantActivities)
	}

	wantServices := []string{".SyncService"}
	if !reflect.DeepEqual(meta.Services, wantServices) {
		t.Errorf("Services = %v, want %v", meta.Services, wantServices)
	}

	wantReceivers := []string{".BootReceiver"}
	if !reflect.DeepEqual(meta.Receivers, wantReceivers) {
		t.Errorf("Receivers = %v, want %v", meta.Receivers, wantReceivers)
	}
}

func TestParseFromApktoolOutput_MinimalManifest(t *testing.T) {
	minimal := `<?xml version="1.0" encoding="utf-8"?>
<manifest xmlns:android="http://schemas.android.com/apk/res/android"
    package="com.minimal.app">
    <application />
</manifest>`

	dir := writeTempManifest(t, minimal)
	meta, err := ParseFromApktoolOutput(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if meta.PackageName != "com.minimal.app" {
		t.Errorf("PackageName = %q, want %q", meta.PackageName, "com.minimal.app")
	}
	// Optional numeric fields should default to zero
	if meta.VersionCode != 0 {
		t.Errorf("VersionCode = %d, want 0", meta.VersionCode)
	}
	if meta.MinSDK != 0 {
		t.Errorf("MinSDK = %d, want 0", meta.MinSDK)
	}
	if meta.TargetSDK != 0 {
		t.Errorf("TargetSDK = %d, want 0", meta.TargetSDK)
	}
	if len(meta.Permissions) != 0 {
		t.Errorf("Permissions = %v, want empty", meta.Permissions)
	}
	if len(meta.Activities) != 0 {
		t.Errorf("Activities = %v, want empty", meta.Activities)
	}
}

func TestParseFromApktoolOutput_MissingFile(t *testing.T) {
	dir := t.TempDir()
	// No AndroidManifest.xml written

	_, err := ParseFromApktoolOutput(dir)
	if err == nil {
		t.Fatal("expected error for missing manifest, got nil")
	}
}

func TestParseFromApktoolOutput_InvalidXML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AndroidManifest.xml")
	if err := os.WriteFile(path, []byte("not xml at all <<<>>>"), 0o644); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}

	_, err := ParseFromApktoolOutput(dir)
	if err == nil {
		t.Fatal("expected error for invalid XML, got nil")
	}
}

func TestParseFromApktoolOutput_NoPermissionsOrComponents(t *testing.T) {
	manifest := `<?xml version="1.0" encoding="utf-8"?>
<manifest package="com.bare.app" android:versionCode="1" android:versionName="1.0">
    <uses-sdk android:minSdkVersion="26" android:targetSdkVersion="33" />
    <application></application>
</manifest>`

	dir := writeTempManifest(t, manifest)
	meta, err := ParseFromApktoolOutput(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if meta.MinSDK != 26 {
		t.Errorf("MinSDK = %d, want 26", meta.MinSDK)
	}
	if meta.TargetSDK != 33 {
		t.Errorf("TargetSDK = %d, want 33", meta.TargetSDK)
	}
	if meta.VersionCode != 1 {
		t.Errorf("VersionCode = %d, want 1", meta.VersionCode)
	}
	if len(meta.Permissions) != 0 {
		t.Errorf("expected no permissions, got %v", meta.Permissions)
	}
}
