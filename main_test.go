//go:build windows

package main

import "testing"

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

	rs := parseSearch(page)
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
