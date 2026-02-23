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
	"strconv"

	"golang.org/x/sys/windows"

	"sync/atomic"
	"time"
	"unsafe"
)

// this init() must be first, order of it in source code matters as they're executed in order of seen.
func init() {
	// Force the runtime to provide exactly 3 execution contexts: logWorker, main msg loop (wndProc), and hooks!
	// regardless of what the user set in their Environment Variables.
	runtime.GOMAXPROCS(3)
	/*
			While you can technically call it in main(), putting it in init() ensures the scheduler is reconfigured before any other code
			(including third-party library init functions) has a chance to start spawning background goroutines.

			Why init() is the "Correct" Place

		    Env Var Override: It guarantees that even if a user starts your program with GOMAXPROCS=64, your code immediately scales it back to 2 before the "real" work begins.

		    Predictability: The Go runtime's garbage collector and scheduler are sensitive to the number of available processors (P).
			Setting this early prevents the runtime from briefly trying to use more threads than you intend during the startup phase.
			- gemini 3 fast
	*/
	/*
							The Verdict for your Win32 App

						Setting GOMAXPROCS(2) is the "sweet spot" because:

						    Safety: It ensures your Win32 Hook thread always has a "seat" at the table.

						    Efficiency: It prevents the GC from spawning 8 or 16 workers (on a high-core machine) that would compete for cache and context switches,
							which can actually cause "stuttering" in UI applications.
							To understand how the Garbage Collector (GC) behaves when you set GOMAXPROCS(2), we have to look at the difference between Application Parallelism
							and Runtime Background Tasks.

				1. Does it affect the GC?

				Yes, but in a way that protects your performance. The GC in Go is "concurrent." This means it tries to do most of its work while your program is still running.
				It uses the number of Ps (Processors) defined by GOMAXPROCS to decide how many workers it can spawn to scan memory.

				    With GOMAXPROCS(2): The GC will generally try to use a fraction of those 2 slots (usually 25%) for background marking.

				    The "Worker Stealing": If your logWorker is idle, the GC will "steal" that CPU time to clean up memory. If both your Main Thread and logWorker are 100% busy,
					the GC might briefly "assist" by taking a tiny bit of time from one of them to ensure it doesn't run out of memory.

				2. Does it "Pause Everything" (Stop the World)?

				Go's GC is not a traditional "Stop the World" (STW) collector like older versions of Java. It is a Low-Latency Collector.

				    It does have two extremely short STW phases (measured in microseconds, not milliseconds).

				    During these tiny windows, yes, it pauses both your Win32 thread and your logWorker.

				    However, because these pauses are so short (often < 100µs), they are usually invisible to Win32 message loops and won't cause your UI or hooks to "lag" or drop messages.

				3. Can the GC run on its own threads?

				This is the "trick" of the Go runtime: The GC can spawn as many OS threads (Ms) as it needs, but it can only execute on as many Processors (Ps) as you allowed.

				    If you set GOMAXPROCS(2), the Go scheduler says: "At any given instant, only 2 threads are allowed to be actively crunching numbers."

				    If the GC needs to do a background task, it will wait for one of your 2 slots to become "available" (e.g., when your logWorker is waiting for a file write
					or your Win32 loop is waiting for GetMessage).

					Summary of the "Thread Landscape"

		When you run your program with GOMAXPROCS(2) and LockOSThread(), your OS Thread list will look roughly like this:
		Thread Type	Count	Behavior
		Main Thread (Locked)	1	Runs your Win32 Loop. Uses 1 "slot" (P).
		Worker Thread	1	Runs your logWorker. Uses the 2nd "slot" (P).
		GC / Runtime Threads	1-3	Mostly "sleeping" or waiting to "steal" a slot when the Worker is idle.
		Sysmon Thread	1	A tiny background thread that monitors the network and deadlocks (doesn't use a P slot).

					- gemini 3 fast
	*/
}

/* ---------------- DLLs & Procs ---------------- */
var procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")

// var shellHook windows.Handle
var (
	// The Data Pipe (2048 is plenty for lag spikes)
	moveDataChan = make(chan WindowMoveData, 2048)

	// Modern Atomic tracking
	droppedMoveEvents           atomic.Uint64
	droppedLogEvents            atomic.Uint64
	maxChannelFillForMoveEvents atomic.Uint64 // To track how "full" it got
	maxChannelFillForLogEvents  atomic.Uint64 // To track how "full" it got

)

func init() {
	maxChannelFillForMoveEvents.Store(1) // avoid the first message: New Channel Peak: 1 events queued (Dropped: 0)
}

var (
	procSetWinEventHook = user32.NewProc("SetWinEventHook")
	procUnhookWinEvent  = user32.NewProc("UnhookWinEvent")

	winEventHook     windows.Handle
	winEventCallback = windows.NewCallback(winEventProc)
)

var (
	moveCounter     uint64    // how many move events we saw since last log
	lastRateLogTime time.Time // when we last printed the rate
	rateLogInterval = 1 * time.Second
)
var actualPostCounter uint64

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

	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procSetWindowsHookEx    = user32.NewProc("SetWindowsHookExW")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procGetMessage          = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessage     = user32.NewProc("DispatchMessageW")

	procGetAsyncKeyState    = user32.NewProc("GetAsyncKeyState")
	procWindowFromPoint     = user32.NewProc("WindowFromPoint")
	procGetAncestor         = user32.NewProc("GetAncestor")
	procReleaseCapture      = user32.NewProc("ReleaseCapture") // Releases mouse capture if any window has it
	procSendMessage         = user32.NewProc("SendMessageW")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")

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

	procPostMessage       = user32.NewProc("PostMessageW")
	procPostThreadMessage = user32.NewProc("PostThreadMessageW")

	procGetLastError = kernel32.NewProc("GetLastError")

	procSendInput = user32.NewProc("SendInput")
	procLoadIcon  = user32.NewProc("LoadIconW")

	procUnregisterClassW = user32.NewProc("UnregisterClassW")

	procSetPriorityClass  = kernel32.NewProc("SetPriorityClass")
	procGetPriorityClass  = kernel32.NewProc("GetPriorityClass")
	procGetCurrentProcess = kernel32.NewProc("GetCurrentProcess")
	procGetCurrentThread  = kernel32.NewProc("GetCurrentThread")
	procSetThreadPriority = kernel32.NewProc("SetThreadPriority")
	procGetThreadPriority = kernel32.NewProc("GetThreadPriority")

	procSetProcessInformation    = kernel32.NewProc("SetProcessInformation")
	procSetProcessWorkingSetSize = kernel32.NewProc("SetProcessWorkingSetSize")
)

/* ---------------- Constants ---------------- */

const (
	WS_EX_LAYERED     = 0x00080000
	WS_EX_TRANSPARENT = 0x00000020
	LWA_COLORKEY      = 0x00000001
	LWA_ALPHA         = 0x00000002
)

var (
	procSetLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")
	procBeginPaint                 = user32.NewProc("BeginPaint")
	procEndPaint                   = user32.NewProc("EndPaint")
	procDrawText                   = user32.NewProc("DrawTextW")
	procFillRect                   = user32.NewProc("FillRect")

	gdi32                   = windows.NewLazySystemDLL("gdi32.dll")
	procGdiSetTextColor     = gdi32.NewProc("SetTextColor")
	procGdiSetBkMode        = gdi32.NewProc("SetBkMode")
	procGdiCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	procGdiDeleteObject     = gdi32.NewProc("DeleteObject")

	overlayHwnd windows.Handle
	overlayText string

	// Reusable GDI brushes
	magentaBrush windows.Handle
	blackBrush   windows.Handle
)

type PAINTSTRUCT struct {
	Hdc         windows.Handle
	FErase      int32
	RcPaint     RECT
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

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
	WM_RBUTTONDOWN = 0x0204 //guessed
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

	SWP_NOSIZE         = 0x0001
	SWP_NOMOVE         = 0x0002
	SWP_NOZORDER       = 0x0004
	SWP_NOACTIVATE     = 0x0010
	SWP_ASYNCWINDOWPOS = 0x4000
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
	WM_MYSYSTRAY = WM_USER + 2
	//WM_WAKE_UP           = WM_USER + 3
	WM_INJECT_SEQUENCE             = WM_USER + 100
	WM_FOCUS_TARGET_WINDOW_SOMEHOW = WM_USER + 101
	WM_EXIT_VIA_CTRL_C             = WM_USER + 150
	WM_DO_SETWINDOWPOS             = WM_USER + 200 // arbitrary, just unique
)
const (
	MENU_EXIT                         = 1
	MENU_USE_LMB_TO_FOCUS_AS_FALLBACK = 2
	MENU_ACTIVATE_MOVE                = 3
	MENU_RATELIMIT_MOVES              = 4
	MENU_LOG_RATE_OF_MOVES            = 5

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

const (
	ZONE_TOP_LEFT = iota
	ZONE_TOP_CENTER
	ZONE_TOP_RIGHT
	ZONE_MID_LEFT
	ZONE_CENTER
	ZONE_MID_RIGHT
	ZONE_BOT_LEFT
	ZONE_BOT_CENTER
	ZONE_BOT_RIGHT
)

/* ---------------- Types ---------------- */
var (
	resizing           bool
	resizeZone         int
	initialAspectRatio float64
	respectAspectRatio bool = true // Default value for your toggle
)

//TODO: reorder these (I've 'var' and 'type' in this block)

type WindowMoveData struct {
	Hwnd        windows.Handle // Target window
	X           int32          // New X (full 32-bit)
	Y           int32          // New Y
	W, H        int32          //width, height for resize via winkey+RMBdrag
	InsertAfter windows.Handle // ← this one: HWND_TOP, HWND_BOTTOM, etc.
	Flags       uint32         // Optional: extra SWP_ flags
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
	knownMinW int32
	knownMinH int32
}

type NOTIFYICONDATA struct {
	CbSize            uint32
	HWnd              windows.Handle // hold handle to my hidden message window aka self.
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

	// used exclusively to know when to inject shift key tap (shiftdown+shiftUP) at the point when physical winkeyUP aka winUP is detected //TODO: maybe remove because we do it(shifttap) now at gesture start
	winGestureUsed bool = false //false initially
)

var (
	mouseHook windows.Handle
	kbdHook   windows.Handle

	//"the app is effectively single-threaded for these vars (pinned thread, serialized hooks/message loop), so no concurrency risks."- grok expert
	capturing bool
	//currently or previously dragged window HWND, helps with state after doing winkey+L then unlocking session while dragging was in progress.
	targetWnd   windows.Handle
	currentDrag *dragState

	trayIcon NOTIFYICONDATA
)

var focusOnDrag bool                // whether or not to (also)focus dragged window
var doLMBClick2FocusAsFallback bool // if normal(thread attach) focus fails, then do the LMB click on the window to focus it(caveat: can click inside it eg. on its buttons!)
var ratelimitOnMove bool            // use less CPU (see CPU time in task manager) but it's choppier and subconsciously no fun!
var shouldLogDragRate bool          // but only when ratelimitOnMove is true

/* ---------------- Utilities ---------------- */

func getResizeZone(pt POINT, r RECT) int {
	w := r.Right - r.Left
	h := r.Bottom - r.Top

	col := 0
	if pt.X > r.Left+(2*w/3) {
		col = 2
	} else if pt.X > r.Left+(w/3) {
		col = 1
	}

	row := 0
	if pt.Y > r.Top+(2*h/3) {
		row = 2
	} else if pt.Y > r.Top+(h/3) {
		row = 1
	}

	return row*3 + col
}

const minimumW = 300
const minimumH = 300

func calculateResize(drag *dragState, zone int, currentPt POINT) (x, y, w, h int32) {
	dx := currentPt.X - drag.startPt.X
	dy := currentPt.Y - drag.startPt.Y

	origL := drag.startRect.Left
	origT := drag.startRect.Top
	origR := drag.startRect.Right
	origB := drag.startRect.Bottom
	origW := origR - origL
	origH := origB - origT

	// Use the dynamically discovered minimums!
	minW := drag.knownMinW
	minH := drag.knownMinH

	if zone == ZONE_CENTER {
		// UNIFORM CENTER RESIZE
		var dw, dh int32

		if respectAspectRatio {
			if initialAspectRatio >= 1.0 {
				dw = dx * 2
				dh = int32(float64(dw) / initialAspectRatio)
			} else {
				dh = dy * 2
				dw = int32(float64(dh) * initialAspectRatio)
			}
		} else {
			dw = dx * 2
			dh = dy * 2
		}

		w = origW + dw
		h = origH + dh

		// Apply safety minimums, maintaining aspect ratio if we hit the floor
		if w < minW {
			w = minW
			if respectAspectRatio && initialAspectRatio > 0 {
				h = int32(float64(w) / initialAspectRatio)
			}
		}
		if h < minH {
			h = minH
			if respectAspectRatio && initialAspectRatio > 0 {
				w = int32(float64(h) * initialAspectRatio)
			}
		}
		// Second pass for absolute safety
		if w < minW {
			w = minW
		}
		if h < minH {
			h = minH
		}

		x = origL + (origW-w)/2
		y = origT + (origH-h)/2
	} else {
		// 8-GRID EDGE/CORNER RESIZE
		newL, newT, newR, newB := origL, origT, origR, origB

		switch zone {
		case ZONE_TOP_LEFT:
			newL += dx
			newT += dy
		case ZONE_TOP_CENTER:
			newT += dy
		case ZONE_TOP_RIGHT:
			newT += dy
			newR += dx
		case ZONE_MID_LEFT:
			newL += dx
		case ZONE_MID_RIGHT:
			newR += dx
		case ZONE_BOT_LEFT:
			newL += dx
			newB += dy
		case ZONE_BOT_CENTER:
			newB += dy
		case ZONE_BOT_RIGHT:
			newR += dx
			newB += dy
		}

		// Strictly enforce dynamic minimums
		if zone == ZONE_TOP_LEFT || zone == ZONE_MID_LEFT || zone == ZONE_BOT_LEFT {
			if newR-newL < minW {
				newL = newR - minW
			}
		}
		if zone == ZONE_TOP_RIGHT || zone == ZONE_MID_RIGHT || zone == ZONE_BOT_RIGHT {
			if newR-newL < minW {
				newR = newL + minW
			}
		}
		if zone == ZONE_TOP_LEFT || zone == ZONE_TOP_CENTER || zone == ZONE_TOP_RIGHT {
			if newB-newT < minH {
				newT = newB - minH
			}
		}
		if zone == ZONE_BOT_LEFT || zone == ZONE_BOT_CENTER || zone == ZONE_BOT_RIGHT {
			if newB-newT < minH {
				newB = newT + minH
			}
		}

		x, y = newL, newT
		w, h = newR-newL, newB-newT
	}

	return x, y, w, h
}

// this way when winUP happens it won't pop up start menu
func injectShiftTapOnly() {
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
	}

	ret, _, err := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)
	if ret == 0 {
		logf("SendInput for injectShiftTapOnly failed: %v", err)
		//} else {
		//	logf("done injectShiftTapOnly")
	}
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
		//} else {
		//	logf("done injectShiftTapThenWinUp")
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
		return 0, fmt.Errorf("OpenProcess failed: %w", err)
	}
	defer windows.CloseHandle(hProc)

	var token windows.Token
	err = windows.OpenProcessToken(hProc, windows.TOKEN_QUERY, &token)
	if err != nil {
		return 0, fmt.Errorf("OpenProcessToken failed: %w", err)
	}
	defer token.Close()

	var needed uint32
	windows.GetTokenInformation(token, windows.TokenIntegrityLevel, nil, 0, &needed)

	buf := make([]byte, needed)
	err = windows.GetTokenInformation(token, windows.TokenIntegrityLevel, &buf[0], needed, &needed)
	if err != nil {
		return 0, fmt.Errorf("GetTokenInformation failed: %w", err)
	}

	// Debug: log buffer size (should be ~28-40 bytes)
	//logf("Integrity buf len=%d for PID %d", len(buf), pid)

	// TOKEN_MANDATORY_LABEL header is 16 bytes on 64-bit (pointer + attributes + padding)
	const headerSize = 16
	lenb := len(buf)
	if lenb < headerSize+8 { // + min SID header
		return 0, fmt.Errorf("buffer too small: %s", humanBytes(uint64(lenb)))
	}

	// SID starts after header
	//sidBase := uintptr(unsafe.Pointer(&buf[headerSize]))

	// SID fixed header: Revision (1) + SubAuthorityCount (1) + IdentifierAuthority (6) = offset 8 for SubAuthority array
	//subCountPtr := (*uint8)(unsafe.Pointer(sidBase + 1)) // SubAuthorityCount at offset 1
	//subCountPtr := (*uint8)(unsafe.Pointer(uintptr(unsafe.Pointer(&buf[headerSize])) + 1))
	subCountPtr := (*uint8)(unsafe.Add(unsafe.Pointer(&buf[headerSize]), 1))
	subCount := *subCountPtr
	if subCount == 0 {
		return 0, fmt.Errorf("invalid subauthority count: 0")
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

func initTray() error {
	hwnd, err := createMessageWindow() //TODO: how to undo this via defer or something?!
	if err != nil {
		//exitf(1, "Failed to create message window: %v", err)
		return fmt.Errorf("failed to create message window: %w", err)
	}

	trayIcon.HWnd = hwnd //FIXME: need to put this in a diff. variable so it doesn't depend on systray being inited! since it's used in other things!
	trayIcon.CbSize = uint32(unsafe.Sizeof(trayIcon))
	trayIcon.UID = 1
	trayIcon.UFlags = NIF_TIP | NIF_ICON | NIF_MESSAGE

	const IDI_APPLICATION = 32512

	hIcon, _, _ := procLoadIcon.Call(0, IDI_APPLICATION)
	trayIcon.HIcon = windows.Handle(hIcon)
	trayIcon.UCallbackMessage = WM_MYSYSTRAY
	trayIcon.UTimeoutOrVersion = NOTIFYICON_VERSION_4

	copy(trayIcon.SzTip[:], windows.StringToUTF16("winbollocks")) //TODO: make const

	//1
	ret1, _, err1 := procShellNotifyIcon.Call(NIM_ADD, uintptr(unsafe.Pointer(&trayIcon)))
	if ret1 == 0 {
		logf("Failed to add tray icon (real error): '%v' (code %d)", err1, err1)
		// You could exitf or fallback here, but for now just log
	}

	//2, this must happen after NIM_ADD ! (bad chatgpt which suggested it before NIM_ADD)
	ret2, _, err2 := procShellNotifyIcon.Call(NIM_SETVERSION, uintptr(unsafe.Pointer(&trayIcon)))
	if ret2 == 0 {
		logf("NIM_SETVERSION for tray icon failed(are you on pre Windows Vista 2007?): '%v' (code %d)", err2, err2)
		// You could exitf or fallback here, but for now just log
	}

	return nil
}

const WM_DESTROY = 0x0002

/* The WM_DESTROY Breakdown
   Constant Value: 0x0002

   What triggers it: It is sent by the system to a window after the window has been removed from the screen, but before the child windows are destroyed.
   Specifically, calling procDestroyWindow.Call(hwnd) is what triggers the WM_DESTROY message to be sent to that hwnd's wndProc.

   The Flow: User clicks Exit (or Hook panics) → WM_CLOSE → DestroyWindow() → WM_DESTROY → PostQuitMessage().
*/

func cleanupTray() {
	if trayIcon.HWnd == 0 {
		// Never initialized or window creation failed — nothing to clean
		return
	}

	savedHwnd := trayIcon.HWnd
	// Use the same trayIcon struct from initTray
	trayIcon.UFlags = 0 // NIM_DELETE ignores most fields, but set to be safe
	ret, _, err := procShellNotifyIcon.Call(NIM_DELETE, uintptr(unsafe.Pointer(&trayIcon)))
	//ret is non-zero (success), but err can still be set
	if ret == 0 {
		logf("Failed to delete tray icon: %v", err) // optional, for debug
	} else {
		// Zero out the struct to avoid reuse confusion
		trayIcon = NOTIFYICONDATA{}
	}

	//yeah this has to be after NIM_DELETE, according to Gemini 3 Thinking
	ret, _, err = procDestroyWindow.Call(uintptr(savedHwnd))
	if ret == 0 {
		logf("DestroyWindow failed of HWND=0x%X: %v (probably already destroyed or invalid)", savedHwnd, err)
	}
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

	currentDrag = &dragState{startPt: pt, startRect: r}
}

func startDrag(hwnd windows.Handle, pt POINT) {
	//logf("startDrag")
	if isMaximized(hwnd) {
		//windows.ShowWindow(hwnd, windows.SW_RESTORE)
		procShowWindow.Call(uintptr(hwnd), SW_RESTORE)
		//TODO: should I re-maximize if it was maximized, after drag/move is done?
	}

	pid := getWindowPID(hwnd)
	targetIL, e1 := processIntegrityLevel(pid)
	//selfIL, e2 := processIntegrityLevel(uint32(windows.GetCurrentProcessId())) //bugged it said, it noticed.
	// #nosec G115 --- it's actually uint32 wrapped in an int
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

	startManualDrag(hwnd, pt)
}

func keyDown(vk uintptr) bool {
	state, _, _ := procGetAsyncKeyState.Call(vk)
	return state&0x8000 != 0
}

func softReset(releaseCapture bool) { //nevermindTODO: use hardReset instead(well no, because it also resets winGestureUsed!) because it now handles the case when Shift tap needs to be inserted if winGestureUsed !
	//do this first
	capturing = false
	resizing = false
	//do this second
	targetWnd = 0

	currentDrag = nil

	/*
		The Problem: If you call it in the hook, you are releasing capture on the Hook Thread. But window capture is thread-specific.
		If your SetCapture was originally called by the Main Thread (which is usually where windows and UI live),
		calling ReleaseCapture from the Hook Thread might not work the way you expect, or could lead to an inconsistent state where the OS
		thinks Thread A has it but Thread B tried to kill it.

		actually it is my hook thread that calls SetCapture in 2 places one for move and one for resize!
	*/
	if releaseCapture {
		procReleaseCapture.Call()
	}

	hideOverlay() //FIXME: move this to wndProc ! else u hit stutter7 occasionally!
}

func hardReset(releaseCapture bool) {
	var winDown bool = keyDown(VK_LWIN) || keyDown(VK_RWIN)
	if winGestureUsed && winDown {
		injectShiftTapOnly() // this way when winUP happens it won't pop up start menu
		//TODO: inject shift tap at the time gesture is detected!
		winGestureUsed = false
	}
	softReset(releaseCapture)
}

func initOverlay() {
	className := mustUTF16("winbollocksResizingOverlay") //TODO: see if underscores work in this!

	var wc WNDCLASSEX
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	wc.LpfnWndProc = windows.NewCallback(overlayWndProc)
	wc.LpszClassName = className
	hinst, _, _ := procGetModuleHandle.Call(0)
	wc.HInstance = windows.Handle(hinst)
	// Add shadow/background if desired, but we'll paint it

	procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))

	hwndRaw, _, _ := procCreateWindowEx.Call(
		WS_EX_LAYERED|WS_EX_TRANSPARENT|WS_EX_TOOLWINDOW|WS_EX_TOPMOST,
		uintptr(unsafe.Pointer(className)),
		0,
		WS_POPUP,
		0, 0, 400, 100, // Size will be updated dynamically
		0, 0,
		uintptr(wc.HInstance),
		0,
	)

	overlayHwnd = windows.Handle(hwndRaw)

	// Set Magenta (0x00FF00FF) as the transparent color key, and 200/255 opacity for the rest
	procSetLayeredWindowAttributes.Call(uintptr(overlayHwnd), 0x00FF00FF, 220, LWA_COLORKEY|LWA_ALPHA)

	// Create our reusable GDI brushes once
	hMag, _, _ := procGdiCreateSolidBrush.Call(0x00FF00FF)
	magentaBrush = windows.Handle(hMag)

	hBlk, _, _ := procGdiCreateSolidBrush.Call(0x00000000)
	blackBrush = windows.Handle(hBlk)
}

const WM_PAINT = 0x000F

func overlayWndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_PAINT:
		// var ps PAINTSTRUCT
		// hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

		// // 1. Fill background with Magenta (Transparent Key)
		// hBrush, _, _ := procGdiCreateSolidBrush.Call(0x00FF00FF)
		// var rect RECT
		// procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rect)))
		// rect.Right -= rect.Left
		// rect.Left = 0
		// rect.Bottom -= rect.Top
		// rect.Top = 0
		// procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rect)), hBrush)
		// procGdiDeleteObject.Call(hBrush)

		// // 2. Draw black text box background for visibility
		// bgBrush, _, _ := procGdiCreateSolidBrush.Call(0x00000000) // Black
		// procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rect)), bgBrush)
		// procGdiDeleteObject.Call(bgBrush)

		// // 3. Draw Text
		// procGdiSetTextColor.Call(hdc, 0x0000FF00) // Green text
		// procGdiSetBkMode.Call(hdc, 1)             // TRANSPARENT background for text

		// textPtr := mustUTF16(overlayText)
		// procDrawText.Call(hdc, uintptr(unsafe.Pointer(textPtr)), ^uintptr(0), uintptr(unsafe.Pointer(&rect)), 0x24) // DT_CENTER | DT_VCENTER | DT_SINGLELINE

		// procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		// return 0
		var ps PAINTSTRUCT
		hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

		var rect RECT
		procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rect)))
		rect.Right -= rect.Left
		rect.Left = 0
		rect.Bottom -= rect.Top
		rect.Top = 0

		// 1. Fill background with our global Magenta brush (Transparent Key)
		procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rect)), uintptr(magentaBrush))

		// 2. Draw black text box background for visibility with our global Black brush
		procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rect)), uintptr(blackBrush))

		// 3. Draw Text
		procGdiSetTextColor.Call(hdc, 0x0000FF00) // Green text
		procGdiSetBkMode.Call(hdc, 1)             // TRANSPARENT background for text

		textPtr := mustUTF16(overlayText)
		procDrawText.Call(hdc, uintptr(unsafe.Pointer(textPtr)), ^uintptr(0), uintptr(unsafe.Pointer(&rect)), 0x24) // DT_CENTER | DT_VCENTER | DT_SINGLELINE

		procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	}
	ret, _, _ := procDefWindowProc.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

func updateOverlay(x, y, w, h int32, startW, startH int32) {
	if overlayHwnd == 0 {
		return
	}

	diffW := w - startW
	diffH := h - startH
	overlayText = fmt.Sprintf("Size: %dx%d (delta: %d, %d)", w, h, diffW, diffH)

	// Center the overlay over the window being resized
	ox := x + (w / 2) - 150
	oy := y + (h / 2) - 25

	procSetWindowPos.Call(
		uintptr(overlayHwnd),
		HWND_TOPMOST,
		uintptr(ox), uintptr(oy),
		300, 50,
		SWP_NOACTIVATE|0x0040, // SWP_SHOWWINDOW
	)

	// Force redraw
	user32.NewProc("InvalidateRect").Call(uintptr(overlayHwnd), 0, 1)
}

const SW_HIDE = 0

func hideOverlay() {
	if overlayHwnd != 0 {
		procShowWindow.Call(uintptr(overlayHwnd), SW_HIDE)
	}
}

func isWindowForeground(hwnd windows.Handle) bool {
	if hwnd == 0 {
		logf("!! attempted to check the focus of a windows with handle 0")
		return false
	}
	fg, _, _ := procGetForegroundWindow.Call()
	return windows.Handle(fg) == hwnd
}

// aka in window in my own process?
func isOwnWindow(hwnd windows.Handle) bool {
	if hwnd == 0 {
		return false
	}

	var pid uint32
	r1, _, _ := procGetWindowThreadProcessId.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(&pid)),
	)
	if r1 == 0 {
		return false
	}

	return pid == windows.GetCurrentProcessId()
}

// FIXME: make these two funcs be one and return two bools: (samePID, sameTID) and sameTID would be false if samePID is false!

// is window in the same thread ID as the caller thread ID (could still be two diff. processes tho!)
func isInSameThreadID(hwnd windows.Handle) bool {
	var pid uint32
	tid, _, _ := procGetWindowThreadProcessId.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(&pid)),
	)
	if tid == 0 {
		return false
	}
	return uint32(tid) == windows.GetCurrentThreadId()
}

func focusThisHwnd(target windows.Handle) (gotFocused bool) {
	fgRet, _, fgErr := procSetForegroundWindow.Call(uintptr(target))
	if fgRet != 1 {
		lastErr := windows.GetLastError()
		// ie. not "SetForegroundWindow ret=1 err=The operation completed successfully."
		//XXX: you get ret=0 with "err=The operation completed successfully." when Start menu was already open
		logf("failed SetForegroundWindow ret=%d err='%v' lastErr:'%v'", fgRet, fgErr, lastErr)
		return false
	} else { // ie. 1 is TRUE
		return true
	}
}

const (
	WS_CHILD         = 0x40000000
	WS_POPUP         = 0x80000000
	WS_EX_TOOLWINDOW = 0x00000080
	WS_EX_NOACTIVATE = 0x08000000
)
const WS_EX_TOPMOST = 0x00000008
const (
	GWL_STYLE   = -16
	GWL_EXSTYLE = -20
)

var procGetWindowLongPtrW = user32.NewProc("GetWindowLongPtrW")
var modkernel32 = windows.NewLazySystemDLL("kernel32.dll")
var procSetLastError = modkernel32.NewProc("SetLastError")

func getWindowLongPtr(hwnd windows.Handle, index int32) (uintptr, error) {
	if hwnd == 0 {
		return 0, fmt.Errorf("getWindowLongPtr: hwnd is 0")
	}

	// Clear last error so we can detect real failure
	//windows.SetLastError(0)
	// Clear last error so we can detect real failure
	procSetLastError.Call(0)
	//windows.SetLastError(0)

	ret, _, err := procGetWindowLongPtrW.Call(
		uintptr(hwnd),
		uintptr(index),
	)
	_ = err // silence linter
	//Do NOT trust the third return from .Call
	//You did the right thing ignoring it. For many Win32 APIs it is unreliable.

	// Important edge case:
	// GetWindowLongPtr can legally return 0 even on success.
	// The only reliable failure signal is GetLastError.
	if ret == 0 {
		// windows.GetLastError() is safer than trusting err blindly
		lastErr := windows.GetLastError()
		//if lastErr != windows.ERROR_SUCCESS {
		if !errors.Is(lastErr, windows.ERROR_SUCCESS) {
			//return 0, fmt.Errorf("GetWindowLongPtrW failed: %w", lastErr)
			//nolint:wrapcheck
			return 0, lastErr
		}
	}

	return ret, nil
}

func shouldSkipFocusingIt(hwnd windows.Handle) (ret bool, reason string) {
	ret = true
	if hwnd == 0 {
		reason = "hwnd is 0"
		return
	}

	// 2. Read styles
	// style, _, _ := procGetWindowLongPtr.Call(uintptr(hwnd), uintptr(GWL_STYLE))
	// exStyle, _, _ := procGetWindowLongPtr.Call(uintptr(hwnd), uintptr(GWL_EXSTYLE))
	style, err := getWindowLongPtr(hwnd, GWL_STYLE)
	if err != nil {
		logf("GetWindowLongPtr STYLE failed: %v", err)
		reason = "GetWindowLongPtr STYLE failed"
		return
	}

	exStyle, err := getWindowLongPtr(hwnd, GWL_EXSTYLE)
	if err != nil {
		logf("GetWindowLongPtr EXSTYLE failed: %v", err)
		reason = "GetWindowLongPtr EXSTYLE failed"
		return
	}

	s := uint32(style)
	ex := uint32(exStyle)

	// Child windows cannot be foreground windows
	if s&WS_CHILD != 0 {
		reason = "is child"
		return
	}

	// Tool windows are often menus/popups
	if ex&WS_EX_TOOLWINDOW != 0 {
		reason = "is tool window"
		return
	}

	// Explicit no-activate → DO NOT TOUCH
	if ex&WS_EX_NOACTIVATE != 0 {
		reason = "is no WS_EX_NOACTIVATE"
		return
	}

	ret = false
	reason = "shouldn't skip"
	return
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
	{
		b, reason := shouldSkipFocusingIt(target)
		if b {
			logf("shouldSkipFocusingIt for HWND 0x%X because %s", target, reason)
			return true //pretend it's focused
		}

		// 1. Our own process → skip
		if isOwnWindow(target) {
			//don't try to focus self, it will fail to attach
			//logf("ignoring attempt to focus own window(s), pretending it's already focused(to avoid the LMB click to focus it workaround next)")
			// Same process → AttachThreadInput is unnecessary and sometimes harmful

			if isInSameThreadID(target) {
				logf("attempting to focus own window in same thread, sure.")
				//this will make the systray popup menu disappear and spam these: SetWindowPos failed(from within main message loop): hwnd=0x802d6 error=0
				// unless we skip tool windows above!
				fgRet, _, fgErr := procSetForegroundWindow.Call(uintptr(target))
				if fgRet != 1 {
					lastErr := windows.GetLastError()
					// ie. not "SetForegroundWindow ret=1 err=The operation completed successfully."
					//XXX: you get ret=0 with "err=The operation completed successfully." when Start menu was already open
					logf("failed to SetForegroundWindow for own window in same thread(w/o thread attach) ret=%d err='%v' lastErr:'%v'", fgRet, fgErr, lastErr)
					return false
				} else {
					return true
				}
				//focusThisHwnd(target)
			} else {
				//reason = "is own window on diff. thread which might have own msg. loop"
				logf("attempting to focus own window, but it's on a diff. thread in own process, will pretend it's focused(to avoid the LMB click to focus it workaround next) without actually focusing it tho.")
				return true
			}
			//unreachable()
		}
	} // a block

	// if isOwnWindow(target) {
	// 	//don't try to focus self, it will fail to attach
	// 	//logf("ignoring attempt to focus own window(s), pretending it's already focused(to avoid the LMB click to focus it workaround next)")
	// 	// Same process → AttachThreadInput is unnecessary and sometimes harmful

	// 	if isInSameThreadID(target) {
	// 		logf("attempting to focus own window, sure.")
	// 		////procSetForegroundWindow.Call(uintptr(target))
	// 		// fgRet, _, fgErr := procSetForegroundWindow.Call(uintptr(target))
	// 		// if fgRet != 1 {
	// 		// 	lastErr := windows.GetLastError()
	// 		// 	// ie. not "SetForegroundWindow ret=1 err=The operation completed successfully."
	// 		// 	//XXX: you get ret=0 with "err=The operation completed successfully." when Start menu was already open
	// 		// 	logf("SetForegroundWindow ret=%d err=%v lastErr:%v", fgRet, fgErr, lastErr)
	// 		// }
	// 		focusThisHwnd(target)
	// 	} else {
	// 		logf("attempting to focus own window, but it's on a diff. thread in own process, will pretend it's focused(to avoid the LMB click to focus it workaround next) without actually focusing it tho.")
	// 	}
	// 	return true // so it doesn't try the LMB click way next.
	// }

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

	succeeded := focusThisHwnd(target)

	procAttachThreadInput.Call(uintptr(curTid), uintptr(targetThreadId), uintptr(0)) // Detach always

	return succeeded //fgRet != 0
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
func mouseProc(nCode int, wParam, lParam uintptr) uintptr {
	// Start a timer for the hook itself
	start := time.Now()
	// Standard Win32 Hook practice: If nCode < 0, we must pass it
	// to the next hook immediately and stay out of the way.
	if nCode < 0 {
		ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		if time.Since(start) > 5*time.Millisecond {
			logf("stutter1")
		}
		return ret
	}

	// nolint:govet //for unsafeptr
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
		if time.Since(start) > 5*time.Millisecond {
			logf("stutter2")
		}
		return ret
	}

	switch wParam {
	case WM_LBUTTONDOWN: //LMB pressed aka LMBDown or LMB DOWN
		// we don't want to trigger our drag gesture if shift/alt/ctrl was held before winkey, because it might have different meaning to other apps.
		var winDown bool = keyDown(VK_LWIN) || keyDown(VK_RWIN)
		var shiftDown bool = keyDown(VK_SHIFT)
		var ctrlDown bool = keyDown(VK_CONTROL)
		var altDown bool = keyDown(VK_MENU)
		if winDown && !shiftDown && !altDown && !ctrlDown { // only if winkey without any modifiers
			if !winGestureUsed { //wasn't set already
				winGestureUsed = true // we used at least once of our gestures
				injectShiftTapOnly()  // prevent releasing of winkey later from popping up Start menu!
			}

			//var start bool = true // do we start the drag on the window under mouse?

			wantTargetWnd := windowFromPoint(info.Pt)
			if wantTargetWnd == 0 {
				logf("Invalid window, window-move gesture skipped but LMB eaten and start menu will still be prevented(now even if you LMB on a higher integrity eg. admin window before you release winkey)")
				return 1 // swallow LMB
			}

			if capturing {
				//XXX: happens when winkey+LMB then winkey+L to lock, release all, unlock, (now if u move mouse it no longer drags but)
				// if you now start to hold winkey(it will drag if you move mouse) and then press(or hold) LMB (you're here) and
				// move mouse while LMB is held it continues to drag/move that same window.
				logf("already capturing, means you were moving a window pressed winkey+L released all then unlocked session then held winkey(again) " +
					"and pressed(or held) LMB (on same or new window target!) thus you're now here.")
				//In Go, when you use the + operator with string literals (text wrapped in double quotes), the compiler performs constant folding.
				// This means it joins them together while building your binary, so there is zero performance penalty at runtime.

				if targetWnd == 0 {
					//start = false
					panic("impossible state(while single-threaded win32 app in 20feb2026), logic error: you were 'capturing' " +
						"but targetWnd wasn't set to anything(ie. it's 0) but shoulda been set to prev. window! even softReset does capturing=0 then targetWnd=0 first!")
				} else { // non zero targetWnd
					//capturing means you already were dragging a prev. window, reflected by targetWnd not being 0!

					//now, is it a new window you're trying to drag or the same old one?
					// if it's same old one, the dragging is still thought to happen (if winkey is held down anew before moving mouse, else you'd not be here), so don't start a new drag?
					// if it's new, have to softReset() first because otherwise it will still drag the old one! and let it start drag again?

					if targetWnd == wantTargetWnd {
						//same old window
						logf("continuing to drag-move same old window HWND=0x%X from the same old initial coords(ie. you'll see a snap-move first!)", targetWnd)
						//FIXME: should probably use the new mouse coords for this drag, meaning softReset() this variant too and let it start anew(like the below new window one)
						//start = false
						return 1 //swallow LMB
					} else {
						//a new window
						// it's a drag of a new window but we were moving the old window before that and didn't stop (for winkey+L reason for example!)
						logf("Avoided moving the old window HWND=0x%X ie. you were moving a window while winkey+L happened, now you unlocked session and you're newly holding winkey "+
							"but you LMB-ed on ANOTHER window(ie. trying to move another window), so we're not gonna move the old window anymore but the new one!", targetWnd)

						logf("drag-moving new window HWND=0x%X instead of the old one HWND=0x%X", wantTargetWnd, targetWnd)
						softReset(true)
						//start = true
					}
				}
				//start = true
				// } else {
				// 	start = true
			}

			targetWnd = wantTargetWnd // never 0 if we're here!

			// //check prev. dragged window, is it same as current?
			// if targetWnd != wantTargetWnd {
			// 	//we're here because either this is first drag ever or it's a drag of a different/new window!
			// 	if capturing && targetWnd != 0 {
			// 		// it's a drag of a new window but we were moving the old window before that and didn't stop (for winkey+L reason for example!)
			// 		logf("Avoided moving the old window HWND=0x%X ie. you were moving a window while winkey+L happened, now you unlocked session and you're newly holding winkey "+
			// 			"but you LMB-ed on ANOTHER window(ie. trying to move another window), so we're not gonna move the old window anymore but the new one!", targetWnd)
			// 		softReset()
			// 		start = true
			// 	} else {
			// 		// you were not capturing already and
			// 		if targetWnd == 0 {
			// 			//start = false
			// 			panic("impossible state, logic error: you were 'capturing' but targetWnd wasn't set to anything(ie. it's 0) but shoulda been set to prev. window! even softReset does capturing=0 then targetWnd=0 first!")
			// 		}
			// 		// so you're here because you're not capturing already and you had a target before but now's a new one! so need to start dragging!
			// 		start = true
			// 	}
			// 	targetWnd = wantTargetWnd
			// 	logf("drag-moving new window HWND=0x%X", targetWnd)
			// } else {
			// 	logf("continuing to drag-move same old window HWND=0x%X", targetWnd)
			// }

			//if start {
			//FIXME: so we start the drag before doing the focus, works but seems off this way, not visually tho! but might be needed so we can setcapture to self else target might have/set capture(unsure)!
			startDrag(targetWnd, info.Pt)

			//FIXME: find out when to set this to true, vs when (above) checking it, might need to be atomic to avoid a theoretical race, at least in theory if say i do winkey+LMB which checks capturing then before setting it to true here somehow winkey+L and unlock and winkey+LMB happens again!
			capturing = true //FIXME: this probably needs to be atomic if we go multi-threaded later(it's not m.t. yet) like if we make hooks go on their own thread!
			if focusOnDrag && !isWindowForeground(targetWnd) {
				procPostMessage.Call(
					uintptr(trayIcon.HWnd),
					WM_FOCUS_TARGET_WINDOW_SOMEHOW,
					0, // no args to that function
					0,
				)
			}
			//}
			if time.Since(start) > 5*time.Millisecond {
				logf("stutter8")
			}
			return 1 // swallow LMB
		}

	case WM_MOUSEMOVE:
		if capturing && currentDrag != nil {
			//var stopDrag bool = false
			// //FIXME: LMB is swallowed during our gesture move, even tho it would be down physically! so can't use async state! and in case of Winkey+L the LMB UP event is never seen by us, thus we don't know if LMB is UP physically when session is unlocked!
			// var isLMBstillDown bool = keyDown(VK_LBUTTON)
			// if !isLMBstillDown {
			// 	logf("LMB is no longer held down, stopping drag")
			// 	stopDrag = true
			// }
			var winDown bool = keyDown(VK_LWIN) || keyDown(VK_RWIN)
			if !winDown {
				logf("winkey is no longer down, stopping drag")
				//nevermindTODO: make systray option to keep dragging even if winkey's no longer down(bad idea for winkey+L case, see todo.txt about it), once initiated. But this means the edge case with Winkey+L (search for it above) can happen! unless i check if LMB is still down in async state here hmmm... actually i can't do it due to winkey+L and because we eat LMB Down so async state cannot be used to check it!
				//stopDrag = true
				hardReset(true) //XXX: resets gesture used which means doesn't prevent a winUP from popping start menu, this is correct because we detected winkey as being UP here!

				break //exit case/switch!
			}
			// if stopDrag {
			// 	logf("stopping drag due to above, resetting state.")
			// 	hardReset()//resets gesture used which means doesn't prevent a winUP from popping start menu
			// 	break
			// }

			if time.Since(lastResize) >= forceMoveOrResizeActionsToBeThisManyMSApart*time.Millisecond {
				// At the very beginning of the drag/move logic (e.g., right after checking if dragging is active)
				var now time.Time
				if ratelimitOnMove {
					now = time.Now()
					// Count every potential move (even if we skip due to debounce)
					moveCounter++
					//FIXME: should allow logging even if rate limiting isn't enabled.
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
						logf("Drag move rate: %s potential / %s actual moves in %.2fs → %.1f / %.1f per sec",
							withCommas(moveCounter), withCommas(actualPostCounter), secondsElapsed,
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
						Flags:       SWP_NOSIZE | SWP_NOACTIVATE | SWP_NOZORDER | SWP_ASYNCWINDOWPOS,
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
						//procPostThreadMessage.Call(uintptr(mainThreadId), WM_DO_SETWINDOWPOS, 0, 0)
						//the reason we use PostMessage and not PostThreadMessage here is because while systray menu popup is open it runs its own msg loop and calls my wndProc so it will ignore all of these doorbells until popup is closed if i use postThreadMessage!
						r, _, err := procPostMessage.Call(uintptr(trayIcon.HWnd), WM_DO_SETWINDOWPOS, 0, 0)
						if r == 0 {
							logf("PostMessage of WM_DO_SETWINDOWPOS for WM_MOUSEMOVE failed: %v", err)
						}

					default:
						// FAIL: The channel (2048 slots) is completely full.
						// This happens if the Main Thread is frozen (e.g., Admin console lag).
						// We MUST NOT block here, or we will freeze the user's entire mouse cursor.
						// We just increment our "shame counter" and move on.
						droppedMoveEvents.Add(1) //TODO: use diff. one to keep track of drops due to channel full
					}

					if ratelimitOnMove {
						lastMovePostedTime = now
						lastPostedX = newX
						lastPostedY = newY
					}
					//return 0 //0 = let it thru
					//XXX: let it fall thru so CallNextHookEx is also called!
				} // willPostMessage
			} //>=10ms
		} //main 'if', for capturing aka moving/dragging window

		if resizing && currentDrag != nil {
			if time.Since(lastResize) >= forceMoveOrResizeActionsToBeThisManyMSApart*time.Millisecond {
				nx, ny, nw, nh := calculateResize(currentDrag, resizeZone, info.Pt) //TODO: move this into wndProc aka into handleActualMove() ?!

				// Send to your mover channel
				moveDataChan <- WindowMoveData{
					Hwnd:  targetWnd,
					X:     nx,
					Y:     ny,
					W:     nw,
					H:     nh,
					Flags: SWP_NOZORDER | SWP_NOACTIVATE, //| SWP_ASYNCWINDOWPOS, // no good atm because shrink doesn't work only grow
				}
				// Trigger the move window
				procPostMessage.Call(uintptr(trayIcon.HWnd), WM_DO_SETWINDOWPOS, 0, 0)
			} //>=10ms
			//XXX: let it fall thru so the move isn't eaten.
		} //second 'if', for resizing

	case WM_LBUTTONUP: //LMB released aka LMBUP aka LMB UP
		if capturing && currentDrag != nil {
			//logf("was manual-capturing, now resetting state and releasing mouse capture")
			// capturing = false
			// currentDrag = nil
			// targetWnd = 0
			// procReleaseCapture.Call()
			softReset(true) // this means that when winkey goes UP it will make sure from keyboardProc that start menu doesn't pop up!

			//return 0 //0 is to let it thru (1 was to swallow)
			//XXX: let it fall thru so CallNextHookEx is also called!

			//actually we can't let it thru because LMB Down was eaten, so if LMBUP is allowed then when u move say firefox's Help popup menu while hovering on About it will open About as if just clicked because it triggers on LMBUp!
			return 1 //eat it
		} // else let it pass
		if capturing || currentDrag != nil {
			panic("race detected2, or at best improper cleanup")
		}

	case WM_RBUTTONUP: //RMB released aka RMBUP aka RMB UP
		if resizing && currentDrag != nil {
			softReset(true)
			if time.Since(start) > 5*time.Millisecond {
				logf("stutter7") // FIXME: hitting only this one! yep it's hideOverlay(), do it in wndProc heh!
			}
			return 1 // Swallow
		}
		if resizing || currentDrag != nil {
			panic("race detected1, or at best improper cleanup")
		}

	case WM_RBUTTONDOWN: //RMB pressed aka RMBDown aka RMBdrag
		var winDown bool = keyDown(VK_LWIN) || keyDown(VK_RWIN)
		var shiftDown bool = keyDown(VK_SHIFT)
		var ctrlDown bool = keyDown(VK_CONTROL)
		var altDown bool = keyDown(VK_MENU)
		if winDown && !shiftDown && !altDown && !ctrlDown { // only if winkey without any modifiers
			if !winGestureUsed {
				winGestureUsed = true
				injectShiftTapOnly() // prevent releasing of winkey later from popping up Start menu!
			}
			// if winGestureUsed {
			// 	logf("you're already using another gesture") //FIXME: this can happen for other reasons as well, winkey+L ?
			// 	return 1                                     //eat key
			// }
			if resizing {
				logf("already resizing") //FIXME: like for 'capturing'
			}
			if currentDrag != nil {
				//FIXME: what to do here.
				logf("didn't clean up last resize/drag gesture")
				return 1
			}
			targetWnd = windowFromPoint(info.Pt)
			if targetWnd != 0 {

				resizing = true

				var r RECT
				procGetWindowRect.Call(uintptr(targetWnd), uintptr(unsafe.Pointer(&r)))

				currentDrag = &dragState{startPt: info.Pt, startRect: r,
					knownMinW: minimumW, // Start with your 300px default
					knownMinH: minimumH}
				resizeZone = getResizeZone(info.Pt, r)

				w := float64(r.Right - r.Left)
				h := float64(r.Bottom - r.Top)
				initialAspectRatio = w / h

				procSetCapture.Call(uintptr(trayIcon.HWnd))
				if time.Since(start) > 5*time.Millisecond {
					logf("stutter6")
				}
				return 1 // Swallow
			}
		} //if

	case WM_MBUTTONDOWN: //MMB pressed
		var winDown bool = keyDown(VK_LWIN) || keyDown(VK_RWIN)
		var shiftDown bool = keyDown(VK_SHIFT)
		var ctrlDown bool = keyDown(VK_CONTROL)
		var altDown bool = keyDown(VK_MENU)

		if winDown && !ctrlDown && !altDown {
			//winDOWN and MMB pressed without ctrl/alt but maybe or not shiftDOWN too, it's a gesture of ours:
			if !winGestureUsed { //wasn't set already
				winGestureUsed = true // we used at least once of our gestures
				injectShiftTapOnly()  // prevent releasing of winkey later from popping up Start menu!
			}

			if time.Since(lastResize) >= forceMoveOrResizeActionsToBeThisManyMSApart*time.Millisecond {
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
						//procPostThreadMessage.Call(uintptr(mainThreadId), WM_DO_SETWINDOWPOS, 0, 0)
						r, _, err := procPostMessage.Call(uintptr(trayIcon.HWnd), WM_DO_SETWINDOWPOS, 0, 0)
						if r == 0 {
							logf("PostMessage of WM_DO_SETWINDOWPOS for MMB failed: %v", err)
						}

					default:
						// FAIL: The channel (2048 slots) is completely full.
						// This happens if the Main Thread is frozen (e.g., Admin console lag).
						// We MUST NOT block here, or we will freeze the user's entire mouse cursor.
						// We just increment our "shame counter" and move on.
						droppedMoveEvents.Add(1)
					}
				}
			} // if every 10ms or more

			if time.Since(start) > 5*time.Millisecond {
				logf("stutter5")
			}
			return 1 // swallow MMB
		} // the 'if' in MMB
	} //switch

	if time.Since(start) > 5*time.Millisecond {
		logf("stutter3")
	}
	// Always pass the event down the chain so other apps don't break
	ret, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
	if time.Since(start) > 5*time.Millisecond {
		logf("stutter4")
	}
	return ret
}

/* ---------------- Main ---------------- */

func createMessageWindow() (windows.Handle, error) {
	if curThreadID := windows.GetCurrentThreadId(); mainThreadID != curThreadID {
		exitf(1, "unexpected: main loop thread and wndProc are on different threads mainThreadID: %d, curThreadID: %d", mainThreadID, curThreadID)
	}
	className, err := windows.UTF16PtrFromString("winbollocksHidden")
	if err != nil {
		return 0, fmt.Errorf("UTF16PtrFromString failed for class name: %w", err)
	}

	var wc WNDCLASSEX
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	wc.LpfnWndProc = wndProc
	wc.LpszClassName = className
	hinst, _, _ := procGetModuleHandle.Call(0) // "If this parameter is NULL, GetModuleHandle returns a handle to the file used to create the calling process (.exe file)."
	wc.HInstance = windows.Handle(hinst)

	procSetLastError.Call(0)
	// Register class — check return value
	ret, _, err := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))
	if ret == 0 {
		lastErr := windows.GetLastError()
		return 0, fmt.Errorf("RegisterClassEx failed: %v (error code: %w)", err, lastErr) //XXX: multiple %w is illegal and will panic.
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
		return 0, fmt.Errorf("CreateWindowEx failed: %v (error code: %w)", err, lastErr)
	}

	return windows.Handle(hwndRaw), nil
}

var (
	hookThreadId, mainThreadID uint32
	// Optional: prepare a mutex for later when we secure 'currentDrag'
	// dragStateMutex sync.RWMutex
)
var hookPanicPayload atomic.Value // We use atomic for thread-safety
var mainAcknowledgedShutdown = make(chan struct{})

func hookWorker() {
	// 1. Lock this goroutine to a single, dedicated OS thread. Crucial!
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Run this last to catch any secondary panics
	defer secondary_defer() //this runs second but only if first doesn't os.exit ie. it fails/panics!
	//defer primary_defer() //this runs first, can't run this here due to needing to be on same thread as main to deinit other things!
	// defer time.Sleep(2 * time.Second)
	// defer procPostQuitMessage.Call(0) //FIXME: no effect even with 2 sec delay after it!

	// The Cross-Thread Panic Bridge
	defer func() {
		if r := recover(); r != nil {
			// 1. Store the panic payload so main can read it
			hookPanicPayload.Store(r)

			if status, ok := r.(exitStatus); ok {
				currentExitCode = status.Code
				// This was an intentional exit(code)
				//if code != 0 {
				logf("hookWorker thread intentionally exited with code: '%d' and error message: '%s'", currentExitCode, status.Message)
				//}
			} else {
				currentExitCode = 1 //FIXME: this is accessed from two diff. threads, protect it.
				stack := debug.Stack()
				logf("--- hookWorker thread CRASH: %v ---\nStack: %s\n--- END---", r, stack)
			}
			logf("CRITICAL: from hookWorker, signaling main thread to die...")

			// 2. Nuke the main thread's GetMessage loop, works only if systray popup menu isn't open!
			// Use PostThreadMessage to mainThreadId, or post WM_CLOSE to your main HWND
			procPostThreadMessage.Call(uintptr(mainThreadID), WM_QUIT, 0, 0)
			//doneFIXME: what if main is dead too, and would ignore the signal or what, then we exit here? sure after X seconds

			if trayIcon.HWnd != 0 {
				// Post to the Window Handle, NOT the Thread ID.
				// This cuts through modal menus like the systray popup menu!
				procPostMessage.Call(uintptr(trayIcon.HWnd), WM_CLOSE, 0, 0)
			}
			/* When you right-click your tray icon and the menu appears, the code is stuck inside the TrackPopupMenu Win32 call.
				That function runs its own private message loop.
			   The Problem: It looks for mouse clicks and keyboard hits. If it sees a message with HWND == NULL (which is what PostThreadMessage creates),
			   it often just throws it away. Your main loop never gets to see it.
			*/

			const waitForMainSeconds = 2
			// 2. The Watchdog Timer
			select {
			case <-mainAcknowledgedShutdown:
				//logf("Main acknowledged panic. Handing over control...")
				logf("hookWorker is now waiting %d seconds for main to exit us...", waitForMainSeconds)
				// We wait here forever. Why? Because we want the main thread's
				// deinit() to be the one that finishes and potentially waits for
				// the user's "Press a key or Enter" keypress.
				select {}

			case <-time.After(waitForMainSeconds * time.Second):
				//logf("Main thread UNRESPONSIVE after 2s. Emergency exit.")
				logf("hookWorker done waiting for main to die, proceeding to secondary_defer which exits...")
				// Main is frozen. If we don't exit now, the app hangs forever.
				//we let it continue which means it calls secondary_defer() next!
			}
		} //if recover
		logf("hookWorker clean exit (but not quitting thread)")
		select {} //infinite wait, or else secondary_defer() will trigger, FIXME: find a better way to not os.Exit and still exit this thread. liek tell secondary_defer to not os.Exit via a global bool?!
	}() // defer

	// 2. Save the OS Thread ID so our main thread can talk to it later
	hookThreadId = windows.GetCurrentThreadId()
	if mainThreadID == hookThreadId { //FIXME: temp, should be == here!
		exitf(1, "main loop msg and hooks are NOT on two different threads, this will never happen unless code logic is broken!")
	}
	logf("Hook worker thread started. ThreadID: %d", hookThreadId)

	setAndVerifyPriority()

	// 3. INSTALL HOOKS HERE
	mouseCallback = windows.NewCallback(mouseProc)
	h, _, err := procSetWindowsHookEx.Call(WH_MOUSE_LL, mouseCallback, 0, 0)
	if h == 0 {
		exitf(1, "Got error: %v", err)
		unreachable()
	} else {
		mouseHook = windows.Handle(h)
		defer func() {
			procUnhookWindowsHookEx.Call(uintptr(mouseHook))
			mouseHook = 0
			logf("unhooked mouseHook")
		}()
	}

	kbdCB := windows.NewCallback(keyboardProc)
	hk, _, err := procSetWindowsHookEx.Call(
		WH_KEYBOARD_LL,
		kbdCB,
		0,
		0,
	)
	if hk == 0 {
		exitf(1, "Got error: %v", err)
		unreachable()
	} else {
		kbdHook = windows.Handle(hk)
		defer func() {
			procUnhookWindowsHookEx.Call(uintptr(kbdHook))
			kbdHook = 0
			logf("unhooked kbdHook")
		}()
	}

	// 4. The Thread's Private Message Loop
	var msg MSG
	for {
		ret, _, _ := procGetMessage.Call(
			uintptr(unsafe.Pointer(&msg)),
			0, 0, 0,
		)

		// ret == 0 means WM_QUIT was received. ret == -1 aka ^uintptr(0) is an error.
		if ret == 0 || ret == ^uintptr(0) {
			break
		}

		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}

	logf("Hook worker thread received WM_QUIT or error, exiting and unhooking...")
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

var lastResize time.Time

const forceMoveOrResizeActionsToBeThisManyMSApart = 10

func handleActualMoveOrResize(data WindowMoveData) {
	// 1. RATE LIMIT: Don't hit the OS more than once every 10-16ms (approx 60-100Hz)
	// Most monitors are 60Hz-144Hz. Anything faster than 10ms is wasted CPU.
	if time.Since(lastResize) < forceMoveOrResizeActionsToBeThisManyMSApart*time.Millisecond {
		//logf("ignored move/resize")
		droppedMoveEvents.Add(1)
		return
	}

	defer func() {
		lastResize = time.Now()
	}()

	target := data.Hwnd
	// if resizing {
	// 	//actually we could be done resizing and still get resize things or move things from the queue due to delays.
	// 	//so this is no good to check.
	// 	if data.Flags&SWP_NOSIZE != 0 {
	// 		//inconsistent state.
	// 		panic("bad coding, you passed SWP_NOSIZE while attempting to resize!")
	// 	} else {
	// 		//it's a resize, consistent.
	// 	}
	// }
	// //FIXME: remove this 'if' later
	// if (data.W != 0 || data.H != 0) && data.Flags&SWP_NOSIZE == SWP_NOSIZE {
	// 	//flags |= SWP_NOSIZE
	// 	panic("bad coding, you passed SWP_NOSIZE while attempting to resize!")
	// }
	ret, _, _ := procSetWindowPos.Call(
		uintptr(target),
		uintptr(data.InsertAfter),
		uintptr(data.X), uintptr(data.Y),
		uintptr(data.W), uintptr(data.H),
		uintptr(data.Flags),
	)

	if ret == 0 {
		errCode, _, _ := procGetLastError.Call()
		logf("SetWindowPos failed(from within main message loop): hwnd=0x%x error=%d", target, errCode)
		if errCode == 5 { // Access denied (UIPI likely)
			showTrayInfo("winbollocks", "Cannot move/resize elevated window (access denied), you'd have to run as admin.")
		}
		// // Optional: fallback to native drag simulation (simulates title-bar drag, often works when SetWindowPos is blocked) - grok
		// pt := POINT{X: x, Y: y}
		// lParamNative := uintptr(pt.Y)<<16 | uintptr(pt.X)
		// procPostMessage.Call(uintptr(target), WM_NCLBUTTONDOWN, HTCAPTION, lParamNative)
	} else if data.W != 0 || data.H != 0 {
		if currentDrag != nil {
			if !resizing {
				logf("delayed resizing detected, while not 'resizing'.")
			}
			// --- THE ANTI-SLIDE REALITY CHECK --- (gemini 3.1 pro)
			// We asked the OS to resize it to data.W x data.H. Did it listen?
			var r RECT
			ret, _, err := procGetWindowRect.Call(uintptr(target), uintptr(unsafe.Pointer(&r)))
			if ret == 0 {
				// Optional: Get the specific system error
				errCode, _, _ := procGetLastError.Call()
				logf("GetWindowRect during resize failed: hwnd=0x%x, errCode=%d, err:%v", target, errCode, err)
				// Safety: If we can't get the Rect, we can't do Anti-Slide or Overlay updates safely.
				return
			}
			actualW := r.Right - r.Left
			actualH := r.Bottom - r.Top
			nx, ny, nw, nh := data.X, data.Y, data.W, data.H

			clamped := false
			// If actual width is larger than what we requested AND larger than our currently known minimum
			if actualW > data.W && actualW > currentDrag.knownMinW {
				currentDrag.knownMinW = actualW
				clamped = true
			}
			if actualH > data.H && actualH > currentDrag.knownMinH {
				currentDrag.knownMinH = actualH
				clamped = true
			}

			// If the OS clamped the size, the X/Y we sent caused the opposite edge to slide!
			// We must immediately correct the window position to restore the anchor.
			if clamped {
				var pt POINT
				procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))) // Get latest mouse position

				nx, ny, nw, nh = calculateResize(currentDrag, resizeZone, pt)

				// Snap the window back to the correct anchor point
				procSetWindowPos.Call(
					uintptr(target),
					uintptr(data.InsertAfter),
					uintptr(nx), uintptr(ny),
					uintptr(nw), uintptr(nh),
					uintptr(data.Flags),
				)
			}

			//if data.Flags&SWP_NOSIZE == 0 { // actually in this 'else if' block we know we're in resize mode
			//	//lacks SWP_NOSIZE so it's a resize!
			// ---- OVERLAY UPDATE ----
			startW := currentDrag.startRect.Right - currentDrag.startRect.Left
			startH := currentDrag.startRect.Bottom - currentDrag.startRect.Top
			updateOverlay(nx, ny, nw, nh, startW, startH)
			//}
			// ------------------------
		}
	} //else
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

var wndProc = windows.NewCallback(func(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_DO_SETWINDOWPOS:
		drainMoveChannel() // Pull everything from the channel
		return 0           // Handled

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

	case WM_INJECT_SEQUENCE:
		//avoids injecting from the hook
		which := uint16(wParam)        // ie. uint16(vk))
		injectShiftTapThenWinUp(which) // it's correct casting, as per AI.
		return 0
	case WM_FOCUS_TARGET_WINDOW_SOMEHOW:
		//this is here because avoids focusing window or injecting LMB from the hook
		if !forceForeground(targetWnd) {
			var extra string
			if doLMBClick2FocusAsFallback {
				extra = "; next, falling back to injected LMB click which, unfortunately, means here that it will click at the point in the window where u tried to move it which eg. in total commander might be on the exit button and it will exit!"
			} else {
				extra = "."
			}
			logf("Failed to force foreground(ie. to activate/focus window) this happens consistently when Start menu was already open(ie. press and release winkey once)%s", extra)

			if doLMBClick2FocusAsFallback {
				//logf("injecting LMB click")
				// injecting a LMB_down then LMB_up so that the target window gets a click to focus and bring it to front
				// this is a good workaround for focusing it which windows wouldn't allow via procSetForegroundWindow (unless attaching to target window's thread!)
				injectLMBClick()
			}
			// // Non-attachment focus: Simulate safe click to focus, doesn't work due to focus stealing prevention (win11) and thus only flashes the taskbar button of the target window. Actually the flashing is due to the above focus try(via attach thread first) failing! This may or may not do it alone, unsure.
			// ret, _, err := procPostMessage.Call(uintptr(targetWnd), WM_LBUTTONDOWN, 1, makeLParam(10, 10)) // MK_LBUTTON = 1, safe pos
			// logf("Post WM_LBUTTONDOWN for focus ret=%d err=%v", ret, err)
			// ret, _, err = procPostMessage.Call(uintptr(targetWnd), WM_LBUTTONUP, 0, makeLParam(10, 10)) // Release to avoid hold
			// logf("Post WM_LBUTTONUP for focus ret=%d err=%v", ret, err)
		}
		return 0
	case WM_MYSYSTRAY:

		// Strip high word to get the low 16-bit message code
		low := uint32(lParam & 0xFFFF)

		// if low != WM_MOUSEMOVE { // any non-mouse_move(0x10200 on v4) events:
		// 	logf("WM_TRAY received with lParam %x, %x", lParam, low)
		// }

		//if ((lParam & 0x0FFFF) == WM_RBUTTONUP) || ((lParam & 0x0FFFF) == WM_CONTEXTMENU) {
		if low == WM_RBUTTONUP { // RMB on systray aka RMBUp or RMBUP on systray aka RMB button released
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

			focusText := mustUTF16("Activate window when moved if not in focus (uses thread-attaching focus method).")
			doLMBClick2FocusAsFallbackText := mustUTF16("Fallback: Use Left Mouse Click to focus (Warning: will click underlying UI elements).")
			ratelimitText := mustUTF16("Rate-limit window moves(by 5x, uses less CPU but looks choppier so ur subconscious will hate it)")
			sldrText := mustUTF16("Log rate of moves(only if rate-limit above is enabled)")

			exitText := mustUTF16("Exit")

			var actFlags uintptr = MF_STRING // untyped constants can auto-convert, but not untyped vars(in the below call)
			if focusOnDrag {
				actFlags |= MF_CHECKED
			}

			procAppendMenu.Call(hMenu, actFlags, MENU_ACTIVATE_MOVE,
				uintptr(unsafe.Pointer(focusText)))

			var lmbFlags uintptr = MF_STRING
			if doLMBClick2FocusAsFallback {
				lmbFlags |= MF_CHECKED
			}
			if !focusOnDrag {
				lmbFlags |= MF_DISABLED | MF_GRAYED
			}
			procAppendMenu.Call(hMenu, lmbFlags, MENU_USE_LMB_TO_FOCUS_AS_FALLBACK,
				uintptr(unsafe.Pointer(doLMBClick2FocusAsFallbackText)))

			var rlFlags uintptr = MF_STRING
			if ratelimitOnMove {
				rlFlags |= MF_CHECKED
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
			case MENU_ACTIVATE_MOVE:
				focusOnDrag = !focusOnDrag
			case MENU_USE_LMB_TO_FOCUS_AS_FALLBACK:
				doLMBClick2FocusAsFallback = !doLMBClick2FocusAsFallback
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

	case WM_CLOSE:
		//exit(0)
		//WM_CLOSE → DestroyWindow() → WM_DESTROY → PostQuitMessage() -> getmessage() -> break loop -> outside of loop continuation...
		procDestroyWindow.Call(uintptr(hwnd))
		return 0
	case WM_DESTROY:
		procPostQuitMessage.Call(0)
		return 0
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
	} //switch

	//let the default window proc handle the rest:
	ret, _, _ := procDefWindowProc.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
})

const WM_QUIT = 0x0012

func deinit() {
	deinitThreadId := windows.GetCurrentThreadId()
	hardReset(false)
	if hookThreadId != 0 {
		// Send WM_QUIT (0x0012) directly to the hook thread's message queue
		procPostThreadMessage.Call(uintptr(hookThreadId), WM_QUIT, 0, 0)
	}
	if deinitThreadId == hookThreadId {
		//XXX:The rule is: The thread that calls SetWindowsHookEx MUST be the thread that calls UnhookWindowsHookEx.
		//noFIXME: won't the above run the below as defer-ers and thus race ? actually can I even unhook those from this diff. thread?! it won't because those are in defer-ers too, so deferers are serialized.
		if mouseHook != 0 {
			procUnhookWindowsHookEx.Call(uintptr(mouseHook))
			mouseHook = 0
			logf("cleaned mouseHook from deinit()")
		}
		if kbdHook != 0 {
			procUnhookWindowsHookEx.Call(uintptr(kbdHook))
			kbdHook = 0
			logf("cleaned kbdHook from deinit()")
		}
	}

	cleanupTray()

	if overlayHwnd != 0 {
		// Destroy the overlay window
		procDestroyWindow.Call(uintptr(overlayHwnd))
		overlayHwnd = 0
	}

	if magentaBrush != 0 {
		procGdiDeleteObject.Call(uintptr(magentaBrush))
		magentaBrush = 0
	}
	if blackBrush != 0 {
		procGdiDeleteObject.Call(uintptr(blackBrush))
		blackBrush = 0
	}

	instance, _, _ := procGetModuleHandle.Call(0)
	classNamePtr := mustUTF16("OverlayClass")
	procUnregisterClassW.Call(uintptr(unsafe.Pointer(classNamePtr)), instance)

	//This puts a WM_QUIT message in the queue, which causes GetMessage to return 0 and gracefully break the loop.
	procPostQuitMessage.Call(0)
	/*
		PostThreadMessage(id, WM_QUIT, ...) literally pushes a message into the queue.

		PostQuitMessage(0) doesn't actually "post" a message immediately. It sets a internal "quit" flag in the thread's message queue.
		The next time your GetMessage loop looks for work and finds no other messages, it "synthesizes" a WM_QUIT message on the fly.
	*/
	//however, we used to be singlethreaded and then we were in the same thread that executes that loop so the chances are 0 that we get back to it and more likely that we'll os.Exit
	//but now, hmm... well we're in deinit() of the same thread so it's same thing, heh.
	if winEventHook != 0 {
		logf("cleaned winEventHook from deinit()")
		procUnhookWinEvent.Call(uintptr(winEventHook))
		winEventHook = 0
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
	procPostMessage.Call(
		uintptr(trayIcon.HWnd),
		WM_EXIT_VIA_CTRL_C,
		uintptr(ctrlType),
		0,
	)
	return 1 // 1=true aka i handled this event ie. don't do the default handling which would exit.
})

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
	logChanSize   uint64 = 4096
	logChan              = make(chan string, logChanSize) // Buffer of this many log messages
	logWorkerDone        = make(chan struct{})            // The "I'm finished" signal
)

const attemptAtomicSwapThisManyTimes uint = 100

func logf(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	now := time.Now().Format("Mon Jan 2 15:04:05.000000000 MST 2006") // these values must be used exactly, they're like specific % placeholders.
	//now := time.Now().Format("2006-01-02T15:04:05.000Z07:00")
	finalMsg := fmt.Sprintf("[%s] %s\n", now, s)

	// Check the current pressure on the pipe
	//len() - It never returns a negative value — for all supported kinds (arrays, slices, maps, strings, channels) the result is >= 0 (and for nil slices/maps/channels it’s 0).
	currentDepth := uint64(len(logChan))
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
	focusOnDrag = true
	doLMBClick2FocusAsFallback = false
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
		exitf(2, "CreateMutex failed entirely: '%v' (code: %d)%s", err, err, extra)
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
	defer func() {
		// This only executes AFTER close(logChan) is called AND the buffer is empty, or a panic happened here
		close(logWorkerDone)
		//this ^ allows the process (main) to exit because it is stuck waiting in closeAndFlushLog() which is right before os.Exit
	}()

	//doneTODO: find out what happens if a panic or exitf() happens inside this logWorker which is running in a goroutine thus not subject to those 2 deferers we made for nice clean exit!
	defer func() {
		if r2 := recover(); r2 != nil {
			directLoggerf("![CRITICAL ERROR IN logWorker thread]: '%v'\n%s\n----snip----", r2, debug.Stack())
		} else {
			directLoggerf("logWorker thread here, normal exit")
		}
		// fall thru to the above defer
	}()

	// This runs on Thread B.
	// even If fmt.Fprint blocks for 10 seconds here, Thread A (your mouse hook)
	// keeps spinning at 100% speed on its own CPU core.
	var counter uint32 = 0
	const MaxBeforeReset uint32 = 4_294_967_295 - 10_000_000
	const modVal = 50
	for msg := range logChan {
		counter++
		internalLogger(msg) // good call here
		if counter%modVal == 0 {
			verifyMemoryIsLocked() // can logf itself! so modVal must be > than how many msgs it can log worst case(currently 1) else i will infinite loop here.
			//time.Sleep(10 * time.Second) //FIXME: temp, remove this!
			if counter > MaxBeforeReset {
				counter = 0
			}
		}
		// if counter%5 == 0 {
		// 	runtime.GC() // garbage collect, no apparent effect!
		// }
	}
	drops := droppedLogEvents.Load()
	if drops > 0 {
		directLoggerf("Dropped %s log events due to contention. This should never happen.", withCommas(drops))
	}
	maxLogEvents := maxChannelFillForLogEvents.Load()
	if maxLogEvents > 1 {
		directLoggerf("Most log events seen at one time ie. peak queued on log channel: %s, out of logChanSize: %s", withCommas(maxLogEvents), withCommas(logChanSize))
	}
	maxMoveEvents := maxChannelFillForMoveEvents.Load()
	if maxMoveEvents > 1 {
		directLoggerf("Most move/resize events queued: %s (Dropped: %s which were <%dms apart, to prevent mouse stuttering)",
			withCommas(maxMoveEvents), withCommas(droppedMoveEvents.Load()), forceMoveOrResizeActionsToBeThisManyMSApart)
		//logf("for testing when a panic in logWorker happens after main's keypress, right before main's os.Exit!")
	}
} //logWorker

func directLoggerf(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	now := time.Now().Format("Mon Jan 2 15:04:05.000000000 MST 2006") // these values must be used exactly, they're like specific % placeholders.
	//now := time.Now().Format("2006-01-02T15:04:05.000Z07:00")
	finalMsg := fmt.Sprintf("[%s] %s\n", now, s)
	internalLogger(finalMsg) // good call here
}

// never call this directly, instead call directLoggerf()
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
	// XXX: This blocks until close(logWorkerDone) happens in the worker
	<-logWorkerDone
}

type theILockedMainThreadToken struct{}

var currentExitCode int = 0

// graceful exit if primary_defer() failed!
// secondary defer, never runs unless primary defer is defective(ie. panics in itself)
func secondary_defer() {
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

// a placeholder for graceful exit
func primary_defer() { //primary defer
	// SIGNAL THE WATCHDOG:
	// Closing this channel releases the hookWorker from its 2s timer.
	select {
	case <-mainAcknowledgedShutdown:
		// already closed
	default:
		close(mainAcknowledgedShutdown)
	}

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

	deinit()

	logf("Execution finished.")
	if writeProfile {
		writeHeapProfileOnExit()
	}
	// 2. Use your high-quality "clrbuf" waiter
	// Only pause if we have an actual console window and an error occurred

	// // 2. Check if Stdin is actually a terminal (not a pipe/null)
	if stdinIsConsoleInteractive() {
		releaseSingleInstance() // don't hog the mutex while waiting for key
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
	/*
			How the Scheduler Sees Your Code

		The Go scheduler uses three entities: G (Goroutine), M (Machine/OS Thread), and P (Processor/Context). GOMAXPROCS controls the number of Ps.

		    Main Goroutine (Thread A): By calling LockOSThread(), you tie your Main Goroutine to a specific OS Thread. Because it’s locked, it "clogs" one P (Processor context) while it's running your Win32 loop.

		    logWorker (Thread B): If GOMAXPROCS is set to 1, there is only one "seat" available for Go code to run. Since the Main Thread is sitting in that seat (locked), the logWorker will be starved and won't run until the Main Thread yields or sleeps.

		    Setting to 2: This creates two "seats." The Main Thread takes one, and the logWorker can take the second one on a different OS thread/core.
			- Gemini 3 Fast
	*/

	defer secondary_defer() //this runs second but only if first doesn't os.exit ie. it fails/panics!

	defer primary_defer() //this runs first

	installCtrlHandlerIfConsole()

	ensureSingleInstance("winbollocks_uniqueID_123lol", MutexScopeSession)

	//(Passing 0 to GOMAXPROCS just returns the current setting without changing it.)
	cpus := int64(runtime.NumCPU())
	if cpus < 0 {
		exitf(1, "negative number of CPUs returned %s", withCommasSigned(cpus))
	}
	logf("GOMAXPROCS is set to: %d instead of the default-if-unset %s or wtw value was set in your env. var (if any)", runtime.GOMAXPROCS(0), withCommas(uint64(cpus)))

	// 3. Your logic (Task 1: don't use log.Fatal inside here!)
	if err := runApplication(token); err != nil {
		exitf(2, "Error: %v\n", err)
	}
	logf("Went past runApplication, now at  main()'s end.")
} //main

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
// the hind below has no apparent effect or i didn't use it in proper context and something else was in effect which made it seems as having no effect!
//
//go:panicnhint
func exitf(code int, format string, a ...interface{}) {
	//deinit()
	//this panic will run the primary and potentially secondary(if primary fails) deferrers! ie. primary_defer
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

	mainThreadID = windows.GetCurrentThreadId()
	logf("main loop thread started. ThreadID: %d", mainThreadID)

	if err := initTray(); err != nil {
		exitf(1, "Failed to init tray: %v", err)
	}

	go hookWorker()

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

	// Global foreground change hook, this is the WH_SHELL hook, changed tho to accommodate needs.
	h, _, err := procSetWinEventHook.Call(
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
		defer func() {
			procUnhookWinEvent.Call(uintptr(winEventHook))
			winEventHook = 0
			logf("normal unhooking of winEventHook, from main thread")
		}()
	}

	initOverlay()

	//You should call lockRAM() at the very end of your initialization sequence, but before you enter the main message loop (GetMessage).
	lockRAM()
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
			//WM_QUIT	0x0012	(Not handled in wndProc) This causes GetMessage to return 0.
			break // Loop breaks because hookWorker sent WM_QUIT, or we did WM_CLOSE or WM_DESTROY on main window which eventually triggered a WM_QUIT !
		}
		/*
					Why Hooks don't need Dispatch

			In a normal window setup, you need DispatchMessage to send a message to a WndProc. But Low-Level Hooks (WH_MOUSE_LL) are not window messages.

			When you install a Low-Level Hook, the OS injects a requirement into your thread: "Whenever the mouse moves, pause the system and run this
			specific callback function on this thread."

			The OS's Hook Manager doesn't wait for DispatchMessage. Instead, it intercepts your thread while it is inside the GetMessage (or PeekMessage) call.

			    The flow: GetMessage is called → The OS sees there's a mouse event → The OS executes your mouseProc callback directly while the thread is
				still "inside" the GetMessage syscall → Your callback returns → GetMessage finally returns to your loop with a (potentially unrelated) message.
		*/

		// Handle System Tray / Window Messages
		// This ensures your wndProc gets called!
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}

	// THE LOOP EXITED. Why? Let's check if the hook thread crashed.
	if p := hookPanicPayload.Load(); p != nil {
		logf("main loop exited because hookThread panic'd")
		// Re-throw the exact same panic on the MAIN thread.
		// This will naturally trigger main's primary_defer()!
		panic(p)
	} else {
		logf("main loop exited normally")
	}
	return nil // no error
}

var (
	psapi                 = windows.NewLazyDLL("psapi.dll")
	procQueryWorkingSetEx = psapi.NewProc("QueryWorkingSetEx")
)

type PSAPI_WORKING_SET_EX_BLOCK struct {
	Flags uintptr
}

func (b *PSAPI_WORKING_SET_EX_BLOCK) IsValid() bool {
	// Bit 0 of VirtualAttributes (the 'Valid' bit) indicates if the page
	// is currently resident in physical RAM.
	return b.Flags&1 == 1 // Bit 0 is the 'Valid' (resident) bit
}

type PSAPI_WORKING_SET_EX_INFORMATION struct {
	VirtualAddress    uintptr
	VirtualAttributes PSAPI_WORKING_SET_EX_BLOCK
}

// Define this at the top level (global)
var (
	// Ensure it's not optimized away by making it a package-level variable
	integrityCheckVar int64 = 0xDEADC0DE
)

func verifyMemoryIsLocked() {
	//var testVar int = 42 // Variable we want to check, bad, on stack always hot.
	hProc := getCurrentProcess()

	// PSAPI_WORKING_SET_EX_INFORMATION
	// This tells Windows: "Tell me about the physical state of this specific address"
	info := PSAPI_WORKING_SET_EX_INFORMATION{
		VirtualAddress: uintptr(unsafe.Pointer(&integrityCheckVar)),
	}

	ret, _, err := procQueryWorkingSetEx.Call(
		hProc,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)

	if ret == 0 {
		logf("Failed QueryWorkingSetEx: %v", err)
		return
	}

	if !info.VirtualAttributes.IsValid() {
		//		logf("Verification: Memory at 0x%X is currently resident in RAM.", info.VirtualAddress)
		//} else {
		logf("Verification: Memory at 0x%X is currently PAGED OUT. This is unexpected!", info.VirtualAddress)
	}
}

var (
	advapi32                  = windows.NewLazySystemDLL("advapi32.dll") // Add this!
	procOpenProcessToken      = advapi32.NewProc("OpenProcessToken")
	procLookupPrivilegeValue  = advapi32.NewProc("LookupPrivilegeValueW")
	procAdjustTokenPrivileges = advapi32.NewProc("AdjustTokenPrivileges")
)

const (
	TOKEN_ADJUST_PRIVILEGES = 0x0020
	TOKEN_QUERY             = 0x0008
	SE_PRIVILEGE_ENABLED    = 0x00000002
	SE_INC_WORKING_SET_NAME = "SeIncrementWorkingSetPrivilege"
)

type LUID struct {
	LowPart  uint32
	HighPart int32
}

type LUID_AND_ATTRIBUTES struct {
	Luid       LUID
	Attributes uint32
}

type TOKEN_PRIVILEGES struct {
	PrivilegeCount uint32
	Privileges     [1]LUID_AND_ATTRIBUTES
}

func lockRAM() {
	//Warning for Defensive Coding: SetProcessWorkingSetSize can fail if the values you provide are too high or if the user doesn't have the
	// SE_INC_WORKING_SET_NAME privilege (though for small amounts like 10–50MB, Windows usually grants it to "High" priority processes without drama).
	//hProc, _, _ := procGetCurrentProcess.Call()
	hProc := getCurrentProcess()

	//To successfully increase your working set, you often need the SE_INC_WORKING_SET_NAME privilege. Simply calling the API might fail silently or return "Access Denied."
	// 1. Enable the Privilege
	var token uintptr
	ret, _, err := procOpenProcessToken.Call(hProc, TOKEN_ADJUST_PRIVILEGES|TOKEN_QUERY, uintptr(unsafe.Pointer(&token)))
	if ret != 0 {
		var luid LUID
		lpName, _ := windows.UTF16PtrFromString(SE_INC_WORKING_SET_NAME)
		ret, _, err = procLookupPrivilegeValue.Call(0, uintptr(unsafe.Pointer(lpName)), uintptr(unsafe.Pointer(&luid)))

		if ret != 0 {
			tp := TOKEN_PRIVILEGES{
				PrivilegeCount: 1,
				Privileges: [1]LUID_AND_ATTRIBUTES{
					{Luid: luid, Attributes: SE_PRIVILEGE_ENABLED},
				},
			}
			// AdjustTokenPrivileges returns success even if it partially fails,
			// so we must check GetLastError (err) specifically.
			ret, _, err = procAdjustTokenPrivileges.Call(token, 0, uintptr(unsafe.Pointer(&tp)), 0, 0, 0)
			if ret == 0 || err != windows.Errno(0) {
				logf("Warning: Could not enable SeIncrementWorkingSetPrivilege, err: '%v', continuing tho.", err)
			}
		}
		windows.CloseHandle(windows.Handle(token))
	}

	// 2. Set the Working Set Size
	// We'll request 20MB min and 50MB max.

	// We request that 20MB to 50MB stay in RAM at all times.
	// This effectively "VirtualLocks" the core of your app.
	var min2 uint64 = 20 * 1024 * 1024
	var max2 uint64 = 50 * 1024 * 1024

	ret, _, err = procSetProcessWorkingSetSize.Call(hProc, uintptr(min2), uintptr(max2))
	if ret == 0 {
		logf("Failed SetProcessWorkingSetSize to min:%s and max:%s, err: %v", humanBytes(min2), humanBytes(max2), err)
	} else {
		logf("Working Set locked between %s and %s", humanBytes(min2), humanBytes(max2))
	}

	verifyMemoryIsLocked() //kinda useless to do now

	// 2. Schedule the "Heisenberg-proof" check
	// We wait 30 seconds to let Windows try to 'trim' our RAM.
	time.AfterFunc(30*time.Second, func() {
		verifyMemoryIsLocked()
	})
}

func humanBytes(bytes uint64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	const unit uint64 = 1024
	div, exp := unit, 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	suffix := []string{"KiB", "MiB", "GiB", "TiB"}[exp]
	return fmt.Sprintf("%.2f %s", float64(bytes)/float64(div), suffix)
}

func withCommas(n uint64) string {
	s := strconv.FormatUint(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func withCommasSigned(n int64) string {
	s := strconv.FormatInt(n, 10)
	if n < 0 {
		return "-" + withCommasSigned(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

const (
	NORMAL_PRIORITY_CLASS uintptr = 0x20
	HIGH_PRIORITY_CLASS   uintptr = 0x00000080

	THREAD_PRIORITY_TIME_CRITICAL int32 = 15
)

// GetCurrentProcess returns a valid pseudo-handle which happens to be -1.
// In Go, ^uintptr(0) (all bits set) is the numeric representation of -1
const CURRENT_PROCESS_PSEUDO_HANDLE = ^uintptr(0) // All bits set to 1

// In uintptr fashion (64-bit), -2 is: 0xFFFFFFFFFFFFFFFE aka ^uintptr(1)
const CURRENT_THREAD_PSEUDO_HANDLE uintptr = ^uintptr(1)

var (
	ntdll                       = windows.NewLazyDLL("ntdll.dll")
	procNtSetInformationProcess = ntdll.NewProc("NtSetInformationProcess")
)

const (
	//PROCESS_IO_PRIORITY uint32 = 7
	// NTDLL ProcessInfoClass Enum
	PROCESS_IO_PRIORITY uint32 = 33 // 0x21

	//In the undocumented internal ntdll.dll API, Memory Priority is 39.
	//But we are calling the public kernel32.dll API (SetProcessInformation). In kernel32, the constant for ProcessMemoryPriority is 0.
	//PROCESS_MEMORY_PRIORITY uint32 = 9  // The info class for Memory Priority
	//PROCESS_PAGE_PRIORITY   uint32 = 39 // Alternative for some Win10/11 builds
	// Kernel32 ProcessInformationClass Enum
	PROCESS_MEMORY_PRIORITY uint32 = 0 // Fixed: It is 0, not 9 or 39!

	// I/O Priority Values
	// 0 = Very Low, 1 = Low, 2 = Normal. (Standard apps cannot exceed 2).
	IO_PRIORITY_NORMAL uint32 = 2
	// I/O Priority Hints
	IO_PRIORITY_HIGH uint32 = 4
)

// MEMORY_PRIORITY_INFORMATION struct for SetProcessInformation
type MEMORY_PRIORITY_INFORMATION struct {
	MemoryPriority uint32
}

func getCurrentProcess() (hProc uintptr) {
	//Unlike most functions that return a real handle you have to track and close, GetCurrentProcess just returns (HANDLE)-1 (or 0xFFFFFFFF).
	// It’s a constant that points to "the process that is calling this function."
	//Technically, according to Microsoft's documentation, this function cannot fail.
	//a rename to Local isn't needed here but i wanna be sure visibly too.
	hProcLocal, _, err := procGetCurrentProcess.Call()
	if hProcLocal == 0 || hProcLocal != CURRENT_PROCESS_PSEUDO_HANDLE {
		// This virtually never happens, but if it did,
		// the system is in a very weird state.
		exitf(1, "Critical: GetCurrentProcess returned 0x%X, err: %v", hProcLocal, err)
	}
	return hProcLocal
}

func getCurrentThread() (hThread uintptr) {
	//Note that GetCurrentThread also returns a pseudo-handle (usually -2), so it doesn't need to be closed either.
	currThread, _, err := procGetCurrentThread.Call()
	if currThread == 0 || currThread != CURRENT_THREAD_PSEUDO_HANDLE {
		exitf(1, "Critical: GetCurrentProcess returned 0x%X, err: %v", currThread, err)
	}
	return currThread
}

// required high prio(normal is stuttering) to avoid mouse stuttering during the whole Gemini AI website version reply in Firefox.
// "By being "High Priority," you tell the Windows Scheduler that your thread should have a longer quantum (more time before being interrupted)
// and a shorter wait time to be re-scheduled. It ensures that when the "Mouse Interrupt" fires, your Go code is ready to answer the door immediately."
func setAndVerifyPriority() {
	hProc := getCurrentProcess()

	// Set to HIGH_PRIORITY_CLASS (0x80)
	const wantedProcessPrio uintptr = HIGH_PRIORITY_CLASS
	ntStatus, _, err := procSetPriorityClass.Call(hProc, wantedProcessPrio)
	if ntStatus == 0 {
		logf("Failed to set process priority class to 0x%x, err:%v", wantedProcessPrio, err)
		//return
	}

	// Verify it actually changed
	prio, _, err := procGetPriorityClass.Call(hProc)
	if prio == 0x00000080 {
		logf("Process priority confirmed: 0x%x where 0x%x is Normal.", wantedProcessPrio, NORMAL_PRIORITY_CLASS)
	} else {
		logf("Priority mismatch! OS returned prio: 0x%x instead of 0x%x and err was: %v", prio, wantedProcessPrio, err)
	}

	const wantedThreadPrio int32 = THREAD_PRIORITY_TIME_CRITICAL
	//By setting the thread prio to 15, you are at the absolute ceiling of the "Dynamic" priority range.
	// Only "Realtime" processes can go higher (16–31). This ensures that even if your Go app's other threads
	// (like the one doing logging or tray icon management) get bogged down, the thread handling the mouse hook has a "VIP pass" at the CPU's door.

	currThread := getCurrentThread()

	//In Go, the Garbage Collector runs on background threads. If your Process Priority is High (13) but your Hook Thread is Time Critical (15),
	// the Hook Thread will actually preempt the Go Garbage Collector if they both want the CPU at the same time.
	//This is the secret sauce for low-latency Go on Windows: you've made the hook more important than the language's own housekeeping.
	// - gemini 3 fast
	//The Process is High, but the Hook Thread (current thread) is "Time Critical." This ensures that even if your Go app starts doing a heavy Garbage Collection on another thread,
	// the Hook Thread gets the absolute maximum "right of way."
	tRet, _, tErr := procSetThreadPriority.Call(currThread, uintptr(wantedThreadPrio))
	if tRet == 0 {
		logf("Failed to set thread priority, err: %v", tErr)
	} else {
		// Verify Thread Priority
		// procGetThreadPriority = kernel32.NewProc("GetThreadPriority")
		tprio, _, err2 := procGetThreadPriority.Call(currThread)

		// GetThreadPriority returns an int. 15 is TIME_CRITICAL.
		if int32(tprio) == wantedThreadPrio {
			logf("Thread Priority confirmed: %d", tprio)
		} else {
			logf("Thread Priority mismatch! OS returned prio: %d instead of %d and err was: %v", int32(tprio), wantedThreadPrio, err2)
		}
	}

	//FIXME: so since memprio and i/o prio below aren't set to anything different than normal, maybe don't try to set them at all ie. remove the code doing it!

	// --- Memory Priority (Using Kernel32) ---
	// this is so we don't get paged out to swap/pagefile
	var wantedMemPrio uint32 = 5 // 6 is Very High(doesn't work, it fails w/ invalid param!), 5 is the value i saw in process explorer if nothing's setting it at all.

	wantedType := PROCESS_MEMORY_PRIORITY
	memPrio := MEMORY_PRIORITY_INFORMATION{MemoryPriority: wantedMemPrio}

	ntStatus, _, err = procSetProcessInformation.Call(
		hProc,
		uintptr(wantedType), // 0
		uintptr(unsafe.Pointer(&memPrio)),
		unsafe.Sizeof(memPrio),
	)

	if ntStatus != 0 {
		logf("Memory Priority set to %d where 5 is Normal", memPrio.MemoryPriority)
	} else {
		logf("Failed SetProcessInformation (Memory) to %d, err: %v", wantedMemPrio, err)
	}

	// --- I/O Priority (Using NTDLL) ---
	// 4. Set I/O Priority (to 4 - High)
	// This affects disk access (logs), not mouse input. So I don't think i need this unless maybe there's constant heavy disk thrashing or gigs being written, then i need my logs(new log lines) saved not 2 minutes later.
	// IMPORTANT: We MUST use uint32 here so Sizeof returns 4, not 8.
	//IO_PRIORITY_HIGH(aka 4) will fail with NTSTATUS: 0xC000000D err: The operation completed successfully. and 3 will fail with NTSTATUS: 0xC0000061
	//You received 0xC000000D (STATUS_INVALID_PARAMETER) because Windows strictly limits I/O priority for user-mode applications. (even if running as admin btw)
	var ioHint uint32 = IO_PRIORITY_NORMAL //aka 2 works as it's the default anyway.
	// Note: NtSetInformationProcess returns an NTSTATUS, where 0 is STATUS_SUCCESS
	ntStatus, _, err = procNtSetInformationProcess.Call(
		hProc,
		uintptr(PROCESS_IO_PRIORITY), //33
		uintptr(unsafe.Pointer(&ioHint)),
		unsafe.Sizeof(ioHint),
	)
	if ntStatus != 0 {
		logf("Failed NtSetInformationProcess (I/O), NTSTATUS: 0x%X err: %v", ntStatus, err)
	} else {
		logf("I/O Priority set to %d where default is 2", ioHint)
	}
}

// Separate function to keep the loop readable
func drainMoveChannel() {
	for {
		// Track High-Water Mark
		currentFill := uint64(len(moveDataChan))
		if currentFill > maxChannelFillForMoveEvents.Load() {
			//TODO: recheck the logic in this when using more than 1 thread (currently only 1)
			maxChannelFillForMoveEvents.Store(currentFill)
			logf("New Channel Peak: %s events queued (Dropped: %s)",
				withCommas(currentFill), withCommas(droppedMoveEvents.Load()))
		}

		select {
		case data := <-moveDataChan:
			// Use the data (the struct copy) to move the window.
			// No heap pointers, no garbage collector stress!
			handleActualMoveOrResize(data) // Move the window
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

const TH32CS_SNAPPROCESS = 0x00000002

func getProcessName(pid uint32) string {
	snapshot, _, _ := procCreateToolhelp32Snapshot.Call(TH32CS_SNAPPROCESS, 0)
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
