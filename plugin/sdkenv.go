package android

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
	"github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk/archive"
)

// SDK 来源。
const (
	srcLocalProps = "local.properties"
	srcEnvHome    = "ANDROID_HOME"
	srcEnvRoot    = "ANDROID_SDK_ROOT"
	srcDefault    = "default"
)

// 官方仓库与国内镜像（腾讯云同步了 Google 的 SDK 仓库，含 cmdline-tools 与新版 Android CLI 用的索引）。
const (
	googleRepo  = "https://dl.google.com/android/repository"
	tencentRepo = "https://mirrors.cloud.tencent.com/AndroidSDK"
)

// sdkLoc 是解析出的 Android SDK 位置。
type sdkLoc struct {
	Dir    string `json:"dir"`
	Source string `json:"source,omitempty"` // local.properties | ANDROID_HOME | ANDROID_SDK_ROOT | default；没找到为空
	Exists bool   `json:"exists"`
}

// lookupEnv 先看工程环境变量，再看进程环境。
func lookupEnv(env map[string]string, key string) string {
	if v, ok := env[key]; ok {
		return v
	}
	return os.Getenv(key)
}

// locateSDK 按 Android Gradle 插件的顺序找 SDK：local.properties 的 sdk.dir → ANDROID_HOME → ANDROID_SDK_ROOT；
// 都没有时找各平台的默认安装位置（AGP 自己不会找，运行时由本插件把 ANDROID_HOME 传给 Gradle）。
// 都找不到时 Dir 为默认安装位置、Exists=false。
func locateSDK(root string, env map[string]string) sdkLoc {
	props := parseProperties(sdk.ReadText(filepath.Join(root, "local.properties"), 64*1024))
	if d := strings.TrimSpace(props["sdk.dir"]); d != "" {
		d = sdk.Expand(d)
		if !filepath.IsAbs(d) {
			d = filepath.Join(root, d)
		}
		return sdkLoc{Dir: d, Source: srcLocalProps, Exists: isSDKDir(d)}
	}
	for _, k := range []string{srcEnvHome, srcEnvRoot} {
		if d := strings.TrimSpace(lookupEnv(env, k)); d != "" {
			d = sdk.Expand(d)
			return sdkLoc{Dir: d, Source: k, Exists: isSDKDir(d)}
		}
	}
	cands := defaultSDKDirs()
	for _, d := range cands {
		if isSDKDir(d) {
			return sdkLoc{Dir: d, Source: srcDefault, Exists: true}
		}
	}
	return sdkLoc{Dir: cands[0]}
}

// defaultSDKDirs 是各平台常见的 SDK 位置，第一个是 Android Studio 的默认安装位置。
func defaultSDKDirs() []string {
	home := sdk.HomeDir()
	switch runtime.GOOS {
	case "darwin":
		return []string{filepath.Join(home, "Library", "Android", "sdk"), "/opt/homebrew/share/android-commandlinetools", "/usr/local/share/android-commandlinetools", "/usr/local/share/android-sdk"}
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			local = filepath.Join(home, "AppData", "Local")
		}
		return []string{filepath.Join(local, "Android", "Sdk"), `C:\Android\sdk`, `C:\Android\android-sdk`}
	default:
		return []string{filepath.Join(home, "Android", "Sdk"), filepath.Join(home, "Android", "sdk"), filepath.Join(home, "android-sdk"), "/opt/android-sdk", "/usr/lib/android-sdk"}
	}
}

// isSDKDir 判断目录像不像 Android SDK（有 platform-tools / platforms / build-tools / cmdline-tools 之一）。
func isSDKDir(d string) bool {
	if d == "" || !sdk.DirExists(d) {
		return false
	}
	for _, sub := range []string{"platform-tools", "platforms", "build-tools", "cmdline-tools", "tools"} {
		if sdk.DirExists(filepath.Join(d, sub)) {
			return true
		}
	}
	return false
}

// pkgInstalled 判断 SDK 包目录是否装好（有 package.xml 或 source.properties）。
func pkgInstalled(dir string) bool {
	return sdk.FileExists(filepath.Join(dir, "package.xml")) || sdk.FileExists(filepath.Join(dir, "source.properties"))
}

// platformPackage：compileSdk "35" → "platforms;android-35"，"36.1" → "platforms;android-36.1"，预览代号同理。
func platformPackage(level string) string { return "platforms;android-" + level }

func hasPlatform(sdkDir, level string) bool {
	d := filepath.Join(sdkDir, "platforms", "android-"+level)
	return pkgInstalled(d) || sdk.FileExists(filepath.Join(d, "android.jar"))
}

func hasBuildTools(sdkDir, ver string) bool {
	return pkgInstalled(filepath.Join(sdkDir, "build-tools", ver))
}

// hasNDK 看 ndk/<版本>，以及老的 ndk-bundle（版本写在 source.properties 的 Pkg.Revision）。
func hasNDK(sdkDir, ver string) bool {
	if pkgInstalled(filepath.Join(sdkDir, "ndk", ver)) {
		return true
	}
	p := parseProperties(sdk.ReadText(filepath.Join(sdkDir, "ndk-bundle", "source.properties"), 16*1024))
	return p["Pkg.Revision"] != "" && strings.HasPrefix(p["Pkg.Revision"], ver)
}

// adbPath 返回 SDK 里的 adb；没有时返回 ""。
func adbPath(sdkDir string) string {
	p := filepath.Join(sdkDir, "platform-tools", sdk.ExeName("adb"))
	if sdk.FileExists(p) {
		return p
	}
	return ""
}

// emulatorPath 返回 SDK 里的模拟器程序；没有时返回 ""。
func emulatorPath(sdkDir string) string {
	p := filepath.Join(sdkDir, "emulator", sdk.ExeName("emulator"))
	if sdk.FileExists(p) {
		return p
	}
	return ""
}

// sdkTool 是装 SDK 包用的命令行工具。cmdline-tools 23 起 sdkmanager 只是 Android CLI（bin/android）的外壳。
type sdkTool struct {
	Path string // 可执行文件
	CLI  bool   // true：Android CLI（android sdk install a/b）；false：老的 sdkmanager（a;b，要从 stdin 接受许可）
}

// findSDKTool 找 cmdline-tools：latest 优先，其次版本号最高的，最后是老的 tools/bin/sdkmanager。
func findSDKTool(sdkDir string) *sdkTool {
	var dirs []string
	base := filepath.Join(sdkDir, "cmdline-tools")
	if entries, err := os.ReadDir(base); err == nil {
		var vers []string
		for _, e := range entries {
			if e.IsDir() && e.Name() != "latest" && !strings.HasPrefix(e.Name(), ".") {
				vers = append(vers, e.Name())
			}
		}
		sdk.SortVersionsDesc(vers)
		dirs = append(dirs, filepath.Join(base, "latest"))
		for _, v := range vers {
			dirs = append(dirs, filepath.Join(base, v))
		}
	}
	dirs = append(dirs, filepath.Join(sdkDir, "tools"))
	for _, d := range dirs {
		if p := filepath.Join(d, "bin", sdk.ExeName("android")); sdk.FileExists(p) {
			return &sdkTool{Path: p, CLI: true}
		}
		name := "sdkmanager"
		if runtime.GOOS == "windows" {
			name = "sdkmanager.bat"
		}
		if p := filepath.Join(d, "bin", name); sdk.FileExists(p) {
			return &sdkTool{Path: p}
		}
	}
	return nil
}

// missingPackages 列出工程需要但 SDK 里没有的包（平台、指定的 build-tools、指定版本且没写 ndkPath 的 NDK），去重。
func missingPackages(sdkDir string, projects []*AppProject) []string {
	var out []string
	add := func(p string) {
		if !contains(out, p) {
			out = append(out, p)
		}
	}
	for _, p := range projects {
		if p.CompileSdk != "" && !hasPlatform(sdkDir, p.CompileSdk) {
			add(platformPackage(p.CompileSdk))
		}
		if p.BuildTools != "" && !hasBuildTools(sdkDir, p.BuildTools) {
			add("build-tools;" + p.BuildTools)
		}
		if p.NdkVersion != "" && p.NdkPath == "" && !hasNDK(sdkDir, p.NdkVersion) {
			add("ndk;" + p.NdkVersion)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// cmdline-tools 与 SDK 包安装（在修复任务里执行，输出进任务控制台）

// repoURLs 按镜像区域排列官方源与国内镜像。
func repoURLs(ctx *sdk.Context, file string) []string {
	g, t := googleRepo+"/"+file, tencentRepo+"/"+file
	if useChinaMirror(ctx) {
		return []string{t, g}
	}
	return []string{g, t}
}

func useChinaMirror(ctx *sdk.Context) bool {
	return ctx.Init != nil && ctx.Init.Mirrors.Region == "china"
}

func (p *Plugin) cacheDir(ctx *sdk.Context) string {
	if ctx.Init != nil && ctx.Init.CacheDir != "" {
		return ctx.Init.CacheDir
	}
	return filepath.Join(os.TempDir(), "paperplane-android")
}

// repoArchive 是 repository2-3.xml 里一个包的一份下载。
type repoArchive struct {
	URL  string
	SHA1 string
	Size int64
}

type repoXML struct {
	Packages []struct {
		Path     string `xml:"path,attr"`
		Obsolete string `xml:"obsolete,attr"`
		Channel  struct {
			Ref string `xml:"ref,attr"`
		} `xml:"channelRef"`
		Revision struct {
			Major int `xml:"major"`
			Minor int `xml:"minor"`
		} `xml:"revision"`
		Archives []struct {
			Size     int64  `xml:"complete>size"`
			Checksum string `xml:"complete>checksum"`
			URL      string `xml:"complete>url"`
			HostOS   string `xml:"host-os"`
			HostArch string `xml:"host-arch"`
		} `xml:"archives>archive"`
	} `xml:"remotePackage"`
}

// pickCmdlineTools 从仓库清单里挑出本机平台的 cmdline-tools;latest（稳定通道）。
func pickCmdlineTools(data []byte, goos, goarch string) (*repoArchive, error) {
	var r repoXML
	if err := xml.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	hostOS := map[string]string{"darwin": "macosx", "windows": "windows", "linux": "linux"}[goos]
	hostArch := map[string]string{"arm64": "aarch64", "amd64": "x64", "386": "x86"}[goarch]
	for _, pk := range r.Packages {
		if pk.Path != "cmdline-tools;latest" || (pk.Channel.Ref != "" && pk.Channel.Ref != "channel-0") {
			continue
		}
		var generic *repoArchive
		for _, a := range pk.Archives {
			if a.HostOS != hostOS {
				continue
			}
			ra := &repoArchive{URL: a.URL, SHA1: strings.ToLower(strings.TrimSpace(a.Checksum)), Size: a.Size}
			if a.HostArch == hostArch {
				return ra, nil
			}
			if a.HostArch == "" && generic == nil {
				generic = ra
			}
		}
		if generic != nil {
			return generic, nil
		}
	}
	return nil, fmt.Errorf("cmdline-tools for %s/%s not found in repository", goos, goarch)
}

// installCmdlineTools 下载 cmdline-tools 到 <sdk>/cmdline-tools/latest。清单从官方源取（拿 sha1 校验），
// 安装包按镜像区域先试国内镜像；sha1 不符拒收。
func (p *Plugin) installCmdlineTools(ctx *sdk.Context, sdkDir, taskID string) error {
	if t := findSDKTool(sdkDir); t != nil {
		return nil
	}
	cache := filepath.Join(p.cacheDir(ctx), "downloads")
	listFile := filepath.Join(cache, "repository2-3.xml")
	_ = os.Remove(listFile)
	ctx.Host.Console(taskID, ctx.T("install.fetchingRepo"))
	if _, err := ctx.Host.Download(ctx, &sdk.DownloadParams{URLs: []string{googleRepo + "/repository2-3.xml", tencentRepo + "/repository2-3.xml"}, Dest: listFile, TaskID: taskID, Title: ctx.T("install.fetchingRepo")}); err != nil {
		return sdk.UserError("%s", ctx.T("err.download", "repository2-3.xml", err.Error()))
	}
	data, err := os.ReadFile(listFile)
	if err != nil {
		return err
	}
	ar, err := pickCmdlineTools(data, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return sdk.UserError("%s", ctx.T("err.download", "cmdline-tools", err.Error()))
	}
	zipFile := filepath.Join(cache, ar.URL)
	if !(sdk.FileExists(zipFile) && fileSHA1(zipFile) == ar.SHA1) {
		_ = os.Remove(zipFile)
		if _, err := ctx.Host.Download(ctx, &sdk.DownloadParams{URLs: repoURLs(ctx, ar.URL), Dest: zipFile, TaskID: taskID, Title: ctx.T("install.downloading", "Android SDK Command-line Tools")}); err != nil {
			return sdk.UserError("%s", ctx.T("err.download", ar.URL, err.Error()))
		}
		if got := fileSHA1(zipFile); ar.SHA1 != "" && got != ar.SHA1 {
			_ = os.Remove(zipFile)
			return sdk.UserError("%s", ctx.T("err.checksum", ar.URL))
		}
	}
	ctx.Host.Console(taskID, ctx.T("install.extracting", filepath.Join(sdkDir, "cmdline-tools", "latest")))
	base := filepath.Join(sdkDir, "cmdline-tools")
	tmp := filepath.Join(base, ".latest-"+sdk.StableID(ar.URL))
	_ = os.RemoveAll(tmp)
	if err := archive.Extract(zipFile, tmp, "zip", 1); err != nil {
		_ = os.RemoveAll(tmp)
		return sdk.UserError("%s", ctx.T("err.extract", err.Error()))
	}
	dst := filepath.Join(base, "latest")
	if sdk.DirExists(dst) {
		// 残缺的 latest（没有可执行文件才会走到这里）
		_ = os.RemoveAll(dst)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if findSDKTool(sdkDir) == nil {
		return sdk.UserError("%s", ctx.T("err.noSdkTool"))
	}
	return nil
}

func fileSHA1(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sdkInstallCmd 组装安装 SDK 包的命令。mirror 为 true 时 Android CLI 走国内镜像。
// 老的 sdkmanager 是 Java 程序，不认 HTTPS_PROXY，这里把代理环境变量转成它的 --proxy 参数。
func sdkInstallCmd(t *sdkTool, sdkDir string, pkgs []string, mirror bool, env map[string]string) (string, []string) {
	if t.CLI {
		args := []string{"--no-metrics", "--sdk=" + sdkDir}
		if mirror {
			args = append(args, "--sdk_url="+tencentRepo, "--sdk_index="+tencentRepo+"/package_list.binpb")
		}
		args = append(args, "sdk", "install")
		for _, p := range pkgs {
			args = append(args, strings.ReplaceAll(p, ";", "/"))
		}
		return t.Path, args
	}
	args := []string{"--sdk_root=" + sdkDir}
	args = append(args, proxyArgs(env)...)
	return t.Path, append(args, pkgs...)
}

// proxyArgs 把 HTTPS_PROXY / HTTP_PROXY 转成 sdkmanager 的代理参数。
func proxyArgs(env map[string]string) []string {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		v := strings.TrimSpace(lookupEnv(env, k))
		if v == "" {
			continue
		}
		if !strings.Contains(v, "://") {
			v = "http://" + v
		}
		u, err := url.Parse(v)
		if err != nil || u.Hostname() == "" {
			continue
		}
		port := u.Port()
		if port == "" {
			port = "80"
		}
		kind := "http"
		if strings.HasPrefix(u.Scheme, "socks") {
			kind = "socks"
		}
		return []string{"--proxy=" + kind, "--proxy_host=" + u.Hostname(), "--proxy_port=" + port}
	}
	return nil
}

// licenseInput 是给老 sdkmanager 的许可确认输入（只在用户点了带确认文案的修复后使用）。
var licenseInput = strings.Repeat("y\n", 40)

// installPackages 用 cmdline-tools 安装 SDK 包（缺 cmdline-tools 时先装）。jdk 用于运行老的 sdkmanager。
func (p *Plugin) installPackages(ctx *sdk.Context, sdkDir string, pkgs []string, tcs map[string]sdk.Toolchain, env map[string]string, taskID string) error {
	if err := os.MkdirAll(sdkDir, 0o755); err != nil {
		return err
	}
	if err := p.installCmdlineTools(ctx, sdkDir, taskID); err != nil {
		return err
	}
	if len(pkgs) == 0 {
		return nil
	}
	t := findSDKTool(sdkDir)
	if t == nil {
		return sdk.UserError("%s", ctx.T("err.noSdkTool"))
	}
	jenv, jpath := p.javaEnv(ctx, tcs)
	run := func(mirror bool) (*sdk.ExecResult, error) {
		cmd, args := sdkInstallCmd(t, sdkDir, pkgs, mirror, env)
		return ctx.Host.Exec(ctx, &sdk.ExecParams{Command: cmd, Args: args, Cwd: sdkDir, Env: sdk.MergeEnv(env, jenv), PathPrepend: jpath,
			Console: taskID, Stdin: licenseInput, Title: ctx.T("install.packages", strings.Join(pkgs, ", "))})
	}
	mirror := t.CLI && useChinaMirror(ctx)
	r, err := run(mirror)
	if err != nil {
		return err
	}
	if r.ExitCode != 0 && mirror && !r.Cancelled {
		// 镜像没同步到或出错：退回官方源再试一次
		ctx.Host.Console(taskID, ctx.T("install.retryOfficial"))
		if r, err = run(false); err != nil {
			return err
		}
	}
	if r.Cancelled {
		return sdk.NewError(sdk.ErrCodeCancelled, "cancelled")
	}
	if r.ExitCode != 0 {
		return sdk.UserError("%s", ctx.T("err.sdkInstall", strings.Join(pkgs, ", "), r.ExitCode))
	}
	return nil
}

// acceptLicenses 运行 sdkmanager --licenses 并回答 y（只在用户点了「接受许可」修复后执行）。新版 Android CLI 不需要这一步。
func (p *Plugin) acceptLicenses(ctx *sdk.Context, sdkDir string, tcs map[string]sdk.Toolchain, env map[string]string, taskID string) error {
	if err := p.installCmdlineTools(ctx, sdkDir, taskID); err != nil {
		return err
	}
	t := findSDKTool(sdkDir)
	if t == nil {
		return sdk.UserError("%s", ctx.T("err.noSdkTool"))
	}
	if t.CLI {
		// Android CLI 安装时自己处理许可；补装一次 platform-tools 会写入 licenses/android-sdk-license
		return p.installPackages(ctx, sdkDir, []string{"platform-tools"}, tcs, env, taskID)
	}
	jenv, jpath := p.javaEnv(ctx, tcs)
	args := append([]string{"--sdk_root=" + sdkDir, "--licenses"}, proxyArgs(env)...)
	r, err := ctx.Host.Exec(ctx, &sdk.ExecParams{Command: t.Path, Args: args, Cwd: sdkDir, Env: sdk.MergeEnv(env, jenv), PathPrepend: jpath,
		Console: taskID, Stdin: licenseInput, Title: ctx.T("install.licenses")})
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return sdk.UserError("%s", ctx.T("err.sdkInstall", "--licenses", r.ExitCode))
	}
	return nil
}

// javaEnv 给老 sdkmanager 准备 JAVA_HOME：优先模块选定的 JDK，没有就从内核已知的 JDK 里挑 17 以上版本最高的。
func (p *Plugin) javaEnv(ctx *sdk.Context, tcs map[string]sdk.Toolchain) (map[string]string, []string) {
	if jdk, ok := tcs["jdk"]; ok && jdk.Path != "" {
		env, path := sdk.ToolchainEnv(jdk)
		env["JAVA_HOME"] = jdk.Path
		return env, path
	}
	list, err := ctx.Host.Toolchains(ctx, "jdk")
	if err != nil || len(list) == 0 {
		return nil, nil
	}
	sort.SliceStable(list, func(i, j int) bool { return sdk.CompareVersions(list[i].Version, list[j].Version) > 0 })
	for _, jdk := range list {
		if major(jdk.Version) >= 17 {
			env, path := sdk.ToolchainEnv(jdk)
			env["JAVA_HOME"] = jdk.Path
			return env, path
		}
	}
	return nil, nil
}

func major(v string) int {
	v = strings.TrimPrefix(v, "1.")
	if i := strings.IndexAny(v, ".+_-"); i >= 0 {
		v = v[:i]
	}
	n, _ := strconv.Atoi(v)
	return n
}
