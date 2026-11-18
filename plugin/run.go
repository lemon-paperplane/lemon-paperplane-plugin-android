package android

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// runTasksRequest / runTasksResult 对应 gradle 插件的 runTasks 服务（见 lemon-paperplane-plugin-gradle/plugin/plugin.go）。
type runTasksRequest struct {
	Module     sdk.ModuleInfo           `json:"module"`
	Toolchains map[string]sdk.Toolchain `json:"toolchains,omitempty"`
	Env        map[string]string        `json:"env,omitempty"`
	RunID      string                   `json:"runId,omitempty"`
	Title      string                   `json:"title,omitempty"`
	Tasks      []string                 `json:"tasks"`
	Args       []string                 `json:"args,omitempty"`
}

type runTasksResult struct {
	ExitCode  int  `json:"exitCode"`
	Cancelled bool `json:"cancelled,omitempty"`
}

// 模拟器开机最多等多久。
const bootTimeout = 6 * time.Minute

// PrepareRun：选设备（必要时启动模拟器并等开机）→ gradle install<Variant> → 返回插件运行器。
func (p *Plugin) PrepareRun(ctx *sdk.Context, rp *sdk.PrepareRunParams) (*sdk.LaunchSpec, error) {
	if rp.Config.Type != typeApp {
		return nil, sdk.ErrNotSupported
	}
	opts := rp.Config.Options
	m := buildModel(&rp.Module, rp.Env)
	app := m.findApp(sdk.OptString(opts, optModule, ""))
	if app == nil {
		return nil, sdk.UserError("%s", ctx.T("err.noApp", sdk.OptString(opts, optModule, "")))
	}
	if !m.SDK.Exists {
		return nil, sdk.UserError("%s", ctx.T("err.noSdk", m.SDK.Dir))
	}
	adb := adbPath(m.SDK.Dir)
	if adb == "" {
		return nil, sdk.UserError("%s", ctx.T("err.noAdb", m.SDK.Dir))
	}
	vname := sdk.OptString(opts, optVar, "debug")
	v, ok := app.findVariant(vname)
	if !ok {
		if len(app.Flavors) > 0 || !strings.EqualFold(vname, "debug") {
			return nil, sdk.UserError("%s", ctx.T("err.unknownVariant", vname, strings.Join(app.Variants, ", ")))
		}
		v = Variant{Name: "debug", BuildType: "debug"}
	}

	// 1. 设备
	cl := newADB(adb, rp.Env)
	devs, err := cl.devices(ctx)
	if err != nil {
		return nil, sdk.UserError("%s", ctx.T("err.adb", err.Error()))
	}
	avds := listAVDs(avdHome(rp.Env))
	sel, serr := chooseDevice(sdk.OptString(opts, optDevice, ""), devs, avds)
	if serr != nil {
		return nil, sdk.UserError("%s", selectErrorText(ctx.T, serr))
	}
	serial := sel.Serial
	switch {
	case sel.StartAVD != "":
		serial, err = p.bootAVD(ctx, rp, m.SDK.Dir, sel.StartAVD, cl, devs)
		if err != nil {
			return nil, err
		}
	case sel.WaitBoot:
		if err := p.waitBoot(ctx, rp.RunID, cl, serial, sel.Label, nil, ""); err != nil {
			return nil, err
		}
	}
	label := sel.Label
	ctx.Host.Console(rp.RunID, ctx.T("run.device", label))
	p.saveLastDevice(ctx, rp.Config.ID, serial)

	// 2. 构建并安装（gradle 插件负责 JDK、镜像、包装器、守护进程）
	task := taskPath(app.Path, "install"+capitalize(v.Name))
	genv := sdk.MergeEnv(rp.Env, map[string]string{"ANDROID_SERIAL": serial})
	if m.SDK.Source == srcDefault {
		// 没有 local.properties 也没设环境变量：把找到的 SDK 告诉 Gradle（两个变量都设，AGP 不允许两者不一致）
		genv["ANDROID_HOME"] = m.SDK.Dir
		genv["ANDROID_SDK_ROOT"] = m.SDK.Dir
	}
	var res runTasksResult
	if err := ctx.Host.Call(ctx, "gradle", "runTasks", runTasksRequest{
		Module: rp.Module, Toolchains: rp.Toolchains, Env: genv, RunID: rp.RunID,
		Title: ctx.T("run.installTitle", v.Name, label), Tasks: []string{task},
	}, &res); err != nil {
		return nil, err
	}
	if res.Cancelled {
		return nil, sdk.NewError(sdk.ErrCodeCancelled, "cancelled")
	}
	if res.ExitCode != 0 {
		return nil, sdk.UserError("%s", ctx.T("err.installFailed", task, res.ExitCode))
	}

	// 3. 包名：优先读构建产物里的 output-metadata.json
	pkg := outputApplicationID(app, v)
	if pkg == "" {
		pkg = app.packageName(v)
	}
	if pkg == "" {
		return nil, sdk.UserError("%s", ctx.T("err.noPackage", app.Name))
	}

	// 4. 交给运行器：启动 → logcat → 停止时 force-stop
	pluginDir := ""
	if ctx.Init != nil {
		pluginDir = ctx.Init.PluginDir
	}
	args := runnerArgs{ADB: adb, Serial: serial, Package: pkg, Activity: sdk.OptString(opts, optAct, ""),
		ForceStop: sdk.OptBool(opts, optStop, true), Locale: ctx.Locale(), PluginDir: pluginDir}
	cmd, argv := sdk.RunnerCommand(args.argv()...)
	return &sdk.LaunchSpec{
		Command:       cmd,
		Args:          argv,
		Cwd:           app.Dir,
		ReadyPatterns: []string{readyPattern(ctx.T, pkg)},
		StopSignal:    "SIGTERM",
		StopTimeoutMs: 10000,
		Display:       fmt.Sprintf("adb -s %s · %s", serial, pkg),
	}, nil
}

// translator 是取文案的函数（ctx.T 或 Bundle.T），便于单测用真实文案。
type translator func(key string, args ...any) string

// readyPattern 生成运行器就绪行的正则（运行器用同一份文案打印，进程号位置换成 \d+）。
func readyPattern(t translator, pkg string) string {
	const mark = "\x00PID\x00"
	return strings.Replace(regexp.QuoteMeta(t("run.started", pkg, mark)), mark, `\d+`, 1)
}

// selectErrorText 把选设备失败映射成本地化文案（Diagnose 也按这些文案识别，见 deviceErrorKind）。
func selectErrorText(t translator, e *selectError) string {
	switch e.Kind {
	case selMultiple:
		return t("err.multipleDevices")
	case selChooseAVD:
		return t("err.chooseAvd")
	case selNotFound:
		return t("err.deviceNotFound", e.Device)
	case selAVDNotFound:
		return t("err.avdNotFound", e.Device)
	case selUnauthorized:
		return t("err.unauthorized", e.Device)
	case selOffline:
		return t("err.offline", e.Device)
	case selNoPermission:
		return t("err.noPermission", e.Device)
	}
	return t("err.noDevice")
}

// outputApplicationID 读 <模块>/build/outputs/apk/<风味>/<buildType>/output-metadata.json 的 applicationId。
func outputApplicationID(app *AppProject, v Variant) string {
	base := filepath.Join(app.Dir, "build", "outputs", "apk")
	var cands []string
	if len(v.Flavors) > 0 {
		cands = append(cands, filepath.Join(base, variantName(v.Flavors, ""), v.BuildType))
	} else {
		cands = append(cands, filepath.Join(base, v.BuildType))
	}
	cands = append(cands, filepath.Join(base, v.Name))
	for _, d := range cands {
		var meta struct {
			ApplicationID string `json:"applicationId"`
		}
		if sdk.ReadJSON(filepath.Join(d, "output-metadata.json"), &meta) == nil && meta.ApplicationID != "" {
			return meta.ApplicationID
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 模拟器

// bootAVD 启动 AVD 并等它开机完成，返回序列号。模拟器带窗口，运行停止后也不关（和 Android Studio 一样）。
func (p *Plugin) bootAVD(ctx *sdk.Context, rp *sdk.PrepareRunParams, sdkDir, name string, cl *adbClient, before []Device) (string, error) {
	emuSDK := sdkDir
	emu := emulatorPath(sdkDir)
	if emu == "" {
		// 工程用的 SDK 里没装模拟器：找别的 SDK（AVD 是用户级的，系统镜像跟着模拟器所在的 SDK 找）
		for _, d := range append([]string{lookupEnv(rp.Env, srcEnvHome), lookupEnv(rp.Env, srcEnvRoot)}, defaultSDKDirs()...) {
			if d != "" {
				if e := emulatorPath(d); e != "" {
					emu, emuSDK = e, d
					break
				}
			}
		}
	}
	if emu == "" {
		return "", sdk.UserError("%s", ctx.T("err.noEmulator", sdkDir))
	}
	extra := sdk.SplitArgs(settingString(ctx, rp.Settings, "emulatorArgs"))
	env := environ(sdk.MergeEnv(rp.Env, map[string]string{"ANDROID_HOME": emuSDK, "ANDROID_SDK_ROOT": emuSDK}))
	logFile := filepath.Join(p.cacheDir(ctx), "emulator", sdk.StableID(name)+".log")
	// 一条消息发出（两条通知在内核里可能乱序）
	ctx.Host.Console(rp.RunID, ctx.T("run.startingEmulator", name)+"\n$ "+strings.Join(append([]string{emu, "-avd", name}, extra...), " "))
	exited, err := startEmulator(emu, name, extra, env, logFile)
	if err != nil {
		return "", sdk.UserError("%s", ctx.T("err.emulatorStart", err.Error()))
	}
	known := map[string]bool{}
	for _, d := range before {
		known[d.Serial] = true
	}
	serial := ""
	start := time.Now()
	lastNote := start
	for serial == "" {
		select {
		case <-ctx.Done():
			return "", sdk.NewError(sdk.ErrCodeCancelled, "cancelled")
		case <-exited:
			return "", sdk.UserError("%s", ctx.T("err.emulatorExited", tailFile(logFile, 15)))
		case <-time.After(1500 * time.Millisecond):
		}
		devs, _ := cl.devices(ctx)
		for _, d := range devs {
			if d.Emulator() && (d.AVD == name || (!known[d.Serial] && d.AVD == "")) {
				serial = d.Serial
				break
			}
		}
		if time.Since(start) > bootTimeout {
			return "", sdk.UserError("%s", ctx.T("err.bootTimeout", name, int(bootTimeout.Seconds())))
		}
		if time.Since(lastNote) >= 15*time.Second {
			lastNote = time.Now()
			ctx.Host.Console(rp.RunID, ctx.T("run.waitingBoot", int(time.Since(start).Seconds())))
		}
	}
	if err := p.waitBoot(ctx, rp.RunID, cl, serial, name, exited, logFile); err != nil {
		return "", err
	}
	return serial, nil
}

// waitBoot 等设备开机完成（sys.boot_completed=1）且包管理服务可用。
func (p *Plugin) waitBoot(ctx *sdk.Context, runID string, cl *adbClient, serial, label string, exited <-chan struct{}, logFile string) error {
	ctx.Host.Console(runID, ctx.T("run.waitBoot", label))
	start := time.Now()
	lastNote := start
	for {
		if cl.bootCompleted(ctx, serial) {
			out, _, code, err := cl.run(ctx, 10*time.Second, "-s", serial, "shell", "pm", "path", "android")
			if err == nil && code == 0 && strings.Contains(out, "package:") {
				ctx.Host.Console(runID, ctx.T("run.booted", label, int(time.Since(start).Seconds())))
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return sdk.NewError(sdk.ErrCodeCancelled, "cancelled")
		case <-exited:
			return sdk.UserError("%s", ctx.T("err.emulatorExited", tailFile(logFile, 15)))
		case <-time.After(2 * time.Second):
		}
		if time.Since(start) > bootTimeout {
			return sdk.UserError("%s", ctx.T("err.bootTimeout", label, int(bootTimeout.Seconds())))
		}
		if time.Since(lastNote) >= 15*time.Second {
			lastNote = time.Now()
			ctx.Host.Console(runID, ctx.T("run.waitingBoot", int(time.Since(start).Seconds())))
		}
	}
}

// settingString 读插件设置：先看本次请求带的设置，再看初始化时的。
func settingString(ctx *sdk.Context, settings map[string]any, key string) string {
	if v, ok := settings[key].(string); ok {
		return v
	}
	if v, ok := ctx.Setting(key, "").(string); ok {
		return v
	}
	return ""
}

// ---------------------------------------------------------------------------
// 记住每个运行配置上次用的设备（卸载旧版修复要知道装到了哪台）

func (p *Plugin) lastDeviceFile(ctx *sdk.Context) string {
	dir := os.TempDir()
	if ctx.Init != nil && ctx.Init.DataDir != "" {
		dir = ctx.Init.DataDir
	}
	return filepath.Join(dir, "last-device.json")
}

func (p *Plugin) saveLastDevice(ctx *sdk.Context, configID, serial string) {
	if configID == "" || serial == "" {
		return
	}
	m := map[string]string{}
	f := p.lastDeviceFile(ctx)
	_ = sdk.ReadJSON(f, &m)
	m[configID] = serial
	if b, err := json.Marshal(m); err == nil {
		_ = os.MkdirAll(filepath.Dir(f), 0o755)
		_ = os.WriteFile(f, b, 0o644)
	}
}

func (p *Plugin) lastDevice(ctx *sdk.Context, configID string) string {
	m := map[string]string{}
	_ = sdk.ReadJSON(p.lastDeviceFile(ctx), &m)
	return m[configID]
}

// ---------------------------------------------------------------------------
// 运行器参数

// runnerArgs 是 PrepareRun 交给运行器的参数（命令行）。
type runnerArgs struct {
	ADB       string
	Serial    string
	Package   string
	Activity  string
	ForceStop bool
	Locale    string
	PluginDir string
}

func (a runnerArgs) argv() []string {
	return []string{"--adb", a.ADB, "--serial", a.Serial, "--package", a.Package, "--activity", a.Activity,
		"--force-stop=" + strconv.FormatBool(a.ForceStop), "--locale", a.Locale, "--plugin-dir", a.PluginDir}
}
