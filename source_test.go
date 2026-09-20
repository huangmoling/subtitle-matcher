//go:build windows

package main

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------- 语言判定

func TestLangRank(t *testing.T) {
	cases := []struct {
		in       string
		wantLang string
		wantRank int
	}{
		// subtitlecat 用英文标注
		{"Chinese (Simplified)", langSimp, 2},
		{"Chinese (Traditional)", langTrad, 1},
		{"English", "", 0},
		{"Korean", "", 0},
		// aiyi1 用中文标注
		{"中文简体", langSimp, 2},
		{"中文繁体", langTrad, 1},
		{"中英双语", langSimp, 2},
		// javzimu 只能从文件名猜
		{"ABC-123-zh-CN.srt", langSimp, 2},
		{"ABC-123-cht.ass", langTrad, 1},
		{"ABC-123-繁.srt", langTrad, 1},
	}
	for _, c := range cases {
		gotLang, gotRank := langRank(c.in)
		if gotLang != c.wantLang || gotRank != c.wantRank {
			t.Errorf("langRank(%q) = (%q,%d), want (%q,%d)",
				c.in, gotLang, gotRank, c.wantLang, c.wantRank)
		}
	}

	// 回归：曾经把 "chi" 当成简体关键词，
	// 结果 "Chinese (Traditional)" 被判成简体
	if l, r := langRank("Chinese (Traditional)"); r != 1 || l != langTrad {
		t.Errorf("繁体被误判成 %q (rank=%d)", l, r)
	}
}

// ---------------------------------------------------------------- 选优

func TestPickBest(t *testing.T) {
	// 同语言取大的
	got, ok := pickBest([]candidate{
		{Source: "A", Lang: langSimp, LangRank: 2, Size: 10 * 1024},
		{Source: "B", Lang: langSimp, LangRank: 2, Size: 50 * 1024},
	})
	if !ok || got.Source != "B" {
		t.Errorf("同语言应取更大的，got %+v ok=%v", got, ok)
	}

	// 语言优先于大小：再小的简体也胜过更大的繁体
	got, ok = pickBest([]candidate{
		{Source: "A", Lang: langTrad, LangRank: 1, Size: 900 * 1024},
		{Source: "B", Lang: langSimp, LangRank: 2, Size: 5 * 1024},
	})
	if !ok || got.Source != "B" {
		t.Errorf("简体应优先于繁体，got %+v", got)
	}

	// 只有繁体时用繁体
	got, ok = pickBest([]candidate{
		{Source: "A", Lang: langTrad, LangRank: 1, Size: 7 * 1024},
	})
	if !ok || got.Lang != langTrad {
		t.Errorf("繁体候补失败，got %+v ok=%v", got, ok)
	}

	// 都不是中文 → 放弃
	if _, ok := pickBest([]candidate{
		{Source: "A", Lang: "", LangRank: 0, Size: 99 * 1024},
	}); ok {
		t.Error("没有中文时不应选出结果")
	}
	if _, ok := pickBest(nil); ok {
		t.Error("空候选不应选出结果")
	}

	// 大小未知（0）时不应当成"最大"而压过已知大小的同语言候选
	got, _ = pickBest([]candidate{
		{Source: "A", Lang: langSimp, LangRank: 2, Size: 0},
		{Source: "B", Lang: langSimp, LangRank: 2, Size: 20 * 1024},
	})
	if got.Source != "B" {
		t.Errorf("已知大小应胜过未知，got %+v", got)
	}
}

// rankCandidates 是"下载失败就退到下一个候选"的基础，顺序必须稳定
func TestRankCandidates(t *testing.T) {
	in := []candidate{
		{Source: "C", Lang: "", LangRank: 0, Size: 999 * 1024},   // 非中文，应被剔除
		{Source: "A", Lang: langTrad, LangRank: 1, Size: 80 * 1024},
		{Source: "B", Lang: langSimp, LangRank: 2, Size: 10 * 1024},
		{Source: "D", Lang: langSimp, LangRank: 2, Size: 60 * 1024},
		{Source: "E", Lang: langTrad, LangRank: 1, Size: 90 * 1024},
	}

	got := rankCandidates(in)
	if len(got) != 4 {
		t.Fatalf("应保留 4 个中文候选，got %d", len(got))
	}

	want := []string{"D", "B", "E", "A"} // 简体按大小降序，然后繁体按大小降序
	for i, w := range want {
		if got[i].Source != w {
			t.Errorf("第 %d 位应为 %s，got %s（完整顺序 %v）", i, w, got[i].Source, srcs(got))
		}
	}

	// 没有中文候选时返回空
	if r := rankCandidates([]candidate{{Source: "X", LangRank: 0}}); len(r) != 0 {
		t.Errorf("无中文候选时应返回空，got %d", len(r))
	}
	if r := rankCandidates(nil); len(r) != 0 {
		t.Errorf("nil 输入应返回空，got %d", len(r))
	}
}

func srcs(cs []candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Source
	}
	return out
}

// ---------------------------------------------------------------- aiyi1

const aiyiSearchFixture = `<html><body>
<div class="post-warp">
	<div class="post-box">	    				
			<div class="post-header">
				<h2 class="post-title tra"><a href="https://www.aiyi1.com/10376.html" class="tra">ABF-353 (translated from Chinese)</a></h2>
			</div>
	    		<div class="post-content">		
				<div class="post-thumb">
					<div class="clear"></div>	
				</div>
				字幕格式：SRT

字幕语种：中文简体

字幕来源：个人

主要演员：八挂うみ

匹配视频：ABF-353

下载字幕 | 15KB

文件名：ABF-353-zh-CN.srt

&nbsp;

观影：作品观看平台				<div class="post-tags">
					<span class="views"><a>&nbsp;热度&nbsp;114</a></span>
				</div>
		</div>
	<div class="post-box">	    				
			<div class="post-header">
				<h2 class="post-title tra"><a href="https://www.aiyi1.com/10373.html" class="tra">ABP-573 (translated from Chinese)</a></h2>
			</div>
	    		<div class="post-content">		
				字幕格式：SRT

字幕语种：中文繁体

匹配视频：ABP-573

下载字幕 | 33KB

文件名：ABP-573-zh-TW.srt

				<div class="post-tags"></div>
		</div>
</div>
<div id="sidebar">
	<ul class="wp-block-latest-posts__list">
		<li><a class="wp-block-latest-posts__post-title" href="https://www.aiyi1.com/9999.html">ZZZ-999 (translated from Chinese)</a></li>
	</ul>
</div>
</body></html>`

const aiyiPostFixture = `<html><body>
			<div class="post">
				<h1 class="post-title"><span>ABF-353 (translated from Chinese)</span></h1>
				<div class="main_post">
				<div class="p_info"><span class="info_date info_ico">09-16</span></div>
				<div class="main-content">
					<p>字幕格式：SRT</p>
<p>字幕语种：中文简体</p>
<p>字幕来源：个人</p>
<p>主要演员：八挂うみ</p>
<p>匹配视频：ABF-353</p>
<p><a href="https://a.hinimg.com/aiyi-files/2026/09/ABF-353-zh-CN.srt">下载字幕 | 15KB</a></p>
<p>文件名：ABF-353-zh-CN.srt</p>
<p>&nbsp;</p>
<p>观影：<span style="text-decoration-line: underline;"><a href="https://n.urlge.com/nonew/zuopin/movies.html" target="_blank" rel="noopener noreferrer nofollow">作品观看平台</a></span></p>
				</div>
				<div class="p_tags">
                	<div class="tagcloud">标签：<a href="https://www.aiyi1.com/tag/abf-353" rel="tag">ABF-353</a></div>
                </div>
				</div>
			</div>
</body></html>`

func TestParseAiyiSearch(t *testing.T) {
	items := parseAiyiSearch(aiyiSearchFixture)
	if len(items) != 2 {
		t.Fatalf("应解析出 2 条结果，got %d", len(items))
	}

	first := items[0]
	if first.title != "ABF-353 (translated from Chinese)" {
		t.Errorf("标题解析错误: %q", first.title)
	}
	if first.href != "https://www.aiyi1.com/10376.html" {
		t.Errorf("链接解析错误: %q", first.href)
	}
	if first.match != "ABF-353" {
		t.Errorf("匹配视频解析错误: %q", first.match)
	}
	if first.langRaw != "中文简体" {
		t.Errorf("语种解析错误: %q", first.langRaw)
	}
	if first.size != 15*1024 || first.sizeStr != "15KB" {
		t.Errorf("大小解析错误: size=%d sizeStr=%q", first.size, first.sizeStr)
	}
	if first.format != "SRT" {
		t.Errorf("格式解析错误: %q", first.format)
	}

	// 侧边栏的「近期字幕」不该被当成搜索结果
	for _, it := range items {
		if strings.Contains(it.title, "ZZZ-999") {
			t.Error("侧边栏内容被误当成搜索结果")
		}
	}

	// 第二条是繁体
	if l, r := langRank(items[1].langRaw); l != langTrad || r != 1 {
		t.Errorf("第二条应为繁体，got %q rank=%d", l, r)
	}
}

func TestParseAiyiDownload(t *testing.T) {
	got := parseAiyiDownload(aiyiPostFixture)
	want := "https://a.hinimg.com/aiyi-files/2026/09/ABF-353-zh-CN.srt"
	if got != want {
		t.Errorf("下载链接解析错误\n got: %q\nwant: %q", got, want)
	}

	// 正文里的「作品观看平台」链接不能被当成字幕下载
	if strings.Contains(got, "urlge.com") {
		t.Error("抓到了错误的链接（观影链接）")
	}

	// 没有正文区块时应安全返回空
	if got := parseAiyiDownload("<html><body>nothing here</body></html>"); got != "" {
		t.Errorf("无正文时应返回空，got %q", got)
	}
}

// ---------------------------------------------------------------- javzimu

func TestDecodeJZ(t *testing.T) {
	// 正常空结果
	r, ok := decodeJZ([]byte(`{"code":0,"data":[],"result":"ok"}`))
	if !ok || r.Result != "ok" || len(r.Data) != 0 {
		t.Errorf("正常响应解析失败: ok=%v %+v", ok, r)
	}

	// 需要人机验证（这是当前真实会遇到的响应）
	r, ok = decodeJZ([]byte(`{"error":"Verification required.","turnstile_required":true,"remaining":0}`))
	if !ok {
		t.Fatal("验证响应应能解析")
	}
	if !r.TurnstileRequired {
		t.Error("应识别出 turnstile_required")
	}
	if err := jzError(r); err == nil || !strings.Contains(err.Error(), "人机验证") {
		t.Errorf("应转换成人机验证错误，got %v", err)
	}

	// 带结果
	r, ok = decodeJZ([]byte(`{"code":0,"result":"ok","data":[{"cid":"abc","ext":"srt","name":"ABC-123-zh-CN.srt","duration":5400000,"_ts":123,"_sig":"xyz"}]}`))
	if !ok || len(r.Data) != 1 {
		t.Fatalf("带结果响应解析失败: ok=%v %+v", ok, r)
	}
	if r.Data[0].Cid != "abc" || r.Data[0].Name != "ABC-123-zh-CN.srt" {
		t.Errorf("条目字段解析错误: %+v", r.Data[0])
	}

	// HTML 错误页不能被当成 JSON
	if _, ok := decodeJZ([]byte("<html><body>oops</body></html>")); ok {
		t.Error("HTML 不应被解析成 JSON 响应")
	}
	if _, ok := decodeJZ([]byte("")); ok {
		t.Error("空内容不应被解析成 JSON 响应")
	}
}

// ---------------------------------------------------------------- 小工具

func TestExtFromURL(t *testing.T) {
	cases := map[string]string{
		"https://a.hinimg.com/x/ABF-353-zh-CN.srt":          ".srt",
		"https://javzimu.com/api/download?cid=a&ext=ass":    ".srt",
		"/subs/1/x.ass":                                     ".ass",
		"https://example.com/no-extension":                  ".srt",
	}
	for in, want := range cases {
		if got := extFromURL(in); got != want {
			t.Errorf("extFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		0:       "未知",
		512:     "512 B",
		1536:    "1.5 KB",
		15 * 1024: "15.0 KB",
		2 * 1024 * 1024: "2.00 MB",
	}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveAgainst(t *testing.T) {
	cases := []struct{ base, href, want string }{
		{"https://www.subtitlecat.com/", "subs/1/x.html", "https://www.subtitlecat.com/subs/1/x.html"},
		{"https://www.subtitlecat.com/", "/subs/1/x.srt", "https://www.subtitlecat.com/subs/1/x.srt"},
		{"https://www.subtitlecat.com/", "https://other.com/a.srt", "https://other.com/a.srt"},
	}
	for _, c := range cases {
		if got := resolveAgainst(c.base, c.href); got != c.want {
			t.Errorf("resolveAgainst(%q,%q) = %q, want %q", c.base, c.href, got, c.want)
		}
	}
}

func TestShortErr(t *testing.T) {
	if got := shortErr(errVerification); got != "需要人机验证" {
		t.Errorf("人机验证错误应被简化，got %q", got)
	}
	long := strings.Repeat("字", 200)
	got := shortErr(errString(long))
	if len([]rune(got)) > 61 {
		t.Errorf("过长的错误应被截断，got %d 字", len([]rune(got)))
	}
}

type errString string

func (e errString) Error() string { return string(e) }
