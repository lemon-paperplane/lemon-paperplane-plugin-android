package android

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

func facet(projects ...map[string]any) *sdk.Facet {
	var list []any
	for _, p := range projects {
		list = append(list, p)
	}
	// 走一遍 JSON，和内核传过来的类型一致
	b, _ := json.Marshal(map[string]any{"projects": list})
	var data map[string]any
	_ = json.Unmarshal(b, &data)
	return &sdk.Facet{ID: "gradle", Data: data}
}

func projectByPath(ps []*AppProject, path string) *AppProject {
	for _, p := range ps {
		if p.Path == path {
			return p
		}
	}
	return nil
}

func TestGroovyProject(t *testing.T) {
	root, _ := filepath.Abs("testdata/groovy")
	f := facet(map[string]any{"path": ":", "dir": ""}, map[string]any{"path": ":app", "dir": "app", "android": true}, map[string]any{"path": ":lib", "dir": "lib", "android": true})
	gps := facetProjects(f, root)
	if apps := detectApps(root, gps); !reflect.DeepEqual(apps, []string{":app"}) {
		t.Fatalf("detectApps = %v", apps)
	}
	ps := scanProjects(root, gps)
	if len(ps) != 2 {
		t.Fatalf("projects = %d", len(ps))
	}
	app := projectByPath(ps, ":app")
	if app == nil || !app.App {
		t.Fatalf("app = %+v", app)
	}
	want := map[string]string{"namespace": app.Namespace, "applicationId": app.ApplicationID, "compileSdk": app.CompileSdk, "minSdk": app.MinSdk,
		"targetSdk": app.TargetSdk, "buildTools": app.BuildTools, "ndk": app.NdkVersion}
	exp := map[string]string{"namespace": "com.example.groovy", "applicationId": "com.example.groovy", "compileSdk": "34", "minSdk": "21",
		"targetSdk": "34", "buildTools": "34.0.0", "ndk": "26.1.10909125"}
	if !reflect.DeepEqual(want, exp) {
		t.Fatalf("got %v\nwant %v", want, exp)
	}
	if !reflect.DeepEqual(app.BuildTypes, []string{"debug", "release", "staging"}) {
		t.Fatalf("buildTypes %v", app.BuildTypes)
	}
	if !reflect.DeepEqual(app.Dimensions, []string{"tier", "abi"}) || len(app.Flavors) != 4 {
		t.Fatalf("dims %v flavors %+v", app.Dimensions, app.Flavors)
	}
	if len(app.Variants) != 12 || app.Variants[0] != "freeArmDebug" || !contains(app.Variants, "paidX86Staging") {
		t.Fatalf("variants %v", app.Variants)
	}
	for v, pkg := range map[string]string{
		"freeArmDebug":   "com.example.groovy.free.debug",
		"freearmdebug":   "com.example.groovy.free.debug", // 变体名大小写不敏感
		"paidX86Release": "com.example.groovy.pro",        // 风味覆盖 applicationId
		"paidArmDebug":   "com.example.groovy.pro.debug",
		"freeX86Staging": "com.example.groovy.free.staging", // 后缀不带点时补点
		"freeArmRelease": "com.example.groovy.free",
		"debug":          "", // 有风味时没有单独的 debug 变体
		"unknown":        "",
	} {
		got := ""
		if vv, ok := app.findVariant(v); ok {
			got = app.packageName(vv)
		}
		if got != pkg {
			t.Errorf("packageName(%s) = %q, want %q", v, got, pkg)
		}
	}
	if v, _ := app.findVariant("paidX86Release"); taskPath(app.Path, "install"+capitalize(v.Name)) != ":app:installPaidX86Release" {
		t.Errorf("task %s", taskPath(app.Path, "install"+capitalize(v.Name)))
	}
	lib := projectByPath(ps, ":lib")
	if lib == nil || lib.App || lib.CompileSdk != "33" {
		t.Fatalf("lib = %+v", lib)
	}
}

func TestKtsProject(t *testing.T) {
	root, _ := filepath.Abs("testdata/kts")
	f := facet(map[string]any{"path": ":", "dir": ""}, map[string]any{"path": ":app", "dir": "app"}, map[string]any{"path": ":feature", "dir": "feature"})
	gps := facetProjects(f, root)
	if apps := detectApps(root, gps); !reflect.DeepEqual(apps, []string{":app"}) {
		t.Fatalf("detectApps = %v", apps)
	}
	ps := scanProjects(root, gps)
	app := projectByPath(ps, ":app")
	if app == nil || !app.App {
		t.Fatalf("app = %+v", ps)
	}
	if app.CompileSdk != "36.1" || app.MinSdk != "24" || app.TargetSdk != "36" || app.Namespace != "com.example.kts" || app.ApplicationID != "com.example.kts" {
		t.Fatalf("app = %+v", app)
	}
	if !reflect.DeepEqual(app.Variants, []string{"debug", "release", "benchmark"}) {
		t.Fatalf("variants %v", app.Variants)
	}
	if v, _ := app.findVariant("debug"); app.packageName(v) != "com.example.kts.dev" {
		t.Fatalf("debug package %s", app.packageName(v))
	}
	feat := projectByPath(ps, ":feature")
	if feat == nil || feat.App || feat.CompileSdk != "35" || feat.MinSdk != "23" {
		t.Fatalf("feature = %+v", feat)
	}
	// gradle 插件的模型优先于 facet 数据
	model, _ := json.Marshal(map[string]any{"projects": []map[string]any{{"path": ":app", "name": "app", "dir": filepath.Join(root, "app"), "relDir": "app", "android": true}}})
	mi := &sdk.ModuleInfo{Dir: root, Facets: []sdk.Facet{*f}, Models: map[string]json.RawMessage{"gradle": model}}
	if gp := gradleProjects(mi); len(gp) != 1 || gp[0].BuildFile != filepath.Join(root, "app", "build.gradle.kts") {
		t.Fatalf("gradleProjects = %+v", gp)
	}
}

func TestUnityLikeProject(t *testing.T) {
	root, _ := filepath.Abs("testdata/unity")
	f := facet(map[string]any{"path": ":", "dir": "", "android": true}, map[string]any{"path": ":launcher", "dir": "launcher", "android": true}, map[string]any{"path": ":unityLibrary", "dir": "unityLibrary", "android": true})
	gps := facetProjects(f, root)
	if apps := detectApps(root, gps); !reflect.DeepEqual(apps, []string{":launcher"}) {
		t.Fatalf("detectApps = %v", apps)
	}
	ps := scanProjects(root, gps)
	if len(ps) != 2 {
		t.Fatalf("根工程（只有 apply false）不应算安卓工程：%+v", ps)
	}
	l := projectByPath(ps, ":launcher")
	// targetSdk 写在 apply from 引入的 shared/common.gradle 里
	if l.TargetSdk != "35" || l.NdkPath != "/nonexistent/Unity/NDK" || l.BuildTools != "36.0.0" || l.MinSdk != "29" {
		t.Fatalf("launcher = %+v", l)
	}
}

func TestCompileSdkForms(t *testing.T) {
	cat := &catalog{Versions: map[string]string{"compilesdk": "35", "android.compile": "34"}, Plugins: map[string]string{}}
	vars := map[string]string{"compileSdkVersion": "33", "COMPILE_SDK": "android-32"}
	for src, want := range map[string]string{
		"compileSdk 35":                        "35",
		"compileSdk = 35":                      "35",
		"compileSdk(35)":                       "35",
		"compileSdkVersion 35":                 "35",
		"compileSdkVersion = 35":               "35",
		"compileSdkVersion 'android-34'":       "34",
		`compileSdkVersion "android-34"`:       "34",
		"compileSdk { version = release(36) }": "36",
		"compileSdk {\n version = release(36) {\n minorApiLevel = 1\n }\n}": "36.1",
		"compileSdk {\n version = release(36) {\n minorApiLevel = 0\n }\n}": "36",
		`compileSdk { version = preview("CinnamonBun") }`:                   "CinnamonBun",
		"compileSdk = libs.versions.compileSdk.get().toInt()":               "35",
		"compileSdk libs.versions.android.compile.get().toInteger()":        "34",
		"compileSdk rootProject.ext.compileSdkVersion":                      "33",
		`compileSdk = rootProject.extra["compileSdkVersion"] as Int`:        "33",
		"compileSdkVersion COMPILE_SDK":                                     "32",
		`compileSdkPreview = "Baklava"`:                                     "Baklava",
		"compileSdk 36\ncompileSdkMinor 1":                                  "36.1",
		"compileSdk = unknownThing":                                         "",
		"minSdk 21":                                                         "",
	} {
		if got := compileSdkOf(src, vars, cat); got != want {
			t.Errorf("compileSdkOf(%q) = %q, want %q", src, got, want)
		}
	}
	for src, want := range map[string]string{
		"minSdk 21":                                  "21",
		"minSdk = 24":                                "24",
		"minSdkVersion 19":                           "19",
		"minSdk { version = release(23) }":           "23",
		"minSdkPreview 'Tiramisu'\nminSdk 21":        "21",
		"targetSdk 34":                               "",
		"minSdk = rootProject.ext.compileSdkVersion": "33",
	} {
		if got := sdkLevelOf(src, "minSdk", minSdkBlockRe, vars, cat); got != want {
			t.Errorf("minSdk(%q) = %q, want %q", src, got, want)
		}
	}
}

func TestIsAndroidApp(t *testing.T) {
	cat := &catalog{Plugins: map[string]string{"android.application": pluginApplication, "android.library": "com.android.library", "agp.app": pluginApplication}}
	cases := map[string]bool{
		"plugins { id 'com.android.application' }":                                                        true,
		`plugins { id("com.android.application") }`:                                                       true,
		"plugins {\n id 'com.android.application' version '8.10.0' apply false\n}":                        false,
		"plugins {\n alias(libs.plugins.android.application) apply false\n}":                              false,
		"plugins {\n alias(libs.plugins.android.application)\n}":                                          true,
		"plugins {\n alias(libs.plugins.agp.app)\n}":                                                      true,
		"plugins {\n alias(libs.plugins.android.library)\n}":                                              false,
		"apply plugin: 'com.android.application'":                                                         true,
		`apply(plugin = "com.android.application")`:                                                       true,
		"apply plugin: 'com.android.library'":                                                             false,
		"plugins { id(\"my.convention.app\") }\nandroid {\n defaultConfig { applicationId = \"a.b\" }\n}": true,
		"plugins { id(\"my.convention.lib\") }\nandroid {\n namespace = \"a.b\"\n}":                       false,
	}
	for src, want := range cases {
		if got := isAndroidApp(stripComments(src), cat); got != want {
			t.Errorf("isAndroidApp(%q) = %v, want %v", src, got, want)
		}
	}
	// 注释掉的插件不算
	if isAndroidApp(stripComments("plugins {\n // id 'com.android.application'\n}"), cat) {
		t.Error("commented plugin counted")
	}
}

func TestTopEntries(t *testing.T) {
	body := `
        debug { applicationIdSuffix ".d" }
        getByName("release") {
            isMinifyEnabled = true
            proguardFiles(getDefaultProguardFile("x"), "y")
        }
        create("qa") { initWith(getByName("debug")) }
        maybeCreate("perf")
        all { println("x") }
        'quoted' { }
        configureEach { }
`
	var names []string
	for _, e := range topEntries(body) {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "debug,release,qa,quoted" {
		t.Fatalf("entries %v", names)
	}
}

func TestNormSuffix(t *testing.T) {
	for in, want := range map[string]string{"": "", ".debug": ".debug", "debug": ".debug", ".free.debug": ".free.debug", "free.debug": ".free.debug", "..x": ".x"} {
		if got := normSuffix(in); got != want {
			t.Errorf("normSuffix(%q) = %q, want %q", in, got, want)
		}
	}
	if taskPath(":", "installDebug") != ":installDebug" || taskPath(":a:b", "installDebug") != ":a:b:installDebug" {
		t.Error("taskPath")
	}
}
