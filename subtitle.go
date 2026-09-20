//go:build windows

// subtitle.go —— 字幕内容校验与时间轴规范化
//
// 这部分是踩坑踩出来的：不同站点的字幕写法差异极大，
// 有些写法播放器（ffmpeg / Jellyfin / Emby）根本认不出来。
package main

import (
	"encoding/binary"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf16"
)

// 字幕文件扩展名（用于判断文件夹里是否已有字幕）
var subtitleExts = map[string]bool{
	".srt": true, ".ass": true, ".ssa": true, ".sub": true,
	".vtt": true, ".smi": true, ".idx": true, ".ttml": true, ".sbv": true,
}

var (
	// 任意时间轴（宽松）：兼容全角冒号、-> 与 -->、. 与 , 作毫秒分隔
	// 用它判断「这是不是字幕」，比死抠 "-->" 靠谱
	tsAnyRe = regexp.MustCompile(`\d{1,2}[:：]\d{2}[:：]\d{2}[.,]\d{1,3}\s*-+>`)

	// 不可见字符：部分站点会把零宽空格塞进时间轴数字中间
	// （00：01：46.0\u200b\u200b00），导致任何解析器都读不出时间。
	// 故意不动 U+200D（ZWJ）——它在 emoji / 印度语系里有实际语义。
	zeroWidthRe = regexp.MustCompile("[\u00ad\u200b\u200c\u200e\u200f\u2060\ufeff]")

	assScriptRe = regexp.MustCompile(`(?im)^\s*\[Script Info\]`)
)

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
// 「被站点拦截」。现在改为：先排除 HTML 错误页，再用宽松的时间轴正则确认。
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
	// ASS/SSA 用 [Script Info] 开头，没有 SRT 那种时间轴行
	if assScriptRe.MatchString(text) {
		return true
	}
	return tsAnyRe.MatchString(text)
}

// isASS 判断内容是否为 ASS/SSA 格式
func isASS(data []byte) bool {
	text, _ := decodeToUTF8(data)
	head := text
	if len(head) > 1024 {
		head = head[:1024]
	}
	return assScriptRe.MatchString(head)
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
