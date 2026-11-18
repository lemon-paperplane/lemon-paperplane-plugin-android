package android

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStudioCandidates(t *testing.T) {
	mac := studioCandidates("darwin", "/Users/u", func(string) string { return "" })
	if mac[0] != "/Applications/Android Studio.app" || !containsPath(mac, "/Users/u/Applications/JetBrains Toolbox/Android Studio.app") {
		t.Fatalf("mac %v", mac)
	}
	env := map[string]string{"ProgramFiles": `C:\Program Files`, "LOCALAPPDATA": `C:\Users\u\AppData\Local`}
	win := studioCandidates("windows", `C:\Users\u`, func(k string) string { return env[k] })
	if !containsPath(win, filepath.Join(`C:\Program Files`, "Android", "Android Studio", "bin", "studio64.exe")) {
		t.Fatalf("win %v", win)
	}
	if lin := studioCandidates("linux", "/home/u", func(string) string { return "" }); !containsPath(lin, "/opt/android-studio/bin/studio.sh") {
		t.Fatalf("linux %v", lin)
	}
}

func containsPath(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestReleaseOutputs(t *testing.T) {
	dir := t.TempDir()
	app := &AppProject{Dir: dir}
	touch(t, filepath.Join(dir, "build", "outputs", "apk", "release", "app-release-unsigned.apk"))
	touch(t, filepath.Join(dir, "build", "outputs", "apk", "release", "output-metadata.json"))
	touch(t, filepath.Join(dir, "build", "outputs", "apk", "freeArm", "release", "app-free-arm-release.apk"))
	touch(t, filepath.Join(dir, "build", "outputs", "bundle", "freeArmRelease", "app-free-arm-release.aab"))
	if got := releaseOutputs(app, Variant{Name: "release", BuildType: "release"}, false); len(got) != 1 || !strings.HasSuffix(got[0], "app-release-unsigned.apk") {
		t.Fatalf("apk %v", got)
	}
	v := Variant{Name: "freeArmRelease", Flavors: []string{"free", "arm"}, BuildType: "release"}
	if got := releaseOutputs(app, v, false); len(got) != 1 || !strings.HasSuffix(got[0], "app-free-arm-release.apk") {
		t.Fatalf("flavor apk %v", got)
	}
	if got := releaseOutputs(app, v, true); len(got) != 1 || !strings.HasSuffix(got[0], ".aab") {
		t.Fatalf("aab %v", got)
	}
}

func TestOutputApplicationID(t *testing.T) {
	dir := t.TempDir()
	app := &AppProject{Dir: dir, ApplicationID: "com.static"}
	p := filepath.Join(dir, "build", "outputs", "apk", "free", "debug", "output-metadata.json")
	touch(t, p)
	writeFile(t, p, `{"version":3,"artifactType":{"type":"APK","kind":"Directory"},"applicationId":"com.real.free.debug","variantName":"freeDebug","elements":[]}`)
	v := Variant{Name: "freeDebug", Flavors: []string{"free"}, BuildType: "debug"}
	if got := outputApplicationID(app, v); got != "com.real.free.debug" {
		t.Fatalf("got %q", got)
	}
	if got := outputApplicationID(app, Variant{Name: "debug", BuildType: "debug"}); got != "" {
		t.Fatalf("no metadata: %q", got)
	}
}
