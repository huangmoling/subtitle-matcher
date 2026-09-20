//go:build windows

// source.go —— 字幕来源抽象
//
// 每个站点实现同一个 source 接口：给一个番号，返回若干「候选字幕」。
// 候选之间带着语言和文件大小，最终由 pickBest 跨站选优。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- 语言

const (
	langSimp = "简体"
	langTrad = "繁体"
)

// langRank 把各站五花八门的语言标注归一成「简体=2 / 繁体=1 / 其它=0」。
// subtitlecat 用英文，aiyi1 用「中文简体 / 中文繁体」，javzimu 只能靠文件名猜。
func langRank(name string) (string, int) {
	low := strings.ToLower(name)

	simplified := []string{"简体", "双语", "简中", "简"}
	// 注意：不要放 "chi" 这种前缀，它会被 "Chinese (Traditional)" 命中
	englishSimp := []string{"simplified", "zh-cn", "zh_cn", "chs"}
	for _, k := range simplified {
		if strings.Contains(name, k) {
			return langSimp, 2
		}
	}
	for _, k := range englishSimp {
		if strings.Contains(low, k) {
			return langSimp, 2
		}
	}

	traditional := []string{"繁体", "繁中", "繁"}
	englishTrad := []string{"traditional", "zh-tw", "cht", "zh_tw"}
	for _, k := range traditional {
		if strings.Contains(name, k) {
			return langTrad, 1
		}
	}
	for _, k := range englishTrad {
		if strings.Contains(low, k) {
			return langTrad, 1
		}
	}
	return "", 0
}

// ---------------------------------------------------------------- 候选

type candidate struct {
	Source   string // 站点显示名
	Lang     string // 简体 / 繁体
	LangRank int    // 2 = 简体优先，1 = 繁体候补
	Size     int64  // 字节；0 表示未知
	SizeText string
	URL      string // 字幕直链
	Referer  string
	Title    string
	Ext      string // .srt / .ass
}

type source interface {
	Name() string
	Search(ctx context.Context, code string) ([]candidate, error)
}

// errVerification 站点要求人机验证，本次跳过（不尝试绕过）
var errVerification = errors.New("站点要求人机验证，已跳过")

// ---------------------------------------------------------------- 番号

var (
	// 番号格式：可选纯数字前缀 + 2~10 位字母 + 可选分隔符 + 2~6 位数字
	// 例：SNOS-115、SSIS-001、259LUXU-1234、HEYZO-1234、MIDE-123-C
	codeRe = regexp.MustCompile(`(?i)^\d{0,5}[A-Z]{2,10}[-_ ]?\d{2,6}`)

	// 归一化比对用：去掉所有非字母数字
	nonAlnumRe = regexp.MustCompile(`[^A-Za-z0-9]`)
)

func looksLikeCode(name string) bool { return codeRe.MatchString(name) }

func normalizeCode(s string) string {
	return strings.ToUpper(nonAlnumRe.ReplaceAllString(s, ""))
}

// ---------------------------------------------------------------- subtitlecat

const subtitlecatBase = "https://www.subtitlecat.com/"

const (
	langSimplified  = "Chinese (Simplified)"
	langTraditional = "Chinese (Traditional)"
)

var (
	scRowRe    = regexp.MustCompile(`(?is)<tr>(.*?)</tr>`)
	scLinkRe   = regexp.MustCompile(`(?is)<a\s+href="([^"]+)"[^>]*>(.*?)</a>`)
	scMetricRe = regexp.MustCompile(`(?is)sub-table__metric-value">\s*([^<]*?)\s*<`)
	scSrtRe    = regexp.MustCompile(`(?is)<a\s[^>]*href="([^"]+\.srt)"[^>]*>\s*Download\s*</a>`)
	htmlTagRe  = regexp.MustCompile(`(?s)<[^>]+>`)
)

type scResult struct {
	name    string
	href    string
	size    int64
	sizeStr string
}

type subtitlecatSource struct{ h *httpGetter }

func (s *subtitlecatSource) Name() string { return "SubtitleCat" }

func (s *subtitlecatSource) Search(ctx context.Context, code string) ([]candidate, error) {
	searchURL := subtitlecatBase + "index.php?search=" + url.QueryEscape(code)
	body, err := s.h.Get(ctx, searchURL, subtitlecatBase, acceptHTML)
	if err != nil {
		return nil, err
	}

	results := parseSCSearch(string(body))
	if len(results) == 0 {
		return nil, nil
	}

	// 只保留标题里确实含该番号的结果，避免张冠李戴
	norm := normalizeCode(code)
	var matched []scResult
	for _, r := range results {
		if strings.Contains(normalizeCode(r.name), norm) {
			matched = append(matched, r)
		}
	}
	if len(matched) == 0 {
		return nil, nil
	}

	// 按 SIZE 从大到小排。因为"同语言取最大"，第一个带简体的结果
	// 就是全局最优的简体——拿到它就可以收工了，不必再翻后面的详情页。
	// （subtitlecat 的详情页实测 1.4~8s，偶尔还会卡住，少翻一次省很多时间）
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].size > matched[j].size })
	const maxDetailFetches = 4
	if len(matched) > maxDetailFetches {
		matched = matched[:maxDetailFetches]
	}

	var out []candidate
	var haveSimp, haveTrad bool
	for _, r := range matched {
		// 已经有简体了，说明最优解到手，停止翻页
		if haveSimp {
			break
		}
		detailURL := resolveAgainst(subtitlecatBase, r.href)
		page, err := s.h.Get(ctx, detailURL, searchURL, acceptHTML)
		if err != nil {
			continue
		}
		h := string(page)

		if link := findLangDownload(h, langSimplified); link != "" {
			out = append(out, candidate{
				Source: s.Name(), Lang: langSimp, LangRank: 2,
				Size: r.size, SizeText: r.sizeStr,
				URL: resolveAgainst(subtitlecatBase, link), Referer: detailURL,
				Title: r.name, Ext: ".srt",
			})
			haveSimp = true
			break
		}
		// 找不到简体时，先记下最大的繁体作为兜底，继续往后找简体
		if !haveTrad {
			if link := findLangDownload(h, langTraditional); link != "" {
				out = append(out, candidate{
					Source: s.Name(), Lang: langTrad, LangRank: 1,
					Size: r.size, SizeText: r.sizeStr,
					URL: resolveAgainst(subtitlecatBase, link), Referer: detailURL,
					Title: r.name, Ext: ".srt",
				})
				haveTrad = true
			}
		}
	}
	return out, nil
}

// parseSCSearch 从搜索结果页提取所有条目
func parseSCSearch(page string) []scResult {
	var out []scResult
	for _, m := range scRowRe.FindAllStringSubmatch(page, -1) {
		row := m[1]
		lm := scLinkRe.FindStringSubmatch(row)
		if lm == nil {
			continue
		}
		href := strings.TrimSpace(lm[1])
		if !strings.Contains(href, "subs/") || !strings.HasSuffix(href, ".html") {
			continue
		}
		name := strings.TrimSpace(htmlTagRe.ReplaceAllString(lm[2], ""))

		metrics := scMetricRe.FindAllStringSubmatch(row, -1)
		if len(metrics) == 0 {
			continue
		}
		sizeStr := strings.TrimSpace(metrics[0][1])

		out = append(out, scResult{
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
		if m := scSrtRe.FindStringSubmatch(b); m != nil {
			return m[1]
		}
	}
	return ""
}

// ---------------------------------------------------------------- aiyi1（爱译网）

const aiyi1Base = "https://www.aiyi1.com/"

var (
	aiyiTitleRe = regexp.MustCompile(`(?is)<h2 class="post-title[^"]*">\s*<a\s+href="([^"]+)"[^>]*>(.*?)</a>`)
	aiyiLangRe  = regexp.MustCompile(`字幕语种：([^<\r\n]*)`)
	aiyiFmtRe   = regexp.MustCompile(`字幕格式：([^<\r\n]*)`)
	aiyiSizeRe  = regexp.MustCompile(`下载字幕\s*\|\s*([^<\r\n]*)`)
	aiyiFileRe  = regexp.MustCompile(`文件名：([^<\r\n]*)`)
	aiyiMatchRe = regexp.MustCompile(`匹配视频：([^<\r\n]*)`)
	aiyiDLRe    = regexp.MustCompile(`(?is)<a\s+href="([^"]+)"[^>]*>\s*下载字幕[^<]*</a>`)
)

type aiyiItem struct {
	title   string
	href    string
	match   string
	format  string
	langRaw string
	size    int64
	sizeStr string
	file    string
}

type aiyi1Source struct{ h *httpGetter }

func (s *aiyi1Source) Name() string { return "爱译网" }

func (s *aiyi1Source) Search(ctx context.Context, code string) ([]candidate, error) {
	searchURL := aiyi1Base + "?s=" + url.QueryEscape(code)
	body, err := s.h.Get(ctx, searchURL, aiyi1Base, acceptHTML)
	if err != nil {
		return nil, err
	}

	items := parseAiyiSearch(string(body))
	if len(items) == 0 {
		return nil, nil
	}

	norm := normalizeCode(code)
	var matched []aiyiItem
	for _, it := range items {
		if strings.Contains(normalizeCode(it.match), norm) ||
			strings.Contains(normalizeCode(it.title), norm) {
			matched = append(matched, it)
		}
	}
	if len(matched) == 0 {
		return nil, nil
	}

	sort.SliceStable(matched, func(i, j int) bool { return matched[i].size > matched[j].size })
	const maxPostFetches = 4
	if len(matched) > maxPostFetches {
		matched = matched[:maxPostFetches]
	}

	var out []candidate
	seen := map[string]bool{}
	for _, it := range matched {
		lang, rank := langRank(it.langRaw)
		if rank == 0 {
			continue
		}
		// 同一个语言只取最大的那条
		if seen[lang] {
			continue
		}
		page, err := s.h.Get(ctx, it.href, searchURL, acceptHTML)
		if err != nil {
			continue
		}
		dl := parseAiyiDownload(string(page))
		if dl == "" {
			continue
		}
		seen[lang] = true
		out = append(out, candidate{
			Source: s.Name(), Lang: lang, LangRank: rank,
			Size: it.size, SizeText: it.sizeStr,
			URL: dl, Referer: it.href,
			Title: it.title, Ext: extFromURL(dl),
		})
	}
	return out, nil
}

// parseAiyiSearch 解析搜索结果页里的 <div class="post-box"> 区块。
// 这些字段在列表页是纯文本（没有链接），所以用文本正则取。
func parseAiyiSearch(page string) []aiyiItem {
	parts := strings.Split(page, `<div class="post-box">`)
	var out []aiyiItem
	for _, p := range parts[1:] {
		if i := strings.Index(p, `<div class="post-tags"`); i >= 0 {
			p = p[:i]
		}
		tm := aiyiTitleRe.FindStringSubmatch(p)
		if tm == nil {
			continue
		}
		it := aiyiItem{
			href:  strings.TrimSpace(tm[1]),
			title: strings.TrimSpace(htmlTagRe.ReplaceAllString(tm[2], "")),
		}
		it.match = firstGroup(aiyiMatchRe, p)
		it.format = firstGroup(aiyiFmtRe, p)
		it.langRaw = firstGroup(aiyiLangRe, p)
		it.sizeStr = firstGroup(aiyiSizeRe, p)
		it.size = parseSize(it.sizeStr)
		it.file = firstGroup(aiyiFileRe, p)
		out = append(out, it)
	}
	return out
}

// parseAiyiDownload 从文章页正文里取出字幕直链
func parseAiyiDownload(page string) string {
	i := strings.Index(page, `class="main-content"`)
	if i < 0 {
		return ""
	}
	seg := page[i:]
	if j := strings.Index(seg, `class="p_tags"`); j > 0 {
		seg = seg[:j]
	}
	if m := aiyiDLRe.FindStringSubmatch(seg); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// ---------------------------------------------------------------- javzimu（JAV字幕）

const javzimuBase = "https://javzimu.com/"

type jzItem struct {
	Cid      string `json:"cid"`
	Ext      string `json:"ext"`
	Name     string `json:"name"`
	Duration int64  `json:"duration"`
	TS       int64  `json:"_ts"`
	Sig      string `json:"_sig"`
}

type jzResp struct {
	Code   int    `json:"code"`
	Result string `json:"result"`
	Error  string `json:"error"`

	TurnstileRequired bool `json:"turnstile_required"`
	CaptchaRequired   bool `json:"captcha_required"`
	CaptchaBlocked    bool `json:"captcha_blocked"`
	Remaining         int  `json:"remaining"`

	Data []jzItem `json:"data"`
}

type javzimuSource struct{ h *httpGetter }

func (s *javzimuSource) Name() string { return "JAV字幕" }

func (s *javzimuSource) Search(ctx context.Context, code string) ([]candidate, error) {
	apiURL := javzimuBase + "api/search?name=" + url.QueryEscape(code)
	body, err := s.h.Get(ctx, apiURL, javzimuBase, acceptJSON)
	if err != nil {
		// 403 时站点会返回 JSON 说明原因，优先按它判断
		var se *statusError
		if errors.As(err, &se) {
			if r, ok := decodeJZ(se.Body()); ok {
				return nil, jzError(r)
			}
		}
		return nil, err
	}

	r, ok := decodeJZ(body)
	if !ok {
		return nil, fmt.Errorf("返回内容无法解析")
	}
	if r.TurnstileRequired || r.CaptchaRequired || r.CaptchaBlocked {
		return nil, jzError(r)
	}

	norm := normalizeCode(code)
	var out []candidate
	for _, it := range r.Data {
		if !strings.Contains(normalizeCode(it.Name), norm) {
			continue
		}
		// javzimu 不标注语种，只能从文件名猜；猜不出就按简体处理
		// （站点定位就是中文字幕站，且简体占绝对多数）
		lang, rank := langRank(it.Name)
		if rank == 0 {
			lang, rank = langSimp, 2
		}
		ext := strings.ToLower(it.Ext)
		if ext == "" {
			ext = "srt"
		}

		q := url.Values{}
		q.Set("cid", it.Cid)
		q.Set("ext", it.Ext)
		q.Set("name", it.Name)
		if it.TS != 0 {
			q.Set("_ts", strconv.FormatInt(it.TS, 10))
		}
		if it.Sig != "" {
			q.Set("_sig", it.Sig)
		}

		out = append(out, candidate{
			Source: s.Name(), Lang: lang, LangRank: rank,
			Size: 0, SizeText: "未知", // 站点不提供大小，后面用 HEAD 补
			URL:  javzimuBase + "api/download?" + q.Encode(),
			Referer: javzimuBase,
			Title:   it.Name, Ext: "." + ext,
		})
	}
	return out, nil
}

func decodeJZ(b []byte) (jzResp, bool) {
	var r jzResp
	if err := json.Unmarshal(b, &r); err != nil {
		return r, false
	}
	// 至少要有一个我们认识的字段，避免把 HTML 错误页当成 JSON
	if r.Result == "" && r.Error == "" && r.Data == nil {
		return r, false
	}
	return r, true
}

func jzError(r jzResp) error {
	switch {
	case r.TurnstileRequired:
		return fmt.Errorf("%w（Cloudflare Turnstile）", errVerification)
	case r.CaptchaRequired, r.CaptchaBlocked:
		return fmt.Errorf("%w（图片验证码）", errVerification)
	case r.Error != "":
		return errors.New(r.Error)
	}
	return errors.New("未知错误")
}

// ---------------------------------------------------------------- 公共小工具

func firstGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func extFromURL(rawURL string) string {
	if i := strings.IndexByte(rawURL, '?'); i >= 0 {
		rawURL = rawURL[:i]
	}
	if i := strings.LastIndexByte(rawURL, '.'); i >= 0 && len(rawURL)-i <= 5 {
		return strings.ToLower(rawURL[i:])
	}
	return ".srt"
}

func resolveAgainst(base, href string) string {
	b, err := url.Parse(base)
	if err != nil {
		return href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	return b.ResolveReference(u).String()
}

// parseSize 把 "15KB" / "1.2 MB" / "500B" 统一换算成字节
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

func humanSize(n int64) string {
	switch {
	case n <= 0:
		return "未知"
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.2f MB", float64(n)/(1024*1024))
	}
}

// rankCandidates 只保留中文候选，并按「语言优先（简 > 繁），同语言取文件最大」排序。
func rankCandidates(cands []candidate) []candidate {
	out := make([]candidate, 0, len(cands))
	for _, c := range cands {
		if c.LangRank > 0 {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].LangRank != out[j].LangRank {
			return out[i].LangRank > out[j].LangRank
		}
		return out[i].Size > out[j].Size
	})
	return out
}

// pickBest 跨站选优：语言优先（简 > 繁），同语言下取文件最大的那个。
func pickBest(cands []candidate) (candidate, bool) {
	ranked := rankCandidates(cands)
	if len(ranked) == 0 {
		return candidate{}, false
	}
	return ranked[0], true
}
