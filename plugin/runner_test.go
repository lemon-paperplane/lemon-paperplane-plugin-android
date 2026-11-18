package android

import (
	"regexp"
	"strings"
	"testing"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

func bundle(t *testing.T, locale string) *sdk.Bundle {
	b, err := sdk.LoadBundle("..", locale)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseResolveActivity(t *testing.T) {
	out := "priority=0 preferredOrder=0 match=0x108000 specificIndex=-1 isDefault=true\r\ncom.example.app/.MainActivity\r\n"
	if got := parseResolveActivity(out, "com.example.app"); got != "com.example.app/.MainActivity" {
		t.Fatalf("got %q", got)
	}
	if got := parseResolveActivity("No activity found\n", "x"); got != "" {
		t.Fatalf("no activity: %q", got)
	}
	for in, want := range map[string]string{
		".MainActivity":              "com.a/.MainActivity",
		"com.a.ui.Main":              "com.a/com.a.ui.Main",
		"com.other/.Main":            "com.other/.Main",
		" .Main ":                    "com.a/.Main",
		"com.a/.Outer$InnerActivity": "com.a/.Outer$InnerActivity",
	} {
		if got := componentName("com.a", in); got != want {
			t.Errorf("componentName(%q) = %q", in, got)
		}
	}
	if shellQuote("com.a/.Main") != "com.a/.Main" || shellQuote("com.a/.Outer$Inner") != "'com.a/.Outer$Inner'" {
		t.Errorf("shellQuote %s", shellQuote("com.a/.Outer$Inner"))
	}
}

func TestAmStartOutput(t *testing.T) {
	ok := "Starting: Intent { act=android.intent.action.MAIN cat=[android.intent.category.LAUNCHER] cmp=com.a/.Main }\nStatus: ok\nLaunchState: COLD\nActivity: com.a/.Main\nTotalTime: 512\nWaitTime: 520\nComplete\n"
	if amStartFailed(ok) {
		t.Error("ok output treated as failure")
	}
	bad := "Starting: Intent { cmp=com.a/.Nope }\nError type 3\nError: Activity class {com.a/com.a.Nope} does not exist.\n"
	if !amStartFailed(bad) {
		t.Error("error output not detected")
	}
	if !amStartFailed("Exception occurred while executing 'start':\njava.lang.SecurityException: Permission Denial") {
		t.Error("security exception not detected")
	}
}

func TestPidParsing(t *testing.T) {
	if parsePidof("12345\n") != "12345" || parsePidof("12345 12399\n") != "12345" || parsePidof("") != "" || parsePidof("/system/bin/sh: pidof: not found") != "" {
		t.Error("parsePidof")
	}
	ps := `USER     PID   PPID  VSIZE  RSS     WCHAN    PC         NAME
root      1     0     8904   788   ffffffff 00000000 S /init
u0_a62    2345  1830  1501232 41232 ffffffff 00000000 S com.a
u0_a62    2399  1830  1401232 31232 ffffffff 00000000 S com.a:remote
`
	if pidFromPS(ps, "com.a") != "2345" || pidFromPS(ps, "com.b") != "" {
		t.Error("pidFromPS")
	}
}

func TestDeviceErrors(t *testing.T) {
	for _, s := range []string{"error: device 'emulator-5554' not found", "error: device offline", "error: no devices/emulators found", "error: device unauthorized.\nThis adb server's $ADB_VENDOR_KEYS is not set", "error: closed"} {
		if !isDeviceError(s) {
			t.Errorf("not a device error: %q", s)
		}
	}
	if isDeviceError("") || isDeviceError("/system/bin/sh: pidof: not found") {
		t.Error("false positive")
	}
}

func TestCrashSection(t *testing.T) {
	log := `--------- beginning of crash
11-18 10:00:00.000  1111  1111 E AndroidRuntime: FATAL EXCEPTION: main
11-18 10:00:00.000  1111  1111 E AndroidRuntime: Process: com.other, PID: 1111
11-18 10:00:00.000  1111  1111 E AndroidRuntime: java.lang.IllegalStateException: other
11-18 10:00:01.000  2222  2222 E AndroidRuntime: FATAL EXCEPTION: main
11-18 10:00:01.000  2222  2222 E AndroidRuntime: Process: com.a, PID: 2222
11-18 10:00:01.000  2222  2222 E AndroidRuntime: java.lang.RuntimeException: boom
11-18 10:00:01.000  2222  2222 E AndroidRuntime: 	at com.a.MainActivity.onCreate(MainActivity.java:12)
`
	got := crashSection(log, "com.a")
	if len(got) != 4 || !strings.Contains(got[2], "boom") {
		t.Fatalf("crashSection = %q", got)
	}
}

// 就绪正则要能匹配运行器用同一份文案打印的就绪行（两种语言）。
func TestReadyPattern(t *testing.T) {
	for _, loc := range []string{"zh-Hans", "en"} {
		b := bundle(t, loc)
		re := regexp.MustCompile(readyPattern(b.T, "com.example.app"))
		line := b.T("run.started", "com.example.app", "12345")
		if !re.MatchString(line) {
			t.Errorf("%s: %q does not match %q", loc, re, line)
		}
		if re.MatchString(b.T("run.started", "com.example.other", "12345")) {
			t.Errorf("%s: matched another package", loc)
		}
	}
}

func TestRunnerArgs(t *testing.T) {
	a := runnerArgs{ADB: "/sdk/adb", Serial: "emulator-5554", Package: "com.a", ForceStop: false, Locale: "en", PluginDir: "/p"}
	argv := a.argv()
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--force-stop=false") || !strings.Contains(joined, "--serial emulator-5554") {
		t.Fatalf("argv %v", argv)
	}
}
