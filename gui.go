//go:build windows

// gui.go —— 图形界面
//
// 全部用 Win32 原生控件搭出来（Edit / Button / ListView / Progress），
// 不引入任何 GUI 框架，保证单文件 exe 体积和启动速度。
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// ---------------------------------------------------------------- 控件 ID

const (
	idLabelPath = 1001
	idEditPath  = 1002
	idBtnBrowse = 1003
	idBtnOpen   = 1004
	idChkCodes  = 1005
	idChkNorm   = 1006
	idChkSkip   = 1007
	idBtnStart  = 1008
	idBtnStop   = 1009
	idList      = 1010
	idProgress  = 1011
	idStatus    = 1012
)

const (
	wmCreate       = 0x0001
	wmEraseBkgnd   = 0x0014
	wmGetMinMaxInf = 0x0024
	wmTimer        = 0x0113
	wmApp          = 0x8000
	wmAppRefresh   = wmApp + 1
	wmAppDone      = wmApp + 2

	// 秒表定时器：等待站点响应时没有进度事件，靠它让"用时"继续走
	timerTick = 1
)

const (
	colorBtnFace = 15
	swShowNormal = 1
)

type minMaxInfo struct {
	ptReserved     pointT
	ptMaxSize      pointT
	ptMaxPosition  pointT
	ptMinTrackSize pointT
	ptMaxTrackSize pointT
}

// ---------------------------------------------------------------- 状态

// row 界面上的一行 = 一个视频文件。
//
// code 是视频文件名（含扩展名），字幕就用去掉扩展名的那部分命名；
// dir 是相对扫描根目录的所在目录 —— 递归扫描后必须显示它，
// 否则不同子目录里的同名文件在界面上没法区分。
type row struct {
	code      string
	dir       string
	source    string
	lang      string
	size      string
	sizeBytes int64
	status    string
	detail    string
}

// 列下标，排序与右键都要用，集中定义避免写错数字
const (
	colCode = iota
	colDir
	colSource
	colLang
	colSize
	colStatus
	colDetail
	colCount
)

// listColumns 列定义。顺序必须与上面的列下标常量一致。
// 最后一列（说明）的宽度由 layout() 按窗口剩余宽度动态算，这里给的是下限。
var listColumns = []struct {
	title string
	width int
	align int32
}{
	{"番号", 190, lvcfmtLeft},
	{"目录", 200, lvcfmtLeft},
	{"来源", 95, lvcfmtLeft},
	{"语言", 55, lvcfmtCenter},
	{"大小", 75, lvcfmtRight},
	{"状态", 75, lvcfmtCenter},
	{"说明", 240, lvcfmtLeft},
}

type app struct {
	hwnd  uintptr
	hInst uintptr

	hLabel   uintptr
	hEdit    uintptr
	hBrowse  uintptr
	hOpen    uintptr
	hChkCode uintptr
	hChkNorm uintptr
	hChkSkip uintptr
	hStart   uintptr
	hStop    uintptr
	hList    uintptr
	hProg    uintptr
	hStatus  uintptr

	fonts  fontSet
	icon   uintptr
	iconSm uintptr

	mu       sync.Mutex
	rows     []row
	shown    int
	rendered []string
	running  bool
	stopping bool
	cancel   context.CancelFunc
	started  time.Time

	// 排序：rows 始终保持扫描顺序（下标 == 进度回调里的 Index），
	// view 才是显示顺序，posOf 是它的逆映射。
	// 直接对 rows 排序会把进度更新打到错误的行上。
	view       []int
	posOf      []int
	sortCol    int
	sortAsc    bool
	fullRedraw bool
	sortHint   string // 空闲时显示"已按…排序"之类的提示

	initialDir string
}

var (
	gApp            *app
	wndProcCallback = syscall.NewCallback(wndProc)
)

// ---------------------------------------------------------------- 入口

func runGUI(initialDir string) int {
	runtime.LockOSThread()

	a := &app{initialDir: initialDir, sortCol: -1}
	gApp = a

	hInst, _, _ := procGetModuleHandleW.Call(0)
	a.hInst = hInst

	if exe, err := os.Executable(); err == nil {
		a.icon, a.iconSm = loadAppIcon(exe)
	}
	if a.icon == 0 {
		a.icon, _, _ = procLoadIconW.Call(0, idiApplication)
	}
	if a.iconSm == 0 {
		a.iconSm = a.icon
	}

	if err := a.registerClass(); err != nil {
		messageBox(0, "启动失败", err.Error(), mbOK|mbIconError)
		return 1
	}
	if err := a.createWindow(); err != nil {
		messageBox(0, "启动失败", err.Error(), mbOK|mbIconError)
		return 1
	}

	a.fonts = newFontSet(a.hwnd)
	a.createControls()
	a.applyFonts()
	a.layout()
	a.refresh()

	procShowWindow.Call(a.hwnd, swShow)
	procUpdateWindow.Call(a.hwnd)

	return a.messageLoop()
}

func (a *app) registerClass() error {
	arrow, _, _ := procLoadCursorW.Call(0, idcArrow)
	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		style:         csHRedraw | csVRedraw | csDblClks,
		lpfnWndProc:   wndProcCallback,
		hInstance:     a.hInst,
		hIcon:         a.icon,
		hCursor:       arrow,
		hbrBackground: colorBtnFace + 1,
		lpszClassName: utf16ptr("SubtitleMatcherWnd"),
		hIconSm:       a.iconSm,
	}
	r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if r == 0 {
		return fmt.Errorf("注册窗口类失败: %v", err)
	}
	return nil
}

func (a *app) createWindow() error {
	const (
		style = wsOverlapped | wsCaption | wsSysMenu | wsThickFrame |
			wsMinimizeBox | wsMaximizeBox | wsClipChildren
		exStyle = wsExAppWindow
	)

	// 清单里声明了 PerMonitorV2，窗口坐标是物理像素，所以先按系统 DPI 折算
	dpi := systemDPI()
	width, height := 1180*dpi/96, 680*dpi/96

	r := rectT{0, 0, int32(width), int32(height)}
	procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&r)), style, 0, exStyle)

	hwnd, _, err := procCreateWindowExW.Call(
		exStyle,
		utf16ptr("SubtitleMatcherWnd"),
		utf16ptr(appTitle),
		style,
		uintptr(cwUseDefault), uintptr(cwUseDefault),
		uintptr(r.right-r.left), uintptr(r.bottom-r.top),
		0, 0, a.hInst, 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("创建窗口失败: %v", err)
	}
	a.hwnd = hwnd
	return nil
}

const cwUseDefault = 0x80000000

// ---------------------------------------------------------------- 控件

func (a *app) mk(class, text string, style, exStyle uint32, id int) uintptr {
	h, _, _ := procCreateWindowExW.Call(
		uintptr(exStyle), utf16ptr(class), utf16ptr(text),
		uintptr(style), 0, 0, 10, 10,
		a.hwnd, uintptr(id), a.hInst, 0)
	return h
}

func (a *app) createControls() {
	icc := initCommonControlsExT{
		dwSize: uint32(unsafe.Sizeof(initCommonControlsExT{})),
		dwICC:  iccListView | iccProgress,
	}
	procInitCommonControlsEx.Call(uintptr(unsafe.Pointer(&icc)))

	a.hLabel = a.mk("STATIC", "字幕库目录", wsChild|wsVisible|ssLeft, 0, idLabelPath)
	a.hEdit = a.mk("EDIT", "", wsChild|wsVisible|wsTabStop|wsBorder|esAutoHScroll, wsExClientEdge, idEditPath)
	a.hBrowse = a.mk("BUTTON", "浏览…", wsChild|wsVisible|wsTabStop|bsPushButton, 0, idBtnBrowse)
	a.hOpen = a.mk("BUTTON", "打开目录", wsChild|wsVisible|wsTabStop|bsPushButton, 0, idBtnOpen)

	a.hChkCode = a.mk("BUTTON", "只处理番号文件夹", wsChild|wsVisible|wsTabStop|bsAutoCheckBox, 0, idChkCodes)
	a.hChkNorm = a.mk("BUTTON", "自动修正时间轴格式", wsChild|wsVisible|wsTabStop|bsAutoCheckBox, 0, idChkNorm)
	a.hChkSkip = a.mk("BUTTON", "已有字幕则跳过", wsChild|wsVisible|wsTabStop|bsAutoCheckBox, 0, idChkSkip)

	a.hStart = a.mk("BUTTON", "开始匹配", wsChild|wsVisible|wsTabStop|bsDefPushButton, 0, idBtnStart)
	a.hStop = a.mk("BUTTON", "停止", wsChild|wsVisible|wsTabStop|bsPushButton, 0, idBtnStop)

	a.hList = a.mk("SysListView32", "",
		wsChild|wsVisible|wsTabStop|wsBorder|lvsReport|lvsSingleSel|lvsShowSelAlways,
		wsExClientEdge, idList)

	a.hProg = a.mk("msctls_progress32", "", wsChild|wsVisible, 0, idProgress)
	a.hStatus = a.mk("STATIC", "就绪", wsChild|wsVisible|ssLeft, 0, idStatus)

	// 列表视图
	sendMsg(a.hList, lvmSetExtendedListViewStyle, 0,
		lvsExGridLines|lvsExFullRowSelect|lvsExDoubleBuffer)

	// 列头要能点（排序）必须显式打开 HDS_BUTTONS，ListView 不会自动加
	enableHeaderButtons(a.hList)

	// 列宽集中放这里：layout() 算「说明」列剩余宽度时也用这份数据
	for i, c := range listColumns {
		col := lvColumnW{
			mask:     lvcfFmt | lvcfWidth | lvcfText | lvcfSubItem,
			fmt:      c.align,
			cx:       int32(scaleFor(a.hwnd, c.width)),
			iSubItem: int32(i),
			pszText:  utf16ptr(c.title),
		}
		sendMsg(a.hList, lvmInsertColumnW, uintptr(i), uintptr(unsafe.Pointer(&col)))
	}

	setChecked(a.hChkCode, true)
	setChecked(a.hChkNorm, true)
	setChecked(a.hChkSkip, true)
	enableWindow(a.hStop, false)

	if a.initialDir != "" {
		if abs, err := filepath.Abs(a.initialDir); err == nil {
			setWindowText(a.hEdit, abs)
		} else {
			setWindowText(a.hEdit, a.initialDir)
		}
	}

	sendMsg(a.hProg, pbmSetRange32, 0, 1000)
}

func (a *app) applyFonts() {
	if a.fonts.normal == 0 {
		return
	}
	for _, h := range []uintptr{
		a.hLabel, a.hEdit, a.hBrowse, a.hOpen,
		a.hChkCode, a.hChkNorm, a.hChkSkip,
		a.hStart, a.hStop, a.hList, a.hStatus,
	} {
		if h != 0 {
			sendMsg(h, wmSetFont, a.fonts.normal, 1)
		}
	}
}

// ---------------------------------------------------------------- 布局

func (a *app) layout() {
	var cr rectT
	procGetClientRect.Call(a.hwnd, uintptr(unsafe.Pointer(&cr)))
	w, h := int(cr.right), int(cr.bottom)

	s := func(v int) int { return scaleFor(a.hwnd, v) }
	margin := s(14)
	rowH := s(26)
	gap := s(8)

	y := margin

	// 第 1 行：目录
	lblW := s(80)
	btnW := s(80)
	a.move(a.hLabel, margin, y+s(4), lblW, s(20))

	editX := margin + lblW + gap
	editW := w - editX - margin - btnW*2 - gap*2
	if editW < s(120) {
		editW = s(120)
	}
	a.move(a.hEdit, editX, y, editW, rowH)

	a.move(a.hBrowse, editX+editW+gap, y, btnW, rowH)
	a.move(a.hOpen, editX+editW+gap*2+btnW, y, btnW, rowH)

	y += rowH + s(10)

	// 第 2 行：选项
	a.move(a.hChkCode, margin, y, s(130), s(22))
	a.move(a.hChkNorm, margin+s(140), y, s(140), s(22))
	a.move(a.hChkSkip, margin+s(290), y, s(130), s(22))

	y += s(22) + s(10)

	// 第 3 行：操作按钮
	a.move(a.hStart, margin, y, s(100), s(30))
	a.move(a.hStop, margin+s(110), y, s(80), s(30))

	y += s(30) + s(10)

	// 底部
	statusH := s(20)
	progH := s(18)
	bottom := h - margin
	statusY := bottom - statusH
	progY := statusY - gap - progH
	listH := progY - s(10) - y
	if listH < s(80) {
		listH = s(80)
	}

	a.move(a.hList, margin, y, w-margin*2, listH)
	a.move(a.hProg, margin, progY, w-margin*2, progH)
	a.move(a.hStatus, margin, statusY, w-margin*2, statusH)

	a.resizeLastColumn(w - margin*2)
}

func (a *app) move(h uintptr, x, y, w, hh int) {
	if h == 0 {
		return
	}
	procMoveWindow.Call(h, uintptr(int32(x)), uintptr(int32(y)),
		uintptr(int32(w)), uintptr(int32(hh)), 1)
}

// resizeLastColumn 让「说明」列吃掉剩余宽度
func (a *app) resizeLastColumn(total int) {
	fixed := 0
	for i := 0; i < colCount-1; i++ {
		fixed += scaleFor(a.hwnd, listColumns[i].width)
	}
	last := total - fixed - scaleFor(a.hwnd, 6)
	min := scaleFor(a.hwnd, listColumns[colCount-1].width)
	if last < min {
		last = min
	}
	sendMsg(a.hList, lvmSetColumnWidth, uintptr(colCount-1), uintptr(int32(last)))
}

// ---------------------------------------------------------------- 列表刷新

func (a *app) rowKey(r row) string {
	return r.source + "\x00" + r.lang + "\x00" + r.size + "\x00" + r.status + "\x00" + r.detail
}

func (a *app) setSubItem(i, sub int, text string) {
	it := lvItemW{
		mask:     lvifText,
		iItem:    int32(i),
		iSubItem: int32(sub),
		pszText:  utf16ptr(text),
	}
	sendMsg(a.hList, lvmSetItemTextW, uintptr(i), uintptr(unsafe.Pointer(&it)))
}

// setRow 把第 pos 行（显示位置）整行刷成 r
func (a *app) setRow(pos int, r row) {
	a.setSubItem(pos, colCode, r.code)
	a.setSubItem(pos, colDir, r.dir)
	a.setSubItem(pos, colSource, r.source)
	a.setSubItem(pos, colLang, r.lang)
	a.setSubItem(pos, colSize, r.size)
	a.setSubItem(pos, colStatus, r.status)
	a.setSubItem(pos, colDetail, r.detail)
}

func (a *app) insertRow(pos int, r row) {
	it := lvItemW{
		mask:     lvifText,
		iItem:    int32(pos),
		iSubItem: 0,
		pszText:  utf16ptr(r.code),
	}
	sendMsg(a.hList, lvmInsertItemW, 0, uintptr(unsafe.Pointer(&it)))
	a.setSubItem(pos, colDir, r.dir)
	a.setSubItem(pos, colSource, r.source)
	a.setSubItem(pos, colLang, r.lang)
	a.setSubItem(pos, colSize, r.size)
	a.setSubItem(pos, colStatus, r.status)
	a.setSubItem(pos, colDetail, r.detail)
}

// ---------------------------------------------------------------- 排序

// rebuildViewLocked 按当前排序设置重算显示顺序。
// 调用方必须已持有 a.mu。
func (a *app) rebuildViewLocked() {
	a.view = make([]int, len(a.rows))
	for i := range a.view {
		a.view[i] = i
	}
	if a.sortCol >= 0 {
		sort.SliceStable(a.view, func(x, y int) bool {
			ix, iy := a.view[x], a.view[y]
			if a.sortAsc {
				return a.lessLocked(ix, iy, a.sortCol)
			}
			return a.lessLocked(iy, ix, a.sortCol)
		})
	}
	a.posOf = make([]int, len(a.rows))
	for pos, idx := range a.view {
		a.posOf[idx] = pos
	}
	// 顺序变了，整张表都得重画
	a.fullRedraw = true
}

// lessLocked 是排序比较函数。调用方必须已持有 a.mu。
func (a *app) lessLocked(i, j, col int) bool {
	ri, rj := a.rows[i], a.rows[j]
	switch col {
	case colCode:
		return naturalLess(ri.code, rj.code)
	case colDir:
		return naturalLess(ri.dir, rj.dir)
	case colSource:
		if ri.source != rj.source {
			return ri.source < rj.source
		}
		return naturalLess(ri.code, rj.code)
	case colLang:
		_, li := langRank(ri.lang)
		_, lj := langRank(rj.lang)
		if li != lj {
			return li < lj
		}
		return naturalLess(ri.code, rj.code)
	case colSize:
		if ri.sizeBytes != rj.sizeBytes {
			return ri.sizeBytes < rj.sizeBytes
		}
		return naturalLess(ri.code, rj.code)
	case colStatus:
		si, sj := statusRank(ri.status), statusRank(rj.status)
		if si != sj {
			return si < sj
		}
		return naturalLess(ri.code, rj.code)
	default:
		if ri.detail != rj.detail {
			return ri.detail < rj.detail
		}
		return naturalLess(ri.code, rj.code)
	}
}

// statusRank 让「需要关注」的状态排在前面，而不是按拼音排。
func statusRank(s string) int {
	switch s {
	case stFailed.Text():
		return 0
	case stDownloading.Text():
		return 1
	case stSearching.Text():
		return 2
	case stWaiting.Text():
		return 3
	case stSkipped.Text():
		return 4
	case stDone.Text():
		return 5
	}
	return 6
}

// sortBy 响应表头点击：同一列再点一次就反向。
// 必须在 GUI 线程调用。
func (a *app) sortBy(col int) {
	if col < 0 || col >= colCount {
		return
	}

	a.mu.Lock()
	if a.sortCol == col {
		a.sortAsc = !a.sortAsc
	} else {
		a.sortCol = col
		// 大小默认从大到小（用户最关心最大的），其余从 A 到 Z
		a.sortAsc = col != colSize
	}
	a.rebuildViewLocked()
	n := len(a.rows)
	asc := a.sortAsc

	arrow := "▲"
	if !asc {
		arrow = "▼"
	}
	a.sortHint = fmt.Sprintf("「%s」%s（%d 项）", listColumns[col].title, arrow, n)
	a.mu.Unlock()

	clearSortIndicator(a.hList, colCount)
	setSortIndicator(a.hList, col, asc)
	a.refresh()
}

// naturalLess 让 ABF-2 排在 ABF-10 前面（纯字符串比较会反过来）。
func naturalLess(a, b string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		da, db := isDigitByte(a[i]), isDigitByte(b[j])
		if da && db {
			si, sj := i, j
			for i < len(a) && isDigitByte(a[i]) {
				i++
			}
			for j < len(b) && isDigitByte(b[j]) {
				j++
			}
			na := strings.TrimLeft(a[si:i], "0")
			nb := strings.TrimLeft(b[sj:j], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		ca, cb := lowerByte(a[i]), lowerByte(b[j])
		if ca != cb {
			return ca < cb
		}
		i++
		j++
	}
	return len(a)-i < len(b)-j
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

func lowerByte(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// ---------------------------------------------------------------- 列表刷新

// refresh 必须在 GUI 线程调用：把 Go 侧的状态同步到控件上
func (a *app) refresh() {
	a.mu.Lock()
	defer a.mu.Unlock()

	// 排序变化后整表重画
	if a.fullRedraw {
		sendMsg(a.hList, lvmDeleteAllItems, 0, 0)
		a.shown = 0
		a.rendered = nil
		a.fullRedraw = false
	}

	for a.shown < len(a.view) {
		idx := a.view[a.shown]
		a.insertRow(a.shown, a.rows[idx])
		a.rendered = append(a.rendered, a.rowKey(a.rows[idx]))
		a.shown++
	}
	for pos := 0; pos < a.shown && pos < len(a.view); pos++ {
		idx := a.view[pos]
		k := a.rowKey(a.rows[idx])
		if k == a.rendered[pos] {
			continue
		}
		a.setRow(pos, a.rows[idx])
		a.rendered[pos] = k
	}

	// 统计
	var done, skipped, failed, finished int
	for _, r := range a.rows {
		switch r.status {
		case stDone.Text():
			done++
			finished++
		case stSkipped.Text():
			skipped++
			finished++
		case stFailed.Text():
			failed++
			finished++
		}
	}

	if len(a.rows) > 0 {
		sendMsg(a.hProg, pbmSetPos, uintptr(finished*1000/len(a.rows)), 0)
	}

	// 状态栏正文按优先级取，排序信息一律追加在后面 ——
	// 否则跑完之后点表头排序，用户看不到任何反馈。
	var base string
	switch {
	case a.stopping:
		base = "正在停止…（已发出的请求会先结束）"
	case a.running:
		base = fmt.Sprintf(
			"进行中 %d / %d ｜ 成功 %d · 跳过 %d · 失败 %d ｜ 用时 %s",
			finished, len(a.rows), done, skipped, failed,
			time.Since(a.started).Truncate(time.Second))
	case finished > 0:
		base = fmt.Sprintf(
			"完成 ｜ 共 %d 个 · 成功 %d · 跳过 %d · 失败 %d ｜ 用时 %s",
			len(a.rows), done, skipped, failed,
			time.Since(a.started).Truncate(time.Second))
	default:
		base = "就绪：选择目录后点击「开始匹配」｜ 点表头可排序，行上点右键可复制文件名"
	}
	if a.sortHint != "" && !a.stopping {
		base += " ｜ 排序：" + a.sortHint
	}
	setWindowText(a.hStatus, base)
}

// ---------------------------------------------------------------- 任务

func (a *app) start() {
	if a.running {
		return
	}
	root := strings.TrimSpace(getWindowText(a.hEdit))
	if root == "" {
		messageBox(a.hwnd, appTitle, "请先选择字幕库目录。", mbOK|mbIconWarning)
		return
	}
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		messageBox(a.hwnd, appTitle, "目录不存在或不可访问：\n"+root, mbOK|mbIconWarning)
		return
	}

	cfg := config{
		OnlyCodes:    isChecked(a.hChkCode),
		Normalize:    isChecked(a.hChkNorm),
		SkipExisting: isChecked(a.hChkSkip),
		Concurrency:  3,
	}

	targets, ignored, err := scanVideos(root, cfg.OnlyCodes)
	if err != nil {
		messageBox(a.hwnd, appTitle, "读取目录失败：\n"+err.Error(), mbOK|mbIconError)
		return
	}
	if len(targets) == 0 {
		msg := "该目录下没有找到视频文件。\n\n支持的格式：\n" +
			strings.Join(videoExtList(), "、")
		if len(ignored) > 0 {
			msg += fmt.Sprintf("\n\n已按番号规则忽略 %d 个视频（取消勾选「只处理番号」可全部处理）。", len(ignored))
		}
		messageBox(a.hwnd, appTitle, msg, mbOK|mbIconInformation)
		return
	}

	// 先把所有行铺出来，再开始跑，界面一眼能看到全貌
	a.mu.Lock()
	a.rows = make([]row, len(targets))
	for i, t := range targets {
		a.rows[i] = row{
			code:   t.Name,
			dir:    t.Rel,
			status: stWaiting.Text(),
		}
	}
	a.shown = 0
	a.rendered = nil
	a.sortCol = -1 // 默认按扫描顺序（即路径顺序）
	a.sortHint = ""
	a.running = true
	a.stopping = false
	a.started = time.Now()
	a.rebuildViewLocked()
	a.mu.Unlock()

	clearSortIndicator(a.hList, colCount)
	a.refresh()

	setTimer(a.hwnd, timerTick, 1000)

	enableWindow(a.hStart, false)
	enableWindow(a.hStop, true)
	enableWindow(a.hBrowse, false)
	enableWindow(a.hEdit, false)

	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.cancel = cancel
	a.mu.Unlock()

	go func() {
		runAll(ctx, targets, cfg, func(p progress) {
			a.applyProgress(p)
			procPostMessageW.Call(a.hwnd, wmAppRefresh, 0, 0)
		})
		procPostMessageW.Call(a.hwnd, wmAppDone, 0, 0)
	}()
}

// videoExtList 把支持的扩展名排成稳定的顺序，只用于提示文案。
func videoExtList() []string {
	exts := make([]string, 0, len(videoExts))
	for e := range videoExts {
		exts = append(exts, e)
	}
	sort.Strings(exts)
	return exts
}

func (a *app) applyProgress(p progress) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if p.Index < 0 || p.Index >= len(a.rows) {
		return
	}
	r := &a.rows[p.Index]
	if p.Status != stWaiting {
		r.status = p.Status.Text()
	}
	if p.Source != "" {
		r.source = p.Source
	}
	if p.Lang != "" {
		r.lang = p.Lang
	}
	if p.Size != "" {
		r.size = p.Size
	}
	if p.SizeBytes > 0 {
		r.sizeBytes = p.SizeBytes
	}
	if p.Detail != "" {
		r.detail = p.Detail
	}
}

func (a *app) stop() {
	a.mu.Lock()
	cancel := a.cancel
	a.stopping = true
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	setWindowText(a.hStatus, "正在停止…（已发出的请求会先结束）")
}

func (a *app) finish() {
	a.mu.Lock()
	a.running = false
	a.stopping = false
	a.mu.Unlock()

	killTimer(a.hwnd, timerTick)

	enableWindow(a.hStart, true)
	enableWindow(a.hStop, false)
	enableWindow(a.hBrowse, true)
	enableWindow(a.hEdit, true)
	a.refresh()
}

// ---------------------------------------------------------------- 复制

// copyRowName 把第 pos 行（显示位置，不是扫描下标）的视频文件名放进剪贴板。
// 右键的 NM_RCLICK 和 WM_CONTEXTMENU 两条路径都走这里。
func (a *app) copyRowName(pos int) {
	a.mu.Lock()
	if pos < 0 || pos >= len(a.view) {
		a.mu.Unlock()
		return
	}
	r := a.rows[a.view[pos]]
	a.mu.Unlock()

	name := r.code
	if name == "" {
		return
	}
	if err := copyToClipboard(a.hwnd, name); err != nil {
		setWindowText(a.hStatus, "复制失败："+err.Error())
		return
	}

	// 选中该行，让用户看清复制的是哪一条
	selectListItem(a.hList, pos)

	where := r.dir
	if where == "" || where == "." {
		where = "根目录"
	}
	setWindowText(a.hStatus, fmt.Sprintf("已复制文件名：%s ｜ 位置：%s", name, where))
}

// ---------------------------------------------------------------- 窗口过程

func wndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	a := gApp
	if a == nil {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
		return r
	}

	switch msg {
	case wmCreate:
		a.hwnd = hwnd
		return 0

	case wmSize:
		a.layout()
		return 0

	case wmGetMinMaxInf:
		mmi := (*minMaxInfo)(unsafe.Pointer(lparam))
		mmi.ptMinTrackSize.x = int32(scaleFor(hwnd, 1000))
		mmi.ptMinTrackSize.y = int32(scaleFor(hwnd, 520))
		return 0

	case wmCommand:
		switch loword(wparam) {
		case idBtnBrowse:
			if path, err := pickFolder(hwnd, "请选择要扫描的字幕库目录"); err == nil {
				setWindowText(a.hEdit, path)
			}
		case idBtnOpen:
			path := strings.TrimSpace(getWindowText(a.hEdit))
			if path != "" {
				procShellExecuteW.Call(hwnd, utf16ptr("open"), utf16ptr(path), 0, 0, swShowNormal)
			}
		case idBtnStart:
			a.start()
		case idBtnStop:
			a.stop()
		}
		return 0

	case wmNotify:
		nm := (*nmListView)(unsafe.Pointer(lparam))
		switch int32(nm.hdr.code) {
		case lvnColumnClick:
			a.sortBy(int(nm.iSubItem))
			return 0
		case nmRClick:
			if nm.iItem >= 0 {
				a.copyRowName(int(nm.iItem))
			}
			return 0
		}
		return 0

	case wmContextMenu:
		// 有些情况下 ListView 只把右键转成 WM_CONTEXTMENU（不带 NM_RCLICK），
		// 这里补一条命中测试的路径，两条路都走同一个复制逻辑。
		if wparam != a.hList {
			break
		}
		cx, cy := screenToClient(a.hList, lowordSigned(lparam), hiwordSigned(lparam))
		if item, _ := listHitTest(a.hList, cx, cy); item >= 0 {
			a.copyRowName(item)
		}
		return 0

	case wmAppRefresh:
		a.refresh()
		return 0

	case wmAppDone:
		a.finish()
		return 0

	case wmTimer:
		if wparam == timerTick {
			a.refresh()
		}
		return 0

	case wmClose:
		a.mu.Lock()
		running := a.running
		a.mu.Unlock()
		if running {
			r := messageBox(hwnd, appTitle,
				"任务正在运行。\n\n确定要停止并退出吗？",
				mbYesNo|mbIconQuestion|mbSetForeground)
			if r != idYes {
				return 0
			}
			a.stop()
		}
		procDestroyWindow.Call(hwnd)
		return 0

	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}

	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

func (a *app) messageLoop() int {
	var msg msgT
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
	return int(msg.wParam)
}
