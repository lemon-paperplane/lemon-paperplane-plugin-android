package android

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

const studioDownloadURL = "https://developer.android.com/studio"

// InvokeAction：在 Android Studio 中打开；构建签名包（assembleRelease / bundleRelease，在任务里跑，输出进任务控制台）。
func (p *Plugin) InvokeAction(ctx *sdk.Context, a *sdk.InvokeActionParams) (*sdk.InvokeActionResult, error) {
	switch a.Action {
	case "openInStudio":
		return p.openInStudio(ctx, a)
	case "buildRelease":
		return p.buildRelease(ctx, a)
	}
	return nil, sdk.ErrNotSupported
}

// studioCandidates 是各平台 Android Studio 的常见安装位置（含 JetBrains Toolbox）。
func studioCandidates(goos, home string, env func(string) string) []string {
	switch goos {
	case "darwin":
		var out []string
		for _, root := range []string{"/Applications", filepath.Join(home, "Applications"), filepath.Join(home, "Applications", "JetBrains Toolbox")} {
			for _, name := range []string{"Android Studio.app", "Android Studio Preview.app"} {
				out = append(out, filepath.Join(root, name))
			}
		}
		return out
	case "windows":
		var out []string
		for _, root := range []string{env("ProgramFiles"), env("ProgramFiles(x86)"), filepath.Join(env("LOCALAPPDATA"), "Programs")} {
			if root != "" {
				out = append(out, filepath.Join(root, "Android", "Android Studio", "bin", "studio64.exe"), filepath.Join(root, "Android Studio", "bin", "studio64.exe"))
			}
		}
		if la := env("LOCALAPPDATA"); la != "" {
			out = append(out, filepath.Join(la, "JetBrains", "Toolbox", "scripts", "studio.cmd"))
		}
		return out
	default:
		return []string{
			"/opt/android-studio/bin/studio.sh", "/usr/local/android-studio/bin/studio.sh",
			filepath.Join(home, "android-studio", "bin", "studio.sh"),
			filepath.Join(home, ".local", "share", "JetBrains", "Toolbox", "scripts", "studio"),
			"/snap/bin/android-studio",
		}
	}
}

// findStudio 返回本机的 Android Studio；macOS 返回 .app 路径。
func findStudio() string {
	for _, c := range studioCandidates(runtime.GOOS, sdk.HomeDir(), os.Getenv) {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		for _, n := range []string{"android-studio", "studio.sh", "studio"} {
			if p, err := exec.LookPath(n); err == nil {
				return p
			}
		}
	}
	return ""
}

func (p *Plugin) openInStudio(ctx *sdk.Context, a *sdk.InvokeActionParams) (*sdk.InvokeActionResult, error) {
	studio := findStudio()
	if studio == "" {
		return &sdk.InvokeActionResult{Message: ctx.T("msg.studioNotFound"), OpenURL: studioDownloadURL}, nil
	}
	dir := a.Module.Dir
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("open", "-a", studio, dir)
	} else {
		cmd = exec.Command(studio, dir)
		detach(cmd)
	}
	if err := cmd.Start(); err != nil {
		return nil, sdk.UserError("%s", ctx.T("err.openStudio", err.Error()))
	}
	go func() { _ = cmd.Wait() }()
	return &sdk.InvokeActionResult{Message: ctx.T("msg.openedStudio")}, nil
}

// buildRelease 构建发布包：apk 用 assemble<Variant>，aab 用 bundle<Variant>（默认 release）。
func (p *Plugin) buildRelease(ctx *sdk.Context, a *sdk.InvokeActionParams) (*sdk.InvokeActionResult, error) {
	m := buildModel(&a.Module, a.Env)
	app := m.findApp(argString(a.Args, "module"))
	if app == nil {
		return nil, sdk.UserError("%s", ctx.T("err.noApp", argString(a.Args, "module")))
	}
	vname := firstNonEmpty(argString(a.Args, "variant"), "release")
	v, ok := app.findVariant(vname)
	if !ok {
		return nil, sdk.UserError("%s", ctx.T("err.unknownVariant", vname, strings.Join(app.Variants, ", ")))
	}
	bundle := argString(a.Args, "format") == "aab"
	task := "assemble" + capitalize(v.Name)
	if bundle {
		task = "bundle" + capitalize(v.Name)
	}
	task = taskPath(app.Path, task)
	env := sdk.MergeEnv(a.Env)
	if m.SDK.Source == srcDefault {
		env["ANDROID_HOME"] = m.SDK.Dir
		env["ANDROID_SDK_ROOT"] = m.SDK.Dir
	}
	var res runTasksResult
	if err := ctx.Host.Call(ctx, "gradle", "runTasks", runTasksRequest{Module: a.Module, Toolchains: a.Toolchains, Env: env, RunID: a.TaskID,
		Title: ctx.T("action.buildRelease.title", task), Tasks: []string{task}}, &res); err != nil {
		return nil, err
	}
	if res.Cancelled {
		return nil, sdk.NewError(sdk.ErrCodeCancelled, "cancelled")
	}
	if res.ExitCode != 0 {
		return nil, sdk.UserError("%s", ctx.T("err.taskFailed", task, res.ExitCode))
	}
	files := releaseOutputs(app, v, bundle)
	if len(files) == 0 {
		return &sdk.InvokeActionResult{Message: ctx.T("msg.built", task)}, nil
	}
	msg := ctx.T("msg.builtFiles", strings.Join(files, "\n"))
	for _, f := range files {
		if strings.Contains(filepath.Base(f), "-unsigned") {
			msg += "\n" + ctx.T("msg.unsigned")
			break
		}
	}
	return &sdk.InvokeActionResult{Message: msg}, nil
}

// releaseOutputs 列出构建产物：apk 在 build/outputs/apk/<风味>/<buildType>/，aab 在 build/outputs/bundle/<变体>/。
func releaseOutputs(app *AppProject, v Variant, bundle bool) []string {
	var dirs []string
	ext := ".apk"
	if bundle {
		ext = ".aab"
		dirs = append(dirs, filepath.Join(app.Dir, "build", "outputs", "bundle", v.Name))
	} else {
		base := filepath.Join(app.Dir, "build", "outputs", "apk")
		if len(v.Flavors) > 0 {
			dirs = append(dirs, filepath.Join(base, variantName(v.Flavors, ""), v.BuildType))
		} else {
			dirs = append(dirs, filepath.Join(base, v.BuildType))
		}
		dirs = append(dirs, filepath.Join(base, v.Name))
	}
	var out []string
	for _, d := range dirs {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ext) {
				out = append(out, filepath.Join(d, e.Name()))
			}
		}
		if len(out) > 0 {
			break
		}
	}
	sort.Strings(out)
	return out
}

func argString(args map[string]any, key string) string {
	if s, ok := args[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}
