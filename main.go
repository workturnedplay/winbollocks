//go:build windows

// Copyright 2026 workturnedplay
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows"
)

/* ---------------- DLLs & Procs ---------------- */

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	shcore   = windows.NewLazySystemDLL("shcore.dll")

	procSetWindowsHookEx    = user32.NewProc("SetWindowsHookExW")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procGetMessage          = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessage     = user32.NewProc("DispatchMessageW")

	procGetAsyncKeyState    = user32.NewProc("GetAsyncKeyState")
	procWindowFromPoint     = user32.NewProc("WindowFromPoint")
	procGetAncestor         = user32.NewProc("GetAncestor")
	procReleaseCapture      = user32.NewProc("ReleaseCapture")
	procSendMessage         = user32.NewProc("SendMessageW")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")

	procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")

	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procGetWindowPlacement       = user32.NewProc("GetWindowPlacement")
	procGetWindowRect            = user32.NewProc("GetWindowRect")
	procShowWindow               = user32.NewProc("ShowWindow")
	procSetWindowPos             = user32.NewProc("SetWindowPos")

	procDefWindowProc   = user32.NewProc("DefWindowProcW")
	procRegisterClassEx = user32.NewProc("RegisterClassExW")
	procCreateWindowEx  = user32.NewProc("CreateWindowExW")

	procGetModuleHandle = kernel32.NewProc("GetModuleHandleW")

	procSetCapture = user32.NewProc("SetCapture")

	procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")

	procGetForegroundWindow = user32.NewProc("GetForegroundWindow")

	procCreatePopupMenu = user32.NewProc("CreatePopupMenu")
	procAppendMenu      = user32.NewProc("AppendMenuW")
	procTrackPopupMenu  = user32.NewProc("TrackPopupMenu")
	procGetCursorPos    = user32.NewProc("GetCursorPos")

	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetProcessDpiAwareness        = shcore.NewProc("SetProcessDpiAwareness")

	procAttachThreadInput = user32.NewProc("AttachThreadInput")

	procPostMessage = user32.NewProc("PostMessageW")

	procGetDesktopWindow = user32.NewProc("GetDesktopWindow")
)

/* ---------------- Constants ---------------- */

const (

	// Low-level keyboard hook flag
	LLKHF_INJECTED = 0x00000010
)

const (
	NOTIFYICON_VERSION_4 = 4
	NIM_SETVERSION       = 0x00000004
)

const (
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = (HANDLE)-4
	DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = ^uintptr(3)

	// PROCESS_PER_MONITOR_DPI_AWARE = 2
	PROCESS_PER_MONITOR_DPI_AWARE = 2
)

const (
	WM_MBUTTONDOWN = 0x0207
	HWND_BOTTOM    = uintptr(1) // good
	//HWND_TOP       = ^uintptr(1) // (HWND)-1  bad AI
	HWND_TOP = uintptr(0) // good

	HWND_TOPMOST   = ^uintptr(0) // (HWND)-1
	HWND_NOTOPMOST = ^uintptr(1) // (HWND)-2
	//HWND_TOP       = ^uintptr(2) // (HWND)-3 bad
	//HWND_BOTTOM    = ^uintptr(3) // (HWND)-4 bad, gg AI

)

const (
	WH_MOUSE_LL = 14

	WM_LBUTTONDOWN   = 0x0201
	WM_LBUTTONUP     = 0x0202
	WM_MOUSEMOVE     = 0x0200
	WM_NCLBUTTONDOWN = 0x00A1

	HTCAPTION = 2

	GA_ROOT      = 2
	GA_ROOTOWNER = 3

	NIM_ADD    = 0x00000000
	NIM_MODIFY = 0x00000001

	NIF_TIP  = 0x00000004
	NIF_INFO = 0x00000010
)

const (
	SW_RESTORE  = 9
	SW_MAXIMIZE = 3

	SWP_NOSIZE     = 0x0001
	SWP_NOMOVE     = 0x0002
	SWP_NOZORDER   = 0x0004
	SWP_NOACTIVATE = 0x0010
)

const (
	WM_SYSCOMMAND = 0x0112
	SC_MOVE       = 0xF010
)

// Win32 message constants missing from x/sys/windows
const (
	WM_USER      = 0x0400
	WM_CLOSE     = 0x0010
	WM_RBUTTONUP = 0x0205
)

const (
	WM_START_NATIVE_DRAG = WM_USER + 1
	WM_TRAY              = WM_USER + 2
	WM_INJECT_SEQUENCE   = WM_USER + 100
)
const (
	MENU_FORCE_MANUAL  = 1
	MENU_ACTIVATE_MOVE = 2
	MENU_EXIT          = 3
	MF_STRING          = 0x0000
	MF_CHECKED         = 0x0008
)

const (
	WM_KEYDOWN    = 0x0100
	WM_KEYUP      = 0x0101
	WM_SYSKEYDOWN = 0x0104
	WM_SYSKEYUP   = 0x0105
)

const (
	WH_KEYBOARD_LL = 13
)

const (
	INPUT_KEYBOARD     = 1
	KEYEVENTF_KEYUP    = 0x0002
	KEYEVENTF_SCANCODE = 0x0008
	KEYEVENTF_EXTENDED = 0x0001

	// Modifier virtual keys
	VK_SHIFT   = 0x10
	VK_CONTROL = 0x11
	VK_MENU    = 0x12 // Alt key
	//no VK_WIN exists, must OR the two manually

	VK_LBUTTON = 0x01
	VK_RBUTTON = 0x02
	VK_MBUTTON = 0x04
	VK_LWIN    = 0x5B
	VK_RWIN    = 0x5C

	VK_LSHIFT = 0xA0
	VK_RSHIFT = 0xA1

	VK_LCONTROL = 0xA2
	VK_RCONTROL = 0xA3
	VK_LMENU    = 0xA4 // Left Alt
	VK_RMENU    = 0xA5 // Right Alt

	VK_E   = 0x45
	VK_F   = 0x46
	VK_F12 = 0x7B // F12

)

/* ---------------- Types ---------------- */
type KEYBDINPUT struct {
	WVk         uint16
	WScan       uint16
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

type INPUT struct {
	Type uint32
	_    uint32 // explicit padding for 64-bit alignment
	Ki   KEYBDINPUT
	_    [8]byte // tail padding to make union 32 bytes, because Ki should be MOUSEINPUT(32) not KEYBDINPUT(24 bytes) because the former's the biggest member of the union.
}

type KBDLLHOOKSTRUCT struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type WNDCLASSEX struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       windows.Handle
}

type WINDOWPLACEMENT struct {
	Length           uint32
	Flags            uint32
	ShowCmd          uint32
	PtMinPosition    POINT
	PtMaxPosition    POINT
	RcNormalPosition RECT
}

type POINT struct {
	X, Y int32
}

type RECT struct {
	Left, Top, Right, Bottom int32
}

type MSLLHOOKSTRUCT struct {
	Pt          POINT
	MouseData   uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type dragState struct {
	startPt   POINT
	startRect RECT
	manual    bool
}

type NOTIFYICONDATA struct {
	CbSize            uint32
	HWnd              windows.Handle
	UID               uint32
	UFlags            uint32
	UCallbackMessage  uint32
	HIcon             windows.Handle
	SzTip             [128]uint16
	DwState           uint32
	DwStateMask       uint32
	SzInfo            [256]uint16
	UTimeoutOrVersion uint32
	SzInfoTitle       [64]uint16
	DwInfoFlags       uint32
}

type MSG struct {
	HWnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      POINT
}

/* ---------------- Globals ---------------- */
var procSendInput = user32.NewProc("SendInput")

var (
	kbdHook windows.Handle

//	winDownSeen      atomic.Bool
//
// swallowNextWinUp atomic.Bool
)

var (
	winDown   atomic.Bool
	shiftDown atomic.Bool
	ctrlDown  atomic.Bool
	altDown   atomic.Bool

	winGestureUsed atomic.Bool //false initially
)

var (
	hHook       windows.Handle
	capturing   atomic.Bool
	targetWnd   windows.Handle
	currentDrag *dragState

	trayIcon NOTIFYICONDATA
)
var forceManual atomic.Bool // Default is false, if left like this.
var activateOnMove atomic.Bool

/* ---------------- Utilities ---------------- */

func injectShiftTapThenWinUp(whichWinUp uint16) {
	/*
		You are correctly not setting WVk when using KEYEVENTF_SCANCODE. Windows explicitly documents that when SCANCODE is set, WVk is ignored. Mixing them leads to inconsistent behavior on some builds.
	*/
	inputs := []INPUT{
		{
			Type: INPUT_KEYBOARD,
			Ki: KEYBDINPUT{
				//WVk: VK_SHIFT, // don't, it's wrong to use vk instead of scancodes for Shift
				//WVk: VK_E,
				//WScan:   0x12, // scancode for 'E',
				WScan:   0x36, // 0x2A is for Left Shift, and 0x36 is Right Shift scancode.
				DwFlags: KEYEVENTF_SCANCODE,
			},
		},
		{ // putting this after winUP below has same effect!
			Type: INPUT_KEYBOARD,
			Ki: KEYBDINPUT{
				//WVk:     VK_SHIFT,
				//WVk: VK_E,
				//DwFlags: KEYEVENTF_KEYUP,
				//WScan:   0x12, // 'E' key
				WScan:   0x36, // 0x2A is for Left Shift, and 0x36 is Right Shift scancode.
				DwFlags: KEYEVENTF_SCANCODE | KEYEVENTF_KEYUP,
			},
		},
		{
			Type: INPUT_KEYBOARD,
			Ki: KEYBDINPUT{
				WVk:     whichWinUp,
				DwFlags: KEYEVENTF_KEYUP,
			},
		},
	}

	procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)
}

func activateWindow(hwnd windows.Handle) {
	// Get target window thread
	var pid uint32
	targetTID, _, _ := procGetWindowThreadProcessId.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(&pid)),
	)

	// Get our thread
	selfTID := windows.GetCurrentThreadId()

	if targetTID != uintptr(selfTID) {
		// Attach threads
		procAttachThreadInput.Call(
			uintptr(selfTID),
			targetTID,
			1,
		)
	}

	// Activate
	//procSetForegroundWindow.Call(uintptr(hwnd))
	//XXX: doesn't work, well only in the first 1-2 seconds, then flashes taskbar button for that window instead!

	//temp-start:
	// 1️⃣ Bring to top WITHOUT activation
	procSetWindowPos.Call(
		uintptr(hwnd),
		HWND_TOP,
		0, 0, 0, 0,
		SWP_NOMOVE|SWP_NOSIZE|SWP_NOACTIVATE,
	)

	// 2️⃣ Attempt activation
	//procSetForegroundWindow.Call(uintptr(hwnd))

	// 3️⃣ Reinforce Z-order (still no activate)
	// procSetWindowPos.Call(
	// uintptr(hwnd),
	// HWND_TOP,
	// 0, 0, 0, 0,
	// SWP_NOMOVE|SWP_NOSIZE|SWP_NOACTIVATE,
	// )
	//temp-end

	if targetTID != uintptr(selfTID) {
		// Detach threads
		procAttachThreadInput.Call(
			uintptr(selfTID),
			targetTID,
			0,
		)
	}
}

func initDPIAwareness() {
	// Try modern API first (Win10 1607+)
	if procSetProcessDpiAwarenessContext.Find() == nil {
		r, _, _ := procSetProcessDpiAwarenessContext.Call(
			DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2,
		)
		if r != 0 {
			return // success
		}
	}

	// Fallback: Windows 8.1+
	if procSetProcessDpiAwareness.Find() == nil {
		procSetProcessDpiAwareness.Call(
			PROCESS_PER_MONITOR_DPI_AWARE,
		)
	}
}

// func winKeyDown() bool {
// l, _, _ := procGetAsyncKeyState.Call(VK_LWIN)
// r, _, _ := procGetAsyncKeyState.Call(VK_RWIN)
// return (l&0x8000 != 0) || (r&0x8000 != 0)
//}

func windowFromPoint(pt POINT) windows.Handle {
	ret, _, _ := procWindowFromPoint.Call(*(*uintptr)(unsafe.Pointer(&pt)))
	if ret == 0 {
		return 0
	}
	root, _, _ := procGetAncestor.Call(ret, GA_ROOTOWNER)
	return windows.Handle(root)
}

func getWindowPID(hwnd windows.Handle) uint32 {
	var pid uint32
	//windows.GetWindowThreadProcessId(hwnd, &pid)
	procGetWindowThreadProcessId.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(&pid)),
	)

	return pid
}

func isMaximized(hwnd windows.Handle) bool {
	//var wp windows.WINDOWPLACEMENT
	//wp.Length = uint32(unsafe.Sizeof(wp))
	//	windows.GetWindowPlacement(hwnd, &wp)
	var wp WINDOWPLACEMENT
	wp.Length = uint32(unsafe.Sizeof(wp))
	procGetWindowPlacement.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(&wp)),
	)

	return wp.ShowCmd == windows.SW_MAXIMIZE
}

/* ---------------- Integrity ---------------- */

func processIntegrityLevel(pid uint32) (uint32, error) {
	hProc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(hProc)

	var token windows.Token
	if err = windows.OpenProcessToken(hProc, windows.TOKEN_QUERY, &token); err != nil {
		return 0, err
	}
	defer token.Close()

	var needed uint32
	windows.GetTokenInformation(token, windows.TokenIntegrityLevel, nil, 0, &needed)

	buf := make([]byte, needed)
	if err = windows.GetTokenInformation(token, windows.TokenIntegrityLevel, &buf[0], needed, &needed); err != nil {
		return 0, err
	}

	sid := (*windows.SID)(unsafe.Pointer(&buf[8]))
	subAuthCount := *(*uint8)(unsafe.Pointer(uintptr(unsafe.Pointer(sid)) + 1))
	rid := *(*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(sid)) + uintptr(8+4*(subAuthCount-1))))
	return rid, nil
}

/* ---------------- Tray ---------------- */

func initTray(hwnd windows.Handle) {

	trayIcon.CbSize = uint32(unsafe.Sizeof(trayIcon))
	trayIcon.HWnd = hwnd
	trayIcon.UID = 1
	trayIcon.UFlags = NIF_TIP

	procLoadIcon := user32.NewProc("LoadIconW")

	const IDI_APPLICATION = 32512

	hIcon, _, _ := procLoadIcon.Call(0, IDI_APPLICATION)
	trayIcon.HIcon = windows.Handle(hIcon)
	trayIcon.UFlags |= 0x00000002 // NIF_ICON

	trayIcon.UCallbackMessage = WM_TRAY
	trayIcon.UFlags |= 0x00000001 // NIF_MESSAGE
	trayIcon.UTimeoutOrVersion = NOTIFYICON_VERSION_4

	copy(trayIcon.SzTip[:], windows.StringToUTF16("winbollocks"))
	procShellNotifyIcon.Call(NIM_SETVERSION, uintptr(unsafe.Pointer(&trayIcon)))
	procShellNotifyIcon.Call(NIM_ADD, uintptr(unsafe.Pointer(&trayIcon)))
}

func showTrayInfo(title, msg string) {
	logf("systray info: %s", msg)
	//the tray notification shows differently than a tooltip on win11 (didn't test it on anything else tho)
	//and I think you've to turn it on like(this only if you have Do Not Disturn 'on' already) System->Notifications->Set priority notifications, Add Apps(button) and pick winbollocks.exe
	// then you see it slide from the right, on top of systray, as a notifcation rectangle.
	//if you don't have Do not disturb on, it shows the same and you don't have to add it as priority notif. at all.
	// because it is already turned on in System->Notifications, Notifications from apps and other senders
	trayIcon.UFlags |= NIF_INFO
	trayIcon.UTimeoutOrVersion = 5000 //5sec, though Win11 ignores it and uses system accessibility settings)
	copy(trayIcon.SzInfoTitle[:], windows.StringToUTF16(title))
	copy(trayIcon.SzInfo[:], windows.StringToUTF16(msg))
	procShellNotifyIcon.Call(NIM_MODIFY, uintptr(unsafe.Pointer(&trayIcon)))
}

/* ---------------- Drag Logic ---------------- */

func startManualDrag(hwnd windows.Handle, pt POINT) {
	var r RECT
	//windows.GetWindowRect(hwnd, (*windows.RECT)(unsafe.Pointer(&r)))
	procGetWindowRect.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(&r)),
	)

	//	procSetCapture.Call(0) // capture to current thread
	procSetCapture.Call(uintptr(trayIcon.HWnd)) // Capture to your hidden window:
	// This ensures:
	//mouse capture is owned by your thread
	//capture is released cleanly
	//no weird input edge cases

	currentDrag = &dragState{startPt: pt, startRect: r, manual: true}
}

func startDrag(hwnd windows.Handle, pt POINT) (usedManual bool) {
	if isMaximized(hwnd) {
		//windows.ShowWindow(hwnd, windows.SW_RESTORE)
		procShowWindow.Call(uintptr(hwnd), SW_RESTORE)
		//TODO: should I re-maximize if it was maximized, after drag/move is done?
	}

	pid := getWindowPID(hwnd)
	targetIL, e1 := processIntegrityLevel(pid)
	//selfIL, e2 := processIntegrityLevel(uint32(windows.GetCurrentProcessId())) //bugged it said, it noticed.
	selfPID := uint32(os.Getpid())
	selfIL, e2 := processIntegrityLevel(selfPID)
	if e1 == nil && e2 == nil && targetIL > selfIL {
		showTrayInfo("winbollocks", "Cannot use native drag on elevated window")
		return
	}
	manual := forceManual.Load()
	usedManual = manual
	if manual {
		startManualDrag(hwnd, pt)
		return
	}

	// procSetForegroundWindow.Call(uintptr(hwnd))
	// //procReleaseCapture.Call()

	// //lParam := uintptr(uint32(pt.Y))<<16 | uintptr(uint32(pt.X)&0xFFFF)
	// //procSendMessage.Call(uintptr(hwnd), WM_NCLBUTTONDOWN, HTCAPTION, lParam)
	// procReleaseCapture.Call()
	// procSendMessage.Call(
	// uintptr(hwnd),
	// WM_SYSCOMMAND,
	// SC_MOVE|HTCAPTION,
	// 0,
	// )

	procPostMessage.Call(
		uintptr(trayIcon.HWnd),
		WM_START_NATIVE_DRAG,
		uintptr(hwnd),
		0,
	)

	currentDrag = &dragState{manual: false}
	return
}

func keyDown(vk uintptr) bool {
	state, _, _ := procGetAsyncKeyState.Call(vk)
	return state&0x8000 != 0
}

// the state of mod keys that my keyboard hook sees, now works.
func winOnlyIsDown() bool {
	return winDown.Load() &&
		!shiftDown.Load() &&
		!ctrlDown.Load() &&
		!altDown.Load()
}

// func winAndShiftOnlyAreDown() bool {
// 	return winDown.Load() &&
// 		shiftDown.Load() &&
// 		!ctrlDown.Load() &&
// 		!altDown.Load()
// }

// the current state of mod keys, works.
func winOnlyIsDown_viaState() bool {
	return (keyDown(VK_LWIN) || keyDown(VK_RWIN)) &&
		!keyDown(VK_SHIFT) &&
		!keyDown(VK_CONTROL) &&
		!keyDown(VK_MENU)
}

func hardResetIfDesynced() {
	if winDown.Load() {
		if !keyDown(VK_LWIN) && !keyDown(VK_RWIN) {
			hardReset()
		}
	}

	if capturing.Load() {
		// LMB not physically down anymore
		if !keyDown(VK_LBUTTON) {
			hardReset()
		}
	}
}

func hardReset() {
	winDown.Store(keyDown(VK_LWIN) || keyDown(VK_RWIN))
	shiftDown.Store(keyDown(VK_SHIFT))
	ctrlDown.Store(keyDown(VK_CONTROL))
	altDown.Store(keyDown(VK_MENU))

	winGestureUsed.Store(false)
	capturing.Store(false)
	currentDrag = nil
	targetWnd = 0

	procReleaseCapture.Call()
}

/* ---------------- Mouse Hook ---------------- */

/*
"High-input scenarios (gaming, rapid typing) may queue many events, but your callbacks still run one-by-one — the queue just grows temporarily. If you take too long in a callback (> ~1 second, controlled by LowLevelHooksTimeout registry key), Windows may drop or timeout subsequent calls, but it won't parallelize them." - Grok

"When a qualifying input event occurs (e.g., a mouse move or key press), the system detects installed low-level hooks and posts a special internal message (not a standard WM_ message) to the message queue of the thread that installed the hook. Your message loop then retrieves and dispatches this message, and during dispatch, Windows invokes your hook callback (mouseProc or keyboardProc)." - Grok
*/
func mouseProc(nCode int, wParam uintptr, lParam uintptr) uintptr {
	if nCode < 0 {
		ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		return ret
	}

	//nolint:govet
	info := (*MSLLHOOKSTRUCT)(unsafe.Pointer(lParam))

	switch wParam {
	case WM_LBUTTONDOWN: //LMB pressed.
		//if winKeyDown() {
		//if winDownSeen.Load() { //&& !swallowNextWinUp.Load() { {
		if winOnlyIsDown() { // only if winkey without any modifiers
			if !winGestureUsed.Load() { //wasn't set already
				winGestureUsed.Store(true) // we used at least once of our gestures
			}

			// we don't want to trigger our drag if shift/alt/ctrl was held before winkey, because it might have different meaning to other apps.
			// if !swallowNextWinUp.Load() {
			// swallowNextWinUp.Store(true)
			// }

			//if winKeyDown() && !capturing.Load() {
			//hwnd := windowFromPoint(info.Pt)
			//hwnd, _, _ := user32.NewProc("GetForegroundWindow").Call()

			//hwndRaw, _, _ := procGetForegroundWindow.Call()
			//hwnd := windows.Handle(hwndRaw)
			hwnd := windowFromPoint(info.Pt)
			if hwnd == 0 {
				break
			}
			//if hwnd != 0 {
			//capturing.Store(true)
			targetWnd = hwnd
			manual := startDrag(hwnd, info.Pt)
			if manual {

				if activateOnMove.Load() {
					activateWindow(hwnd)
					// AttachThreadInput(self, target, TRUE)
					// procSetForegroundWindow.Call(uintptr(hwnd))
					// AttachThreadInput(self, target, FALSE)
				}
				capturing.Store(true)
				return 1 // swallow LMB only for manual
			} else {
				return 0 // let native move receive input
			}
			//}
		}

	case WM_MOUSEMOVE:
		if capturing.Load() && currentDrag != nil && currentDrag.manual {
			dx := info.Pt.X - currentDrag.startPt.X
			dy := info.Pt.Y - currentDrag.startPt.Y
			r := currentDrag.startRect
			// windows.SetWindowPos(
			// targetWnd, 0,
			// r.Left+dx, r.Top+dy,
			// 0, 0,
			// windows.SWP_NOSIZE|windows.SWP_NOZORDER|windows.SWP_NOACTIVATE,
			// )
			procSetWindowPos.Call(
				uintptr(targetWnd),
				0,
				uintptr(r.Left+dx),
				uintptr(r.Top+dy),
				0,
				0,
				SWP_NOSIZE|SWP_NOZORDER|SWP_NOACTIVATE,
			)

			return 0 //0 = let thru
		}

	case WM_LBUTTONUP: //LMB released
		if capturing.Load() {
			capturing.Store(false)
			currentDrag = nil
			targetWnd = 0
			procReleaseCapture.Call()

			return 0 //0 is to let it thru (1 was to swallow)
		}

	case WM_MBUTTONDOWN: //MMB pressed
		if winDown.Load() && !ctrlDown.Load() && !altDown.Load() {
			//winDOWN and MMB pressed without ctrl/alt but maybe or not shiftDOWN too, it's a gesture of ours:
			if !winGestureUsed.Load() { //wasn't set already
				winGestureUsed.Store(true) // we used at least once of our gestures
			}
			if !shiftDown.Load() {
				// winkey_DOWN but no other modifiers(including shift) is down
				// and LMB is down, ofc, then we start move window gesture:

				hwnd := windowFromPoint(info.Pt)
				if hwnd != 0 {
					// Send to back, no activation
					// if you do this for a focused window then no amount of LMB will bring it back to front unless it loses focus first!
					procSetWindowPos.Call(
						uintptr(hwnd),
						HWND_BOTTOM,
						0, 0, 0, 0,
						SWP_NOMOVE|SWP_NOSIZE|SWP_NOACTIVATE,
					)
				}
			} else {
				// shift is down too, so winkey_DOWN and shiftDOWN and LMB are down
				// but no other modifiers like ctrl or alt are down
				// then we start the bring focused window to front gesture:

				//hwnd := windowFromPoint(info.Pt) // window under cursor
				hwnd, _, _ := procGetForegroundWindow.Call() // whichever the currently focused window is, wherever it is
				if hwnd != 0 {
					//logf("oh yea")
					// Bring to front, no activation, works only for the currently focused window which was sent to back before
					//has no effect because AI gave me the wrong constant value for HWND_TOP ! thanks chatgpt 5.2 !
					procSetWindowPos.Call(
						uintptr(hwnd),
						HWND_TOP,
						0, 0, 0, 0,
						SWP_NOMOVE|SWP_NOSIZE, //|SWP_NOACTIVATE,
					)

					// // Step 1: temporarily force topmost
					// procSetWindowPos.Call(
					// uintptr(hwnd),
					// HWND_TOPMOST,
					// 0, 0, 0, 0,
					// SWP_NOMOVE|SWP_NOSIZE,
					// )

					// // Step 2: immediately remove topmost
					// procSetWindowPos.Call(
					// uintptr(hwnd),
					// HWND_NOTOPMOST,
					// 0, 0, 0, 0,
					// SWP_NOMOVE|SWP_NOSIZE,
					// )

					// // Step 1: Activate desktop, ie. defocus current window.
					// desktop, _, _ := procGetDesktopWindow.Call()
					//logf("desktop hwnd = 0x%x", desktop)
					// procSetForegroundWindow.Call(desktop)

					// // Step 2: Activate the same window that was focused.
					// procSetForegroundWindow.Call(uintptr(hwnd))
				}
			} // else
			return 1 // swallow MMB
		} // the 'if' in MMB

	} //switch

	ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
	return ret
}

/* ---------------- Main ---------------- */

func createMessageWindow() windows.Handle {
	className, _ := windows.UTF16PtrFromString("winbollocksHidden")

	var wc WNDCLASSEX
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	wc.LpfnWndProc = wndProc
	wc.LpszClassName = className
	//wc.HInstance = windows.GetModuleHandle(nil)
	hinst, _, _ := procGetModuleHandle.Call(0)
	wc.HInstance = windows.Handle(hinst)

	procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))

	hwnd, _, _ := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		0,
		0,
		0, 0, 0, 0,
		0,
		0,
		uintptr(wc.HInstance),
		0,
	)

	return windows.Handle(hwnd)
}

func mustUTF16(s string) *uint16 {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		panic(err)
	}
	return p
}

var mouseCallback uintptr

var wndProc = windows.NewCallback(func(hwnd uintptr, msg uint32, wParam, lparam uintptr) uintptr {
	switch msg {
	case WM_START_NATIVE_DRAG:
		target := windows.Handle(wParam)
		if target != 0 {
			procSetForegroundWindow.Call(uintptr(target))
			procReleaseCapture.Call()
			procSendMessage.Call(
				uintptr(target),
				WM_SYSCOMMAND,
				SC_MOVE|HTCAPTION,
				0,
			)
		}
		return 0
	case WM_INJECT_SEQUENCE:
		//avoids injecting from the hook
		which := uint16(wParam)        // ie. uint16(vk))
		injectShiftTapThenWinUp(which) // it's correct casting, as per AI.
		return 0
	case WM_TRAY:
		if lparam == WM_RBUTTONUP {
			hMenu, _, _ := procCreatePopupMenu.Call()

			manualText, _ := windows.UTF16PtrFromString("Manual move (no focus)")
			exitText, _ := windows.UTF16PtrFromString("Exit")

			flags := MF_STRING
			if forceManual.Load() {
				flags |= MF_CHECKED
			}

			procAppendMenu.Call(hMenu, uintptr(flags), MENU_FORCE_MANUAL, uintptr(unsafe.Pointer(manualText)))

			var actFlags uintptr = MF_STRING // untyped constants can auto-convert, but not untyped vars(in the below call)
			if activateOnMove.Load() {
				actFlags |= MF_CHECKED
			}
			procAppendMenu.Call(hMenu, actFlags, MENU_ACTIVATE_MOVE,
				uintptr(unsafe.Pointer(mustUTF16("Activate window on move"))))

			procAppendMenu.Call(hMenu, MF_STRING, MENU_EXIT, uintptr(unsafe.Pointer(exitText)))

			var pt POINT
			procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

			procSetForegroundWindow.Call(hwnd)

			cmd, _, _ := procTrackPopupMenu.Call(
				hMenu,
				0x0100, // TPM_RETURNCMD
				uintptr(pt.X),
				uintptr(pt.Y),
				0,
				hwnd,
				0,
			)
			// Required by MSDN to dismiss menu correctly
			procSendMessage.Call(hwnd, 0, 0, 0) // Send WM_NULL

			switch cmd {
			case MENU_FORCE_MANUAL:
				forceManual.Store(!forceManual.Load())
			case MENU_ACTIVATE_MOVE:
				activateOnMove.Store(!activateOnMove.Load())

			case MENU_EXIT:
				procUnhookWindowsHookEx.Call(uintptr(hHook))
				exit(0)
			}

		}
		return 0

	case WM_CLOSE: //case 0x0010: // WM_CLOSE
		procUnhookWindowsHookEx.Call(uintptr(hHook))
		exit(0)
	}
	ret, _, _ := procDefWindowProc.Call(hwnd, uintptr(msg), wParam, lparam)
	return ret
})

func exit(code int) {

	//TODO: add the others?
	capturing.Store(false)
	procReleaseCapture.Call()
	os.Exit(code) // Hooks are removed after this. Your state must already be sane.
}

// var ctrlHandler = windows.NewCallback(func(ctrlType uint32) uintptr {
// switch ctrlType {
// case 0, 2: // CTRL_C_EVENT, CTRL_CLOSE_EVENT
// procUnhookWindowsHookEx.Call(uintptr(hHook))
// os.Exit(0)
// }
// return 1
// })

// var logFile *os.File

// func initLog() {
// var err error
// logFile, err = os.OpenFile(
// "debug.log",
// os.O_CREATE|os.O_WRONLY|os.O_APPEND,
// 0644,
// )
// if err != nil {
// return
// }
// }

// func logf(format string, args ...any) {
// if logFile == nil {
// initLog()
// if logFile == nil {
// return
// }
// }
// fmt.Fprintf(logFile, format+"\n", args...)
// logFile.Sync()
// }

var (
	logFile        *os.File
	hasConsole     bool
	consoleChecked bool
)

func detectConsole() {
	if consoleChecked {
		return
	}
	consoleChecked = true

	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	err := windows.GetConsoleMode(h, &mode)
	hasConsole = (err == nil)
}

func initLogFile() {
	if logFile != nil {
		return
	}
	f, err := os.OpenFile(
		"winbollocks_debug.log",
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0644,
	)
	if err == nil {
		logFile = f
	}
}

func logf(format string, args ...any) {
	detectConsole()

	if hasConsole {
		fmt.Fprintf(os.Stdout, format+"\n", args...)
		return
	}

	if logFile == nil {
		initLogFile()
		if logFile == nil {
			return
		}
	}
	//if logFile != nil {
	fmt.Fprintf(logFile, format+"\n", args...)
	logFile.Sync()
	//}
}

func injectLetterE() {
	// inputs := []INPUT{
	// {
	// Type: INPUT_KEYBOARD,
	// Ki: KEYBDINPUT{WVk: 'E'},
	// },
	// {
	// Type: INPUT_KEYBOARD,
	// Ki: KEYBDINPUT{WVk: 'E', DwFlags: KEYEVENTF_KEYUP},
	// },
	// }
	// procSendInput.Call(
	// uintptr(len(inputs)),
	// uintptr(unsafe.Pointer(&inputs[0])),
	// unsafe.Sizeof(inputs[0]),
	// )

	injectKeyTap('E')
}

func injectKeyTap(vk uint16) {
	inputs := []INPUT{
		{
			Type: INPUT_KEYBOARD,
			Ki: KEYBDINPUT{
				WVk: vk,
			},
		},
		{
			Type: INPUT_KEYBOARD,
			Ki: KEYBDINPUT{
				WVk:     vk,
				DwFlags: KEYEVENTF_KEYUP,
			},
		},
	}

	r, _, err := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)

	logf("SendInput ret=%d err=%v", r, err)
	//logf("sizeof(INPUT)=%d", unsafe.Sizeof(INPUT{}))
	//logf("sizeof(KEYBDINPUT)=%d", unsafe.Sizeof(KEYBDINPUT{}))

}

/*
5️⃣ Why this wiring is correct (sanity check)

Timeline:

# Win DOWN → allowed through

LMB DOWN → swallowed, swallowNextWinUp = true

# Mouse moves → manual drag

LMB UP → drag ends (no Win logic here)

# Win UP → swallowed once

Shell sees:

# Win state already UP

# No Win-UP message

Mouse gesture occurred
→ suppress Start, clear Win context

No stuck state.
No replay.
No surprises.

The corrected, accurate model (this matches your experiments)

Windows suppresses Start on Win_UP if either of these is true:

Mechanism A — “Something happened” (gesture path)

If any non-Win key transition occurs between Win_DOWN and Win_UP
→ Start is suppressed
→ That key does NOT need to be held at Win_UP

This is why:

Shift_DOWN → Shift_UP anywhere in the interval works

Win_DOWN → E_DOWN → E_UP → Win_UP works

# Your very first Shift experiment was already sufficient

You were correct from the start.

Mechanism B — “Win is not alone” (modifier state path)

If another modifier is currently down at Win_UP
→ Start is suppressed

This is why:

# Holding Shift while releasing Win also works

Releasing Shift before Win_UP makes Start appear again

This is a different check, evaluated at Win_UP time.
*/
/* pro:
For low-level hooks (WH_KEYBOARD_LL, WH_MOUSE_LL):

• Returning non-zero from your hook consumes the event (prevents it from reaching the system).
• Returning 0 allows it to continue.
• CallNextHookEx does not call the next hook directly. It is a dispatcher rendezvous / continuation point.
• The dispatcher runs all hooks, collects the first non-zero result (if any), and that value is what every deferred CallNextHookEx returns.
• Therefore:
– If you intend to swallow an event, do not call CallNextHookEx and return non-zero.
– If you intend to pass it through, either return 0 immediately or return the value from CallNextHookEx.
*/
/* correction:
Low-level hooks (WH_KEYBOARD_LL / WH_MOUSE_LL)

All hooks are called sequentially, regardless of return value.
There is no early abort of later hooks.
What a non-zero return does is:

• it tells Windows “this event is consumed”
• Windows will not deliver it to the target application
• but other hooks still run

ffs, AI, chatgpt 5.2 make up ur gdammn mind already, what is true and what isn't!!!

"No, your low-level hooks (WH_KEYBOARD_LL and WH_MOUSE_LL) will not be called in parallel in any realistic scenario that would require atomics for shared state." - Grok
*/
func keyboardProc(nCode int, wParam uintptr, lParam uintptr) uintptr {
	/*
			For low-level hooks:

		• Return non-zero → event is swallowed
		• Return zero → event continues

		Calling CallNextHookEx and returning its value means:
		“I am not making a decision; propagate whatever decision the rest of the chain makes.”

		If you want to consume the event, you must not call CallNextHookEx.
	*/
	if nCode < 0 {
		//If nCode is less than zero, the hook procedure must pass the message to CallNextHookEx without further processing.
		ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		return ret
	}

	k := (*KBDLLHOOKSTRUCT)(unsafe.Pointer(lParam))
	vk := k.VkCode
	// You see here even modifiers repeat just like letters, when held down!
	//logf("vk=%#x wParam=%#x flags=%#x", vk, wParam, k.Flags)

	/*SendInput is synchronous from your point of view, but injected events are queued back into the same input stream.
	  Windows marks injected events with LLKHF_INJECTED.
	  You explicitly ignore injected events:
	*/
	/*
		now is this mandatory
		Without this, your injected Win-UP would recursively trigger injectShiftTapThenWinUp again and you’d summon an infinite keyboard demon 👹
	*/
	if k.Flags&LLKHF_INJECTED != 0 {
		// This key event was generated by SendInput
		// Do NOT treat it as user input
		ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		return ret
	}

	// switch wParam {
	// case WM_KEYDOWN, WM_SYSKEYDOWN:
	// if vk == VK_LWIN || vk == VK_RWIN {
	// winDownSeen.Store(true)
	// swallowNextWinUp.Store(false) // safety valve
	// return 0                       // let Win DOWN through
	// }

	// case WM_KEYUP, WM_SYSKEYUP:
	// if vk == VK_LWIN || vk == VK_RWIN {
	// winDownSeen.Store(false)

	// if swallowNextWinUp.Load() {
	// // This is the entire trick
	// swallowNextWinUp.Store(false)
	// return 1 // swallow Win UP
	// }

	// return 0
	// }
	// }

	// // ---- Win DOWN: always let through ----
	// if (wParam == WM_KEYDOWN || wParam == WM_SYSKEYDOWN) &&
	// (vk == VK_LWIN || vk == VK_RWIN) {
	// //swallowNextWinUp.Store(false) // allowing this means it won't swallow next winkey up below.
	// winDownSeen.Store(true)
	// return 0
	// }

	// // ---- Win UP: conditionally swallow ----
	// if (wParam == WM_KEYUP || wParam == WM_SYSKEYUP) &&
	// (vk == VK_LWIN || vk == VK_RWIN) {

	// winDownSeen.Store(false)

	// //Letting Winkey UP(aka winkey released) through(ie. pass thru) → Start menu opens, Winkey clears
	// // Swallowing Winkey UP → Start menu suppressed, Winkey remains logically active, so pressing E runs explorer because winkey+E does it!
	// if swallowNextWinUp.Load() {
	// swallowNextWinUp.Store(false)
	// return 0 // 🔥 swallow BOTH KEYUP and SYSKEYUP
	// }
	// return 0
	// }

	// && vk == VK_F12
	// if (wParam == WM_KEYDOWN || wParam == WM_SYSKEYDOWN) && vk == VK_F {
	// // when you press f key it presses e key, temporary test.
	// injectLetterE()
	// return 1 // swallow F12
	// }

	// Key DOWN
	if wParam == WM_KEYDOWN || wParam == WM_SYSKEYDOWN {
		switch vk {

		case VK_LWIN, VK_RWIN:
			winDown.Store(true)
		//case VK_SHIFT: // Low-level keyboard hooks do NOT reliably(read: at all) deliver VK_SHIFT. VK_SHIFT is a virtual aggregation key used by higher-level APIs (like GetKeyState), not by the LL hook stream.
		case VK_LSHIFT, VK_RSHIFT:
			shiftDown.Store(true)
		case VK_LCONTROL, VK_RCONTROL:
			ctrlDown.Store(true)
		case VK_LMENU, VK_RMENU: // Alt
			altDown.Store(true)
		}
	}

	// Key UP
	if wParam == WM_KEYUP || wParam == WM_SYSKEYUP {
		switch vk {
		case VK_LWIN, VK_RWIN:
			/*
			   You now have this pipeline:
			   Detect real Win-UP
			   If no other modifiers are physically down:
			   Inject RShift down
			   Inject RShift up
			   Inject the swallowed Win-UP
			   Return 1 from the hook to suppress the original Win-UP
			   Ignore injected events via LLKHF_INJECTED
			   This satisfies all constraints:
			   Start menu suppressed
			   Win state restored
			   No stuck modifiers
			   No dependence on timers
			   No reliance on Explorer heuristics
			   Deterministic behavior
			*/

			//nolint:staticcheck,QF1011 // QF1011: could omit type bool from declaration; it will be inferred
			var checkBefore bool = winOnlyIsDown()
			winDown.Store(false)

			if checkBefore {
				//was winkey DOWN (ie. held/pressed) until now and no other modifiers like alt/shift/ctrl were too?!
				//then we can insert a shift DOWN then shift UP which would cause the winkey UP to not trigger Start menu popup!
				/*“Could another key sneak in during the injection?”

				In theory, yes.
				In practice, it’s vanishingly unlikely.

				Why:

				SendInput enqueues events atomically

				The time window is microseconds

				Even if it happens, worst case:
				the user pressed and held shift and now we cancelled it so he has to repress it to be seen as held again.

				*/

				if !winGestureUsed.Load() {
					// don't suppress winkey_UP if we didn't use it for our gestures, so this allows say winkeyDown then winkeyUp to open Start menu
					return 0 // pass thru the winkeyUP
				}
				//next ok, we gotta suppress winkeyUP, else Start menu will pop open which is annoying because we just used winkey+LMB drag for example, not pressed winkey then released it
				winGestureUsed.Store(false) // gesture ends with winkey_UP
				// • Injecting input from inside a WH_KEYBOARD_LL hook is documented as undefined.
				// great, it was correct and other do it before, but now it's bad!
				//injectShiftTapThenWinUp(uint16(vk)) // it's correct casting, as per AI.

				/* Using Right Shift is a defensible and, in this context, slightly superior choice. The edge cases you walked through are the right ones to think about, and you resolved them correctly:

				If the user is already holding any modifier (including RShift), you suppress injection entirely.

				Therefore you will never undo a user-held modifier.

				The only remaining risk window is the micro-interval between your modifier check and the injected tap, which is operationally negligible and unavoidable in any design that is not kernel-mode.

				That is as good as it gets in user-mode.
				*/
				procPostMessage.Call(
					uintptr(trayIcon.HWnd),
					WM_INJECT_SEQUENCE,
					uintptr(vk), // VK_LWIN or VK_RWIN,
					0,
				)

				return 1 // eat this winUP here(by returning non-zero!), else the injects are queued after it, so it opens Start right after this !
				/* well crap:
								Explorer / the shell ignores injected keyboard events when deciding whether to open Start.
								That’s why:

				Your injected Shift DOWN → Shift UP does nothing for Start suppression

				Even though the same physical sequence (real Shift) works perfectly

				Even though SendInput does update key state and does generate hooks

				Your intention

				At Win UP:

				Inject Shift DOWN

				Inject Shift UP

				Inject Win UP

				Eat the real Win UP

				You expect Explorer to think:

				“Ah, Win wasn’t alone — suppress Start.”
				*/
			}

		//case VK_SHIFT:
		case VK_LSHIFT, VK_RSHIFT:
			shiftDown.Store(false)
		case VK_LCONTROL, VK_RCONTROL:
			ctrlDown.Store(false)
		case VK_LMENU, VK_RMENU:
			altDown.Store(false)
		}
	}

	ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
	return ret
}

func assertStructSizes() {
	const (
		expectedINPUT      = 40
		expectedKEYBDINPUT = 24
	)

	if unsafe.Sizeof(INPUT{}) != expectedINPUT {
		logf("FATAL: INPUT size mismatch (%d)", unsafe.Sizeof(INPUT{}))
		panic("INPUT size mismatch: ABI layout is wrong") // exit code 2
	}

	if unsafe.Sizeof(KEYBDINPUT{}) != expectedKEYBDINPUT {
		logf("FATAL: KEYBDINPUT size mismatch (%d)", unsafe.Sizeof(KEYBDINPUT{}))
		panic("KEYBDINPUT size mismatch: ABI layout is wrong") // exit code 2
	}
}

func main() {
	runtime.LockOSThread() // first!
	assertStructSizes()
	//procSetConsoleCtrlHandler.Call(ctrlHandler, 1) // doesn't work(has no console) for: go build -mod=vendor -ldflags="-H=windowsgui" .

	forceManual.Store(true)

	initDPIAwareness() //If you call it after window creation, it does nothing.

	//cb := windows.NewCallback(mouseProc)
	mouseCallback = windows.NewCallback(mouseProc)
	h, _, err := procSetWindowsHookEx.Call(WH_MOUSE_LL, mouseCallback, 0, 0)
	if h == 0 {
		logf("Got error:", err) // has no console!
		//os.Exit(1)
		exit(1)
	} else {
		hHook = windows.Handle(h)
		defer procUnhookWindowsHookEx.Call(uintptr(hHook))
	}

	kbdCB := windows.NewCallback(keyboardProc)
	hk, _, err := procSetWindowsHookEx.Call(
		WH_KEYBOARD_LL,
		kbdCB,
		0,
		0,
	)
	if hk == 0 {
		logf("Got error:", err) // has no console!

		exit(2)
	} else {
		kbdHook = windows.Handle(hk)
		defer procUnhookWindowsHookEx.Call(uintptr(kbdHook))
	}

	hwnd := createMessageWindow()
	initTray(hwnd)

	var msg MSG
	for {
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
}
