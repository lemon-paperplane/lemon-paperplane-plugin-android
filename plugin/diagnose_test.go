package android

import (
	"regexp"
	"strings"
	"testing"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// PrepareRun 的选设备错误经内核写成「启动失败：<文案>」，Diagnose 要能认回来（两种语言）。
func TestDeviceErrorKind(t *testing.T) {
	for _, loc := range []string{"zh-Hans", "en"} {
		b := bundle(t, loc)
		for _, kind := range []string{selNoDevice, selMultiple, selChooseAVD, selNotFound, selAVDNotFound, selUnauthorized, selOffline, selNoPermission} {
			line := "启动失败：" + selectErrorText(b.T, &selectError{Kind: kind, Device: "Pixel 7 · R58N1"})
			if got := deviceErrorKind([]string{"> Task :app:preBuild", line}, b.T); got != kind {
				t.Errorf("%s %s: got %q from %q", loc, kind, got, line)
			}
		}
		if got := deviceErrorKind([]string{"BUILD SUCCESSFUL in 3s"}, b.T); got != "" {
			t.Errorf("%s: false positive %q", loc, got)
		}
		// 选设备错误文案不能触发声明式「未授权」规则（否则重复两条）
		re := regexp.MustCompile(`(?:device unauthorized|DeviceException:.*unauthorized)`)
		if re.MatchString(selectErrorText(b.T, &selectError{Kind: selUnauthorized, Device: "X"})) {
			t.Errorf("%s: unauthorized text matches the declarative rule", loc)
		}
	}
	if deviceErrorKind([]string{"> java.util.concurrent.ExecutionException: com.android.builder.testing.api.DeviceException: No connected devices!"}, bundle(t, "en").T) != selNotFound {
		t.Error("AGP no connected devices")
	}
}

func TestDeviceFixes(t *testing.T) {
	ctx := &sdk.Context{}
	devs := []Device{
		{Serial: "R1", State: "device", Model: "Pixel_7"},
		{Serial: "emulator-5554", State: "device", AVD: "Medium"},
		{Serial: "R2", State: "unauthorized"},
	}
	fixes := deviceFixes(ctx, devs, []AVD{{Name: "Medium"}, {Name: "XR"}}, "R1")
	var vals []string
	for _, f := range fixes {
		if f.Kind != sdk.FixSetRunOption || f.Params["key"] != "device" {
			t.Fatalf("fix %+v", f)
		}
		vals = append(vals, f.Params["value"])
	}
	// 当前已选的 R1、未授权的 R2 不给；运行中的模拟器和 AVD 合并
	if strings.Join(vals, ",") != "avd:Medium,avd:XR" {
		t.Fatalf("values %v", vals)
	}
}

func TestTemplateMatch(t *testing.T) {
	if !templateMatch("启动失败：找不到设备 R58N1，请确认已连接", "找不到设备 {0}，请确认已连接") {
		t.Error("should match")
	}
	if templateMatch("找不到设备", "找不到设备 {0}，请确认已连接") {
		t.Error("should not match")
	}
	if !templateMatch("Emulator X was not found", "Emulator {0} was not found") || templateMatch("was not found Emulator", "Emulator {0} was not found") {
		t.Error("order")
	}
}

func TestCrashDiagnosis(t *testing.T) {
	tail := []string{
		"11-18 10:00:00.000  2222  2222 I MainActivity: onCreate",
		"11-18 10:00:01.000  2222  2222 E AndroidRuntime: FATAL EXCEPTION: main",
		"11-18 10:00:01.000  2222  2222 E AndroidRuntime: Process: com.a, PID: 2222",
		"11-18 10:00:01.000  2222  2222 E AndroidRuntime: java.lang.RuntimeException: Unable to start activity ComponentInfo{com.a/com.a.Main}: java.lang.NullPointerException",
		"11-18 10:00:01.000  2222  2222 E AndroidRuntime: \tat android.app.ActivityThread.performLaunchActivity(ActivityThread.java:4164)",
		"11-18 10:00:01.000  2222  2222 E AndroidRuntime: Caused by: java.lang.IllegalStateException: paperplane test crash",
		"11-18 10:00:01.000  2222  2222 E AndroidRuntime: \tat com.a.Main.onCreate(Main.java:22)",
		"App crashed: com.a",
	}
	d := crashDiagnosis(bundle(t, "en").T, tail)
	if d == nil || d.Title != "App crashed: java.lang.IllegalStateException: paperplane test crash" {
		t.Fatalf("crash diagnosis %+v", d)
	}
	if !strings.HasPrefix(d.Detail, "FATAL EXCEPTION: main\nProcess: com.a") {
		t.Fatalf("detail %q", d.Detail)
	}
	if crashDiagnosis(bundle(t, "en").T, []string{"I Test: hi"}) != nil {
		t.Error("false positive")
	}
}

// settings.gradle 里 throw new GradleException('中文提示') 的原文要显示出来。
func TestGradleFailureSettingsException(t *testing.T) {
	tail := strings.Split(`
FAILURE: Build failed with an exception.

* Where:
Settings file '/Users/x/vrtalk-play-app-android/settings.gradle' line: 13

* What went wrong:
A problem occurred evaluating settings 'VRTalkPlay'.
> 请先在 vrtalk-play-app-core 中执行 VRTalk → 编译并导出到Android，再打开此工程。

* Try:
> Run with --stacktrace option to get the stack trace.
> Run with --info or --debug option to get more log output.

BUILD FAILED in 1s`, "\n")
	gf := parseGradleFailure(tail)
	if gf == nil || !gf.scriptError() {
		t.Fatalf("gf %+v", gf)
	}
	b := bundle(t, "zh-Hans")
	if gf.message() != "请先在 vrtalk-play-app-core 中执行 VRTalk → 编译并导出到Android，再打开此工程。" {
		t.Fatalf("message %q", gf.message())
	}
	d := sdk.Diagnosis{Title: b.T("diag.gradle.title", gf.message())}
	if !strings.Contains(d.Title, "请先在 vrtalk-play-app-core 中执行") {
		t.Fatal(d.Title)
	}
	if !whereRe.MatchString(gf.Where[0]) {
		t.Fatalf("where %v", gf.Where)
	}
}

func TestGradleFailureTask(t *testing.T) {
	tail := strings.Split(`* What went wrong:
Execution failed for task ':app:installDebug'.
> java.util.concurrent.ExecutionException: com.android.builder.testing.api.DeviceException: No connected devices!

* Try:
> Run with --stacktrace option to get the stack trace.`, "\n")
	gf := parseGradleFailure(tail)
	if gf == nil || gf.scriptError() || gf.message() != "No connected devices!" {
		t.Fatalf("gf %+v msg %q", gf, gf.message())
	}
	nested := strings.Split(`* What went wrong:
Could not determine the dependencies of task ':app:compileDebugJavaWithJavac'.
> Could not resolve all task dependencies for configuration ':app:debugCompileClasspath'.
   > Could not find com.foo:bar:1.0.

`, "\n")
	if g := parseGradleFailure(nested); g == nil || g.message() != "Could not find com.foo:bar:1.0." {
		t.Fatalf("nested %+v", g)
	}
	if parseGradleFailure([]string{"BUILD SUCCESSFUL"}) != nil {
		t.Error("false positive")
	}
}

// 声明式规则：用真实日志行检查命中与捕获组。
func TestManifestRulesMatch(t *testing.T) {
	m, err := sdk.LoadManifest("..")
	if err != nil {
		t.Fatal(err)
	}
	rules := map[string]*regexp.Regexp{}
	hints := map[string]string{}
	for _, r := range m.Contributes.Diagnostics {
		rules[r.ID] = regexp.MustCompile(r.Pattern)
		hints[r.ID] = r.Hint
		if len(r.Facets) != 1 || r.Facets[0] != "android" {
			t.Errorf("%s 没有限定 android facet", r.ID)
		}
	}
	cases := []struct {
		rule, line, group string
	}{
		{"android.install-update-incompatible", "> java.util.concurrent.ExecutionException: com.android.builder.testing.api.DeviceException: com.android.ddmlib.InstallException: INSTALL_FAILED_UPDATE_INCOMPATIBLE: Existing package com.example.app signatures do not match newer version; ignoring!", "com.example.app"},
		{"android.install-update-incompatible", "Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE: Package com.example.app signatures do not match previously installed version; ignoring!]", "com.example.app"},
		{"android.install-update-incompatible", "INSTALL_FAILED_UPDATE_INCOMPATIBLE", ""},
		{"android.install-version-downgrade", "InstallException: INSTALL_FAILED_VERSION_DOWNGRADE: Downgrade detected: Update version code 1 is older than current 5", ""},
		{"android.install-insufficient-storage", "com.android.ddmlib.InstallException: INSTALL_FAILED_INSUFFICIENT_STORAGE", ""},
		{"android.install-user-restricted", "INSTALL_FAILED_USER_RESTRICTED: Install canceled by user", ""},
		{"android.install-no-matching-abis", "INSTALL_FAILED_NO_MATCHING_ABIS: Failed to extract native libraries, res=-113", "Failed to extract native libraries, res=-113"},
		{"android.install-older-sdk", "INSTALL_FAILED_OLDER_SDK: Failed parse during installPackageLI", ""},
		{"android.licenses", "Failed to install the following Android SDK packages as some licences have not been accepted.", ""},
		{"android.licenses", "You have not accepted the license agreements of the following SDK components:", ""},
		{"android.licenses", "License for package Android SDK Platform 35 not accepted.", ""},
		{"android.missing-target", "Failed to find target with hash string 'android-34' in: /sdk", "android-34"},
		{"android.missing-platform", "Failed to find Platform SDK with path: platforms;android-36.1", "android-36.1"},
		{"android.missing-build-tools", "Failed to find Build Tools revision 30.0.3", "30.0.3"},
		{"android.ndk-version", "No version of NDK matched the requested version 25.1.8937393. Versions available locally: 27.0.12077973", "25.1.8937393"},
		{"android.ndk-missing", "NDK not configured. Download it with SDK manager. Preferred NDK version is '27.0.12077973'.", ""},
		{"android.ndk-missing", "[CXX1101] NDK at /sdk/ndk/27.0.12077973 did not have a source.properties file", ""},
		{"android.device-unauthorized", "com.android.ddmlib.AdbCommandRejectedException: device unauthorized.", ""},
	}
	for _, c := range cases {
		re := rules[c.rule]
		if re == nil {
			t.Fatalf("no rule %s", c.rule)
		}
		if !strings.Contains(c.line, hints[c.rule]) {
			t.Errorf("%s: hint %q not in line", c.rule, hints[c.rule])
		}
		sub := re.FindStringSubmatch(c.line)
		if sub == nil {
			t.Errorf("%s: no match for %q", c.rule, c.line)
			continue
		}
		g := ""
		if len(sub) > 1 {
			g = sub[1]
		}
		if g != c.group {
			t.Errorf("%s: group %q, want %q", c.rule, g, c.group)
		}
	}
	// 正常的 Gradle 输出不应误报
	for _, line := range []string{"> Task :app:installDebug", "Installing APK 'app-debug.apk' on 'Pixel_7(AVD) - 16' for :app:debug", "Installed on 1 device.", "BUILD SUCCESSFUL in 12s"} {
		for id, re := range rules {
			if strings.Contains(line, hints[id]) && re.MatchString(line) {
				t.Errorf("%s matched normal line %q", id, line)
			}
		}
	}
}
