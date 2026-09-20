//go:build windows

// win32.go —— 纯 syscall 的 Win32 界面层
//
// 全程只用 syscall 调用系统 DLL，不引入 cgo、不依赖任何第三方库，
// 这样最终产物仍然是「一个 exe 双击即用」。
package main

import (
	"errors"
	"sync"
	"syscall"
	"unsafe"
)

// ---------------------------------------------------------------- DLL

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	ole32    = syscall.NewLazyDLL("ole32.dll")
	comctl32 = syscall.NewLazyDLL("comctl32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
)

var (
	// user32
	procRegisterClassExW   = user32.NewProc("RegisterClassExW")
	procCreateWindowExW    = user32.NewProc("CreateWindowExW")
	procDefWindowProcW     = user32.NewProc("DefWindowProcW")
	procDestroyWindow      = user32.NewProc("DestroyWindow")
	procShowWindow         = user32.NewProc("ShowWindow")
	procUpdateWindow       = user32.NewProc("UpdateWindow")
	procGetMessageW        = user32.NewProc("GetMessageW")
	procTranslateMessage   = user32.NewProc("TranslateMessage")
	procDispatchMessageW   = user32.NewProc("DispatchMessageW")
	procPostQuitMessage    = user32.NewProc("PostQuitMessage")
	procPostMessageW       = user32.NewProc("PostMessageW")
	procSendMessageW       = user32.NewProc("SendMessageW")
	procSetWindowTextW     = user32.NewProc("SetWindowTextW")
	procGetWindowTextW     = user32.NewProc("GetWindowTextW")
	procGetWindowTextLenW  = user32.NewProc("GetWindowTextLengthW")
	procMessageBoxW        = user32.NewProc("MessageBoxW")
	procGetClientRect      = user32.NewProc("GetClientRect")
	procMoveWindow         = user32.NewProc("MoveWindow")
	procLoadCursorW        = user32.NewProc("LoadCursorW")
	procLoadIconW          = user32.NewProc("LoadIconW")
	procEnableWindow       = user32.NewProc("EnableWindow")
	procSetTimer           = user32.NewProc("SetTimer")
	procKillTimer          = user32.NewProc("KillTimer")
	procScreenToClient     = user32.NewProc("ScreenToClient")
	procGetWindowLongPtrW  = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW  = user32.NewProc("SetWindowLongPtrW")
	procGetDpiForWindow    = user32.NewProc("GetDpiForWindow")
	procGetDpiForSystem    = user32.NewProc("GetDpiForSystem")
	procAdjustWindowRectEx = user32.NewProc("AdjustWindowRectEx")
	procSetWindowPos       = user32.NewProc("SetWindowPos")
	procGetSystemMetrics   = user32.NewProc("GetSystemMetrics")
	procSetFocus           = user32.NewProc("SetFocus")
	procSetForegroundWnd   = user32.NewProc("SetForegroundWindow")
	procIsWindowEnabled    = user32.NewProc("IsWindowEnabled")
	procOpenClipboard      = user32.NewProc("OpenClipboard")
	procCloseClipboard     = user32.NewProc("CloseClipboard")
	procEmptyClipboard     = user32.NewProc("EmptyClipboard")
	procSetClipboardData   = user32.NewProc("SetClipboardData")

	// kernel32
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")

	// comctl32
	procInitCommonControlsEx = comctl32.NewProc("InitCommonControlsEx")

	// gdi32
	procCreateFontW  = gdi32.NewProc("CreateFontW")
	procDeleteObject = gdi32.NewProc("DeleteObject")

	// shell32
	procSHBrowseForFolderW   = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListW = shell32.NewProc("SHGetPathFromIDListW")
	procExtractIconExW       = shell32.NewProc("ExtractIconExW")
	procShellExecuteW        = shell32.NewProc("ShellExecuteW")

	// ole32
	procCoInitializeEx = ole32.NewProc("CoInitializeEx")
	procCoUninitialize = ole32.NewProc("CoUninitialize")
	procCoTaskMemFree  = ole32.NewProc("CoTaskMemFree")
)

// ---------------------------------------------------------------- 常量

const (
	// 窗口样式
	wsOverlapped   = 0x00000000
	wsCaption      = 0x00C00000
	wsSysMenu      = 0x00080000
	wsThickFrame   = 0x00040000
	wsMinimizeBox  = 0x00020000
	wsMaximizeBox  = 0x00010000
	wsChild        = 0x40000000
	wsVisible      = 0x10000000
	wsTabStop      = 0x00010000
	wsBorder       = 0x00800000
	wsVScroll      = 0x00200000
	wsHScroll      = 0x00100000
	wsClipChildren = 0x02000000
	wsClipSiblings = 0x04000000

	// 扩展样式
	wsExClientEdge   = 0x00000200
	wsExStaticEdge   = 0x00020000
	wsExAppWindow    = 0x00040000
	wsExControlParen = 0x00010000

	// 消息
	wmDestroy     = 0x0002
	wmSize        = 0x0005
	wmSetFont     = 0x0030
	wmCommand     = 0x0111
	wmNotify      = 0x004E
	wmContextMenu = 0x007B
	wmClose       = 0x0010
	wmGetMinMaxInfo = 0x0024
	wmSetIcon     = 0x0080
	wmCtlColorStatic = 0x0138

	// 控件消息
	emSetSel        = 0x00B1
	emReplaceSel    = 0x00C2
	emSetReadOnly   = 0x00CF
	emSetLimitText  = 0x00C5
	lvmFirst        = 0x1000
	lvmDeleteAllItems = lvmFirst + 9
	lvmHitTest      = lvmFirst + 18
	lvmGetHeader    = lvmFirst + 31
	lvmSetItemState = lvmFirst + 43
	lvmInsertItemW  = lvmFirst + 77
	lvmSetItemTextW = lvmFirst + 116
	lvmInsertColumnW = lvmFirst + 97
	lvmSetExtendedListViewStyle = lvmFirst + 54
	lvmEnsureVisible = lvmFirst + 19
	lvmSetColumnWidth = lvmFirst + 30

	// 表头（Header Control）
	// 注意 HDM_FIRST+5、+6 是保留值，SETITEMW 在 +12，别按 GETITEMW(+11) 顺推。
	hdmFirst     = 0x1200
	hdmGetItemW  = hdmFirst + 11
	hdmSetItemW  = hdmFirst + 12
	hdiFormat    = 0x0004
	hdfSortUp    = 0x0400
	hdfSortDown  = 0x0200

	// 通知码（WM_NOTIFY 的 NMHDR.code）。这些是"负的"标识符：
	//   NM_FIRST  = (0U-0U)   = 0
	//   LVN_FIRST = (0U-100U) = -100
	// 写错就会静默收不到通知——LVN_COLUMNCLICK 是 -108，不是 -8。
	lvnFirst        = -100
	lvnColumnClick  = lvnFirst - 8
	nmFirst         = 0
	nmRClick        = nmFirst - 5

	// 列表项状态位
	lvisFocused  = 0x0001
	lvisSelected = 0x0002

	pbmSetRange32   = 0x0406
	pbmSetPos       = 0x0402
	pbmSetState     = 0x0404
	bmGetCheck      = 0x00F0
	bmSetCheck      = 0x00F1
	cbAddString     = 0x0143
	cbSetCurSel     = 0x014E
	cbGetCurSel     = 0x0147

	// 窗口显示
	swShow     = 5
	swMinimize = 6

	// 系统图标
	idiApplication = 32512
	idcArrow       = 32512
	idcWait        = 32513

	// 光标 / 图标加载
	imageIcon = 1
	lrDefaultSize = 0x00000040
	lrShared      = 0x00008000

	// BROWSEINFO 标志
	bifReturnOnlyFSDirs = 0x0001
	bifEditBox          = 0x0010
	bifNewDialogStyle   = 0x0040

	// COM
	coinitApartmentThreaded = 0x2
	coinitDisableOleDDE     = 0x4

	// MessageBox
	mbOK              = 0x00000000
	mbYesNo           = 0x00000004
	mbIconQuestion    = 0x00000020
	mbIconError       = 0x00000010
	mbIconWarning     = 0x00000030
	mbIconInformation = 0x00000040
	mbSetForeground   = 0x00010000

	idYes = 6
	idNo  = 7

	// 列表视图
	lvsReport          = 0x0001
	lvsSingleSel       = 0x0004
	lvsShowSelAlways   = 0x0008
	// 注意：LVS_NOSORTHEADER 是 0x8000。0x0040 是 LVS_SHAREIMAGELISTS，
	// 两者搞混会让"禁止列头排序"这个开关完全不生效。
	lvsNoSortHeader = 0x8000

	// 表头（Header）样式。HDS_BUTTONS 是列头可点、且会发 HDN_ITEMCLICK 的前提，
	// 而 ListView 实测并不会自动加上它。
	hdsButtons = 0x0001

	// GetWindowLongPtrW / SetWindowLongPtrW 的索引，-16 = GWL_STYLE
	gwlStyle = ^uintptr(15)
	lvsExGridLines     = 0x00000001
	lvsExFullRowSelect = 0x00000020
	lvsExDoubleBuffer  = 0x00010000

	// LVCOLUMN 的 mask 位（注意：和 LVITEM 的 LVIF_* 完全不是一套）
	lvcfFmt     = 0x0001
	lvcfWidth   = 0x0002
	lvcfText    = 0x0004
	lvcfSubItem = 0x0008

	// LVCFMT_* 列对齐方式
	lvcfmtLeft   = 0x0000
	lvcfmtRight  = 0x0001
	lvcfmtCenter = 0x0002

	// LVITEM 的 mask 位
	lvifText  = 0x0001
	lvifImage = 0x0002
	lvifState = 0x0008

	// 按钮样式
	bsPushButton   = 0x00000000
	bsAutoCheckBox = 0x00000003
	bsDefPushButton = 0x00000001

	// 编辑框样式
	esLeft        = 0x0000
	esAutoHScroll = 0x0080
	esReadOnly    = 0x0800
	esMultiline   = 0x0004

	// 静态文本样式
	ssLeft   = 0x00000000
	ssRight  = 0x00000002
	ssCenter = 0x00000001

	// 进度条
	pbstNormal = 0x00000000

	// 字体
	fwNormal    = 400
	fwBold      = 700
	defaultChar = 1

	// SetWindowPos
	swpNoZOrder   = 0x0004
	swpNoActivate = 0x0010

	// InitCommonControlsEx
	iccListView   = 0x00000001
	iccProgress   = 0x00000020
	iccBarClasses = 0x00000002

	// 剪贴板
	cfUnicodeText = 13
	gmemMoveable  = 0x0002

	// 窗口类样式
	csHRedraw = 0x0002
	csVRedraw = 0x0001
	csDblClks = 0x0008
)

// ---------------------------------------------------------------- 结构体

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  uintptr
	lpszClassName uintptr
	hIconSm       uintptr
}

type rectT struct {
	left, top, right, bottom int32
}

type pointT struct {
	x, y int32
}

type msgT struct {
	hwnd     uintptr
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       pointT
	lPrivate uint32
}

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

type initCommonControlsExT struct {
	dwSize uint32
	dwICC  uint32
}

type lvColumnW struct {
	mask       uint32
	fmt        int32
	cx         int32
	pszText    uintptr
	cchTextMax int32
	iSubItem   int32
	iImage     int32
	iOrder     int32
	cxMin      int32
	cxDefault  int32
	cxIdeal    int32
}

type lvItemW struct {
	mask       uint32
	iItem      int32
	iSubItem   int32
	state      uint32
	stateMask  uint32
	pszText    uintptr
	cchTextMax int32
	iImage     int32
	lParam     uintptr
	iIndent    int32
	iGroupID   int32
	cColumns   uint32
	puColumns  uintptr
	piColFmt   uintptr
	iGroup     int32
}

// nmhdr 是 WM_NOTIFY 的 lParam 指向的头三个字段（64 位下占 24 字节，含尾部对齐）。
type nmhdr struct {
	hwndFrom uintptr
	idFrom   uintptr
	code     uint32
}

// nmListView 对应 NMLISTVIEW，LVN_COLUMNCLICK / NM_RCLICK 都用它。
// 字段偏移：iItem 24、iSubItem 28、ptAction 44、lParam 56。
type nmListView struct {
	hdr       nmhdr
	iItem     int32
	iSubItem  int32
	uNewState uint32
	uOldState uint32
	uChanged  uint32
	ptAction  pointT
	lParam    uintptr
}

// lvHitTestInfo 对应 LVHITTESTINFO，用于把鼠标坐标换成列表项下标。
type lvHitTestInfo struct {
	pt       pointT
	flags    uint32
	iItem    int32
	iSubItem int32
}

// hdItemW 对应 HDITEMW，只用来读写表头的 fmt（排序箭头就藏在这里）。
type hdItemW struct {
	mask       uint32
	cxy        int32
	pszText    uintptr
	hbm        uintptr
	cchTextMax int32
	fmt        int32
	lParam     uintptr
	iImage     int32
	iOrder     int32
	typ        uint32
	pvFilter   uintptr
	state      uint32
}

// ---------------------------------------------------------------- 小工具

// 传给 Win32 的 UTF-16 缓冲区必须"活到系统读完为止"。
//
// Go 的 GC 看不见 uintptr 里藏着的指针，所以如果只返回一个 uintptr，
// 那块内存随时可能被回收，SendMessage 读到的就是垃圾——列表列头
// 文字整片消失就是这么来的。这里把缓冲区挂到一个环形切片上保活。
var (
	keepMu   sync.Mutex
	keepAlive [][]uint16
)

const keepAliveLimit = 4096

func utf16ptr(s string) uintptr {
	b, err := syscall.UTF16FromString(s)
	if err != nil || len(b) == 0 {
		b = []uint16{0}
	}
	keepMu.Lock()
	keepAlive = append(keepAlive, b)
	if len(keepAlive) > keepAliveLimit {
		keepAlive = keepAlive[len(keepAlive)-keepAliveLimit/2:]
	}
	keepMu.Unlock()
	return uintptr(unsafe.Pointer(&b[0]))
}

func loword(v uintptr) int { return int(v & 0xFFFF) }

func hiword(v uintptr) int { return int((v >> 16) & 0xFFFF) }

// lowordSigned / hiwordSigned 按有符号取：WM_CONTEXTMENU 的 lParam
// 装的是屏幕坐标，可能为负（多显示器在主屏左侧时）。
func lowordSigned(v uintptr) int32 { return int32(int16(v & 0xFFFF)) }

func hiwordSigned(v uintptr) int32 { return int32(int16((v >> 16) & 0xFFFF)) }

// sendMsg 向窗口/控件发送消息
func sendMsg(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	r, _, _ := procSendMessageW.Call(hwnd, uintptr(msg), wparam, lparam)
	return r
}

func setWindowText(hwnd uintptr, s string) {
	procSetWindowTextW.Call(hwnd, utf16ptr(s))
}

func getWindowText(hwnd uintptr) string {
	n, _, _ := procGetWindowTextLenW.Call(hwnd)
	if int(n) <= 0 {
		return ""
	}
	buf := make([]uint16, int(n)+1)
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}

func enableWindow(hwnd uintptr, enable bool) {
	v := uintptr(0)
	if enable {
		v = 1
	}
	procEnableWindow.Call(hwnd, v)
}

// setTimer 每 ms 毫秒给 hwnd 发一次 wmTimer。id 用于区分并取消。
func setTimer(hwnd uintptr, id uintptr, ms int) {
	procSetTimer.Call(hwnd, id, uintptr(ms), 0)
}

func killTimer(hwnd uintptr, id uintptr) {
	procKillTimer.Call(hwnd, id)
}

// ---------------------------------------------------------------- ListView 辅助

// setSortIndicator 在表头对应列上画出排序箭头（▲ 升序 / ▼ 降序）。
//
// 表头的箭头位在 HDITEM.fmt 里，得先读出来再改，
// 直接写 fmt 会把 HDF_STRING 之类的原始标志冲掉。
func setSortIndicator(list uintptr, col int, asc bool) {
	hdr := sendMsg(list, lvmGetHeader, 0, 0)
	if hdr == 0 {
		return
	}
	item := hdItemW{mask: hdiFormat}
	sendMsg(hdr, hdmGetItemW, uintptr(col), uintptr(unsafe.Pointer(&item)))

	item.fmt &^= hdfSortUp | hdfSortDown
	if asc {
		item.fmt |= hdfSortUp
	} else {
		item.fmt |= hdfSortDown
	}
	item.mask = hdiFormat
	sendMsg(hdr, hdmSetItemW, uintptr(col), uintptr(unsafe.Pointer(&item)))
}

// clearSortIndicator 抹掉所有列的排序箭头。
func clearSortIndicator(list uintptr, n int) {
	hdr := sendMsg(list, lvmGetHeader, 0, 0)
	if hdr == 0 {
		return
	}
	for i := 0; i < n; i++ {
		item := hdItemW{mask: hdiFormat}
		sendMsg(hdr, hdmGetItemW, uintptr(i), uintptr(unsafe.Pointer(&item)))
		item.fmt &^= hdfSortUp | hdfSortDown
		item.mask = hdiFormat
		sendMsg(hdr, hdmSetItemW, uintptr(i), uintptr(unsafe.Pointer(&item)))
	}
}

// selectListItem 选中并聚焦第 i 项（右键复制时给用户一个视觉反馈）。
func selectListItem(list uintptr, i int) {
	it := lvItemW{
		state:     lvisSelected | lvisFocused,
		stateMask: lvisSelected | lvisFocused,
	}
	sendMsg(list, lvmSetItemState, uintptr(i), uintptr(unsafe.Pointer(&it)))
}

// listHitTest 把客户区坐标换成 (项下标, 子列下标)，都没命中时返回 -1。
func listHitTest(list uintptr, x, y int32) (int, int) {
	hi := lvHitTestInfo{pt: pointT{x: x, y: y}, iItem: -1, iSubItem: -1}
	sendMsg(list, lvmHitTest, 0, uintptr(unsafe.Pointer(&hi)))
	return int(hi.iItem), int(hi.iSubItem)
}

// enableHeaderButtons 给 ListView 的表头补上 HDS_BUTTONS。
//
// 别指望 ListView 自己加：实测本机 comctl32 v6 建出来的表头样式是
// 0x500000c2（只有 HOTTRACK/DRAGDROP/FULLDRAG），没有 HDS_BUTTONS。
// 缺了它，表头不会发 HDN_ITEMCLICK，ListView 也就不会往上抛
// LVN_COLUMNCLICK —— 点列头排序会一点反应都没有。
func enableHeaderButtons(list uintptr) {
	hdr := sendMsg(list, lvmGetHeader, 0, 0)
	if hdr == 0 {
		return
	}
	style, _, _ := procGetWindowLongPtrW.Call(hdr, gwlStyle)
	procSetWindowLongPtrW.Call(hdr, gwlStyle, style|hdsButtons)
}

// screenToClient 把屏幕坐标换成 hwnd 的客户区坐标。
func screenToClient(hwnd uintptr, x, y int32) (int32, int32) {
	pt := pointT{x: x, y: y}
	procScreenToClient.Call(hwnd, uintptr(unsafe.Pointer(&pt)))
	return pt.x, pt.y
}

func isChecked(hwnd uintptr) bool {
	return sendMsg(hwnd, bmGetCheck, 0, 0) == 1
}

func setChecked(hwnd uintptr, on bool) {
	v := uintptr(0)
	if on {
		v = 1
	}
	sendMsg(hwnd, bmSetCheck, v, 0)
}

// messageBox 弹一个模态提示框，返回用户点的是哪个按钮（IDYES / IDNO / IDOK …）
func messageBox(owner uintptr, title, text string, flags uint) int {
	r, _, _ := procMessageBoxW.Call(owner, utf16ptr(text), utf16ptr(title), uintptr(flags))
	return int(r)
}

// scaleFor 按窗口 DPI 缩放尺寸。
// GetDpiForWindow 是 Win10 1607+ 才有的导出，找不到就退回 96 DPI。
func scaleFor(hwnd uintptr, v int) int {
	dpi := 96
	if hwnd != 0 && procGetDpiForWindow.Find() == nil {
		if d, _, _ := procGetDpiForWindow.Call(hwnd); d > 0 {
			dpi = int(d)
		}
	}
	return v * dpi / 96
}

// systemDPI 取主显示器 DPI，用于窗口创建前估算尺寸
func systemDPI() int {
	if procGetDpiForSystem.Find() == nil {
		if d, _, _ := procGetDpiForSystem.Call(); d > 0 {
			return int(d)
		}
	}
	return 96
}

// ---------------------------------------------------------------- 字体

type fontSet struct {
	normal uintptr
	bold   uintptr
	title  uintptr
}

func makeFont(dpi int, pointSize int, weight int) uintptr {
	height := -((pointSize * dpi) + 36) / 72
	face := utf16ptr("Microsoft YaHei UI")
	if face == 0 {
		return 0
	}
	h, _, _ := procCreateFontW.Call(
		uintptr(int32(height)), 0, 0, 0, uintptr(weight),
		0, 0, 0, defaultChar, 0, 0, 0, 0, face,
	)
	return h
}

func newFontSet(hwnd uintptr) fontSet {
	dpi := 96
	if hwnd != 0 {
		if d, _, _ := procGetDpiForWindow.Call(hwnd); d > 0 {
			dpi = int(d)
		}
	}
	return fontSet{
		normal: makeFont(dpi, 9, fwNormal),
		bold:   makeFont(dpi, 9, fwBold),
		title:  makeFont(dpi, 12, fwBold),
	}
}

// ---------------------------------------------------------------- 剪贴板

func copyToClipboard(hwnd uintptr, text string) error {
	procOpenClipboard.Call(hwnd)
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()

	u := syscall.StringToUTF16(text)
	size := uintptr(len(u) * 2)
	h, _, _ := procGlobalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return errors.New("分配剪贴板内存失败")
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return errors.New("锁定剪贴板内存失败")
	}
	dst := unsafe.Slice((*uint16)(unsafe.Pointer(p)), len(u))
	copy(dst, u)
	procGlobalUnlock.Call(h)
	procSetClipboardData.Call(cfUnicodeText, h)
	return nil
}

// ---------------------------------------------------------------- 图标

// loadAppIcon 取出 exe 内嵌的第一个图标（由 rsrc.syso 提供）。
// 用 ExtractIconExW 而不是 LoadIconW，这样不必关心资源 ID。
func loadAppIcon(exePath string) (large, small uintptr) {
	if exePath != "" {
		var lg, sm uintptr
		n, _, _ := procExtractIconExW.Call(
			utf16ptr(exePath), 0,
			uintptr(unsafe.Pointer(&lg)), uintptr(unsafe.Pointer(&sm)), 1)
		if n > 0 {
			return lg, sm
		}
	}
	l, _, _ := procLoadIconW.Call(0, idiApplication)
	s, _, _ := procLoadIconW.Call(0, idiApplication)
	return l, s
}

// ---------------------------------------------------------------- 文件夹选择框

func pickFolder(owner uintptr, title string) (string, error) {
	procCoInitializeEx.Call(0, coinitApartmentThreaded|coinitDisableOleDDE)
	defer procCoUninitialize.Call()

	display := make([]uint16, 260)
	bi := browseInfoW{
		hwndOwner:      owner,
		pszDisplayName: uintptr(unsafe.Pointer(&display[0])),
		lpszTitle:      utf16ptr(title),
		ulFlags:        bifReturnOnlyFSDirs | bifNewDialogStyle | bifEditBox,
	}

	pidl, _, _ := procSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return "", errCancelled
	}
	defer procCoTaskMemFree.Call(pidl)

	path := make([]uint16, 260)
	ok, _, _ := procSHGetPathFromIDListW.Call(pidl, uintptr(unsafe.Pointer(&path[0])))
	if ok == 0 {
		return "", errors.New("无法解析所选文件夹路径")
	}
	return syscall.UTF16ToString(path), nil
}

var errCancelled = errors.New("已取消选择")
