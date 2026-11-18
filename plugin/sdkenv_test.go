package android

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestLocateSDK(t *testing.T) {
	root := t.TempDir()
	sdkDir := t.TempDir()
	touch(t, filepath.Join(sdkDir, "platform-tools", "adb"))
	// local.properties 优先，且 \: 转义要还原
	escaped := strings.ReplaceAll(sdkDir, ":", `\:`)
	_ = os.WriteFile(filepath.Join(root, "local.properties"), []byte("## comment\nsdk.dir="+escaped+"\n"), 0o644)
	if loc := locateSDK(root, map[string]string{"ANDROID_HOME": "/elsewhere"}); loc.Dir != sdkDir || loc.Source != srcLocalProps || !loc.Exists {
		t.Fatalf("local.properties: %+v", loc)
	}
	// 指向不存在的目录：照样返回（AGP 也会用它），Exists=false
	_ = os.WriteFile(filepath.Join(root, "local.properties"), []byte("sdk.dir=/nonexistent/sdk\n"), 0o644)
	if loc := locateSDK(root, nil); loc.Exists || loc.Source != srcLocalProps {
		t.Fatalf("missing: %+v", loc)
	}
	_ = os.Remove(filepath.Join(root, "local.properties"))
	if loc := locateSDK(root, map[string]string{"ANDROID_HOME": sdkDir}); loc.Dir != sdkDir || loc.Source != srcEnvHome || !loc.Exists {
		t.Fatalf("env: %+v", loc)
	}
	if loc := locateSDK(root, map[string]string{"ANDROID_HOME": "", "ANDROID_SDK_ROOT": sdkDir}); loc.Source != srcEnvRoot {
		t.Fatalf("sdk root: %+v", loc)
	}
}

func TestMissingPackages(t *testing.T) {
	sdkDir := t.TempDir()
	touch(t, filepath.Join(sdkDir, "platforms", "android-35", "android.jar"))
	touch(t, filepath.Join(sdkDir, "build-tools", "35.0.0", "source.properties"))
	touch(t, filepath.Join(sdkDir, "ndk", "27.0.12077973", "source.properties"))
	ps := []*AppProject{
		{Name: "app", CompileSdk: "35", BuildTools: "35.0.0", NdkVersion: "27.0.12077973"},
		{Name: "lib", CompileSdk: "36.1", BuildTools: "36.0.0", NdkVersion: "26.1.10909125"},
		{Name: "unity", CompileSdk: "35", NdkVersion: "27.2.12479018", NdkPath: "/Unity/NDK"}, // 写了 ndkPath 不查 SDK 里的 NDK
	}
	got := missingPackages(sdkDir, ps)
	want := []string{"platforms;android-36.1", "build-tools;36.0.0", "ndk;26.1.10909125"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

const repoSample = `<?xml version="1.0" encoding="UTF-8" standalone="no"?>
<sdk:sdk-repository xmlns:sdk="http://schemas.android.com/sdk/android/repo/repository2/03" xmlns:common="http://schemas.android.com/repository/android/common/02">
  <channel id="channel-0">stable</channel>
  <channel id="channel-3">canary</channel>
  <remotePackage path="cmdline-tools;latest">
    <revision><major>23</major><minor>0</minor></revision>
    <channelRef ref="channel-0"/>
    <archives>
      <archive><complete><size>181052239</size><checksum type="sha1">e025545c62a8e64c7559119566a569fb1dec5f60</checksum><url>commandlinetools-linux-16111833_latest.zip</url></complete><host-os>linux</host-os></archive>
      <archive><complete><size>155582504</size><checksum type="sha1">112cf9618794a997ff273537d55bee02c22abffe</checksum><url>commandlinetools-mac_x86_64-16111833_latest.zip</url></complete><host-os>macosx</host-os><host-arch>x64</host-arch></archive>
      <archive><complete><size>155384151</size><checksum type="sha1">ad03dc49bfacfd52c110b14104ea548b8a07e830</checksum><url>commandlinetools-mac_arm64-16111833_latest.zip</url></complete><host-os>macosx</host-os><host-arch>aarch64</host-arch></archive>
      <archive><complete><size>154957218</size><checksum type="sha1">57d04f2d75eb8e8fffc5000a987e5de4b5a63e9d</checksum><url>commandlinetools-win-16111833_latest.zip</url></complete><host-os>windows</host-os></archive>
    </archives>
  </remotePackage>
  <remotePackage path="cmdline-tools;latest">
    <revision><major>24</major><minor>0</minor></revision>
    <channelRef ref="channel-3"/>
    <archives><archive><complete><size>1</size><checksum type="sha1">00</checksum><url>commandlinetools-mac_arm64-canary.zip</url></complete><host-os>macosx</host-os></archive></archives>
  </remotePackage>
</sdk:sdk-repository>`

func TestPickCmdlineTools(t *testing.T) {
	for _, c := range []struct{ goos, goarch, url string }{
		{"darwin", "arm64", "commandlinetools-mac_arm64-16111833_latest.zip"},
		{"darwin", "amd64", "commandlinetools-mac_x86_64-16111833_latest.zip"},
		{"linux", "amd64", "commandlinetools-linux-16111833_latest.zip"},
		{"windows", "amd64", "commandlinetools-win-16111833_latest.zip"},
	} {
		a, err := pickCmdlineTools([]byte(repoSample), c.goos, c.goarch)
		if err != nil || a.URL != c.url {
			t.Errorf("%s/%s: %+v %v", c.goos, c.goarch, a, err)
		}
	}
	if a, _ := pickCmdlineTools([]byte(repoSample), "darwin", "arm64"); a.SHA1 != "ad03dc49bfacfd52c110b14104ea548b8a07e830" {
		t.Errorf("sha1 %s", a.SHA1)
	}
}

func TestSDKTool(t *testing.T) {
	newer := t.TempDir()
	touch(t, filepath.Join(newer, "cmdline-tools", "latest", "bin", "sdkmanager"))
	touch(t, filepath.Join(newer, "cmdline-tools", "latest", "bin", "android"))
	tool := findSDKTool(newer)
	if tool == nil || !tool.CLI {
		t.Fatalf("new cmdline-tools: %+v", tool)
	}
	cmd, args := sdkInstallCmd(tool, "/sdk", []string{"platforms;android-35", "platform-tools"}, true, nil)
	if !strings.HasSuffix(cmd, "android") || strings.Join(args, " ") != "--no-metrics --sdk=/sdk --sdk_url="+tencentRepo+" --sdk_index="+tencentRepo+"/package_list.binpb sdk install platforms/android-35 platform-tools" {
		t.Fatalf("cli args %v", args)
	}
	older := t.TempDir()
	name := "sdkmanager"
	if runtime.GOOS == "windows" {
		name = "sdkmanager.bat"
	}
	touch(t, filepath.Join(older, "cmdline-tools", "9.0", "bin", name))
	tool = findSDKTool(older)
	if tool == nil || tool.CLI {
		t.Fatalf("old cmdline-tools: %+v", tool)
	}
	_, args = sdkInstallCmd(tool, "/sdk", []string{"platforms;android-35"}, true, map[string]string{"HTTPS_PROXY": "http://127.0.0.1:7890"})
	if strings.Join(args, " ") != "--sdk_root=/sdk --proxy=http --proxy_host=127.0.0.1 --proxy_port=7890 platforms;android-35" {
		t.Fatalf("sdkmanager args %v", args)
	}
	if findSDKTool(t.TempDir()) != nil {
		t.Fatal("empty sdk")
	}
}

func TestProxyArgs(t *testing.T) {
	clear := map[string]string{"HTTPS_PROXY": "", "https_proxy": "", "HTTP_PROXY": "", "http_proxy": ""}
	if got := proxyArgs(clear); got != nil {
		t.Fatalf("no proxy: %v", got)
	}
	env := map[string]string{"HTTPS_PROXY": "", "https_proxy": "", "HTTP_PROXY": "socks5://10.0.0.1:1080", "http_proxy": ""}
	if got := strings.Join(proxyArgs(env), " "); got != "--proxy=socks --proxy_host=10.0.0.1 --proxy_port=1080" {
		t.Fatalf("socks: %s", got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
