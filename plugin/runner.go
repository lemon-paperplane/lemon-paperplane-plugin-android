package android

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// RunMain 是插件自带的运行器（内核托管的主进程，命令行见 runnerArgs）：
// 解析启动 Activity → am start -W 启动 → 取进程号 → 打印就绪行 → logcat --pid 实时输出；
// 每 2 秒检查进程：退出就结束（崩溃时退出码 1），进程号变了就跟随；
// 收到 SIGTERM / SIGINT 时按设置 force-stop 应用再退出；设备断开时退出码 2。
func (p *Plugin) RunMain(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var a runnerArgs
	fs.StringVar(&a.ADB, "adb", "adb", "")
	fs.StringVar(&a.Serial, "serial", "", "")
	fs.StringVar(&a.Package, "package", "", "")
	fs.StringVar(&a.Activity, "activity", "", "")
	fs.BoolVar(&a.ForceStop, "force-stop", true, "")
	fs.StringVar(&a.Locale, "locale", "en", "")
	fs.StringVar(&a.PluginDir, "plugin-dir", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if a.Package == "" || a.Serial == "" {
		fmt.Fprintln(os.Stderr, "usage: __run --adb <adb> --serial <serial> --package <pkg>")
		return 2
	}
	b, _ := sdk.LoadBundle(a.PluginDir, a.Locale)
	r := &runner{args: a, l10n: b, adb: &adbClient{path: a.ADB, env: os.Environ()}, out: os.Stdout, errOut: os.Stderr}
	return r.main()
}

type runner struct {
	args   runnerArgs
	l10n   *sdk.Bundle
	adb    *adbClient
	out    io.Writer
	errOut io.Writer
	outMu  sync.Mutex
}

func (r *runner) T(key string, args ...any) string { return r.l10n.T(key, args...) }

// say 打印运行器自己的一行（和 logcat 输出走同一个 stdout，加锁避免交错）。
func (r *runner) say(text string) {
	r.outMu.Lock()
	fmt.Fprintln(r.out, text)
	r.outMu.Unlock()
}

func (r *runner) sayErr(text string) {
	r.outMu.Lock()
	fmt.Fprintln(r.errOut, text)
	r.outMu.Unlock()
}

// 监视循环的结果。
const (
	evStop         = iota // 收到停止信号
	evExited              // 应用进程没了
	evRestarted           // 进程号变了
	evDisconnected        // 设备断开
)

func (r *runner) main() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	var stopping atomic.Bool
	go func() {
		<-sigs
		stopping.Store(true)
		cancel()
	}()
	finish := func(code int) int {
		if stopping.Load() {
			return r.stop()
		}
		return code
	}

	// 1. 启动 Activity
	comp := r.args.Activity
	if comp == "" {
		c, err := r.resolveActivity(ctx)
		if err != nil {
			r.sayErr(err.Error())
			return finish(1)
		}
		comp = c
	}
	comp = componentName(r.args.Package, comp)
	r.say(r.T("run.launching", comp))
	out, errOut, code, err := r.adb.run(ctx, 90*time.Second, "-s", r.args.Serial, "shell", "am", "start", "-W", "-S",
		"-a", "android.intent.action.MAIN", "-c", "android.intent.category.LAUNCHER", "-n", shellQuote(comp))
	for _, l := range strings.Split(strings.TrimRight(strings.ReplaceAll(out+errOut, "\r", ""), "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			r.say("  " + l)
		}
	}
	if stopping.Load() {
		return r.stop()
	}
	if err != nil || code != 0 || amStartFailed(out+errOut) {
		if isDeviceError(errOut) {
			r.sayErr(r.T("run.disconnected", r.args.Serial, strings.TrimSpace(errOut)))
			return 2
		}
		r.sayErr(r.T("run.startFailed", comp))
		return 1
	}

	// 2. 进程号
	pid := ""
	deadline := time.Now().Add(10 * time.Second)
	for pid == "" && time.Now().Before(deadline) && ctx.Err() == nil {
		pid, _ = r.pidof(ctx)
		if pid == "" {
			sleepCtx(ctx, 300*time.Millisecond)
		}
	}
	if stopping.Load() {
		return r.stop()
	}
	if pid == "" {
		// 启动后马上崩了：把崩溃缓冲区里这个应用的记录打出来
		r.dumpCrash(ctx)
		r.sayErr(r.T("run.noProcess", r.args.Package))
		return 1
	}
	r.say(r.T("run.started", r.args.Package, pid))

	// 3. 跟踪 logcat，直到应用退出 / 停止 / 设备断开
	crashed := false
	restarts := 0
	for {
		lc, err := r.startLogcat(pid)
		if err != nil {
			r.sayErr(err.Error())
			return finish(1)
		}
		ev, info := r.watch(ctx, lc, &stopping, pid)
		if lc.crash.Load() {
			crashed = true
		}
		switch ev {
		case evStop:
			lc.kill()
			return r.stop()
		case evExited:
			// logcat 可能还有没吐完的崩溃堆栈
			lc.drain(1500 * time.Millisecond)
			if lc.crash.Load() {
				crashed = true
			}
			if crashed {
				r.sayErr(r.T("run.appCrashed", r.args.Package))
				return 1
			}
			r.say(r.T("run.appExited", r.args.Package))
			return 0
		case evRestarted:
			lc.kill()
			pid = info
			r.say(r.T("run.restarted", pid))
		case evDisconnected:
			lc.kill()
			r.sayErr(r.T("run.disconnected", r.args.Serial, info))
			return 2
		default: // logcat 自己退出了、应用还在：重开
			lc.kill()
			restarts++
			if restarts > 5 {
				r.sayErr(r.T("run.logcatFailed"))
				return 1
			}
			sleepCtx(ctx, time.Second)
		}
	}
}

// watch 每 2 秒查一次进程号，同时看 logcat 是否还在。返回事件与附带信息（新进程号 / 断开原因）。
func (r *runner) watch(ctx context.Context, lc *logcatProc, stopping *atomic.Bool, pid string) (int, string) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	check := func() (int, string, bool) {
		cur, err := r.pidof(ctx)
		if stopping.Load() {
			return evStop, "", true
		}
		if err != nil {
			if st := r.deviceState(ctx); st != "device" {
				return evDisconnected, firstNonEmpty(errMessage(err), st), true
			}
			return 0, "", false
		}
		if cur == "" {
			return evExited, "", true
		}
		if cur != pid {
			return evRestarted, cur, true
		}
		return 0, "", false
	}
	for {
		select {
		case <-ctx.Done():
			return evStop, ""
		case <-lc.done:
			// 停止信号会同时结束 logcat（同一进程组），稍等一下再判断是不是在停止
			select {
			case <-ctx.Done():
				return evStop, ""
			case <-time.After(300 * time.Millisecond):
			}
			if ev, info, ok := check(); ok {
				return ev, info
			}
			return -1, ""
		case <-tick.C:
			if ev, info, ok := check(); ok {
				return ev, info
			}
		}
	}
}

// stop 处理停止：按设置 force-stop 应用。
func (r *runner) stop() int {
	if r.args.ForceStop {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, errOut, code, err := r.adb.run(ctx, 6*time.Second, "-s", r.args.Serial, "shell", "am", "force-stop", r.args.Package)
		if err == nil && code == 0 {
			r.say(r.T("run.forceStopped", r.args.Package))
		} else {
			r.sayErr(r.T("run.forceStopFailed", r.args.Package, strings.TrimSpace(errOut)))
		}
	}
	return 0
}

// resolveActivity 解析应用的启动 Activity（普通启动器，没有再试电视启动器）。
func (r *runner) resolveActivity(ctx context.Context) (string, error) {
	for _, cat := range []string{"android.intent.category.LAUNCHER", "android.intent.category.LEANBACK_LAUNCHER"} {
		out, errOut, _, err := r.adb.run(ctx, 15*time.Second, "-s", r.args.Serial, "shell", "cmd", "package", "resolve-activity", "--brief",
			"-a", "android.intent.action.MAIN", "-c", cat, r.args.Package)
		if err != nil {
			return "", err
		}
		if isDeviceError(errOut) {
			return "", errors.New(r.T("run.disconnected", r.args.Serial, strings.TrimSpace(errOut)))
		}
		if c := parseResolveActivity(out, r.args.Package); c != "" {
			return c, nil
		}
	}
	return "", errors.New(r.T("run.noLauncher", r.args.Package))
}

// pidof 返回应用主进程号；没有在运行返回 ""。设备层面的错误返回 error。
func (r *runner) pidof(ctx context.Context) (string, error) {
	out, errOut, code, err := r.adb.run(ctx, 6*time.Second, "-s", r.args.Serial, "shell", "pidof", r.args.Package)
	if err != nil {
		return "", err
	}
	if isDeviceError(errOut) {
		return "", errors.New(strings.TrimSpace(errOut))
	}
	if strings.Contains(out+errOut, "not found") && code != 0 {
		// 老系统（6.0 以前）没有 pidof：用 ps
		psOut, psErr, _, err := r.adb.run(ctx, 6*time.Second, "-s", r.args.Serial, "shell", "ps")
		if err != nil {
			return "", err
		}
		if isDeviceError(psErr) {
			return "", errors.New(strings.TrimSpace(psErr))
		}
		return pidFromPS(psOut, r.args.Package), nil
	}
	return parsePidof(out), nil
}

// deviceState 返回 adb get-state 的结果（device / offline / unauthorized / 错误信息）。
func (r *runner) deviceState(ctx context.Context) string {
	out, errOut, _, err := r.adb.run(ctx, 6*time.Second, "-s", r.args.Serial, "get-state")
	if err != nil {
		return errMessage(err)
	}
	if s := strings.TrimSpace(out); s != "" {
		return s
	}
	return strings.TrimSpace(errOut)
}

// dumpCrash 打印崩溃缓冲区最近的记录（只挑这个应用的那一段）。
func (r *runner) dumpCrash(ctx context.Context) {
	out, _, _, err := r.adb.run(ctx, 10*time.Second, "-s", r.args.Serial, "logcat", "-b", "crash", "-d", "-t", "300", "-v", "threadtime")
	if err != nil {
		return
	}
	for _, l := range crashSection(out, r.args.Package) {
		r.sayErr(l)
	}
}

// ---------------------------------------------------------------------------
// logcat 进程

type logcatProc struct {
	cmd   *exec.Cmd
	done  chan struct{}
	lines chan struct{} // 每输出一行发一次（drain 用）
	crash atomic.Bool
}

// startLogcat 跟踪某个进程号的日志（Android 7+ 的 --pid），彩色 threadtime 格式。
func (r *runner) startLogcat(pid string) (*logcatProc, error) {
	cmd := exec.Command(r.adb.path, "-s", r.args.Serial, "logcat", "--pid="+pid, "-v", "threadtime", "-v", "color")
	cmd.Env = r.adb.env
	hideWindow(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = r.errOut
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	lc := &logcatProc{cmd: cmd, done: make(chan struct{}), lines: make(chan struct{}, 1)}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, "FATAL EXCEPTION") || strings.Contains(line, "Fatal signal") {
				lc.crash.Store(true)
			}
			r.outMu.Lock()
			fmt.Fprintln(r.out, line)
			r.outMu.Unlock()
			select {
			case lc.lines <- struct{}{}:
			default:
			}
		}
		_ = cmd.Wait()
		close(lc.done)
	}()
	return lc, nil
}

// drain 在应用退出后再收一会儿日志：最长 max，连续 400ms 没有新行就结束。
func (lc *logcatProc) drain(max time.Duration) {
	deadline := time.After(max)
	for {
		select {
		case <-lc.done:
			return
		case <-deadline:
			lc.kill()
			return
		case <-lc.lines:
		case <-time.After(400 * time.Millisecond):
			lc.kill()
			return
		}
	}
}

func (lc *logcatProc) kill() {
	if lc.cmd.Process != nil {
		_ = lc.cmd.Process.Kill()
	}
	select {
	case <-lc.done:
	case <-time.After(2 * time.Second):
	}
}

// ---------------------------------------------------------------------------
// 纯函数（有单测）

// parseResolveActivity 从 `cmd package resolve-activity --brief` 的输出里取 "包名/Activity"。
func parseResolveActivity(out, pkg string) string {
	lines := strings.Split(strings.ReplaceAll(out, "\r", ""), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || strings.ContainsAny(l, " \t=") {
			continue
		}
		if strings.Contains(l, "/") {
			return l
		}
	}
	return ""
}

// componentName 把 Activity 写法统一成 "包名/类名"：".Main" → pkg/.Main，"com.x.Main" → pkg/com.x.Main。
func componentName(pkg, act string) string {
	act = strings.TrimSpace(act)
	if strings.Contains(act, "/") {
		return act
	}
	return pkg + "/" + act
}

// amStartFailed 判断 am start 的输出是不是失败。
func amStartFailed(out string) bool {
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "Error:") || strings.HasPrefix(l, "Error type") || strings.HasPrefix(l, "Exception occurred") ||
			strings.HasPrefix(l, "java.lang.SecurityException") || strings.HasPrefix(l, "Status: timeout") {
			return true
		}
	}
	return false
}

// parsePidof 取 pidof 输出的第一个进程号。
func parsePidof(out string) string {
	for _, f := range strings.Fields(out) {
		if isDigits(f) {
			return f
		}
	}
	return ""
}

// pidFromPS 从 ps 输出里找进程名等于包名的那一行的 PID（第二列）。
func pidFromPS(out, pkg string) string {
	for _, l := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		f := strings.Fields(l)
		if len(f) >= 3 && f[len(f)-1] == pkg && isDigits(f[1]) {
			return f[1]
		}
	}
	return ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// isDeviceError 判断 adb 的 stderr 是不是设备层面的错误（断开、离线、未授权）。
func isDeviceError(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, k := range []string{"error: device", "no devices/emulators found", "device offline", "device unauthorized", "device not found", "error: closed", "error: protocol fault", "cannot connect to daemon"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func errMessage(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

// crashSection 从 logcat 崩溃缓冲区里挑出属于某个应用的崩溃记录（FATAL EXCEPTION 起、到 Process: <pkg> 所在的那一段）。
func crashSection(out, pkg string) []string {
	lines := strings.Split(strings.ReplaceAll(out, "\r", ""), "\n")
	var res []string
	for i := 0; i < len(lines); i++ {
		if !strings.Contains(lines[i], "FATAL EXCEPTION") {
			continue
		}
		block := []string{lines[i]}
		ours := false
		j := i + 1
		for ; j < len(lines) && !strings.Contains(lines[j], "FATAL EXCEPTION"); j++ {
			if strings.TrimSpace(lines[j]) == "" || strings.HasPrefix(lines[j], "--------- beginning of") {
				continue
			}
			block = append(block, lines[j])
			if strings.Contains(lines[j], "Process: "+pkg+",") {
				ours = true
			}
		}
		if ours {
			res = block
		}
		i = j - 1
	}
	if len(res) > 80 {
		res = res[:80]
	}
	return res
}

// shellQuote 给设备上的 shell 加引号（Activity 名可能含 $）。
func shellQuote(s string) string {
	if !strings.ContainsAny(s, "$ '\"\\`;&|<>()*?") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
