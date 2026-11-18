package android

import (
	"context"
	"regexp"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// devOptionsURL 是官方「开启开发者选项和 USB 调试」说明。
const devOptionsURL = "https://developer.android.com/studio/debug/dev-options"

// Diagnose 补充声明式规则做不到的诊断：
//   - 选设备失败：每台设备 / 每个模拟器给一个「用 X 运行」（setRunOption device）；
//   - SDK location not found：找到了 SDK 就设 ANDROID_HOME，找不到就下载安装；
//   - 应用崩溃：取出异常与堆栈；
//   - Gradle 构建失败：把 What went wrong 原文（如 settings.gradle 抛出的 GradleException）显示出来。
func (p *Plugin) Diagnose(ctx *sdk.Context, d *sdk.DiagnoseParams) (*sdk.DiagnoseResult, error) {
	if !d.Module.HasFacet(facetID) {
		return &sdk.DiagnoseResult{}, nil
	}
	var out []sdk.Diagnosis
	if d.Stage == "run" {
		if dg := p.deviceDiagnosis(ctx, d); dg != nil {
			out = append(out, *dg)
		}
		if dg := crashDiagnosis(ctx.T, d.LogTail); dg != nil {
			out = append(out, *dg)
		}
	}
	if anyContains(d.LogTail, "SDK location not found") {
		out = append(out, sdkLocationDiagnosis(ctx, d.Module.Dir))
	}
	if gf := parseGradleFailure(d.LogTail); gf != nil && (gf.scriptError() || (len(d.Matched) == 0 && len(out) == 0)) {
		out = append(out, gf.diagnosis(ctx))
	}
	return &sdk.DiagnoseResult{Diagnoses: out}, nil
}

// deviceDiagnosis 识别 PrepareRun 的选设备错误（同一插件、同一语言的文案），给出可选设备。
func (p *Plugin) deviceDiagnosis(ctx *sdk.Context, d *sdk.DiagnoseParams) *sdk.Diagnosis {
	kind := deviceErrorKind(d.LogTail, ctx.T)
	if kind == "" {
		return nil
	}
	m := buildModel(&d.Module, nil)
	var devs []Device
	if adb := adbPath(m.SDK.Dir); adb != "" {
		c, cancel := context.WithTimeout(ctx, 8*time.Second)
		devs, _ = newADB(adb, nil).devices(c)
		cancel()
	}
	avds := listAVDs(avdHome(nil))
	current := ""
	if d.Config != nil {
		current = sdk.OptString(d.Config.Options, optDevice, "")
	}
	dg := &sdk.Diagnosis{RuleID: "android.device", Severity: "error", Title: ctx.T("diag.chooseDevice.title"), Message: ctx.T("diag.chooseDevice.message")}
	switch kind {
	case selUnauthorized, selOffline, selNoPermission:
		dg.Title = ctx.T("diag.unauthorized.title")
		dg.Message = ctx.T("diag.unauthorized.message")
		dg.Fixes = append(dg.Fixes, sdk.Fix{ID: "retry", Title: ctx.T("fix.retry"), Kind: sdk.FixRestart, Recommended: true})
	case selNoDevice:
		dg.Title = ctx.T("diag.noDevice.title")
		dg.Message = ctx.T("diag.noDevice.message")
	}
	dg.Fixes = append(dg.Fixes, deviceFixes(ctx, devs, avds, current)...)
	if kind == selNoDevice || len(dg.Fixes) == 0 {
		dg.Fixes = append(dg.Fixes, sdk.Fix{ID: "devOptions", Title: ctx.T("fix.devOptions"), Kind: sdk.FixOpenURL, Params: map[string]string{"url": devOptionsURL}})
	}
	if len(dg.Fixes) > 0 && !hasRecommended(dg.Fixes) {
		dg.Fixes[0].Recommended = true
	}
	return dg
}

// deviceErrorKind 在日志里找 PrepareRun 的选设备错误（selectErrorText 生成的文案），返回最后一个命中的种类。
func deviceErrorKind(lines []string, t translator) string {
	kind := ""
	for _, line := range lines {
		switch {
		case strings.Contains(line, t("err.noDevice")):
			kind = selNoDevice
		case strings.Contains(line, t("err.multipleDevices")):
			kind = selMultiple
		case strings.Contains(line, t("err.chooseAvd")):
			kind = selChooseAVD
		case templateMatch(line, t("err.deviceNotFound")):
			kind = selNotFound
		case templateMatch(line, t("err.avdNotFound")):
			kind = selAVDNotFound
		case templateMatch(line, t("err.unauthorized")):
			kind = selUnauthorized
		case templateMatch(line, t("err.offline")):
			kind = selOffline
		case templateMatch(line, t("err.noPermission")):
			kind = selNoPermission
		case strings.Contains(line, "No connected devices!"):
			kind = selNotFound
		}
	}
	return kind
}

// deviceFixes 为每台在线设备、每个 AVD 生成「用 X 运行」。模拟器用 avd:名字（序列号每次启动会变）。
func deviceFixes(ctx *sdk.Context, devs []Device, avds []AVD, current string) []sdk.Fix {
	var fixes []sdk.Fix
	seen := map[string]bool{}
	add := func(value, label string) {
		if value == current || seen[value] {
			return
		}
		seen[value] = true
		fixes = append(fixes, sdk.Fix{ID: "device:" + value, Title: ctx.T("fix.runOn", label), Kind: sdk.FixSetRunOption,
			Params: map[string]string{"key": optDevice, "value": value}})
	}
	for _, d := range devs {
		if !d.Online() {
			continue
		}
		if d.Emulator() && d.AVD != "" {
			add(avdOptionPrefix+d.AVD, d.AVD)
		} else {
			add(d.Serial, d.Label())
		}
	}
	for _, a := range avds {
		add(avdOptionPrefix+a.Name, ctx.T("device.avd", firstNonEmpty(a.Display, a.Name)))
	}
	return fixes
}

func hasRecommended(fs []sdk.Fix) bool {
	for _, f := range fs {
		if f.Recommended {
			return true
		}
	}
	return false
}

// templateMatch 判断一行是否由带 {0} 占位的文案生成（占位两侧的固定文字都在、且按顺序）。
func templateMatch(line, tpl string) bool {
	before, after, ok := strings.Cut(tpl, "{0}")
	if !ok {
		return strings.Contains(line, tpl)
	}
	i := strings.Index(line, before)
	if i < 0 {
		return false
	}
	return after == "" || strings.Contains(line[i+len(before):], after)
}

func anyContains(lines []string, s string) bool {
	for _, l := range lines {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// sdkLocationDiagnosis：Gradle 找不到 SDK。本机找得到就设 ANDROID_HOME（不改 local.properties），否则下载安装。
func sdkLocationDiagnosis(ctx *sdk.Context, root string) sdk.Diagnosis {
	loc := locateSDK(root, nil)
	d := sdk.Diagnosis{RuleID: "android.sdk-location", Severity: "error", Title: ctx.T("diag.sdkLocation.title")}
	if loc.Exists {
		d.Message = ctx.T("diag.sdkLocation.found", loc.Dir)
		d.Fixes = []sdk.Fix{{ID: "useSdk", Title: ctx.T("fix.setAndroidHome"), Kind: sdk.FixSetEnv, Recommended: true,
			Params: map[string]string{"key": "ANDROID_HOME", "value": loc.Dir, "scope": "project"}}}
		return d
	}
	d.Message = ctx.T("diag.sdkLocation.missing", loc.Dir)
	d.Fixes = []sdk.Fix{{ID: "installSdk", Title: ctx.T("fix.installSdk"), Kind: sdk.FixCustom, Recommended: true,
		Params: map[string]string{"sdk": loc.Dir}, Confirm: installConfirm(ctx, "Android SDK → "+loc.Dir)}}
	return d
}

// ---------------------------------------------------------------------------
// 崩溃

var exceptionLineRe = regexp.MustCompile(`(?:^|\s)((?:[a-z][\w$]*\.)+[A-Z][\w$]*(?:Exception|Error)(?::.*)?)$`)

// crashDiagnosis 从日志尾部找 FATAL EXCEPTION，取出异常行与堆栈。
func crashDiagnosis(t translator, lines []string) *sdk.Diagnosis {
	start := -1
	for i, l := range lines {
		if strings.Contains(l, "FATAL EXCEPTION") {
			start = i
		}
	}
	if start < 0 {
		return nil
	}
	var detail []string
	exc, cause := "", ""
	for i, l := range lines[start:] {
		msg := strings.TrimSpace(logcatMessage(l))
		if i > 0 && strings.Contains(l, "FATAL EXCEPTION") {
			break
		}
		if len(detail) < 40 {
			detail = append(detail, logcatMessage(l))
		}
		if strings.HasPrefix(msg, "Caused by: ") {
			if m := exceptionLineRe.FindStringSubmatch(strings.TrimPrefix(msg, "Caused by: ")); m != nil {
				cause = m[1] // 最后一个 Caused by 是根因
			}
		} else if exc == "" {
			if m := exceptionLineRe.FindStringSubmatch(msg); m != nil {
				exc = m[1]
			}
		}
	}
	if cause != "" {
		exc = cause
	}
	title := t("diag.crash.title")
	if exc != "" {
		title = t("diag.crash.titleWith", exc)
	}
	return &sdk.Diagnosis{RuleID: "android.crash", Severity: "error", Title: title, Message: t("diag.crash.message"), Detail: strings.Join(detail, "\n"), Line: lines[start]}
}

// logcatMessage 去掉 threadtime 格式的前缀（日期 时间 pid tid 级别 标签:），只留消息。
var threadtimeRe = regexp.MustCompile(`^\d\d-\d\d\s+\d\d:\d\d:\d\d\.\d+\s+\d+\s+\d+\s+[VDIWEFA]\s+[^:]*:\s?`)

func logcatMessage(l string) string { return threadtimeRe.ReplaceAllString(l, "") }

// ---------------------------------------------------------------------------
// Gradle 失败摘要

type gradleFailure struct {
	Where []string // * Where: 段
	What  []string // * What went wrong: 段
}

// parseGradleFailure 从日志里取 Gradle 的 * Where / * What went wrong 段（只取第一个失败）。
func parseGradleFailure(lines []string) *gradleFailure {
	var gf *gradleFailure
	section := ""
	for _, raw := range lines {
		l := strings.TrimRight(raw, " \r")
		t := strings.TrimSpace(l)
		switch {
		case t == "* Where:":
			if gf != nil && len(gf.What) > 0 {
				return gf
			}
			if gf == nil {
				gf = &gradleFailure{}
			}
			section = "where"
			continue
		case t == "* What went wrong:":
			if gf != nil && len(gf.What) > 0 {
				return gf
			}
			if gf == nil {
				gf = &gradleFailure{}
			}
			section = "what"
			continue
		case strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "BUILD FAILED") || strings.HasPrefix(t, "FAILURE:"):
			section = ""
			continue
		}
		switch section {
		case "where":
			if t != "" {
				gf.Where = append(gf.Where, t)
			}
		case "what":
			if t == "" && len(gf.What) > 0 {
				section = ""
				continue
			}
			if t != "" {
				gf.What = append(gf.What, l)
			}
		}
	}
	if gf == nil || len(gf.What) == 0 {
		return nil
	}
	return gf
}

var (
	whereRe      = regexp.MustCompile(`^(?:Settings|Build|Script|Initialization script) file '([^']+)' line: (\d+)`)
	excPrefixRe  = regexp.MustCompile(`^(?:(?:[a-z][\w$]*\.)+[A-Z][\w$]*(?:Exception|Error): )+`)
	evalFailedRe = regexp.MustCompile(`^(?:A problem occurred (?:evaluating|configuring) (?:settings|project|root project|script)|Could not compile (?:settings|build) file)`)
)

// scriptError：构建脚本（settings.gradle / build.gradle）求值时抛的错，原文就是给人看的提示。
func (g *gradleFailure) scriptError() bool {
	for _, w := range g.Where {
		if whereRe.MatchString(w) {
			return true
		}
	}
	return len(g.What) > 0 && evalFailedRe.MatchString(strings.TrimSpace(g.What[0]))
}

// message 取最具体的一句：最后一个 "> " 开头的行（去掉异常类名前缀）；没有就是第一行。
func (g *gradleFailure) message() string {
	msg := ""
	for _, l := range g.What {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, ">") {
			msg = strings.TrimSpace(strings.TrimPrefix(t, ">"))
		}
	}
	if msg == "" && len(g.What) > 0 {
		msg = strings.TrimSpace(g.What[0])
	}
	if s := excPrefixRe.ReplaceAllString(msg, ""); s != "" {
		msg = s
	}
	return msg
}

func (g *gradleFailure) diagnosis(ctx *sdk.Context) sdk.Diagnosis {
	d := sdk.Diagnosis{RuleID: "android.gradle-failure", Severity: "error", Title: ctx.T("diag.gradle.title", g.message()), Detail: strings.Join(g.What, "\n")}
	for _, w := range g.Where {
		if m := whereRe.FindStringSubmatch(w); m != nil {
			d.Message = ctx.T("diag.gradle.where", m[1], m[2])
			break
		}
	}
	if d.Message == "" && len(g.Where) > 0 {
		d.Message = strings.Join(g.Where, "\n")
	}
	return d
}

// ---------------------------------------------------------------------------
// 修复

// ApplyFix 执行 custom 修复：装 SDK / 包 / cmdline-tools、接受许可、卸载旧版。
func (p *Plugin) ApplyFix(ctx *sdk.Context, f *sdk.ApplyFixParams) (*sdk.ApplyFixResult, error) {
	m := buildModel(&f.Module, f.Env)
	sdkDir := firstNonEmpty(f.Fix.Params["sdk"], m.SDK.Dir)
	switch f.Fix.ID {
	case "installSdk":
		pkgs := []string{"platform-tools"}
		for _, pk := range missingPackages(sdkDir, m.Projects) {
			if !contains(pkgs, pk) {
				pkgs = append(pkgs, pk)
			}
		}
		if err := p.installPackages(ctx, sdkDir, pkgs, f.Toolchains, f.Env, f.TaskID); err != nil {
			return nil, err
		}
		res := &sdk.ApplyFixResult{Rerun: true, Message: ctx.T("msg.sdkInstalled", sdkDir), Changes: &sdk.ProjectChanges{Reimport: true}}
		if m.SDK.Source == "" || m.SDK.Source == srcDefault {
			// Gradle 自己不会去默认位置找：记到工程环境变量里（不改 local.properties）
			v := sdkDir
			res.Changes.Env = map[string]*string{"ANDROID_HOME": &v}
		}
		return res, nil
	case "installPackages":
		var pkgs []string
		for _, s := range strings.Split(f.Fix.Params["packages"], ",") {
			if s = strings.TrimSpace(s); s != "" {
				pkgs = append(pkgs, s)
			}
		}
		if len(pkgs) == 0 {
			return nil, sdk.UserError("%s", ctx.T("err.noPackagesToInstall"))
		}
		if err := p.installPackages(ctx, sdkDir, pkgs, f.Toolchains, f.Env, f.TaskID); err != nil {
			return nil, err
		}
		return &sdk.ApplyFixResult{Rerun: true, Message: ctx.T("msg.packagesInstalled", strings.Join(pkgs, ", ")), Changes: &sdk.ProjectChanges{Reimport: true}}, nil
	case "installNdk":
		var pkgs []string
		for _, pr := range m.Projects {
			if pr.NdkVersion != "" && pr.NdkPath == "" && !contains(pkgs, "ndk;"+pr.NdkVersion) {
				pkgs = append(pkgs, "ndk;"+pr.NdkVersion)
			}
		}
		if len(pkgs) == 0 {
			return nil, sdk.UserError("%s", ctx.T("err.ndkVersionUnknown"))
		}
		if err := p.installPackages(ctx, sdkDir, pkgs, f.Toolchains, f.Env, f.TaskID); err != nil {
			return nil, err
		}
		return &sdk.ApplyFixResult{Rerun: true, Message: ctx.T("msg.packagesInstalled", strings.Join(pkgs, ", "))}, nil
	case "installCmdlineTools":
		if err := p.installCmdlineTools(ctx, sdkDir, f.TaskID); err != nil {
			return nil, err
		}
		return &sdk.ApplyFixResult{Message: ctx.T("msg.cmdlineInstalled"), Changes: &sdk.ProjectChanges{Reimport: true}}, nil
	case "acceptLicenses":
		if err := p.acceptLicenses(ctx, sdkDir, f.Toolchains, f.Env, f.TaskID); err != nil {
			return nil, err
		}
		return &sdk.ApplyFixResult{Rerun: true, Message: ctx.T("msg.licensesAccepted")}, nil
	case "uninstall":
		return p.uninstall(ctx, f, m)
	}
	return nil, sdk.ErrNotSupported
}

// uninstall 卸载设备上的旧版（签名不一致 / 版本降级时）。设备取这个运行配置上次安装用的那台。
func (p *Plugin) uninstall(ctx *sdk.Context, f *sdk.ApplyFixParams, m *Model) (*sdk.ApplyFixResult, error) {
	adb := adbPath(m.SDK.Dir)
	if adb == "" {
		return nil, sdk.UserError("%s", ctx.T("err.noAdb", m.SDK.Dir))
	}
	pkg := strings.TrimSpace(f.Fix.Params["package"])
	serial := ""
	if f.Config != nil {
		if pkg == "" {
			if app := m.findApp(sdk.OptString(f.Config.Options, optModule, "")); app != nil {
				if v, ok := app.findVariant(sdk.OptString(f.Config.Options, optVar, "debug")); ok {
					pkg = firstNonEmpty(outputApplicationID(app, v), app.packageName(v))
				}
			}
		}
		serial = p.lastDevice(ctx, f.Config.ID)
		if serial == "" {
			if dv := sdk.OptString(f.Config.Options, optDevice, ""); dv != "" && !strings.HasPrefix(dv, avdOptionPrefix) {
				serial = dv
			}
		}
	}
	if pkg == "" {
		if app := defaultApp(m.apps()); app != nil {
			pkg = app.packageName(Variant{Name: "debug", BuildType: "debug"})
		}
	}
	if pkg == "" {
		return nil, sdk.UserError("%s", ctx.T("err.noPackage", ""))
	}
	if serial == "" {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		devs, _ := newADB(adb, f.Env).devices(c)
		cancel()
		var online []Device
		for _, d := range devs {
			if d.Online() {
				online = append(online, d)
			}
		}
		if len(online) != 1 {
			return nil, sdk.UserError("%s", ctx.T("err.uninstallWhichDevice"))
		}
		serial = online[0].Serial
	}
	r, err := ctx.Host.Exec(ctx, &sdk.ExecParams{Command: adb, Args: []string{"-s", serial, "uninstall", pkg}, Env: f.Env, Console: f.TaskID, Capture: f.TaskID == "",
		Title: ctx.T("fix.uninstall.title", pkg, serial)})
	if err != nil {
		return nil, err
	}
	if r.ExitCode != 0 {
		return nil, sdk.UserError("%s", ctx.T("err.uninstallFailed", pkg, strings.TrimSpace(r.Stderr+r.Stdout)))
	}
	return &sdk.ApplyFixResult{Rerun: true, Message: ctx.T("msg.uninstalled", pkg)}, nil
}
