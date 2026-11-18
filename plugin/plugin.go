// Package android 是 Lemon Paperplane 的安卓插件：在 Gradle 工程里识别安卓应用子工程，
// 静态解析 build.gradle(.kts)，检查 Android SDK，选设备 / 启动模拟器，经 gradle 插件安装，
// 再由插件自带的运行器启动应用、跟踪 logcat、停止时 force-stop。
package android

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

const (
	pluginID  = "android"
	facetID   = "android"
	typeApp   = "android-app"
	optModule = "module"
	optVar    = "variant"
	optDevice = "device"
	optAct    = "activity"
	optStop   = "forceStop"

	// 分析时的动态选项
	ovModules  = "android.modules"
	ovVariants = "android.variants"
	ovDevices  = "android.devices"
)

// Plugin 实现安卓插件。
type Plugin struct{}

// New 返回插件实例。
func New() any { return &Plugin{} }

// Model 是安卓插件的模型（AnalyzeResult.Model）。
type Model struct {
	SDK      sdkLoc        `json:"sdk"`
	Projects []*AppProject `json:"projects"` // 全部安卓子工程（应用与库）
	Devices  []Device      `json:"devices,omitempty"`
	AVDs     []AVD         `json:"avds,omitempty"`
}

// apps 返回应用子工程。
func (m *Model) apps() []*AppProject {
	var out []*AppProject
	for _, p := range m.Projects {
		if p.App {
			out = append(out, p)
		}
	}
	return out
}

// findApp 按运行配置的 module 选项找应用：Gradle 路径、名字或目录；空则取默认应用。
func (m *Model) findApp(v string) *AppProject {
	apps := m.apps()
	if v == "" {
		return defaultApp(apps)
	}
	for _, a := range apps {
		if a.Path == v || a.Path == ":"+strings.TrimPrefix(v, ":") || a.Name == v || a.RelDir == filepath.ToSlash(v) || a.Dir == v {
			return a
		}
	}
	return nil
}

// defaultApp：名为 app 的优先，否则第一个。
func defaultApp(apps []*AppProject) *AppProject {
	for _, a := range apps {
		if a.Name == "app" {
			return a
		}
	}
	if len(apps) > 0 {
		return apps[0]
	}
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// ---------------------------------------------------------------------------
// 识别

// Detect 由 gradle facet 激活：Gradle 根下有应用了 com.android.application 的子工程时加 android facet。
func (p *Plugin) Detect(ctx *sdk.Context, d *sdk.DetectParams) (*sdk.DetectResult, error) {
	var gf *sdk.Facet
	for i := range d.Facets {
		if d.Facets[i].ID == "gradle" {
			gf = &d.Facets[i]
		}
	}
	if gf == nil {
		return &sdk.DetectResult{}, nil
	}
	apps := detectApps(d.Dir, facetProjects(gf, d.Dir))
	if len(apps) == 0 {
		return &sdk.DetectResult{}, nil
	}
	return &sdk.DetectResult{Facets: []sdk.Facet{{ID: facetID, Data: map[string]any{"apps": apps}}}}, nil
}

// detectApps 返回应用子工程的 Gradle 路径（只读构建文件，不跑命令）。
func detectApps(root string, gps []gradleProject) []string {
	cat := parseCatalog(filepath.Join(root, "gradle", "libs.versions.toml"))
	var apps []string
	for _, gp := range gps {
		if gp.BuildFile == "" {
			continue
		}
		if isAndroidApp(readScriptWithIncludes(gp.BuildFile), cat) {
			apps = append(apps, gp.Path)
		}
	}
	return apps
}

// ---------------------------------------------------------------------------
// 分析

// buildModel 静态解析模块（Gradle 根）下的安卓子工程并定位 SDK。
func buildModel(m *sdk.ModuleInfo, env map[string]string) *Model {
	return &Model{
		SDK:      locateSDK(m.Dir, env),
		Projects: scanProjects(m.Dir, gradleProjects(m)),
	}
}

// Analyze 静态分析：子工程配置、SDK 与缺失的包、设备列表、运行配置。带 gradle facet 但不是安卓工程的模块返回空结果。
func (p *Plugin) Analyze(ctx *sdk.Context, a *sdk.AnalyzeParams) (*sdk.AnalyzeResult, error) {
	if !a.Module.HasFacet(facetID) {
		return &sdk.AnalyzeResult{}, nil
	}
	m := buildModel(&a.Module, a.Env)
	apps := m.apps()
	if m.SDK.Exists {
		if adb := adbPath(m.SDK.Dir); adb != "" {
			c, cancel := context.WithTimeout(ctx, 8*time.Second)
			if devs, err := newADB(adb, a.Env).devices(c); err == nil {
				m.Devices = devs
			}
			cancel()
		}
	}
	m.AVDs = listAVDs(avdHome(a.Env))
	res := &sdk.AnalyzeResult{Model: mustJSON(m)}
	def := defaultApp(apps)
	for _, app := range apps {
		res.RunConfigurations = append(res.RunConfigurations, sdk.RunConfiguration{
			ID:      sdk.StableID(typeApp, app.Path),
			Name:    app.Name,
			Type:    typeApp,
			Options: map[string]any{optModule: app.Path, optVar: "debug", optDevice: "", optAct: "", optStop: true},
			Default: app == def,
			Source:  "auto",
		})
	}
	if def != nil {
		if id := firstNonEmpty(def.ApplicationID, def.Namespace); id != "" {
			res.Info = append(res.Info, sdk.InfoItem{Label: "applicationId", Value: id})
		}
		if def.MinSdk != "" || def.TargetSdk != "" {
			res.Info = append(res.Info, sdk.InfoItem{Label: "minSdk / targetSdk", Value: firstNonEmpty(def.MinSdk, "?") + " / " + firstNonEmpty(def.TargetSdk, "?")})
		}
	}
	if m.SDK.Exists {
		res.Info = append(res.Info, sdk.InfoItem{Label: "Android SDK", Value: shortPath(m.SDK.Dir)})
	}
	res.Problems = p.problems(ctx, m)
	res.OptionValues = optionValues(ctx, m)
	return res, nil
}

// optionValues 生成运行配置表单的动态选项。没有选项也输出 []（界面按数组解析）。
func optionValues(ctx *sdk.Context, m *Model) map[string][]sdk.Option {
	mods := []sdk.Option{}
	vars := []sdk.Option{}
	seen := map[string]bool{}
	for _, app := range m.apps() {
		mods = append(mods, sdk.Option{Value: app.Path, Label: app.Name})
		for _, v := range app.Variants {
			if !seen[v] {
				seen[v] = true
				vars = append(vars, sdk.Option{Value: v, Label: v})
			}
		}
	}
	return map[string][]sdk.Option{
		ovModules:  mods,
		ovVariants: vars,
		ovDevices:  deviceOptions(ctx, m.Devices, m.AVDs),
	}
}

// shortPath 把主目录换成 ~。
func shortPath(p string) string {
	home := sdk.HomeDir()
	if home != "" && strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}

// installConfirm 是所有安装类修复的确认文案（安装 SDK 组件即表示接受 Android SDK 许可协议）。
func installConfirm(ctx *sdk.Context, what string) string {
	return ctx.T("confirm.install", what)
}

// problems 是静态就能发现的问题：没有 SDK、缺平台 / build-tools / NDK / platform-tools / cmdline-tools。
func (p *Plugin) problems(ctx *sdk.Context, m *Model) []sdk.Diagnosis {
	var out []sdk.Diagnosis
	loc := m.SDK
	if !loc.Exists {
		d := sdk.Diagnosis{RuleID: "android.no-sdk", Severity: "error"}
		switch loc.Source {
		case srcLocalProps:
			d.Title = ctx.T("problem.sdkDirMissing.title", "local.properties sdk.dir")
			d.Message = ctx.T("problem.sdkDirMissing.message", loc.Dir)
		case srcEnvHome, srcEnvRoot:
			d.Title = ctx.T("problem.sdkDirMissing.title", loc.Source)
			d.Message = ctx.T("problem.sdkDirMissing.message", loc.Dir)
		default:
			d.Title = ctx.T("problem.noSdk.title")
			d.Message = ctx.T("problem.noSdk.message", loc.Dir)
		}
		d.Fixes = []sdk.Fix{{ID: "installSdk", Title: ctx.T("fix.installSdk"), Kind: sdk.FixCustom, Recommended: true,
			Params: map[string]string{"sdk": loc.Dir}, Confirm: installConfirm(ctx, "Android SDK → "+loc.Dir)}}
		return append(out, d)
	}
	if loc.Source == srcDefault {
		out = append(out, sdk.Diagnosis{RuleID: "android.sdk-not-configured", Severity: "warning",
			Title: ctx.T("problem.sdkNotConfigured.title"), Message: ctx.T("problem.sdkNotConfigured.message", loc.Dir),
			Fixes: []sdk.Fix{{ID: "useSdk", Title: ctx.T("fix.setAndroidHome"), Kind: sdk.FixSetEnv, Recommended: true,
				Params: map[string]string{"key": "ANDROID_HOME", "value": loc.Dir, "scope": "project"}}}})
	}
	if adbPath(loc.Dir) == "" {
		out = append(out, installProblem(ctx, "android.no-platform-tools", "error", ctx.T("problem.noAdb.title"), ctx.T("problem.noAdb.message"), loc.Dir, []string{"platform-tools"}))
	}
	for _, pkg := range missingPackages(loc.Dir, m.Projects) {
		kind, ver, _ := strings.Cut(pkg, ";")
		var title, msg string
		switch kind {
		case "platforms":
			title = ctx.T("problem.noPlatform.title", strings.TrimPrefix(ver, "android-"))
			msg = ctx.T("problem.noPlatform.message", whoNeeds(m.Projects, func(p *AppProject) bool { return platformPackage(p.CompileSdk) == pkg }))
		case "build-tools":
			title = ctx.T("problem.noBuildTools.title", ver)
			msg = ctx.T("problem.noBuildTools.message", whoNeeds(m.Projects, func(p *AppProject) bool { return p.BuildTools == ver }))
		case "ndk":
			title = ctx.T("problem.noNdk.title", ver)
			msg = ctx.T("problem.noNdk.message", whoNeeds(m.Projects, func(p *AppProject) bool { return p.NdkVersion == ver }))
		}
		out = append(out, installProblem(ctx, "android.missing-"+kind, "warning", title, msg, loc.Dir, []string{pkg}))
	}
	for _, pr := range m.Projects {
		if pr.NdkPath != "" && !sdk.DirExists(pr.NdkPath) {
			out = append(out, sdk.Diagnosis{RuleID: "android.ndk-path", Severity: "warning",
				Title: ctx.T("problem.ndkPath.title", pr.Name), Message: ctx.T("problem.ndkPath.message", pr.NdkPath)})
		}
	}
	if findSDKTool(loc.Dir) == nil {
		out = append(out, sdk.Diagnosis{RuleID: "android.no-cmdline-tools", Severity: "info",
			Title: ctx.T("problem.noCmdline.title"), Message: ctx.T("problem.noCmdline.message"),
			Fixes: []sdk.Fix{{ID: "installCmdlineTools", Title: ctx.T("fix.installCmdline"), Kind: sdk.FixCustom,
				Params: map[string]string{"sdk": loc.Dir}, Confirm: installConfirm(ctx, "Android SDK Command-line Tools")}}})
	}
	return out
}

// installProblem 生成一条「缺 SDK 包」问题，修复是用 cmdline-tools 安装。
func installProblem(ctx *sdk.Context, rule, severity, title, msg, sdkDir string, pkgs []string) sdk.Diagnosis {
	return sdk.Diagnosis{RuleID: rule, Severity: severity, Title: title, Message: msg,
		Fixes: []sdk.Fix{{ID: "installPackages", Title: ctx.T("fix.installPackages", strings.Join(pkgs, ", ")), Kind: sdk.FixCustom, Recommended: true,
			Params: map[string]string{"sdk": sdkDir, "packages": strings.Join(pkgs, ",")}, Confirm: installConfirm(ctx, strings.Join(pkgs, ", "))}}}
}

// whoNeeds 列出需要某个包的子工程名。
func whoNeeds(projects []*AppProject, f func(*AppProject) bool) string {
	var names []string
	for _, p := range projects {
		if f(p) {
			names = append(names, p.Name)
		}
	}
	return strings.Join(names, ", ")
}

// Options 实时提供下拉选项（options/list）：设备列表（在线设备 + AVD）。界面打开运行配置时及打开期间定期调用，
// 只读 local.properties 找 SDK、跑一次 adb devices，不解析构建文件。
func (p *Plugin) Options(ctx *sdk.Context, in *sdk.OptionsParams) (*sdk.OptionsResult, error) {
	out := map[string][]sdk.Option{}
	for _, k := range in.Keys {
		if k != ovDevices {
			continue
		}
		var devs []Device
		if loc := locateSDK(in.Module.Dir, in.Env); loc.Exists {
			if adb := adbPath(loc.Dir); adb != "" {
				c, cancel := context.WithTimeout(ctx, 8*time.Second)
				if d, err := newADB(adb, in.Env).devices(c); err == nil {
					devs = d
				}
				cancel()
			}
		}
		out[k] = deviceOptions(ctx, devs, listAVDs(avdHome(in.Env)))
	}
	return &sdk.OptionsResult{Values: out}, nil
}
