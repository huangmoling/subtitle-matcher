//go:build windows

package main

import (
	"strings"
	"testing"
	"unicode/utf16"
)

// 模拟 subtitlecat 详情页中的语言区块
func block(lang, href string) string {
	if href == "" {
		// 只有翻译按钮、没有下载链接
		return `<div class="sub-single">
  <span><img src="/assets/flags/cn.png" alt="zh-CN" class="flag"></span>
  <span>` + lang + `</span>
  <span><button onclick="translate_from_server_folder('zh-CN','x.srt','/subs/1/')" class="yellow-link">Translate</button></span>
</div>
<!-- ./Sub single -->`
	}
	return `<div class="sub-single">
  <span><img src="/assets/flags/cn.png" alt="zh-CN" class="flag"></span>
  <span>` + lang + `</span>
  <span><a id="download" onclick="log_download(1);" href="` + href + `" class="green-link">Download</a>
  <span id="voting" style="display:none;"><a href="javascript:vote('zh-CN',1,+1)">👍</a></span></span>
</div>
<!-- ./Sub single -->`
}

func TestFindLangDownload(t *testing.T) {
	page := `<html><body>
` + block("English", "/subs/9/x-en.srt") + `
` + block("Chinese (Traditional)", "/subs/8/x-zh-TW.srt") + `
` + block("Chinese (Simplified)", "/subs/7/x-zh-CN.srt") + `
` + block("Korean", "") + `
</body></html>`

	if got := findLangDownload(page, langSimplified); got != "/subs/7/x-zh-CN.srt" {
		t.Errorf("简体优先失败, got %q", got)
	}
	if got := findLangDownload(page, langTraditional); got != "/subs/8/x-zh-TW.srt" {
		t.Errorf("繁体查找失败, got %q", got)
	}

	// 只有繁体时的候补场景
	onlyTW := `<html><body>` + block("English", "/subs/9/x-en.srt") +
		block("Chinese (Traditional)", "/subs/8/x-zh-TW.srt") + `</body></html>`
	if got := findLangDownload(onlyTW, langSimplified); got != "" {
		t.Errorf("不该找到简体, got %q", got)
	}
	if got := findLangDownload(onlyTW, langTraditional); got != "/subs/8/x-zh-TW.srt" {
		t.Errorf("繁体候补失败, got %q", got)
	}

	// 只有翻译按钮、无下载链接 → 视为不可下载
	noDl := `<html><body>` + block("Chinese (Simplified)", "") + `</body></html>`
	if got := findLangDownload(noDl, langSimplified); got != "" {
		t.Errorf("无下载链接时应返回空, got %q", got)
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"62 KB":  62 * 1024,
		"105 KB": 105 * 1024,
		"1.5 MB": int64(1.5 * 1024 * 1024),
		"2 GB":   2 * 1024 * 1024 * 1024,
		"900 B":  900,
		"bad":    0,
	}
	for in, want := range cases {
		if got := parseSize(in); got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseSearchSortsBySize(t *testing.T) {
	page := `<table><tbody>
<tr>
  <td><a href="subs/1449/SNOS-115.html">SNOS-115</a> (translated from English)</td>
  <td class="sub-table__stars">x</td>
  <td class="sub-table__metric"><span class="sub-table__metric-label">Size</span><span class="sub-table__metric-value">62 KB</span></td>
  <td class="sub-table__metric"><span class="sub-table__metric-value">19</span></td>
  <td class="sub-table__metric"><span class="sub-table__metric-value">19</span></td>
</tr>
<tr>
  <td><a href="subs/1585/SNOS-115%20jp.html">SNOS-115 jp</a> (translated from Japanese)</td>
  <td class="sub-table__stars">x</td>
  <td class="sub-table__metric"><span class="sub-table__metric-value">105 KB</span></td>
  <td class="sub-table__metric"><span class="sub-table__metric-value">7</span></td>
  <td class="sub-table__metric"><span class="sub-table__metric-value">7</span></td>
</tr>
</tbody></table>`

	rs := parseSCSearch(page)
	if len(rs) != 2 {
		t.Fatalf("应解析出 2 条结果, got %d", len(rs))
	}
	if rs[0].name != "SNOS-115" || rs[0].size != 62*1024 {
		t.Errorf("第 1 条解析错误: %+v", rs[0])
	}
	if rs[1].sizeStr != "105 KB" || rs[1].size != 105*1024 {
		t.Errorf("第 2 条解析错误: %+v", rs[1])
	}
	// 归一化比对：标题应含有番号
	if normalizeCode("SNOS-115 jp") != "SNOS115JP" {
		t.Errorf("normalizeCode 错误: %q", normalizeCode("SNOS-115 jp"))
	}
}

func TestLooksLikeCode(t *testing.T) {
	yes := []string{"SNOS-115", "SSIS-001", "259LUXU-1234", "HEYZO-1234", "MIDE-123-C", "snos 115", "ABP-123"}
	no := []string{".workbuddy-ai", "subtitle-matcher", "test", "movies", "新建文件夹", "cat.webp"}
	for _, s := range yes {
		if !looksLikeCode(s) {
			t.Errorf("应识别为番号: %q", s)
		}
	}
	for _, s := range no {
		if looksLikeCode(s) {
			t.Errorf("不应识别为番号: %q", s)
		}
	}
}

// aisubs.app 来源的字幕用 "->" 和全角冒号，曾经被误判成"被站点拦截"
const aisubsSample = "0\n00：00：00.000-> 00：00：02.000\n  你\n\n1\n00：00：30.000-> 00：00：32.000\n  我\n"

func TestLooksLikeSubtitle(t *testing.T) {
	standard := "1\n00:00:01,000 --> 00:00:02,000\n你好\n"
	webvtt := "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\n你好\n"
	blocked := "<!DOCTYPE html>\n<html><head><title>Just a moment...</title></head></html>"
	blocked2 := "<html>\n<body>Access denied</body>\n</html>"

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"标准 SRT", standard, true},
		{"WebVTT", webvtt, true},
		{"aisubs 非标准格式", aisubsSample, true},
		{"Cloudflare 拦截页", blocked, false},
		{"拒绝访问页", blocked2, false},
		{"空内容", "", false},
	}
	for _, c := range cases {
		if got := looksLikeSubtitle([]byte(c.in)); got != c.want {
			t.Errorf("%s: looksLikeSubtitle = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNormalizeSubtitle(t *testing.T) {
	want := "0\n00:00:00,000 --> 00:00:02,000\n  你\n\n1\n00:00:30,000 --> 00:00:32,000\n  我\n"
	out, changed := normalizeSubtitle([]byte(aisubsSample))
	if !changed {
		t.Fatal("aisubs 格式应报告发生了修改")
	}
	if string(out) != want {
		t.Errorf("规范化结果不符\n got: %q\nwant: %q", out, want)
	}

	// 已经是标准格式 → 一个字节都不该动
	std := "1\n00:00:01,000 --> 00:00:02,000\n你好\n"
	out2, changed2 := normalizeSubtitle([]byte(std))
	if changed2 {
		t.Error("标准 SRT 不应报告修改")
	}
	if string(out2) != std {
		t.Errorf("标准 SRT 内容被改动: %q", out2)
	}

	// 正文里的全角冒号和箭头不能被误改
	body := "1\n00:00:01,000 --> 00:00:02,000\n他说：A->B\n"
	out3, changed3 := normalizeSubtitle([]byte(body))
	if changed3 {
		t.Error("时间轴标准时不应因正文含特殊字符而改动")
	}
	if string(out3) != body {
		t.Errorf("正文被误改: %q", out3)
	}

	// 毫秒位数不足要补齐
	short := "0\n0：0：1.5-> 0：0：2.25\n嗨\n"
	out4, _ := normalizeSubtitle([]byte(short))
	if !strings.Contains(string(out4), "00:00:01,500 --> 00:00:02,250") {
		t.Errorf("毫秒补齐失败: %q", out4)
	}
}

// 站点部分字幕把零宽空格塞进时间轴数字中间，真实数据里出现过
func TestNormalizeSubtitleZeroWidth(t *testing.T) {
	in := "0\n00：01：46.0\u200b\u200b00-> 00：01：47.000\n  你\n"
	want := "0\n00:01:46,000 --> 00:01:47,000\n  你\n"
	out, changed := normalizeSubtitle([]byte(in))
	if !changed {
		t.Fatal("含零宽空格的时间轴应被规范化")
	}
	if string(out) != want {
		t.Errorf("零宽空格处理失败\n got: %q\nwant: %q", out, want)
	}
	if strings.ContainsRune(string(out), '\u200b') {
		t.Error("输出里仍残留零宽空格")
	}

	// 正文里的零宽空格也应清掉，且不应破坏正文其余部分
	body := "1\n00:00:01,000 --> 00:00:02,000\n你\u200b好\n"
	out2, changed2 := normalizeSubtitle([]byte(body))
	if !changed2 {
		t.Error("正文含零宽空格时应报告修改")
	}
	if string(out2) != "1\n00:00:01,000 --> 00:00:02,000\n你好\n" {
		t.Errorf("正文清理结果不符: %q", out2)
	}

	// ZWJ（U+200D）有语义，不能被误删
	zwj := "1\n00:00:01,000 --> 00:00:02,000\n👨\u200d👩\n"
	out3, _ := normalizeSubtitle([]byte(zwj))
	if !strings.Contains(string(out3), "\u200d") {
		t.Error("ZWJ 被误删，emoji 会被破坏")
	}
}

// 源文件里真实存在的几种损坏写法
func TestNormalizeSubtitleMalformed(t *testing.T) {
	cases := map[string]string{
		// 分钟被重复了一次
		"01：59：18.940-> 02：12：12：18.190": "01:59:18,940 --> 02:12:18,190",
		// 毫秒 4 位
		"01：06：27.200-> 01：06：29.2900": "01:06:27,200 --> 01:06:29,290",
		// 两者同时出现
		"02：12：48.280-> 02：12：12：50.280": "02:12:48,280 --> 02:12:50,280",
		// 正常的也要保持正确
		"00：00：00.000-> 00：00：02.000": "00:00:00,000 --> 00:00:02,000",
	}
	for in, want := range cases {
		got, ok := parseTimestampLine(in)
		if !ok {
			t.Errorf("应能解析: %q", in)
			continue
		}
		if got != want {
			t.Errorf("\n in: %q\n got: %q\nwant: %q", in, got, want)
		}
	}

	// 重复段不相等 → 不敢猜，保持原样
	if out, ok := parseTimestampLine("01:02:03:04.500-> 01:02:03,500"); ok {
		t.Errorf("重复段不相等时不应改写，却改成了 %q", out)
	}

	// 正文里的箭头不能被当成时间轴
	if out, ok := parseTimestampLine("他说：A->B"); ok {
		t.Errorf("正文被误判为时间轴: %q", out)
	}

	// 结尾带 \r 的 CRLF 行要保留 \r
	if got, ok := parseTimestampLine("00：00：01.000-> 00：00：02.000\r"); !ok || got != "00:00:01,000 --> 00:00:02,000\r" {
		t.Errorf("CRLF 处理失败: ok=%v got=%q", ok, got)
	}
}

func TestNormalizeSubtitleUTF16(t *testing.T) {
	src := "1\n00:00:01,000 --> 00:00:02,000\n你好\n"
	u := utf16.Encode([]rune(src))
	buf := []byte{0xFF, 0xFE} // UTF-16LE BOM
	for _, c := range u {
		buf = append(buf, byte(c), byte(c>>8))
	}

	out, changed := normalizeSubtitle(buf)
	if !changed {
		t.Fatal("UTF-16 应被转成 UTF-8")
	}
	if string(out) != src {
		t.Errorf("转码结果不符\n got: %q\nwant: %q", out, src)
	}
}
