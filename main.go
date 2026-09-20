//go:build windows

// SubtitleCat 番号字幕匹配下载器
//
// 功能：
//  1. 弹出 Windows 原生文件夹选择框，任选任意磁盘下的目录
//  2. 列出该目录下的所有子文件夹（番号目录）
//  3. 已存在字幕文件的文件夹自动跳过
//  4. 用文件夹名在 subtitlecat.com 搜索字幕
//  5. 结果按 SIZE 从大到小排序，优先下载最大的
//  6. 语言优先 Chinese (Simplified)，其次 Chinese (Traditional)，都没有则跳过
//  7. 下载的字幕保存为「文件夹同名.srt」
//
// 用法：
//
//	SubtitleCatMatcher.exe            双击运行，弹出文件夹选择框
//	SubtitleCatMatcher.exe "D:\影片"  直接指定目录（跳过选择框）
//	SubtitleCatMatcher.exe -h         查看全部参数
package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

// ---------------------------------------------------------------- 常量

const (
	baseURL = "https://www.subtitlecat.com/"
	ua      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"

	langSimplified  = "Chinese (Simplified)"
	langTraditional = "Chinese (Traditional)"
)

// 字幕文件扩展名（用于判断文件夹里是否已有字幕）
var subtitleExts = map[string]bool{
	".srt": true, ".ass": true, ".ssa": true, ".sub": true,
	".vtt": true, ".smi": true, ".idx": true, ".ttml": true, ".sbv": true,
}

// ---------------------------------------------------------------- ANSI 颜色

const (
	cReset = "\x1b[0m"
	cDim   = "\x1b[90m"
	cRed   = "\x1b[31m"
	cGreen = "\x1b[32m"
	cYell  = "\x1b[33m"
	cCyan  = "\x1b[36m"
	cBold  = "\x1b[1m"
)

// ---------------------------------------------------------------- 正则

var (
	rowRe    = regexp.MustCompile(`(?is)<tr>(.*?)</tr>`)
	linkRe   = regexp.MustCompile(`(?is)<a\s+href="([^"]+)"[^>]*>(.*?)</a>`)
	metricRe = regexp.MustCompile(`(?is)sub-table__metric-value">\s*([^<]*?)\s*<`)
	srtRe    = regexp.MustCompile(`(?is)<a\s[^>]*href="([^"]+\.srt)"[^>]*>\s*Download\s*</a>`)
	tagRe    = regexp.MustCompile(`(?s)<[^>]+>`)

	// 番号格式：可选纯数字前缀 + 2~10 位字母 + 可选分隔符 + 2~6 位数字
	// 例：SNOS-115、SSIS-001、259LUXU-1234、HEYZO-1234、MIDE-123-C
	codeRe = regexp.MustCompile(`(?i)^\d{0,5}[A-Z]{2,10}[-_ ]?\d{2,6}`)

	// 用于归一化比对：去掉所有非字母数字
	nonAlnumRe = regexp.MustCompile(`[^A-Za-z0-9]`)

	// 任意时间轴（宽松）：兼容全角冒号、-> 与 -->、. 与 , 作毫秒分隔
	// 用它判断"这是不是字幕"，比死抠 "-->" 靠谱
	tsAnyRe = regexp.MustCompile(`\d{1,2}[:：]\d{2}[:：]\d{2}[.,]\d{1,3}\s*-+>`)

	// 整行时间轴由 parseTimestampLine 逐行处理，这里不再用正则硬套
	// （源文件存在 HH:MM:MM:SS.mmm 这种把分钟重复一次的写法，正则套不住）

	// 不可见字符：站点部分字幕会把零宽空格塞进时间轴数字中间
	// （00：01：46.0\u200b\u200b00），导致任何解析器都读不出时间。
	// 故意不动 U+200D（ZWJ）——它在 emoji / 印度语系里有实际语义。
	zeroWidthRe = regexp.MustCompile("[\u00ad\u200b\u200c\u200e\u200f\u2060\ufeff]")
)

// looksLikeCode 判断文件夹名是否像番号
func looksLikeCode(name string) bool { return codeRe.MatchString(name) }

// normalizeCode 归一化番号，便于宽松比对（SNOS-115 == snos 115）
func normalizeCode(s string) string {
	return strings.ToUpper(nonAlnumRe.ReplaceAllString(s, ""))
}

// ---------------------------------------------------------------- 数据结构

type searchResult struct {
	name    string
	href    string
	size    int64
	sizeStr string
}

type outcome struct {
	folder string
	status string // ok / skip-has-sub / no-result / err
	detail string
}

// ---------------------------------------------------------------- HTTP

var httpClient = &http.Client{Timeout: 45 * time.Second}

var (
	lastRequest time.Time
	requestGap  = 700 * time.Millisecond // 可用 -delay 调整
)

// politeDelay 控制请求频率，避免给站点造成压力
func politeDelay() {
	if !lastRequest.IsZero() {
		if d := requestGap - time.Since(lastRequest); d > 0 {
			time.Sleep(d)
		}
	}
	lastRequest = time.Now()
}

func fetch(rawURL, referer string) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		politeDelay()

		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", ua)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9,zh-CN;q=0.8")
		if referer != "" {
			req.Header.Set("Referer", referer)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
		} else {
			body, rerr := io.ReadAll(resp.Body)
			resp.Body.Close()
			switch {
			case rerr != nil:
				lastErr = rerr
			case resp.StatusCode != http.StatusOK:
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			default:
				return body, nil
			}
		}
		if attempt < 3 {
			time.Sleep(time.Duration(attempt) * 900 * time.Millisecond)
		}
	}
	return nil, lastErr
}

// ---------------------------------------------------------------- 解析

func parseSize(s string) int64 {
	s = strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "KB"):
		mult, s = 1024, strings.TrimSpace(strings.TrimSuffix(s, "KB"))
	case strings.HasSuffix(s, "MB"):
		mult, s = 1024*1024, strings.TrimSpace(strings.TrimSuffix(s, "MB"))
	case strings.HasSuffix(s, "GB"):
		mult, s = 1024*1024*1024, strings.TrimSpace(strings.TrimSuffix(s, "GB"))
	case strings.HasSuffix(s, "B"):
		mult, s = 1, strings.TrimSpace(strings.TrimSuffix(s, "B"))
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return int64(f * float64(mult))
}

// parseSearch 从搜索结果页提取所有条目
func parseSearch(page string) []searchResult {
	var out []searchResult
	for _, m := range rowRe.FindAllStringSubmatch(page, -1) {
		row := m[1]
		lm := linkRe.FindStringSubmatch(row)
		if lm == nil {
			continue
		}
		href := strings.TrimSpace(lm[1])
		if !strings.Contains(href, "subs/") || !strings.HasSuffix(href, ".html") {
			continue
		}
		name := strings.TrimSpace(tagRe.ReplaceAllString(lm[2], ""))

		metrics := metricRe.FindAllStringSubmatch(row, -1)
		if len(metrics) == 0 {
			continue
		}
		sizeStr := strings.TrimSpace(metrics[0][1])

		out = append(out, searchResult{
			name:    name,
			href:    href,
			size:    parseSize(sizeStr),
			sizeStr: sizeStr,
		})
	}
	return out
}

// findLangDownload 在详情页中查找指定语言的可下载 .srt 链接
func findLangDownload(page, langName string) string {
	blocks := strings.Split(page, `<div class="sub-single">`)
	for _, b := range blocks[1:] {
		if i := strings.Index(b, "<!-- ./Sub single -->"); i >= 0 {
			b = b[:i]
		}
		if !strings.Contains(b, ">"+langName+"<") {
			continue
		}
		if m := srtRe.FindStringSubmatch(b); m != nil {
			return m[1]
		}
	}
	return ""
}

func resolveURL(href string) string {
	base, _ := url.Parse(baseURL)
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	return base.ResolveReference(u).String()
}

// ---------------------------------------------------------------- 字幕校验与规范化

// decodeToUTF8 把带 BOM / UTF-16 的内容转成 UTF-8 文本。
// 第二个返回值表示是否发生过转换。
func decodeToUTF8(data []byte) (string, bool) {
	if len(data) >= 2 {
		switch {
		case data[0] == 0xFF && data[1] == 0xFE:
			return decodeUTF16(data[2:], binary.LittleEndian), true
		case data[0] == 0xFE && data[1] == 0xFF:
			return decodeUTF16(data[2:], binary.BigEndian), true
		}
	}
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return string(data[3:]), true
	}
	return string(data), false
}

func decodeUTF16(b []byte, order binary.ByteOrder) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, order.Uint16(b[i:]))
	}
	return string(utf16.Decode(u))
}

// looksLikeSubtitle 判断下载内容到底是字幕，还是站点拦截/错误页。
//
// 早期版本死抠 "-->"，结果把 aisubs.app 这类用 "->" 的合法字幕全部误判成
// "被站点拦截"。现在改为：先排除 HTML 错误页，再用宽松的时间轴正则确认。
func looksLikeSubtitle(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	text, _ := decodeToUTF8(data)

	head := text
	if len(head) > 2048 {
		head = head[:2048]
	}
	low := strings.ToLower(head)
	if strings.Contains(low, "<!doctype") || strings.Contains(low, "<html") ||
		strings.Contains(low, "access denied") || strings.Contains(low, "<head>") {
		return false
	}
	return tsAnyRe.MatchString(text)
}

// padLeft 左侧补零到至少 n 位（不截断）—— 用于时/分/秒
func padLeft(s string, n int) string {
	for len(s) < n {
		s = "0" + s
	}
	return s
}

// padMS 把毫秒补成 3 位。毫秒是小数，所以 .5 → 500（右补），
// 而 .2900 这种多余的尾数按前 3 位截断成 290。
func padMS(s string) string {
	for len(s) < 3 {
		s += "0"
	}
	if len(s) > 3 {
		s = s[:3]
	}
	return s
}

// allDigits 判断 s 是否是非空纯数字且不超过 n 位
func allDigits(s string, n int) bool {
	if s == "" || len(s) > n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseTimestamp 解析单个时间戳，返回标准 SRT 写法（HH:MM:SS,mmm）。
//
// 兼容真实数据里见过的各种脏写法：
//
//	00：00：00.000   全角冒号 + 点号毫秒
//	0:0:1.5          位数不足
//	02:12:12:50.280  分钟被重复了一次（折叠成 02:12:50.280）
func parseTimestamp(s string) (string, bool) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "：", ":")
	s = strings.ReplaceAll(s, ".", ",")

	groups := strings.Split(s, ":")
	if len(groups) != 3 && len(groups) != 4 {
		return "", false
	}

	// 最后一段形如 SS,mmm
	secMS := strings.SplitN(groups[len(groups)-1], ",", 2)
	if len(secMS) != 2 {
		return "", false
	}
	nums := append(append([]string{}, groups[:len(groups)-1]...), secMS[0])
	ms := secMS[1]

	// 毫秒允许超长（源文件出现过 .2900），后面统一截到 3 位
	if !allDigits(ms, 6) {
		return "", false
	}
	for _, n := range nums {
		if !allDigits(n, 3) {
			return "", false
		}
	}

	// 4 段：HH:MM:MM:SS —— 只有重复段确实相等才折叠，避免把正常内容猜坏
	if len(nums) == 4 {
		if nums[1] != nums[2] {
			return "", false
		}
		nums = []string{nums[0], nums[1], nums[3]}
	}

	return fmt.Sprintf("%s:%s:%s,%s",
		padLeft(nums[0], 2), padLeft(nums[1], 2), padLeft(nums[2], 2), padMS(ms)), true
}

// parseTimestampLine 尝试把一整行时间轴转成标准写法。
// 不是时间轴行就返回 ok=false，正文原样保留。
func parseTimestampLine(line string) (string, bool) {
	body, cr := line, ""
	if strings.HasSuffix(body, "\r") {
		body, cr = body[:len(body)-1], "\r"
	}

	left, right, found := "", "", false
	for _, sep := range []string{"-->", "->"} {
		if i := strings.Index(body, sep); i >= 0 {
			left, right, found = body[:i], body[i+len(sep):], true
			break
		}
	}
	if !found {
		return line, false
	}

	start, okS := parseTimestamp(left)
	if !okS {
		return line, false
	}
	end, okE := parseTimestamp(right)
	if !okE {
		return line, false
	}
	return start + " --> " + end + cr, true
}

// normalizeSubtitle 逐行把非标准时间轴统一成标准 SRT：
//
//	00：00：00.000-> 00：00：02.000   →   00:00:00,000 --> 00:00:02,000
//
// 这很关键：ffmpeg（Jellyfin / Emby 的字幕解析）只认 "-->" 和半角冒号，
// 原样保存的话字幕在播放器里根本不会显示。
// 同时清掉零宽空格、把 UTF-16 / 带 BOM 的内容转成 UTF-8。
// 只重写能被完整解析为时间轴的行，正文一个字都不动。
func normalizeSubtitle(data []byte) ([]byte, bool) {
	text, decoded := decodeToUTF8(data)

	// 先清掉夹在时间轴里的不可见字符，否则数字根本连不起来
	cleaned := zeroWidthRe.ReplaceAllString(text, "")

	lines := strings.Split(cleaned, "\n")
	for i, line := range lines {
		if fixed, ok := parseTimestampLine(line); ok {
			lines[i] = fixed
		}
	}
	out := strings.Join(lines, "\n")

	if out == text && !decoded {
		return data, false
	}
	return []byte(out), true
}

// ---------------------------------------------------------------- 核心流程

// findSubtitle 搜索番号并挑出最佳中文字幕
//
// 规则：搜索结果按 SIZE 从大到小排序 → 逐个打开详情页 →
// 优先取 Chinese (Simplified)，没有则取 Chinese (Traditional) → 都没有则换下一条。
func findSubtitle(code string) (dlURL, lang, srcName, sizeStr string, err error) {
	searchURL := baseURL + "index.php?search=" + url.QueryEscape(code)

	body, err := fetch(searchURL, baseURL)
	if err != nil {
		return "", "", "", "", fmt.Errorf("搜索请求失败: %w", err)
	}

	results := parseSearch(string(body))
	if len(results) == 0 {
		return "", "", "", "", nil
	}

	// 优先按照 SIZE 从大到小
	sort.SliceStable(results, func(i, j int) bool { return results[i].size > results[j].size })

	// 只保留标题里确实含有该番号的结果，避免张冠李戴
	normCode := normalizeCode(code)
	var matched []searchResult
	for _, r := range results {
		if strings.Contains(normalizeCode(r.name), normCode) {
			matched = append(matched, r)
		}
	}
	if len(matched) == 0 {
		return "", "", "", "", nil
	}

	for _, r := range matched {
		detailURL := resolveURL(r.href)
		page, err := fetch(detailURL, searchURL)
		if err != nil {
			continue
		}
		h := string(page)

		if link := findLangDownload(h, langSimplified); link != "" {
			return resolveURL(link), langSimplified, r.name, r.sizeStr, nil
		}
		if link := findLangDownload(h, langTraditional); link != "" {
			return resolveURL(link), langTraditional, r.name, r.sizeStr, nil
		}
	}
	return "", "", "", "", nil
}

// hasSubtitle 判断目录内是否已存在字幕文件
func hasSubtitle(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if subtitleExts[strings.ToLower(filepath.Ext(e.Name()))] {
			return e.Name(), true
		}
	}
	return "", false
}

// download 下载并保存字幕。normalize 为真时把非标准时间轴修成标准 SRT。
func download(dlURL, referer, destPath string, normalize bool) (int64, bool, error) {
	data, err := fetch(dlURL, referer)
	if err != nil {
		return 0, false, err
	}
	if !looksLikeSubtitle(data) {
		return 0, false, errors.New("返回内容不是字幕（可能被站点拦截）")
	}

	fixed := false
	if normalize {
		data, fixed = normalizeSubtitle(data)
	}

	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		return 0, false, err
	}
	return int64(len(data)), fixed, nil
}

// processFolder 处理单个番号文件夹
func processFolder(dir string, verbose, normalize bool) outcome {
	name := filepath.Base(dir)

	if existing, ok := hasSubtitle(dir); ok {
		return outcome{name, "skip-has-sub", "已存在字幕: " + existing}
	}

	dlURL, lang, srcName, sizeStr, err := findSubtitle(name)
	if err != nil {
		return outcome{name, "err", err.Error()}
	}
	if dlURL == "" {
		return outcome{name, "no-result", "未找到中文（简/繁）字幕"}
	}

	dest := filepath.Join(dir, name+".srt")
	n, fixed, err := download(dlURL, baseURL, dest, normalize)
	if err != nil {
		return outcome{name, "err", err.Error()}
	}

	detail := fmt.Sprintf("%s | 来源《%s》 %s | %.1f KB", lang, srcName, sizeStr, float64(n)/1024)
	if fixed {
		detail += " | 已规范化格式"
	}
	if verbose {
		fmt.Printf("      %s%s%s\n", cDim, dlURL, cReset)
	}
	return outcome{name, "ok", detail}
}

// ---------------------------------------------------------------- 主程序

func main() {
	enableANSI()

	var (
		flagDir     = flag.String("d", "", "直接指定目录（不弹出选择框）")
		flagDelay   = flag.Duration("delay", 700*time.Millisecond, "每次请求之间的间隔")
		flagQuiet   = flag.Bool("q", false, "安静模式，只输出结果")
		flagVerbose = flag.Bool("v", false, "输出详细下载地址")
		flagAll     = flag.Bool("all", false, "不过滤番号，处理所有子文件夹")
		flagRaw     = flag.Bool("raw", false, "原样保存字幕，不修正非标准时间轴格式")
	)
	flag.Usage = usage
	flag.Parse()

	interactive := len(os.Args) == 1

	if *flagDelay >= 0 {
		requestGap = *flagDelay
	}

	if !*flagQuiet {
		banner()
	}

	// 1. 选择目录
	dir := *flagDir
	if dir == "" && flag.NArg() > 0 {
		dir = flag.Arg(0)
	}
	if dir == "" {
		fmt.Printf("%s正在打开文件夹选择框…%s\n", cCyan, cReset)
		picked, err := pickFolder("请选择包含番号文件夹的目录")
		if err != nil {
			fmt.Printf("%s✗ %v%s\n", cRed, err, cReset)
			pauseIf(interactive)
			os.Exit(1)
		}
		dir = picked
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		absDir = dir
	}
	info, err := os.Stat(absDir)
	if err != nil || !info.IsDir() {
		fmt.Printf("%s✗ 目录不存在或不可访问: %s%s\n", cRed, absDir, cReset)
		pauseIf(interactive)
		os.Exit(1)
	}

	fmt.Printf("%s目标目录：%s%s\n\n", cBold, absDir, cReset)

	// 2. 列出子文件夹
	entries, err := os.ReadDir(absDir)
	if err != nil {
		fmt.Printf("%s✗ 读取目录失败: %v%s\n", cRed, err, cReset)
		pauseIf(interactive)
		os.Exit(1)
	}

	var folders, ignored []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !*flagAll && !looksLikeCode(e.Name()) {
			ignored = append(ignored, e.Name())
			continue
		}
		folders = append(folders, filepath.Join(absDir, e.Name()))
	}
	sort.Strings(folders)

	if len(ignored) > 0 {
		fmt.Printf("%s已忽略 %d 个非番号文件夹：%s\n", cDim, len(ignored), strings.Join(ignored, ", "))
	}

	if len(folders) == 0 {
		fmt.Printf("%s未找到符合番号格式的子文件夹。%s\n", cYell, cReset)
		fmt.Printf("%s（如需处理全部文件夹，请加 -all 参数）%s\n", cDim, cReset)
		pauseIf(interactive)
		return
	}

	fmt.Printf("%s共发现 %d 个文件夹：%s\n", cCyan, len(folders), cReset)
	for i, f := range folders {
		fmt.Printf("  %s%2d.%s %s\n", cDim, i+1, cReset, filepath.Base(f))
	}
	fmt.Println()

	// 3. 逐个处理
	var results []outcome
	ok, skipped, failed := 0, 0, 0

	for i, folder := range folders {
		name := filepath.Base(folder)

		oc := processFolder(folder, *flagVerbose, !*flagRaw)
		results = append(results, oc)

		switch oc.status {
		case "ok":
			ok++
			fmt.Printf("\r%s[%d/%d]%s %s%-40s%s %s✔ %s%s\n",
				cBold, i+1, len(folders), cReset, cCyan, name, cReset, cGreen, oc.detail, cReset)
		case "skip-has-sub":
			skipped++
			fmt.Printf("\r%s[%d/%d]%s %s%-40s%s %s⏭ 跳过（%s）%s\n",
				cBold, i+1, len(folders), cReset, cCyan, name, cReset, cDim, oc.detail, cReset)
		case "no-result":
			skipped++
			fmt.Printf("\r%s[%d/%d]%s %s%-40s%s %s⏭ 跳过（%s）%s\n",
				cBold, i+1, len(folders), cReset, cCyan, name, cReset, cYell, oc.detail, cReset)
		default:
			failed++
			fmt.Printf("\r%s[%d/%d]%s %s%-40s%s %s✗ %s%s\n",
				cBold, i+1, len(folders), cReset, cCyan, name, cReset, cRed, oc.detail, cReset)
		}
	}

	// 4. 汇总
	fmt.Printf("\n%s──────────── 汇总 ────────────%s\n", cDim, cReset)
	fmt.Printf("  %s成功下载: %d%s\n", cGreen, ok, cReset)
	fmt.Printf("  %s跳过:     %d%s\n", cDim, skipped, cReset)
	if failed > 0 {
		fmt.Printf("  %s失败:     %d%s\n", cRed, failed, cReset)
	}
	fmt.Printf("  %s合计:     %d%s\n", cBold, len(folders), cReset)

	if failed > 0 && !*flagQuiet {
		fmt.Printf("\n%s失败明细：%s\n", cYell, cReset)
		for _, r := range results {
			if r.status == "err" {
				fmt.Printf("  %s✗ %s — %s%s\n", cRed, r.folder, r.detail, cReset)
			}
		}
	}

	pauseIf(interactive)
}

func banner() {
	fmt.Printf("%s", cCyan)
	fmt.Println("╔══════════════════════════════════════════════════════╗")
	fmt.Println("║        SubtitleCat 番号字幕匹配下载器                ║")
	fmt.Println("║   subtitlecat.com · 简体优先 / 繁体候补 / 按大小取最大  ║")
	fmt.Println("╚══════════════════════════════════════════════════════╝")
	fmt.Printf("%s\n", cReset)
}

func usage() {
	fmt.Fprintf(os.Stderr, `SubtitleCat 番号字幕匹配下载器

用法:
  SubtitleCatMatcher.exe [选项] [目录]

  不带参数双击运行时会弹出文件夹选择框。
  也可以直接传入目录路径跳过选择框。

选项:
  -d string     直接指定目录
  -delay dur    每次请求之间的间隔 (默认 700ms)
  -all          不过滤番号，处理所有子文件夹
  -raw          原样保存字幕，不修正非标准时间轴格式
  -v            输出详细下载地址
  -q            安静模式，只输出结果
  -h            显示本帮助

说明:
  默认只处理「看起来像番号」的文件夹（如 SNOS-115、SSIS-001、259LUXU-1234）。
  字幕保存为「文件夹同名.srt」；文件夹内已有字幕则自动跳过。
  部分站点字幕（如 aisubs.app 来源）时间轴写成 00：00：00.000-> 这种非标准形式，
  播放器（ffmpeg / Jellyfin / Emby）无法识别，会自动修正为标准 SRT 后再保存。
`)
}

// pauseIf 仅在交互式（双击）运行时暂停，方便查看结果
func pauseIf(interactive bool) {
	if !interactive {
		return
	}
	fmt.Printf("\n%s按 Enter 键退出…%s", cDim, cReset)
	bufio.NewReader(os.Stdin).ReadString('\n')
}

// ================================================================
// Windows 原生 API：文件夹选择框 + 控制台 ANSI 支持
// ================================================================

var (
	shell32 = syscall.NewLazyDLL("shell32.dll")
	ole32   = syscall.NewLazyDLL("ole32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procSHBrowseForFolderW   = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListW = shell32.NewProc("SHGetPathFromIDListW")
	procCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")
	procCoInitializeEx       = ole32.NewProc("CoInitializeEx")
	procCoUninitialize       = ole32.NewProc("CoUninitialize")
	procGetConsoleMode       = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode       = kernel32.NewProc("SetConsoleMode")
)

// BROWSEINFOW 结构体（64 位布局与 Win32 一致）
type browseInfoW struct {
	hwndOwner      uintptr
	pidlRoot       uintptr
	pszDisplayName uintptr
	lpszTitle      uintptr
	ulFlags        uint32
	lpfn           uintptr
	lParam         uintptr
	iImage         int32
}

const (
	bifReturnOnlyFSDirs = 0x0001
	bifEditBox          = 0x0010
	bifNewDialogStyle   = 0x0040
	coinitApartmentThr  = 0x2
	enableVirtualTerm   = 0x0004
)

// pickFolder 弹出 Windows 原生文件夹选择框，返回所选路径
func pickFolder(title string) (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	procCoInitializeEx.Call(0, coinitApartmentThr)
	defer procCoUninitialize.Call()

	titlePtr, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return "", err
	}

	displayBuf := make([]uint16, 260)
	bi := browseInfoW{
		pszDisplayName: uintptr(unsafe.Pointer(&displayBuf[0])),
		lpszTitle:      uintptr(unsafe.Pointer(titlePtr)),
		ulFlags:        bifReturnOnlyFSDirs | bifNewDialogStyle | bifEditBox,
	}

	pidl, _, _ := procSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return "", errors.New("已取消选择")
	}
	defer procCoTaskMemFree.Call(pidl)

	pathBuf := make([]uint16, 260)
	ok, _, _ := procSHGetPathFromIDListW.Call(pidl, uintptr(unsafe.Pointer(&pathBuf[0])))
	if ok == 0 {
		return "", errors.New("无法解析所选文件夹路径")
	}
	return syscall.UTF16ToString(pathBuf), nil
}

// enableANSI 开启控制台 ANSI 转义序列支持（Windows 10+）
func enableANSI() {
	h, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil || h == 0 {
		return
	}
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		return
	}
	procSetConsoleMode.Call(uintptr(h), uintptr(mode|enableVirtualTerm))
}
