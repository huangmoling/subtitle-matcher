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
	wmApp          = 0x8000
	wmAppRefresh   = wmApp + 1
	wmAppDone      = wmApp + 2
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

type row struct {
	code   string
	source string
	lang   string
	size   string
	status string
	detail string
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
	cancel   context.CancelFunc
	started  time.Time

	initialDir string
}

var (
	gApp            *app
	wndProcCallback = syscall.NewCallback(wndProc)
)

// ---------------------------------------------------------------- 入口

func runGUI(initialDir string) int {
	runtime.LockOSThread()

	a := &app{initialDir: initialDir}
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
	width, height := 1000*dpi/96, 660*dpi/96

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
		wsChild|wsVisible|wsTabStop|wsBorder|lvsReport|lvsSingleSel|lvsShowSelAlways|lvsNoSortHeader,
		wsExClientEdge, idList)

	a.hProg = a.mk("msctls_progress32", "", wsChild|wsVisible, 0, idProgress)
	a.hStatus = a.mk("STATIC", "就绪", wsChild|wsVisible|ssLeft, 0, idStatus)

	// 列表视图
	sendMsg(a.hList, lvmSetExtendedListViewStyle, 0,
		lvsExGridLines|lvsExFullRowSelect|lvsExDoubleBuffer)

	cols := []struct {
		title string
		width int
		align int32
	}{
		{"番号", 170, lvcfmtLeft},
		{"来源", 110, lvcfmtLeft},
		{"语言", 60, lvcfmtCenter},
		{"大小", 80, lvcfmtRight},
		{"状态", 80, lvcfmtCenter},
		{"说明", 380, lvcfmtLeft},
	}
	for i, c := range cols {
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
	for _, wd := range []int{170, 110, 60, 80, 80} {
		fixed += scaleFor(a.hwnd, wd)
	}
	last := total - fixed - scaleFor(a.hwnd, 6)
	if last < scaleFor(a.hwnd, 160) {
		last = scaleFor(a.hwnd, 160)
	}
	sendMsg(a.hList, lvmSetColumnWidth, 5, uintptr(int32(last)))
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

func (a *app) insertRow(i int, r row) {
	it := lvItemW{
		mask:     lvifText,
		iItem:    int32(i),
		iSubItem: 0,
		pszText:  utf16ptr(r.code),
	}
	sendMsg(a.hList, lvmInsertItemW, 0, uintptr(unsafe.Pointer(&it)))
	a.setSubItem(i, 1, r.source)
	a.setSubItem(i, 2, r.lang)
	a.setSubItem(i, 3, r.size)
	a.setSubItem(i, 4, r.status)
	a.setSubItem(i, 5, r.detail)
}

// refresh 必须在 GUI 线程调用：把 Go 侧的状态同步到控件上
func (a *app) refresh() {
	a.mu.Lock()
	defer a.mu.Unlock()

	for a.shown < len(a.rows) {
		a.insertRow(a.shown, a.rows[a.shown])
		a.rendered = append(a.rendered, a.rowKey(a.rows[a.shown]))
		a.shown++
	}
	for i := 0; i < a.shown && i < len(a.rows); i++ {
		k := a.rowKey(a.rows[i])
		if k == a.rendered[i] {
			continue
		}
		r := a.rows[i]
		a.setSubItem(i, 1, r.source)
		a.setSubItem(i, 2, r.lang)
		a.setSubItem(i, 3, r.size)
		a.setSubItem(i, 4, r.status)
		a.setSubItem(i, 5, r.detail)
		a.rendered[i] = k
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

	switch {
	case a.running:
		setWindowText(a.hStatus, fmt.Sprintf(
			"进行中 %d / %d ｜ 成功 %d · 跳过 %d · 失败 %d ｜ 用时 %s",
			finished, len(a.rows), done, skipped, failed,
			time.Since(a.started).Truncate(time.Second)))
	case finished > 0:
		setWindowText(a.hStatus, fmt.Sprintf(
			"完成 ｜ 共 %d 个 · 成功 %d · 跳过 %d · 失败 %d ｜ 用时 %s",
			len(a.rows), done, skipped, failed,
			time.Since(a.started).Truncate(time.Second)))
	default:
		setWindowText(a.hStatus, "就绪：选择目录后点击「开始匹配」")
	}
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

	folders, ignored, err := scanFolders(root, cfg.OnlyCodes)
	if err != nil {
		messageBox(a.hwnd, appTitle, "读取目录失败：\n"+err.Error(), mbOK|mbIconError)
		return
	}
	if len(folders) == 0 {
		msg := "该目录下没有可处理的子文件夹。"
		if len(ignored) > 0 {
			msg += fmt.Sprintf("\n\n已按番号规则忽略 %d 个文件夹。", len(ignored))
		}
		messageBox(a.hwnd, appTitle, msg, mbOK|mbIconInformation)
		return
	}

	// 先把所有行铺出来，再开始跑，界面一眼能看到全貌
	a.mu.Lock()
	a.rows = make([]row, len(folders))
	for i, f := range folders {
		a.rows[i] = row{code: filepath.Base(f), status: stWaiting.Text()}
	}
	a.rendered = nil
	a.shown = 0
	a.running = true
	a.started = time.Now()
	a.mu.Unlock()

	sendMsg(a.hList, lvmDeleteAllItems, 0, 0)
	a.refresh()

	enableWindow(a.hStart, false)
	enableWindow(a.hStop, true)
	enableWindow(a.hBrowse, false)
	enableWindow(a.hEdit, false)

	ctx, cancel := context.WithCancel(context.Background())
	a.mu.Lock()
	a.cancel = cancel
	a.mu.Unlock()

	go func() {
		runAll(ctx, folders, cfg, func(p progress) {
			a.applyProgress(p)
			procPostMessageW.Call(a.hwnd, wmAppRefresh, 0, 0)
		})
		procPostMessageW.Call(a.hwnd, wmAppDone, 0, 0)
	}()
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
	if p.Detail != "" {
		r.detail = p.Detail
	}
}

func (a *app) stop() {
	a.mu.Lock()
	cancel := a.cancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	setWindowText(a.hStatus, "正在停止…（已发出的请求会先结束）")
}

func (a *app) finish() {
	a.mu.Lock()
	a.running = false
	a.mu.Unlock()

	enableWindow(a.hStart, true)
	enableWindow(a.hStop, false)
	enableWindow(a.hBrowse, true)
	enableWindow(a.hEdit, true)
	a.refresh()
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
		mmi.ptMinTrackSize.x = int32(scaleFor(hwnd, 820))
		mmi.ptMinTrackSize.y = int32(scaleFor(hwnd, 480))
		return 0

	case wmCommand:
		switch loword(wparam) {
		case idBtnBrowse:
			if path, err := pickFolder(hwnd, "请选择包含番号文件夹的字幕库目录"); err == nil {
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

	case wmAppRefresh:
		a.refresh()
		return 0

	case wmAppDone:
		a.finish()
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
