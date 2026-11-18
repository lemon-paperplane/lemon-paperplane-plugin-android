package android

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"
)

// AppProject 是一个安卓 Gradle 子工程（应用或库）的静态解析结果。尽力而为：写成变量、
// 约定插件（buildSrc / build-logic）里的配置解析不到时留空。
type AppProject struct {
	Path          string            `json:"path"`             // Gradle 路径 ":app"
	Name          string            `json:"name"`             // 显示名
	Dir           string            `json:"dir"`              // 绝对路径
	RelDir        string            `json:"relDir,omitempty"` // 相对 Gradle 根，/ 分隔
	BuildFile     string            `json:"buildFile,omitempty"`
	App           bool              `json:"app"` // 应用了 com.android.application
	Namespace     string            `json:"namespace,omitempty"`
	ApplicationID string            `json:"applicationId,omitempty"`
	CompileSdk    string            `json:"compileSdk,omitempty"` // "35"、"36.1"，预览版为代号
	MinSdk        string            `json:"minSdk,omitempty"`
	TargetSdk     string            `json:"targetSdk,omitempty"`
	NdkVersion    string            `json:"ndkVersion,omitempty"`
	NdkPath       string            `json:"ndkPath,omitempty"`
	BuildTools    string            `json:"buildToolsVersion,omitempty"`
	BuildTypes    []string          `json:"buildTypes,omitempty"`
	TypeSuffix    map[string]string `json:"typeSuffix,omitempty"` // buildType → applicationIdSuffix
	Dimensions    []string          `json:"flavorDimensions,omitempty"`
	Flavors       []Flavor          `json:"productFlavors,omitempty"`
	Variants      []string          `json:"variants,omitempty"`
}

// Flavor 是一个 productFlavor。
type Flavor struct {
	Name          string `json:"name"`
	Dimension     string `json:"dimension,omitempty"`
	ApplicationID string `json:"applicationId,omitempty"`
	Suffix        string `json:"applicationIdSuffix,omitempty"`
}

// Variant 是一个构建变体：风味组合 + buildType。
type Variant struct {
	Name      string   // freeDebug
	Flavors   []string // [free]
	BuildType string   // debug
}

const pluginApplication = "com.android.application"

var (
	androidBlockRe    = regexp.MustCompile(`(?m)(?:^|[\s;{(])android\s*\{`)
	defaultConfigRe   = regexp.MustCompile(`\bdefaultConfig\s*\{`)
	buildTypesRe      = regexp.MustCompile(`\bbuildTypes\s*\{`)
	productFlavorsRe  = regexp.MustCompile(`\bproductFlavors\s*\{`)
	pluginsBlockRe    = regexp.MustCompile(`(?m)^\s*plugins\s*\{`)
	compileSdkBlockRe = regexp.MustCompile(`\bcompileSdk\s*\{`)
	minSdkBlockRe     = regexp.MustCompile(`\bminSdk\s*\{`)
	targetSdkBlockRe  = regexp.MustCompile(`\btargetSdk\s*\{`)

	idPluginRe     = regexp.MustCompile(`\bid\s*\(?\s*["']([\w.\-]+)["']\s*\)?`)
	aliasPluginRe  = regexp.MustCompile(`\balias\s*\(\s*libs\.plugins\.([\w.]+)\s*\)`)
	applyFalseRe   = regexp.MustCompile(`\bapply\s*(?:\(\s*)?false`)
	applyPluginRe  = regexp.MustCompile(`\bapply\s*\(?\s*plugin\s*(?::|=)\s*["']([\w.\-]+)["']`)
	applyFromRe    = regexp.MustCompile(`\bapply\s*\(?\s*from\s*(?::|=)\s*(?:file\s*\(\s*)?["']([^"'$]+\.gradle(?:\.kts)?)["']`)
	appIDRe        = regexp.MustCompile(`\bapplicationId\s*(?:=\s*)?\(?\s*["']([\w.]+)["']`)
	appIDSuffixRe  = regexp.MustCompile(`\bapplicationIdSuffix\s*(?:=\s*)?\(?\s*["']([^"']+)["']`)
	namespaceRe    = regexp.MustCompile(`\bnamespace\s*(?:=\s*)?\(?\s*["']([\w.]+)["']`)
	ndkVersionRe   = regexp.MustCompile(`\bndkVersion\s*(?:=\s*)?\(?\s*["']([\w.\-]+)["']`)
	ndkPathRe      = regexp.MustCompile(`\bndkPath\s*(?:=\s*)?\(?\s*["']([^"']+)["']`)
	buildToolsRe   = regexp.MustCompile(`\bbuildToolsVersion\s*(?:=\s*)?\(?\s*["']([\w.\-]+)["']`)
	dimensionRe    = regexp.MustCompile(`\bdimension\s*(?:=\s*)?\(?\s*["']([\w\-]+)["']`)
	flavorDimsRe   = regexp.MustCompile(`\bflavorDimensions\s*(?:\+=|=|\.add\s*\(|\.addAll\s*\(|\()?\s*(?:listOf\s*\(|mutableListOf\s*\(|\[)?\s*((?:["'][\w\-]+["']\s*,?\s*)+)`)
	quotedRe       = regexp.MustCompile(`["']([\w\-]+)["']`)
	releaseCallRe  = regexp.MustCompile(`\brelease\s*\(\s*(\d+)\s*\)`)
	previewCallRe  = regexp.MustCompile(`\bpreview\s*\(\s*["'](\w+)["']\s*\)`)
	minorAPIRe     = regexp.MustCompile(`\bminorApiLevel\s*(?:=\s*)?(\d+)`)
	compileMinorRe = regexp.MustCompile(`\bcompileSdkMinor\s*(?:=\s*)?\(?\s*(\d+)`)
	compilePrevRe  = regexp.MustCompile(`\bcompileSdkPreview\s*(?:=\s*)?\(?\s*["'](\w+)["']`)
	numberRe       = regexp.MustCompile(`^\d+(?:\.\d+)?$`)
	identRe        = regexp.MustCompile(`[A-Za-z_][\w]*`)
	catalogRefRe   = regexp.MustCompile(`\blibs\.versions\.([\w.]+?)(?:\.get\s*\(\s*\)|\.toString|\.to|\s|$)`)
	keyRefRe       = regexp.MustCompile(`(?:extra|ext|property|findProperty|getProperty|properties)\s*(?:\[\s*|\(\s*)["'](\w+)["']`)
	scriptVarRe    = regexp.MustCompile(`(?m)(?:^|\s|\.|\{)(?:def\s+|val\s+|var\s+|const\s+val\s+|ext\.|extra\[\s*"|set\(\s*")?([A-Za-z_][\w]*)"?\s*\]?\s*(?:=|,)\s*["']?([\w.\-]+)["']?\s*\)?\s*$`)
	sdkRefRe       = regexp.MustCompile(`(?m)\b(compileSdk|compileSdkVersion|minSdk|minSdkVersion|targetSdk|targetSdkVersion)\b\s*(?:=\s*|\(\s*|\s+)([^\s{}()][^\n;]*)`)
	nameEntryRe    = regexp.MustCompile(`(?:getByName|create|named|maybeCreate|register)\s*\(\s*["']([\w\-]+)["']`)
	bareEntryRe    = regexp.MustCompile(`^["']?([A-Za-z_][\w\-]*)["']?$`)
	notEntryNames  = map[string]bool{"all": true, "configureEach": true, "each": true, "forEach": true, "matching": true, "withType": true, "whenObjectAdded": true, "if": true, "else": true, "android": true}
)

// stripComments 去掉 // 与 /* */ 注释，保留字符串原样（含三引号字符串），保留换行。
func stripComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	n := len(src)
	for i := 0; i < n; i++ {
		c := src[i]
		switch {
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				i++
			}
			if i < n {
				b.WriteByte('\n')
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			i += 2
			for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
				if src[i] == '\n' {
					b.WriteByte('\n')
				}
				i++
			}
			i++
		case c == '"' || c == '\'':
			if i+2 < n && src[i+1] == c && src[i+2] == c {
				end := strings.Index(src[i+3:], string([]byte{c, c, c}))
				if end < 0 {
					b.WriteString(src[i:])
					return b.String()
				}
				b.WriteString(src[i : i+3+end+3])
				i += 3 + end + 2
				continue
			}
			j := i + 1
			for j < n && src[j] != c && src[j] != '\n' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j >= n {
				j = n - 1
			}
			b.WriteString(src[i : j+1])
			i = j
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// blockEnd 返回 src[open]=='{' 对应的 '}' 位置（跳过字符串里的括号）；没有配对时返回 len(src)。
func blockEnd(src string, open int) int {
	depth := 0
	for i := open; i < len(src); i++ {
		switch c := src[i]; c {
		case '"', '\'':
			j := i + 1
			for j < len(src) && src[j] != c && src[j] != '\n' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			i = j
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return len(src)
}

// blocks 返回所有匹配 re（以 '{' 结尾）的块的内容。
func blocks(src string, re *regexp.Regexp) []string {
	var out []string
	last := 0
	for _, loc := range re.FindAllStringIndex(src, -1) {
		if loc[0] < last {
			continue
		}
		open := strings.LastIndexByte(src[loc[0]:loc[1]], '{')
		if open < 0 {
			continue
		}
		open += loc[0]
		end := blockEnd(src, open)
		out = append(out, src[open+1:min(end, len(src))])
		last = end
	}
	return out
}

// removeBlocks 去掉所有匹配 re 的块（含块头），用于只看外层配置。
func removeBlocks(src string, re *regexp.Regexp) string {
	var b strings.Builder
	last := 0
	for _, loc := range re.FindAllStringIndex(src, -1) {
		if loc[0] < last {
			continue
		}
		open := strings.LastIndexByte(src[loc[0]:loc[1]], '{') + loc[0]
		end := blockEnd(src, open)
		b.WriteString(src[last:loc[0]])
		last = min(end+1, len(src))
	}
	b.WriteString(src[last:])
	return b.String()
}

// namedEntry 是 buildTypes / productFlavors 里的一项。
type namedEntry struct {
	Name string
	Body string
}

// topEntries 列出容器块（buildTypes { ... }）里第一层的命名项：`debug {`、`getByName("debug") {`、`create("free") {`。
func topEntries(body string) []namedEntry {
	var out []namedEntry
	segStart := 0
	for i := 0; i < len(body); i++ {
		switch c := body[i]; c {
		case '"', '\'':
			j := i + 1
			for j < len(body) && body[j] != c && body[j] != '\n' {
				if body[j] == '\\' {
					j++
				}
				j++
			}
			i = j
		case '\n', ';', '}':
			segStart = i + 1
		case '{':
			head := strings.TrimSpace(body[segStart:i])
			end := blockEnd(body, i)
			name := ""
			if m := nameEntryRe.FindStringSubmatch(head); m != nil {
				name = m[1]
			} else if m := bareEntryRe.FindStringSubmatch(head); m != nil && !notEntryNames[m[1]] {
				name = m[1]
			}
			if name != "" {
				out = append(out, namedEntry{Name: name, Body: body[i+1 : min(end, len(body))]})
			}
			i = end
			segStart = end + 1
		}
	}
	return out
}

// readScriptWithIncludes 读取构建文件并展开 `apply from: 'x.gradle'`（只展开一层、本地相对路径），去注释。
func readScriptWithIncludes(file string) string {
	src := stripComments(sdk.ReadText(file, 0))
	dir := filepath.Dir(file)
	var extra []string
	seen := map[string]bool{}
	for _, m := range applyFromRe.FindAllStringSubmatch(src, -1) {
		p := m[1]
		if strings.Contains(p, "://") {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, filepath.FromSlash(p))
		}
		if seen[p] || len(seen) >= 6 {
			continue
		}
		seen[p] = true
		if t := sdk.ReadText(p, 512*1024); t != "" {
			extra = append(extra, stripComments(t))
		}
	}
	if len(extra) == 0 {
		return src
	}
	return src + "\n" + strings.Join(extra, "\n")
}

// appliedPlugins 返回脚本实际应用的插件 id（`apply false` 的不算）。cat 用于展开 alias(libs.plugins.x)。
func appliedPlugins(src string, cat *catalog) []string {
	var out []string
	add := func(id string) {
		for _, x := range out {
			if x == id {
				return
			}
		}
		out = append(out, id)
	}
	for _, body := range blocks(src, pluginsBlockRe) {
		for _, line := range splitStatements(body) {
			if applyFalseRe.MatchString(line) {
				continue
			}
			if m := idPluginRe.FindStringSubmatch(line); m != nil {
				add(m[1])
			}
			if m := aliasPluginRe.FindStringSubmatch(line); m != nil && cat != nil {
				if id := cat.Plugins[catalogKey(m[1])]; id != "" {
					add(id)
				}
			}
		}
	}
	for _, m := range applyPluginRe.FindAllStringSubmatch(src, -1) {
		add(m[1])
	}
	return out
}

// splitStatements 按换行和分号切分（plugins 块里一行一个声明）。
func splitStatements(body string) []string {
	return strings.FieldsFunc(body, func(r rune) bool { return r == '\n' || r == ';' })
}

// isAndroidApp 判断脚本是不是安卓应用：应用了 com.android.application，
// 或者（约定插件的写法）android 块里写了 applicationId。
func isAndroidApp(src string, cat *catalog) bool {
	for _, id := range appliedPlugins(src, cat) {
		if id == pluginApplication || id == "android" {
			return true
		}
	}
	for _, body := range blocks(src, androidBlockRe) {
		if appIDRe.MatchString(body) {
			return true
		}
	}
	return false
}

// isAndroidScript 判断脚本是不是安卓工程（应用或库）。
func isAndroidScript(src string, cat *catalog) bool {
	for _, id := range appliedPlugins(src, cat) {
		if strings.HasPrefix(id, "com.android.") || id == "android" || id == "android-library" {
			return true
		}
	}
	return androidBlockRe.MatchString(src)
}

// parseAndroidScript 解析构建脚本里的 android 配置。vars 用于展开变量写法的 SDK 版本。
func parseAndroidScript(src string, vars map[string]string, cat *catalog) *AppProject {
	p := &AppProject{TypeSuffix: map[string]string{}}
	p.App = isAndroidApp(src, cat)
	bodies := blocks(src, androidBlockRe)
	android := strings.Join(bodies, "\n")
	if android == "" {
		android = src
	}
	if m := namespaceRe.FindStringSubmatch(android); m != nil {
		p.Namespace = m[1]
	}
	if m := ndkVersionRe.FindStringSubmatch(android); m != nil {
		p.NdkVersion = m[1]
	}
	if m := ndkPathRe.FindStringSubmatch(android); m != nil {
		p.NdkPath = m[1]
	}
	if m := buildToolsRe.FindStringSubmatch(android); m != nil {
		p.BuildTools = m[1]
	}
	defaults := strings.Join(blocks(android, defaultConfigRe), "\n")
	if m := appIDRe.FindStringSubmatch(defaults); m != nil {
		p.ApplicationID = m[1]
	}
	// 外层（不含 buildTypes / productFlavors）的 SDK 版本
	outer := removeBlocks(removeBlocks(android, buildTypesRe), productFlavorsRe)
	p.CompileSdk = compileSdkOf(outer, vars, cat)
	p.MinSdk = sdkLevelOf(defaults, "minSdk", minSdkBlockRe, vars, cat)
	if p.MinSdk == "" {
		p.MinSdk = sdkLevelOf(outer, "minSdk", minSdkBlockRe, vars, cat)
	}
	p.TargetSdk = sdkLevelOf(defaults, "targetSdk", targetSdkBlockRe, vars, cat)
	if p.TargetSdk == "" {
		p.TargetSdk = sdkLevelOf(outer, "targetSdk", targetSdkBlockRe, vars, cat)
	}
	// buildTypes：debug / release 总是存在
	types := []string{"debug", "release"}
	for _, body := range blocks(android, buildTypesRe) {
		for _, e := range topEntries(body) {
			if !contains(types, e.Name) {
				types = append(types, e.Name)
			}
			if m := appIDSuffixRe.FindStringSubmatch(e.Body); m != nil {
				p.TypeSuffix[e.Name] = m[1]
			}
		}
	}
	p.BuildTypes = types
	for _, m := range flavorDimsRe.FindAllStringSubmatch(android, -1) {
		for _, q := range quotedRe.FindAllStringSubmatch(m[1], -1) {
			if !contains(p.Dimensions, q[1]) {
				p.Dimensions = append(p.Dimensions, q[1])
			}
		}
	}
	for _, body := range blocks(android, productFlavorsRe) {
		for _, e := range topEntries(body) {
			f := Flavor{Name: e.Name}
			if m := dimensionRe.FindStringSubmatch(e.Body); m != nil {
				f.Dimension = m[1]
			}
			if m := appIDRe.FindStringSubmatch(e.Body); m != nil {
				f.ApplicationID = m[1]
			}
			if m := appIDSuffixRe.FindStringSubmatch(e.Body); m != nil {
				f.Suffix = m[1]
			}
			p.Flavors = append(p.Flavors, f)
		}
	}
	for _, v := range p.variants() {
		p.Variants = append(p.Variants, v.Name)
	}
	if len(p.TypeSuffix) == 0 {
		p.TypeSuffix = nil
	}
	return p
}

// compileSdkOf 解析 compileSdk 的各种写法，返回平台版本（"35"、"36.1"）或预览代号。
func compileSdkOf(src string, vars map[string]string, cat *catalog) string {
	// 新写法：compileSdk { version = release(36) { minorApiLevel = 1 } } / preview("CinnamonBun")
	for _, body := range blocks(src, compileSdkBlockRe) {
		if m := releaseCallRe.FindStringSubmatch(body); m != nil {
			v := m[1]
			if mm := minorAPIRe.FindStringSubmatch(body); mm != nil && mm[1] != "0" {
				v += "." + mm[1]
			}
			return v
		}
		if m := previewCallRe.FindStringSubmatch(body); m != nil {
			return m[1]
		}
	}
	rest := removeBlocks(src, compileSdkBlockRe)
	v := ""
	for _, m := range sdkRefRe.FindAllStringSubmatch(rest, -1) {
		if m[1] != "compileSdk" && m[1] != "compileSdkVersion" {
			continue
		}
		if r := resolveLevel(m[2], vars, cat); r != "" {
			v = r
			break
		}
	}
	if v == "" {
		if m := compilePrevRe.FindStringSubmatch(rest); m != nil {
			return m[1]
		}
		return ""
	}
	if m := compileMinorRe.FindStringSubmatch(rest); m != nil && m[1] != "0" && !strings.Contains(v, ".") {
		v += "." + m[1]
	}
	return v
}

// sdkLevelOf 解析 minSdk / targetSdk（含 xxxVersion 旧写法与 minSdk { version = release(24) } 新写法）。
func sdkLevelOf(src, key string, blockRe *regexp.Regexp, vars map[string]string, cat *catalog) string {
	for _, body := range blocks(src, blockRe) {
		if m := releaseCallRe.FindStringSubmatch(body); m != nil {
			return m[1]
		}
		if m := previewCallRe.FindStringSubmatch(body); m != nil {
			return m[1]
		}
	}
	rest := removeBlocks(src, blockRe)
	for _, m := range sdkRefRe.FindAllStringSubmatch(rest, -1) {
		if m[1] != key && m[1] != key+"Version" {
			continue
		}
		if r := resolveLevel(m[2], vars, cat); r != "" {
			return r
		}
	}
	return ""
}

// resolveLevel 把 SDK 版本表达式解析成数字：35、"android-35"、变量、版本目录引用、.toInt() 之类的尾巴。
func resolveLevel(expr string, vars map[string]string, cat *catalog) string {
	e := strings.TrimSpace(expr)
	e = strings.TrimSuffix(e, ")")
	e = strings.TrimSpace(strings.TrimPrefix(e, "("))
	e = strings.Trim(e, `"'`)
	e = strings.TrimPrefix(e, "android-")
	if numberRe.MatchString(e) {
		return e
	}
	if cat != nil {
		if m := catalogRefRe.FindStringSubmatch(e + " "); m != nil {
			if v := cat.Versions[catalogKey(m[1])]; v != "" {
				return strings.TrimPrefix(v, "android-")
			}
		}
	}
	lookup := func(k string) string {
		if v, ok := vars[k]; ok {
			v = strings.TrimPrefix(strings.Trim(v, `"'`), "android-")
			if numberRe.MatchString(v) {
				return v
			}
		}
		return ""
	}
	if m := keyRefRe.FindStringSubmatch(e); m != nil {
		if v := lookup(m[1]); v != "" {
			return v
		}
	}
	// rootProject.ext.compileSdkVersion / Versions.compileSdk / compileSdkVersion.toInteger()
	ids := identRe.FindAllString(e, -1)
	for i := len(ids) - 1; i >= 0; i-- {
		switch ids[i] {
		case "toInt", "toInteger", "get", "toString", "parseInt", "Integer", "as", "Int", "it":
			continue
		}
		if v := lookup(ids[i]); v != "" {
			return v
		}
	}
	return ""
}

// collectVars 收集 gradle.properties 与脚本里的简单变量（ext / extra / val / def），用于展开 SDK 版本。
func collectVars(propsFiles []string, scripts ...string) map[string]string {
	vars := map[string]string{}
	for _, f := range propsFiles {
		for k, v := range parseProperties(sdk.ReadText(f, 256*1024)) {
			if _, ok := vars[k]; !ok {
				vars[k] = v
			}
		}
	}
	for _, src := range scripts {
		for _, m := range scriptVarRe.FindAllStringSubmatch(src, -1) {
			if _, ok := vars[m[1]]; !ok {
				vars[m[1]] = m[2]
			}
		}
	}
	return vars
}

// parseProperties 解析 .properties（只处理 key=value / key:value，支持 \: 转义）。
func parseProperties(text string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == '!' {
			continue
		}
		i := strings.IndexAny(line, "=:")
		if i < 0 {
			continue
		}
		k := strings.TrimSpace(line[:i])
		v := strings.TrimSpace(line[i+1:])
		v = strings.NewReplacer(`\:`, ":", `\=`, "=", `\\`, `\`).Replace(v)
		out[k] = v
	}
	return out
}

// variants 返回构建变体：风味按维度做笛卡尔积，再乘 buildType。
func (p *AppProject) variants() []Variant {
	types := p.BuildTypes
	if len(types) == 0 {
		types = []string{"debug", "release"}
	}
	groups := p.flavorGroups()
	combos := [][]string{{}}
	for _, g := range groups {
		var next [][]string
		for _, c := range combos {
			for _, f := range g {
				next = append(next, append(append([]string{}, c...), f))
			}
		}
		combos = next
	}
	var out []Variant
	for _, c := range combos {
		for _, t := range types {
			out = append(out, Variant{Name: variantName(c, t), Flavors: c, BuildType: t})
		}
	}
	return out
}

// flavorGroups 按维度分组风味（维度按 flavorDimensions 声明的顺序；没写维度的归入唯一维度）。
func (p *AppProject) flavorGroups() [][]string {
	if len(p.Flavors) == 0 {
		return nil
	}
	dims := append([]string{}, p.Dimensions...)
	for _, f := range p.Flavors {
		if f.Dimension != "" && !contains(dims, f.Dimension) {
			dims = append(dims, f.Dimension)
		}
	}
	if len(dims) == 0 {
		dims = []string{""}
	}
	byDim := map[string][]string{}
	for _, f := range p.Flavors {
		d := f.Dimension
		if d == "" {
			d = dims[0]
		}
		byDim[d] = append(byDim[d], f.Name)
	}
	var out [][]string
	for _, d := range dims {
		if len(byDim[d]) > 0 {
			out = append(out, byDim[d])
		}
	}
	return out
}

// variantName 拼变体名：freeArmDebug。
func variantName(flavors []string, buildType string) string {
	var b strings.Builder
	for i, f := range flavors {
		if i == 0 {
			b.WriteString(f)
		} else {
			b.WriteString(capitalize(f))
		}
	}
	if b.Len() == 0 {
		return buildType
	}
	b.WriteString(capitalize(buildType))
	return b.String()
}

// findVariant 按名字（大小写不敏感）找变体。
func (p *AppProject) findVariant(name string) (Variant, bool) {
	for _, v := range p.variants() {
		if strings.EqualFold(v.Name, name) {
			return v, true
		}
	}
	return Variant{}, false
}

// packageName 推算某个变体安装后的包名：applicationId（风味可覆盖）+ 风味后缀 + buildType 后缀。
func (p *AppProject) packageName(v Variant) string {
	id := p.ApplicationID
	if id == "" {
		id = p.Namespace
	}
	suffix := ""
	for _, fn := range v.Flavors {
		for _, f := range p.Flavors {
			if f.Name != fn {
				continue
			}
			if f.ApplicationID != "" {
				id = f.ApplicationID
			}
			suffix += normSuffix(f.Suffix)
		}
	}
	suffix += normSuffix(p.TypeSuffix[v.BuildType])
	if id == "" {
		return ""
	}
	return id + suffix
}

// normSuffix：每段后缀不以 "." 开头时 AGP 会补一个点。
func normSuffix(s string) string {
	if s == "" {
		return ""
	}
	parts := strings.Split(s, ".")
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return ""
	}
	return "." + strings.Join(out, ".")
}

// taskPath 返回子工程上的任务路径：":app:installDebug"，根工程为 ":installDebug"。
func taskPath(projectPath, task string) string {
	if projectPath == "" || projectPath == ":" {
		return ":" + task
	}
	return projectPath + ":" + task
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 版本目录（gradle/libs.versions.toml）：只取 versions 与 plugins。

type catalog struct {
	Versions map[string]string
	Plugins  map[string]string // 别名（规范化）→ 插件 id
}

var (
	tomlKVRe    = regexp.MustCompile(`^([\w.\-]+)\s*=\s*(.+)$`)
	tomlStrRe   = regexp.MustCompile(`^"([^"]*)"`)
	tomlFieldRe = regexp.MustCompile(`([\w.]+)\s*=\s*"([^"]*)"`)
	accessor    = strings.NewReplacer("-", ".", "_", ".")
)

// catalogKey 规范化版本目录别名：Gradle 生成访问器时 - 与 _ 都变成 .（libs.plugins.android.application）。
func catalogKey(k string) string { return strings.ToLower(accessor.Replace(k)) }

func parseCatalog(file string) *catalog {
	c := &catalog{Versions: map[string]string{}, Plugins: map[string]string{}}
	f, err := os.Open(file)
	if err != nil {
		return c
	}
	defer f.Close()
	section := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.Trim(line, "[] ")
			continue
		}
		m := tomlKVRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key, val := catalogKey(m[1]), strings.TrimSpace(m[2])
		switch section {
		case "versions":
			if sm := tomlStrRe.FindStringSubmatch(val); sm != nil {
				c.Versions[key] = sm[1]
			} else if numberRe.MatchString(val) {
				c.Versions[key] = val
			} else {
				for _, fm := range tomlFieldRe.FindAllStringSubmatch(val, -1) {
					if fm[1] == "strictly" || fm[1] == "require" || fm[1] == "prefer" {
						c.Versions[key] = fm[2]
						break
					}
				}
			}
		case "plugins":
			if sm := tomlStrRe.FindStringSubmatch(val); sm != nil {
				id, _, _ := strings.Cut(sm[1], ":")
				c.Plugins[key] = id
			} else {
				for _, fm := range tomlFieldRe.FindAllStringSubmatch(val, -1) {
					if fm[1] == "id" {
						c.Plugins[key] = fm[2]
					}
				}
			}
		}
	}
	return c
}

// ---------------------------------------------------------------------------
// 文件定位

// findBuildFile 返回目录下的 build.gradle(.kts)。
func findBuildFile(dir string) string {
	for _, n := range []string{"build.gradle.kts", "build.gradle"} {
		if p := filepath.Join(dir, n); sdk.FileExists(p) {
			return p
		}
	}
	return ""
}

// gradleProject 是从 gradle 插件拿到的子工程（模型或 facet 数据）。
type gradleProject struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Dir       string `json:"dir"`
	RelDir    string `json:"relDir"`
	BuildFile string `json:"buildFile"`
	Android   bool   `json:"android"`
}

// gradleProjects 读取 gradle 插件的模型（analyze 时有），没有就用 facet 数据里的子工程清单。
func gradleProjects(m *sdk.ModuleInfo) []gradleProject {
	var model struct {
		Projects []gradleProject `json:"projects"`
	}
	if m.Model("gradle", &model) && len(model.Projects) > 0 {
		for i := range model.Projects {
			fixProject(&model.Projects[i], m.Dir)
		}
		return model.Projects
	}
	if f := m.Facet("gradle"); f != nil {
		return facetProjects(f, m.Dir)
	}
	return nil
}

// facetProjects 解析 gradle facet 的 data.projects：[{path, dir(相对), android}]。
func facetProjects(f *sdk.Facet, root string) []gradleProject {
	list, _ := f.Data["projects"].([]any)
	var out []gradleProject
	for _, x := range list {
		e, _ := x.(map[string]any)
		if e == nil {
			continue
		}
		gp := gradleProject{}
		gp.Path, _ = e["path"].(string)
		gp.RelDir, _ = e["dir"].(string)
		gp.Android, _ = e["android"].(bool)
		fixProject(&gp, root)
		out = append(out, gp)
	}
	return out
}

func fixProject(gp *gradleProject, root string) {
	if gp.Dir == "" {
		gp.Dir = filepath.Join(root, filepath.FromSlash(gp.RelDir))
	}
	if gp.RelDir == "" && gp.Dir != root {
		if rel, err := filepath.Rel(root, gp.Dir); err == nil && rel != "." {
			gp.RelDir = filepath.ToSlash(rel)
		}
	}
	if gp.BuildFile == "" {
		gp.BuildFile = findBuildFile(gp.Dir)
	}
	if gp.Name == "" {
		if i := strings.LastIndexByte(gp.Path, ':'); i >= 0 && i < len(gp.Path)-1 {
			gp.Name = gp.Path[i+1:]
		} else {
			gp.Name = filepath.Base(gp.Dir)
		}
	}
}

// scanProjects 解析 Gradle 根目录下所有安卓子工程（应用和库），按 Gradle 路径排序。
func scanProjects(root string, gps []gradleProject) []*AppProject {
	cat := parseCatalog(filepath.Join(root, "gradle", "libs.versions.toml"))
	rootFile := findBuildFile(root)
	rootSrc := stripComments(sdk.ReadText(rootFile, 0))
	var out []*AppProject
	for _, gp := range gps {
		if gp.BuildFile == "" {
			continue
		}
		src := readScriptWithIncludes(gp.BuildFile)
		if !gp.Android && !isAndroidScript(src, cat) {
			continue
		}
		vars := collectVars([]string{filepath.Join(root, "gradle.properties"), filepath.Join(gp.Dir, "gradle.properties")}, src, rootSrc)
		ap := parseAndroidScript(src, vars, cat)
		ap.Path, ap.Name, ap.Dir, ap.RelDir, ap.BuildFile = gp.Path, gp.Name, gp.Dir, gp.RelDir, gp.BuildFile
		if ap.Path == ":" && ap.Name == "" {
			ap.Name = filepath.Base(root)
		}
		if !ap.App && !isAndroidScript(src, cat) {
			continue
		}
		out = append(out, ap)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
