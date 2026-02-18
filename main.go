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

// CRAPitkillsallfromrevive//nolint:revive,var-declaration
//
// XXX: yes this works too, here: //revive:disable:var-declaration
package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/pprof"

	"golang.org/x/sys/windows"

	"sync/atomic"
	"time"
	"unsafe"
)

/* ---------------- DLLs & Procs ---------------- */
var procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")

// var shellHook windows.Handle
var (
	// The Data Pipe (2048 is plenty for lag spikes)
	moveDataChan = make(chan WindowMoveData, 2048)

	// Modern Atomic tracking
	droppedMoveEvents           atomic.Uint32
	droppedLogEvents            atomic.Uint32
	maxChannelFillForMoveEvents atomic.Int64 // To track how "full" it got
	maxChannelFillForLogEvents  atomic.Int64 // To track how "full" it got

	// To tell the hook where to send the "Doorbell"
	mainThreadId uint32

	procPostThreadMessage = user32.NewProc("PostThreadMessageW")
)

func init() {
	maxChannelFillForMoveEvents.Store(1) // avoid the first message: New Channel Peak: 1 events queued (Dropped: 0)
}

// Define the struct for GetGUIThreadInfo
type GUITHREADINFO struct {
	CbSize        uint32
	Flags         uint32
	HwndActive    windows.Handle
	HwndFocus     windows.Handle
	HwndCapture   windows.Handle
	HwndMenuOwner windows.Handle
	HwndMoveSize  windows.Handle
	HwndCaret     windows.Handle
	RcCaret       windows.Rect
}

var procGetGUIThreadInfo = user32.NewProc("GetGUIThreadInfo")

var (
	procSetWinEventHook = user32.NewProc("SetWinEventHook")
	procUnhookWinEvent  = user32.NewProc("UnhookWinEvent")

	winEventHook     windows.Handle
	winEventCallback = windows.NewCallback(winEventProc)
)

var (
	moveCounter     int       // how many move events we saw since last log
	lastRateLogTime time.Time // when we last printed the rate
	rateLogInterval = 1 * time.Second
)
var actualPostCounter int

// Globals
var (
	lastMovePostedTime       time.Time
	lastPostedX, lastPostedY int32
)

// XXX: yes this works too, here: //revive:disable:var-naming
const MIN_MOVE_INTERVAL = 33 * time.Millisecond // ~30 fps – very pleasant

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
	procGetCapture          = user32.NewProc("GetCapture")
	procReleaseCapture      = user32.NewProc("ReleaseCapture") // Releases mouse capture if any window has it
	procSendMessage         = user32.NewProc("SendMessageW")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")

	procPeekMessage = user32.NewProc("PeekMessageW")

	procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")
	procDestroyWindow   = user32.NewProc("DestroyWindow")

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

	//procGetDesktopWindow = user32.NewProc("GetDesktopWindow")
	procGetLastError = kernel32.NewProc("GetLastError")

	procSendInput = user32.NewProc("SendInput")
	procLoadIcon  = user32.NewProc("LoadIconW")
)

/* ---------------- Constants ---------------- */

const (
	PM_NOREMOVE = 0x0000
	PM_REMOVE   = 0x0001
	PM_NOYIELD  = 0x0002
)

const GUI_INMOVESIZE = 0x00000002

const (
	MOUSEEVENTF_LEFTDOWN   = 0x0002
	MOUSEEVENTF_LEFTUP     = 0x0004
	MOUSEEVENTF_RIGHTDOWN  = 0x0008
	MOUSEEVENTF_RIGHTUP    = 0x0010
	MOUSEEVENTF_MIDDLEDOWN = 0x0020
	MOUSEEVENTF_MIDDLEUP   = 0x0040
)

const (

	// Low-level keyboard hook flag
	LLKHF_INJECTED = 0x00000010
	// mouse:
	LLMHF_INJECTED = 0x00000001
)

const (
	NOTIFYICON_VERSION_4 = 4
	NIM_SETVERSION       = 0x00000004
)

const (
	WM_QUERYENDSESSION = 0x0011
	WM_ENDSESSION      = 0x0016
)

const (
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = (HANDLE)-4
	DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = ^uintptr(3)

	// PROCESS_PER_MONITOR_DPI_AWARE = 2
	PROCESS_PER_MONITOR_DPI_AWARE = 2
)

const (
	WM_MBUTTONDOWN = 0x0207
	HWND_BOTTOM    = windows.Handle(uintptr(1)) // good
	//HWND_TOP       = ^uintptr(1) // (HWND)-1  bad AI
	HWND_TOP = windows.Handle(uintptr(0)) // good

	HWND_TOPMOST   = ^uintptr(0) // (HWND)-1
	HWND_NOTOPMOST = ^uintptr(1) // (HWND)-2
	//HWND_TOP       = ^uintptr(2) // (HWND)-3 bad
	//HWND_BOTTOM    = ^uintptr(3) // (HWND)-4 bad, gg AI

)

const (
	WH_MOUSE_LL = 14

	WM_MOUSEMOVE   = 0x0200
	WM_LBUTTONDOWN = 0x0201
	WM_LBUTTONUP   = 0x0202
	WM_RBUTTONUP   = 0x0205 // even winxp would have this
	WM_CONTEXTMENU = 0x007B // winxp won't have this tho

	WM_NCLBUTTONDOWN = 0x00A1

	HTCAPTION = 2

	GA_ROOT      = 2
	GA_ROOTOWNER = 3

	NIM_ADD    = 0x00000000
	NIM_MODIFY = 0x00000001
	NIM_DELETE = 0x00000002

	NIF_MESSAGE = 0x00000001
	NIF_ICON    = 0x00000002
	NIF_TIP     = 0x00000004
	NIF_INFO    = 0x00000010
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
	WM_USER  = 0x0400
	WM_CLOSE = 0x0010
)

const (
	WM_START_NATIVE_DRAG = WM_USER + 1
	WM_MYTRAY            = WM_USER + 2
	//WM_WAKE_UP           = WM_USER + 3
	WM_INJECT_SEQUENCE             = WM_USER + 100
	WM_FOCUS_TARGET_WINDOW_SOMEHOW = WM_USER + 101
	WM_EXIT_VIA_CTRL_C             = WM_USER + 150
	WM_DO_SETWINDOWPOS             = WM_USER + 200 // arbitrary, just unique
)
const (
	MENU_EXIT              = 1
	MENU_FORCE_MANUAL      = 2
	MENU_ACTIVATE_MOVE     = 3
	MENU_RATELIMIT_MOVES   = 4
	MENU_LOG_RATE_OF_MOVES = 5

	MF_STRING = 0x0000

	MF_GRAYED   = 0x00000001
	MF_DISABLED = 0x00000002
	MF_CHECKED  = 0x00000008
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
	INPUT_MOUSE        = 0
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
	//left winkey
	VK_LWIN = 0x5B
	//right winkey
	VK_RWIN = 0x5C

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

type WindowMoveData struct {
	Hwnd        windows.Handle // Target window
	X           int32          // New X (full 32-bit)
	Y           int32          // New Y
	InsertAfter windows.Handle // ← this one: HWND_TOP, HWND_BOTTOM, etc.
	Flags       uint32         // Optional: extra SWP_ flags
	// Add more if needed (e.g., Width, Height for resize)
}

type KEYBDINPUT struct {
	WVk         uint16
	WScan       uint16
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

type MOUSEINPUT struct {
	Dx          int32
	Dy          int32
	MouseData   uint32
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

var (
	// winDown   atomic.Bool
	// shiftDown atomic.Bool
	// ctrlDown  atomic.Bool
	// altDown   atomic.Bool

	winGestureUsed bool = false //false initially
)

var (
	mouseHook windows.Handle
	kbdHook   windows.Handle

	//"the app is effectively single-threaded for these vars (pinned thread, serialized hooks/message loop), so no concurrency risks."- grok expert
	capturing   bool
	targetWnd   windows.Handle
	currentDrag *dragState

	trayIcon NOTIFYICONDATA
)
var forceManual bool // Default is false, if left like this.
var activateOnManualMoveOnly bool
var ratelimitOnMove bool
var shouldLogDragRate bool // but only when ratelimitOnMove is true

/* ---------------- Utilities ---------------- */

func guiThreadInMoveSize(tid uint32) (bool, error) {
	var info GUITHREADINFO
	info.CbSize = uint32(unsafe.Sizeof(info))

	r1, _, err := procGetGUIThreadInfo.Call(
		uintptr(tid),
		uintptr(unsafe.Pointer(&info)),
	)
	if r1 == 0 {
		return false, err
	}

	return (info.Flags & GUI_INMOVESIZE) != 0, nil
}

func waitForMoveLoop(tid uint32, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		inMove, err := guiThreadInMoveSize(tid)
		if err == nil && inMove {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

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

	ret, _, err := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)
	if ret == 0 {
		logf("SendInput for injectShiftTapThenWinUp failed: %v", err)
	}
}
func injectLMBClick() {
	inputs := []INPUT{
		{
			Type: INPUT_MOUSE,
			Ki:   KEYBDINPUT{}, // union placeholder
		},
		{
			Type: INPUT_MOUSE,
			Ki:   KEYBDINPUT{}, // union placeholder
		},
	}

	// Fill the union as MOUSEINPUT
	(*MOUSEINPUT)(unsafe.Pointer(&inputs[0].Ki)).DwFlags = MOUSEEVENTF_LEFTDOWN
	(*MOUSEINPUT)(unsafe.Pointer(&inputs[1].Ki)).DwFlags = MOUSEEVENTF_LEFTUP

	//Your inject (MOUSEEVENTF_LEFTDOWN/UP): Defaults relative (Dx/Dy=0 = no move, click at current cursor).

	ret, _, err := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)

	if ret == 0 {
		logf("SendInput mouse click failed: %v", err)
	} else {
		//TODO: remove, temp.
		logf("Used LMB click to focus, caveat: target window got a LMB click at the point where you started the window move so it could've clicked an UI button!")
	}
}

func injectLMBDown() {
	inputs := []INPUT{
		{
			Type: INPUT_MOUSE,
			Ki:   KEYBDINPUT{}, // union placeholder
		},
		// {
		// 	Type: INPUT_MOUSE,
		// 	Ki:   KEYBDINPUT{}, // union placeholder
		// },
	}

	// Fill the union as MOUSEINPUT
	(*MOUSEINPUT)(unsafe.Pointer(&inputs[0].Ki)).DwFlags = MOUSEEVENTF_LEFTDOWN
	//(*MOUSEINPUT)(unsafe.Pointer(&inputs[1].Ki)).DwFlags = MOUSEEVENTF_LEFTUP

	//Your inject (MOUSEEVENTF_LEFTDOWN/UP): Defaults relative (Dx/Dy=0 = no move, click at current cursor).

	//SendInput is synchronous—blocks until inputs queued/processed by system. In WH_MOUSE_LL (global, synchronous chain), this blocks all mouse input until done.
	//SendInput is synchronous — blocks caller until inputs queued to system queue (not processed).
	ret, _, err := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)

	if ret == 0 {
		logf("SendInput mouse click failed: %v", err)
	} else {
		//TODO: remove, temp.
		logf("Injected LMB down, ret=%d err=%v", ret, err)
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
	root, _, _ := procGetAncestor.Call(ret, GA_ROOT)
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
	var wp WINDOWPLACEMENT
	wp.Length = uint32(unsafe.Sizeof(wp))
	//"GetWindowPlacement is a synchronous query into USER32, but it does not send a message to the target window. It reads window state maintained by the window manager (the same data used by the shell for task switching)." -chatgpt5.2
	// so GetWindowPlacement does not block on a hung window.
	r, _, _ := procGetWindowPlacement.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(&wp)),
	)
	if r == 0 {
		return false
	}
	return wp.ShowCmd == windows.SW_MAXIMIZE
}

/* ---------------- Integrity ---------------- */

func processIntegrityLevel(pid uint32) (uint32, error) { // grok 4.1 fast thinking, made, 4th try
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

	// Debug: log buffer size (should be ~28-40 bytes)
	//logf("Integrity buf len=%d for PID %d", len(buf), pid)

	// TOKEN_MANDATORY_LABEL header is 16 bytes on 64-bit (pointer + attributes + padding)
	const headerSize = 16
	if len(buf) < headerSize+8 { // + min SID header
		return 0, fmt.Errorf("buffer too small: %d", len(buf))
	}

	// SID starts after header
	//sidBase := uintptr(unsafe.Pointer(&buf[headerSize]))

	// SID fixed header: Revision (1) + SubAuthorityCount (1) + IdentifierAuthority (6) = offset 8 for SubAuthority array
	//subCountPtr := (*uint8)(unsafe.Pointer(sidBase + 1)) // SubAuthorityCount at offset 1
	//subCountPtr := (*uint8)(unsafe.Pointer(uintptr(unsafe.Pointer(&buf[headerSize])) + 1))
	subCountPtr := (*uint8)(unsafe.Add(unsafe.Pointer(&buf[headerSize]), 1))
	subCount := *subCountPtr
	if subCount == 0 {
		return 0, fmt.Errorf("invalid subauthority count")
	}

	// SubAuthority array starts at offset 8 from SID base
	//subAuthBase := sidBase + 8

	// RID is the last SubAuthority
	//ridOffset := uintptr(subCount-1) * 4
	//ridPtr := (*uint32)(unsafe.Pointer(subAuthBase + ridOffset))
	//ridPtr := (*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(&buf[headerSize])) + 8 + (uintptr(subCount-1) * 4))) //this is fine
	offset := uintptr(8 + (subCount-1)*4)
	ridPtr := (*uint32)(unsafe.Add(unsafe.Pointer(&buf[headerSize]), offset))
	rid := *ridPtr

	return rid, nil
}

/* ---------------- Tray ---------------- */

func initTray(hwnd windows.Handle) {
	trayIcon.CbSize = uint32(unsafe.Sizeof(trayIcon))
	trayIcon.HWnd = hwnd
	trayIcon.UID = 1
	trayIcon.UFlags = NIF_TIP | NIF_ICON | NIF_MESSAGE

	const IDI_APPLICATION = 32512

	hIcon, _, _ := procLoadIcon.Call(0, IDI_APPLICATION)
	trayIcon.HIcon = windows.Handle(hIcon)
	trayIcon.UCallbackMessage = WM_MYTRAY
	trayIcon.UTimeoutOrVersion = NOTIFYICON_VERSION_4

	copy(trayIcon.SzTip[:], windows.StringToUTF16("winbollocks")) //TODO: make const

	//1
	ret1, _, err1 := procShellNotifyIcon.Call(NIM_ADD, uintptr(unsafe.Pointer(&trayIcon)))
	if ret1 == 0 {
		logf("Failed to add tray icon (real error): %v (code %d)", err1, err1)
		// You could exitf or fallback here, but for now just log
	}

	//2, this must happen after NIM_ADD ! (bad chatgpt which suggested it before NIM_ADD)
	ret2, _, err2 := procShellNotifyIcon.Call(NIM_SETVERSION, uintptr(unsafe.Pointer(&trayIcon)))
	if ret2 == 0 {
		logf("NIM_SETVERSION for tray icon failed(are you on pre Windows Vista 2007?): %v (code %d)", err2, err2)
		// You could exitf or fallback here, but for now just log
	}

}

func cleanupTray() {
	if trayIcon.HWnd == 0 {
		// Never initialized or window creation failed — nothing to clean
		return
	}
	// Optional: Destroy the message-only window first (good hygiene)

	ret, _, err := procDestroyWindow.Call(uintptr(trayIcon.HWnd))
	if ret == 0 {
		logf("DestroyWindow failed: %v (probably already destroyed or invalid)", err)
	}

	// Use the same trayIcon struct from initTray
	trayIcon.UFlags = 0 // NIM_DELETE ignores most fields, but set to be safe
	ret, _, err = procShellNotifyIcon.Call(NIM_DELETE, uintptr(unsafe.Pointer(&trayIcon)))
	//ret is non-zero (success), but err can still be set
	if ret == 0 {
		logf("Failed to delete tray icon: %v", err) // optional, for debug
	}
	// Optional: zero out the struct to avoid reuse confusion
	trayIcon = NOTIFYICONDATA{}
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
	//logf("startDrag")
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
	if e1 != nil {
		logf("e1: %v", e1)
	}
	if e2 != nil {
		logf("e2: %v", e2)
	}
	//logf("Target PID: %d, targetIL: %d, selfIL: %d", pid, targetIL, selfIL)

	usedManual = forceManual
	if forceManual {
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

	currentDrag = &dragState{manual: false}

	if true {
		logf("Doing native drag via wndProc")
		procPostMessage.Call(
			uintptr(trayIcon.HWnd),
			WM_START_NATIVE_DRAG,
			uintptr(hwnd),
			0,
		)
	} else {
		logf("Doing native drag via mouseProc aka in hook, bad as it doesn't work because the LMB down is queued after these so it never does anything.")
		target := hwnd
		if target != 0 {
			logf("got target %d", target)
			// get target thread id
			// Fix: Capture thread ID from return value, good grok (bad chatgpt)
			var targetProcessId uint32
			r1, _, err := procGetWindowThreadProcessId.Call(uintptr(target), uintptr(unsafe.Pointer(&targetProcessId)))
			if r1 == 0 {
				logf("GetWindowThreadProcessId failed: %v", err)
				return
			}
			targetThreadId := uint32(r1) // This is the actual thread ID

			// current thread id
			curTid := windows.GetCurrentThreadId()

			// attach input so SetForegroundWindow is allowed
			// Attach (check success)
			attachRet, _, attachErr := procAttachThreadInput.Call(uintptr(curTid), uintptr(targetThreadId), uintptr(1)) // TRUE
			if attachRet == 0 {
				logf("AttachThreadInput failed: %v", attachErr)
				return
			}

			// Get current cursor pos for lParam (screen coords, fresh)
			var cursorPt POINT
			getRet, _, getErr := procGetCursorPos.Call(uintptr(unsafe.Pointer(&cursorPt)))
			if getRet == 0 {
				logf("GetCursorPos failed: %v", getErr)
				// Fallback to your +5 method or bail
			}
			//lParamNative := uintptr(uint32(cursorPt.Y)<<16 | uint32(cursorPt.X)) // MAKEWPARAM equivalent
			var lParamNative uintptr = makeLParam(cursorPt.X, cursorPt.Y)
			//var lParamNative uintptr = 0

			procSetForegroundWindow.Call(uintptr(target)) //XXX: focus is required for WM_NCLBUTTONDOWN to work!
			//procReleaseCapture.Call()
			// detach input even if SetForegroundWindow failed
			procAttachThreadInput.Call(uintptr(curTid), uintptr(targetThreadId), uintptr(0)) // FALSE

			//procSendMessage.Call(uintptr(target), WM_NCLBUTTONDOWN, HTCAPTION, lParamNative) // synchronous
			// ret, _, err = procSendMessage.Call(
			// 	uintptr(target),
			// 	WM_NCLBUTTONDOWN,
			// 	HTCAPTION,
			// 	lParamNative,
			// )
			// logf("WM_NCLBUTTONDOWN ret=%d err=%v", ret, err)

			injectLMBDown() // this blocks here and Windows times out the hook after 500ms or so.
			/*
				You confirmed correctly: AttachThreadInput is only needed for SetForegroundWindow (to bypass focus restrictions),
				not for PostMessage / SendMessage itself. Cross-thread PostMessage works without attachment (messages are queued to the target's thread safely).
				SendMessage requires the target thread to be pumping messages, but no attachment needed unless focus/input state is involved.
				- grok fast
			*/
			// Use PostMessage (async) to avoid sync issues
			ret, _, err := procPostMessage.Call(
				uintptr(target),
				WM_NCLBUTTONDOWN,
				HTCAPTION,
				lParamNative,
			)
			logf("PostMessage WM_NCLBUTTONDOWN ret=%d err=%v", ret, err)

			// attempt to bring the target to the foreground and start the native move

			// procSetForegroundWindow.Call(uintptr(target))
			// procReleaseCapture.Call()
			// ret, _, err = procSendMessage.Call(
			// 	uintptr(target),
			// 	WM_SYSCOMMAND,
			// 	SC_MOVE|HTCAPTION,
			// 	0,
			// )
			// logf("WM_SYSCOMMAND ret=%d err=%v", ret, err)
		}
	} // else

	return
}

func keyDown(vk uintptr) bool {
	state, _, _ := procGetAsyncKeyState.Call(vk)
	return state&0x8000 != 0
}

// the state of mod keys that my keyboard hook sees, now works.
// func winOnlyIsDown() bool {
// 	return winDown.Load() &&
// 		!shiftDown.Load() &&
// 		!ctrlDown.Load() &&
// 		!altDown.Load()
// }

// func winAndShiftOnlyAreDown() bool {
// 	return winDown.Load() &&
// 		shiftDown.Load() &&
// 		!ctrlDown.Load() &&
// 		!altDown.Load()
// }

// the current state of mod keys, works.
// func winOnlyIsDown() bool {
// 	return (keyDown(VK_LWIN) || keyDown(VK_RWIN)) &&
// 		!keyDown(VK_SHIFT) &&
// 		!keyDown(VK_CONTROL) &&
// 		!keyDown(VK_MENU)
// }

func hardResetIfDesynced(winDownInHook bool) {
	// if winDown.Load() {
	// 	if !keyDown(VK_LWIN) && !keyDown(VK_RWIN) {
	// 		hardReset()
	// 	}
	// }

	if capturing {
		// LMB not physically down anymore
		if !keyDown(VK_LBUTTON) && !winDownInHook {
			logf("Desync detected: Capture/LMB state reset")
			hardReset()
		}
	}
}

func hardReset() {
	// winDown.Store(keyDown(VK_LWIN) || keyDown(VK_RWIN))
	// shiftDown.Store(keyDown(VK_SHIFT))
	// ctrlDown.Store(keyDown(VK_CONTROL))
	// altDown.Store(keyDown(VK_MENU))

	winGestureUsed = false
	capturing = false
	currentDrag = nil
	targetWnd = 0

	procReleaseCapture.Call()
}

func isWindowForeground(hwnd windows.Handle) bool {
	if hwnd == 0 {
		logf("!! attempted to check the focus of a windows with handle 0")
		return false
	}
	fg, _, _ := procGetForegroundWindow.Call()
	return windows.Handle(fg) == hwnd
}

// aka focus(activate) the window, works by attaching to target window's thread, so Windows won't do its focus stealing prevention thing!
// also, this way I don't have to inject LMB down then LMB up aka a LMB click event to focus it, risking pressing Exit button on total commander for example.
// however, TODO: now i do have to make sure hooks are running on a separate thread (than main msg. loop) because this is potentially blocking and can deadlock, depending on target app.
func forceForeground(target windows.Handle) bool {
	if target == 0 {
		logf("!! attempted to focus a windows with handle 0")
		return false
	}
	if isWindowForeground(target) {
		return true // Already good, no-op
	}

	var targetProcessId uint32
	r1, _, err := procGetWindowThreadProcessId.Call(uintptr(target), uintptr(unsafe.Pointer(&targetProcessId)))
	if r1 == 0 {
		logf("GetWindowThreadProcessId failed: %v", err)
		return false
	}
	targetThreadId := uint32(r1)

	curTid := windows.GetCurrentThreadId()
	attachRet, _, attachErr := procAttachThreadInput.Call(uintptr(curTid), uintptr(targetThreadId), uintptr(1))
	if attachRet == 0 {
		logf("AttachThreadInput failed: %v", attachErr)
		return false
	}

	fgRet, _, fgErr := procSetForegroundWindow.Call(uintptr(target))
	if fgRet != 1 {
		// ie. not "SetForegroundWindow ret=1 err=The operation completed successfully."
		//XXX: you get ret=0 with "err=The operation completed successfully." when Start menu was already open
		logf("SetForegroundWindow ret=%d err=%v", fgRet, fgErr)
	}

	procAttachThreadInput.Call(uintptr(curTid), uintptr(targetThreadId), uintptr(0)) // Detach always

	return fgRet != 0
}

func logLMBState(prefix string) {
	state, _, _ := procGetAsyncKeyState.Call(VK_LBUTTON)
	if state&0x8000 != 0 {
		logf("%s: LMB is DOWN (0x%04X)", prefix, state)
	} else {
		logf("%s: LMB is UP   (0x%04X)", prefix, state)
	}
}

/* ---------------- Mouse Hook ---------------- */

/*
"High-input scenarios (gaming, rapid typing) may queue many events, but your callbacks still run one-by-one — the queue just grows temporarily. If you take too long in a callback (> ~1 second, controlled by LowLevelHooksTimeout registry key), Windows may drop or timeout subsequent calls, but it won't parallelize them." - Grok

"When a qualifying input event occurs (e.g., a mouse move or key press), the system detects installed low-level hooks and posts a special internal message (not a standard WM_ message) to the message queue of the thread that installed the hook. Your message loop then retrieves and dispatches this message, and during dispatch, Windows invokes your hook callback (mouseProc or keyboardProc)." - Grok
*/
func mouseProc(nCode int, wParam uintptr, lParam uintptr) uintptr {
	// Standard Win32 Hook practice: If nCode < 0, we must pass it
	// to the next hook immediately and stay out of the way.
	if nCode < 0 {
		ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		return ret
	}

	info := (*MSLLHOOKSTRUCT)(unsafe.Pointer(lParam)) // XXX: warns without the .\.vscode\settings.json the unsafeptr false part.
	// Trick the linter: convert to pointer via an interface or a helper
	// that doesn't trigger the "unsafeptr" heuristic.
	// var p interface{} = lParam
	// //nolint:govet,unsafeptr
	// info := (*MSLLHOOKSTRUCT)(unsafe.Pointer(p.(uintptr)))

	if info.Flags&LLMHF_INJECTED != 0 {
		// This mouse event was generated by SendInput
		// Do NOT treat it as user input
		ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		return ret
	}

	//logf("in mouseProc")//spammy on mouse movements!
	//hardResetIfDesynced()

	switch wParam {
	case WM_LBUTTONDOWN: //LMB pressed.
		var winDown bool = keyDown(VK_LWIN) || keyDown(VK_RWIN)
		var shiftDown bool = keyDown(VK_SHIFT)
		var ctrlDown bool = keyDown(VK_CONTROL)
		var altDown bool = keyDown(VK_MENU)
		//if winKeyDown() {
		//if winDownSeen.Load() { //&& !swallowNextWinUp.Load() { {
		if winDown && !shiftDown && !altDown && !ctrlDown { // only if winkey without any modifiers
			if !winGestureUsed { //wasn't set already
				winGestureUsed = true // we used at least once of our gestures
			}
			if capturing {
				//FIXME: happens when winkey+LMB then winkey+L to lock, release both, unlock, move mouse (still drag/moves window), hold winkey and press LMB and u're here.
				logf("already capturing")
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
				logf("Invalid window, window-move gesture skipped but LMB eaten and start menu will still be prevented(unless you LMB on a higher integrity eg. admin window before you release winkey)")
				return 1 // swallow LMB
			}
			//if hwnd != 0 {
			//capturing.Store(true)
			targetWnd = hwnd
			manual := startDrag(hwnd, info.Pt)
			if manual {

				// if activateOnMove.Load() {
				// 	activateWindow(hwnd)
				// 	// AttachThreadInput(self, target, TRUE)
				// 	// procSetForegroundWindow.Call(uintptr(hwnd))
				// 	// AttachThreadInput(self, target, FALSE)
				// }
				capturing = true
				if activateOnManualMoveOnly && !isWindowForeground(targetWnd) {

					procPostMessage.Call(
						uintptr(trayIcon.HWnd),
						WM_FOCUS_TARGET_WINDOW_SOMEHOW,
						0, // no args to that function
						0,
					)
					//}
				}
				return 1 // swallow LMB only for manual
				//} else {
				//	return 0 // let native move receive input
			} else {
				logLMBState("before swallowing it")
				return 1 // FIXME: temp swallow LMB for non-manual too. Because i need to insert a LMB down before WM_NCLBUTTONDOWN else WM_NCLBUTTONDOWN will be first!
			}
			//XXX: else, let it fall thru(let native move receive input - required!) so CallNextHookEx is called too
			//}
		}

	case WM_MOUSEMOVE:
		if capturing && currentDrag != nil && currentDrag.manual {
			// At the very beginning of the drag/move logic (e.g., right after checking if dragging is active)
			var now time.Time
			if ratelimitOnMove {
				now = time.Now()
				// Count every potential move (even if we skip due to debounce)
				moveCounter++
				//logf("%d", moveCounter) //FIXME: temp, remove
			}

			dx := info.Pt.X - currentDrag.startPt.X
			dy := info.Pt.Y - currentDrag.startPt.Y
			r := currentDrag.startRect
			// windows.SetWindowPos(
			// targetWnd, 0,
			// r.Left+dx, r.Top+dy,
			// 0, 0,
			// windows.SWP_NOSIZE|windows.SWP_NOZORDER|windows.SWP_NOACTIVATE,
			// )
			//XXX: "Calling SetWindowPos from inside a WH_MOUSE_LL or WH_KEYBOARD_LL hook is strongly discouraged for the same reason as SendMessage:" - so I should postMessage here and handle this in my message loop
			newX := r.Left + dx
			newY := r.Top + dy
			// procSetWindowPos.Call(
			// 	uintptr(targetWnd),
			// 	0,
			// 	uintptr(r.Left+dx),
			// 	uintptr(r.Top+dy),
			// 	0,
			// 	0,
			// 	SWP_NOSIZE|SWP_NOZORDER|SWP_NOACTIVATE,
			// )

			//THISIGNORESALLfrom_staticcheck//nolint:staticcheck,QF1011: could omit type bool from declaration; it will be inferred from the right-hand side (staticcheck)go-golangci-lint-v2
			var willPostMessage bool = !ratelimitOnMove || (newX != lastPostedX || newY != lastPostedY) && now.Sub(lastMovePostedTime) >= MIN_MOVE_INTERVAL
			// Optional: Also count only the ones that would have posted (uncomment if you want both stats)
			if ratelimitOnMove && shouldLogDragRate && willPostMessage {
				actualPostCounter++
			}

			// Periodic logging every ~1 second
			if ratelimitOnMove && shouldLogDragRate && now.Sub(lastRateLogTime) >= rateLogInterval {
				var secondsElapsed float64 = now.Sub(lastRateLogTime).Seconds()
				if secondsElapsed > 0 {
					rate := float64(moveCounter) / secondsElapsed
					// logf("Drag move rate: %d events in %.2fs → %.1f moves/sec",
					// 	moveCounter, secondsElapsed, rate)
					// In the periodic log block:
					logf("Drag move rate: %d potential / %d actual moves in %.2fs → %.1f / %.1f per sec",
						moveCounter, actualPostCounter, secondsElapsed,
						rate, //float64(moveCounter)/secondsElapsed,
						float64(actualPostCounter)/secondsElapsed)
				}

				// Reset counters
				moveCounter = 0
				actualPostCounter = 0
				lastRateLogTime = now
			}

			// Then proceed with your existing debounce/post logic...
			if willPostMessage { //(newX != lastPostedX || newY != lastPostedY) &&
				//now.Sub(lastMovePostedTime) >= MIN_MOVE_INTERVAL {
				// Inside the if (debounce condition):
				//actualPostCounter++
				// prepare data & procPostMessage.Call(...)

				//data := new(WindowMoveData) // Heap-allocated, TODO: avoid heap allocation somehow.
				// Create a local copy of the data.
				// This stays on the STACK, so it's lightning fast.
				data := WindowMoveData{
					Hwnd:        targetWnd,
					X:           newX,
					Y:           newY,
					InsertAfter: 0, // this is the value for HWND_TOP but SWP_NOZORDER below makes it unused, supposedly!
					Flags:       SWP_NOSIZE | SWP_NOACTIVATE | SWP_NOZORDER,
				}
				//data.Hwnd = targetWnd
				//data.X = newX // int32, full range
				//data.Y = newY
				//data.InsertAfter = 0 // this is the value for HWND_TOP but SWP_NOZORDER below makes it unused, supposedly!

				//data.Flags = SWP_NOSIZE | SWP_NOACTIVATE | SWP_NOZORDER // Or dynamic

				//// Post the move request instead of doing the windows move/drag motion here
				// procPostMessage.Call(
				// 	uintptr(trayIcon.HWnd),
				// 	WM_DO_SETWINDOWPOS,
				// 	0,                             // unused, target is in the struct!
				// 	uintptr(unsafe.Pointer(data)), // lParam = pointer to struct
				// )

				/* THE SELECT BLOCK:
				   This is Go's magic for non-blocking communication.
				*/
				select {
				case moveDataChan <- data:
					// SUCCESS: The data was copied into the buffered channel.
					// Now we ring the "Doorbell" to wake up the Main Thread.
					// PostThreadMessage is an asynchronous "fire and forget" call.
					procPostThreadMessage.Call(uintptr(mainThreadId), WM_DO_SETWINDOWPOS, 0, 0)

				default:
					// FAIL: The channel (2048 slots) is completely full.
					// This happens if the Main Thread is frozen (e.g., Admin console lag).
					// We MUST NOT block here, or we will freeze the user's entire mouse cursor.
					// We just increment our "shame counter" and move on.
					droppedMoveEvents.Add(1)
				}

				if ratelimitOnMove {
					lastMovePostedTime = now
					lastPostedX = newX
					lastPostedY = newY
				}
				//return 0 //0 = let it thru
				//XXX: let it fall thru so CallNextHookEx is also called!
			}
		} //main 'if'

	case WM_LBUTTONUP: //LMB released
		if capturing {
			capturing = false
			currentDrag = nil
			targetWnd = 0
			procReleaseCapture.Call()

			//return 0 //0 is to let it thru (1 was to swallow)
			//XXX: let it fall thru so CallNextHookEx is also called!
		}

	case WM_MBUTTONDOWN: //MMB pressed
		var winDown bool = keyDown(VK_LWIN) || keyDown(VK_RWIN)
		var shiftDown bool = keyDown(VK_SHIFT)
		var ctrlDown bool = keyDown(VK_CONTROL)
		var altDown bool = keyDown(VK_MENU)

		if winDown && !ctrlDown && !altDown {
			//winDOWN and MMB pressed without ctrl/alt but maybe or not shiftDOWN too, it's a gesture of ours:
			if !winGestureUsed { //wasn't set already
				winGestureUsed = true // we used at least once of our gestures
			}

			//data := new(WindowMoveData) // Heap-allocated, TODO: fix this the same way as for mouse move event!
			var data WindowMoveData // stack allocated — zero cost

			var hwnd windows.Handle
			if !shiftDown {
				// winkey + MMB → send active window to bottom

				// winkey_DOWN but no other modifiers(including shift) is down
				// and LMB is down, ofc, then we start move window gesture:

				data.InsertAfter = HWND_BOTTOM
				data.Flags = SWP_NOMOVE | SWP_NOSIZE | SWP_NOACTIVATE
				hwnd = windowFromPoint(info.Pt) // window under cursor

			} else {
				// winkey + shift + MMB → bring focused window to top

				// shift is down too, so winkey_DOWN and shiftDOWN and LMB are down
				// but no other modifiers like ctrl or alt are down
				// then we start the bring focused window to front gesture:
				data.InsertAfter = HWND_TOP
				data.Flags = SWP_NOMOVE | SWP_NOSIZE        //|SWP_NOACTIVATE,
				ret, _, _ := procGetForegroundWindow.Call() // whichever the currently focused window is, wherever it is
				hwnd = windows.Handle(ret)                  // ← explicit cast
				// Bring to front, no activation, works only for the currently focused window which was sent to back before
				//had no effect because AI gave me the wrong constant value for HWND_TOP ! thanks chatgpt 5.2 !
			} // else

			if hwnd != 0 {
				// Send to back, no activation
				// if you do this for a focused window then no amount of LMB will bring it back to front unless it loses focus first!
				// procSetWindowPos.Call(
				// 	uintptr(hwnd),
				// 	HWND_BOTTOM,
				// 	0, 0, 0, 0,
				// 	SWP_NOMOVE|SWP_NOSIZE|SWP_NOACTIVATE,
				// )

				data.Hwnd = hwnd
				data.X = 0 // int32, full range
				data.Y = 0

				// Post the move request instead of doing the windows move/drag motion here
				// procPostMessage.Call(
				// 	uintptr(trayIcon.HWnd),
				// 	WM_DO_SETWINDOWPOS,
				// 	0,                             // unused, target is in the struct!
				// 	uintptr(unsafe.Pointer(data)), // lParam = pointer to struct, XXX: this was bad, it would get GC-ed, Grok figured it out after i was mid-refactoring via Gemini(which didn't)
				// )
				select {
				case moveDataChan <- data:
					// SUCCESS: The data was copied into the buffered channel.
					// Now we ring the "Doorbell" to wake up the Main Thread.
					// PostThreadMessage is an asynchronous "fire and forget" call.
					procPostThreadMessage.Call(uintptr(mainThreadId), WM_DO_SETWINDOWPOS, 0, 0)

				default:
					// FAIL: The channel (2048 slots) is completely full.
					// This happens if the Main Thread is frozen (e.g., Admin console lag).
					// We MUST NOT block here, or we will freeze the user's entire mouse cursor.
					// We just increment our "shame counter" and move on.
					droppedMoveEvents.Add(1)
				}
			}
			return 1 // swallow MMB
		} // the 'if' in MMB

	} //switch

	// Always pass the event down the chain so other apps don't break
	ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
	return ret
}

/* ---------------- Main ---------------- */

func createMessageWindow() (windows.Handle, error) {
	className, err := windows.UTF16PtrFromString("winbollocksHidden")
	if err != nil {
		return 0, fmt.Errorf("UTF16PtrFromString failed for class name: %v", err)
	}

	var wc WNDCLASSEX
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	wc.LpfnWndProc = wndProc
	wc.LpszClassName = className
	//nolint:errcheck
	hinst, _, _ := procGetModuleHandle.Call(0) // "If this parameter is NULL, GetModuleHandle returns a handle to the file used to create the calling process (.exe file)."
	wc.HInstance = windows.Handle(hinst)

	// Register class — check return value
	ret, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	if ret == 0 {
		lastErr := windows.GetLastError()
		return 0, fmt.Errorf("RegisterClassEx failed: %v (error code: %d)", err, lastErr)
	}

	hwndRaw, _, err := procCreateWindowEx.Call(
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
	if hwndRaw == 0 {
		lastErr := windows.GetLastError()
		return 0, fmt.Errorf("CreateWindowEx failed: %v (error code: %d)", err, lastErr)
	}

	return windows.Handle(hwndRaw), nil
}

func mustUTF16(s string) *uint16 {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		logf("failed in mustUTF16, err:%v", err)
		panic(err)
	}
	return p
}

var mouseCallback uintptr

func handleActualMove(data WindowMoveData) {
	target := data.Hwnd
	x := data.X
	y := data.Y

	ret, _, _ := procSetWindowPos.Call(
		uintptr(target),
		uintptr(data.InsertAfter),
		uintptr(x), uintptr(y),
		0, 0,
		uintptr(data.Flags),
	)

	if ret == 0 {
		errCode, _, _ := procGetLastError.Call()
		logf("SetWindowPos failed(from within main message loop): hwnd=0x%x error=%d", target, errCode)
		if errCode == 5 { // Access denied (UIPI likely)
			showTrayInfo("winbollocks", "Cannot move elevated window (access denied), you'd have to run as admin.")
		}
		// // Optional: fallback to native drag simulation (simulates title-bar drag, often works when SetWindowPos is blocked) - grok
		// pt := POINT{X: x, Y: y}
		// lParamNative := uintptr(pt.Y)<<16 | uintptr(pt.X)
		// procPostMessage.Call(uintptr(target), WM_NCLBUTTONDOWN, HTCAPTION, lParamNative)
	}
}

// func makeLParam(x, y int32) uintptr { // chatgpt
//
//		return uintptr(uint32(uint16(x)) | (uint32(uint16(y)) << 16))
//	}
//
// func makeLParam(x, y int32) uintptr { //grok, dumb for y needs &
//
//		return uintptr((int64(y) << 16) | (int64(x) & 0xFFFF))
//	}
//
// func makeLParam(x, y int32) uintptr { //grok, fixed by me
//
//		return uintptr(((int64(y) << 16) & 0xFFFF0000) | (int64(x) & 0xFFFF))
//	}
func makeLParam(x, y int32) uintptr { // grok again
	//AND ensures 16-bit truncation, prevents high bits bleed. No warnings, handles negatives.
	// cast doesn't change bits only interpretation
	//The cast to uint32 doesn't "change" the bits in a harmful way for your scenario (2's complement representation is preserved,
	// and &0xFFFF truncates to the low 16 bits correctly before shifting).
	// The following line suppresses the warning:
	// #nosec G115 -- safe: coords are screen pixels, always fit in 16 bits
	return uintptr((uint32(y)&0xFFFF)<<16 | (uint32(x) & 0xFFFF))
}

func isWindowInMoveLoop(targetThreadID uint32) bool {
	var info GUITHREADINFO
	info.CbSize = uint32(unsafe.Sizeof(info))

	// We pass the target thread ID. If 0, it gets the foreground thread.
	ret, _, _ := procGetGUIThreadInfo.Call(uintptr(targetThreadID), uintptr(unsafe.Pointer(&info)))
	if ret == 0 {
		return false
	}

	// Flags bit 0x00000002 is GUI_INMOVESIZE
	// Or we can just check if HwndMoveSize is non-zero
	return info.HwndMoveSize != 0
}

const WM_CANCELMODE = 0x001F

func cancelMoveMode(hwnd windows.Handle) error {
	ret, _, err := procSendMessage.Call(uintptr(hwnd), WM_CANCELMODE, 0, 0)
	if ret == 0 {
		// WM_CANCELMODE doesn't really return anything useful; just log the syscall error
		if err != nil && err.(windows.Errno) != 0 {
			return fmt.Errorf("WM_CANCELMODE failed: %v", err)
		}
	}
	return nil
}

// func PeekMessage(msg *MSG, hwnd windows.Handle, msgMin, msgMax, removeMsg uint32) (bool, error) {
// 	ret, _, err := procPeekMessageW.Call(
// 		uintptr(unsafe.Pointer(msg)),
// 		uintptr(hwnd),
// 		uintptr(msgMin),
// 		uintptr(msgMax),
// 		uintptr(removeMsg),
// 	)

// 	if ret == 0 {
// 		// ret == 0 means either:
// 		//   - no message available
// 		//   - error
// 		// To distinguish, check err against ERROR_SUCCESS.
// 		if err != windows.ERROR_SUCCESS {
// 			return false, err
// 		}
// 		return false, nil
// 	}

// 	return true, nil
// }

var wndProc = windows.NewCallback(func(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_QUERYENDSESSION:
		// system is asking permission to end session
		logf("system is asking permission to end session")
		return 1 // allow
	case WM_ENDSESSION:
		if wParam != 0 {
			logf("WM_ENDSESSION with wParam!=0 aka system shutdown or restart detected")
			// ensure flush here if buffered
		} else {
			logf("WM_ENDSESSION with wParam == 0 (weird?!)")
		}
		exitf(20, "due to WM_ENDSESSION")
		unreachable()
		return 0

	//TODO: add option in systray if 'true' keep moving the window even after winkey is released, else stop; the latter case would stop it from moving after coming back from unlock screen, if it was moving when lock happened.
	//TODO: Add WH_SHELL Hook for Focus Change Detection - in progress.
	//TODO: Do the postmessage for any other UI calls inside hooks (e.g., ShowWindow, SetForegroundWindow attempts, etc.) — postmessage them too.

	case WM_START_NATIVE_DRAG:
		logf("doing it via WM_START_NATIVE_DRAG")

		target := windows.Handle(wParam)
		if target != 0 {
			//logf("got target %d", target)
			// get target thread id
			// var targetThreadId uint32
			// procGetWindowThreadProcessId.Call(uintptr(target), uintptr(unsafe.Pointer(&targetThreadId))) // bad chatgpt
			// Fix: Capture thread ID from return value, good grok
			var targetProcessId uint32
			r1, _, err := procGetWindowThreadProcessId.Call(uintptr(target), uintptr(unsafe.Pointer(&targetProcessId)))
			if r1 == 0 {
				logf("GetWindowThreadProcessId for target HWND 0x%X failed: %v", target, err)
				return 0
			}
			targetThreadId := uint32(r1) // This is the actual thread ID

			// current thread id
			curTid := windows.GetCurrentThreadId()

			// attach input so SetForegroundWindow is allowed
			// Attach (check success)
			attachRet, _, attachErr := procAttachThreadInput.Call(uintptr(curTid), uintptr(targetThreadId), uintptr(1)) // TRUE
			if attachRet == 0 {
				//if AttachThreadInput returns 0 (failure), you do not need to attempt detach, as the merge never happened (state remains unattached).
				//Historical context: Pre-Vista, failed attach could leave partial state; Vista+ UIPI makes it atomic (fail = clean).
				logf("AttachThreadInput for thread id '%d' failed: %v", curTid, attachErr)
				return 0 // Bail, or fallback to manual
			} else {
				logf("attached")
			}
			// time.Sleep(1000 * time.Millisecond)
			// logf("waited 1 sec")

			//due to 'goto' must declare these here:
			var success, focused bool
			var start time.Time

			//focus is needed, else it can do the drag without visually moving it, but on LMBUp(released LMB) it updates the window position! and it sometimes doesn't trigger at all (possibly due to a diff. issue that's still ongoing atm)
			fgRet, _, lastErr := procSetForegroundWindow.Call(uintptr(target)) //XXX: focus is required for WM_NCLBUTTONDOWN to work!
			//done: check return/err values for this ^

			if fgRet == 0 {
				// SetForegroundWindow returned FALSE → failed
				errCode := windows.GetLastError()
				logf("SetForegroundWindow failed (fgRet='%d')on HWND 0x%X: %v (last error %v)",
					fgRet, target, lastErr, errCode)

				// detach input even if SetForegroundWindow failed

				// procAttachThreadInput.Call(uintptr(curTid), uintptr(targetThreadId), uintptr(0)) // FALSE
				// return 0
				goto detach
			} else {
				// detach input even if SetForegroundWindow succeeded
				// XXX: this really makes native drag never work because it needs to be attached for the below sends to work!
				//procAttachThreadInput.Call(uintptr(curTid), uintptr(targetThreadId), uintptr(0)) // FALSE, detaching here makes native drag not work at all
			}

			//const WM_NULL = 0x0000
			// //Nudge target's pump to flush activation messages faster
			// //XXX: no effect on kicking things into gear and not sometimes failing to start native drag!
			// for i := 0; i < 1000; i++ {
			// 	procPostMessage.Call(uintptr(target), WM_NULL, 0, 0) // harmless dummy message
			// }

			//this is overkill, not actually needed, apparently, but leaving it in for now.
			//FIXME: need to wait until focus settled, OR something else is preventing the native drag from starting if it wasn't already focused!
			// 2. Wait until target is really foreground (poll, timeout 500ms)
			const maxWaitMs = 500
			const pollMs = 10
			// Compile-time assertion: maxWaitMs > pollMs
			type _ [1 + (maxWaitMs - pollMs)]int // if max <= poll, negative array size = compile error
			// or with custom message (Go doesn't support, but error says "negative constant index")
			//_ = [1]int{}[maxWaitMs-pollMs : maxWaitMs-pollMs+1] // alternative slice trick, fails if diff <=0
			const pollInterval = pollMs * time.Millisecond
			focused = false
			start = time.Now()
			for time.Since(start).Milliseconds() < maxWaitMs {

				// From a foundational perspective, GetForegroundWindow returns the HWND with keyboard focus (the "foreground" flag),
				// but this flag sets before the full activation process completes (e.g., animations, pump settlement, or visual effects).
				// Exploring mechanics: Focus change is multi-step: SetForegroundWindow sets the flag, but Windows' DWM (Desktop Window Manager)
				//  handles animations (restore/minimize/fade) + pump flush, taking 10-50ms (or longer on slow hardware).
				//  So GetForegroundWindow "lies" in that it reports the flag, but the window isn't "settled" for modals like drag (pump busy).

				//  So GetForegroundWindow is telling the truth about the flag, but the window may not yet be in a state
				//  where the native drag modal (or other input-sensitive code) can reliably start. This is why your poll exits
				//  quickly but drag sometimes fails on unfocused targets — the flag is set, but the app hasn't finished processing activation messages.

				fg, _, err := procGetForegroundWindow.Call()
				//procGetForegroundWindow.Call()never sets err to non-zero on failure.
				//GetForegroundWindow is documented to return NULL (0) on failure, but it does not set last error reliably (many WinAPI functions behave this way — error code is not guaranteed to be updated).
				//if err.Is() != windows.Errno(0) || fg == 0 {
				if fg == 0 {
					//So err will almost always be windows.ErrorNumber(0) (syscall.Errno(0)), even when fg == 0 (real failure).
					logf("Focusing got err: '%v' but ret was '%d' aka HWND 0x%X", err, fg, fg)
				} else if fg == uintptr(target) {
					logf("Got focus now on target HWND 0x%X.", target)
					focused = true
					break
				} else {
					logf("Not focused yet, waiting more.")
				}
				time.Sleep(pollInterval)
			}
			if !focused {
				logf("Failed to focus target window '%d' in %d ms, aborting the native drag because without focus it won't natively drag!", target, maxWaitMs)
				return 0
			}

			//procSetCapture.Call(uintptr(target)) // XXX: this make native dragging never trigger!

			// SYNC POINT: Flush the target's queue
			// This ensures focus/activation messages are DONE before we drag
			// see it's SendMessage aka sync not PostMessage aka async!
			procSendMessage.Call(uintptr(target), 0, 0, 0) // 0 is WM_NULL

			//procReleaseCapture.Call() //FIXME: unclear if this is ever needed and when. If i do use it here, it misses to do native drag more times than when I don't use it!

			// 			When/If You Need procReleaseCapture.Call()
			// From a foundational perspective, ReleaseCapture releases the mouse capture if any window (including the target or your hidden one) has it, ensuring no prior capture interferes with new modals (e.g., native drag). Exploring mechanics: Windows capture "owns" mouse events for one window — if active, other windows ignore downs/moves. In your flow, if the target had capture (e.g., from prior selection), your SC_MOVE/injection might fail (modal won't start). Historical context: Pre-Win7, capture bugs were common; now it's robust, but explicit release is best practice.
			// From a practical example viewpoint: In AutoHotkey drag scripts, ReleaseCapture is called before injection to "reset" — prevents "stuck" drags. In your tests, avoiding it works because Total Commander rarely captures on down (only in modals), but in apps like Notepad (text selection) or Explorer (file drag), it might fail without. Nuances: Call after attachment (shared state), before injection/post. Edge: If no capture, no-op. If your capture (from hook), releases it safely.
			// Implications for winbollocks: Safe to add — prevents rare failures (e.g., target had capture from prior gesture). Related: Log GetCapture() before/after to debug if needed.
			// Recommendation: Add it after focus, before injection/post — no downside.
			// - grok

			//procSendMessage.Call(uintptr(target), WM_NCLBUTTONDOWN, HTCAPTION, lParamNative) // synchronous
			// ret, _, err = procSendMessage.Call(
			// 	uintptr(target),
			// 	WM_NCLBUTTONDOWN,
			// 	HTCAPTION,
			// 	lParamNative,
			// )
			// logf("WM_NCLBUTTONDOWN ret=%d err=%v", ret, err)

			// //XXX: this 40ms delay isn't needed when spy++ v17 is running, but when it's not it always misses the first drag attempt if target wasn't focused and winbollocks was just started.
			// logf("sleeping 100 ms before SC_MOVE")
			// time.Sleep(100 * time.Millisecond) //FIXME: temp, remove this! but make it 100ms and it misses the native drag more often! weird.
			// logf("done slept 100 ms before SC_MOVE")

			/*
				You confirmed correctly: AttachThreadInput is only needed for SetForegroundWindow (to bypass focus restrictions),
				not for PostMessage / SendMessage itself. Cross-thread PostMessage works without attachment (messages are queued to the target's thread safely).
				SendMessage requires the target thread to be pumping messages, but no attachment needed unless focus/input state is involved.
				- grok fast
			*/
			if false {
				// Get current cursor pos for lParam (screen coords, fresh)
				var cursorPt POINT
				getRet, _, getErr := procGetCursorPos.Call(uintptr(unsafe.Pointer(&cursorPt)))
				if getRet == 0 {
					logf("GetCursorPos failed: %v", getErr)
					// Fallback to your +5 method or bail
				}
				//lParamNative := uintptr(uint32(cursorPt.Y)<<16 | uint32(cursorPt.X)) // MAKEWPARAM equivalent
				var lParamNative uintptr = makeLParam(cursorPt.X, cursorPt.Y)
				//var lParamNative uintptr = 0
				// Use PostMessage (async) to avoid sync issues
				ret, _, err := procPostMessage.Call(
					uintptr(target),
					WM_NCLBUTTONDOWN,
					HTCAPTION,
					lParamNative,
				)
				logf("PostMessage WM_NCLBUTTONDOWN ret=%d err=%v", ret, err)
			} else {
				// attempt to bring the target to the foreground and start the native move

				// procSetForegroundWindow.Call(uintptr(target))
				//procReleaseCapture.Call()
				//logf("called procReleaseCapture which makes it miss native drag start more times than w/o it!")
				//XXX: don't use Send instead of Post here or it will recurse/reenter this once and stop at this until LMB UP happens, but this means a second drag is initiated afterwards.
				ret, _, err := procPostMessage.Call(
					uintptr(target),
					WM_SYSCOMMAND,
					SC_MOVE|HTCAPTION,
					0,
				)
				logf("WM_SYSCOMMAND SC_MOVE|HTCAPTION ret=%d err=%v", ret, err)
			}
			//procSendMessage.Call(uintptr(target), 0, 0, 0) // 0 is WM_NULL

			//From a validity perspective, lParam = 0 is completely valid for WM_LBUTTONDOWN — the message doesn't require non-zero coords,
			// and Windows won't reject it. The lParam is a 32-bit value (on 64-bit too, packed as DWORD),
			// where the low-order word is the x-coordinate (signed 16-bit) and high-order word is y (signed 16-bit).
			// So lParam = 0 means x=0, y=0 — a click at the upper-left corner of the window's client area (not the desktop top-left).
			// Post WM_LBUTTONDOWN to main HWND (lParam = 0 = client 0,0)
			{ // a block to satisfy 'goto detach'
				//this is needed so it doesn't hit the child eg. LListBox in tcmd thus causing a file drag instead of a window native move drag
				ret, _, err := procPostMessage.Call(uintptr(target), WM_LBUTTONDOWN, 1, 0) // wParam= MK_LBUTTON = 1, lParam=0
				logf("WM_LBUTTONDOWN ret=%d err=%v", ret, err)
			}

			//procSendMessage.Call(uintptr(target), 0, 0, 0) // 0 is WM_NULL

			//FIXME: what to do when any of these postmessage fail?!

			// // ===== synchronization point =====
			// if !waitForMoveLoop(0 /*targetThreadId*/, 500*time.Millisecond) {
			// 	logf("move loop never started")
			// 	goto detach
			// }

			//this is required to happen even tho the first time ever (ie. after running the program) the native dragging will always fail! unless spy++ is running and watching msgs of target window eg. tcmd!
			logLMBState("before injecting LMBDown") // it's UP because hook swallowed it!
			injectLMBDown()                         //XXX: LMB is UP(due to swallowed in hook) before the inject and DOWN instantly after inject!
			logLMBState("after injecting LMBDown")  // it's DOWN now.

			// // USER32 is now definitely in modal move loop
			// logf("GUI_INMOVESIZE detected")

			// logLMBState("before injecting it") //it's UP (due to swallowed in hook)

			// Wait up to 50ms for the window to actually start the drag
			success = false
			for i := 0; i < 200; i++ {
				// pump messages, avoids stuttered mouse and allows my hooks to run.
				for {
					var msg MSG
					ret, _, _ := procPeekMessage.Call(
						uintptr(unsafe.Pointer(&msg)),
						0,
						0,
						0,
						PM_REMOVE,
					)
					if ret == 0 {
						break
					}
					procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
					procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
				}

				capHwnd, _, _ := procGetCapture.Call()
				if capHwnd == uintptr(target) {

					//logf("capturing detected, sleeping 20ms now...")
					success = true
					// WE FOUND IT, but don't leave yet!
					// Give it 20ms of "Attached" time to finish entering the loop
					//time.Sleep(20 * time.Millisecond)
					break // It started!
				}
				//logf("capturing not yet... %d, err: %v", capHwnd, err) // hwnd is 0 and err is success
				time.Sleep(5 * time.Millisecond)
			}

			if !success {
				logf("capturing failed...")
				// if err := cancelMoveMode(target); err != nil { //no error but no effect either, LMBDown is still needed to properly "close" the "capturing failed..." state for future tries.
				// 	logf("cancelMoveMode returned error: %v", err)
				// }
				// now detach safely
				goto detach
			} else {
				logf("capturing detected")
			}

			// {
			// 	ret, _, err := procSendMessage.Call(
			// 		uintptr(target),
			// 		WM_SYSCOMMAND,
			// 		SC_MOVE|HTCAPTION,
			// 		0,
			// 	)
			// 	logf("2 WM_SYSCOMMAND SC_MOVE|HTCAPTION ret=%d err=%v", ret, err)
			// }

			//time.Sleep(500 * time.Millisecond) //FIXME: temp, remove this!
			// //time.Sleep(40 * time.Millisecond) //FIXME: temp, remove this!

			// logLMBState("before injecting LMBDown") // it's UP because hook swallowed it!
			// // //now insert a LMB down (since the real one we swallowed) - required for WM_NCLBUTTONDOWN to work.
			// // //required for WM_SYSCOMMAND SC_MOVE|HTCAPTION to work, but must injected be AFTER it(not before)!
			// injectLMBDown() //XXX: LMB is UP(due to swallowed in hook) before the inject and DOWN instantly after inject!
			// // //nvmthisbecauseitsnonsensicalnowFIXME: only inject LMB down if the physical state is still down? else we'd have to ensure the LMB up is also injected later? but when if LMB was up already physically, can't know when drag ended then!
			// logLMBState("after injecting LMBDown") //it's DOWN even tho we're single threaded so my hook didn't run!
			// // //XXX: so "Child interception is the killer in failures." ie. this LMBDown hits the LListBox a child of target's main hwnd but the SC_MOVE hits the main hwnd which also needs the WM_LBUTTONDOWN to hit it (not child) for native drag to start!

			//time.Sleep(100 * time.Millisecond)//no effect on sometimes missing the native drag start, probably because my msg.loop and my hooks are on same thread.

			// logf("sleeping 100 ms before detach")
			// time.Sleep(100 * time.Millisecond) //FIXME: temp, remove this!
			// logf("done slept 100 ms before detach")

			// {
			// 	success2 := false
			// 	// Poll for the actual move loop state
			// 	for i := 0; i < 200; i++ {
			// 		if isWindowInMoveLoop(0) { //targetThreadId) {
			// 			success2 = true
			// 			logf("Confirmed: Window is in native MoveSize loop!")
			// 			// Still give it that 20ms marriage buffer to be safe
			// 			time.Sleep(20 * time.Millisecond)
			// 			break
			// 		}
			// 		time.Sleep(5 * time.Millisecond)
			// 	}

			// 	//but it doesn't map to native drag, it's wtw else!
			// 	if !success2 {
			// 		// 	logf("Native drag LATCHED successfully")
			// 		// } else {
			// 		logf("isWindowInMoveLoop is still false")
			// 	}
			// }

		detach:
			//must detach here(late), else it won't do the drag.
			//procAttachThreadInput.Call(uintptr(curTid), uintptr(targetThreadId), uintptr(0)) // FALSE
			// After all injection and SC_MOVE are done
			detachRet, _, detachErr := procAttachThreadInput.Call(
				uintptr(curTid),
				uintptr(targetThreadId),
				uintptr(0), // FALSE = detach
			)

			if detachRet == 0 {
				errCode := windows.GetLastError()
				logf("DetachThreadInput failed on target 0x%X: %v (error code %d)", target, detachErr, errCode)

				// What to do if detach fails (rare, but serious)
				// Option 1: Log and continue (usually safe)
				// Option 2: Force a hard reset of your state
				hardReset()
				// Option 3: Retry once after short delay
				// time.Sleep(10 * time.Millisecond)
				// procAttachThreadInput.Call(uintptr(curTid), uintptr(targetThreadId), 0)
			} else {
				logf("detached, DetachThreadInput succeeded for target 0x%X", target)
			}
			//logf("detached")
			logLMBState("after injecting it and after detached")
		}
		return 0
	case WM_INJECT_SEQUENCE:
		//avoids injecting from the hook
		which := uint16(wParam)        // ie. uint16(vk))
		injectShiftTapThenWinUp(which) // it's correct casting, as per AI.
		return 0
	case WM_FOCUS_TARGET_WINDOW_SOMEHOW:
		//this is here because avoids focusing window or injecting LMB from the hook
		if !forceForeground(targetWnd) {
			logf("Failed to force foreground(ie. to activate/focus window) this happens consistently when Start menu was already open; next, falling back to injected LMB click which, unfortunatelly, means here that it will click at the point in the window where u tried to move it which eg. in total commander might be on the exit button and it will exit!")
			// Optionally keep your old inject as backup

			//logf("injecting LMB click")
			// injecting a LMB_down then LMB_up so that the target window gets a click to focus and bring it to front
			// this is a good workaround for focusing it which windows wouldn't allow via procSetForegroundWindow (unless attaching to target window's thread!)
			injectLMBClick()
		}
		return 0
	case WM_MYTRAY:

		// Strip high word to get the low 16-bit message code
		low := uint32(lParam & 0xFFFF)

		// if low != WM_MOUSEMOVE { // any non-mouse_move(0x10200 on v4) events:
		// 	logf("WM_TRAY received with lParam %x, %x", lParam, low)
		// }

		//if ((lParam & 0x0FFFF) == WM_RBUTTONUP) || ((lParam & 0x0FFFF) == WM_CONTEXTMENU) {
		if low == WM_RBUTTONUP {
			/*
				Yes — handling WM_RBUTTONUP (after masking with 0xFFFF) alone would work on every Windows version, because:
				  XP → only 0x0205
				  Vista+ → both 0x0205 and 0x007B, but 0x0205 is still sent
			*/
			// Get mouse position early (always do this manually — wParam/lParam don't carry it reliably) - Grok
			var pt POINT
			procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

			logf("popping tray menu")
			hMenu, _, _ := procCreatePopupMenu.Call()

			exitText := mustUTF16("Exit")
			manualText := mustUTF16("Manual move (no focus)") //TODO: make default and remove the disabled variant!
			focusText := mustUTF16("Activate(focus) window on manual move(caveat: presses LMB once(but only if the thread-attaching variant fails) if not already focused)")
			ratelimitText := mustUTF16("Rate-limit window moves(by 5x, uses less CPU)")
			sldrText := mustUTF16("Log rate of moves(only if rate-limit above is enabled)")

			flags := MF_STRING
			if forceManual {
				flags |= MF_CHECKED
			}

			procAppendMenu.Call(hMenu, uintptr(flags), MENU_FORCE_MANUAL, uintptr(unsafe.Pointer(manualText)))

			var actFlags uintptr = MF_STRING // untyped constants can auto-convert, but not untyped vars(in the below call)
			if activateOnManualMoveOnly {
				actFlags |= MF_CHECKED
			}
			if !forceManual {
				// grey out this if manual move is not enabled, so it's more obvious it only applies to it,
				// because in native drag(FIXME: bugged currently(only works some of the time and on cmd.exe it lags due to our msg. loop and hooks are on same thread!)) it always focuses!
				actFlags |= MF_DISABLED
			}
			procAppendMenu.Call(hMenu, actFlags, MENU_ACTIVATE_MOVE,
				uintptr(unsafe.Pointer(focusText)))

			var rlFlags uintptr = MF_STRING
			if ratelimitOnMove {
				rlFlags |= MF_CHECKED
			}
			if !forceManual {
				//rate limit only applies to manual move mode
				rlFlags |= MF_DISABLED
			}
			procAppendMenu.Call(hMenu, rlFlags, MENU_RATELIMIT_MOVES,
				uintptr(unsafe.Pointer(ratelimitText)))

			var sldrFlags uintptr = MF_STRING
			if shouldLogDragRate {
				sldrFlags |= MF_CHECKED
			}
			// Disable (grey) the "Log rate of moves" item when rate-limit is off
			if !ratelimitOnMove {
				sldrFlags |= MF_DISABLED | MF_GRAYED
			}
			procAppendMenu.Call(hMenu, sldrFlags, MENU_LOG_RATE_OF_MOVES,
				uintptr(unsafe.Pointer(sldrText)))

			procAppendMenu.Call(hMenu, MF_STRING, MENU_EXIT, uintptr(unsafe.Pointer(exitText)))

			// var pt POINT
			// procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

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
				forceManual = !forceManual
			case MENU_ACTIVATE_MOVE:
				activateOnManualMoveOnly = !activateOnManualMoveOnly
			case MENU_RATELIMIT_MOVES:
				ratelimitOnMove = !ratelimitOnMove
				if !ratelimitOnMove {
					moveCounter = 0
					actualPostCounter = 0
					now := time.Now()
					lastRateLogTime = now
					lastMovePostedTime = now
					lastPostedX = -1
					lastPostedY = -1
				}
			case MENU_LOG_RATE_OF_MOVES:
				shouldLogDragRate = !shouldLogDragRate

			case MENU_EXIT:
				//procUnhookWindowsHookEx.Call(uintptr(mouseHook))
				exit(0)
			}

		} // fi RMB context menu
		return 0

	case WM_CLOSE: //case 0x0010: // WM_CLOSE
		//procUnhookWindowsHookEx.Call(uintptr(mouseHook))
		exit(0)
	case WM_EXIT_VIA_CTRL_C:
		var ctrlType uint32 = uint32(wParam)
		switch ctrlType {
		//case 0, 2: // CTRL_C_EVENT, CTRL_CLOSE_EVENT
		case CTRL_C_EVENT:
			// procUnhookWindowsHookEx.Call(uintptr(hHook))
			// os.Exit(0)
			//todo()
			exitf(128, "exit via Ctrl+C")

		case CTRL_BREAK_EVENT:
			exitf(128, "exit via Ctrl+Break")
		case CTRL_CLOSE_EVENT:
			exitf(127, "exit via Ctrl+Close event (wtf is this?!)") //TODO: find out what this is.
		default:
			exitf(129, "exit via unknown event %d", ctrlType)
		}
		unreachable()
	case WM_DO_SETWINDOWPOS:
		panic("!!! shouldn't have gotten WM_DO_SETWINDOWPOS in wndProc!")
	} //switch

	//let the default window proc handle the rest:
	ret, _, _ := procDefWindowProc.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
})

func deinit() {
	//TODO: add the others? or perhaps there's no point?!
	capturing = false
	procReleaseCapture.Call()
	if mouseHook != 0 {
		procUnhookWindowsHookEx.Call(uintptr(mouseHook))
		mouseHook = 0
	}
	if kbdHook != 0 {
		procUnhookWindowsHookEx.Call(uintptr(kbdHook))
		kbdHook = 0
	}
}

// type exitCode int // Custom type so recover knows it's an intentional exit
func exit(code int) {
	// if code == 0 {
	// 	return // Just return and let main finish naturally, so bad Gemini 3 Fast!
	// }
	//os.Exit(code) // Hooks are removed after this. Your state must already be sane.
	// Panic with our custom type so main's defer can catch it
	// panic(exitStatus{
	// 	Code:    code,
	// 	Message: "express exit with that exit code",
	// })
	exitf(code, "express exit")
}

const CTRL_C_EVENT = 0
const CTRL_BREAK_EVENT = 1
const CTRL_CLOSE_EVENT = 2

// done: keep this for the devbuild.bat mode?! ie. when having console!
var ctrlHandler = windows.NewCallback(func(ctrlType uint32) uintptr {
	/*
			The handler registered via SetConsoleCtrlHandler (and indirectly through Go’s os/signal) is executed on a dedicated control-handler thread, not on the thread that created your window.

		That matters because:

		Win32 requires many window operations — especially DestroyWindow() — to be performed on the creating thread.

		Calling DestroyWindow() from the Ctrl+C handler thread can fail with:

		invalid handle

		access denied

		already destroyed semantics

		or simply undefined teardown behavior.
		-chatgpt 5.2
		ok So u can't attempt to destroy hwnd from this thread, it will 'access denied' !
		so we don't exit from here, we tell message window to exit for us.
	*/
	// defer secondary_defer()
	// defer primary_defer()

	// switch ctrlType {
	// //case 0, 2: // CTRL_C_EVENT, CTRL_CLOSE_EVENT
	// case CTRL_C_EVENT:
	// 	// procUnhookWindowsHookEx.Call(uintptr(hHook))
	// 	// os.Exit(0)
	// 	//todo()
	// 	exitf(128, "exit via Ctrl+C")

	// case CTRL_BREAK_EVENT:
	// 	exitf(128, "exit via Ctrl+Break")
	// case CTRL_CLOSE_EVENT:
	// 	exitf(127, "exit via Ctrl+Close event (wtf is this?!)") //TODO: find out what this is.
	// default:
	// 	exitf(129, "exit via unknown event %d", ctrlType)
	// }
	//unreachable()
	procPostMessage.Call(
		uintptr(trayIcon.HWnd),
		WM_EXIT_VIA_CTRL_C,
		uintptr(ctrlType),
		0,
	)
	return 1 // 1=true aka i handled this event ie. don't do the default handling which would exit.
})

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
	logFile *os.File
	//hasConsole bool
	useStderr bool // true if os.Stderr is valid/writable
	//consoleChecked bool
)

// func detectConsole() {
// 	if consoleChecked {
// 		return
// 	}

//		h := windows.Handle(os.Stdout.Fd())
//		var mode uint32
//		err := windows.GetConsoleMode(h, &mode)
//		hasConsole = (err == nil)
//		consoleChecked = true
//	}
func init() {
	useStderr = false

	// //detectConsole()
	// h := windows.Handle(os.Stderr.Fd())
	// var mode uint32
	// err := windows.GetConsoleMode(h, &mode)
	// hasConsole = (err == nil)
	h := windows.Handle(os.Stderr.Fd())
	var mode uint32
	err := windows.GetConsoleMode(h, &mode) // optional, for true console
	if err != nil {
		return
	}
	n, err := windows.GetFileType(h)
	if err != nil {
		return
	}
	useStderr = (n != windows.INVALID_FILE_ATTRIBUTES) // basic validity
	// Optional: Test writability
	if useStderr {
		_, writeErr := os.Stderr.WriteString("") // zero-write test
		useStderr = writeErr == nil
	}
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

var (
	// buffer size here matters only in the case where you used devbuild.bat AND are running as admin eg. runasadmin.bat AND you drag scrollbar or select text because that blocks the printf which blocks the hooks since this is single threaded at the moment (message loop and hooks are on same 1 thread)
	logChanSize   = 4096
	logChan       = make(chan string, logChanSize) // Buffer of this many log messages
	logWorkerDone = make(chan struct{})            // The "I'm finished" signal
)

const attemptAtomicSwapThisManyTimes uint = 100

func logf(format string, args ...any) {

	s := fmt.Sprintf(format, args...)
	now := time.Now().Format("Mon Jan 2 15:04:05.000000000 MST 2006") // these values must be used exactly, they're like specific % placeholders.
	//now := time.Now().Format("2006-01-02T15:04:05.000Z07:00")
	finalMsg := fmt.Sprintf("[%s] %s\n", now, s)

	// Check the current pressure on the pipe
	currentDepth := int64(len(logChan))
	// Update the high water mark if this is a new record
	// We use a loop or a CompareAndSwap to ensure we never overwrite
	// a higher value from another thread (though likely overkill here)
	wentAccordingToPlan := false
	//TODO: this logic for maxChannelFillForMoveEvents too.
	for range attemptAtomicSwapThisManyTimes { // try this only 100 times, to prevent infinite loop in impossible cases.
		oldMax := maxChannelFillForLogEvents.Load()
		if currentDepth <= oldMax {
			// Nothing to do, current is smaller
			wentAccordingToPlan = true
			break
		}
		if maxChannelFillForLogEvents.CompareAndSwap(oldMax, currentDepth) {
			// Optional: logf it? Careful, don't cause recursion!
			// Better to just let the exit logic report the final max.
			wentAccordingToPlan = true
			break
		}
		// If we reach here, another thread changed oldMax, so we loop again
	}

	// select with default makes this NON-BLOCKING
	select {
	case logChan <- finalMsg:
		// Message sent to the background worker
	default:
		// If the buffer is full, we drop the log so we don't lag the mouse
		droppedLogEvents.Add(1)
	}

	// 2. Note the problem if we exhausted the 100 tries
	if !wentAccordingToPlan {
		// We failed to record the peak after 100 tries.
		// Increment a "Contention Error" counter
		panic(fmt.Sprintf("Failed(%d times) to set an atomic to int64 value %d. Happened during this log msg: '%s'", attemptAtomicSwapThisManyTimes, currentDepth, finalMsg))
	}
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

	//no effect: //nolint:govet,unsafeptr // Win32 hook lParam is OS-owned pointer valid for callback duration
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

	/*
			The sequence for a key release is effectively:

		Hardware generates a key-up interrupt

		Windows constructs the keyboard event

		Low-level keyboard hooks are called

		Windows updates the global async key state

		The event is delivered to higher layers (message queues, hotkeys, etc.)

		So when you are inside keyboardProc handling WM_KEYUP for VK_LWIN:

		The event means “Win is being released”

		But the async key state has not yet been updated

		Therefore GetAsyncKeyState(VK_LWIN) still reports the key as down (0x8000 set)
		- chatgpt 5.2
	*/

	// Key UP
	if wParam == WM_KEYUP || wParam == WM_SYSKEYUP {
		switch vk {
		case VK_LWIN, VK_RWIN:
			//logf("winUP")
			//hardResetIfDesynced(false)
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

			//var checkBefore bool = winDown && !shiftDown && !altDown && !ctrlDown
			// if winDown {
			// 	// so this always triggers here, unclear as to why.
			// 	//XXX: "Short version: inside a low-level keyboard hook, GetAsyncKeyState still reflects the previous global key state, not the transition you are currently handling." - chatgpt5.2
			// 	logf("desync of winkey(is down but should be up) detected in keyboardProc.")
			// }
			//winDown.Store(false)
			//XXX: so winDown is true here even though we're handling the winUp in this here block.
			//if true { //winDown && !shiftDown && !altDown && !ctrlDown {
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

			//if !winGestureUsed {
			// don't suppress winkey_UP if we didn't use it for our gestures, so this allows say winkeyDown then winkeyUp to open Start menu
			//return 0 // pass thru the winkeyUP
			//XXX: let it fall thru(aka pass thru the winkeyUP), so that procCallNextHookEx is called!
			//} else
			if winGestureUsed {
				//next ok, we gotta suppress winkeyUP, else Start menu will pop open which is annoying because we just used winkey+LMB drag for example, not pressed winkey then released it
				winGestureUsed = false // gesture ends with winkey_UP

				// • Injecting input from inside a WH_KEYBOARD_LL hook is documented as undefined.
				// great, it was correct and other do it before, but now it's bad!
				//injectShiftTapThenWinUp(uint16(vk)) // it's correct casting, as per AI.

				/* Using Right Shift is a defensible and, in this context, slightly superior choice. The edge cases you walked through are the right ones to think about, and you resolved them correctly:

				If the user is already holding any modifier (including RShift), you suppress injection entirely.

				Therefore you will never undo a user-held modifier.

				The only remaining risk window is the micro-interval between your modifier check and the injected tap, which is operationally negligible and unavoidable in any design that is not kernel-mode.

				That is as good as it gets in user-mode.
				*/
				/*
						PostMessage is asynchronous.

					Semantics:

					• The message is placed into the target thread’s message queue.
					• The function returns immediately.
					• No reentrancy, no waiting for processing.
					• If the queue is full or the window is gone, the post can fail, but it does not block.
					chatgpt5.2
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
			} // XXX: else, don't suppress winkey_UP if we didn't use it for our gestures, so this allows say winkeyDown then winkeyUp to open Start menu, so let it fall thru(aka pass thru the winkeyUP), so that procCallNextHookEx is called!

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

// func shellProc(nCode int, wParam uintptr, lParam uintptr) uintptr {
// 	if nCode >= 0 {
// 		if nCode == 4 { // HSHELL_WINDOWACTIVATED
// 			hwnd := windows.Handle(wParam)
// 			var pid uint32
// 			procGetWindowThreadProcessId.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&pid)))
// 			il, err := processIntegrityLevel(pid)
// 			if err == nil && il >= 0x3000 { // high integrity or above
// 				logf("Elevated window focused (IL=0x%x, hwnd=0x%x) → reconciling state", il, hwnd)
// 				//hardResetIfDesynced() // your recovery
// 			}
// 		}
// 	}
// 	ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
// 	return ret
// }

/*
The init() Execution Flow

	Variable Initialization: First, all variables declared at the package level (outside functions) are initialized to their values or zero-values.

	init() execution: Then, any init() functions in the package run automatically.

	main() execution: Finally, the main() function starts.

Key Rules about init():

	No Arguments/Returns: It must look exactly like func init() { ... }.

	Multiple Inits: You can actually have multiple init() functions in the same file or package; they will run in the order they appear.

	One-Time Use: It runs exactly once per program execution, no matter how many other packages import that package.

Since you are doing Win32 stuff (message loops, handles, etc.), here is what you should avoid in init():

	    Don't create Windows/UI Elements: If you create a Window handle (HWND) in init(), the thread that created it might not be the same thread that runs your main() message loop. In Win32, windows are "owned" by the thread that created them. If the threads mismatch, your message loop won't receive events for that window.

	    Avoid heavy logic: init() blocks the startup of the entire program. If init() hangs, your app never reaches main().

	    Order of execution: If you have multiple files, init() functions run in the order the files are presented to the compiler. This can lead to "initialization order" bugs that are very hard to debug.
		- Gemini 3 Fast

		also don't use logf() here because it calls windows stuff to detect if it has console!
*/
func init() {
	//defaults:
	forceManual = true
	activateOnManualMoveOnly = true
	ratelimitOnMove = false
	shouldLogDragRate = false

	lastPostedX = -1
	lastPostedY = -1
	now := time.Now()
	//FIXME: these 2 need to be set when startDragging(see 'capturing' bool) happens(ie. state changed from not dragging to dragging, so 1 time not on every drag/move event!), every time! so not here!
	lastRateLogTime = now
	lastMovePostedTime = now

}

var isAdmin bool // Package level
func init() {
	// This runs automatically before main()
	//okthenTODO: is this gonna be a problem in init() before that lock in main happens?!
	/*1. The init() vs. LockOSThread worry
	No, it won't be a problem. The reason we lock the thread in main is specifically for the Message Loop and the Hook.
	windows.GetCurrentProcessToken() is a standard system call that doesn't care which thread it runs on.
	It just asks the OS for the current process's security context. You can safely call it in init() without any thread-locking prerequisites.
	*/
	token := windows.GetCurrentProcessToken()
	isAdmin = token.IsElevated()
}

var (
	procCreateMutex  = windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateMutexW")
	procReleaseMutex = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReleaseMutex")
	procCloseHandle  = windows.NewLazySystemDLL("kernel32.dll").NewProc("CloseHandle")
)

type MutexScope int

const (
	MutexScopeSession MutexScope = iota // 0
	MutexScopeMachine                   // 1
)

func (s MutexScope) Prefix() string {
	switch s {
	case MutexScopeSession:
		return "Local\\" // want this for winbollocks
	case MutexScopeMachine:
		return "Global\\" // don't want this for winbollocks, but do for dnsbollocks
	default:
		panic(fmt.Sprintf("Unhandled MutexScope value: %d", s))
	}
}

var mutexHandle uintptr

func releaseSingleInstance() {
	if mutexHandle != 0 {
		//defer is tied to the function, not to inner scopes, so it happens only when this func. exits!
		//defers do run when a panic happens
		defer func() { mutexHandle = 0 }()
		// Release ownership if we own it
		//procReleaseMutex.Call(mutexHandle)
		r1, _, e1 := procReleaseMutex.Call(mutexHandle)
		if r1 == 0 {
			logf("ReleaseMutex failed: %v", e1)
		}
		// Close handle so other instances can acquire
		//procCloseHandle.Call(mutexHandle)
		r2, _, e2 := procCloseHandle.Call(mutexHandle)
		if r2 == 0 {
			logf("CloseHandle failed: %v", e2)
		}
		//mutexHandle = 0
	}
}

func ensureSingleInstance(name string, scope MutexScope) {
	// Create a global mutex. The "Global\" prefix works across terminal sessions.
	/*
		Global\: The mutex is visible to all users on the machine. If User A is logged in and User B fast-switches to their account, User B cannot run the app.

		Local\: The mutex is visible only to the current session. User A and User B can both run the app simultaneously in their own sessions.
	*/
	//namePtr, _ := windows.UTF16PtrFromString("Global\\" + name)
	// Use "Local\\" for per-session isolation (allows multiple users on same machine)
	// Omit prefix entirely for same effect, but explicit is clearer.
	prefix := scope.Prefix() // panics if invalid/missing case
	str := prefix + name
	namePtr, err0 := windows.UTF16PtrFromString(str)
	//namePtr, err0 := windows.UTF16PtrFromString("Global\\" + name)
	if err0 != nil {
		exitf(3, "UTF16PtrFromString (in ensureSingleInstance) for str '%s' failed: %v", str, err0)
	}

	// CreateMutex(lpMutexAttributes, bInitialOwner, lpName)
	// CreateMutex: Security attributes NULL (0), Initial owner TRUE (1), Name
	ret, _, callErr := procCreateMutex.Call(0, 1, uintptr(unsafe.Pointer(namePtr)))

	// if windows.GetLastError() == windows.ERROR_ALREADY_EXISTS {
	// 	// We don't want to pause here usually, just die quietly or alert user.
	// 	// fmt.Printf("Application '%s' is already running.\n", name)
	// 	// os.Exit(0)
	// 	// Use our new exit logic to ensure the defer pause happens
	// 	exitf(0, "Application '%s' is already running.", name)
	// }

	// Normalize to an error we can use with errors.Is.
	var err error
	if callErr != nil && !errors.Is(callErr, windows.Errno(0)) {
		err = callErr
	} else if last := windows.GetLastError(); last != nil && !errors.Is(last, windows.Errno(0)) {
		err = last
	}

	if err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			exitf(5, "Application '%s' is already running.", name)
		}
		// other error handling if needed:
		// exitf(1, "CreateMutex failed: %v", err)
	}

	// If handle is 0, we didn't even create it (likely Access Denied for Global\)
	if ret == 0 {
		var extra string = ""
		if errors.Is(callErr, windows.Errno(5)) { // aka 'Access Denied'==5
			extra = " this means mutex attempt was 'Global\\' and it was already acquired by an admin-running exe"
		}
		exitf(2, "CreateMutex failed entirely: %v (code: %d)%s", err, err, extra)
	}
	// Note: We don't technically need to close this handle manually.
	// As long as the process is alive, the mutex is held.
	// When the process dies, Windows cleans it up.
	//_ = ret
	mutexHandle = ret
}

const writeProfile bool = false

var (
	profileWritten atomic.Bool
)

// In your defer panic/recover block or in exitf / exit()
func writeHeapProfileOnExit() {
	if profileWritten.Load() {
		return // already done
	}
	profileWritten.Store(true)

	f, err := os.Create("heap_final.prof")
	if err != nil {
		logf("Failed to create heap profile: %v", err)
		return
	}
	defer f.Close()

	runtime.GC() // Force a full collection first (cleaner profile)
	if err := pprof.WriteHeapProfile(f); err != nil {
		logf("WriteHeapProfile failed: %v", err)
	} else {
		logf("Heap profile written to heap_final.prof")
	}
}

func logWorker() {
	// This runs on Thread B.
	// even If fmt.Fprint blocks for 10 seconds here, Thread A (your mouse hook)
	// keeps spinning at 100% speed on its own CPU core.
	//counter := 0
	for msg := range logChan {
		//counter++
		internalLogger(msg)
		// if counter%500 == 0 {
		// 	time.Sleep(10 * time.Second) //FIXME: temp, remove this!
		// }
	}
	drops := droppedLogEvents.Load()
	if drops > 0 {
		internalLogger(fmt.Sprintf("Dropped %d log events due to contention.\n", drops))
	}
	maxfill := maxChannelFillForLogEvents.Load()
	if maxfill > 1 {
		internalLogger(fmt.Sprintf("Most log events seen at one time ie. peak queued on log channel: %d, out of logChanSize: %d\n", maxfill, logChanSize))
	}
	// This only executes AFTER close(logChan) is called AND the buffer is empty
	close(logWorkerDone)
}

func internalLogger(finalMsg string) {
	//detectConsole()
	if useStderr {
		// --- START TIMING ---
		startPrint := time.Now()
		//fmt.Fprintf(os.Stderr, "[%s] %s\n", timestamp, s)
		fmt.Fprintf(os.Stderr, "%s", finalMsg)
		duration := time.Since(startPrint)
		// --- END TIMING ---
		// Only alert us if the print took longer than a "frame" (16ms)
		if duration > 16*time.Millisecond {
			// Note: Printing this might trigger another lag, but it's for science!
			// XXX: used to happen when running as admin and u LMB drag the scroll bar or LMB on the text area which begins selection and auto selects 1 char already! when logging was happening on same thread as hooks and msg.loop.
			fmt.Fprintf(os.Stderr, "!!! LOG LAG DETECTED: %v !!!\n", duration)
		}
		return
	}

	if logFile == nil {
		initLogFile()
		if logFile == nil {
			return
		}
	}

	fmt.Fprintf(logFile, "%s", finalMsg)
	logFile.Sync()
}

func closeAndFlushLog() {
	// 1. Close the channel to tell the worker "no more logs are coming"
	close(logChan) // Yes — the worker will drain everything that was already queued before close.
	// 2. Wait for the worker to finish printing the backlog
	// This blocks until close(logWorkerDone) happens in the worker
	<-logWorkerDone
}

type theILockedMainThreadToken struct{}

var currentExitCode int = 0

func secondary_defer() { //secondary defer, never ran unless primary defer is defective(ie. panics in itself)
	var exitcode int
	// SECONDARY SAFETY: Catches panics that happen inside the primary defer (which is below)
	if r2 := recover(); r2 != nil {
		logf("!secondary defer here! [CRITICAL ERROR IN primary DEFER]: '%v'\n%s\n----snip----", r2, debug.Stack())
		exitcode = 120
	} else {
		logf("!secondary defer here! This shouldn't be reached ever. It means primary defer didn't os.Exit as it should. So, bad coding/logic, if here.")
		exitcode = 121
	}
	logf("!secondary defer here! Primary defer wanted to exit with exitcode: '%d' but we do: '%d'", currentExitCode, exitcode)
	closeAndFlushLog()
	os.Exit(exitcode) // XXX: oughtta be the only os.Exit! well 2of2
}

func primary_defer() { //primary defer

	/*
		What does recover() do? If your code has a panic (like a nil pointer dereference), the program usually crashes and closes the window immediately.
		recover() catches that panic, stops the "dying" process, and lets you print the error and pause before exiting.
	*/
	if r := recover(); r != nil {
		if status, ok := r.(exitStatus); ok {
			currentExitCode = status.Code
			// This was an intentional exit(code)
			//if code != 0 {
			logf("Program intentionally exited with code: '%d' and error message: '%s'", currentExitCode, status.Message)
			//}
		} else {
			currentExitCode = 1
			stack := debug.Stack()
			logf("--- CRASH: %v ---\nStack: %s\n--- END---", r, stack)
			//debug.PrintStack()
		}
	}
	cleanupTray()

	logf("Execution finished.")
	if writeProfile {
		writeHeapProfileOnExit()
	}
	// 2. Use your high-quality "clrbuf" waiter
	//detectConsole() // updated global bool even if logf was never executed(if it was then it updated it already) and if we forgot to put this in an init()
	// Only pause if we have an actual console window and an error occurred

	// hConsole, _, _ := procGetConsoleWindow.Call()
	// // 2. If no handle, try to attach to the parent's console
	// if hConsole == 0 {
	// 	var (
	// 		procAttachConsole     = kernel32.NewProc("AttachConsole")
	// 		ATTACH_PARENT_PROCESS = uintptr(^uint32(0)) // -1
	// 	)
	// 	ret, _, _ := procAttachConsole.Call(ATTACH_PARENT_PROCESS)
	// 	if ret != 0 {
	// 		hConsole, _, _ = procGetConsoleWindow.Call()
	// 	}
	// }

	// // 2. Check if Stdin is actually a terminal (not a pipe/null)

	// these 2 lines fixes things:
	// stat, err := os.Stdin.Stat()
	// isTerminal := err == nil && ((stat.Mode() & os.ModeCharDevice) != 0)
	//these two lines instead, broke all logging by causing a panic here: (uncomment them for testing purposes)
	// stat, _ := os.Stdin.Stat()
	// isTerminal := (stat.Mode() & os.ModeCharDevice) != 0

	//fixedFIXME: panics in here are silent when build.bat not devbuild.bat was used! not even log file gets them!

	//if hasConsole || hConsole != 0 || isTerminal || true {
	if stdinIsConsoleInteractive() {
		releaseSingleInstance() // don't hog the mutex while waiting for key
		//logf("s2")
		//todo()
		//logf("s3")
		//waitAnyKeyIfInteractive() //TODO: copy code over from the other project, for this. Or make it a common library or smth, then vendor it.
		logf("Press Enter to exit... TODO: use any key and clrbuf before&after")
		var dummy string
		_, _ = fmt.Scanln(&dummy)
	} else {
		logf("not waiting for keypress")
	}

	//XXX: these should be last:
	closeAndFlushLog()
	// 3. exit
	os.Exit(currentExitCode) // XXX: oughtta be the only os.Exit! well 1of2
}

func stdinIsConsoleInteractive() bool {
	h := windows.Handle(os.Stdin.Fd())

	var mode uint32
	err := windows.GetConsoleMode(h, &mode)
	return err == nil
}

func main() {
	// 1. Lock THIS specific thread (Thread A) to the OS for Win32/Hooks.
	runtime.LockOSThread() // first! in main() not in init() ! That runtime.LockOSThread() call in main is there because of a specific Windows requirement: Hooks and Message Loops are thread-bound.
	token := theILockedMainThreadToken{}
	/*
	   	When you call go func() { ... }(), you are telling the Go Scheduler to create a new goroutine.
	   	Unless you explicitly call runtime.LockOSThread() inside that new goroutine,
	   	the scheduler is free to run it on any available OS thread (Core 2, Core 3, etc.).

	   By calling runtime.LockOSThread() at the top of main, you are only "locking" the Main Thread.
	    You are essentially saying: "Hey Go, this specific thread is now reserved for Win32 GUI stuff.
	    Don't move me, and don't let anyone else sit here." All other goroutines (like your new log worker)
	    will see that the Main Thread is "busy" and locked, so they will automatically be spawned on different OS threads.
	*/
	// 2. Spawn the worker. The "Main Thread" Lock: Since we are using runtime.LockOSThread() in main, we want to be absolutely certain that the Go scheduler has finished its "Main Thread" bookkeeping before we start spawning background workers that we expect to land on other cores.
	// The Go scheduler sees Thread A is locked, so it puts this on Thread B.
	go logWorker()

	defer secondary_defer() //this runs second but only if first doesn't os.exit ie. it fails/panics!

	defer primary_defer() //this runs first

	installCtrlHandlerIfConsole()

	ensureSingleInstance("winbollocks_uniqueID_123lol", MutexScopeSession)

	// 3. Your logic (Task 1: don't use log.Fatal inside here!)
	if err := runApplication(token); err != nil {
		exitf(2, "Error: %v\n", err)
	}
}

func getConsoleWindow() (windows.HWND, error) {
	r1, _, err := procGetConsoleWindow.Call()

	hwnd := windows.HWND(r1)

	if hwnd == 0 {
		// syscall wrappers often return err == "The operation completed successfully."
		// when no failure occurred, so treat that as nil.
		if err != nil && err != windows.ERROR_SUCCESS {
			return 0, err
		}

		// No console is a normal state, not an error.
		return 0, nil
	}

	return hwnd, nil
}

func hasRealConsole() bool {
	hwnd, err := getConsoleWindow()
	if err != nil {
		return false
	}
	return hwnd != 0
}

func installCtrlHandlerIfConsole() {
	if !hasRealConsole() {
		return
	} else {
		logf("Installing Ctrl+C handler due to console.")
	}
	procSetConsoleCtrlHandler.Call(ctrlHandler, 1) // this doesn't work(ie. has no console) for: go build -mod=vendor -ldflags="-H=windowsgui" .
}

func todo() {
	panic("TODO: not yet implemented")
}

func unreachable() {
	panic("unreachable code was reached, bad assumptions or programmer then ;p")
}

//	func exitErrorf(format string, a ...interface{}) {
//		panic(fmt.Errorf(format, a...))
//	}
type exitStatus struct {
	Code    int
	Message string
}

// exitf allows you to provide a code and a formatted message
func exitf(code int, format string, a ...interface{}) {
	deinit()
	panic(exitStatus{
		Code:    code,
		Message: fmt.Sprintf(format, a...),
	})
}

// XXX: in here, return errors like 'return fmt.Errorf("something went wrong")' instead of using log.Fatal or os.Exit(1)
func runApplication(_token theILockedMainThreadToken) error { //XXX: must be called on main() and after that runtime.LockOSThread()
	_ = _token // silence warning!
	assertStructSizes()
	logf("Started")

	if writeProfile {
		// In main(), before the GetMessage loop:
		f, err := os.Create("cpu.prof")
		if err != nil {
			logf("Failed to create CPU profile: %v", err)
			// or exitf if critical
		} else {
			if err := pprof.StartCPUProfile(f); err != nil {
				logf("StartCPUProfile failed: %v", err)
				f.Close()
			} else {
				// Defer stop/write — put this in your main defer block
				defer func() {
					pprof.StopCPUProfile()
					f.Close()
					logf("CPU profile written to cpu.prof")
				}()
			}
		}
	}

	initDPIAwareness() //If you call it after window creation, it does nothing.

	//cb := windows.NewCallback(mouseProc)
	mouseCallback = windows.NewCallback(mouseProc)
	h, _, err := procSetWindowsHookEx.Call(WH_MOUSE_LL, mouseCallback, 0, 0)
	if h == 0 {
		//return
		//logf("Got error: %v", err) // has no console!
		//os.Exit(1)
		// exit(1)
		// exitErrorf()
		return fmt.Errorf("Got error: %v", err)
	} else {
		mouseHook = windows.Handle(h)
		defer procUnhookWindowsHookEx.Call(uintptr(mouseHook))
	}

	kbdCB := windows.NewCallback(keyboardProc)
	hk, _, err := procSetWindowsHookEx.Call(
		WH_KEYBOARD_LL,
		kbdCB,
		0,
		0,
	)
	if hk == 0 {
		//logf("Got error: %v", err) // has no console!
		return fmt.Errorf("Got error: %v", err)
	} else {
		kbdHook = windows.Handle(hk)
		defer procUnhookWindowsHookEx.Call(uintptr(kbdHook))
	}

	// shellH, _, err := procSetWindowsHookEx.Call(
	// 	5, // WH_SHELL
	// 	windows.NewCallback(shellProc),
	// 	0, 0,
	// )
	// if shellH != 0 {
	// 	shellHook = windows.Handle(shellH)
	// 	defer procUnhookWindowsHookEx.Call(uintptr(shellHook))
	// } else {
	// 	//XXX: "WH_SHELL hook failed: Cannot set nonlocal hook without a module handle." - apparently needs to be done via a .dll, gg Grok /s
	// 	logf("WH_SHELL hook failed: %v", err)
	// }

	// Global foreground change hook, this is the WH_SHELL hook, changed tho to accomodate needs.
	h, _, err = procSetWinEventHook.Call(
		0x0003, // EVENT_SYSTEM_FOREGROUND min
		//0x0003, // max
		0x8005, // EVENT_OBJECT_FOCUS (Catch lower-level focus shifts)
		0,      // hmod = 0 (out-of-context callback)
		winEventCallback,
		0,             // idProcess = 0 (all)
		0,             // idThread = 0 (all)
		0x0000|0x0002, // WINEVENT_OUTOFCONTEXT | WINEVENT_SKIPOWNPROCESS
	)
	if h == 0 {
		logf("SetWinEventHook failed: %v", err)
	} else {
		winEventHook = windows.Handle(h)
		defer procUnhookWinEvent.Call(uintptr(winEventHook))
	}

	hwnd, err := createMessageWindow() //TODO: how to undo this via defer or something?!
	if err != nil {
		//exitf(1, "Failed to create message window: %v", err)
		return fmt.Errorf("Failed to create message window: %v", err)
	}
	initTray(hwnd)

	mainThreadId = windows.GetCurrentThreadId() // Set the global for the hook
	var msg MSG
	for {
		/* GetMessage is the "Event-Driven" king.
		   It puts this thread to sleep at 0% CPU.
		   It only wakes up if:
		   1. A real Windows message (Key, Exit, Window Move) arrives.
		   2. Our Hook sends the WM_WAKE_UP "Doorbell".
		*/
		r, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		/*
					Why not handle WM_WAKE_UP in wndProc?

			This is a technical nuance of Win32: When we use PostThreadMessage, the message is sent to the thread, but it doesn't have a window handle (hwnd is 0).

			    DispatchMessage only sends messages to a wndProc if they have a valid hwnd.

			    If hwnd is 0, DispatchMessage does nothing.

			    Therefore, you must catch WM_WAKE_UP directly in the GetMessage loop before it hits the dispatcher.
		*/
		// Catch the Doorbell before DispatchMessage sees it
		if msg.Message == WM_DO_SETWINDOWPOS {
			drainMoveChannel() // Pull everything from the channel
			continue           // Skip Dispatching this custom message
		}
		// Handle System Tray / Window Messages
		// This ensures your wndProc gets called!
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
	return nil // no error
}

// Separate function to keep the loop readable
func drainMoveChannel() {
	for {
		// Track High-Water Mark
		currentFill := int64(len(moveDataChan))
		if currentFill > maxChannelFillForMoveEvents.Load() {
			//TODO: recheck the logic in this when using more than 1 thread (currently only 1)
			maxChannelFillForMoveEvents.Store(currentFill)
			logf("New Channel Peak: %d events queued (Dropped: %d)",
				currentFill, droppedMoveEvents.Load())
		}

		select {
		case data := <-moveDataChan:
			// Use the data (the struct copy) to move the window.
			// No heap pointers, no garbage collector stress!
			handleActualMove(data) // Move the window
		default:
			return // Channel empty, go back to GetMessage
		}
	}
}

var (
	// ... your existing procs ...
	procGetWindowText       = user32.NewProc("GetWindowTextW")
	procGetWindowTextLength = user32.NewProc("GetWindowTextLengthW")

	procCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32First           = kernel32.NewProc("Process32FirstW")
	procProcess32Next            = kernel32.NewProc("Process32NextW")
)

func getWindowText(hwnd windows.Handle) string {
	ret, _, _ := procGetWindowTextLength.Call(uintptr(hwnd))
	if ret == 0 {
		return ""
	}
	buf := make([]uint16, ret+1)
	procGetWindowText.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), ret+1)
	return windows.UTF16ToString(buf)
}

func getProcessName(pid uint32) string {
	// TH32CS_SNAPPROCESS = 0x00000002
	snapshot, _, _ := procCreateToolhelp32Snapshot.Call(0x00000002, 0)
	if snapshot == uintptr(windows.InvalidHandle) {
		return "unknown"
	}
	defer windows.CloseHandle(windows.Handle(snapshot))

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	ret, _, _ := procProcess32First.Call(snapshot, uintptr(unsafe.Pointer(&entry)))
	for ret != 0 {
		if entry.ProcessID == pid {
			return windows.UTF16ToString(entry.ExeFile[:])
		}
		ret, _, _ = procProcess32Next.Call(snapshot, uintptr(unsafe.Pointer(&entry)))
	}
	return "not found"
}

var procGetClassName = user32.NewProc("GetClassNameW")

func getClassName(hwnd windows.Handle) string {
	buf := make([]uint16, 256)
	ret, _, _ := procGetClassName.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if ret == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:ret])
}

var shouldLogFocusChanges = false

func winEventProc(hWinEventHook windows.Handle, event uint32, hwnd windows.Handle, idObject int32, idChild int32, dwEventThread uint32, dwmsEventTime uint32) uintptr {
	//fmt.Println("DEBUG: hook called")

	var eventName string

	switch event {
	case 0x0003:
		eventName = "EVENT_SYSTEM_FOREGROUND"
	case 0x0008:
		eventName = "EVENT_SYSTEM_MENUSTART"
	case 0x0009:
		eventName = "EVENT_SYSTEM_MENUEND"
	case 0x8000:
		eventName = "EVENT_OBJECT_CREATE"
		return 0
	case 0x8001:
		eventName = "EVENT_OBJECT_DESTROY"
		return 0
	case 0x8002:
		eventName = "EVENT_OBJECT_SHOW"
	case 0x8003:
		eventName = "EVENT_OBJECT_HIDE"
	case 0x8004:
		eventName = "EVENT_OBJECT_REORDER"
	case 0x8005:
		eventName = "EVENT_OBJECT_FOCUS"
	default:
		// Return early if it's an event we aren't tracking to keep logs clean
		return 0
	}

	// Get the top-level owner of this HWND to see if it belongs to CMD
	// GA_ROOT (2) gets the "real" parent window
	rootHwnd, _, _ := procGetAncestor.Call(uintptr(hwnd), 2)

	var pid uint32
	procGetWindowThreadProcessId.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&pid)))

	title := getWindowText(windows.Handle(rootHwnd))
	procName := getProcessName(pid)
	class := getClassName(hwnd)

	if shouldLogFocusChanges {
		logf("[%s] HWND=0x%x (Root=0x%x) objId=%d childId=%d [%s] Class=[%s] PID=%d (%s)",
			eventName, hwnd, rootHwnd, idObject, idChild, title, class, pid, procName)
	}

	if event == 0x0003 { // EVENT_SYSTEM_FOREGROUND
		if shouldLogFocusChanges {
			logf("Foreground changed to hwnd=0x%x", hwnd)
		}

		// Optional: Check for elevated
		var pid uint32
		procGetWindowThreadProcessId.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&pid)))
		il, err := processIntegrityLevel(pid)
		if err == nil && il >= 0x3000 {
			if shouldLogFocusChanges {
				logf("Elevated foreground (IL=0x%x) → reconciling state", il)
			}
			//hardResetIfDesynced() // your recovery function, TODO:
			// Or force suppression if Win held, etc.
		} else {
			if shouldLogFocusChanges {
				//logf("Err: %v, IL=0x%x", err, il)
				logf("PID=%d IL=0x%x err=%v", pid, il, err)
				//logf("Foreground hwnd=0x%x PID=%d bufLen=%d subCount=%d RID=0x%x err=%v", hwnd, pid, len(buf), subCount, rid, err)
			}
		}
		//} else {
		//	logf("event: %v hwnd=0x%x", event, hwnd)
	}
	return 0 // WinEvent callbacks return 0 (no chaining)
}
