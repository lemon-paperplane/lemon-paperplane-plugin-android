package android

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// Device 是 `adb devices -l` 里的一台设备。
type Device struct {
	Serial  string `json:"serial"`
	State   string `json:"state"` // device | unauthorized | offline | no permissions | recovery | ...
	Model   string `json:"model,omitempty"`
	Product string `json:"product,omitempty"`
	Name    string `json:"device,omitempty"`
	AVD     string `json:"avd,omitempty"` // 模拟器对应的 AVD 名
}

// Online 表示可以安装运行。
func (d Device) Online() bool { return d.State == "device" }

// Emulator 判断是不是本机模拟器。
func (d Device) Emulator() bool { return strings.HasPrefix(d.Serial, "emulator-") }

// Label 是界面上显示的名字：型号 · 序列号；模拟器用 AVD 名。
func (d Device) Label() string {
	name := strings.ReplaceAll(d.Model, "_", " ")
	if d.AVD != "" {
		name = d.AVD
	}
	if name == "" {
		return d.Serial
	}
	return name + " · " + d.Serial
}

// AVD 是一个安卓虚拟设备。
type AVD struct {
	Name    string `json:"name"`
	Display string `json:"display,omitempty"`
	Target  string `json:"target,omitempty"`
}

// avdOptionPrefix 是运行配置 device 选项里 AVD 的写法：avd:<名字>。
const avdOptionPrefix = "avd:"

// parseDevices 解析 `adb devices -l` 的输出。
func parseDevices(out string) []Device {
	var list []Device
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "*") || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "adb ") || strings.HasPrefix(line, "error:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		d := Device{Serial: fields[0], State: fields[1]}
		rest := fields[2:]
		if d.State == "no" && len(fields) > 2 && fields[2] == "permissions" {
			d.State = "no permissions"
			rest = fields[3:]
		}
		for _, f := range rest {
			k, v, ok := strings.Cut(f, ":")
			if !ok {
				continue
			}
			switch k {
			case "model":
				d.Model = v
			case "product":
				d.Product = v
			case "device":
				d.Name = v
			}
		}
		list = append(list, d)
	}
	return list
}

// avdHome 返回 AVD 目录：ANDROID_AVD_HOME → ANDROID_USER_HOME/avd → ANDROID_SDK_HOME/.android/avd → ~/.android/avd。
func avdHome(env map[string]string) string {
	if d := lookupEnv(env, "ANDROID_AVD_HOME"); d != "" {
		return d
	}
	if d := lookupEnv(env, "ANDROID_USER_HOME"); d != "" {
		return filepath.Join(d, "avd")
	}
	if d := lookupEnv(env, "ANDROID_SDK_HOME"); d != "" {
		return filepath.Join(d, ".android", "avd")
	}
	return filepath.Join(sdk.HomeDir(), ".android", "avd")
}

// listAVDs 读 AVD 目录里的 <名字>.ini（不跑 emulator -list-avds，快且不依赖 SDK）。
func listAVDs(dir string) []AVD {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []AVD
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ini") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".ini")
		ini := parseProperties(sdk.ReadText(filepath.Join(dir, e.Name()), 16*1024))
		path := ini["path"]
		if path == "" && ini["path.rel"] != "" {
			path = filepath.Join(filepath.Dir(dir), filepath.FromSlash(ini["path.rel"]))
		}
		if path == "" || !sdk.DirExists(path) {
			if alt := filepath.Join(dir, name+".avd"); sdk.DirExists(alt) {
				path = alt
			} else {
				continue
			}
		}
		cfg := parseProperties(sdk.ReadText(filepath.Join(path, "config.ini"), 64*1024))
		out = append(out, AVD{Name: name, Display: cfg["avd.ini.displayname"], Target: ini["target"]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---------------------------------------------------------------------------
// adb 调用

// adbClient 直接调用 adb（短命令，带超时）。env 为完整的进程环境。
type adbClient struct {
	path string
	env  []string
}

func newADB(path string, overlay map[string]string) *adbClient {
	return &adbClient{path: path, env: environ(overlay)}
}

// environ 在当前进程环境上覆盖 overlay，返回 KEY=VALUE 列表。
func environ(overlay map[string]string) []string {
	m := map[string]string{}
	var order []string
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, seen := m[k]; !seen {
			order = append(order, k)
		}
		m[k] = v
	}
	for k, v := range overlay {
		if _, seen := m[k]; !seen {
			order = append(order, k)
		}
		m[k] = v
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+m[k])
	}
	return out
}

// run 执行 adb，返回 stdout、stderr、退出码。
func (a *adbClient) run(ctx context.Context, timeout time.Duration, args ...string) (string, string, int, error) {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(c, a.path, args...)
	cmd.Env = a.env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = 2 * time.Second // adb 首次会拉起后台服务，别等它的输出管道
	hideWindow(cmd)
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
			err = nil
		} else {
			code = -1
		}
	}
	if c.Err() == context.DeadlineExceeded && err == nil {
		err = c.Err()
	}
	return out.String(), errb.String(), code, err
}

// devices 列出设备，并补上模拟器对应的 AVD 名。
func (a *adbClient) devices(ctx context.Context) ([]Device, error) {
	out, errOut, code, err := a.run(ctx, 15*time.Second, "devices", "-l")
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, errors.New(strings.TrimSpace(errOut + out))
	}
	list := parseDevices(out)
	for i := range list {
		if list[i].Emulator() && list[i].Online() {
			list[i].AVD = a.avdName(ctx, list[i].Serial)
		}
	}
	return list, nil
}

// avdName 查询模拟器的 AVD 名：`adb emu avd name`，不行再读系统属性。
func (a *adbClient) avdName(ctx context.Context, serial string) string {
	if out, _, code, err := a.run(ctx, 3*time.Second, "-s", serial, "emu", "avd", "name"); err == nil && code == 0 {
		if n := firstLine(out); n != "" && n != "OK" && !strings.HasPrefix(n, "KO") {
			return n
		}
	}
	for _, prop := range []string{"ro.boot.qemu.avd_name", "ro.kernel.qemu.avd_name"} {
		if out, _, code, err := a.run(ctx, 3*time.Second, "-s", serial, "shell", "getprop", prop); err == nil && code == 0 {
			if n := firstLine(out); n != "" {
				return n
			}
		}
	}
	return ""
}

func firstLine(s string) string {
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// bootCompleted 判断设备是否开机完成。
func (a *adbClient) bootCompleted(ctx context.Context, serial string) bool {
	out, _, code, err := a.run(ctx, 5*time.Second, "-s", serial, "shell", "getprop", "sys.boot_completed")
	return err == nil && code == 0 && strings.TrimSpace(out) == "1"
}

// ---------------------------------------------------------------------------
// 选设备

// 选设备失败的原因（PrepareRun 映射成本地化文案，Diagnose 据此给出「用 X 运行」的修复）。
const (
	selNoDevice     = "noDevice"     // 没有在线设备，也没有 AVD
	selMultiple     = "multiple"     // 多台在线设备，需要选
	selChooseAVD    = "chooseAvd"    // 没有在线设备、有多个 AVD，需要选
	selNotFound     = "notFound"     // 指定的设备不在
	selAVDNotFound  = "avdNotFound"  // 指定的 AVD 不存在
	selUnauthorized = "unauthorized" // 设备未授权 USB 调试
	selOffline      = "offline"      // 设备离线
	selNoPermission = "noPermission" // Linux udev 权限
)

// selection 是选设备的结果：用已连接的设备，或者启动一个 AVD。
type selection struct {
	Serial   string
	Label    string
	StartAVD string // 非空：需要启动这个 AVD
	WaitBoot bool   // 设备已在列表里但还没开机完成（模拟器启动中）
}

type selectError struct {
	Kind   string
	Device string
}

func (e *selectError) Error() string { return e.Kind + " " + e.Device }

// chooseDevice 按运行配置的 device 选项选设备：空为自动；"avd:名字" 为模拟器；否则是序列号。
func chooseDevice(opt string, devs []Device, avds []AVD) (*selection, *selectError) {
	opt = strings.TrimSpace(opt)
	notReady := func(d Device) *selectError {
		switch d.State {
		case "unauthorized":
			return &selectError{Kind: selUnauthorized, Device: d.Label()}
		case "no permissions":
			return &selectError{Kind: selNoPermission, Device: d.Label()}
		default:
			return &selectError{Kind: selOffline, Device: d.Label()}
		}
	}
	switch {
	case opt == "":
		var online, waiting []Device
		for _, d := range devs {
			if d.Online() {
				online = append(online, d)
			} else {
				waiting = append(waiting, d)
			}
		}
		switch {
		case len(online) == 1:
			return &selection{Serial: online[0].Serial, Label: online[0].Label()}, nil
		case len(online) > 1:
			return nil, &selectError{Kind: selMultiple}
		case len(waiting) > 0:
			// 模拟器在开机中（offline）也算：等它
			for _, d := range waiting {
				if d.Emulator() && d.State == "offline" {
					return &selection{Serial: d.Serial, Label: d.Label(), WaitBoot: true}, nil
				}
			}
			return nil, notReady(waiting[0])
		case len(avds) == 1:
			return &selection{StartAVD: avds[0].Name, Label: avds[0].Name}, nil
		case len(avds) > 1:
			return nil, &selectError{Kind: selChooseAVD}
		}
		return nil, &selectError{Kind: selNoDevice}
	case strings.HasPrefix(opt, avdOptionPrefix):
		name := strings.TrimPrefix(opt, avdOptionPrefix)
		for _, d := range devs {
			if d.Emulator() && d.AVD == name {
				if d.Online() {
					return &selection{Serial: d.Serial, Label: d.Label()}, nil
				}
				return &selection{Serial: d.Serial, Label: d.Label(), WaitBoot: true}, nil
			}
		}
		for _, a := range avds {
			if a.Name == name {
				return &selection{StartAVD: name, Label: name}, nil
			}
		}
		return nil, &selectError{Kind: selAVDNotFound, Device: name}
	default:
		for _, d := range devs {
			if d.Serial == opt {
				if d.Online() {
					return &selection{Serial: d.Serial, Label: d.Label()}, nil
				}
				if d.Emulator() && d.State == "offline" {
					return &selection{Serial: d.Serial, Label: d.Label(), WaitBoot: true}, nil
				}
				return nil, notReady(d)
			}
		}
		return nil, &selectError{Kind: selNotFound, Device: opt}
	}
}

// deviceOptions 生成运行配置 device 下拉选项：自动、在线设备、AVD（运行中的模拟器合并成一项）。
func deviceOptions(ctx *sdk.Context, devs []Device, avds []AVD) []sdk.Option {
	opts := []sdk.Option{{Value: "", Label: ctx.T("device.auto")}}
	running := map[string]bool{}
	for _, d := range devs {
		if d.Emulator() && d.AVD != "" {
			running[d.AVD] = true
			opts = append(opts, sdk.Option{Value: avdOptionPrefix + d.AVD, Label: ctx.T("device.avdRunning", d.AVD)})
			continue
		}
		label := d.Label()
		if !d.Online() {
			label = ctx.T("device.notReady", label, d.State)
		}
		opts = append(opts, sdk.Option{Value: d.Serial, Label: label})
	}
	for _, a := range avds {
		if running[a.Name] {
			continue
		}
		opts = append(opts, sdk.Option{Value: avdOptionPrefix + a.Name, Label: ctx.T("device.avd", firstNonEmpty(a.Display, a.Name))})
	}
	return opts
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 启动模拟器

// startEmulator 启动 AVD（带窗口，脱离插件进程，运行结束后也不关），输出写日志文件。
// 返回的通道在模拟器进程退出时关闭。
func startEmulator(emu, name string, extra []string, env []string, logFile string) (<-chan struct{}, error) {
	_ = os.MkdirAll(filepath.Dir(logFile), 0o755)
	lf, err := os.Create(logFile)
	if err != nil {
		return nil, err
	}
	args := append([]string{"-avd", name}, extra...)
	cmd := exec.Command(emu, args...)
	cmd.Env = env
	cmd.Dir = filepath.Dir(emu)
	cmd.Stdout, cmd.Stderr = lf, lf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		lf.Close()
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		lf.Close()
		close(done)
	}()
	return done, nil
}

// tailFile 返回文件最后 n 行。
func tailFile(path string, n int) string {
	data := sdk.ReadText(path, 0)
	lines := strings.Split(strings.TrimRight(data, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
