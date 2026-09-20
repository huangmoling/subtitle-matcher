//go:build windows

// engine.go —— 扫描、并行搜索、跨站选优、下载
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------- 配置

type config struct {
	OnlyCodes    bool // 只处理看起来像番号的文件夹
	Normalize    bool // 自动修正时间轴格式
	SkipExisting bool // 文件夹里已有字幕则跳过
	Concurrency  int  // 同时处理多少个文件夹

	// SearchTimeout 是「单个文件夹的搜索阶段」总时限。
	// 实测 aiyi1 的搜索接口偶尔会卡住几十秒，没有这个上限的话
	// 一个卡住的站点就能把整个任务拖死。
	SearchTimeout time.Duration
	// DownloadTimeout 是单个字幕文件的下载时限
	DownloadTimeout time.Duration
}

func defaultConfig() config {
	return config{
		OnlyCodes:    true,
		Normalize:    true,
		SkipExisting: true,
		Concurrency:  3,
		// subtitlecat 的搜索与详情页实测 1~8s（偶尔更久），
		// 一个文件夹通常要 2~4 次请求，45s 足够走完而不误判超时。
		SearchTimeout:   45 * time.Second,
		DownloadTimeout: 90 * time.Second,
	}
}

// withDefaults 补齐零值。
// 调用方很容易只填几个字段（比如只改 Concurrency），
// 漏掉的超时如果是 0，context.WithTimeout 会立刻到期，
// 结果就是"所有站点都超时"这种莫名其妙的表现。
func (c config) withDefaults() config {
	d := defaultConfig()
	if c.Concurrency <= 0 {
		c.Concurrency = d.Concurrency
	}
	if c.SearchTimeout <= 0 {
		c.SearchTimeout = d.SearchTimeout
	}
	if c.DownloadTimeout <= 0 {
		c.DownloadTimeout = d.DownloadTimeout
	}
	return c
}

// ---------------------------------------------------------------- 状态

type rowStatus int

const (
	stWaiting rowStatus = iota
	stSearching
	stDownloading
	stDone
	stSkipped
	stFailed
)

func (s rowStatus) Text() string {
	switch s {
	case stSearching:
		return "搜索中"
	case stDownloading:
		return "下载中"
	case stDone:
		return "已完成"
	case stSkipped:
		return "已跳过"
	case stFailed:
		return "失败"
	default:
		return "等待中"
	}
}

type progress struct {
	Index  int
	Status rowStatus
	Source string
	Lang   string
	Size   string
	Detail string
}

type summary struct {
	Total, Done, Skipped, Failed int
}

// ---------------------------------------------------------------- 来源集合

type sourceSet struct {
	list    []source
	getters map[string]*httpGetter
	dl      *httpGetter // 下载专用：CDN 往往比站点本身慢得多，给它更宽松的超时
}

func newSourceSet() *sourceSet {
	// 每个站点独立的限速、超时与重试次数：
	//   subtitlecat 有 Cloudflare，放慢一点、允许重试；
	//   aiyi1 的搜索接口会越请求越慢（实测 1.4s → 6s → 卡死），
	//        所以拉大间隔、超时收紧、且不重试——重试一个卡住的接口纯属浪费；
	//   javzimu 基本都要求人机验证，短超时快速失败即可。
	scGet := newHTTPGetter(700*time.Millisecond, 20*time.Second, 2)
	ayGet := newHTTPGetter(1000*time.Millisecond, 15*time.Second, 1)
	jzGet := newHTTPGetter(1500*time.Millisecond, 10*time.Second, 1)
	dlGet := newHTTPGetter(200*time.Millisecond, 60*time.Second, 2)

	sc := &subtitlecatSource{h: scGet}
	ay := &aiyi1Source{h: ayGet}
	jz := &javzimuSource{h: jzGet}

	return &sourceSet{
		list: []source{sc, ay, jz},
		getters: map[string]*httpGetter{
			sc.Name(): scGet,
			ay.Name(): ayGet,
			jz.Name(): jzGet,
		},
		dl: dlGet,
	}
}

// searchAll 并行搜索全部来源，哪个有结果用哪个。
//
// 关键点：等所有来源都返回，或者等到 ctx 超时为止——两者取先到。
// 某个站点卡住时，用已经拿到的候选继续往下走，而不是一起干等。
func (ss *sourceSet) searchAll(ctx context.Context, code string) ([]candidate, []string) {
	type result struct {
		name  string
		cands []candidate
		err   error
	}

	ch := make(chan result, len(ss.list))
	var wg sync.WaitGroup
	for _, s := range ss.list {
		wg.Add(1)
		go func(s source) {
			defer wg.Done()
			c, err := s.Search(ctx, code)
			// 带缓冲的通道，写不进去也不会把 goroutine 卡住
			select {
			case ch <- result{s.Name(), c, err}:
			default:
			}
		}(s)
	}

	var cands []candidate
	var notes []string
	for i := 0; i < len(ss.list); i++ {
		select {
		case r, ok := <-ch:
			if !ok {
				return cands, notes
			}
			switch {
			case r.err != nil:
				notes = append(notes, fmt.Sprintf("%s %s", r.name, shortErr(r.err)))
			case len(r.cands) == 0:
				notes = append(notes, fmt.Sprintf("%s 无结果", r.name))
			default:
				cands = append(cands, r.cands...)
			}
		case <-ctx.Done():
			notes = append(notes, fmt.Sprintf("部分站点响应超时（%d 个）", len(ss.list)-i))
			return cands, notes
		}
	}
	return cands, notes
}

// enrichSizes 对「站点没给大小」的候选补一次 HEAD，让比大小这件事公平。
func (ss *sourceSet) enrichSizes(ctx context.Context, cands []candidate) {
	const maxProbes = 3
	probes := 0
	for i := range cands {
		if cands[i].Size > 0 || probes >= maxProbes {
			continue
		}
		g := ss.getters[cands[i].Source]
		if g == nil {
			continue
		}
		probes++
		if n, err := g.Head(ctx, cands[i].URL, cands[i].Referer); err == nil && n > 0 {
			cands[i].Size = n
			cands[i].SizeText = humanSize(n)
		}
	}
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, errVerification):
		return "需要人机验证"
	case errors.Is(err, context.DeadlineExceeded):
		return "响应超时"
	case errors.Is(err, context.Canceled):
		return "已取消"
	}
	msg := err.Error()
	if len([]rune(msg)) > 50 {
		return string([]rune(msg)[:50]) + "…"
	}
	return msg
}

// ---------------------------------------------------------------- 扫描

// scanFolders 列出目录下的子文件夹。
// onlyCodes 为真时只保留「看起来像番号」的目录名。
func scanFolders(root string, onlyCodes bool) (folders, ignored []string, err error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if onlyCodes && !looksLikeCode(e.Name()) {
			ignored = append(ignored, e.Name())
			continue
		}
		folders = append(folders, filepath.Join(root, e.Name()))
	}
	sort.Strings(folders)
	sort.Strings(ignored)
	return folders, ignored, nil
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

// ---------------------------------------------------------------- 处理

type outcome struct {
	status rowStatus
	detail string
}

func (ss *sourceSet) processFolder(ctx context.Context, dir string, cfg config, report func(progress)) outcome {
	name := filepath.Base(dir)

	if cfg.SkipExisting {
		if existing, ok := hasSubtitle(dir); ok {
			return outcome{stSkipped, "已有字幕 " + existing}
		}
	}

	// 搜索阶段整体限时，卡住的站点会被掐断
	searchCtx, cancel := context.WithTimeout(ctx, cfg.SearchTimeout)
	cands, notes := ss.searchAll(searchCtx, name)
	cancel()

	if err := ctx.Err(); err != nil {
		return outcome{stSkipped, "已取消"}
	}

	if len(cands) == 0 {
		// 「站点超时」和「站点确实没有」要分开报：
		// 前者是网络/风控问题，稍后重试往往就好了，不该当成"没有字幕"。
		timeouts := 0
		for _, n := range notes {
			if strings.Contains(n, "超时") {
				timeouts++
			}
		}
		if timeouts == len(ss.list) {
			return outcome{stFailed, "所有站点响应超时，请稍后重试 ｜ " + strings.Join(notes, "；")}
		}
		detail := "未找到中文（简/繁）字幕"
		if len(notes) > 0 {
			detail += " ｜ " + strings.Join(notes, "；")
		}
		return outcome{stSkipped, detail}
	}

	// 站点没给大小的先补上，再比大小
	ss.enrichSizes(ctx, cands)

	ranked := rankCandidates(cands)
	if len(ranked) == 0 {
		return outcome{stSkipped, "结果中没有中文（简/繁）字幕"}
	}

	// 按优先级依次尝试：首选下载失败就退到下一个候选
	// （实测爱译网的 CDN 偶尔超时，但 subtitlecat 上有同一部片子）
	const maxDownloadTries = 3
	var lastErr error
	for i, c := range ranked {
		if i >= maxDownloadTries {
			break
		}
		if err := ctx.Err(); err != nil {
			return outcome{stSkipped, "已取消"}
		}

		report(progress{
			Status: stDownloading,
			Source: c.Source,
			Lang:   c.Lang,
			Size:   c.SizeText,
			Detail: fmt.Sprintf("选中 %s · %s · %s", c.Source, c.Lang, c.SizeText),
		})

		dlCtx, cancelDL := context.WithTimeout(ctx, cfg.DownloadTimeout)
		saved, n, fixed, err := ss.download(dlCtx, c, dir, name, cfg.Normalize)
		cancelDL()
		if err == nil {
			detail := fmt.Sprintf("%s · %s · %s → %s", c.Source, c.Lang, humanSize(n), saved)
			if fixed {
				detail += "（已规范化时间轴）"
			}
			return outcome{stDone, detail}
		}
		lastErr = err
	}
	return outcome{stFailed, "下载失败: " + shortErr(lastErr)}
}

// download 下载选中的候选并按内容决定扩展名，落盘为「文件夹同名.后缀」。
func (ss *sourceSet) download(ctx context.Context, c candidate, dir, baseName string, normalize bool) (string, int64, bool, error) {
	g := ss.dl
	if g == nil {
		g = newHTTPGetter(300*time.Millisecond, 60*time.Second, 2)
	}

	data, err := g.Get(ctx, c.URL, c.Referer, acceptAny)
	if err != nil {
		return "", 0, false, fmt.Errorf("下载失败: %w", err)
	}
	if !looksLikeSubtitle(data) {
		return "", 0, false, errors.New("返回内容不是字幕（可能被站点拦截）")
	}

	fixed := false
	if normalize {
		data, fixed = normalizeSubtitle(data)
	}

	// 按真实内容定扩展名：ASS 存成 .srt 会让播放器读不出来
	ext := ".srt"
	if isASS(data) {
		ext = ".ass"
	}
	dest := filepath.Join(dir, baseName+ext)

	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return "", 0, false, fmt.Errorf("写入失败: %w", err)
	}
	return baseName + ext, int64(len(data)), fixed, nil
}

// runAll 并发处理所有文件夹，边处理边通过 report 汇报进度。
func runAll(ctx context.Context, folders []string, cfg config, report func(progress)) summary {
	cfg = cfg.withDefaults()
	ss := newSourceSet()

	n := cfg.Concurrency
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}

	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var sum summary
	sum.Total = len(folders)

	for i, dir := range folders {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int, dir string) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			if ctx.Err() != nil {
				return
			}
			report(progress{Index: i, Status: stSearching, Detail: "并行搜索中…"})

			oc := ss.processFolder(ctx, dir, cfg, func(p progress) {
				p.Index = i
				report(p)
			})

			mu.Lock()
			switch oc.status {
			case stDone:
				sum.Done++
			case stSkipped:
				sum.Skipped++
			default:
				sum.Failed++
			}
			mu.Unlock()

			report(progress{Index: i, Status: oc.status, Detail: oc.detail})
		}(i, dir)
	}

	wg.Wait()
	return sum
}
