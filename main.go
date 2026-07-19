//go:build windows && amd64

// winbollocks targets 64-bit Windows only. Several Win32 ABI details are
// architecture-specific:
//   - The INPUT / KEYBDINPUT struct layout includes explicit 64-bit padding.
//   - WindowFromPoint receives POINT by value packed into a single 64-bit
//     register (the amd64 calling convention); on x86 it would be two stack args.
//   - assertStructSizes() validates the 64-bit ABI layout at startup.
// Add a separate build target (and struct definitions) before enabling x86.

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
	"context"
	//"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/workturnedplay/wincoe"
)

// this init() must be first, order of it in source code matters as they're executed in order of seen.
func init() {
	// Force the runtime to provide exactly 3 execution contexts: logWorker, main msg loop (wndProc), and hooks!
	// regardless of what the user set in their Environment Variables.
	//runtime.GOMAXPROCS(3) //FIXME: so, don't do this at all, ie. remove this!
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

var selfHInstance windows.Handle

func init() {
	//GetModuleHandle(0): it returns the base address of your own .exe module, which is a constant value for the entire lifetime of your process.
	// Because you pass 0 (NULL), it does not increment a reference count, so it never requires CloseHandle or FreeLibrary.
	res := procGetModuleHandle.Call(0) // "If this parameter is NULL, GetModuleHandle returns a handle to the file used to create the calling process (.exe file)."
	if res.Failed() {
		panic(fmt.Sprintf("CRITICAL: GetModuleHandle(0) failed for self instance: %v", res.Err))
	}
	selfHInstance = windows.Handle(res.R1)
}

/* ---------------- DLLs & Procs ---------------- */

// var shellHook windows.Handle
var (
	// The Data Pipe (2048 is plenty for lag spikes)
	moveDataChan = make(chan WindowMoveData, 2048)

	// Modern Atomic tracking
	droppedMoveOrResizeEvents   atomic.Uint64
	droppedLogEvents            atomic.Uint64
	maxChannelFillForMoveEvents atomic.Uint64 // To track how "full" it got
	maxChannelFillForLogEvents  atomic.Uint64 // To track how "full" it got

)

func init() {
	maxChannelFillForMoveEvents.Store(1) // avoid the first message: New Channel Peak: 1 events queued (Dropped: 0)
}

var (
	winEventHook     windows.Handle
	winEventCallback = windows.NewCallback(winEventProc)
)

var appStartTime = time.Now() //only useful because time.Time keeps track of monotonic clock, good for .Sub() operations!

var (
	//The Problem: Variables like moveCounter and lastPostedX are incremented in the Hook Thread but get reset from the Main Thread when the user toggles the rate limiter in the system tray.
	moveCounter     atomic.Uint64 // how many move events we saw since last log
	lastRateLogTime atomic.Int64  // when we last printed the rate // Monotonic nanoseconds from appStartTime
	rateLogInterval = 1 * time.Second
)
var actualPostCounter atomic.Uint64

// Globals
var (
	//the timestamp when the message to Move a window was queued onto the move channel
	lastMovePostedTime       atomic.Int64 // Monotonic nanoseconds from appStartTime
	lastPostedX, lastPostedY atomic.Int32
)

// MIN_MOVE_INTERVAL the minimum amount of time between window moves, ie. throttle anything faster than this!
// XXX: yes this works too, here: //revive:disable:var-naming
const MIN_MOVE_INTERVAL = 33 * time.Millisecond // ~30 fps – very pleasant

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	shcore   = windows.NewLazySystemDLL("shcore.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	psapi    = windows.NewLazySystemDLL("psapi.dll")
	advapi32 = windows.NewLazySystemDLL("advapi32.dll")
	ntdll    = windows.NewLazySystemDLL("ntdll.dll")
	wtsapi32 = windows.NewLazySystemDLL("wtsapi32.dll")

	// procNtSetInformationProcess = ntdll.NewProc("NtSetInformationProcess")
	procNtSetInformationProcess = wincoe.NewBoundProc(ntdll, "NtSetInformationProcess", wincoe.CheckNTSTATUS) // NTSTATUS (0 == success)

	// procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	// procSetWindowsHookEx    = user32.NewProc("SetWindowsHookExW")
	// procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	// procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	// procGetMessage          = user32.NewProc("GetMessageW")
	// procTranslateMessage    = user32.NewProc("TranslateMessage")
	// procDispatchMessage     = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = wincoe.NewBoundProc(user32, "PostQuitMessage", wincoe.CheckNone) // void-ish, but safe
	procSetWindowsHookEx    = wincoe.NewBoundProc(user32, "SetWindowsHookExW", wincoe.CheckNull)
	procCallNextHookEx      = wincoe.NewBoundProc(user32, "CallNextHookEx", wincoe.CheckNone) // returns next hook result
	procUnhookWindowsHookEx = wincoe.NewBoundProc(user32, "UnhookWindowsHookEx", wincoe.CheckBool)
	procGetMessage          = wincoe.NewBoundProc(user32, "GetMessageW", wincoe.CheckMinusOne) // -1 on error, 0 on WM_QUIT
	procSendMessage         = wincoe.NewBoundProc(user32, "SendMessageW", wincoe.CheckNone)    // LRESULT
	procTranslateMessage    = wincoe.NewBoundProc(user32, "TranslateMessage", wincoe.CheckNone)
	procDispatchMessage     = wincoe.NewBoundProc(user32, "DispatchMessageW", wincoe.CheckNone) // returns value from window proc

	// procGetAsyncKeyState    = user32.NewProc("GetAsyncKeyState")
	// procWindowFromPoint     = user32.NewProc("WindowFromPoint")
	// procGetAncestor         = user32.NewProc("GetAncestor")
	// procReleaseCapture      = user32.NewProc("ReleaseCapture") // Releases mouse capture if any window has it
	// procSendMessage         = user32.NewProc("SendMessageW")
	// procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procGetAsyncKeyState    = wincoe.NewBoundProc(user32, "GetAsyncKeyState", wincoe.CheckNone) // returns short
	procWindowFromPoint     = wincoe.NewBoundProc(user32, "WindowFromPoint", wincoe.CheckNull)
	procGetAncestor         = wincoe.NewBoundProc(user32, "GetAncestor", wincoe.CheckNull)
	procReleaseCapture      = wincoe.NewBoundProc(user32, "ReleaseCapture", wincoe.CheckBool) // Releases mouse capture if any window has it
	procSetForegroundWindow = wincoe.NewBoundProc(user32, "SetForegroundWindow", wincoe.CheckBool)

	// procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")
	// procDestroyWindow   = user32.NewProc("DestroyWindow")
	procShellNotifyIcon = wincoe.NewBoundProc(shell32, "Shell_NotifyIconW", wincoe.CheckBool)
	procDestroyWindow   = wincoe.NewBoundProc(user32, "DestroyWindow", wincoe.CheckBool)

	//procSendMessageTimeout = user32.NewProc("SendMessageTimeoutW")
	procSendMessageTimeout = wincoe.NewBoundProc(user32, "SendMessageTimeoutW", wincoe.CheckZero) // or CheckErrno depending on usage

	// procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	// procGetWindowPlacement       = user32.NewProc("GetWindowPlacement")
	// procGetWindowRect            = user32.NewProc("GetWindowRect")
	// procShowWindow               = user32.NewProc("ShowWindow")
	// procSetWindowPos             = user32.NewProc("SetWindowPos")
	procGetWindowThreadProcessID = wincoe.NewBoundProc(user32, "GetWindowThreadProcessId", wincoe.CheckZero)
	procGetWindowPlacement       = wincoe.NewBoundProc(user32, "GetWindowPlacement", wincoe.CheckBool)
	procGetWindowRect            = wincoe.NewBoundProc(user32, "GetWindowRect", wincoe.CheckBool)
	procShowWindow               = wincoe.NewBoundProc(user32, "ShowWindow", wincoe.CheckNone)
	procSetWindowPos             = wincoe.NewBoundProc(user32, "SetWindowPos", wincoe.CheckBool)

	// procDefWindowProc   = user32.NewProc("DefWindowProcW")
	// procRegisterClassEx = user32.NewProc("RegisterClassExW")
	// procCreateWindowEx  = user32.NewProc("CreateWindowExW")
	procDefWindowProc   = wincoe.NewBoundProc(user32, "DefWindowProcW", wincoe.CheckNone)   // LRESULT
	procRegisterClassEx = wincoe.NewBoundProc(user32, "RegisterClassExW", wincoe.CheckZero) // atom / 0 on fail
	procCreateWindowEx  = wincoe.NewBoundProc(user32, "CreateWindowExW", wincoe.CheckNull)

	// procGetModuleHandle = kernel32.NewProc("GetModuleHandleW")
	procGetModuleHandle = wincoe.NewBoundProc(kernel32, "GetModuleHandleW", wincoe.CheckNull)

	// procSetCapture = user32.NewProc("SetCapture")
	// procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
	// procGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	procSetCapture            = wincoe.NewBoundProc(user32, "SetCapture", wincoe.CheckNone)
	procGetCapture            = wincoe.NewBoundProc(user32, "GetCapture", wincoe.CheckNone)
	procSetConsoleCtrlHandler = wincoe.NewBoundProc(kernel32, "SetConsoleCtrlHandler", wincoe.CheckBool)
	procGetForegroundWindow   = wincoe.NewBoundProc(user32, "GetForegroundWindow", wincoe.CheckNone)
	// procIsWindow              = wincoe.NewBoundProc(user32, "IsWindow", wincoe.CheckBool)

	// procCreatePopupMenu = user32.NewProc("CreatePopupMenu")
	// procAppendMenu      = user32.NewProc("AppendMenuW")
	// procTrackPopupMenu  = user32.NewProc("TrackPopupMenu")
	// procGetCursorPos    = user32.NewProc("GetCursorPos")
	procCreatePopupMenu = wincoe.NewBoundProc(user32, "CreatePopupMenu", wincoe.CheckNull)
	procAppendMenu      = wincoe.NewBoundProc(user32, "AppendMenuW", wincoe.CheckBool)
	//"This API returns BOOL only if TPM_RETURNCMD is specified. Otherwise it returns nonzero merely because the menu was displayed.If you don't always pass TPM_RETURNCMD, CheckBool is fine. If you do always pass TPM_RETURNCMD, then returning 0 may simply mean the user dismissed the menu without choosing anything." - chatgpt 5.5
	procTrackPopupMenu = wincoe.NewBoundProc(user32, "TrackPopupMenu", wincoe.CheckNone)
	procDestroyMenu    = wincoe.NewBoundProc(user32, "DestroyMenu", wincoe.CheckBool)
	procGetCursorPos   = wincoe.NewBoundProc(user32, "GetCursorPos", wincoe.CheckBool)

	// procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	// procSetProcessDpiAwareness        = shcore.NewProc("SetProcessDpiAwareness")
	//doneTODO: need to impl. Find() like Call() was, maybe!
	procSetProcessDpiAwarenessContext = wincoe.NewBoundProc(user32, "SetProcessDpiAwarenessContext", wincoe.CheckBool)
	procSetProcessDpiAwareness        = wincoe.NewBoundProc(shcore, "SetProcessDpiAwareness", wincoe.CheckHRESULT)

	// procAttachThreadInput = user32.NewProc("AttachThreadInput")
	procAttachThreadInput = wincoe.NewBoundProc(user32, "AttachThreadInput", wincoe.CheckBool)

	// procPostMessage       = user32.NewProc("PostMessageW")
	// procPostThreadMessage = user32.NewProc("PostThreadMessageW")
	procPostMessage       = wincoe.NewBoundProc(user32, "PostMessageW", wincoe.CheckBool)
	procPostThreadMessage = wincoe.NewBoundProc(user32, "PostThreadMessageW", wincoe.CheckBool)

	// procGetLastError = kernel32.NewProc("GetLastError")
	//procGetLastError = wincoe.NewBoundProc(kernel32, "GetLastError", wincoe.CheckNone) // shouldn't have to use this?

	// procSendInput = user32.NewProc("SendInput")
	// procLoadIcon  = user32.NewProc("LoadIconW")
	procSendInput = wincoe.NewBoundProc(user32, "SendInput", wincoe.CheckZero) // UINT (count)
	procLoadIcon  = wincoe.NewBoundProc(user32, "LoadIconW", wincoe.CheckNull)

	// procUnregisterClassW = user32.NewProc("UnregisterClassW")
	procUnregisterClassW = wincoe.NewBoundProc(user32, "UnregisterClassW", wincoe.CheckBool)

	// Priority / process
	// procSetPriorityClass  = kernel32.NewProc("SetPriorityClass")
	// procGetPriorityClass  = kernel32.NewProc("GetPriorityClass")
	// procGetCurrentProcess = kernel32.NewProc("GetCurrentProcess")
	// procGetCurrentThread  = kernel32.NewProc("GetCurrentThread")
	// procSetThreadPriority = kernel32.NewProc("SetThreadPriority")
	// procGetThreadPriority = kernel32.NewProc("GetThreadPriority")
	procSetPriorityClass  = wincoe.NewBoundProc(kernel32, "SetPriorityClass", wincoe.CheckBool)
	procGetPriorityClass  = wincoe.NewBoundProc(kernel32, "GetPriorityClass", wincoe.CheckZero)
	procGetCurrentProcess = wincoe.NewBoundProc(kernel32, "GetCurrentProcess", wincoe.CheckEquals(CURRENT_PROCESS_PSEUDO_HANDLE))
	procGetCurrentThread  = wincoe.NewBoundProc(kernel32, "GetCurrentThread", wincoe.CheckEquals(CURRENT_THREAD_PSEUDO_HANDLE))
	procSetThreadPriority = wincoe.NewBoundProc(kernel32, "SetThreadPriority", wincoe.CheckBool)
	procGetThreadPriority = wincoe.NewBoundProc(kernel32, "GetThreadPriority", wincoe.CheckThreadPriority)

	// procSetProcessInformation    = kernel32.NewProc("SetProcessInformation")
	// procSetProcessWorkingSetSize = kernel32.NewProc("SetProcessWorkingSetSize")
	procSetProcessInformation    = wincoe.NewBoundProc(kernel32, "SetProcessInformation", wincoe.CheckBool)
	procSetProcessWorkingSetSize = wincoe.NewBoundProc(kernel32, "SetProcessWorkingSetSize", wincoe.CheckBool)

	// GDI / layered
	// procSetLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")
	// procBeginPaint                 = user32.NewProc("BeginPaint")
	// procEndPaint                   = user32.NewProc("EndPaint")
	// procDrawText                   = user32.NewProc("DrawTextW")
	// procFillRect                   = user32.NewProc("FillRect")
	procSetLayeredWindowAttributes = wincoe.NewBoundProc(user32, "SetLayeredWindowAttributes", wincoe.CheckBool)
	procBeginPaint                 = wincoe.NewBoundProc(user32, "BeginPaint", wincoe.CheckNull)
	procEndPaint                   = wincoe.NewBoundProc(user32, "EndPaint", wincoe.CheckBool)
	procDrawText                   = wincoe.NewBoundProc(user32, "DrawTextW", wincoe.CheckZero)
	procFillRect                   = wincoe.NewBoundProc(user32, "FillRect", wincoe.CheckZero)

	// procGdiSetTextColor     = gdi32.NewProc("SetTextColor")
	// procGdiSetBkMode        = gdi32.NewProc("SetBkMode")
	// procGdiCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	// procGdiDeleteObject     = gdi32.NewProc("DeleteObject")
	procGdiSetTextColor     = wincoe.NewBoundProc(gdi32, "SetTextColor", wincoe.CheckGDIError)
	procGdiSetBkMode        = wincoe.NewBoundProc(gdi32, "SetBkMode", wincoe.CheckZero)
	procGdiCreateSolidBrush = wincoe.NewBoundProc(gdi32, "CreateSolidBrush", wincoe.CheckNull)
	procGdiDeleteObject     = wincoe.NewBoundProc(gdi32, "DeleteObject", wincoe.CheckBool)

	// procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	// procSetCursorPos     = user32.NewProc("SetCursorPos")

	//Per Win32 docs, GetSystemMetrics returns 0 for both "the queried value is legitimately 0" and "the index is invalid/unsupported" — it does not set GetLastError in any meaningful way for these system-metric indices.
	procGetSystemMetrics = wincoe.NewBoundProc(user32, "GetSystemMetrics", wincoe.CheckNone) // returns int, 0 on failure for most indices
	procSetCursorPos     = wincoe.NewBoundProc(user32, "SetCursorPos", wincoe.CheckBool)

	// procInvalidateRect = user32.NewProc("InvalidateRect")
	procInvalidateRect = wincoe.NewBoundProc(user32, "InvalidateRect", wincoe.CheckBool)

	// GetWindowLongPtrW returns LONG_PTR (can be 0 legitimately); we treat non-zero as "success" for most usages
	procGetWindowLongPtrW = wincoe.NewBoundProc(user32, "GetWindowLongPtrW", wincoe.CheckNone)
	procSetLastError      = wincoe.NewBoundProc(kernel32, "SetLastError", wincoe.CheckNone) // void-like
	// procGetWindowLongPtrW = user32.NewProc("GetWindowLongPtrW")
	// procSetLastError      = kernel32.NewProc("SetLastError")

	// procCreateMutex  = kernel32.NewProc("CreateMutexW")
	// procReleaseMutex = kernel32.NewProc("ReleaseMutex")
	// procCloseHandle  = kernel32.NewProc("CloseHandle")
	procCreateMutex  = wincoe.NewBoundProc(kernel32, "CreateMutexW", wincoe.CheckNull)
	procReleaseMutex = wincoe.NewBoundProc(kernel32, "ReleaseMutex", wincoe.CheckBool)
	procCloseHandle  = wincoe.NewBoundProc(kernel32, "CloseHandle", wincoe.CheckBool)

	// procQueryWorkingSetEx = psapi.NewProc("QueryWorkingSetEx")
	procQueryWorkingSetEx = wincoe.NewBoundProc(psapi, "QueryWorkingSetEx", wincoe.CheckBool)

	// procOpenProcessToken      = advapi32.NewProc("OpenProcessToken")
	// procLookupPrivilegeValue  = advapi32.NewProc("LookupPrivilegeValueW")
	// procAdjustTokenPrivileges = advapi32.NewProc("AdjustTokenPrivileges")
	procOpenProcessToken     = wincoe.NewBoundProc(advapi32, "OpenProcessToken", wincoe.CheckBool)
	procLookupPrivilegeValue = wincoe.NewBoundProc(advapi32, "LookupPrivilegeValueW", wincoe.CheckBool)
	// AdjustTokenPrivileges is special: returns BOOL but sets LastError even on partial success (ERROR_NOT_ALL_ASSIGNED)
	procAdjustTokenPrivileges = wincoe.NewBoundProc(advapi32, "AdjustTokenPrivileges", wincoe.CheckAdjustTokenPrivileges)

	// procGetClassName = user32.NewProc("GetClassNameW")
	procGetClassName = wincoe.NewBoundProc(user32, "GetClassNameW", wincoe.CheckZero) // returns length

	// procInternalGetWindowText = user32.NewProc("InternalGetWindowText")
	procInternalGetWindowText = wincoe.NewBoundProc(user32, "InternalGetWindowText", wincoe.CheckStringLength) // returns length

	// procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleWindow = wincoe.NewBoundProc(kernel32, "GetConsoleWindow", wincoe.CheckNone)

	// procSetWinEventHook = user32.NewProc("SetWinEventHook")
	// procUnhookWinEvent  = user32.NewProc("UnhookWinEvent")
	procSetWinEventHook = wincoe.NewBoundProc(user32, "SetWinEventHook", wincoe.CheckNull)
	procUnhookWinEvent  = wincoe.NewBoundProc(user32, "UnhookWinEvent", wincoe.CheckBool)

	procWTSRegisterSessionNotification   = wincoe.NewBoundProc(wtsapi32, "WTSRegisterSessionNotification", wincoe.CheckBool)
	procWTSUnRegisterSessionNotification = wincoe.NewBoundProc(wtsapi32, "WTSUnRegisterSessionNotification", wincoe.CheckBool)

	procMonitorFromWindow = wincoe.NewBoundProc(user32, "MonitorFromWindow", wincoe.CheckNone) // returns HMONITOR; 0 means no monitor
	procGetMonitorInfo    = wincoe.NewBoundProc(user32, "GetMonitorInfoW", wincoe.CheckBool)
)

/* ---------------- Constants ---------------- */

const (
	WS_EX_LAYERED     = 0x00080000
	WS_EX_TRANSPARENT = 0x00000020
	LWA_COLORKEY      = 0x00000001
	LWA_ALPHA         = 0x00000002
)

var (
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
	WM_WTSSESSION_CHANGE = 0x02B1

	WTS_SESSION_LOCK   = 0x7
	WTS_SESSION_UNLOCK = 0x8

	NOTIFY_FOR_THIS_SESSION = 0
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
	//In Windows Vista and later, when you choose Version 4 behavior, Windows suppresses the standard legacy tooltip by default.
	// It assumes that because you are using the modern API version, you intend to provide your own advanced, application-drawn popup UI rather than a plain text tooltip.
	//To tell Windows that you still want to display the standard text tooltip while using Version 4, you must explicitly add the NIF_SHOWTIP flag to your UFlags.
	NIF_SHOWTIP = 0x00000080
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

// HungWindowTimeout if target window doesn't respond in 150ms we consider it hung and don't attempt to attach our input thread to it in an attempt to succeed at focusing it because it would also hang us.
const HungWindowTimeout = 150 // ms
const (
	SMTO_NORMAL      = 0x0000
	SMTO_ABORTIFHUNG = 0x0002
)

const (
	WM_NULL      = 0
	WM_MYSYSTRAY = WM_USER + 2
	//WM_WAKE_UP           = WM_USER + 3
	WM_INJECT_SEQUENCE             = WM_USER + 100
	WM_FOCUS_TARGET_WINDOW_SOMEHOW = WM_USER + 101
	WM_EXIT_VIA_CTRL_C             = WM_USER + 150
	WM_DO_SETWINDOWPOS             = WM_USER + 200 // arbitrary, just unique
	WM_HIDE_OVERLAY                = WM_USER + 205
	WM_BRING_TO_FRONT              = WM_USER + 206
	// WM_DO_SET_CAPTURE              = WM_USER + 210
	WM_DO_RELEASE_CAPTURE = WM_USER + 215
)
const (
	MENU_EXIT                                   = 1
	MENU_USE_LMB_TO_FOCUS_AS_FALLBACK           = 2
	MENU_ACTIVATE_MOVE                          = 3
	MENU_RATELIMIT_MOVES                        = 4
	MENU_LOG_RATE_OF_MOVES                      = 5
	MENU_TOGGLE_ASYNC_RESIZE                    = 6
	MENU_TOGGLE_REQUIRE_WINDOWN                 = 7
	MENU_TOGGLE_COALESCE_EVENTS                 = 8
	MENU_TOGGLE_IMMEDIATE_OVERLAY_REPAINT       = 9
	MENU_TOGGLE_MISSED_GESTURE_RECOVERY         = 10
	MENU_TOGGLE_INJECT_BUTTON_UP_ON_RECOVERY    = 11
	MENU_TOGGLE_BRING_TO_FRONT_ON_DRAG          = 12
	MENU_TOGGLE_BYPASS_GESTURES_WHEN_FULLSCREEN = 13
	MENU_TOGGLE_USE_THREADATTACHINPUT_FOR_FOCUS = 14
	MENU_TOGGLE_ACTIVATE_RESIZE                 = 15
	MENU_TOGGLE_BRING_TO_FRONT_ON_RESIZE        = 16

	MF_STRING = 0x0000

	MF_GRAYED   = 0x00000001
	MF_DISABLED = 0x00000002
	MF_CHECKED  = 0x00000008
)

const MONITOR_DEFAULTTONEAREST uintptr = 2

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
	ResizeZone  int
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
}

type MONITORINFO struct {
	CbSize    uint32
	RcMonitor RECT
	RcWork    RECT
	DwFlags   uint32
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
	//The Problem: winGestureUsed, capturing, and resizing manage your state machine. They are flipped by keyboardProc/mouseProc but cleared by hardReset/softReset (which can be triggered by the Main Thread via winEventProc).
	// used exclusively to know when to inject shift key tap (shiftdown+shiftUP) at the point when physical winkeyUP aka winUP is detected //noTODO: maybe remove because we do it(shifttap) now at gesture start
	winGestureUsed atomic.Bool
)

var (
	mouseHook windows.Handle
	kbdHook   windows.Handle

	trayIcon    NOTIFYICONDATA
	mainMsgHwnd windows.Handle
)

type DragMode int

func (m DragMode) String() string {
	switch m {
	case ModeMove:
		return "drag-move"
	case ModeResize:
		return "resize"
	default:
		return "unknown"
	}
}

const (
	ModeMove   DragMode = iota // aka drag-move
	ModeResize                 // resizing window
)

type dragSession struct {
	//currently or previously dragged window HWND, helps with state after doing winkey+L then unlocking session while dragging was in progress.
	targetWnd          windows.Handle
	state              dragState
	resizeZone         int
	mode               DragMode
	initialAspectRatio float64

	// viaMissedGestureRecovery is true when this session was started by the
	// missed-gesture recovery path (see checkForMissedGestureOnNextMove)
	// instead of a real WM_LBUTTONDOWN/WM_RBUTTONDOWN our hook actually saw
	// and swallowed. In that case the initiating LMB/RMB-down was delivered
	// to the target window normally (our hooks were blind to it while a
	// higher-integrity window still had the foreground), so the target's
	// own input state (e.g. a console starting a text selection on
	// LMB-down) genuinely believes the button is held. We inject a
	// synthetic LMB-up/RMB-up right when the session starts (see the
	// recovery branch in mouseProc's WM_MOUSEMOVE handling) so the target
	// stops extending that state (e.g. its selection) on every subsequent
	// move we let through. The eventual REAL up-event must still reach it
	// normally instead of being swallowed like an ordinary gesture's — see
	// mouseProc's WM_LBUTTONUP/WM_RBUTTONUP handling — both as a backstop
	// in case the target ignored our synthetic one, and so whatever window
	// is now under the cursor gets a normal release.
	viaMissedGestureRecovery bool
}

// A single atomic pointer handles the entire active state machine.
// Perfect Safety via Immutability
// The golden rule that makes this work is: Once a dragSession struct is created, its fields are never altered.
//
//	To stop a drag: You point the global variable to nil (softReset).
//	To start a new drag: You allocate a brand new struct on the heap and point the global variable to it (startDrag).
var activeSession atomic.Pointer[dragSession]

// Variables like focusOnDrag are modified in wndProc (Main Thread) when the user clicks the tray menu, but they are read constantly in mouseProc (Hook Thread).
var focusOnDrag atomic.Bool                // whether or not to (also)focus dragged window
var doLMBClick2FocusAsFallback atomic.Bool // if normal(thread attach) focus fails, then do the LMB click on the window to focus it(caveat: can click inside it eg. on its buttons!)
var ratelimitOnMove atomic.Bool            // use less CPU (see CPU time in task manager) but it's choppier and subconsciously no fun!
var shouldLogDragRate atomic.Bool          // but only when ratelimitOnMove is true
var asyncResize atomic.Bool
var requireWinDownHeldDuringGesture atomic.Bool // if true, the gesture(resize or move) stops when winkey is UP
var coalesceMoveResizeEvents atomic.Bool
var immediateOverlayRepaint atomic.Bool

// foregroundWasHigherIntegrity / checkForMissedGestureOnNextMove implement the
// missed-gesture recovery: winkey+LMB/RMB-dragging a window into focus
// from behind a higher-integrity window (e.g.
// Task Manager, while winbollocks isn't elevated). Our low-level hooks are
// blind while the higher-integrity window is still foreground, so the
// initiating LMB/RMB-down is swallowed before we ever see it. winEventProc
// arms checkForMissedGestureOnNextMove the instant the foreground regains a
// non-blocking integrity level; mouseProc's WM_MOUSEMOVE handling consumes it.
var foregroundWasHigherIntegrity atomic.Bool
var checkForMissedGestureOnNextMove atomic.Bool

// missedGestureRecoveryEnabled gates the missed-gesture recovery feature
// (see foregroundWasHigherIntegrity / checkForMissedGestureOnNextMove).
// Defaults to true; toggleable via systray.
var missedGestureRecoveryEnabled atomic.Bool

// injectButtonUpOnMissedGestureRecovery gates whether starting a
// missed-gesture-recovery drag/resize session (see viaMissedGestureRecovery)
// injects a synthetic LMB-up/RMB-up to stop the target window's own
// click-drag state (e.g. a console's text-selection extension) from
// fighting the window move on every subsequent mouse-move we let through.
// Off by default: the injection is a genuine, unfiltered button-up
// delivered wherever the target window currently is, so it can have side
// effects unrelated to selection state — e.g. a bare RMB-up landing outside
// any active selection in a classic conhost console window triggers Paste,
// and a click on a push-button rather than a text area could fire that
// control's action a little early. Toggleable via systray.
var injectButtonUpOnMissedGestureRecovery atomic.Bool

// bringToFrontOnDrag, when true, brings the drag target to the front of the
// Z-order at the moment a move gesture starts (useful after winkey+MMB sent it
// to the back). Toggleable via systray.
var bringToFrontOnDrag atomic.Bool

// focusOnResize / bringToFrontOnResize are ModeResize's independent
// counterparts to focusOnDrag / bringToFrontOnDrag above: whether to focus,
// and/or bring to front, the target window the moment a resize gesture
// starts. Kept as separate toggles (not shared with the move-gesture ones)
// so the two modes can be configured independently. Toggleable via systray.
var focusOnResize atomic.Bool
var bringToFrontOnResize atomic.Bool

// bypassGesturesWhenFullscreen, when true, suppresses winkey+mouse gestures
// whose resolved target window is fullscreen (exclusive or
// borderless-fullscreen) on its monitor. Checked live via
// isWindowFullscreenOnMonitor against that specific target at gesture-start
// time (see shouldBypassGestureNow) rather than against a cached
// foreground-change snapshot, so toggling this setting or switching targets
// mid-session is always reflected immediately instead of lagging behind the
// last EVENT_SYSTEM_FOREGROUND WinEvent.
var bypassGesturesWhenFullscreen atomic.Bool

var useThreadAttachInputForFocus atomic.Bool

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

// const minimumW = 300
// const minimumH = 300

func calculateResize(session *dragSession, currentPt POINT) (x, y, w, h int32) {
	drag := session.state
	zone := session.resizeZone
	//Since session.initialAspectRatio is a primitive float64 only utilized within one localized aspect-ratio conditional block, leaving it as a direct property read is both perfectly idiomatic and prevents an unnecessary stack allocation!
	//var initialAspectRatio float64 = session.initialAspectRatio

	dx := currentPt.X - drag.startPt.X
	dy := currentPt.Y - drag.startPt.Y

	origL := drag.startRect.Left
	origT := drag.startRect.Top
	origR := drag.startRect.Right
	origB := drag.startRect.Bottom
	origW := origR - origL
	origH := origB - origT

	if zone == ZONE_CENTER {
		// UNIFORM CENTER RESIZE
		var dw, dh int32

		if respectAspectRatio {
			if session.initialAspectRatio >= 1.0 {
				dw = dx * 2
				dh = int32(float64(dw) / session.initialAspectRatio)
			} else {
				dh = dy * 2
				dw = int32(float64(dh) * session.initialAspectRatio)
			}
		} else {
			dw = dx * 2
			dh = dy * 2
		}

		w = origW + dw
		h = origH + dh

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

		x, y = newL, newT
		w, h = newR-newL, newB-newT
	}

	// --- ANCHOR-AWARE HARD SAFETY FLOOR ---
	// Enforce a safe minimum size (e.g., 32x32) while locking down the correct coordinates
	// so the window never slides when it hits this boundary floor.
	const safeMin = 32

	if zone == ZONE_CENTER {
		if w < safeMin {
			w = safeMin
			x = origL + (origW-safeMin)/2
		}
		if h < safeMin {
			h = safeMin
			y = origT + (origH-safeMin)/2
		}
	} else {
		if w < safeMin {
			w = safeMin
			switch zone {
			case ZONE_TOP_LEFT, ZONE_MID_LEFT, ZONE_BOT_LEFT:
				// Left side is dragging inward -> Freeze the Right Edge (origR)
				x = origR - safeMin
			case ZONE_TOP_RIGHT, ZONE_MID_RIGHT, ZONE_BOT_RIGHT:
				// Right side is dragging inward -> Freeze the Left Edge (origL)
				x = origL
			}
		}
		if h < safeMin {
			h = safeMin
			switch zone {
			case ZONE_TOP_LEFT, ZONE_TOP_CENTER, ZONE_TOP_RIGHT:
				// Top side is dragging inward -> Freeze the Bottom Edge (origB)
				y = origB - safeMin
			case ZONE_BOT_LEFT, ZONE_BOT_CENTER, ZONE_BOT_RIGHT:
				// Bottom side is dragging inward -> Freeze the Top Edge (origT)
				y = origT
			}
		}
	}

	return x, y, w, h
}

// injectShiftTapOnly uses the unassigned vkE8 key to mask the Winkey.
// It is guaranteed to register as a state change, disarming the Start menu,
// even if Shift, Ctrl, or Alt are currently held down.
//
// old info:
// this way when winUP happens it won't pop up start menu
// this doesn't work in this one case only: if(in this order!) shift was held before winkey down then eg. MMB happened(so a gesture triggers) then you release shift, it pops startmenu!
// but it does work if you release winkey first, or if you hold winkey before shift, then you can release either and works!
//
// fixed now: using "Unassigned virtual key (vkE8)"(instead of RShift) as per Gemini 3.1 Pro 's suggestion did fix the above case ^!
func injectShiftTapOnly() {
	/*
		You are correctly not setting WVk when using KEYEVENTF_SCANCODE. Windows explicitly documents that when SCANCODE is set, WVk is ignored. Mixing them leads to inconsistent behavior on some builds.
	*/
	// inputs := []INPUT{
	// 	{
	// 		Type: INPUT_KEYBOARD,
	// 		Ki: KEYBDINPUT{
	// 			//WVk: VK_SHIFT, // don't, it's wrong to use vk instead of scancodes for Shift
	// 			//WVk: VK_E,
	// 			//WScan:   0x12, // scancode for 'E',
	// 			WScan:   0x36, // 0x2A is for Left Shift, and 0x36 is Right Shift scancode.
	// 			DwFlags: KEYEVENTF_SCANCODE,
	// 		},
	// 	},
	// 	{ // putting this after winUP below has same effect!
	// 		Type: INPUT_KEYBOARD,
	// 		Ki: KEYBDINPUT{
	// 			//WVk:     VK_SHIFT,
	// 			//WVk: VK_E,
	// 			//DwFlags: KEYEVENTF_KEYUP,
	// 			//WScan:   0x12, // 'E' key
	// 			WScan:   0x36, // 0x2A is for Left Shift, and 0x36 is Right Shift scancode.
	// 			DwFlags: KEYEVENTF_SCANCODE | KEYEVENTF_KEYUP,
	// 		},
	// 	},
	// }
	inputs := []INPUT{
		{
			Type: INPUT_KEYBOARD,
			Ki: KEYBDINPUT{
				WVk: 0xE8, // Unassigned virtual key (vkE8)
			},
		},
		{
			Type: INPUT_KEYBOARD,
			Ki: KEYBDINPUT{
				WVk:     0xE8,
				DwFlags: KEYEVENTF_KEYUP,
			},
		},
	}

	res1 := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)
	if res1.Failed() {
		logf("SendInput for injectShiftTapOnly failed: %v", res1.Err)
		//} else {
		//	logf("done injectShiftTapOnly")
	}
}

// injectShiftTapThenWinUp injects the vkE8 dummy tap(ie. down then up) [instead of RShift which had an edge case!] followed by the Win UP event.
// This prevents Start Menu from poping/showing up.
func injectShiftTapThenWinUp(whichWinUp uint16) {
	/*
		You are correctly not setting WVk when using KEYEVENTF_SCANCODE. Windows explicitly documents that when SCANCODE is set, WVk is ignored. Mixing them leads to inconsistent behavior on some builds.
	*/
	// inputs := []INPUT{
	// 	{
	// 		Type: INPUT_KEYBOARD,
	// 		Ki: KEYBDINPUT{
	// 			//WVk: VK_SHIFT, // don't, it's wrong to use vk instead of scancodes for Shift
	// 			//WVk: VK_E,
	// 			//WScan:   0x12, // scancode for 'E',
	// 			WScan:   0x36, // 0x2A is for Left Shift, and 0x36 is Right Shift scancode.
	// 			DwFlags: KEYEVENTF_SCANCODE,
	// 		},
	// 	},
	// 	{ // putting this after winUP below has same effect!
	// 		Type: INPUT_KEYBOARD,
	// 		Ki: KEYBDINPUT{
	// 			//WVk:     VK_SHIFT,
	// 			//WVk: VK_E,
	// 			//DwFlags: KEYEVENTF_KEYUP,
	// 			//WScan:   0x12, // 'E' key
	// 			WScan:   0x36, // 0x2A is for Left Shift, and 0x36 is Right Shift scancode.
	// 			DwFlags: KEYEVENTF_SCANCODE | KEYEVENTF_KEYUP,
	// 		},
	// 	},
	// 	{
	// 		Type: INPUT_KEYBOARD,
	// 		Ki: KEYBDINPUT{
	// 			WVk:     whichWinUp,
	// 			DwFlags: KEYEVENTF_KEYUP,
	// 		},
	// 	},
	// }
	inputs := []INPUT{
		{
			Type: INPUT_KEYBOARD,
			Ki: KEYBDINPUT{
				WVk: 0xE8, // Unassigned virtual key (vkE8)
			},
		},
		{
			Type: INPUT_KEYBOARD,
			Ki: KEYBDINPUT{
				WVk:     0xE8,
				DwFlags: KEYEVENTF_KEYUP,
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

	res1 := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)
	if res1.Failed() {
		logf("SendInput for injectShiftTapThenWinUp failed: %v", res1.Err)
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

	res1 := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)

	if res1.Failed() {
		logf("SendInput mouse click failed: %v", res1.Err)
	} else {
		//TODO: remove, temp.
		logf("Used LMB click to focus, caveat: target window got a LMB click at the point where you started the window move so it could've clicked an UI button!")
	}
}

const (
	MOUSEEVENTF_ABSOLUTE    = 0x8000
	MOUSEEVENTF_VIRTUALDESK = 0x4000
	MOUSEEVENTF_MOVE        = 0x0001
)

const (
	SM_XVIRTUALSCREEN  = 76
	SM_YVIRTUALSCREEN  = 77
	SM_CXVIRTUALSCREEN = 78
	SM_CYVIRTUALSCREEN = 79
)

func injectLMBClickAtCoords(x, y int32) {
	// SendInput absolute mouse coordinates use the entire virtual desktop,
	// not the primary monitor.
	//
	// Example:
	//
	//   [-1920,0] [0,0]
	//    Monitor2 Monitor1
	//
	// In that layout:
	//
	//   virtualLeft  = -1920
	//   virtualTop   = 0
	//   virtualWidth = 3840
	//   virtualHeight= 1080
	//
	// Therefore we must normalize relative to the virtual desktop origin,
	// not relative to (0,0).

	res1 := procGetSystemMetrics.Call(SM_XVIRTUALSCREEN)
	var virtualLeft int32 = int32(res1.R1)
	res1 = procGetSystemMetrics.Call(SM_YVIRTUALSCREEN)
	var virtualTop int32 = int32(res1.R1)
	res1 = procGetSystemMetrics.Call(SM_CXVIRTUALSCREEN)
	var virtualWidth int32 = int32(res1.R1)
	res1 = procGetSystemMetrics.Call(SM_CYVIRTUALSCREEN)
	var virtualHeight int32 = int32(res1.R1)

	// GetSystemMetrics has no distinguishable failure signal for these indices
	// (0 is returned both on legitimate "value is 0" and on any hypothetical
	// failure, and none of them set a meaningful GetLastError), so there's no
	// .Failed()-style check available here. SM_XVIRTUALSCREEN/SM_YVIRTUALSCREEN
	// legitimately go negative whenever a monitor extends up/left of the
	// primary, and legitimately sit at exactly 0 in the common single/aligned
	// monitor case, so neither "is negative" nor "is zero" can be used as a
	// failure heuristic for the origin values either. Instead, sanity-check
	// self-consistency: the reported origin must actually lie within the
	// reported width/height span, which catches OS/driver-level garbage
	// without misfiring on any legitimate multi-monitor layout.
	//
	// Width/height of 1 would make the rightmost/bottommost pixel also
	// be the leftmost/topmost pixel, so the normalization formula below
	// would divide by zero.
	if virtualWidth <= 1 || virtualHeight <= 1 {
		logf(
			"injectLMBClickAtCoords: invalid virtual desktop size %dx%d",
			virtualWidth,
			virtualHeight,
		)
		return
	}

	// Defensive: the origin must be finite and the resulting bounding box
	// must not overflow int32 arithmetic used below. This does not (and
	// cannot) detect a "failed" GetSystemMetrics call — it only catches an
	// internally inconsistent set of metrics, which would otherwise silently
	// produce garbage normalized coordinates.
	if virtualLeft > math.MaxInt32-virtualWidth || virtualTop > math.MaxInt32-virtualHeight {
		logf(
			"injectLMBClickAtCoords: virtual desktop metrics overflow int32 range: left=%d top=%d width=%d height=%d",
			virtualLeft, virtualTop, virtualWidth, virtualHeight,
		)
		return
	}

	// Convert desktop coordinates into coordinates relative to the
	// virtual desktop origin.
	//
	// Example:
	//
	//   virtualLeft = -1920
	//   x           = -100
	//
	// becomes:
	//
	//   relX = 1820
	//
	// which can then be normalized correctly.
	relX := x - virtualLeft
	relY := y - virtualTop

	// Defensive clamping.
	//
	// Today x/y originate from MSLLHOOKSTRUCT.Pt and should already be
	// inside the virtual desktop bounds.
	//
	// However, this function may eventually get reused from another
	// caller, so clamp coordinates before normalization.
	if relX < 0 {
		relX = 0
	} else if relX >= virtualWidth {
		relX = virtualWidth - 1
	}

	if relY < 0 {
		relY = 0
	} else if relY >= virtualHeight {
		relY = virtualHeight - 1
	}

	//Windows maps pixels to "mickeys"
	// Win32 absolute coordinates span 0..65535 inclusive.
	//
	// Using:
	//
	//   relX * 65535 / (width - 1)
	//
	// guarantees:
	//
	//   leftmost pixel  -> 0
	//   rightmost pixel -> 65535
	//
	// exactly.
	normalizedX := (relX * 65535) / (virtualWidth - 1)
	normalizedY := (relY * 65535) / (virtualHeight - 1)

	inputs := []INPUT{
		{
			Type: INPUT_MOUSE,
		},
		{
			Type: INPUT_MOUSE,
		},
	}

	// Move to target location and press LMB.
	m0 := (*MOUSEINPUT)(unsafe.Pointer(&inputs[0].Ki))
	m0.Dx = normalizedX
	m0.Dy = normalizedY
	m0.DwFlags =
		MOUSEEVENTF_ABSOLUTE |
			MOUSEEVENTF_VIRTUALDESK |
			MOUSEEVENTF_MOVE |
			MOUSEEVENTF_LEFTDOWN

	// Release LMB at the same location.
	m1 := (*MOUSEINPUT)(unsafe.Pointer(&inputs[1].Ki))
	m1.Dx = normalizedX
	m1.Dy = normalizedY
	m1.DwFlags =
		MOUSEEVENTF_ABSOLUTE |
			MOUSEEVENTF_VIRTUALDESK |
			MOUSEEVENTF_MOVE |
			MOUSEEVENTF_LEFTUP

	//you can "save and restore" the cursor position. Since GetCursorPos and SetCursorPos are extremely fast
	// and don't involve the message queue, this will happen so quickly (sub-millisecond) that the user won't perceive the jump.

	// Save the user's current cursor position.
	//
	// SendInput with MOUSEEVENTF_ABSOLUTE physically moves the cursor.
	// We restore it immediately afterwards so the click appears to happen
	// remotely without visibly teleporting the user's mouse.
	var currentPt POINT
	// 1. Capture current physical mouse position to restore it later
	resGetCursorPos := procGetCursorPos.Call(uintptr(unsafe.Pointer(&currentPt)))
	haveOriginalCursorPos := resGetCursorPos.Succeeded()
	if !haveOriginalCursorPos {
		logf("injectLMBClickAtCoords: GetCursorPos failed, err:%v; will not restore cursor position after the injected click (would otherwise teleport it to (0,0))", resGetCursorPos.Err)
	}
	// 2. Inject the click at the original gesture location
	res2 := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)

	//if err != nil || ret != uintptr(len(inputs)) {
	if res2.Failed() || res2.R1 != uintptr(len(inputs)) {
		logf(
			"injectLMBClickAtCoords: SendInput injected %d/%d events: %v",
			res2.R1,
			len(inputs),
			res2.Err,
		)
	}

	if haveOriginalCursorPos {
		// 3. Teleport the mouse back to where the user had it a millisecond ago
		res3 := procSetCursorPos.Call(
			//When SetCursorPos(X, Y) is called, Windows expects the X coordinate to be in the RCX register and Y to be in RDX.
			// Even though the arguments are 32-bit integers, Windows expects the entire 64-bit register to be properly sign-extended.
			// If the upper 32 bits contain garbage or are cleared to zero when they shouldn't be, the CPU behavior or the OS wrapper can misinterpret the value.
			// and that's why the 'inf' cast is needed. What inf? It's enough they're int32 cast to uintptr!
			// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
			uintptr(currentPt.X),
			// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
			uintptr(currentPt.Y),
		)

		//if restoreRet == 0 {
		if res3.Failed() {
			logf("injectLMBClickAtCoords: SetCursorPos failed: %v", res3.Err)
		}
	}
}

func injectLMBDown() {
	inputs := []INPUT{
		{
			Type: INPUT_MOUSE,
			Ki:   KEYBDINPUT{}, // union placeholder
		},
	}

	// Fill the union as MOUSEINPUT
	(*MOUSEINPUT)(unsafe.Pointer(&inputs[0].Ki)).DwFlags = MOUSEEVENTF_LEFTDOWN

	//Your inject (MOUSEEVENTF_LEFTDOWN): Defaults relative (Dx/Dy=0 = no move, click at current cursor).

	//SendInput is synchronous—blocks until inputs queued/processed by system. In WH_MOUSE_LL (global, synchronous chain), this blocks all mouse input until done.
	//SendInput is synchronous — blocks caller until inputs queued to system queue (not processed).
	res1 := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)

	//if err != nil || ret == 0 {
	if res1.Failed() || res1.R1 != uintptr(len(inputs)) {
		logf("SendInput mouse click failed: %v", res1.Err)
	} else {
		//TODO: remove, temp.
		logf("Injected LMB down, ret=%d err=%v", res1.R1, res1.Err)
	}
}

func initDPIAwareness() {
	// Try the modern API first (Win10 1607+).
	if procSetProcessDpiAwarenessContext.Find() == nil {
		res1 := procSetProcessDpiAwarenessContext.Call(
			DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2,
		)
		if res1.Succeeded() {
			return
		}
		// ERROR_ACCESS_DENIED means DPI awareness was already locked before main()
		// ran — most likely by an embedded application manifest in a .syso file.
		// This is expected and benign; log informationally and skip the fallback.
		if res1.ErrIs(windows.ERROR_ACCESS_DENIED) {
			logf("DPI awareness already set before main() (application manifest?); runtime initialization skipped.")
			return
		}
		logf("SetProcessDpiAwarenessContext failed (not manifest-locked), err:'%v'; trying shcore fallback.", res1.Err)
	}

	// Fallback: Windows 8.1+ shcore API.
	if procSetProcessDpiAwareness.Find() == nil {
		res2 := procSetProcessDpiAwareness.Call(PROCESS_PER_MONITOR_DPI_AWARE)
		if res2.Failed() {
			// SetProcessDpiAwareness returns an HRESULT, not a Win32 errno.
			// E_ACCESSDENIED (0x80070005) means DPI is already locked; this is
			// the HRESULT equivalent of the ERROR_ACCESS_DENIED check above.
			// Note: windows.ERROR_ACCESS_DENIED (errno 5) != windows.Errno(0x80070005),
			// so we must compare R1 directly rather than using ErrIs.
			const hresultEAccessDenied uintptr = 0x80070005
			//The hresultEAccessDenied constant is intentionally local to the function rather than a package-level constant, since it's a HRESULT interpretation of an API that's an exception to the project's general use of CheckBool/CheckErrno.
			if res2.R1 == hresultEAccessDenied {
				logf("DPI awareness (shcore fallback) already set before main() (application manifest?); skipping.")
				return
			}
			logf("SetProcessDpiAwareness PROCESS_PER_MONITOR_DPI_AWARE failed, err:'%v'", res2.Err)
		}
	}
}

func windowFromPoint(pt POINT) windows.Handle {
	// On amd64, Win32 passes POINT by value in a single 64-bit register.
	// Reinterpreting the 8-byte struct as a uintptr packs X (low 32 bits)
	// and Y (high 32 bits) exactly as the calling convention requires.
	// This is intentionally amd64-specific; see the //go:build constraint.
	res1 := procWindowFromPoint.Call(*(*uintptr)(unsafe.Pointer(&pt)))
	//if err != nil || ret == 0 {
	if res1.Failed() {
		return 0
	}
	res2 := procGetAncestor.Call(res1.R1, GA_ROOT)
	//if err2 != nil {
	if res2.Failed() {
		return 0 //kinda redundant because root == 0 if err2 != nil
	}
	return windows.Handle(res2.R1)
}

func getWindowPID(hwnd windows.Handle) uint32 {
	var pid uint32
	res1 := procGetWindowThreadProcessID.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(&pid)),
	)
	if res1.Failed() {
		logf("getWindowPID: GetWindowThreadProcessId failed for HWND=0x%X, err: %v", hwnd, res1.Err)
	}

	return pid
}

func isMaximized(hwnd windows.Handle) bool {
	var wp WINDOWPLACEMENT
	wp.Length = uint32(unsafe.Sizeof(wp))
	//"GetWindowPlacement is a synchronous query into USER32, but it does not send a message to the target window. It reads window state maintained by the window manager (the same data used by the shell for task switching)." -chatgpt5.2
	// so GetWindowPlacement does not block on a hung window.
	res1 := procGetWindowPlacement.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(&wp)),
	)
	//if r == 0 {
	if res1.Failed() {
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
	//defer windows.CloseHandle(hProc)
	defer closeHandleLogged(hProc, "processIntegrityLevel:OpenProcess hProc")

	var token windows.Token
	err = windows.OpenProcessToken(hProc, windows.TOKEN_QUERY, &token)
	if err != nil {
		return 0, fmt.Errorf("OpenProcessToken failed: %w", err)
	}
	//defer token.Close()
	defer closeHandleLogged(windows.Handle(token), "processIntegrityLevel:OpenProcessToken token")

	var needed uint32
	err = windows.GetTokenInformation(token, windows.TokenIntegrityLevel, nil, 0, &needed)
	if err == nil {
		return 0, fmt.Errorf("GetTokenInformation getting the proper size, succeeded but it's supposed to fail because we're passing 0 to get the proper size")
	}

	buf := make([]byte, needed)
	err = windows.GetTokenInformation(token, windows.TokenIntegrityLevel, &buf[0], needed, &needed)
	if err != nil {
		return 0, fmt.Errorf("GetTokenInformation after having size, failed: %w", err)
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
	if requiredLen := headerSize + int(offset) + 4; requiredLen > lenb {
		return 0, fmt.Errorf("SID subauthority count %d would read past end of token-information buffer (need %d bytes, have %d)", subCount, requiredLen, lenb)
	}
	ridPtr := (*uint32)(unsafe.Add(unsafe.Pointer(&buf[headerSize]), offset))
	rid := *ridPtr

	return rid, nil
}

/* ---------------- Tray ---------------- */

// appendMenuChecked wraps AppendMenuW with logging on failure; label is only
// used in the log message to identify which menu item failed.
func appendMenuChecked(hMenu, flags, id uintptr, textStr string) {
	text := mustUTF16(textStr)
	if res := procAppendMenu.Call(hMenu, flags, id, uintptr(unsafe.Pointer(text))); res.Failed() {
		logf("WM_MYSYSTRAY: AppendMenu failed for item with text %q, err=%v", textStr, res.Err)
	}
}

func initTray() error {
	if mainMsgHwnd == 0 {
		return fmt.Errorf("main message window is not initialized")
	}

	trayIcon.HWnd = mainMsgHwnd //doneFIXME: need to put this in a diff. variable so it doesn't depend on systray being inited! since it's used in other things!
	trayIcon.CbSize = uint32(unsafe.Sizeof(trayIcon))
	trayIcon.UID = 1
	// 2. Add NIF_SHOWTIP here to force Windows to show the SzTip text under Version 4
	trayIcon.UFlags = NIF_TIP | NIF_ICON | NIF_MESSAGE | NIF_SHOWTIP

	const IDI_APPLICATION = 32512

	res1 := procLoadIcon.Call(0, IDI_APPLICATION)
	if res1.Failed() {
		return fmt.Errorf("LoadIcon IDI_APPLICATION failed, err: %w", res1.Err)
	}
	trayIcon.HIcon = windows.Handle(res1.R1)
	trayIcon.UCallbackMessage = WM_MYSYSTRAY
	trayIcon.UTimeoutOrVersion = NOTIFYICON_VERSION_4

	tipText := selfName + " " + GetVersion()
	copy(trayIcon.SzTip[:], windows.StringToUTF16(tipText))

	//1 Add the tray icon
	res2 := procShellNotifyIcon.Call(NIM_ADD, uintptr(unsafe.Pointer(&trayIcon)))
	//if ret1 == 0 {
	if res2.Failed() {
		logf("Failed to add tray icon (real error): '%v'", res2.Err)
		// You could exitf or fallback here, but for now just log
	}

	//Set the version behavior
	//2, this must happen after NIM_ADD ! (bad chatgpt which suggested it before NIM_ADD)
	res3 := procShellNotifyIcon.Call(NIM_SETVERSION, uintptr(unsafe.Pointer(&trayIcon)))
	//if ret2 == 0 {
	if res3.Failed() {
		logf("NIM_SETVERSION for tray icon failed(are you on pre Windows Vista 2007?): '%v'", res3.Err)
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

	// Use the same trayIcon struct from initTray
	trayIcon.UFlags = 0 // NIM_DELETE ignores most fields, but set to be safe
	res1 := procShellNotifyIcon.Call(NIM_DELETE, uintptr(unsafe.Pointer(&trayIcon)))
	//ret is non-zero (success), but err can still be set
	//if ret == 0 {
	if res1.Failed() {
		logf("Failed to delete tray icon: %v", res1.Err) // optional, for debug
	} else {
		// Zero out the struct to avoid reuse confusion
		trayIcon = NOTIFYICONDATA{}
	}
}

func showTrayInfo(title, msg string) {
	//FIXME: this call should be rate-limited, or the callers of it should be.

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
	if res1 := procShellNotifyIcon.Call(NIM_MODIFY, uintptr(unsafe.Pointer(&trayIcon))); res1.Failed() {
		logf("Failed to update tray icon info: %v", res1.Err)
	}
}

/* ---------------- Drag Logic ---------------- */

func startManualDrag(hwnd windows.Handle, pt POINT, viaMissedGestureRecovery, wasMaximized bool, preRestoreRect RECT) bool {
	if cur := activeSession.Load(); cur != nil {
		logf("unexpected startManualDrag while already having an activeSession(either drag-move or resizing) mode:%d", cur.mode)
		return false
	}

	var r RECT
	res1 := procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r)))
	if res1.Failed() {
		logf("GetWindowRect on target HWND=0x%X failed for move startup, err:%v", hwnd, res1.Err)
		return false
	}

	if wasMaximized {
		// Compute a top-left so the cursor sits at the same proportional
		// position within the restored window as it had within the maximized one.
		r = alignRestoredWindowToCursor(pt, preRestoreRect, r)
		// Reposition immediately, before the first WM_MOUSEMOVE arrives.
		if res := procSetWindowPos.Call(
			uintptr(hwnd),
			0, // ignored due to SWP_NOZORDER
			// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
			uintptr(r.Left),
			// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
			uintptr(r.Top),
			0, 0, // ignored due to SWP_NOSIZE
			SWP_NOSIZE|SWP_NOZORDER|SWP_NOACTIVATE,
		); res.Failed() {
			logf("SetWindowPos (post-restore alignment) on HWND=0x%X failed: %v; re-reading rect for consistent drag origin", hwnd, res.Err)
			// Re-read the rect so startPt arithmetic stays consistent with
			// wherever the window actually landed.
			if res2 := procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r))); res2.Failed() {
				logf("GetWindowRect (post-SetWindowPos failure) on HWND=0x%X also failed: %v", hwnd, res2.Err)
				// r is still the aligned value; better than nothing.
			}
		}
	}

	activeSession.Store(&dragSession{
		targetWnd:                hwnd,
		mode:                     ModeMove,
		state:                    dragState{startPt: pt, startRect: r},
		viaMissedGestureRecovery: viaMissedGestureRecovery,
	})
	return true
}

func startDrag(hwnd windows.Handle, pt POINT, viaMissedGestureRecovery bool) bool {
	pid := getWindowPID(hwnd)
	targetIL, e1 := processIntegrityLevel(pid)

	if e1 == nil && targetIL > selfIntegrityLevel {
		//XXX: this actually never gets reached because windows doesn't allow winbollocks to see the events(while higher itegrity window is focused) thus the gesture to drag it can never trigger!
		procName := getProcessNameFast(pid)
		showTrayInfo(selfName, fmt.Sprintf("Cannot use native drag on elevated window with pid=%d (%s)", pid, procName))
		return false
	}
	if e1 != nil {
		logf("startDrag:processIntegrityLevel failed, but continuing, err was: %v", e1)
	}

	var preRestoreRect RECT
	wasMaximized := isMaximized(hwnd)
	if wasMaximized {
		// Capture the maximized rect before restoring so alignRestoredWindowToCursor
		// can compute the proportional cursor position within the restored window.
		if res := procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&preRestoreRect))); res.Failed() {
			logf("GetWindowRect (pre-restore) on HWND=0x%X failed: %v; cursor alignment after restore will be skipped", hwnd, res.Err)
			wasMaximized = false // skip alignment rather than use a zero rect
		}
		_ = procShowWindow.Call(uintptr(hwnd), SW_RESTORE)
		//TODO: should I re-maximize if it was maximized, after drag/move is done? probably not!
	}
	return startManualDrag(hwnd, pt, viaMissedGestureRecovery, wasMaximized, preRestoreRect)
}

// applyFocusAndBringToFrontOnGestureStart optionally brings targetWnd to the
// front of the Z-order and/or focuses it, right after a move or resize
// gesture has successfully started. bringToFront and focus are the
// systray-toggleable settings for whichever gesture mode just started
// (ModeMove passes &bringToFrontOnDrag/&focusOnDrag; ModeResize passes its
// own independent &bringToFrontOnResize/&focusOnResize), so the two modes
// remain fully independently configurable rather than sharing state.
// callerName is only used to identify the caller in the WM_BRING_TO_FRONT
// failure log.
func applyFocusAndBringToFrontOnGestureStart(targetWnd windows.Handle, pt POINT, bringToFront, focus *atomic.Bool, callerName string) {
	if bringToFront.Load() {
		// Post a dedicated bring-to-front message rather than routing through
		// the move channel, which would be coalesced away by move events for
		// the same HWND.
		if res := procPostMessage.Call(uintptr(mainMsgHwnd), WM_BRING_TO_FRONT, uintptr(targetWnd), 0); res.Failed() {
			logf("%s: PostMessage WM_BRING_TO_FRONT for HWND=0x%X failed: %v", callerName, targetWnd, res.Err)
		}
	}
	if focus.Load() && !isWindowForeground(targetWnd) { //TODO: should I move this in startDrag?
		//doneFIXME: should probably embed the targetWnd into the message instead of using whichever the current dragged window is, otherwise it might miss focusing the clicked window due to delays in processing if a new window was quick-engouh clicked since!

		if res := procPostMessage.Call(
			uintptr(mainMsgHwnd),
			WM_FOCUS_TARGET_WINDOW_SOMEHOW,
			uintptr(targetWnd),     // wParam
			makeLParam(pt.X, pt.Y), // lParam contains X and Y
		); res.Failed() {
			logf("%s: PostMessage WM_FOCUS_TARGET_WINDOW_SOMEHOW for HWND=0x%X failed: %v", callerName, targetWnd, res.Err)
		}
	}
}

// tryBeginMoveGestureAt is the shared "start (or restart) a window-move drag
// targeting whatever window is under pt" logic. Used both by the real
// WM_LBUTTONDOWN handler (pt = actual click point) and by the missed-gesture
// recovery path from WM_MOUSEMOVE (pt = current mouse position, since we
// don't know where the swallowed click actually happened).
//
// If a ModeMove session is already active, it is ALWAYS torn down and a
// fresh one started at pt/GetWindowRect-now, whether or not it targets the
// same window. This covers:
//   - a genuine duplicate/doubled LMB-down for the SAME in-progress drag
//     (ie. for wtw reasons!) — harmless, since pt/rect haven't meaningfully
//     changed, so the restart is imperceptible.
//   - a stale session surviving a winkey+L lock/unlock cycle — previously
//     this branch returned early without restarting, silently freezing the
//     drag until LMB was released and re-pressed. Now fixed to match the
//     ModeResize/RMB path, which already did this correctly.
//
// If a ModeResize session is active instead, this is a no-op (finish the
// resize first), matching prior behavior.
//
// Returns (started, bypassed). bypassed is true only when the target window
// under pt is fullscreen and bypassGesturesWhenFullscreen is enabled (see
// shouldBypassGestureNow); callers must treat that case as "let the
// originating input event pass through unswallowed" rather than as an
// ordinary failure. started is false for any other reason nothing began (no
// window under pt, a resize is already running, startDrag failed, etc.) —
// callers should still swallow the originating input in that case.
//
// viaMissedGestureRecovery must be true only when called from the
// missed-gesture recovery path (we never saw/swallowed the real LMB-down),
// and false when called from the real WM_LBUTTONDOWN handler (we did). It's
// stored on the resulting dragSession — see dragSession.viaMissedGestureRecover
func tryBeginMoveGestureAt(pt POINT, viaMissedGestureRecovery bool) (started, bypassed bool) {
	wantTargetWnd := windowFromPoint(pt)
	if wantTargetWnd == 0 {
		logf("Invalid window, window-move gesture skipped but LMB eaten and start menu will still be prevented(now even if you LMB on a higher integrity eg. admin window before you release winkey)")
		return false, false
	}

	if shouldBypassGestureNow(wantTargetWnd) {
		return false, true
	}

	if session := activeSession.Load(); session != nil {
		if session.mode != ModeMove {
			logf("Warning: Ignoring new move gesture because %v mode is already running on HWND=0x%X", session.mode, session.targetWnd)
			return false, false
		}

		//XXX: (might be obsolete comment:)happens when winkey+LMB then winkey+L to lock, release all, unlock, (now if u move mouse it no longer drags but)
		// if you now start to hold winkey(it will drag if you move mouse) and then press(or hold) LMB (you're here) and
		// move mouse while LMB is held it continues to drag/move that same window. Also covers a genuine doubled/duplicate
		// LMB-down event for the SAME in-progress drag (ie. for wtw reasons!) - restarting fresh from the current pt/rect
		// is indistinguishable from a no-op in that case since nothing has meaningfully moved between the two events.
		logf("already drag-moving a window, means you were moving a window then pressed winkey+L then released all then unlocked session then held winkey(again) " +
			"and pressed(or held) LMB (on same or new window target!) thus you're now here.")

		if session.targetWnd == 0 {
			panic("impossible state(while single-threaded win32 app in 20feb2026), logic error: you were drag-moving " +
				"but targetWnd wasn't set to anything(ie. it's 0) but shoulda been set to prev. window!")
		}
		// now non zero targetWnd
		//capturing means you already were dragging a prev. window, reflected by targetWnd not being 0!

		//now, is it a new window you're trying to drag or the same old one?
		// if it's same old one, the dragging is still thought to happen (if winkey is held down anew before moving mouse, else you'd not be here), so don't start a new drag?
		// if it's new, have to softReset() first because otherwise it will still drag the old one! and let it start drag again?

		if session.targetWnd == wantTargetWnd {
			//same old window
			//logf("continuing to drag-move same old window HWND=0x%X from the same old initial coords(ie. you'll see a snap-move first!)", session.targetWnd)
			logf("Resetting drag coordinates for same window HWND=0x%X to prevent cursor snap-back", session.targetWnd)
		} else {
			//a new window
			// it's a drag of a new window but we were moving the old window before that and didn't stop (for winkey+L reason for example!)
			logf("Avoided moving the old window HWND=0x%X ie. you were moving a window while winkey+L happened, now you unlocked session and you're newly holding winkey "+
				"but you LMB-ed on ANOTHER window(ie. trying to move another window), so we're not gonna move the old window anymore but the new one!", session.targetWnd)
			logf("drag-moving new window HWND=0x%X instead of the old one HWND=0x%X", wantTargetWnd, session.targetWnd)
		}
		softReset(true)
	}
	//FIXME: so we start the drag before doing the focus(which is below via WM_FOCUS_TARGET_WINDOW_SOMEHOW), works but seems off this way, not visually tho! but might be needed so we can setcapture to self else target might have/set capture(unsure)?!
	if !startDrag(wantTargetWnd, pt, viaMissedGestureRecovery) {
		return false, false
	}
	//so startDrag succeeded if we're here
	session := activeSession.Load()
	if session == nil {
		panic("bad coding: nil session after startDrag returned true")
	}
	applyFocusAndBringToFrontOnGestureStart(session.targetWnd, pt, &bringToFrontOnDrag, &focusOnDrag, "tryBeginMoveGestureAt")
	return true, false
}

// tryBeginResizeGestureAt is tryBeginMoveGestureAt's ModeResize counterpart,
// used by both the real WM_RBUTTONDOWN handler and the missed-gesture
// recovery path from WM_MOUSEMOVE. See tryBeginMoveGestureAt's doc comment
// for the (started, bypassed) return-value contract.
func tryBeginResizeGestureAt(pt POINT, viaMissedGestureRecovery bool) (started, bypassed bool) {
	wantTargetWnd := windowFromPoint(pt)
	if wantTargetWnd == 0 {
		logf("Invalid window, window-resize gesture skipped but RMB eaten and start menu will still be prevented(now even if you RMB on a higher integrity eg. admin window before you release winkey)")
		return false, false
	}

	if shouldBypassGestureNow(wantTargetWnd) {
		return false, true
	}

	if session := activeSession.Load(); session != nil {
		if session.mode != ModeResize {
			logf("Warning: Ignoring new resize gesture because %v mode is already running on HWND=0x%X", session.mode, session.targetWnd)
			return false, false
		}

		logf("already resizing a window, likely due to a Win+L lock interruption, rapid click overlay, or a duplicate RMB-down event.")
		if session.targetWnd == 0 {
			panic("impossible state: logic error: session is ModeResize but targetWnd is 0!")
		}
		// now, check if it's a new window or the same old one we were resizing
		if session.targetWnd == wantTargetWnd {
			// Same window case
			logf("Resetting resize coordinates for same window HWND=0x%X to prevent cursor snap-back", session.targetWnd)
			// doneFIXME FIXED: Instead of allowing a snap-move from stale coordinates,
			// we softReset(true) so the logic falls through (or restarts) using
			// the current cursor position as the brand new origin.
		} else {
			// New window case
			logf("Avoided resizing stale window HWND=0x%X. Switching to new window HWND=0x%X.", session.targetWnd, wantTargetWnd)
			// Let it fall through to initialize a brand new resize session for 'wantTargetWnd'
		}
		softReset(true)
	}

	// Capture the maximized rect before restoring so alignRestoredWindowToCursor
	// can compute the proportional cursor position within the restored window.
	var preRestoreRect RECT
	wasMaximized := isMaximized(wantTargetWnd)

	if wasMaximized {
		if res := procGetWindowRect.Call(uintptr(wantTargetWnd), uintptr(unsafe.Pointer(&preRestoreRect))); res.Failed() {
			logf("GetWindowRect (pre-restore) on HWND=0x%X failed: %v; cursor alignment after restore will be skipped", wantTargetWnd, res.Err)
			wasMaximized = false // skip alignment rather than use a zero rect
		}

		// Restore the window first so the resize starts from, and is measured
		// against, the non-maximized rect. Without this the OS leaves the window
		// in a mixed state (visually resized but still flagged as maximized).
		_ = procShowWindow.Call(uintptr(wantTargetWnd), SW_RESTORE)
	}

	var r RECT
	res1 := procGetWindowRect.Call(uintptr(wantTargetWnd), uintptr(unsafe.Pointer(&r)))
	if res1.Failed() {
		logf("GetWindowRect on target HWND=0x%X failed(ret is 0) for resize startup, err:%v", wantTargetWnd, res1.Err)
		return false, false
	}

	// Reposition the restored window under the cursor BEFORE setting up the resize session
	if wasMaximized {
		r = alignRestoredWindowToCursor(pt, preRestoreRect, r)
		if res := procSetWindowPos.Call(
			uintptr(wantTargetWnd),
			0, // ignored due to SWP_NOZORDER
			// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
			uintptr(r.Left),
			// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
			uintptr(r.Top),
			0, 0, // ignored due to SWP_NOSIZE
			SWP_NOSIZE|SWP_NOZORDER|SWP_NOACTIVATE,
		); res.Failed() {
			logf("SetWindowPos (post-restore alignment) on HWND=0x%X failed: %v; re-reading rect for consistent resize origin", wantTargetWnd, res.Err)
			if res2 := procGetWindowRect.Call(uintptr(wantTargetWnd), uintptr(unsafe.Pointer(&r))); res2.Failed() {
				logf("GetWindowRect (post-SetWindowPos failure) on HWND=0x%X also failed: %v", wantTargetWnd, res2.Err)
			}
		}
	}

	w := r.Right - r.Left
	h := r.Bottom - r.Top
	if w <= 0 || h <= 0 {
		logf("Refusing resize start: invalid window size %dx%d gotten for target HWND=0x%X", w, h, wantTargetWnd)
		return false, false
	}
	activeSession.Store(&dragSession{
		targetWnd: wantTargetWnd,
		mode:      ModeResize,
		state:     dragState{startPt: pt, startRect: r},

		resizeZone:               getResizeZone(pt, r),
		initialAspectRatio:       float64(w) / float64(h),
		viaMissedGestureRecovery: viaMissedGestureRecovery,
	})
	session := activeSession.Load() //weird way to do this Claude Sonnet 5 Extra Thinking (yes Extra this time), because who needs DRY!?!
	if session == nil {
		panic("bad coding: nil session after storing new resize session")
	}
	applyFocusAndBringToFrontOnGestureStart(session.targetWnd, pt, &bringToFrontOnResize, &focusOnResize, "tryBeginResizeGestureAt")
	return true, false
}

// tryPerformMMBGestureAt is MMB's counterpart to tryBeginMoveGestureAt /
// tryBeginResizeGestureAt: resolves the window a winkey+MMB (shiftDown=false,
// send-to-back) or winkey+shift+MMB (shiftDown=true, bring-to-front) gesture
// would act on, applies the same live fullscreen-bypass check (see
// shouldBypassGestureNow) against that resolved window, and if not bypassed
// submits the Z-order change (throttled the same way the real-time handler
// always was). Used by both the real WM_MBUTTONDOWN handler and the
// missed-gesture recovery path in WM_MOUSEMOVE, so winkey+MMB now recovers
// from a higher-UIPI foreground window the same way winkey+LMB/RMB already
// did.
//
// Unlike the move/resize gestures, MMB has no persistent dragSession — it's
// a single, immediate Z-order change — so there's no activeSession
// interaction here.
//
// Returns (started, bypassed) with the same contract as
// tryBeginMoveGestureAt: bypassed means "let the originating input event
// pass through unswallowed"; started is false for any other reason nothing
// was done (no resolvable target window) and callers should still swallow
// the input, same as before this function existed. A throttled/dropped
// attempt (see ShouldThrottle) still counts as started=true, matching the
// original handler's silent-drop behavior (no failure is logged for it).
func tryPerformMMBGestureAt(pt POINT, shiftDown bool) (started, bypassed bool) {
	var hwnd windows.Handle
	if !shiftDown {
		// winkey + MMB -> send window under cursor to bottom of Z-order
		hwnd = windowFromPoint(pt) // window under cursor
	} else {
		// winkey + shift + MMB -> bring currently focused window to top
		/*
					Based on how the Windows API behaves, procGetForegroundWindow should remain CheckNone rather than CheckNull.

			Here is a breakdown of why treating a NULL (0) return value from GetForegroundWindow as an API failure (which CheckNull typically does) will cause bugs in your application:
			1. NULL is a valid, normal state

			According to Microsoft's documentation for GetForegroundWindow:

			    "The return value is a handle to the foreground window. The foreground window can be NULL in certain circumstances, such as when a window is losing activation."

			Other common scenarios where it returns NULL (0) include:

			    The workstation is locked (Ctrl+Alt+Del or Win+L).

			    A screen saver is active.

			    The system is in the middle of a window-switching transition.

			    A full-screen exclusive application (like some games) is changing display modes.

			Because NULL is a legitimate state meaning "there is currently no foreground window," your Go code should handle this as a normal logic branch (e.g., skipping an action or retrying later), rather than treating it as an exceptional system failure.

			2. It does not set GetLastError

			Functions that use CheckNull usually assume that if the function returns 0, it failed, and they will automatically call syscall.GetLastError() to append a descriptive error message.

			However, GetForegroundWindow does not set an error code via SetLastError. If it returns NULL, calling GetLastError will return either 0 (The operation completed successfully.) or a stale error left over from a completely unrelated previous system call. This leads to confusing log pollution or false-positive panics.

			- Gemini 3.5 Thinking
		*/
		res1 := procGetForegroundWindow.Call() // whichever the currently focused window is, wherever it is
		// procGetForegroundWindow is bound with wincoe.CheckNone (GetForegroundWindow has no
		// real failure signal beyond returning NULL), so res1.Failed() can never be true here;
		// check R1 directly, matching GetForegroundWindow's documented NULL-on-failure contract.
		if res1.R1 == 0 {
			logf("Couldn't get currently focused window for the purposes of bringing it to front for winkey+shift+MMB gesture ergo aborting attempt, err=%v callStatus=%v r1=%v", res1.Err, res1.CallStatus, res1.R1)
			return false, false
		}
		hwnd = windows.Handle(res1.R1)
	}

	if hwnd == 0 {
		if !shiftDown {
			logf("hwnd == 0 for winkey+MMB (send to back) thus nothing was done!")
		} else {
			logf("hwnd == 0 for winkey+shift+MMB (bring focused window to front) thus nothing was done!")
		}
		return false, false
	}

	if shouldBypassGestureNow(hwnd) {
		return false, true // foreground is fullscreen; let event through
	}

	if !ShouldThrottle() {
		//data := new(WindowMoveData) // Heap-allocated, TODO: fix this the same way as for mouse move event!
		var data WindowMoveData // stack allocated — zero cost
		if !shiftDown {
			// winkey + MMB → send active window to bottom

			// Send to back, no activation
			// if you do this for a focused window then no amount of LMB will bring it back to front unless it loses focus first!

			// winkey_DOWN but no other modifiers(including shift) is down
			// and LMB is down, ofc, then we start move window gesture:
			data.InsertAfter = HWND_BOTTOM
			data.Flags = SWP_NOMOVE | SWP_NOSIZE | SWP_NOACTIVATE
		} else {
			// winkey + shift + MMB → bring focused window to top

			// shift is down too, so winkey_DOWN and shiftDOWN and LMB are down
			// but no other modifiers like ctrl or alt are down
			// then we start the bring focused window to front gesture:
			data.InsertAfter = HWND_TOP
			data.Flags = SWP_NOMOVE | SWP_NOSIZE
			// Bring to front, no activation, works only for the currently focused window which was sent to back before
		}
		data.Hwnd = hwnd // window under cursor
		data.X = 0       // int32, full range
		data.Y = 0
		enqueueMoveOrResize(data, "MMB gesture (winkey+MMB or winkey+shift+MMB, direct or recovered)")
	} else { // endif every 10ms or more, else drop it
		droppedMoveOrResizeEvents.Add(1) //TODO: use diff. one to keep track of drops due to too-fast thus not-queued
	}

	return true, false
}

// this is the heap-allocation one, presumably - says Gemini 3.1 Pro
func keyDown1(vk uintptr) bool {
	res1 := procGetAsyncKeyState.Call(vk)
	// if res1.R1==0 { //|| res1.Failed() { it's CheckNone so Failed has no meaning here! actually not sure if R1==0 makes any sense as a failure, seems to mean they're all UP
	// 	logf("keyDown: procGetAsyncKeyState failed for vk:%v", vk)
	// }
	return (res1.R1 & 0x8000) != 0
}

// Add this raw proc near your other globals
var rawGetAsyncKeyState = user32.NewProc("GetAsyncKeyState")

// this is the non-heap allocating one as per Gemini 3.1 Pro
func keyDown(vk uintptr) bool {
	// By calling rawGetAsyncKeyState.Call directly, we bypass the LazyProcish
	// interface in BoundProc. The Go compiler's special magic kicks in,
	// the args stay on the stack, and we drop thousands of heap allocations to ZERO.
	ret, _, _ := rawGetAsyncKeyState.Call(vk) //nolint:errcheck // it's void-like?! TODO: double-check
	_ = ret

	// if ret == 0 {//XXX: actually not sure if R1==0 makes any sense as a failure, seems to mean they're all UP
	// 	logf("keyDown: rawGetAsyncKeyState failed for vk:%v", vk)
	// }
	return (ret & 0x8000) != 0
}

/* so keyDown1 vs keyDown now: (unsure what to believe)
You are 100% right to call me out on that! You've got a very sharp eye. It *is* variadic, and normally, any variadic call `f(a, b)` gets rewritten by the compiler into a slice allocation: `f([]uintptr{a, b})`.

So if both functions take a variadic slice, why does yours hit the heap while Go's standard library stays on the stack?

It comes down to **how the compiler handles that exact directive at the immediate call site**, and why your wrapper architecture inadvertently breaks it.

---

### 1. How the `//go:uintptrescapes` Magic Handles Slices

When the Go compiler's escape analysis engine encounters a regular variadic function, it has to evaluate whether the resulting slice escapes.

However, when it sees a function marked with `//go:uintptrescapes` (like `LazyProc.Call`), the compiler activates a highly specialized, hardcoded behavior:

* Instead of treating `a ...uintptr` as a standard slice that could go anywhere, the compiler looks at the call site (`rawGetAsyncKeyState.Call(vk)`) and says: *"I know this function. It's a low-level syscall hook. It is guaranteed to read these arguments instantly and never store this slice context."*
* Because of this guarantee, the compiler **explicitly stack-allocates the hidden backing array for that variadic slice** right in the caller's stack frame.

So yes, a slice is created, but it is created directly on the stack and disappears the moment `keyDown` returns. Zero heap allocations.

---

### 2. Why the Magic Breaks in Your Wrapper

If you were to just put `//go:uintptrescapes` on your custom `BoundProc.Call`, it still wouldn't work. The compiler's magic optimization breaks for two reasons in your architecture:

#### Reason A: The Interface Boundary

Your wrapper uses an interface (`LazyProcish`). The Go compiler performs escape analysis during compilation. When it looks at your wrapper call site:

```go
func (b *BoundProc) Call(args ...uintptr) WinResult {
    return WinCall(b.Proc, b.Check, args...) // b.Proc is an interface!
}

```

The compiler doesn't know *which* concrete struct is behind `b.Proc` at compile time. Because it can't guarantee that the underlying method down the road will respect the stack layout, it plays it safe, gives up, and pushes the variadic slice to the heap.

#### Reason B: The Double-Forwarding Trap

The `//go:uintptrescapes` directive **is not transitive**. It only optimizes the *immediate* call site where the pointer/integer conversion or slice creation happens.

In your code, you have a multi-level chain:

1. `keyDown1` calls `procGetAsyncKeyState.Call(vk)` -> **Level 1: Slice created.**
2. `BoundProc.Call` receives it and calls `WinCall(..., args...)` -> **Level 2: Slice forwarded.**

The moment a variadic slice is passed down through multiple layers of user functions like a hot potato, the escape analysis engine loses track of the stack boundaries and forces it onto the heap.

---

### The Bottom Line

By bypassing the interface and the wrapper functions, and calling `rawGetAsyncKeyState.Call(vk)` directly inside `keyDown`, you bring the call site **directly face-to-face** with the compiler's `//go:uintptrescapes` directive.

The compiler instantly recognizes it, creates the variadic backing array right on the stack, and your ghost allocations completely vanish!
- Gemini 3.5 Thinking
*/

func softReset(releaseCapture bool) { //nevermindTODO: use hardReset instead(well no, because it also resets winGestureUsed!) because it now handles the case when Shift tap needs to be inserted if winGestureUsed !
	//do this first
	activeSession.Store(nil) //XXX: don't set the innards to nil like state and targetWnd ! because old pointer's contents may still be used by other threads; this is Lock-Free Snapshot or Read-Copy-Update (RCU) pattern.
	captureHeldForSession.Store(nil)
	/*
		The Problem: If you call it in the hook, you are releasing capture on the Hook Thread. But window capture is thread-specific.
		If your SetCapture was originally called by the Main Thread (which is usually where windows and UI live),
		calling ReleaseCapture from the Hook Thread might not work the way you expect, or could lead to an inconsistent state where the OS
		thinks Thread A has it but Thread B tried to kill it.

		actually it is my hook thread that calls SetCapture in 2 places one for move and one for resize!
	*/
	if releaseCapture {
		if mainMsgHwnd != 0 {
			if res := procPostMessage.Call(uintptr(mainMsgHwnd), WM_DO_RELEASE_CAPTURE, 0, 0); res.Failed() {
				logf("softReset: PostMessage WM_DO_RELEASE_CAPTURE failed: %v", res.Err)
			}
		} else {
			// fallback, but should rarely hit
			logf("mainMsgHwnd is 0 in softReset when trying to send a WM_DO_RELEASE_CAPTURE, falling back to calling ReleaseCapture now!")
			if res := procReleaseCapture.Call(); res.Failed() {
				logf("softReset: fallback ReleaseCapture failed: %v", res.Err)
			}
		}
	}

	//hideOverlay() //doneFIXME: move this to wndProc ! else u hit stutter7 occasionally!
	// Instead of calling hideOverlay() synchronously on the hook thread,
	// post it asynchronously to your main thread's message window loop.
	if mainMsgHwnd != 0 {
		if res := procPostMessage.Call(uintptr(mainMsgHwnd), WM_HIDE_OVERLAY, 0, 0); res.Failed() {
			logf("softReset: PostMessage WM_HIDE_OVERLAY failed: %v", res.Err)
		}
		// } else {
		// 	logf("unexpected: failed to hideOverlay due to mainMsgHwnd being 0, this gets hit if it's already running.")
	}
}

func hardReset(releaseCapture bool) {
	var winDown bool = keyDown(VK_LWIN) || keyDown(VK_RWIN)
	if winGestureUsed.Load() && winDown {
		injectShiftTapOnly() // this way when winUP happens it won't pop up start menu
		//alreadydoingitTODO: inject shift tap at the time gesture is detected!
		winGestureUsed.Store(false)
	}
	softReset(releaseCapture)
}

// Define the overlay window class name as a constant
const winbollocksResizingOverlayClassName = selfName + "ResizingOverlayClass" //winbollocksResizingOverlayClass //TODO: see if underscores work in this!
const winbollocksHiddenClassName = selfName + "Hidden"                        // winbollocksHidden
const selfName = "winbollocks"

var hiddenClassRegistered atomic.Bool
var overlayClassRegistered atomic.Bool

func initOverlay() error {
	className := mustUTF16(winbollocksResizingOverlayClassName)
	//Both Windows APIs just read the null-terminated UTF-16 string from that memory address during the call; they don't seize ownership or modify it.

	var wc WNDCLASSEX
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	wc.LpfnWndProc = windows.NewCallback(overlayWndProc)
	wc.LpszClassName = className
	wc.HInstance = selfHInstance
	// Add shadow/background if desired, but we'll paint it

	if res1b := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); res1b.Failed() {
		return fmt.Errorf("RegisterClassEx failed in initOverlay(), err: %w", res1b.Err)
	} else {
		overlayClassRegistered.Store(true)
	}

	res2 := procCreateWindowEx.Call(
		WS_EX_LAYERED|WS_EX_TRANSPARENT|WS_EX_TOOLWINDOW|WS_EX_TOPMOST,
		uintptr(unsafe.Pointer(className)),
		0,
		WS_POPUP,
		0, 0, 400, 100, // Size will be updated dynamically
		0, 0,
		uintptr(wc.HInstance),
		0,
	)
	if res2.Failed() {
		return fmt.Errorf("failed procCreateWindowEx() in initOverlay(), err: %w", res2.Err)
	}

	overlayHwnd = windows.Handle(res2.R1 /*aka hwndRaw*/)

	// Set Magenta (0x00FF00FF) as the transparent color key, and 200/255 opacity for the rest
	if resLayered := procSetLayeredWindowAttributes.Call(uintptr(overlayHwnd), 0x00FF00FF, 220, LWA_COLORKEY|LWA_ALPHA); resLayered.Failed() {
		logf("initOverlay: SetLayeredWindowAttributes failed, err: %v; overlay will lack its transparent color-key/opacity, continuing anyway", resLayered.Err)
	}

	// Create our reusable GDI brushes once
	res3 := procGdiCreateSolidBrush.Call(0x00FF00FF)
	if res3.Failed() {
		return fmt.Errorf("failed procGdiCreateSolidBrush() in initOverlay(), err: %w", res3.Err)
	}
	magentaBrush = windows.Handle(res3.R1 /*aka hMag*/)

	res4 := procGdiCreateSolidBrush.Call(0x00000000)
	if res4.Failed() {
		return fmt.Errorf("failed procGdiCreateSolidBrush() in initOverlay(), err: %w", res4.Err)
	}
	blackBrush = windows.Handle(res4.R1 /*aka hBlk*/)

	return nil
}

const WM_PAINT = 0x000F

func overlayWndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr /*aka LRESULT*/ {
	if msg == WM_PAINT {
		var ps PAINTSTRUCT
		res1 := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		if res1.Failed() {
			logf("WM_PAINT in overlayWndProc, BeginPaint() failed, err: %v, ignoring the rest of the paint.", res1.Err)
			return 0 //handled
		}
		hdc := res1.R1

		var rect RECT
		res2 := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rect)))
		if res2.Failed() {
			logf("WM_PAINT in overlayWndProc, GetWindowRect() failed, err: %v, ignoring the rest of the paint.", res2.Err)
			return 0 //handled
		}
		rect.Right -= rect.Left
		rect.Left = 0
		rect.Bottom -= rect.Top
		rect.Top = 0

		// 1. Fill background with our global Magenta brush (Transparent Key)
		res3 := procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rect)), uintptr(magentaBrush))
		if res3.Failed() {
			logf("WM_PAINT in overlayWndProc, FillRect() failed, err: %v, ignoring the rest of the paint.", res3.Err)
			return 0 //handled
		}

		// 2. Draw black text box background for visibility with our global Black brush
		res3 = procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rect)), uintptr(blackBrush))
		if res3.Failed() {
			logf("WM_PAINT in overlayWndProc, FillRect() failed, err: %v, ignoring the rest of the paint.", res3.Err)
			return 0 //handled
		}

		// 3. Draw Text
		res4 := procGdiSetTextColor.Call(hdc, 0x0000FF00) // Green text
		if res4.Failed() {
			logf("WM_PAINT in overlayWndProc, GdiSetTextColor() failed, err: %v, ignoring the rest of the paint.", res4.Err)
			return 0 //handled
		}
		res5 := procGdiSetBkMode.Call(hdc, 1) // TRANSPARENT background for text
		if res5.Failed() {
			logf("WM_PAINT in overlayWndProc, GdiSetBkMode() failed, err: %v, ignoring the rest of the paint.", res5.Err)
			return 0 //handled
		}

		textPtr := mustUTF16(overlayText)
		res6 := procDrawText.Call(hdc, uintptr(unsafe.Pointer(textPtr)), ^uintptr(0), uintptr(unsafe.Pointer(&rect)), 0x24) // DT_CENTER | DT_VCENTER | DT_SINGLELINE
		if res6.Failed() {
			logf("WM_PAINT in overlayWndProc, DrawText() failed, err: %v, ignoring the rest of the paint.", res6.Err)
			return 0 //handled
		}

		if res7 := procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps))); res7.Failed() {
			logf("WM_PAINT in overlayWndProc, EndPaint() failed, err: %v", res7.Err)
			return 0 //handled; keep this dup line, in case I insert something between this and the last return in the block, and i forget to put one return here
		}
		return 0 //handled
	} //if WM_PAINT

	res8 := procDefWindowProc.Call(hwnd, uintptr(msg), wParam, lParam) //DefWindowProcW returns LRESULT.
	// if res8.Failed() {//it's CheckNone and no real failure mode to detect!
	// 	logf("in overlayWndProc, DefWindowProc() failed, err: %v, continuing", res8.Err)
	// }
	return res8.R1 //LRESULT
}

func updateOverlay(x, y, w, h, startW, startH int32) {
	if overlayHwnd == 0 {
		return
	}

	diffW := w - startW
	diffH := h - startH
	overlayText = fmt.Sprintf("Size: %dx%d (delta: %d, %d)", w, h, diffW, diffH)

	// Center the overlay over the window being resized
	ox := x + (w / 2) - 150
	oy := y + (h / 2) - 25

	res1 := procSetWindowPos.Call( //TODO: handle errors/returns here
		uintptr(overlayHwnd),
		HWND_TOPMOST,
		// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
		uintptr(ox),
		// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
		uintptr(oy),
		300, 50,
		SWP_NOACTIVATE|0x0040, // SWP_SHOWWINDOW
	)
	if res1.Failed() {
		logf("in updateOverlay, failed to SetWindowPos of overlayHwnd:0x%X, err:%v, callStatus:%v", overlayHwnd, res1.Err, res1.CallStatus)
	}

	// Force redraw, well the redraw is queued, whenever Windows gets around to it.
	res2 := procInvalidateRect.Call(uintptr(overlayHwnd), 0, 1)
	if res2.Failed() {
		logf("in updateOverlay, failed to InvalidateRect of overlayHwnd:0x%X (meant to eventually cause a repaint), err:%v, callStatus:%v", overlayHwnd, res2.Err, res2.CallStatus)
	}
	/*
		(press alt+z to temporarily toggle wordwrap to read this)

		if I drag-resize tcmd window at a certain rate, the paint for both tcmd window and for the overlay are stopped/frozen to their last painted, but the size of the tcmd window does keep responding in real-time, unclear why/how this happens!?
		Gemini 3.1 Pro says:
			You are experiencing Message Queue Starvation, specifically regarding the WM_PAINT message, combined with how the modern Windows Desktop Window Manager (DWM) handles window frames versus window contents.

		Here is exactly why the sizing works in real-time, but the painting freezes.
		1. The DWM vs. Client Area Disconnect

		When you call SetWindowPos, the OS does two things:

		    The Frame: The Desktop Window Manager (DWM) immediately updates the window's physical boundaries on the GPU. This is handled by Windows itself, which is why the window size keeps changing smoothly in real-time.

		    The Contents: The OS sends WM_WINDOWPOSCHANGED and WM_SIZE messages to the target application (Total Commander) so it can recalculate its internal layout. Once the app finishes that, it is supposed to redraw its contents (the "client area").

		2. WM_PAINT is the Lowest Priority Message in Windows

		Windows is fundamentally message-driven. By design, WM_PAINT is always pushed to the back of the line. Windows will only synthesize and dispatch a WM_PAINT message when the application's message queue is completely empty of higher-priority messages (like mouse movements, keyboard input, or sizing commands).

		Because your WH_MOUSE_LL hook is capturing mouse movements and bombarding the message queue with SetWindowPos calls every 10 milliseconds (100 times a second!), Total Commander's UI thread is choking. It is constantly processing your resize commands and never gets an "idle" moment to actually process the WM_PAINT message to redraw its files and panels.
		3. Why Your Overlay Also Freezes

		Your overlay suffers from the exact same starvation, and the smoking gun is in your updateOverlay function:
		Go

		// Force redraw
		res2 := procInvalidateRect.Call(uintptr(overlayHwnd), 0, 1)

		InvalidateRect does not draw anything. It simply tells Windows: "Hey, mark this area as dirty. Next time you have absolutely nothing else to do and the queue is empty, please send me a WM_PAINT." Because your main thread is busy aggressively draining the moveDataChan and calling SetWindowPos for the target window, the queue is never empty. Your overlay's WM_PAINT is starved until you stop dragging the mouse.
	*/
	//doneTODO: do I want this to happen unconditionally? or should it be in a systray bool like others?!
	if immediateOverlayRepaint.Load() {
		res3 := procUpdateWindow.Call(uintptr(overlayHwnd)) // <--- Forces immediate synchronous repaint
		if res3.Failed() {
			logf("in updateOverlay, failed to UpdateWindow aka repaint of overlayHwnd:0x%X, err:%v, callStatus:%v", overlayHwnd, res3.Err, res3.CallStatus)
		}
	}
}

var procUpdateWindow = wincoe.NewBoundProc(user32, "UpdateWindow", wincoe.CheckBool)

const SW_HIDE = 0

func hideOverlay() {
	if overlayHwnd != 0 {
		_ = procShowWindow.Call(uintptr(overlayHwnd), SW_HIDE)
	}
}

// shouldBypassGestureNow returns true when gesture processing for hwnd (the
// window the gesture would actually target) should be skipped because it's
// fullscreen (exclusive or borderless) on its monitor and the bypass feature
// is enabled. The check is done live via isWindowFullscreenOnMonitor against
// that specific hwnd, rather than against a foreground-change WinEvent
// cache, since such a cache only refreshes on foreground transitions and can
// lag behind whichever window a gesture is actually about to act on.
//
// Callers that can distinguish "bypassed" from "failed for another reason"
// (tryBeginMoveGestureAt, tryBeginResizeGestureAt, tryPerformMMBGestureAt)
// must propagate that distinction back up to mouseProc's switch, since only
// a bypass should let the originating mouse event pass through unswallowed —
// any other failure still swallows it, matching each gesture's prior
// behavior before this bypass feature existed.
func shouldBypassGestureNow(hwnd windows.Handle) bool {
	if !bypassGesturesWhenFullscreen.Load() {
		return false
	}
	should := isWindowFullscreenOnMonitor(hwnd)
	if should {
		//logf("Target window is fullscreen, refusing to trigger gesture. (toggle this behaviour from systray)")
		now := time.Now().UnixNano()
		last := lastFullscreenLogTime.Load()
		const everyXSeconds = 1
		// Only log if 1 second (1,000,000,000 nanoseconds) has passed
		if now-last > int64(everyXSeconds*time.Second) {
			lastFullscreenLogTime.Store(now)
			logf("Target window is fullscreen, refusing to trigger gesture. (toggle this behaviour from systray) (this logline is rate-limited to 1 per %d second(s))", everyXSeconds)
		}
	}
	return should
}

var lastFullscreenLogTime atomic.Int64 // Add this with your other globals

// alignRestoredWindowToCursor repositions the restored-window rect so the
// cursor sits at the same proportional position it held within the maximized
// window. normRect supplies the post-restore dimensions; only Left/Top (and
// therefore Right/Bottom) are adjusted — the size is preserved unchanged.
func alignRestoredWindowToCursor(cursorPt POINT, maxRect, normRect RECT) RECT {
	maxW := maxRect.Right - maxRect.Left
	maxH := maxRect.Bottom - maxRect.Top
	normW := normRect.Right - normRect.Left
	normH := normRect.Bottom - normRect.Top

	// Degenerate rects — return the restored rect as-is.
	if maxW <= 0 || maxH <= 0 || normW <= 0 || normH <= 0 {
		return normRect
	}

	// Cursor's fractional position within the maximized window (0..1).
	relX := float64(cursorPt.X-maxRect.Left) / float64(maxW)
	relY := float64(cursorPt.Y-maxRect.Top) / float64(maxH)

	// Clamp to [0,1] so a cursor outside the window edge doesn't flip the rect.
	if relX < 0 {
		relX = 0
	} else if relX > 1 {
		relX = 1
	}
	if relY < 0 {
		relY = 0
	} else if relY > 1 {
		relY = 1
	}

	newLeft := cursorPt.X - int32(float64(normW)*relX)
	newTop := cursorPt.Y - int32(float64(normH)*relY)
	return RECT{
		Left:   newLeft,
		Top:    newTop,
		Right:  newLeft + normW,
		Bottom: newTop + normH,
	}
}

// isWindowFullscreenOnMonitor returns true if hwnd's bounding rect covers the
// entire area of the monitor it occupies (catches both exclusive fullscreen and
// borderless-fullscreen). Returns false on any API failure.
func isWindowFullscreenOnMonitor(hwnd windows.Handle) bool {
	if hwnd == 0 {
		return false
	}

	// Exclude regular maximized windows (like Notepad).
	// They retain their title bar style even though their window rect bleeds
	// past the monitor edges. True fullscreen or borderless windows drop WS_CAPTION.

	// 1. Get the Window dimensions first
	var r RECT
	if res := procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r))); res.Failed() {
		logf("isWindowFullscreenOnMonitor:GetWindowRect failed, err:%v", res.Err)
		return false
	}

	// 2. Get the Monitor information
	res := procMonitorFromWindow.Call(uintptr(hwnd), MONITOR_DEFAULTTONEAREST)
	hMon := res.R1
	if hMon == 0 {
		logf("isWindowFullscreenOnMonitor:MonitorFromWindow says no monitor!")
		return false
	}
	var mi MONITORINFO
	mi.CbSize = uint32(unsafe.Sizeof(mi))
	if res2 := procGetMonitorInfo.Call(hMon, uintptr(unsafe.Pointer(&mi))); res2.Failed() {
		logf("isWindowFullscreenOnMonitor:GetMonitorInfo failed, err:%v", res2.Err)
		return false
	}

	// 3. GEOMETRY FIRST: Does it engulf the entire monitor?
	isSpanningMonitor := r.Left <= mi.RcMonitor.Left &&
		r.Top <= mi.RcMonitor.Top &&
		r.Right >= mi.RcMonitor.Right &&
		r.Bottom >= mi.RcMonitor.Bottom

	// If it doesn't even fill the screen (like your mini borderless window),
	// it is definitively NOT fullscreen. Return false immediately.
	if !isSpanningMonitor {
		return false
	}

	// 4. STYLE TIE-BREAKER: It fills the screen, but is it just a maximized window?
	if style, err := getWindowLongPtr(hwnd, GWL_STYLE); err != nil {
		logf("isWindowFullscreenOnMonitor:GetWindowLongPtr GWL_STYLE failed: %v", err)
		// Fallback: if style check fails but it fills the screen, err on the side of caution
		return true
	} else {
		// If it fills the screen AND has a caption, it's just a normal maximized window
		// (likely bleeding over the edges due to an auto-hidden taskbar).
		if (style & WS_CAPTION) == WS_CAPTION {
			return false
		}
	}

	// It fills the screen and does NOT have a caption (Chrome, Firefox F11, Games, etc.)
	return true
}

func isWindowForeground(hwnd windows.Handle) bool {
	if hwnd == 0 {
		logf("!! attempted to check the focus of a windows with handle 0")
		return false
	}
	//To answer your performance and safety concerns: GetForegroundWindow and GetCursorPos are both "safe" to call within your mouseProc because they are simple getters
	// that query the system's internal state without sending messages to other windows.
	fg := getForegroundWindow()
	if fg == 0 {
		logf("in isWindowForeground, failed to GetForegroundWindow, it returned hwnd 0x0")
		return false
	}

	return fg == hwnd
}

func getForegroundWindow() windows.Handle {
	res1 := procGetForegroundWindow.Call()
	if res1.R1 == 0 || res1.Failed() { //it's CheckNone so it never fails!
		logf("Failed to GetForegroundWindow, err: %v callStatus: %v", res1.Err, res1.CallStatus)
		return windows.Handle(0)
	}
	return windows.Handle(res1.R1)
}

// aka in window in my own process?
func isOwnWindow(hwnd windows.Handle) bool {
	if hwnd == 0 {
		return false
	}

	var pid uint32
	res1 := procGetWindowThreadProcessID.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(&pid)),
	)
	//if r1 == 0 {
	if res1.Failed() {
		return false
	}

	return pid == selfPID //windows.GetCurrentProcessId()
}

// FIXME: make these two funcs be one and return two bools: (samePID, sameTID) and sameTID would be false if samePID is false!

// is window in the same thread ID as the caller thread ID (could still be two diff. processes tho!)
func isInSameThreadID(hwnd windows.Handle) bool {
	var pid uint32
	res1 := procGetWindowThreadProcessID.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(&pid)),
	)
	// if tid == 0 {
	if res1.Failed() {
		return false
	}
	// #nosec G115 -- safe: Win32 Thread IDs are 32-bit DWORDs
	return uint32(res1.R1 /*aka tid aka thread id*/) == windows.GetCurrentThreadId()
}

// focusThisHwnd requires: procAttachThreadInput to have been done first, to work. XXX: apparently, 17 July 2026, it doesn't require this anymore!!?! maybe I changed something via w11privacy ?! as it used to require it or it would focus-steal prevent it from getting focused! It's for sure the vkE8 tap that happens before this! aka injectShiftTap()
/*
Why your app (winbollocks) doesn't need AttachThreadInput

Windows explicitly outlines the rules for when a process is allowed to call SetForegroundWindow successfully. An app is granted focus privileges if:

    It is already the foreground process. (nope it's not!)

    It received the last input event. (maybe? but more likely because I inject RShift tap before gesture start? it's vkE8 now btw, not Shift, function still named the same tho)

    It is handling a window hook. (true)

Because your app operates via low-level global hooks (WH_MOUSE_LL and WH_KEYBOARD_LL), you are intercepting physical hardware events (Winkey + LMB) in real-time. The OS recognizes that your application is directly tied to the user's active, physical input. Therefore, Windows automatically grants your process the privilege to change the foreground window.

    When it works: SetForegroundWindow succeeds instantly without any thread attaching.

    When it fails (The Start Menu case): When the modern Windows Start Menu or Shell is open, the OS enforces an absolute lock. In this edge case, SetForegroundWindow fails. But guess what? AttachThreadInput doesn't bypass this lock either!

So, in the scenarios where SetForegroundWindow works, it works entirely on its own. In the scenarios where it gets blocked by the Shell, AttachThreadInput was failing or doing nothing anyway.
*/
func focusThisHwnd(target windows.Handle) (gotFocused bool) {
	return setForegroundWindow(target, "failed SetForegroundWindow")
}

const (
	WS_CHILD         = 0x40000000
	WS_POPUP         = 0x80000000
	WS_CAPTION       = 0x00C00000
	WS_EX_NOACTIVATE = 0x08000000
	WS_EX_TOOLWINDOW = 0x00000080
)
const WS_EX_TOPMOST = 0x00000008
const (
	GWL_STYLE   = -16 // We could use ^uintptr(15) to represent -16 (GWL_STYLE) to prevent Go constant overflow errors.
	GWL_EXSTYLE = -20
)

func getWindowLongPtr(hwnd windows.Handle, index int32) (uintptr, error) {
	if hwnd == 0 {
		return 0, fmt.Errorf("getWindowLongPtr: hwnd is 0")
	}

	/*
			The documented pattern is:

		Clear last error.
		Call GetWindowLongPtrW.
		If return value is 0, call GetLastError.
		If error is non-zero → failure.
		If error is zero → success, because the actual value was legitimately zero.
	*/

	// Clear last error so we can detect real failure
	//windows.SetLastError(0)
	// Clear last error so we can detect real failure
	_ = procSetLastError.Call(0)
	//windows.SetLastError(0)

	res1 := procGetWindowLongPtrW.Call( //it's a CheckNone so res1.Err is nil
		uintptr(hwnd),
		// #nosec G115 -- safe: Win32 ABI expects negative offsets to be cast to uintptr
		uintptr(index),
	)
	ret := res1.R1
	//Do NOT trust the third return from .Call
	//You did the right thing ignoring it. For many Win32 APIs it is unreliable.

	// Important edge case:
	// GetWindowLongPtr can legally return 0 even on success.
	// The only reliable failure signal is GetLastError.
	if ret == 0 {
		lastErr := windows.GetLastError() //XXX: so, needed! probably the only case so far!
		/*
				Why windows.GetLastError() is Tricky
			In Go's golang.org/x/sys/windows package, windows.GetLastError() returns an error interface type (under the hood, it’s a windows.Errno).
			If the underlying Windows API reports 0 (which matches ERROR_SUCCESS), Go's windows package translates this to a literal nil error interface, not an error object containing ERROR_SUCCESS.
			Therefore, you will never get an error object where errors.Is(err, windows.ERROR_SUCCESS) evaluates to true, because by the time it reaches your code, a success is just a plain old nil.
		*/
		// GetLastError returns nil if the last error code is 0 (ERROR_SUCCESS)
		if lastErr != nil { //&& !errors.Is(lastErr, windows.ERROR_SUCCESS) {//
			return 0, fmt.Errorf("GetWindowLongPtrW failed: %w", lastErr)
			// //nolint:wrapcheck
			// return 0, lastErr
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
		logf("GetWindowLongPtr GWL_STYLE failed: %v", err)
		reason = "GetWindowLongPtr GWL_STYLE failed"
		return
	}

	exStyle, err := getWindowLongPtr(hwnd, GWL_EXSTYLE)
	if err != nil {
		logf("GetWindowLongPtr GWL_EXSTYLE failed: %v", err)
		reason = "GetWindowLongPtr GWL_EXSTYLE failed"
		return
	}

	// #nosec G115 -- safe: Win32 window styles are 32-bit bitmasks
	s := uint32(style)
	// #nosec G115 -- safe: Win32 extended window styles are 32-bit bitmasks
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
		reason = "has WS_EX_NOACTIVATE (explicit no-activate)"
		return
	}

	ret = false
	reason = "shouldn't skip"
	return
}

// aka focus(activate) the window, works by attaching to target window's thread, so Windows won't do its focus stealing prevention thing!
// also, this way I don't have to inject LMB down then LMB up aka a LMB click event to focus it, risking pressing Exit button on total commander for example.
// however, doneTODO: now i do have to make sure hooks are running on a separate thread (than main msg. loop) because this is potentially blocking and can deadlock, depending on target app.
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
				return setForegroundWindow(target, "failed to SetForegroundWindow for own window in same thread(w/o thread attach) (this usually happens because Start menu was open, as: ret==0 and callErr is success)")
				//XXX: you get ret=0 with "err=The operation completed successfully." when Start menu was already open
				/*
					The SetForegroundWindow Silent Failure
					The Culprit: The Windows 10/11 Start Menu (Focus Stealing Prevention).
					When the Start Menu is open, the Windows Shell (StartMenuExperienceHost.exe) aggressively locks the foreground. Windows actively blocks background applications from stealing focus to prevent malicious pop-ups from hijacking your keystrokes while you're trying to search or launch an app.
					When your code calls SetForegroundWindow while Start is open:
					    It returns 0 (Failure).
					    GetLastError() returns 0 ("The operation completed successfully").
					This isn't a bug in Go or your code; this is Windows politely saying, "I heard your request perfectly, and the answer is absolutely not."
						- Gemini 3.1 Pro
					so since we swallow the LMB click when gesture triggers for ModeMove, and we instead try to basically steal the focus from Start Menu, win11 disallows this. If LMB were allowed then it woulda worked, which is why the fallback synthetic/injected LMB click works and will focus it.

					So the error below is:
					"failed to SetForegroundWindow for own window in same thread(w/o thread attach) ret=0 err='"SetForegroundWindow" windows call reported failure (ret=0) but no usable error was provided' callErr:'The operation completed successfully.'"
				*/
			} else {
				//reason = "is own window on diff. thread which might have own msg. loop"
				logf("attempting to focus own window, but it's on a diff. thread in own process, will pretend it's focused(to avoid the LMB-click-to-focus-it workaround next) without actually focusing it tho.")
				return true //FIXME: we pretend it's focused, but it may be more correct to do this outside of this function? however this case would need to be signalled/returned to know outside what to do, meh!
			}
			//unreachable()
		}
	} // a block to not leak defined vars

	if useThreadAttachInputForFocus.Load() {
		class := getClassName(target)
		isConsole := class == "ConsoleWindowClass" || class == "PseudoConsoleWindow"
		//logf("isConsole:%v class:%v", isConsole, class) //XXX:ok, admin console(or non-admin but set to conhost aka Console Host Terminal in Settings->Default Terminal Application) is console, the normal non-admin one (with "Let Windows decide" or "Windows Terminal" in same Settings) is not console.

		if !isConsole {
			// Only attempt AttachThreadInput for normal GUI windows, else it will fail anyway.

			/*
				When you call AttachThreadInput, you aren't just giving yourself permission to move a window; you are literally merging the input message queues of the two threads.

				As shown in the logic of Windows message queues, each thread usually has its own "mailbox." AttachThreadInput solders those two mailboxes together.
				 If the target thread stops checking its mail, your thread's mail also piles up. By using the SendMessageTimeout "ping" first, you ensure that
				 the other thread is currently checking its mailbox before you solder yours to it.
			*/

			var targetProcessID uint32
			res2 := procGetWindowThreadProcessID.Call(uintptr(target), uintptr(unsafe.Pointer(&targetProcessID)))
			//if r1 == 0 {
			if res2.Failed() {
				logf("GetWindowThreadProcessId failed: %v", res2.Err)
				return false
			}
			var targetThreadID uint32 = uint32(res2.R1)

			// XXX: assuming we're used on mainThreadID only! we should remove these checks and just use mainThreadID
			curTid := windows.GetCurrentThreadId()
			if curTid != mainThreadID {
				logf("dev coding error: forceForeground is being called(next) from a threadID(%d) that wasn't mainThreadID(%d)", curTid, mainThreadID)
			}

			// Use SendMessageTimeout to see if the window is alive
			var result uintptr
			res3 := procSendMessageTimeout.Call(
				uintptr(target),
				WM_NULL, // WM_NULL (harmless ping)
				0,
				0,
				SMTO_ABORTIFHUNG,  //0x0002, // SMTO_ABORTIFHUNG
				HungWindowTimeout, // 150ms timeout
				uintptr(unsafe.Pointer(&result)),
			)

			//if err2 != nil || ret == 0 {
			if res3.Failed() {
				logf("Target window HWND 0x%X is HUNG err='%v'. Aborting AttachThreadInput to prevent deadlock.", target, res3.Err)
				return false
			}

			// Only if the window responds do we proceed with the attachment
			res4 := procAttachThreadInput.Call(uintptr(curTid), uintptr(targetThreadID), uintptr(1))
			// if attachRet == 0 {
			if res4.Failed() {
				/*
					The reality: Microsoft explicitly hardcodes AttachThreadInput to fail if the target thread belongs to a classic console window (conhost.exe or cmd.exe). Console windows do not have a standard USER32 message queue in the way GUI apps do; their input is managed by the Client/Server Runtime Subsystem (CSRSS) or the Conhost subsystem.
					When you ask Windows to attach to a console thread, the OS rejects it and returns ERROR_INVALID_PARAMETER (87) — aka "The parameter is incorrect."
						- Gemini 3.1 Pro
				*/
				logf("AttachThreadInput failed: %v", res4.Err)
				return false
			}

			defer func() {
				if res := procAttachThreadInput.Call(uintptr(curTid), uintptr(targetThreadID), uintptr(0)); res.Failed() {
					logf("forceForeground: AttachThreadInput detach failed for threadIDs %d/%d: %v", curTid, targetThreadID, res.Err)
				}
			}() // Detach always
		} //was not console
	} // was useThreadAttachInputForFocus

	// //FIXME: we should only do this if Start menu is actually open/focused, no?! actually doesn't work at all, bad Gemini suggestion!
	// // Tap ALT to bypass Start Menu lock
	// injectKeyTap(VK_MENU)

	succeeded := focusThisHwnd(target) // still attached here.

	return succeeded //fgRet != 0
}

func logLMBState(prefix string) {
	res1 := procGetAsyncKeyState.Call(VK_LBUTTON)
	state := res1.R1
	if state&0x8000 != 0 {
		logf("%s: LMB is DOWN (0x%04X)", prefix, state)
	} else {
		logf("%s: LMB is UP   (0x%04X)", prefix, state)
	}
}

/* ---------------- Mouse Hook ---------------- */

const Duration5ms time.Duration = 5 * time.Millisecond // aka 5 million ns aka nanosec

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
		res1 := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		if nowDiff := time.Since(start); nowDiff > Duration5ms {
			logf("stutter1 %d ns", nowDiff.Nanoseconds())
		}
		return res1.R1
	}

	// nolint:govet //for unsafeptr, has no effect actually, still warns even with settings.json only this works(outside of vscode): go vet -unsafeptr=false
	info := (*MSLLHOOKSTRUCT)(unsafe.Pointer(lParam)) // XXX: warns without the .\.vscode\settings.json the unsafeptr false part.
	// // Trick the linter: convert to pointer via an interface or a helper
	// // that doesn't trigger the "unsafeptr" heuristic.
	// var p interface{} = lParam
	// //nolint:govet,unsafeptr // because
	// info := (*MSLLHOOKSTRUCT)(unsafe.Pointer(p.(uintptr)))

	if info.Flags&LLMHF_INJECTED != 0 {
		// This mouse event was generated by SendInput
		// Do NOT treat it as user input
		res2 := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		if nowDiff := time.Since(start); nowDiff > Duration5ms {
			logf("stutter2 %d ns", nowDiff.Nanoseconds())
		}
		return res2.R1
	}

	switch wParam {
	case WM_LBUTTONDOWN: //LMB pressed aka LMBDown or LMB DOWN
		// we don't want to trigger our drag gesture if shift/alt/ctrl was held before winkey, because it might have different meaning to other apps.
		winDown, shiftDown, ctrlDown, altDown := modifierKeyState()
		// var winDown bool = keyDown(VK_LWIN) || keyDown(VK_RWIN)
		// var shiftDown bool = keyDown(VK_SHIFT)
		// var ctrlDown bool = keyDown(VK_CONTROL)
		// var altDown bool = keyDown(VK_MENU)
		if winDown && !shiftDown && !altDown && !ctrlDown { // only if winkey without any modifiers
			started, bypassed := tryBeginMoveGestureAt(info.Pt, false)
			if bypassed {
				break // target is fullscreen; let event through
			}
			markGestureUsedOnce()

			if !started {
				logf("failed to begin Move gesture(the why should be above ^) on winkey+LMB pressed")
			}

			if nowDiff := time.Since(start); nowDiff > Duration5ms {
				logf("stutter8 %d ns", nowDiff.Nanoseconds())
			}

			return 1 // swallow LMB
		}

	case WM_MOUSEMOVE:
		session := activeSession.Load()
		if session == nil {
			// See if we might have missed the LMB/RMB-down that would normally have
			// started a gesture, because our low-level hooks were blind while a
			// higher-integrity window (e.g. Task Manager, while we're not elevated)
			// still had the foreground at the moment of the click
			//
			// checkForMissedGestureOnNextMove is armed exactly once by winEventProc,
			// right when the foreground regains a non-blocking integrity level, so
			// the extra GetAsyncKeyState calls below only ever run during that one
			// recovery attempt - never on ordinary moves - keeping this cheap.
			if checkForMissedGestureOnNextMove.CompareAndSwap(true, false) {
				if missedGestureRecoveryEnabled.Load() {
					winDown, shiftDown, ctrlDown, altDown := modifierKeyState()
					if winDown && !ctrlDown && !altDown {
						switch {
						case !shiftDown && keyDown(VK_LBUTTON):
							started, bypassed := tryBeginMoveGestureAt(info.Pt, true)
							if bypassed {
								break // target is fullscreen; nothing to recover this time
							}
							markGestureUsedOnce()
							logf("Recovering a missed winkey+LMB drag-move gesture that started while our hooks were blind due to a higher-integrity foreground window. Run as Administrator to avoid the need to do this for normal windows.")
							if started {
								// The real LMB-down already reached the target window normally
								// (our hook was blind to it), so if it's something like a
								// console, it's genuinely mid its own click-drag (e.g. extending
								// a text selection) and still believes LMB is held. Telling it
								// LMB is up now stops that from fighting our window move on
								// every subsequent mouse-move we let through — our own move
								// logic doesn't need LMB to read as "down", it drives entirely
								// off activeSession + MSLLHOOKSTRUCT. The real LMB-up still
								// reaches the target later too (see WM_LBUTTONUP's
								// viaMissedGestureRecovery handling) — a second "up" while
								// already up is a harmless no-op for most windows.
								// Caveat: if the initiating click actually landed on something
								// like a push-button rather than a text/console area, this
								// synthetic up could fire that control's click action a little
								// early. Not observed in practice (this path only triggers when
								// switching focus away from a higher-integrity window), but
								// worth knowing.
								if injectButtonUpOnMissedGestureRecovery.Load() {
									session2 := activeSession.Load() // it's updated in the above try
									var hwnd windows.Handle
									if session2 != nil {
										hwnd = session2.targetWnd
									} else {
										hwnd = windows.Handle(0)
									}
									logf("Injecting synthetic LMB-up for missed-gesture recovery drag (HWND=0x%X); note this will trigger an unintended click, especially if the initiating click landed on a button, or unwanted paste behavior in some console windows if RMB is used instead.", hwnd)
									injectLMBUp()
								}
							} else {
								logf("failed to begin Move gesture(the why should be above ^) while trying to start it as recovery")
							}
						case !shiftDown && keyDown(VK_RBUTTON):
							started, bypassed := tryBeginResizeGestureAt(info.Pt, true)
							if bypassed {
								break // target is fullscreen; nothing to recover this time
							}
							markGestureUsedOnce()
							logf("Recovering a missed winkey+RMB resize gesture that started while our hooks were blind due to a higher-integrity foreground window. Run as Administrator to avoid the need to do this for normal windows.")
							if started {
								// See the identical comment in the LMB/ModeMove case above.
								if injectButtonUpOnMissedGestureRecovery.Load() {
									session2 := activeSession.Load() // it's updated in the above try
									var hwnd windows.Handle
									if session2 != nil {
										hwnd = session2.targetWnd
									} else {
										hwnd = windows.Handle(0)
									}
									logf("Injecting synthetic RMB-up for missed-gesture recovery resize (HWND=0x%X); note in classic console windows (conhost) a bare RMB-up outside of an active selection triggers Paste, or pop the RMB menu in notepad.", hwnd)
									injectRMBUp()
								}
							} else {
								logf("Failed to begin Resize gesture (reason why should be above ^) while trying to start it as recovery.")
							}
						case keyDown(VK_MBUTTON): //this doesn't get hit, doh! unless you hold it during mouse move, which is unlikely for you to do!
							started, bypassed := tryPerformMMBGestureAt(info.Pt, shiftDown)
							if bypassed {
								break // target is fullscreen; nothing to recover this time
							}
							markGestureUsedOnce()
							if shiftDown {
								logf("Recovering a missed winkey+shift+MMB (bring-to-front) gesture that started while our hooks were blind due to a higher-integrity foreground window. Run as Administrator to avoid the need to do this for normal windows.")
							} else {
								logf("Recovering a missed winkey+MMB (send-to-back) gesture that started while our hooks were blind due to a higher-integrity foreground window. Run as Administrator to avoid the need to do this for normal windows.")
							}
							if !started {
								logf("Failed to recover winkey+MMB gesture (reason why should be above ^)")
							}
						}
						session = activeSession.Load() // may now be non-nil (LMB/RMB recovery only; MMB never touches activeSession)
					}
				}
			}
			if session == nil {
				break // No drag or resize is active (and nothing to recover), do nothing!
			}
		}
		switch session.mode {
		case ModeMove:
			if requireWinDownHeldDuringGesture.Load() {
				var winDown bool = keyDown(VK_LWIN) || keyDown(VK_RWIN)
				if !winDown {
					//cantFIXME: shouldn't I also stop drag if LMB (for ModeMove) or RMB(for ModeResize) aren't also down?! especially when unlocking a Winkey+L locked Desktop which was locked while doing any of the two gestures(ie. winkey+LMB drag to move, then pressed L without first releasing any of winkey or LMB, but then unlocked with both being released which means we didn't sense them being released). It doesn't work, checking async state reports it's actually UP not down because we swallowed it!
					logf("winkey is no longer down, stopping drag")
					//nevermindTODO: make systray option to keep dragging even if winkey's no longer down(bad idea for winkey+L case, see todo.txt about it), once initiated. But this means the edge case with Winkey+L (search for it above) can happen! unless i check if LMB is still down in async state here hmmm... won't work because we ate LMB down and async state depends on us not eating it.
					hardReset(true) //XXX: resets gesture used which means doesn't prevent a winUP from popping start menu, this is correct because we detected winkey as being UP here!

					break //exit case/switch!
				}
			}

			//XXX: doesn't work, because we eat LMB via 'return 1' the async state reports it as UP not down!
			// // doneFIXME: also stop dragging if LMB itself is no longer physically
			// // held, independently of requireWinDownHeldDuringGesture above. This
			// // catches the Winkey+L case: lock mid-drag (winkey+LMB still held), type
			// // the password on the secure desktop (which necessarily releases both,
			// // but our hook never sees either up-event since it isn't invoked on the
			// // secure desktop), then unlock. Without this, a stale ModeMove session
			// // would survive the lock/unlock cycle and keep "dragging" the window on
			// // every subsequent mouse move, with nothing actually held down.
			// //
			// // This relies on GetAsyncKeyState(VK_LBUTTON) reflecting the real
			// // hardware state even though our hook swallowed the original
			// // WM_LBUTTONDOWN — the same assumption the winDown check above already
			// // makes for VK_LWIN, and borne out by the missed-gesture recovery logic
			// // below, which reads keyDown(VK_LBUTTON) for a down-event our hook never
			// // even saw. We skip this for a viaMissedGestureRecovery session: starting
			// // one deliberately injects a synthetic LMB-up (see the recovery branch in
			// // WM_MOUSEMOVE below) so the target's own click-drag state doesn't fight
			// // our window move — which would make this check fire immediately on an
			// // entirely ordinary recovery-drag. Those sessions rely on the real
			// // WM_LBUTTONUP reaching us instead (see its handling below).
			// if !session.viaMissedGestureRecovery && !keyDown(VK_LBUTTON) {
			// 	logf("LMB is no longer down (its up-event was likely missed, e.g. due to a Winkey+L lock/unlock during the drag), stopping drag-move for HWND=0x%X", session.targetWnd)
			// 	hardReset(true)
			// 	break
			// }

			if !ShouldThrottle() {
				// At the very beginning of the drag/move logic (e.g., right after checking if dragging is active)
				var now time.Time
				var nowOffset time.Duration
				if ratelimitOnMove.Load() {
					now = time.Now()
					nowOffset = now.Sub(appStartTime)
					// Count every potential move (even if we skip due to debounce)
					//moveCounter++
					moveCounter.Add(1)
					//FIXME: should allow logging even if rate limiting isn't enabled.
					//logf("%d", moveCounter) //FIXME: temp, remove
				}

				dx := info.Pt.X - session.state.startPt.X
				dy := info.Pt.Y - session.state.startPt.Y
				r := session.state.startRect
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
				var willPostMessage bool = !ratelimitOnMove.Load() || (newX != lastPostedX.Load() || newY != lastPostedY.Load()) && (nowOffset-time.Duration(lastMovePostedTime.Load())) >= MIN_MOVE_INTERVAL
				// Optional: Also count only the ones that would have posted (uncomment if you want both stats)
				if ratelimitOnMove.Load() && shouldLogDragRate.Load() && willPostMessage {
					//actualPostCounter++
					actualPostCounter.Add(1)
				}

				// Periodic logging every ~1 second
				if ratelimitOnMove.Load() && shouldLogDragRate.Load() {
					foo := (nowOffset - time.Duration(lastRateLogTime.Load()))
					if foo >= rateLogInterval {
						var secondsElapsed float64 = foo.Seconds()
						if secondsElapsed > 0 {
							rate := float64(moveCounter.Load()) / secondsElapsed
							// logf("Drag move rate: %d events in %.2fs → %.1f moves/sec",
							// 	moveCounter, secondsElapsed, rate)
							// In the periodic log block:
							logf("Drag move rate: %s potential / %s actual moves in %.2fs \xbb %.1f / %.1f per sec", // \xbb is »
								withCommas(moveCounter.Load()), withCommas(actualPostCounter.Load()), secondsElapsed,
								rate, //float64(moveCounter)/secondsElapsed,
								float64(actualPostCounter.Load())/secondsElapsed)
						}

						// Reset counters
						moveCounter.Store(0)
						actualPostCounter.Store(0)
						lastRateLogTime.Store(int64(nowOffset))
					}
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
						Hwnd:        session.targetWnd,
						X:           newX,
						Y:           newY,
						InsertAfter: 0, // this is the value for HWND_TOP but SWP_NOZORDER below makes it unused, supposedly!

						Flags: SWP_NOSIZE | SWP_NOACTIVATE | SWP_NOZORDER | SWP_ASYNCWINDOWPOS, // for ModeMove
					}
					//data.Hwnd = targetWnd
					//data.X = newX // int32, full range
					//data.Y = newY
					//data.InsertAfter = 0 // this is the value for HWND_TOP but SWP_NOZORDER below makes it unused, supposedly!

					//data.Flags = SWP_NOSIZE | SWP_NOACTIVATE | SWP_NOZORDER // Or dynamic

					//// Post the move request instead of doing the windows move/drag motion here
					// procPostMessage.Call(
					// 	uintptr(mainMsgHwnd),
					// 	WM_DO_SETWINDOWPOS,
					// 	0,                             // unused, target is in the struct!
					// 	uintptr(unsafe.Pointer(data)), // lParam = pointer to struct
					// )

					/* THE SELECT BLOCK:
					   This is Go's magic for non-blocking communication.
					*/
					enqueueMoveOrResize(data, "WM_MOUSEMOVE/ModeMove")
					// select {
					// case moveDataChan <- data:
					// 	// SUCCESS: The data was copied into the buffered channel.
					// 	// Now we ring the "Doorbell" to wake up the Main Thread.
					// 	// PostThreadMessage is an asynchronous "fire and forget" call.
					// 	//procPostThreadMessage.Call(uintptr(mainThreadId), WM_DO_SETWINDOWPOS, 0, 0)
					// 	//the reason we use PostMessage and not PostThreadMessage here is because while systray menu popup is open it runs its own msg loop and calls my wndProc so it will ignore all of these doorbells until popup is closed if i use postThreadMessage!
					// 	res1 := procPostMessage.Call(uintptr(mainMsgHwnd), WM_DO_SETWINDOWPOS, 0, 0)
					// 	// if r == 0 {
					// 	if res1.Failed() {
					// 		logf("PostMessage of WM_DO_SETWINDOWPOS for WM_MOUSEMOVE failed: %v", res1.Err)
					// 	}

					// default:
					// 	// FAIL: The channel (2048 slots) is completely full.
					// 	// This happens if the Main Thread is frozen (e.g., Admin console lag).
					// 	// We MUST NOT block here, or we will freeze the user's entire mouse cursor.
					// 	// We just increment our "shame counter" and move on.
					// 	droppedMoveOrResizeEvents.Add(1) //TODO: use diff. one to keep track of drops due to channel full
					// }

					if ratelimitOnMove.Load() {
						lastMovePostedTime.Store(int64(nowOffset))
						lastPostedX.Store(newX)
						lastPostedY.Store(newY)
					}
					//return 0 //0 = let it thru
					//XXX: let it fall thru so CallNextHookEx is also called!
				} // willPostMessage
			} else // endif >=10ms, else drop:
			{
				droppedMoveOrResizeEvents.Add(1) //TODO: use diff. one to keep track of drops due to too-fast thus not-queued
			}
			//} //main 'if', for capturing aka moving/dragging window
		case ModeResize:
			if requireWinDownHeldDuringGesture.Load() {
				var winDown bool = keyDown(VK_LWIN) || keyDown(VK_RWIN)
				if !winDown {
					logf("winkey is no longer down, stopping resize")
					//don't think of doing this if RMB is no longer down also, it won't work because we 'return 1' on RMB so async state will see it UP, logically.
					// See the identical comment(s) in the ModeMove case above
					hardReset(true) //XXX: resets gesture used which means doesn't prevent a winUP from popping start menu, this is correct because we detected winkey as being UP here!

					break //exit case/switch!
				}
			}

			//XXX: doesn't work, because we eat RMB via 'return 1' the async state reports it as UP not down!
			// // See the identical comment in the ModeMove case above; RMB is
			// // ModeResize's equivalent of ModeMove's LMB.
			// if !session.viaMissedGestureRecovery && !keyDown(VK_RBUTTON) {
			// 	logf("RMB is no longer down (its up-event was likely missed, e.g. due to a Winkey+L lock/unlock during the resize), stopping resize for HWND=0x%X", session.targetWnd)
			// 	hardReset(true)
			// 	break
			// }

			//if resizing.Load() && currentDrag != nil {
			//if time.Since(lastResize) >= forceMoveOrResizeActionsToBeThisManyMSApart*time.Millisecond {
			if !ShouldThrottle() {
				nx, ny, nw, nh := calculateResize(session, info.Pt) //TODO: move this into wndProc aka into handleActualMove() ?!
				flags := uint32(SWP_NOZORDER | SWP_NOACTIVATE)
				if asyncResize.Load() {
					flags |= SWP_ASYNCWINDOWPOS
				}
				data := WindowMoveData{
					Hwnd:       session.targetWnd,
					X:          nx,
					Y:          ny,
					W:          nw,
					H:          nh,
					Flags:      flags,
					ResizeZone: session.resizeZone,
				}

				// Send to your mover channel
				enqueueMoveOrResize(data, "WM_MOUSEMOVE/ModeResize")
				// select {
				// case moveDataChan <- data:
				// 	// Trigger the move window
				// 	res1 := procPostMessage.Call(uintptr(mainMsgHwnd), WM_DO_SETWINDOWPOS, 0, 0)
				// 	// if r == 0 {
				// 	if res1.Failed() {
				// 		logf("PostMessage of WM_DO_SETWINDOWPOS for WM_MOUSEMOVE failed: %v", res1.Err)
				// 	}
				// default:
				// 	// FAIL: The channel (2048 slots) is completely full.
				// 	// This happens if the Main Thread is frozen (e.g., Admin console lag).
				// 	// We MUST NOT block here, or we will freeze the user's entire mouse cursor.
				// 	// We just increment our "shame counter" and move on.
				// 	droppedMoveOrResizeEvents.Add(1) //TODO: use diff. one to keep track of drops due to channel full
				// }
			} else //endif >=10ms, else drop it:
			{
				droppedMoveOrResizeEvents.Add(1) //TODO: use diff. one to keep track of drops due to too-fast thus not-queued
			}
			//XXX: let it fall thru so the move isn't eaten.
			//} //second 'if', for resizing
		} //switch

		// SUPERSEDED: see injectLMBUp()/injectRMBUp(), injected once at
		// recovery-session start instead (in the WM_MOUSEMOVE recovery branch
		// above). A single synthetic up avoids whatever broke dragging visually
		// with the swallow-every-move approach below.
		// if session != nil && session.viaMissedGestureRecovery {//XXX: doesn't work since it eats moves globally, it won't move on drag, it snaps back to origin.
		// 	// This session's LMB/RMB-down was never seen/swallowed by us,
		// 	// so the target window's own button-driven state (e.g. a
		// 	// console's in-progress text selection) is genuinely still
		// 	// active and would keep extending on every move we let through,
		// 	// on top of us repositioning the window ourselves. Our own
		// 	// move/resize logic above already has everything it needs
		// 	// straight from MSLLHOOKSTRUCT, so nothing depends on the
		// 	// target actually receiving these — swallow them for the
		// 	// duration of a recovery-started drag.
		// 	return 1 //swallow
		// 	//Trade-off worth knowing: this swallows WM_MOUSEMOVE system-wide (low-level hooks see raw input, before any per-window routing),
		// 	// so other apps briefly stop getting hover/move updates while a recovery-drag is in progress.
		// 	// Given this path only triggers in the rare "switched away from an elevated window" case,
		// 	// that seems like a good trade for killing the selection-growth bug.
		// }

	case WM_LBUTTONUP: //LMB released aka LMBUP aka LMB UP
		session := activeSession.Load()
		if session == nil {
			break // No drag or resize is active, do nothing!
		}
		if session.mode == ModeMove {
			viaRecovery := session.viaMissedGestureRecovery
			softReset(true) // this means that when winkey goes UP it will make sure from keyboardProc that start menu doesn't pop up!
			if viaRecovery {
				// This session's LMB-down was never seen/swallowed by us, so the
				// target's own state (e.g. a console mid-selection) genuinely
				// needs this LMB-up delivered normally, or it's left stuck
				// thinking LMB is still down until the user clicks it again.
				break // let it fall thru so CallNextHookEx is also called!
			}

			//return 0 //0 is to let it thru (1 was to swallow)
			//XXX: let it fall thru so CallNextHookEx is also called!

			//actually we can't let it thru because LMB Down was eaten, so if LMBUP is allowed then when u move say firefox's Help popup menu while hovering on About it will open About as if just clicked because it triggers on LMBUp!
			return 1 //eat it
		} // else let it pass

	case WM_RBUTTONUP: //RMB released aka RMBUP aka RMB UP
		session := activeSession.Load()
		if session == nil {
			break // No drag or resize is active, do nothing!
		}
		if session.mode == ModeResize {
			viaRecovery := session.viaMissedGestureRecovery
			//if resizing.Load() && currentDrag != nil {
			softReset(true)
			if nowDiff := time.Since(start); nowDiff > Duration5ms {
				logf("stutter7 %d ns", nowDiff.Nanoseconds()) // FIXME: hitting only this one! yep it's hideOverlay(), do it in wndProc heh!
			}
			if viaRecovery {
				// See the identical comment in WM_LBUTTONUP.
				break
			}
			/*
				(alt+z to toggle word wrapping)
				Claude 5 Sonnet High Thinking said:
				"Honest caveat: your app also takes mouse capture (SetCapture(mainMsgHwnd)) once the first move is processed, and releases it via a posted (async) message from softReset. So there's a small theoretical race where, right at button-release, capture might not have been relinquished yet by the time this up-event is routed — in which case it'd go to your hidden window instead of conhost, not fixing the "stuck" state that one time. In practice, for any drag lasting more than a few ms (i.e. essentially all real drags), the posted release will have long since been processed, so this should work correctly the overwhelming majority of the time. If you find it's still occasionally sticky in testing, the more bulletproof fix is to have softReset post the capture-release and then, only for recovery sessions, post a second message that gets processed after it and re-injects a synthetic up-click via SendInput from the main thread (same pattern as WM_INJECT_SEQUENCE) — I didn't implement that since it's meaningfully more invasive and worth validating the simple fix first."
			*/

			return 1 // Swallow
		}

	case WM_RBUTTONDOWN: //RMB pressed aka RMBDown aka RMBdrag
		winDown, shiftDown, ctrlDown, altDown := modifierKeyState()
		// var winDown bool = keyDown(VK_LWIN) || keyDown(VK_RWIN)
		// var shiftDown bool = keyDown(VK_SHIFT)
		// var ctrlDown bool = keyDown(VK_CONTROL)
		// var altDown bool = keyDown(VK_MENU)
		if winDown && !shiftDown && !altDown && !ctrlDown { // only if winkey without any modifiers
			started, bypassed := tryBeginResizeGestureAt(info.Pt, false)
			if bypassed {
				break // target is fullscreen; let event through
			}
			markGestureUsedOnce()

			if !started {
				logf("Failed to begin Resize gesture (reason why should be above ^) on winkey+RMB pressed")
			}

			if nowDiff := time.Since(start); nowDiff > Duration5ms {
				logf("stutter6 %d ns", nowDiff.Nanoseconds())
			}
			return 1 // Swallow
		} //if

	case WM_MBUTTONDOWN: //MMB pressed
		winDown, shiftDown, ctrlDown, altDown := modifierKeyState()

		if winDown && !ctrlDown && !altDown {
			//winDOWN and MMB pressed without ctrl/alt but maybe or not shiftDOWN too, it's a gesture of ours:
			started, bypassed := tryPerformMMBGestureAt(info.Pt, shiftDown)
			if bypassed {
				break // target is fullscreen; let event through
			}
			markGestureUsedOnce()

			if !started {
				logf("Failed to perform winkey+MMB gesture (reason why should be above ^, if any)")
			}

			if nowDiff := time.Since(start); nowDiff > Duration5ms {
				logf("stutter5 %d ns", nowDiff.Nanoseconds())
			}

			return 1 // swallow MMB
		} // the 'if' in MMB
	} //switch

	if nowDiff := time.Since(start); nowDiff > Duration5ms {
		logf("stutter3 %d ns", nowDiff.Nanoseconds())
	}

	// Always pass the event down the chain so other apps don't break
	res1111 := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
	if nowDiff := time.Since(start); nowDiff > Duration5ms {
		logf("stutter4 %d ns", nowDiff.Nanoseconds()) // 1 million ns is 1 ms
	}

	return res1111.R1
}

/* ---------------- Main ---------------- */

func createMessageWindow() (windows.Handle, error) {
	if curThreadID := windows.GetCurrentThreadId(); mainThreadID != curThreadID {
		exitf(1, "unexpected: main loop thread and wndProc are on different threads mainThreadID: %d, curThreadID: %d", mainThreadID, curThreadID)
	}
	classNameUTF16, err := windows.UTF16PtrFromString(winbollocksHiddenClassName)
	if err != nil {
		return 0, fmt.Errorf("UTF16PtrFromString failed for class name %s, err: %w", winbollocksHiddenClassName, err)
	}

	var wc WNDCLASSEX
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	wc.LpfnWndProc = wndProc
	wc.LpszClassName = classNameUTF16
	wc.HInstance = selfHInstance

	procSetLastError.Call(0)
	// Register class — check return value

	if res2 := procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc))); res2.Failed() { //err2 != nil || ret == 0 {
		//lastErr := windows.GetLastError()
		return 0, fmt.Errorf("RegisterClassEx failed: %w", res2.Err) //, lastErr) //XXX: multiple %w is legal in Go v1.20+ (Feb 2023)
	} else {
		hiddenClassRegistered.Store(true)
	}

	res3 := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(classNameUTF16)),
		0,
		0,
		0, 0, 0, 0,
		0,
		0,
		uintptr(wc.HInstance),
		0,
	)
	//if err3 != nil || hwndRaw == 0 {
	if res3.Failed() {
		//lastErr := windows.GetLastError()
		return 0, fmt.Errorf("CreateWindowEx failed: %w", res3.Err) // (error code: %w)", err3, lastErr)
	}

	return windows.Handle(res3.R1 /*aka hwndRaw*/), nil
}

var (
	hookThreadID atomic.Uint32
	mainThreadID uint32 // this one's guaranteed orderly set/read, as the code stands currently!
	// Optional: prepare a mutex for later when we secure 'currentDrag'
	// dragStateMutex sync.RWMutex
)
var hookPanicPayload atomic.Value // We use atomic for thread-safety
var mainAcknowledgedShutdown = make(chan struct{})

// hookWorkerDone is closed by hookWorker's own teardown closure once it
// reaches a genuine no-panic exit (WM_QUIT received normally, no recover()
// payload at all). primary_defer() waits on this — after wincoe.WaitAnyKey()
// — so hookWorker's OS thread has actually finished (both hooks unhooked,
// goroutine returned) before logs get flushed and the process exits.
// Mirrors the existing logWorkerDone/closeAndFlushLog() pattern.
var hookWorkerDone = make(chan struct{})

// hookWorkerSecondaryDefer is hookWorker's analogue of secondary_defer(): a
// last-resort safety net that only does anything if hookWorker's own
// cross-thread-panic-bridge closure (deferred after this one, so it runs
// first — LIFO) panics again while handling the original panic.
//
// Unlike secondary_defer(), reaching this with recover() == nil is NOT a
// bug here — it's the ordinary path once the panic-bridge closure's
// clean-exit branch (see hookWorker's tail code) runs and returns normally
// so this goroutine's OS thread can actually finish. Nothing to do then.
//
// Deliberately uses directLoggerf/os.Exit instead of logf/closeAndFlushLog:
// this runs on a different goroutine than main's own shutdown sequence
// (primary_defer/secondary_defer), so calling closeAndFlushLog() here could
// race a concurrent close(logChan) there — closing an already-closed
// channel panics. directLoggerf writes synchronously and bypasses logChan
// entirely, so it's always safe to call from here no matter what main is
// doing concurrently.
//
// XXX: this is a 3rd os.Exit call site, breaking the existing "oughtta be
// the only os.Exit, 1of2/2of2" comments in primary_defer()/secondary_defer().
// It's a deliberate, narrow exception: a panic inside the panic-bridge
// closure itself means we can no longer trust any cross-thread signaling to
// reach main reliably, so a direct exit here is the safer choice. Worth
// renumbering those comments to 1of3/2of3/3of3 if you're OK with this.
func hookWorkerSecondaryDefer() {
	if r2 := recover(); r2 != nil {
		directLoggerf("!hookWorker secondary defer here! [CRITICAL ERROR IN hookWorker's panic-bridge defer]: '%v'\n%s\n----snip----", r2, debug.Stack())
		const exitCodeNow = 120
		directLoggerf("!hookWorker secondary defer here! forcing process exit with code %d (hookWorker's own exit code was: '%d')", exitCodeNow, currentExitCode.Load())
		closeAndFlushLog()   // still flush the old ones tho.
		os.Exit(exitCodeNow) // XXX: oughtta be the only os.Exit! well 3of3
	}
	// recover() == nil: expected — see doc comment above.
	// so this case just falls thru to next defer ie. runtime.UnlockOSThread() then it will thread finish/exit.
}

func hookWorker() {
	// 1. Lock this goroutine to a single, dedicated OS thread. Crucial!
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Run this last to catch any secondary panics
	//defer secondary_defer() //this runs second but only if first doesn't os.exit ie. it fails/panics! replaced by hookWorkerSecondaryDefer below
	defer hookWorkerSecondaryDefer() // safety net: only force-exits on a genuine secondary panic inside the closure below; a normal return there (clean-exit path) is expected and a no-op here.
	//defer primary_defer() //this runs first, can't run this here due to needing to be on same thread as main to deinit other things!
	// defer time.Sleep(2 * time.Second)
	// defer procPostQuitMessage.Call(0) //FIXME: no effect even with 2 sec delay after it!

	// The Cross-Thread Panic Bridge
	defer func() {
		if r := recover(); r != nil {
			// 1. Store the panic payload so main can read it
			hookPanicPayload.Store(r)

			if status, ok := r.(exitStatus); ok {
				currentExitCode.Store(status.Code)
				// This was an intentional exit(code)
				//if code != 0 {
				logf("hookWorker thread intentionally exited with code: '%d' and error message: '%s'", currentExitCode.Load(), status.Message)
				//}
			} else {
				currentExitCode.Store(1) //doneFIXME: this is accessed from two diff. threads, protect it.
				stack := debug.Stack()
				logf("--- hookWorker thread CRASH: %v ---\nStack: %s\n--- END---", r, stack)
			}
			logf("CRITICAL: from hookWorker, signaling main thread to die...")

			if mainThreadID == 0 {
				badprogramming("BUG: mainThreadID shouldn't be 0 here!")
			}
			// 2. Nuke the main thread's GetMessage loop, works only if systray popup menu isn't open!
			// Use PostThreadMessage to mainThreadId, or post WM_CLOSE to your main HWND
			if res := procPostThreadMessage.Call(uintptr(mainThreadID), WM_QUIT, 0, 0); res.Failed() { //cantbeTODO: investigate if mainThreadID can be unset or 0 here.
				logf("hookWorker panic-bridge: PostThreadMessage(WM_QUIT) to mainThreadID=%d failed, err: %v", mainThreadID, res.Err)
			}
			//doneFIXME: what if main is dead too, and would ignore the signal or what, then we exit here? sure after X seconds

			if mainMsgHwnd != 0 {
				// Post to the Window Handle, NOT the Thread ID.
				// This cuts through modal menus like the systray popup menu!
				if res := procPostMessage.Call(uintptr(mainMsgHwnd), WM_CLOSE, 0, 0); res.Failed() {
					logf("hookWorker panic-bridge: PostMessage(WM_CLOSE) to mainMsgHwnd=0x%X failed, err: %v", mainMsgHwnd, res.Err)
				}
			}
			/* When you right-click your tray icon and the menu appears, the code is stuck inside the TrackPopupMenu Win32 call.
				That function runs its own private message loop.
			   The Problem: It looks for mouse clicks and keyboard hits. If it sees a message with HWND == NULL (which is what PostThreadMessage creates),
			   it often just throws it away. Your main loop never gets to see it.
			*/

			const waitForMainSeconds = 2
			// 2. The Watchdog Timer
			logf("hookWorker is now waiting %d seconds for main to exit us...", waitForMainSeconds)
			select {
			case <-mainAcknowledgedShutdown: // Check if closed
				logf("main() acknowledged shutdown. hookWorker is now waiting indefinitely for main to terminate the process.")
				// We wait here forever. Why? Because we want the main thread's
				// deinit() to be the one that finishes and potentially waits for
				// the user's "Press a key or Enter" keypress. It then calls os.Exit there in primary_defer() in main() thread.
				close(hookWorkerDone)
				select {}

			case <-time.After(waitForMainSeconds * time.Second):
				directLoggerf("CRITICAL: Main thread unresponsive after %d seconds. hookWorker will now hang forever", waitForMainSeconds)
				// Main is frozen.
				//we let it fall thru to below which means it hangs forever
			}
			// Panic path, main unresponsive: block forever, unchanged from
			// before this change. hookWorkerDone is intentionally NOT closed
			// here — primary_defer()'s wait on it will simply time out,
			// which is the correct, already-bounded fallback for this case.
			directLoggerf("hookWorker clean exit (but not quitting thread because main is hung or taking too long to exit)")
			close(hookWorkerDone)
			select {} //infinite wait, or else secondary_defer() will trigger, doneFIXME: find a better way to not os.Exit and still exit this thread. like tell secondary_defer to not os.Exit via a global bool?!
		} //if recover

		// True clean, no-panic exit: the message loop above returned because
		// it received WM_QUIT (posted by deinit() on the main thread), with
		// no panic anywhere. Signal main, then let this goroutine — and its
		// locked OS thread — actually finish, instead of parking in select{}
		// forever like the panic paths above.
		logf("hookWorker clean exit, signaling main and finishing thread")
		close(hookWorkerDone)
	}() // defer

	// 2. Save the OS Thread ID so our main thread can talk to it later
	//hookThreadID = windows.GetCurrentThreadId()
	hookThreadID.Store(windows.GetCurrentThreadId())
	htidcached := hookThreadID.Load()
	if mainThreadID == htidcached {
		exitf(1, "main loop msg and hooks are NOT on two different threads(but same 0x%X tid), this will never happen unless code logic is broken!", htidcached)
	}
	logf("Hook worker thread started. ThreadID: %d", htidcached)

	setAndVerifyPriority()

	// 3. INSTALL HOOKS HERE
	mouseCallback = windows.NewCallback(mouseProc)
	res1 := procSetWindowsHookEx.Call(WH_MOUSE_LL, mouseCallback, 0, 0)
	// if err != nil || h == 0 {
	if res1.Failed() {
		exitf(1, "Got error: %v", res1.Err)
		unreachable()
	} else {
		mouseHook = windows.Handle(res1.R1)
		defer func() {
			if res := procUnhookWindowsHookEx.Call(uintptr(mouseHook)); res.Failed() {
				logf("failed to unhook mouseHook: %v", res.Err)
			} else {
				logf("unhooked mouseHook")
			}
			mouseHook = 0
		}()
	}

	kbdCB := windows.NewCallback(keyboardProc)
	res2 := procSetWindowsHookEx.Call(
		WH_KEYBOARD_LL,
		kbdCB,
		0, // hMod = 0 for low-level
		0, // dwThreadId = 0 = global
	)
	// if err != nil || hk == 0 {
	if res2.Failed() {
		exitf(1, "Got error: %v", res2.Err)
		unreachable()
	} else {
		kbdHook = windows.Handle(res2.R1)
		defer func() {
			if res := procUnhookWindowsHookEx.Call(uintptr(kbdHook)); res.Failed() {
				logf("failed to unhook kbdHook: %v", res.Err)
			} else {
				logf("unhooked kbdHook")
			}
			kbdHook = 0
		}()
	}

	// 4. The Thread's Private Message Loop
	var msg MSG
	for {
		//exitf(1, "temp. manual panic")
		res3 := procGetMessage.Call( // GetMessage here calls the hook(s)
			uintptr(unsafe.Pointer(&msg)),
			0, 0, 0,
		)

		const minus1 = ^uintptr(0)
		// ret == 0 means WM_QUIT was received. ret == -1 aka ^uintptr(0) is an error.
		//if ret == 0 || ret == minus1 {
		if res3.Failed() || res3.R1 == 0 /*aka WM_QUIT*/ {
			logf("Hook worker thread received WM_QUIT(==0) or error(==%d) ret=%d, exiting and unhooking...", minus1, res3.R1)
			break
		}

		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
	}
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

// Use an atomic Int64 to store UnixNano
var lastResizeUnixNano atomic.Int64

// ShouldThrottle returns true if the last action happened too recently.
// Uses the constant directly for a zero-allocation, fast check.
func ShouldThrottle() bool {
	var now int64 = time.Now().UnixNano()
	var last int64 = lastResizeUnixNano.Load()

	// thresholdNanos is calculated at compile-time/startup
	const thresholdNanos int64 = int64(forceMoveOrResizeActionsToBeThisManyMSApart) * int64(time.Millisecond)

	return (now - last) < thresholdNanos
}

// MarkAsResizedNow marks it as "just started processing" — so, called early.
func MarkAsResizedNow() {
	lastResizeUnixNano.Store(time.Now().UnixNano())
}

const forceMoveOrResizeActionsToBeThisManyMSApart = 16 // 16ms is 60fps, 10ms is 100fps

const WS_THICKFRAME = 0x00040000 // or WS_SIZEBOX which has same value (as per chatgpt 5.5)

func handleActualMoveOrResize(data WindowMoveData, bypassThrottle bool) {
	//Top of handleActualMoveOrResize, before the rate-limit check (capture should be set even if we throttle the actual SetWindowPos):
	// Lazy once-per-session SetCapture.
	// We are guaranteed to be on the main thread here (wndProc context).
	if cur := activeSession.Load(); cur != nil && captureHeldForSession.Load() != cur {
		_ = procSetCapture.Call(uintptr(mainMsgHwnd))
		/*
			One caveat worth stating: since you're using WH_MOUSE_LL, you receive all mouse events globally regardless of capture.
			 SetCapture here is about preventing other windows from acting on cursor interactions during a drag, not about receiving events yourself.
			 So if you find a future reason to drop it entirely, no events would be lost.
			  - Claude 4.6 Sonnet High Thinking
		*/
		captureHeldForSession.Store(cur)
	}

	// 1. RATE LIMIT: Don't hit the OS more than once every 10-16ms (approx 60-100Hz)
	// Most monitors are 60Hz-144Hz. Anything faster than 10ms is wasted CPU.
	// If bypassed (e.g. from a coalesced batch), we MUST apply it so we don't drop the final state.
	if !bypassThrottle && ShouldThrottle() {
		// dropped because of execution speed limit
		droppedMoveOrResizeEvents.Add(1) //TODO: so this was queued but decided not to do the action, maybe we need a diff. counter for each kind of dropped type/reason?
		return
	}
	// Mark EARLY — we've decided to process this one
	MarkAsResizedNow()

	// defer func() {
	// 	//lastResize = time.Now() //doneFIXME: this is racey
	// 	// To set the value:
	// 	//lastResize.Store(time.Now())
	// 	MarkAsResized()
	// }()

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

	isResizeEvent := data.W != 0 || data.H != 0 //is a resize not move event

	// //disabled, some windows are resizeable yet still hit this
	// if false && isResizeEvent {
	// 	//Check if the window is actually resizable
	// 	//FIXME: this is wrong because Find dialog during in-progress search is resizeable yet hits this, but Find dialog that says it found nothing hits this too but that's trully unresizeable.
	// 	style, err := getWindowLongPtr(target, GWL_STYLE)
	// 	if err == nil && (uint32(style)&WS_THICKFRAME == 0) {
	// 		logf("Refusing to resize unresizeable window HWND=0x%X", target)
	// 		return
	// 	}
	// }

	//is procSetWindowPos async ?
	var async bool = (data.Flags & SWP_ASYNCWINDOWPOS) != 0
	// If it's a synchronous resize event, run our ultra-smooth Two-Step pipeline
	if !async && isResizeEvent {
		//XXX: so we first resize then move, not do both in one call, this makes the unresizable Find dialog that says nothing was found in regedit be resizable!

		// // --- SAFETY LAYER: ENFORCE SANE MINIMUMS ---
		// // Prevent the window from being resized to 0x0 or negative dimensions,
		// // which makes it disappear completely.
		// const safeMinDim = 32 // 32x32 pixels is an excellent safe floor
		// if data.W < safeMinDim {
		// 	data.W = safeMinDim
		// }
		// if data.H < safeMinDim {
		// 	data.H = safeMinDim
		// }

		// --- STEP 1: RESIZE IN-PLACE (PREVENTS JUMPING) ---
		// We use SWP_NOMOVE so Windows calculates size restrictions at the current position.
		var start time.Time
		if !async {
			start = time.Now()
		}
		res1 := procSetWindowPos.Call( //XXX: this is blocking, depends on target window's responsiveness! which is why this happens on wndProc not inside mouseProc btw.
			uintptr(target),
			uintptr(data.InsertAfter),
			0, 0, // X and Y are ignored because of SWP_NOMOVE

			// #nosec G115 -- safe: Win32 dimensions are sign-extended from int32 into uintptr
			uintptr(data.W),
			// #nosec G115 -- safe: Win32 dimensions are sign-extended from int32 into uintptr
			uintptr(data.H),

			uintptr(data.Flags|SWP_NOMOVE),
		)
		if !async {
			duration := time.Since(start)
			const ifMoreThanMs = 25 //ms
			if duration > ifMoreThanMs*time.Millisecond {
				//only in ModeResize and when SWP_ASYNCWINDOWPOS isn't used(but now it is), then if you try to resize the Find window in regedit (first u must run as admin because regedit runs as admin) while it's searching for some random text! then this triggers!
				logf("SetWindowPos/Resize for HWND 0x%X took %d ms >%dms", target, duration.Milliseconds(), ifMoreThanMs)
			}
		}
		//if err1 != nil { //aka ret == 0 { //failed
		if res1.Failed() {
			//errCode, _, _ := procGetLastError.Call()
			logf("SetWindowPos/Resize failed(from within main message loop): hwnd=0x%x err=%v", target, res1.Err)
			// if errors.Is(err1, windows.ERROR_ACCESS_DENIED) { // Access denied (UIPI likely)
			//if errCode == 5 { // Access denied (UIPI likely)
			if res1.ErrIs(windows.ERROR_ACCESS_DENIED) { // ==5 aka Access denied (UIPI likely)
				showTrayInfo(selfName, "Cannot resize elevated window (access denied), you'd have to run as admin.")
			}
		}
		// --- STEP 2: MEASURE WHAT WINDOWS ACTUALLY ALLOWED ---
		var r RECT
		res2 := procGetWindowRect.Call(uintptr(target), uintptr(unsafe.Pointer(&r)))
		/*
							1. Why GetWindowRect Seems Out of Sync

			When you call SetWindowPos without SWP_ASYNCWINDOWPOS (sync mode), it does indeed block until the target window processes the WM_WINDOWPOSCHANGING and WM_WINDOWPOSCHANGED messages.

			However, Windows applications are highly asynchronous internally. When a modern app (especially one using a custom UI framework, WPF, or complex drawing like Defraggler) receives the resize message, it often just updates its internal state and posts a paint message to itself to redraw later. Furthermore, during WM_WINDOWPOSCHANGING, an application can modify the WINDOWPOS structure to enforce its own minimum size.

			If it does this, SetWindowPos returns, but GetWindowRect might briefly return an intermediate state, or the window manager might not have fully reconciled the visual bounds yet.
		*/
		//if ret == 0 {
		if res2.Failed() {
			//errCode, _, _ := procGetLastError.Call()
			logf("GetWindowRect after resize failed: hwnd=0x%x, err:%v", target, res2.Err)
			// Safety: If we can't get the Rect, we can't do Anti-Slide or Overlay updates safely.
			return
		}

		actualW := r.Right - r.Left
		actualH := r.Bottom - r.Top
		// // ---------------------------------------------------------
		// // TEMP TEST MOCK: Force a fake clamp!
		// // Pretend this window refuses to grow wider than 500 or taller than 400
		// if actualW != 500 {
		// 	actualW = 500
		// 	// We must force the window to actually stay this size for Step 4 to preserve it
		// 	procSetWindowPos.Call(uintptr(target), 0, 0, 0, uintptr(actualW), uintptr(actualH), uintptr(data.Flags|SWP_NOMOVE))
		// }
		// if actualH != 400 {
		// 	actualH = 400
		// 	procSetWindowPos.Call(uintptr(target), 0, 0, 0, uintptr(actualW), uintptr(actualH), uintptr(data.Flags|SWP_NOMOVE))
		// }
		// // ---------------------------------------------------------
		deltaW := actualW - data.W
		deltaH := actualH - data.H

		// --- STEP 3: CALCULATE FAULTLESS POSITIONS ---
		correctedX := data.X
		correctedY := data.Y

		// Correct X based on the tracked zone
		switch data.ResizeZone {
		case ZONE_TOP_LEFT, ZONE_MID_LEFT, ZONE_BOT_LEFT:
			correctedX = data.X - deltaW
		case ZONE_CENTER:
			correctedX = data.X - (deltaW / 2)
			correctedY = data.Y - (deltaH / 2)
		}

		// Correct Y based on the tracked zone
		switch data.ResizeZone {
		case ZONE_TOP_LEFT, ZONE_TOP_CENTER, ZONE_TOP_RIGHT:
			correctedY = data.Y - deltaH
		}
		// --- STEP 4: MOVE TO FINAL CORRECT CORNER POSITION ---
		// We use SWP_NOSIZE because the size was already locked down perfectly in Step 1.
		res3 := procSetWindowPos.Call(
			uintptr(target),
			uintptr(data.InsertAfter),

			// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
			uintptr(correctedX),
			// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
			uintptr(correctedY),

			0, 0, // W and H are ignored because of SWP_NOSIZE
			uintptr(data.Flags|SWP_NOSIZE),
		)
		//if ret2 == 0 { //failed
		if res3.Failed() {
			//errCode, _, _ := procGetLastError.Call()
			logf("SetWindowPos/Move-after-Resize failed(from within main message loop): hwnd=0x%x err=%v", target, res3.Err)
			// if errCode == 5 { // Access denied (UIPI likely)
			if res3.ErrIs(windows.ERROR_ACCESS_DENIED) { // ==5 aka Access denied (UIPI likely)
				showTrayInfo(selfName, "Cannot resizemove elevated window (access denied), you'd have to run as admin.")
			}
		}

		// Always update your visual overlay bounding variables with the true positions
		nx, ny, nw, nh := correctedX, correctedY, actualW, actualH

		session := activeSession.Load()
		if session != nil {
			//session := *ptr // noneedTODO: use this on-stack thing for other session:=activeSession.Load() places; so this was for "The compiler can then perform an optimization called Register Promotion. It can load your entire struct's fields directly into CPU registers (RAX, RBX, etc.)."
			if session.mode != ModeResize {
				//if !resizing.Load() {
				logf("delayed resizing detected, while not 'resizing'.")
			}
			//update overlay
			startW := session.state.startRect.Right - session.state.startRect.Left
			startH := session.state.startRect.Bottom - session.state.startRect.Top
			updateOverlay(nx, ny, nw, nh, startW, startH)
			// } else {
			// 	logf("did a resize but the overlay wasn't updated/shown due to gesture wasn't in effect anymore.")
		}
	} else {
		//here for ModeMove OR async resize
		//XXX: unfixable bug here with async resize, it will move the window even tho the window resisted resizing, during resize only!
		// FALLBACK: Normal single-pass execution for asynchronous mode or simple moves
		res4 := procSetWindowPos.Call(
			uintptr(target),
			uintptr(data.InsertAfter),

			// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
			uintptr(data.X),
			// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
			uintptr(data.Y),
			// #nosec G115 -- safe: Win32 dimensions are sign-extended from int32 into uintptr
			uintptr(data.W),
			// #nosec G115 -- safe: Win32 dimensions are sign-extended from int32 into uintptr
			uintptr(data.H),

			uintptr(data.Flags),
		)
		//if ret == 0 { //failed
		if res4.Failed() {
			//errCode, _, _ := procGetLastError.Call()
			logf("SetWindowPos/Move-or-AsyncResize failed(from within main message loop): hwnd=0x%x err=%v", target, res4.Err)
			// if errCode == 5 { // Access denied (UIPI likely)
			if res4.ErrIs(windows.ERROR_ACCESS_DENIED) { // ==5 aka Access denied (UIPI likely)
				showTrayInfo(selfName, "Cannot Move-or-AsyncResize elevated window (access denied), you'd have to run as admin.")
			}
		}
	}
}

// makeLParam packs signed 16-bit x,y coordinates into a Win32 LPARAM (uintptr).
// This ensures proper sign-extension to 64 bits on x64, matching MAKELPARAM / LPARAM semantics.
// Handles negative coordinates (multi-monitor setups where monitors are left/above primary).
func makeLParam(x, y int32) uintptr { // grok again
	//AND ensures 16-bit truncation, prevents high bits bleed. No warnings, handles negatives.
	// cast doesn't change bits only interpretation
	//The cast to uint32 doesn't "change" the bits in a harmful way for your scenario (2's complement representation is preserved,
	// and &0xFFFF truncates to the low 16 bits correctly before shifting).
	// The following line suppresses the warning:
	// #nosec G115 -- safe: coords are screen pixels, always fit in 16 bits
	//return uintptr((uint32(y)&0xFFFF)<<16 | (uint32(x) & 0xFFFF))

	// Pack low 16 bits of x and y (preserves 2's complement for negatives)
	// 1. Pack exactly as before (low 16=x, high 16=y, 2's complement preserved)
	packed := (uint32(y)&0xFFFF)<<16 | (uint32(x) & 0xFFFF)

	// Critical: cast to int32 first (interprets bit 31 as sign),
	// then to uintptr (sign-extends to 64 bits on x64).
	// This matches C behavior and Microsoft's extension rules.
	// 2. Interpret as signed 32-bit (this captures whether bit 31 is set)
	// 3. Convert to uintptr → proper sign extension to 64 bits
	// #nosec G115 -- safe: coords are screen pixels, always fit in 2x16 bits
	return uintptr(int32(packed))
}

// UnpackLParam extracts the signed X and Y coordinates from a window message lParam.
// This correctly handles negative coordinates on multi-monitor setups.
func UnpackLParam(lParam uintptr) (x, y int32) {
	/* in this:
	// x := int32(lParam & 0xFFFF)
	// y := int32((lParam >> 16) & 0xFFFF)
	// lParam & 0xFFFF extracts the lower 16 bits. Go sees this result as an unsigned 32-bit or 64-bit number (depending on your architecture) because lParam is a uintptr.
	//The bits look like this in memory: 0x0000FF9C.
	//You then cast it directly to int32. Because the highest bit of 0x0000FF9C is 0, Go says: "This is a positive number!" 4. 0x0000FF9C in decimal is 65436. You lost the negative sign.
	*/
	/*Why you don't even need & 0xFFFF here:
	  In Go, casting a larger integer to an int16 automatically discards the upper bits (truncation).
		int16(lParam) takes only the lower 16 bits. If it's 0xFF9C, it becomes -100 as an int16. Then int32(...) turns it into -100 as an int32.
		int16(lParam >> 16) shifts the high word into the lower positions and does the exact same thing for the Y coordinate.
	  If you prefer to keep the mask explicitly visible for code readability, you can keep it, but it must be wrapped inside the int16:
	*/
	// x = int32(int16(lParam))
	// y = int32(int16(lParam >> 16))
	// #nosec G115 -- safe: explicitly truncating to 16-bit to unpack Win32 coordinates
	x = int32(int16(lParam & 0xFFFF))
	// #nosec G115 -- safe: explicitly truncating to 16-bit to unpack Win32 coordinates
	y = int32(int16((lParam >> 16) & 0xFFFF))
	return x, y
}

func wtsSessionChangeName(code uintptr) string {
	switch code {
	case WTS_SESSION_LOCK:
		return "lock"
	case WTS_SESSION_UNLOCK:
		return "unlock"
	default:
		return fmt.Sprintf("0x%x", code)
	}
}

var wndProc = windows.NewCallback(func(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_DO_SETWINDOWPOS:
		if coalesceMoveResizeEvents.Load() {
			drainMoveChannelCoalesced() // ← new coalescing version
		} else {
			drainMoveChannel() // Pull everything from the channel, sequentially
		}
		return 0 // Handled

	case WM_HIDE_OVERLAY:
		hideOverlay()
		return 0

	case WM_BRING_TO_FRONT:
		target := windows.Handle(wParam)
		if target == 0 {
			logf("WM_BRING_TO_FRONT: received with zero HWND; ignoring")
			return 0
		}
		if res := procSetWindowPos.Call(
			uintptr(target),
			uintptr(HWND_TOP),
			0, 0, 0, 0,
			SWP_NOMOVE|SWP_NOSIZE|SWP_NOACTIVATE,
		); res.Failed() {
			logf("WM_BRING_TO_FRONT: SetWindowPos(HWND_TOP) on HWND=0x%X failed: %v", target, res.Err)
		}
		return 0

	// case WM_DO_SET_CAPTURE:
	// 	target := windows.Handle(wParam)
	// 	if target == 0 {
	// 		target = mainMsgHwnd // fallback
	// 		logf("BUG: had to fallback the target to mainMsgHwnd 0x%X", target)
	// 	}
	// 	res1 := procSetCapture.Call(uintptr(target))
	// 	res2 := procGetCapture.Call()
	// 	prev := windows.Handle(res1.R1)
	// 	after := windows.Handle(res2.R1)
	// 	if after != target {
	// 		logf("WARNING: SetCapture in main thread failed to take ownership. Got 0x%X, want 0x%X, prev was 0x%X", after, target, prev)
	// 	}
	// 	// Success case stays silent
	// 	return 0

	case WM_DO_RELEASE_CAPTURE:
		res2 := procGetCapture.Call() //CheckNone
		prev := windows.Handle(res2.R1)
		res1 := procReleaseCapture.Call() //CheckBool
		if res1.Failed() {
			logf("in wndProc, WM_DO_RELEASE_CAPTURE: ReleaseCapture failed, err: %v", res1.Err)
		}
		res3 := procGetCapture.Call() //CheckNone
		current := windows.Handle(res3.R1)
		// Normal case (prev=1 or 0, current=0) → completely silent
		if current != 0 {
			logf("in wndProc part2of2, WM_DO_RELEASE_CAPTURE says the current capture (after releasing) is still 0x%X instead of none aka 0", current)
		} else if prev != 0 && prev != mainMsgHwnd && prev != 1 { // 1 is the common "desktop" value
			// Only log unusual previous owners (debug only)
			logf("in wndProc, WM_DO_RELEASE_CAPTURE: previous owner was unexpected 0x%X (mainMsgHwnd=0x%X)", prev, mainMsgHwnd)
		}

		return 0

	case WM_WTSSESSION_CHANGE:
		switch wParam {
		case WTS_SESSION_LOCK, WTS_SESSION_UNLOCK:
			// Real key/button releases that happen on the secure desktop
			// while locked are invisible to our low-level hooks (it's a
			// separate desktop object entirely), so two independent pieces
			// of state can go stale across a lock/unlock cycle:
			//
			//  1. winGestureUsed can be left stuck 'true' if the physical
			//     winkey-up happened on the secure desktop: keyboardProc
			//     never ran to clear it or inject the compensating
			//     synthetic up (see its WM_KEYUP/WM_SYSKEYUP handling),
			//     yet the real winkey is genuinely up again once we
			//     unlock. Left stuck, some unrelated future stand-alone
			//     winkey tap would have its Start-menu-opening up-event
			//     incorrectly swallowed.
			//
			//  2. Any active drag/resize session is unsafe to keep
			//     trusting. We can't fall back on
			//     GetAsyncKeyState(VK_LBUTTON/VK_RBUTTON) to check whether
			//     the initiating button is still physically held, because
			//     starting a session ALWAYS swallows the real LMB/RMB-down
			//     (see WM_LBUTTONDOWN/WM_RBUTTONDOWN in mouseProc) and, for
			//     an ordinary non-recovery session, its matching up-event
			//     too (see WM_LBUTTONUP/WM_RBUTTONUP there) — every
			//     transition we ourselves swallow is invisible to the OS's
			//     own key-state tracking, so that button's async state is
			//     permanently stale for the entire life of a self-driven
			//     session, not merely across a lock/unlock. So rather than
			//     trying to infer "is it still really held", we
			//     unconditionally drop any in-progress session across
			//     this boundary instead.
			//
			// We act on either LOCK or UNLOCK (whichever fires first for a
			// given cycle) purely for defense-in-depth; only UNLOCK is
			// strictly required, since no further input reaches us at all
			// while genuinely locked.
			winGestureUsed.Store(false)
			if session := activeSession.Load(); session != nil {
				logf("WTS session %s detected mid-%v; discarding stale drag/resize session for HWND=0x%X", wtsSessionChangeName(wParam), session.mode, session.targetWnd)
				softReset(true)
			}
		}
		return 0

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
		targetWindow := windows.Handle(wParam)
		// 2. Perform your focus logic...

		//this is here because avoids focusing window or injecting LMB from the hook
		if !forceForeground(targetWindow) {
			// 3. If fallback click is needed, use absolute coordinates:

			var extra string
			if doLMBClick2FocusAsFallback.Load() {
				extra = "; next, falling back to injected LMB click which, unfortunately, means here that it will click at the point in the window where u tried to move it which eg. in total commander might be on the exit button and it will exit!"
			} else {
				extra = "."
			}
			logf("Failed to force foreground(ie. to activate/focus window) this happens consistently when Start menu was already open(ie. press and release winkey once)%s", extra)

			if doLMBClick2FocusAsFallback.Load() {
				// 1. Extract coordinates from lParam
				x, y := UnpackLParam(lParam)
				//logf("injecting LMB click")
				// injecting a LMB_down then LMB_up so that the target window gets a click to focus and bring it to front
				// this is a good workaround for focusing it which windows wouldn't allow via procSetForegroundWindow (unless attaching to target window's thread!)
				//XXX: we LMB click at the point when gesture started because 150ms later(see HungWindowTimeout) when we realize the target window was not responding we're here and mouse woulda moved (ie. winkey+LMB drag was in progress since!) so LMB-ing where we currently are now is likely gonna LMB a background window thus focusing it instead of our target/initial window where gesture started upon.
				injectLMBClickAtCoords(x, y)

				//XXX: this is bad, it will sometimes move the window to these coords! sometimes it will fail completely because apparently window moved by some pixels down-right and thus it missed clicking it?!
				// // Don't use the raw (x,y) from lParam which might be over a button.
				// // Instead, get the target window's rect.
				// var rect RECT
				// procGetWindowRect.Call(uintptr(targetWindow), uintptr(unsafe.Pointer(&rect)))

				// // Click 10 pixels right and 10 pixels down from the top-left corner (usually safe Title Bar space)
				// safeX := rect.Left //+ 10
				// safeY := rect.Top  //+ 10

				// injectLMBClickAtCoords(safeX, safeY)
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
			if res := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))); res.Failed() {
				logf("WM_MYSYSTRAY: GetCursorPos failed, menu will appear at (0,0): %v", res.Err)
			}

			logf("popping tray menu")

			res1 := procCreatePopupMenu.Call()
			if res1.Failed() {
				logf("in wndProc, WM_MYSYSTRAY, failed to CreatePopupMenu, err=%v", res1.Err)
				return 0 // Handled
			}
			hMenu := res1.R1
			defer func() {
				if res := procDestroyMenu.Call(hMenu); res.Failed() {
					logf("in wndProc, WM_MYSYSTRAY, failed to DestroyMenu, err=%v", res.Err)
				}
			}()

			{
				var actFlags uintptr = MF_STRING // untyped constants can auto-convert, but not untyped vars(in the below call)
				if focusOnDrag.Load() {
					actFlags |= MF_CHECKED
				}
				focusText := "Activate(aka focus) window when moved if not in focus."
				appendMenuChecked(hMenu, actFlags, MENU_ACTIVATE_MOVE, focusText)
			}

			{
				var bringToFrontOnDragFlags uintptr = MF_STRING
				if bringToFrontOnDrag.Load() {
					bringToFrontOnDragFlags |= MF_CHECKED
				}
				// if !focusOnDrag.Load() { //XXX:actually don't, because if it's focused, we can bring it to front
				// 	bringToFrontOnDragFlags |= MF_DISABLED | MF_GRAYED
				// }
				bringToFrontOnDragText := "Bring already-focused(!) window to front of Z-order when starting a drag/move gesture (useful after winkey+MMB sent it to back)"
				appendMenuChecked(hMenu, bringToFrontOnDragFlags,
					MENU_TOGGLE_BRING_TO_FRONT_ON_DRAG, bringToFrontOnDragText)
			}

			{
				var actResizeFlags uintptr = MF_STRING
				if focusOnResize.Load() {
					actResizeFlags |= MF_CHECKED
				}
				focusResizeText := "Activate(aka focus) window when a resize gesture starts if not already in focus."
				appendMenuChecked(hMenu, actResizeFlags,
					MENU_TOGGLE_ACTIVATE_RESIZE, focusResizeText)
			}

			{
				var bringToFrontOnResizeFlags uintptr = MF_STRING
				if bringToFrontOnResize.Load() {
					bringToFrontOnResizeFlags |= MF_CHECKED
				}
				bringToFrontOnResizeText := "Bring already-focused(!) window to front of Z-order when starting a resize gesture (independent of the same option for drag/move above)"
				appendMenuChecked(hMenu, bringToFrontOnResizeFlags,
					MENU_TOGGLE_BRING_TO_FRONT_ON_RESIZE, bringToFrontOnResizeText)
			}

			{
				var useThreadAttachInputForFocusFlags uintptr = MF_STRING
				if useThreadAttachInputForFocus.Load() {
					useThreadAttachInputForFocusFlags |= MF_CHECKED
				}
				useThreadAttachInputForFocusText := "(dontuse)Use AttachThreadInput before attempting any window focus (else focus stealing prevention might happen)"
				appendMenuChecked(hMenu, useThreadAttachInputForFocusFlags,
					MENU_TOGGLE_USE_THREADATTACHINPUT_FOR_FOCUS, useThreadAttachInputForFocusText)
			}

			{
				var lmbFlags uintptr = MF_STRING
				if doLMBClick2FocusAsFallback.Load() {
					lmbFlags |= MF_CHECKED
				}
				if !focusOnDrag.Load() && !focusOnResize.Load() {
					lmbFlags |= MF_DISABLED | MF_GRAYED
				}
				doLMBClick2FocusAsFallbackText := "Fallback: Use Left Mouse Click to focus (Warning: will click underlying UI elements)."
				appendMenuChecked(hMenu, lmbFlags,
					MENU_USE_LMB_TO_FOCUS_AS_FALLBACK, doLMBClick2FocusAsFallbackText)
			}

			{
				var rlFlags uintptr = MF_STRING
				if ratelimitOnMove.Load() {
					rlFlags |= MF_CHECKED
				}
				ratelimitText := "Rate-limit window moves(by 5x, uses less CPU but looks choppier so ur subconscious will hate it)"
				appendMenuChecked(hMenu, rlFlags,
					MENU_RATELIMIT_MOVES, ratelimitText)
			}

			{
				var sldrFlags uintptr = MF_STRING
				if shouldLogDragRate.Load() {
					sldrFlags |= MF_CHECKED
				}
				// Disable (grey) the "Log rate of moves" item when rate-limit is off
				if !ratelimitOnMove.Load() {
					sldrFlags |= MF_DISABLED | MF_GRAYED
				}
				sldrText := "Log rate of moves(only if rate-limit above is enabled)"
				appendMenuChecked(hMenu, sldrFlags,
					MENU_LOG_RATE_OF_MOVES, sldrText)
			}

			{
				var asyncFlags uintptr = MF_STRING
				if asyncResize.Load() {
					asyncFlags |= MF_CHECKED
				}
				asyncText := "Use Async Window Positioning for Resizing(bugged for unresizable windows - it moves them)(don't use this)"
				appendMenuChecked(hMenu, asyncFlags,
					MENU_TOGGLE_ASYNC_RESIZE, asyncText)
			}

			{
				var reqWinDownFlags uintptr = MF_STRING
				if requireWinDownHeldDuringGesture.Load() {
					reqWinDownFlags |= MF_CHECKED
				}
				reqWinDownText := "Require holding down WinKey while performing the gesture(move/resize) - if not you'll hit edge cases" //such as(not anymore this): if you do Winkey+L to lock, then release winkey and LMB(or RMB if resize) then you unlock, the gesture is still in effect(if this is false); actually not anymore now that I got lock/unlock hooks and I reset when winkey+L locks !
				appendMenuChecked(hMenu, reqWinDownFlags,
					MENU_TOGGLE_REQUIRE_WINDOWN, reqWinDownText)
			}

			{
				var coalesceEventsFlags uintptr = MF_STRING
				if coalesceMoveResizeEvents.Load() {
					coalesceEventsFlags |= MF_CHECKED
				}
				coalesceEventsText := "Coalesce Move/Resize (ignores queue history to keep drag responsive), if off it's rate-limited to 60fps"
				appendMenuChecked(hMenu, coalesceEventsFlags,
					MENU_TOGGLE_COALESCE_EVENTS, coalesceEventsText)
			}

			{
				var immediateOverlayRepaintFlags uintptr = MF_STRING
				if immediateOverlayRepaint.Load() {
					immediateOverlayRepaintFlags |= MF_CHECKED
				}
				immediateOverlayRepaintText := "Force immediate repaint of the resize overlay (avoids freezing if dragging at a certain constant rate), if off, it repaints when target window repaints"
				appendMenuChecked(hMenu, immediateOverlayRepaintFlags,
					MENU_TOGGLE_IMMEDIATE_OVERLAY_REPAINT, immediateOverlayRepaintText)

			}

			{
				var missedGestureRecoveryFlags uintptr = MF_STRING
				if missedGestureRecoveryEnabled.Load() {
					missedGestureRecoveryFlags |= MF_CHECKED
				}
				var missedGestureRecoveryText string
				if isAdmin {
					// Ordinary windows can no longer outrank us once elevated (High IL), so
					// this now only matters for the rarer System-integrity foreground
					// windows. Not greyed out: that edge case is uncommon but real, and the
					// IL comparison in winEventProc already makes this a no-op otherwise.
					missedGestureRecoveryText = "Recover winkey+LMB/RMB gestures missed while switching focus from a higher-integrity window (you're elevated, so this now only matters for rarer System-integrity windows)"
				} else {
					missedGestureRecoveryText = "Recover winkey+LMB/RMB gestures missed while switching focus from a higher-integrity window (e.g. Task Manager) (you're not elevated)"
				}
				appendMenuChecked(hMenu, missedGestureRecoveryFlags,
					MENU_TOGGLE_MISSED_GESTURE_RECOVERY, missedGestureRecoveryText)

			}

			{
				var injectButtonUpFlags uintptr = MF_STRING
				if injectButtonUpOnMissedGestureRecovery.Load() {
					injectButtonUpFlags |= MF_CHECKED
				}
				if !missedGestureRecoveryEnabled.Load() {
					injectButtonUpFlags |= MF_DISABLED | MF_GRAYED
				}
				injectButtonUpOnMissedGestureRecoveryText := "(dontuse)On missed-gesture recovery, inject a button-release early (Warning: will click LMB or RMB eg. console-paste unexpectedly)"
				appendMenuChecked(hMenu, injectButtonUpFlags,
					MENU_TOGGLE_INJECT_BUTTON_UP_ON_RECOVERY, injectButtonUpOnMissedGestureRecoveryText)
			}

			{
				var bypassWhenFullscreenFlags uintptr = MF_STRING
				if bypassGesturesWhenFullscreen.Load() {
					bypassWhenFullscreenFlags |= MF_CHECKED
				}
				bypassWhenFullscreenText := "Bypass all gestures when foreground window is fullscreen or borderless-fullscreen (reduces hook overhead while gaming)"

				appendMenuChecked(hMenu, bypassWhenFullscreenFlags,
					MENU_TOGGLE_BYPASS_GESTURES_WHEN_FULLSCREEN, bypassWhenFullscreenText)
			}

			{
				exitText := "Exit"
				appendMenuChecked(hMenu, MF_STRING, MENU_EXIT, exitText)
			}

			// var pt POINT
			// procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

			//doneelsewhereFIXME: doesn't work because prev. focused window was explorer.exe always!
			// // Capture whatever window currently owns the foreground BEFORE we
			// // steal it for ourselves below (required so TrackPopupMenu behaves
			// // like a normal context menu - dismiss on click-away, etc). Without
			// // restoring it afterward, our own invisible hidden window keeps the
			// // real keyboard-input focus indefinitely, even though some other
			// // window (e.g. a dev-build console launched via run.bat) may still
			// // visually show as focused (Windows 11's focus border). Symptom:
			// // clicking Exit and then pressing a key at "Press any key to
			// // exit..." does nothing until you first LMB-click the console.
			// prevForegroundBeforeTrayMenu := getForegroundWindow()
			// restoreForegroundAfterTrayMenu := func() {
			// 	if prevForegroundBeforeTrayMenu == 0 || prevForegroundBeforeTrayMenu == windows.Handle(hwnd) {
			// 		logf("in wndProc, WM_MYSYSTRAY, skipping foreground restore: prev_hwnd:0x%X is 0 or us aka 0x%X", prevForegroundBeforeTrayMenu, hwnd)
			// 		return
			// 	}
			// 	if resIsWin := procIsWindow.Call(uintptr(prevForegroundBeforeTrayMenu)); resIsWin.Failed() {
			// 		logf("in wndProc, WM_MYSYSTRAY, skipping foreground restore: pre-tray-menu HWND=0x%X is no longer a valid window", prevForegroundBeforeTrayMenu)
			// 		return
			// 	}
			// 	if resRestore := procSetForegroundWindow.Call(uintptr(prevForegroundBeforeTrayMenu)); resRestore.Failed() {
			// 		logf("in wndProc, WM_MYSYSTRAY, failed to restore foreground window to pre-tray-menu HWND=0x%X, err=%v callStatus=%v", prevForegroundBeforeTrayMenu, resRestore.Err, resRestore.CallStatus)
			// 	}
			// }

			setForegroundWindow(windows.Handle(hwnd), "WM_MYSYSTRAY: SetForegroundWindow(self) failed")
			// logf("Currently focused window is 0x%X prev:0x%X", hwnd, prevForegroundBeforeTrayMenu)

			res2 := procTrackPopupMenu.Call(
				hMenu,
				TPM_RETURNCMD, //0x0100, // TPM_RETURNCMD
				uintptr(pt.X),
				uintptr(pt.Y),
				0,
				hwnd,
				0,
			)
			if res2.Failed() {
				logf("in wndProc, WM_MYSYSTRAY, failed to TrackPopupMenu, err=%v", res2.Err)
				// restoreForegroundAfterTrayMenu()
				return 0 // Handled
			}
			cmd := res2.R1
			// Required by MSDN to dismiss menu correctly
			_ = procSendMessage.Call(hwnd, WM_NULL, 0, 0) // Send WM_NULL, cannot fail, it's also CheckNone
			// restoreForegroundAfterTrayMenu()

			switch cmd {
			case MENU_ACTIVATE_MOVE:
				focusOnDrag.Store(!focusOnDrag.Load())
			case MENU_USE_LMB_TO_FOCUS_AS_FALLBACK:
				doLMBClick2FocusAsFallback.Store(!doLMBClick2FocusAsFallback.Load())
			case MENU_RATELIMIT_MOVES:
				ratelimitOnMove.Store(!ratelimitOnMove.Load())
				if !ratelimitOnMove.Load() {
					moveCounter.Store(0)
					actualPostCounter.Store(0)
					//nowOffset := time.Now().Sub(appStartTime)
					nowOffset := time.Since(appStartTime)
					lastRateLogTime.Store(int64(nowOffset))
					lastMovePostedTime.Store(int64(nowOffset))
					lastPostedX.Store(-1)
					lastPostedY.Store(-1)
				}
			case MENU_LOG_RATE_OF_MOVES:
				shouldLogDragRate.Store(!shouldLogDragRate.Load())
				// If the user just turned logging ON, flush out old state
				// so the very first log statement starts fresh!
				if shouldLogDragRate.Load() { // When turning ON
					moveCounter.Store(0)
					actualPostCounter.Store(0)

					nowOffset := time.Since(appStartTime)
					lastRateLogTime.Store(int64(nowOffset))
					lastMovePostedTime.Store(int64(nowOffset))

					lastPostedX.Store(-1)
					lastPostedY.Store(-1)
				}

			case MENU_TOGGLE_ASYNC_RESIZE:
				asyncResize.Store(!asyncResize.Load())

			case MENU_TOGGLE_REQUIRE_WINDOWN:
				requireWinDownHeldDuringGesture.Store(!requireWinDownHeldDuringGesture.Load())

			case MENU_TOGGLE_COALESCE_EVENTS:
				coalesceMoveResizeEvents.Store(!coalesceMoveResizeEvents.Load())

			case MENU_TOGGLE_IMMEDIATE_OVERLAY_REPAINT:
				immediateOverlayRepaint.Store(!immediateOverlayRepaint.Load())

			case MENU_TOGGLE_MISSED_GESTURE_RECOVERY:
				missedGestureRecoveryEnabled.Store(!missedGestureRecoveryEnabled.Load())

			case MENU_TOGGLE_INJECT_BUTTON_UP_ON_RECOVERY:
				injectButtonUpOnMissedGestureRecovery.Store(!injectButtonUpOnMissedGestureRecovery.Load())

			case MENU_TOGGLE_BRING_TO_FRONT_ON_DRAG:
				bringToFrontOnDrag.Store(!bringToFrontOnDrag.Load())

			case MENU_TOGGLE_ACTIVATE_RESIZE:
				focusOnResize.Store(!focusOnResize.Load())

			case MENU_TOGGLE_BRING_TO_FRONT_ON_RESIZE:
				bringToFrontOnResize.Store(!bringToFrontOnResize.Load())

			case MENU_TOGGLE_BYPASS_GESTURES_WHEN_FULLSCREEN:
				bypassGesturesWhenFullscreen.Store(!bypassGesturesWhenFullscreen.Load())

			case MENU_TOGGLE_USE_THREADATTACHINPUT_FOR_FOCUS:
				useThreadAttachInputForFocus.Store(!useThreadAttachInputForFocus.Load())

			case MENU_EXIT:
				//procUnhookWindowsHookEx.Call(uintptr(mouseHook))
				exit(0)
			}
		} // fi RMB context menu
		return 0

	case WM_CLOSE:
		//exit(0)
		//WM_CLOSE → DestroyWindow() → WM_DESTROY → PostQuitMessage() -> getmessage() -> break loop -> outside of loop continuation...
		if res := procDestroyWindow.Call(hwnd); res.Failed() {
			logf("in wndProc, WM_CLOSE: DestroyWindow failed for hwnd=0x%X, err: %v", hwnd, res.Err)
		}
		return 0

	case WM_DESTROY:
		_ = procPostQuitMessage.Call(0)
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
	res1111 := procDefWindowProc.Call(hwnd, uintptr(msg), wParam, lParam)
	// if res1111.Failed() {//it's CheckNone and no real failure mode to detect!
	// 	logf("in wndProc, DefWindowProc() failed, err: %v, continuing", res1111.Err)
	// }
	return res1111.R1 //LRESULT
})

const WM_QUIT = 0x0012

// runs only on main() never from any other threads!
func deinit() {
	deinitThreadID := windows.GetCurrentThreadId()
	if mainThreadID != 0 /*ie. is set already*/ && deinitThreadID != mainThreadID {
		badprogramming("BUG: deinit() should only ever run from main/wndProc thread!")
	}
	hardReset(false)

	if timer := memoryVerifyTimer.Load(); timer != nil {
		timer.Stop() // best-effort; harmless no-op if it already fired or was never scheduled
	}

	htidcached := hookThreadID.Load()
	if htidcached != 0 {
		// Send WM_QUIT (0x0012) directly to the hook thread's message queue
		if res := procPostThreadMessage.Call(uintptr(htidcached), WM_QUIT, 0, 0); res.Failed() {
			logf("deinit: PostThreadMessage(WM_QUIT) to hook thread ID=%d failed, err: %v", htidcached, res.Err)
		}
		//itwasdoneFIXME: wait for it to finish deinit-ing ? or to exit thread (currently doesn't exit thread tho) | we're waiting for it in caller of deinit() which is primary_defer()

		if deinitThreadID == htidcached {
			badprogramming("BUG: deinit() should never run from hook thread!")
		}
	}

	cleanupTray()

	//yeah this has to be after NIM_DELETE aka cleanupTray(), according to Gemini 3 Thinking
	deinitMainMsgHwnd()

	deinitOverlayClass()

	//This puts a WM_QUIT message in the queue, which causes GetMessage to return 0 and gracefully break the loop.
	_ = procPostQuitMessage.Call(0)
	/*
		PostThreadMessage(id, WM_QUIT, ...) literally pushes a message into the queue.

		PostQuitMessage(0) doesn't actually "post" a message immediately. It sets a internal "quit" flag in the thread's message queue.
		The next time your GetMessage loop looks for work and finds no other messages, it "synthesizes" a WM_QUIT message on the fly.
	*/
	//however, we used to be singlethreaded and then we were in the same thread that executes that loop so the chances are 0 that we get back to it and more likely that we'll os.Exit
	//but now, hmm... well we're in deinit() of the same thread so it's same thing, heh.
	if winEventHook != 0 {
		logf("cleaned winEventHook from deinit()")
		res1 := procUnhookWinEvent.Call(uintptr(winEventHook))
		// if err9 != nil {
		if res1.Failed() {
			logf("failed UnhookWinEvent, from deinit(), err=%v", res1.Err)
		}
		winEventHook = 0
	}
}

func deinitOverlayClass() {
	if overlayHwnd != 0 {
		// Destroy the overlay window
		if res := procDestroyWindow.Call(uintptr(overlayHwnd)); res.Failed() {
			logf("deinitOverlayClass: DestroyWindow failed for overlayHwnd=0x%X: %v", overlayHwnd, res.Err)
		}
		overlayHwnd = 0
	}

	if magentaBrush != 0 {
		if res := procGdiDeleteObject.Call(uintptr(magentaBrush)); res.Failed() {
			logf("deinitOverlayClass: DeleteObject failed for magentaBrush=0x%X: %v", magentaBrush, res.Err)
		}
		magentaBrush = 0
	}
	if blackBrush != 0 {
		if res := procGdiDeleteObject.Call(uintptr(blackBrush)); res.Failed() {
			logf("deinitOverlayClass: DeleteObject failed for blackBrush=0x%X: %v", blackBrush, res.Err)
		}
		blackBrush = 0
	}

	if overlayClassRegistered.Load() { //deinit it only if it was inited ever
		instance := uintptr(selfHInstance)
		classNamePtr := mustUTF16(winbollocksResizingOverlayClassName)
		if res2 := procUnregisterClassW.Call(uintptr(unsafe.Pointer(classNamePtr)), instance); res2.Failed() {
			logf("deinitOverlayClass: UnregisterClassW failed for overlay class: %v", res2.Err)
		}
	}
}

func deinitMainMsgHwnd() {
	if mainMsgHwnd != 0 {
		res1 := procDestroyWindow.Call(uintptr(mainMsgHwnd))
		// if ret == 0 {
		if res1.Failed() {
			logf("DestroyWindow failed of HWND=0x%X: %v (probably already destroyed or invalid)", mainMsgHwnd, res1.Err)
		}
		mainMsgHwnd = 0
	}

	if hiddenClassRegistered.Load() { //deinit it only if it was inited ever
		instance := uintptr(selfHInstance)
		classNamePtr := mustUTF16(winbollocksHiddenClassName)
		if res3 := procUnregisterClassW.Call(uintptr(unsafe.Pointer(classNamePtr)), instance); res3.Failed() {
			logf("deinitMainMsgHwnd: UnregisterClassW failed for our own hidden class named: %v", res3.Err)
		}
	}
}

// type exitCode int // Custom type so recover knows it's an intentional exit
func exit(code int32) {
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
		uintptr(mainMsgHwnd),
		WM_EXIT_VIA_CTRL_C,
		uintptr(ctrlType),
		0,
	)
	return 1 // 1=true aka i handled this event ie. don't do the default handling which would exit.
})

// slogBridge routes wincoe's internal slog calls into winbollocks' async
// log channel. Without this, wincoe's defensive paths (impossibiru, ClearStdin
// warnings, etc.) write synchronously to os.Stderr, bypassing logWorker entirely
// and risking torn lines or hook stutter under load.
//
// WithAttrs and WithGroup return a fresh slogBridge and intentionally discard the
// accumulated attrs/group chain. wincoe's internal logging is infrequent and
// entirely non-chained, so this simplification carries no practical cost.
type slogBridge struct{}

func (*slogBridge) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (*slogBridge) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String())
	b.WriteString(": ")
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value.Any())
		return true
	})
	logf("[wincoe] %s", b.String())
	return nil
}

func (*slogBridge) WithAttrs(_ []slog.Attr) slog.Handler { return &slogBridge{} }
func (*slogBridge) WithGroup(_ string) slog.Handler      { return &slogBridge{} }

// initWincoeLogging wires wincoe's Logger and bugLogger into winbollocks'
// async log channel. Must be called after logWorker has started (which happens
// in main() before runApplication), so logChan is ready to accept sends.
func initWincoeLogging() {
	/*
		One edge case to be aware of: if something somehow calls through the bridge after closeAndFlushLog() has closed logChan, sending to a closed channel panics.
		In practice this can't happen because wincoe's defensive paths are never reached during teardown, but it's worth knowing the constraint exists.
	*/
	bridge := slog.New(&slogBridge{})
	wincoe.Logger.Store(bridge)
	wincoe.SetBugLogger(bridge)
}

var (
	logFile *os.File
	//hasConsole bool
	canUseConsoleStderr bool // true if os.Stderr is valid/writable and is on console, not on file!
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
	canUseConsoleStderr = false

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
	canUseConsoleStderr = (n != windows.INVALID_FILE_ATTRIBUTES) // basic validity
	// Optional: Test writability
	if canUseConsoleStderr {
		_, writeErr := os.Stderr.WriteString("") // zero-write test
		canUseConsoleStderr = writeErr == nil
	}
}

func initLogFile() {
	if logFile != nil {
		return
	}
	// #nosec: G302 // we want 0644 not 0600 because winbollocks runs as admin usually and want user to can read the log without becoming admin to do so.
	f, err := os.OpenFile(
		selfName+"_debug.log", //"winbollocks_debug.log", //FIXME: keep this in sync with the one in the .bat, or rather make the .bat keep it in sync, somehow.
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
	logFlushChan         = make(chan chan struct{})

	// logQuit is closed exactly once (guarded by logQuitClosed) to ask
	// logWorker to stop waiting on logChan and drain whatever's left. We
	// deliberately never close logChan itself: logf() is called from
	// asynchronous, uncoordinated sources (WinEvent callbacks, the 30s
	// verifyMemoryIsLocked timer scheduled by lockRAM(), hookWorker's own
	// panic-bridge path, etc.) that can outlive closeAndFlushLog(), and a
	// send to a closed channel panics even via select/default. Signaling
	// shutdown through this separate channel means any post-shutdown
	// logf() call harmlessly buffers into (or is counted as dropped from,
	// once full) a channel nobody reads from anymore, instead of crashing
	// the process on exit.
	logQuit       = make(chan struct{})
	logQuitClosed atomic.Bool
)

const attemptAtomicSwapThisManyTimes uint = 100

// formatLogMessage renders a log line the way both logf() and
// directLoggerf() need it: a fixed-format timestamp prefix, the caller's
// formatted message, and a trailing newline. Extracted so the two — and
// logf()'s post-shutdown fallback path below — build the string identically
// and only differ in how they dispatch it (via logChan vs. straight to
// internalLogger).
func formatLogMessage(format string, args ...any) string {
	s := fmt.Sprintf(format, args...)
	now := time.Now().Format("Mon Jan 2 15:04:05.000000000 MST 2006") // these values must be used exactly, they're like specific % placeholders.
	//now := time.Now().Format("2006-01-02T15:04:05.000Z07:00")
	return fmt.Sprintf("[%s] %s\n", now, s)
}

func logf(format string, args ...any) {
	// See the identical up-front check's doc comment in the pre-send branch
	// below; this one covers the (rarer) case where shutdown was already
	// signaled before we even got here.
	finalMsg := formatLogMessage(format, args...)
	if logQuitClosed.Load() {
		internalLogger(finalMsg)
		return
	}

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
	sent := false
	select {
	case logChan <- finalMsg:
		// Message sent to the background worker
		sent = true
	default:
		// If the buffer is full, we drop the log so we don't lag the mouse
		droppedLogEvents.Add(1)
	}

	// Re-check logQuitClosed AFTER attempting the send. The initial check
	// above and this send are not atomic with respect to closeAndFlushLog()
	// signaling shutdown, so there's a narrow window where: (a) we read
	// logQuitClosed as false, (b) closeAndFlushLog() runs and logWorker
	// finishes its final drain (see drainRemainingLogChanMessages) and
	// exits, then (c) our send above lands in logChan anyway — into a
	// channel nobody will ever read from again. Re-checking here catches
	// that window: if shutdown is now signaled, we ALSO emit directly via
	// internalLogger, deliberately accepting a possible duplicate printed
	// line (if logWorker's drain actually raced past this message rather
	// than truly finishing before it arrived) in exchange for never
	// silently losing a message during shutdown. A duplicate line is a
	// trivial cosmetic cost; a lost log line during shutdown/crash
	// diagnostics is not. This check is lock-free — just an atomic load,
	// no contention with the hot mouse/keyboard-hook logf() callers.
	if sent && logQuitClosed.Load() {
		internalLogger(finalMsg)
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

	res1 := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)
	if res1.Failed() || res1.R1 != uintptr(len(inputs)) {
		logf("SendInput failed to inject %d events, injected=%d == ret=%d err=%v", len(inputs), res1.R1, res1.R1, res1.Err)
	}
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
func keyboardProc(nCode int, wParam, lParam uintptr) uintptr {
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
		res1 := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		return res1.R1
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
		res2 := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		return res2.R1
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
			if winGestureUsed.Load() {
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
				/*
						PostMessage is asynchronous.

					Semantics:

					• The message is placed into the target thread’s message queue.
					• The function returns immediately.
					• No reentrancy, no waiting for processing.
					• If the queue is full or the window is gone, the post can fail, but it does not block.
					chatgpt5.2
				*/
				if res := procPostMessage.Call(
					uintptr(mainMsgHwnd),
					WM_INJECT_SEQUENCE,
					uintptr(vk), // VK_LWIN or VK_RWIN,
					0,
				); res.Failed() {
					// Can't recover from inside this low-level keyboard hook; the shift-tap
					// injection that suppresses Start menu simply won't happen this time.
					logf("keyboardProc: PostMessage WM_INJECT_SEQUENCE at end of gesture, failed, err: %v; Start menu may(unlikely tho) pop up, but probably won't because the vkE8 was injected when gesture started", res.Err)
				}

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

	res1111 := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
	return res1111.R1
}

func assertStructSizes() {
	// These are the Win32 ABI sizes for the INPUT union on 64-bit Windows (amd64).
	// The build tag enforces we only reach this code on that architecture.
	// If the Go struct layout ever drifts from what Win32 expects (e.g. due to
	// a struct field reorder), SendInput will silently send garbage — panic early.
	const (
		expectedINPUT      uintptr = 40 // sizeof(INPUT) on x64: 4 type + 4 pad + 32 union
		expectedKEYBDINPUT uintptr = 24 // sizeof(KEYBDINPUT) on x64: with 8-byte DwExtraInfo
	)

	if got := unsafe.Sizeof(INPUT{}); got != expectedINPUT {
		badprogramming(fmt.Sprintf(
			"INPUT ABI size mismatch: Go struct is %d bytes, Win32 x64 expects %d — SendInput will be broken",
			got, expectedINPUT,
		))
	}
	if got := unsafe.Sizeof(KEYBDINPUT{}); got != expectedKEYBDINPUT {
		badprogramming(fmt.Sprintf(
			"KEYBDINPUT ABI size mismatch: Go struct is %d bytes, Win32 x64 expects %d",
			got, expectedKEYBDINPUT,
		))
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
	isAdmin = token.IsElevated() // must init this before setting missedGestureRecoveryEnabled which depends on it, so either put it in same init() and do it first(like now), or put its init() before the init() of missedGestureRecoveryEnabled

	//defaults:
	const bringToFrontByDefaultOnGesture = true
	focusOnDrag.Store(bringToFrontByDefaultOnGesture)        // focus window when gesture applies on it, ie. on drag-Move (but TODO: ? this doesn't apply to on-resize hmm)
	bringToFrontOnDrag.Store(bringToFrontByDefaultOnGesture) // default to same as above ^ TODO: should I make this apply to resize as well?
	focusOnResize.Store(bringToFrontByDefaultOnGesture)
	bringToFrontOnResize.Store(bringToFrontByDefaultOnGesture)

	useThreadAttachInputForFocus.Store(false) // default false because for some reason it doesn't seem needed right now 17 July 2026, for me, tho I coulda sworn it was, before!

	//XXX: needed for cmd.exe running as Admin(because thread-attaching focus method fails!), not needed for task manager (thread-attaching method works!)
	//also needed for focusing a target window while start menu is open already, because thread-attaching focus method fails.
	doLMBClick2FocusAsFallback.Store(true)

	ratelimitOnMove.Store(false)
	shouldLogDragRate.Store(false)
	asyncResize.Store(false)                      // default to sync
	requireWinDownHeldDuringGesture.Store((true)) // default to true
	coalesceMoveResizeEvents.Store(true)          //default to true
	immediateOverlayRepaint.Store(false)          // default to false
	foregroundWasHigherIntegrity.Store(false)     // no known-blocked foreground yet
	checkForMissedGestureOnNextMove.Store(false)  // nothing to recover yet

	//"But there genuinely are windows that can outrank even an elevated High-IL process: anything running at System integrity (0x4000) — some SYSTEM-owned services with UI, certain security-related dialogs, etc. It's rare, but real. " -Claude
	missedGestureRecoveryEnabled.Store(!isAdmin) // default on if not admin

	injectButtonUpOnMissedGestureRecovery.Store(false) // default off, see doc comment on the var

	bypassGesturesWhenFullscreen.Store(false) // default off; opt-in

	lastPostedX.Store(-1)
	lastPostedY.Store(-1)
	nowOffset := time.Since(appStartTime)
	//FIXME: these 2 need to be set when startDragging(see 'capturing' bool) happens(ie. state changed from not dragging to dragging, so 1 time not on every drag/move event!), every time! so not here!
	lastRateLogTime.Store(int64(nowOffset))
	lastMovePostedTime.Store(int64(nowOffset))
}

var selfPID uint32

func init() {
	// #nosec G115 -- safe: Windows PIDs are DWORDs and fit perfectly in uint32
	selfPID = uint32(os.Getpid())
	if selfPID == 0 {
		badprogramming("shouldn't happen that own pid is 0")
	}
	anotherWay := windows.GetCurrentProcessId()
	if selfPID != anotherWay {
		badprogramming(fmt.Sprintf("own pid is reported differently by the 2 different ways: %d vs %d", selfPID, anotherWay))
	}
}

var selfIntegrityLevel uint32

func init() {
	if selfPID == 0 {
		badprogramming("shouldn't happen that own pid is 0, unless init() is currently in a different order than initially programmed")
	}
	// In your main or init, cache your own IL
	il, err := processIntegrityLevel(selfPID)
	if err != nil {
		//myIntegrityLevel = 0x2000 // Default to Medium if check fails
		badprogramming(fmt.Sprintf("can't get own integrity level! err=%v", err)) // and don't wanna default to anything
	} else {
		selfIntegrityLevel = il
	}
}

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
		defer func() { mutexHandle = 0 }() //executes third

		defer func() { //executes second
			//If procReleaseMutex.Call somehow panics (unlikely, but possible with corrupted memory), this is in a defer

			// Close handle so other instances can acquire
			//procCloseHandle.Call(mutexHandle)
			res2 := procCloseHandle.Call(mutexHandle)
			// if r2 == 0 {
			if res2.Failed() {
				logf("CloseHandle failed: %v", res2.Err)
			}
		}()

		//executes first
		// Release ownership if we own it
		//procReleaseMutex.Call(mutexHandle)
		res1 := procReleaseMutex.Call(mutexHandle)
		// if r1 == 0 {
		if res1.Failed() {
			logf("ReleaseMutex failed: %v", res1.Err)
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
	logf("mutex name = %q", str)
	namePtr, err0 := windows.UTF16PtrFromString(str)
	//namePtr, err0 := windows.UTF16PtrFromString("Global\\" + name)
	if err0 != nil {
		exitf(3, "UTF16PtrFromString (in ensureSingleInstance) for str '%s' failed: %v", str, err0)
	}

	// CreateMutex(lpMutexAttributes, bInitialOwner, lpName)
	// CreateMutex: Security attributes NULL (0), Initial owner TRUE (1), Name
	res1 := procCreateMutex.Call(0, 1, uintptr(unsafe.Pointer(namePtr)))

	// // Normalize to an error we can use with errors.Is.
	// var err error
	// if callErr != nil && !errors.Is(callErr, windows.Errno(0)) {
	// 	err = callErr
	// } else if last := windows.GetLastError(); last != nil && !errors.Is(last, windows.Errno(0)) {
	// 	err = last
	// }

	// if err != nil {
	// 	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
	// 		exitf(5, "Application '%s' is already running.", name)
	// 	}
	// 	// other error handling if needed:
	// 	// exitf(1, "CreateMutex failed: %v", err)
	// }
	if res1.Failed() { // aka If handle is 0, we didn't even create it (likely Access Denied for Global\)
		var extra string = ""
		if res1.ErrIs(windows.ERROR_ACCESS_DENIED) {
			extra = " this means mutex attempt was 'Global\\' and it was already acquired by an admin-running exe"
		}
		//exitf(5, "Application '%s' failed to create mutex %s", name, str)
		exitf(2, "CreateMutex failed entirely: '%v' %s", res1.Err, extra)
	}
	if res1.CallStatusIs(windows.ERROR_ALREADY_EXISTS) {
		exitf(5, "Application '%s' is already running.", name)
	}

	// // If handle is 0, we didn't even create it (likely Access Denied for Global\)
	// if ret == 0 {
	// 	var extra string = ""
	// 	if errors.Is(callErr, windows.Errno(5)) { // aka 'Access Denied'==5
	// 		extra = " this means mutex attempt was 'Global\\' and it was already acquired by an admin-running exe"
	// 	}
	// 	exitf(2, "CreateMutex failed entirely: '%v' (code: %d)%s", err, err, extra)
	// }

	// Note: We don't technically need to close this handle manually.
	// As long as the process is alive, the mutex is held.
	// When the process dies, Windows cleans it up.
	//_ = ret
	mutexHandle = res1.R1 // aka ret
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
	const modVal = 50 //must be more than 1, else infinite loop below
loggingLoop:
	for {
		select {
		case msg := <-logChan:
			counter++
			internalLogger(msg) // good call here
			if counter%modVal == 0 {
				verifyMemoryIsLocked() // can logf itself! so modVal must be > than how many msgs it can log worst case(currently 1) else i will infinite loop here.
			}
			if counter > MaxBeforeReset {
				counter = 0
			}
		case ack := <-logFlushChan:
			// Drain all currently queued messages
			for len(logChan) > 0 {
				msg := <-logChan
				counter++
				if counter > MaxBeforeReset {
					counter = 0
				}
				internalLogger(msg)
			}
			close(ack) // Signal back to FlushLogs() that we are done
		case <-logQuit:
			// Shutdown requested. Drain whatever's already buffered in
			// logChan (non-blockingly) so we don't lose messages enqueued
			// right before shutdown, then fall through to report final
			// stats and exit. Deliberately skip the counter/modVal/
			// verifyMemoryIsLocked bookkeeping here — verifyMemoryIsLocked
			// itself calls logf(), and anything it enqueued at this point
			// would never be read (nothing will call logWorker again), so
			// doing that work now would be pure waste.
			drainRemainingLogChanMessages()
			break loggingLoop
		}
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
			withCommas(maxMoveEvents), withCommas(droppedMoveOrResizeEvents.Load()), forceMoveOrResizeActionsToBeThisManyMSApart)
		//logf("for testing when a panic in logWorker happens after main's keypress, right before main's os.Exit!")
	}
} //logWorker

// drainRemainingLogChanMessages performs a final, non-blocking sweep of
// logChan during logWorker shutdown, flushing any messages that were
// enqueued right before closeAndFlushLog() signaled logQuit. logChan is
// never closed (see logQuit's doc comment), so this relies on the channel
// being momentarily empty rather than on a close+range-drain pattern.
func drainRemainingLogChanMessages() {
	for {
		select {
		case msg := <-logChan:
			internalLogger(msg)
		default:
			return
		}
	}
}

func directLoggerf(format string, args ...any) {
	// s := fmt.Sprintf(format, args...)
	// now := time.Now().Format("Mon Jan 2 15:04:05.000000000 MST 2006") // these values must be used exactly, they're like specific % placeholders.
	// //now := time.Now().Format("2006-01-02T15:04:05.000Z07:00")
	// finalMsg := fmt.Sprintf("[%s] %s\n", now, s)
	internalLogger(formatLogMessage(format, args...)) // good call here
}

// never call this directly, instead call directLoggerf()
func internalLogger(finalMsg string) {
	//detectConsole()
	if canUseConsoleStderr {
		// --- START TIMING ---
		startPrint := time.Now()
		//fmt.Fprintf(os.Stderr, "[%s] %s\n", timestamp, s)
		fmt.Fprintf(os.Stderr, "%s", finalMsg)
		duration := time.Since(startPrint)
		// --- END TIMING ---
		// Only alert us if the print took longer than a "frame" (16ms)
		if duration > 16*time.Millisecond { //TODO: make it a const
			// Note: Printing this might trigger another lag, but it's for science!
			// XXX: used to happen when running as admin and u LMB drag the scroll bar or LMB on the text area which begins selection and auto selects 1 char already! when logging was happening on same thread as hooks and msg.loop.
			fmt.Fprintf(os.Stderr, "!!! LOG LAG DETECTED: %v !!!\n", duration) //this won't be seen when compiled without console ie. 'go build -ldflags "-H=windowsgui"'
		}
		return
	}

	if logFile == nil {
		initLogFile()
		if logFile == nil {
			return
		}
	}

	_, err := fmt.Fprintf(logFile, "%s", finalMsg)
	if err != nil && canUseConsoleStderr {
		fmt.Fprintf(os.Stderr, "!!! Err:'%v', Couldn't write to logFile %q the logline: %s", err, logFile.Name(), finalMsg)
	}
	// --- START SYNC TIMING ---
	syncStart := time.Now()
	err2 := logFile.Sync()
	syncDur := time.Since(syncStart)
	// --- END SYNC TIMING ---
	if err2 != nil && canUseConsoleStderr {
		fmt.Fprintf(os.Stderr, "!!! Err:'%v', Couldn't sync logFile %q after writing to it this logline: %s", err2, logFile.Name(), finalMsg)
	}
	// Check if the sync took an unusually long time
	const slowSyncThreshold = 1 * time.Second
	if syncDur > slowSyncThreshold {
		warnMsg := formatLogMessage("LOG SYNC LAG DETECTED: fsync took %v (threshold: %v)", syncDur, slowSyncThreshold)

		// Print to stderr if available
		if canUseConsoleStderr {
			fmt.Fprintf(os.Stderr, "!!! %s", warnMsg)
		}

		// Write to the log file WITHOUT calling Sync() again
		_, err3 := fmt.Fprintf(logFile, "%s", warnMsg)
		if err3 != nil {
			fmt.Fprintf(os.Stderr, "!!! failed to write to logFile %q too (err:%v) the warn msg: %q", logFile.Name(), err3, warnMsg)
		}
	}
}

func closeAndFlushLog() {
	// 1. Signal the worker "no more logs are coming", exactly once. We
	// close logQuit rather than logChan — see logQuit's doc comment for
	// why closing logChan directly is unsafe here. The CompareAndSwap
	// guards against closeAndFlushLog() being invoked more than once (e.g.
	// primary_defer() plus, on a genuine secondary panic, secondary_defer()
	// or hookWorkerSecondaryDefer()) or concurrently from different threads.
	if logQuitClosed.CompareAndSwap(false, true) {
		close(logQuit)
	}
	// 2. Wait for the worker to finish draining and printing the backlog.
	// XXX: This blocks until close(logWorkerDone) happens in the worker.
	// Safe for multiple callers to receive from a closed channel.
	<-logWorkerDone
}

type theILockedMainThreadToken struct{}

// The Problem: currentExitCode is currently an int. If the hookWorker thread crashes, it catches the panic, modifies currentExitCode, and tells the Main thread to die. This is a classic data race if the main thread happens to be in a defer simultaneously.
// the standard library package sync/atomic does not offer an atomic.Int type. Instead, it forces you to be explicit about the memory width—providing atomic.Int32 and atomic.Int64.
// Since your exit code maps directly to OS processes and Windows APIs (where exit codes are standard 32-bit integers), using atomic.Int32 ensures explicit compatibility across any CPU architecture you compile for.
var currentExitCode atomic.Int32 // = 0

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
	logf("!secondary defer here! Primary defer wanted to exit with exitcode: '%d' but we do: '%d'", currentExitCode.Load(), exitcode)
	closeAndFlushLog()
	os.Exit(exitcode) // XXX: oughtta be the only os.Exit! well 2of3
}

// a placeholder for graceful exit
// runs only on main() never in any other threads!
func primary_defer() { //primary defer
	// SIGNAL THE WATCHDOG:
	// Closing this channel releases the hookWorker from its 2s timer.
	select {
	case <-mainAcknowledgedShutdown: // Check if closed
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
			currentExitCode.Store(status.Code)
			// This was an intentional exit(code)
			//if code != 0 {
			logf("Program intentionally exited with code: '%d' and error message: '%s'", currentExitCode.Load(), status.Message)
			//}
		} else {
			currentExitCode.Store(1)
			stack := debug.Stack()
			logf("--- CRASH: %v ---\nStack: %s\n--- END---", r, stack)
			//debug.PrintStack()
		}
	}

	deinit()

	logf("Execution finished (from main())")
	if writeProfile {
		writeHeapProfileOnExit()
	}
	// 2. Use your high-quality "clrbuf" waiter
	// Only pause if we have an actual console window and an error occurred

	// // 2. Check if Stdin is actually a terminal (not a pipe/null)
	if wincoe.IsStdinConsoleInteractive() {
		releaseSingleInstance() // don't hog the mutex while waiting for key, else program exit cleans it.

		if startupTerminalHwnd != 0 {
			//this isn't reached if compiled with 'go build -ldflags="-H=windowsgui" ' because it's 0
			//using direct instead of logf to avoid the intermixing of this msg and the "Press any key" one!
			logf("Explicitly forcing focus back to startup terminal(so keyboard input is sensed here) HWND: 0x%X", startupTerminalHwnd)
			// Use your existing thread-attaching focus method to bypass UIPI/Focus Stealing Prevention
			forceForeground(startupTerminalHwnd)
		}

		// focusedNow := getForegroundWindow()
		// logf("Currently focused window is 0x%X", focusedNow) //before the above fix, this is 0x0 (tho no error happened), or it's explorer.exe's window if restoreForegroundAfterTrayMenu() was allowed to run.

		//doneTODO: sync the logf here but don't kill/close it! to ensure no queued messages get printed while "Press any key" message is shown, else they get appended to the line because it lacks a "\n" on purpose!
		// Flush all pending logs before printing the prompt!
		FlushLogs()
		wincoe.WaitAnyKey() // Press any key
	} else {
		logf("Didn't wait for keypress due to not an interactive/terminal.")
	}

	// Wait for hookWorker's own clean-exit signal before flushing logs and
	// exiting. deinit() already asked hookThreadID to WM_QUIT; this waits
	// for that thread to actually get there, run its UnhookWindowsHookEx
	// defers, and finish, so we don't tear down logging out from under it.
	// Skipped if hookWorker was never started. Bounded by a timeout since a
	// panicking hook thread never closes this channel (see its panic path
	// above) — that's an already-handled, expected case, not a bug.
	if hookThreadID.Load() != 0 {
		const hookWorkerExitTimeout = 2 * time.Second
		select {
		case <-hookWorkerDone: // Check if closed
			logf("main here, hookWorker signaled clean exit; proceeding.")
		case <-time.After(hookWorkerExitTimeout):
			logf("main here, timed out waiting for hookWorker's clean-exit signal (%v); proceeding anyway.", hookWorkerExitTimeout)
		}
	}

	//XXX: these should be last:
	closeAndFlushLog()
	// 3. exit
	os.Exit(int(currentExitCode.Load())) // XXX: oughtta be the only os.Exit! well 1of3
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

	ensureSingleInstance(selfName+"_uniqueID_123lol" /*winbollocks_uniqueID_123lol*/, MutexScopeSession)

	cpus := int64(runtime.NumCPU())
	if cpus < 0 {
		exitf(1, "negative number of CPUs returned %s", withCommasSigned(cpus))
	}
	//(Passing 0 to GOMAXPROCS just returns the current setting without changing it.)
	logf("You've %s physical CPUs, GOMAXPROCS is set to: %d ", withCommas(uint64(cpus)), runtime.GOMAXPROCS(0))

	// 3. Your logic (Task 1: don't use log.Fatal inside here!)
	if err := runApplication(token); err != nil {
		exitf(2, "Error: %v\n", err)
	}
	logf("Went past runApplication, now at  main()'s end.")
} //main

func getConsoleWindow() (windows.HWND, error) {
	res1 := procGetConsoleWindow.Call()

	hwnd := windows.HWND(res1.R1)

	if hwnd == 0 {
		// syscall wrappers often return err == "The operation completed successfully."
		// when no failure occurred, so treat that as nil.
		// if err != nil && err != windows.ERROR_SUCCESS {
		// if res1.Failed() {//it's CheckNone, so useless to check here!
		// 	return 0, fmt.Errorf("in getConsoleWindow, GetConsoleWindow() failed, err=%w", res1.Err)
		// }

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
	if res := procSetConsoleCtrlHandler.Call(ctrlHandler, 1); res.Failed() { // this doesn't work(ie. has no console) for: go build -mod=vendor -ldflags="-H=windowsgui" .
		logf("installCtrlHandlerIfConsole: SetConsoleCtrlHandler failed to install handler, err: %v; Ctrl+C/Break won't be intercepted this run.", res.Err)
	}
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
	Code    int32
	Message string
}

// exitf allows you to provide a code and a formatted message
func exitf(code int32, format string, a ...interface{}) {
	//deinit()
	//this panic will run the primary and potentially secondary(if primary fails) deferrers! ie. primary_defer
	panic(exitStatus{
		Code:    code,
		Message: fmt.Sprintf(format, a...),
	})
}

// XXX: in here, return errors like 'return fmt.Errorf("something went wrong")' instead of using log.Fatal or os.Exit(1)
// however exitf and panics are fine because they're defer-caught properly and thus graceful exit still happens!
func runApplication(_token theILockedMainThreadToken) error { //XXX: must be called on main() and after that runtime.LockOSThread()
	_ = _token // silence warning!
	assertStructSizes()
	initWincoeLogging() // ← must be before any wincoe calls

	// Capture the actual terminal/console window that launched us
	resFg := procGetForegroundWindow.Call()
	startupTerminalHwnd = windows.Handle(resFg.R1)

	logf("Started %s %s", selfName, GetVersion())
	initDarkMode() // ← Tell Windows to enable modern theme support for menus

	if writeProfile {
		// In main(), before the GetMessage loop:
		f, err1 := os.Create("cpu.prof")
		if err1 != nil {
			logf("Failed to create CPU profile: %v", err1)
			// or exitf if critical
		} else {
			if err2 := pprof.StartCPUProfile(f); err2 != nil {
				logf("StartCPUProfile failed: %v", err2)
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

	mainThreadID = windows.GetCurrentThreadId() //XXX: it's set before 'go hookWorker()' below
	logf("main loop thread started. ThreadID: %d", mainThreadID)

	hwnd, err3 := createMessageWindow() //TODO: how to undo this via defer or something?!
	if err3 != nil {
		//exitf(1, "Failed to create message window: %v", err)
		return fmt.Errorf("failed to create message window: %w", err3)
	}
	mainMsgHwnd = hwnd

	if err4 := initTray(); err4 != nil {
		return fmt.Errorf("failed to init tray: %w", err4)
	}

	if res := procWTSRegisterSessionNotification.Call(uintptr(mainMsgHwnd), NOTIFY_FOR_THIS_SESSION); res.Failed() {
		logf("WTSRegisterSessionNotification failed, err: %v; lock/unlock-triggered stale-session cleanup (see WM_WTSSESSION_CHANGE in wndProc) will be unavailable this run.", res.Err)
	} else {
		defer func() {
			if res2 := procWTSUnRegisterSessionNotification.Call(uintptr(mainMsgHwnd)); res2.Failed() {
				logf("WTSUnRegisterSessionNotification failed, err: %v", res2.Err)
			}
		}()
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
	if res1 := procSetWinEventHook.Call(
		uintptr(EVENT_SYSTEM_FOREGROUND), //0x0003, // EVENT_SYSTEM_FOREGROUND min
		//0x0003, // max
		uintptr(EVENT_OBJECT_FOCUS), // max; spans 0x4xxx console band too //0x8005, // EVENT_OBJECT_FOCUS (Catch lower-level focus shifts)

		0, // hmod = 0 (out-of-context callback)
		winEventCallback,
		0, // idProcess = 0 (all)
		0, // idThread = 0 (all)
		uintptr(WINEVENT_OUTOFCONTEXT|WINEVENT_SKIPOWNPROCESS), //0x0000|0x0002, // WINEVENT_OUTOFCONTEXT | WINEVENT_SKIPOWNPROCESS
	); res1.Failed() { //err != nil || h == 0 {
		logf("SetWinEventHook failed, hooking of winEventHook, from main thread: %v", res1.Err)
	} else {
		winEventHook = windows.Handle(res1.R1)
		defer func() {
			res2 := procUnhookWinEvent.Call(uintptr(winEventHook))
			// if err2 != nil {
			if res2.Failed() {
				logf("UnhookWinEvent failed unhooking of winEventHook, from main thread, err: %v", res2.Err)
			}
			winEventHook = 0
			logf("normal unhooking of winEventHook, from main thread")
		}()

		initForegroundIntegrityState() //"This runs synchronously, single-threaded, before the message loop starts pumping — so there's no race with winEventProc itself (it literally can't fire yet)." - Claude
	}

	if err5 := initOverlay(); err5 != nil {
		return fmt.Errorf("failed to initOverlay which is what's displayed when resizing, err: %w", err5)
	}

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
		res3 := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		// if int32(r) <= 0 {
		if res3.Failed() /*aka res3.Err < 0*/ || res3.R1 == 0 /*aka WM_QUIT*/ {
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

	res1 := procQueryWorkingSetEx.Call(
		hProc,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)

	//if ret == 0 {
	if res1.Failed() {
		logf("in verifyMemoryIsLocked, failed QueryWorkingSetEx, err: %v", res1.Err)
		return
	}

	if !info.VirtualAttributes.IsValid() {
		//		logf("Verification: Memory at 0x%X is currently resident in RAM.", info.VirtualAddress)
		//} else {
		logf("Verification: Memory at 0x%X is currently PAGED OUT. This is unexpected!", info.VirtualAddress)
	}
}

const (
	TOKEN_ADJUST_PRIVILEGES = 0x0020
	TOKEN_QUERY             = 0x0008
	SE_PRIVILEGE_ENABLED    = 0x00000002
	SE_INC_WORKING_SET_NAME = "SeIncreaseWorkingSetPrivilege" // not: "SeIncrementWorkingSetPrivilege"
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

// memoryVerifyTimer holds the *time.Timer scheduled by lockRAM() for its
// delayed post-trim verification check, so deinit() can Stop() it during
// shutdown instead of leaving it dangling. Stored via atomic.Pointer for
// defense-in-depth consistency with this file's other globals; in practice
// lockRAM() (called once from runApplication) and deinit() (called once
// from primary_defer(), always the main thread) don't race on it.
var memoryVerifyTimer atomic.Pointer[time.Timer]

func lockRAM() {
	//Warning for Defensive Coding: SetProcessWorkingSetSize can fail if the values you provide are too high or if the user doesn't have the
	// SE_INC_WORKING_SET_NAME privilege (though for small amounts like 10–50MB, Windows usually grants it to "High" priority processes without drama).
	//hProc, _, _ := procGetCurrentProcess.Call()
	hProc := getCurrentProcess()

	//To successfully increase your working set, you often need the SE_INC_WORKING_SET_NAME privilege. Simply calling the API might fail silently or return "Access Denied."
	// 1. Enable the Privilege
	var token uintptr
	res1 := procOpenProcessToken.Call(hProc, TOKEN_ADJUST_PRIVILEGES|TOKEN_QUERY, uintptr(unsafe.Pointer(&token)))
	//if err == nil || ret != 0 {
	if res1.Succeeded() {
		var luid LUID
		lpName, err4 := windows.UTF16PtrFromString(SE_INC_WORKING_SET_NAME)
		if err4 != nil {
			logf("failed UTF16PtrFromString on %q, err='%v', continuing tho.", SE_INC_WORKING_SET_NAME, err4)
		} else {
			res2 := procLookupPrivilegeValue.Call(0, uintptr(unsafe.Pointer(lpName)), uintptr(unsafe.Pointer(&luid)))
			if res2.Failed() {
				logf("failed procLookupPrivilegeValue %q, err: '%v', continuing tho.", SE_INC_WORKING_SET_NAME, res2.Err)
			} else {
				//if err2 == nil || ret2 != 0 {
				//if res2.Succeeded() {
				tp := TOKEN_PRIVILEGES{
					PrivilegeCount: 1,
					Privileges: [1]LUID_AND_ATTRIBUTES{
						{Luid: luid, Attributes: SE_PRIVILEGE_ENABLED},
					},
				}
				// AdjustTokenPrivileges returns success even if it partially fails,
				// so we must check GetLastError (err) specifically.
				res3 := procAdjustTokenPrivileges.Call(token, 0, uintptr(unsafe.Pointer(&tp)), 0, 0, 0)
				//if err3 != nil || ret3 == 0 || !errors.Is(err3, windows.Errno(0)) {
				if res3.Failed() {
					logf("Warning: Could not enable %q, err: '%v', callStatus: '%v', ret: '%d', continuing tho.",
						SE_INC_WORKING_SET_NAME, res3.Err, res3.CallStatus, res3.R1)
				}
				//}
			}
		}
		err5 := windows.CloseHandle(windows.Handle(token))
		if err5 != nil {
			logf("CloseHandle(token) failed, err='%v', continuing tho.", err5)
		}
	} else {
		logf("OpenProcessToken failed, err: '%v', callStatus: '%v'; skipping SeIncreaseWorkingSetPrivilege enablement, continuing tho.", res1.Err, res1.CallStatus)
	}

	// 2. Set the Working Set Size
	// We'll request 20MB min and 50MB max.

	// We request that 20MB to 50MB stay in RAM at all times.
	// This effectively "VirtualLocks" the core of your app.
	var min2 uint64 = 20 * 1024 * 1024
	var max2 uint64 = 50 * 1024 * 1024

	res4 := procSetProcessWorkingSetSize.Call(hProc, uintptr(min2), uintptr(max2))
	//if ret4 == 0 {
	if res4.Failed() {
		logf("Failed SetProcessWorkingSetSize to min:%s and max:%s, err: '%v', continuing tho.", humanBytes(min2), humanBytes(max2), res4.Err)
	} else {
		logf("Working Set locked between %s and %s", humanBytes(min2), humanBytes(max2))
	}

	verifyMemoryIsLocked() //kinda useless to do now

	// 2. Schedule the "Heisenberg-proof" check
	// We wait 30 seconds to let Windows try to 'trim' our RAM.
	timer := time.AfterFunc(30*time.Second, func() {
		verifyMemoryIsLocked()
	})
	// Stored so deinit() can Stop() it. Without this, a short-lived run (or
	// one where wincoe.WaitAnyKey() in a devbuild console takes a while)
	// leaves this goroutine dangling until it either fires — harmless
	// post-fix-#1, but still pointless work mid-shutdown — or the process
	// exits and takes it down anyway.
	memoryVerifyTimer.Store(timer)
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

// CURRENT_PROCESS_PSEUDO_HANDLE is what GetCurrentProcess returns a valid pseudo-handle which happens to be -1.
// In Go, ^uintptr(0) (all bits set) is the numeric representation of -1
const CURRENT_PROCESS_PSEUDO_HANDLE = ^uintptr(0) // All bits set to 1

// CURRENT_THREAD_PSEUDO_HANDLE is what GetCurrentThread returns, a valid pseudo-handle, in uintptr fashion (64-bit), -2 is: 0xFFFFFFFFFFFFFFFE aka ^uintptr(1)
const CURRENT_THREAD_PSEUDO_HANDLE uintptr = ^uintptr(1)

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
	res1 := procGetCurrentProcess.Call()
	hProcLocal := res1.R1
	// procGetCurrentProcess is bound with wincoe.CheckEquals(CURRENT_PROCESS_PSEUDO_HANDLE),
	// so .Failed() here means the OS returned something other than the one
	// value GetCurrentProcess is contractually guaranteed to return.
	if res1.Failed() {
		// This virtually never happens, but if it did,
		// the system is in a very weird state.
		exitf(1, "Critical: GetCurrentProcess returned 0x%X, err: %v, callStatus: %v", hProcLocal, res1.Err, res1.CallStatus)
	}
	return hProcLocal
}

func getCurrentThread() (hThread uintptr) {
	//Note that GetCurrentThread also returns a pseudo-handle (usually -2), so it doesn't need to be closed either.
	res1 := procGetCurrentThread.Call()
	currThread := res1.R1
	// See the identical comment in getCurrentProcess() above; procGetCurrentThread
	// is bound with wincoe.CheckEquals(CURRENT_THREAD_PSEUDO_HANDLE).
	if res1.Failed() {
		exitf(1, "Critical: getCurrentThread returned 0x%X, err: %v, callStatus: %v", currThread, res1.Err, res1.CallStatus)
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
	res1 := procSetPriorityClass.Call(hProc, wantedProcessPrio)
	//if ntStatus == 0 {
	if res1.Failed() {
		logf("Failed to set process priority class to 0x%x, err:%v", wantedProcessPrio, res1.Err)
		//return
	}

	// Verify it actually changed
	res2 := procGetPriorityClass.Call(hProc)
	if res2.Failed() {
		logf("Failed to get process priority, err:%v", res2.Err)
	}
	prio := res2.R1
	if prio == HIGH_PRIORITY_CLASS {
		logf("Process priority confirmed: 0x%x where 0x%x is Normal.", wantedProcessPrio, NORMAL_PRIORITY_CLASS)
	} else {
		logf("Priority mismatch! OS returned prio: 0x%x instead of 0x%x and err was: %v, callStatus: %v", prio, wantedProcessPrio, res2.Err, res2.CallStatus)
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
	res3 := procSetThreadPriority.Call(currThread, uintptr(wantedThreadPrio))
	//if tRet == 0 {
	if res3.Failed() {
		logf("Failed to set thread priority, err: %v", res3.Err)
	} else {
		// Verify Thread Priority
		res4 := procGetThreadPriority.Call(currThread)
		if res4.Failed() {
			logf("Failed to get thread priority, err:%v", res4.Err)
		}
		// #nosec G115 -- safe: Win32 thread priorities are small integers that fit in int32
		tprio := int32(res4.R1)

		// GetThreadPriority returns an int. 15 is TIME_CRITICAL.
		if tprio == wantedThreadPrio {
			logf("Thread Priority confirmed: %d", tprio)
		} else {
			logf("Thread Priority mismatch! OS returned prio: %d instead of %d and err was: %v, callStatus: %v", tprio, wantedThreadPrio, res4.Err, res4.CallStatus)
		}
	}

	//FIXME: so since memprio and i/o prio below aren't set to anything different than normal, maybe don't try to set them at all ie. remove the code doing it!

	// --- Memory Priority (Using Kernel32) ---
	// this is so we don't get paged out to swap/pagefile
	var wantedMemPrio uint32 = 5 // 6 is Very High(doesn't work, it fails w/ invalid param!), 5 is the value i saw in process explorer if nothing's setting it at all.

	wantedType := PROCESS_MEMORY_PRIORITY
	memPrio := MEMORY_PRIORITY_INFORMATION{MemoryPriority: wantedMemPrio}

	res5 := procSetProcessInformation.Call(
		hProc,
		uintptr(wantedType), // 0
		uintptr(unsafe.Pointer(&memPrio)),
		unsafe.Sizeof(memPrio),
	)

	if res5.Succeeded() {
		logf("Memory Priority set to %d where 5 is Normal", memPrio.MemoryPriority)
	} else {
		logf("Failed SetProcessInformation (Memory) to %d, r1: %v, err: %v, callStatus: %v", wantedMemPrio, res5.R1, res5.Err, res5.CallStatus)
	}

	// --- I/O Priority (Using NTDLL) ---
	// 4. Set I/O Priority (to 4 - High)
	// This affects disk access (logs), not mouse input. So I don't think i need this unless maybe there's constant heavy disk thrashing or gigs being written, then i need my logs(new log lines) saved not 2 minutes later.
	// IMPORTANT: We MUST use uint32 here so Sizeof returns 4, not 8.
	//IO_PRIORITY_HIGH(aka 4) will fail with NTSTATUS: 0xC000000D err: The operation completed successfully. and 3 will fail with NTSTATUS: 0xC0000061
	//You received 0xC000000D (STATUS_INVALID_PARAMETER) because Windows strictly limits I/O priority for user-mode applications. (even if running as admin btw)
	var ioHint uint32 = IO_PRIORITY_NORMAL //aka 2 works as it's the default anyway.
	// Note: NtSetInformationProcess returns an NTSTATUS, where 0 is STATUS_SUCCESS
	res6 := procNtSetInformationProcess.Call(
		hProc,
		uintptr(PROCESS_IO_PRIORITY), //33
		uintptr(unsafe.Pointer(&ioHint)),
		unsafe.Sizeof(ioHint),
	)
	//if ntStatus != 0 {
	if res6.Failed() {
		logf("Failed NtSetInformationProcess (I/O), NTSTATUS: 0x%X err: %v", res6.R1, res6.Err)
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
			logf("New MoveOrResize Channel Peak: %s events queued (Dropped: %s (due to throttling(most likely) or less-likely due to channel full))",
				withCommas(currentFill), withCommas(droppedMoveOrResizeEvents.Load()))
		}

		select {
		case data := <-moveDataChan:
			// Use the data (the struct copy) to move the window.
			// No heap pointers, no garbage collector stress!
			// Keep the throttle active here because this loop processes every single event sequentially
			handleActualMoveOrResize(data, false) // Move the window
		default:
			return // Channel empty, go back to GetMessage
		}
	}
}

// // drainMoveChannelCoalesced1 implements latest-wins per window, loses any order (ie. for different windows/hwnd, it won't know which to do first)
// // Replaces the old sequential drain.
// func drainMoveChannelCoalesced1() {
// 	latest := make(map[windows.Handle]WindowMoveData, 8) // small initial capacity; grows rarely

// 	// 1. Non-blocking full drain
// 	for {
// 		select {
// 		case data := <-moveDataChan:
// 			// 2+3. Overwrite → keep only newest state per HWND
// 			latest[data.Hwnd] = data
// 		default:
// 			// Queue empty → proceed to batch apply
// 			goto applyBatch
// 		}
// 	}

// applyBatch:
// 	// 4. Batch apply: exactly once per active window
// 	for hwnd, data := range latest {
// 		if hwnd == 0 || !isWindowValid(hwnd) { // lightweight check
// 			continue
// 		}
// 		handleActualMoveOrResize(data)
// 	}

// 	// Optional: clear map for GC friendliness (not strictly needed)
// 	// clear(latest) // Go 1.21+
// }

// Allocate these once globally. They are only ever accessed by the Main Thread.
var (
	// latest holds only the most recent state for each window
	coalesceMap = make(map[windows.Handle]WindowMoveData, 8)

	// order records the first-seen order of windows in this drain batch.
	// This gives us stable FIFO-like behavior across different windows
	// without sacrificing per-window coalescing.
	coalesceOrder = make([]windows.Handle, 0, 8)
)

// drainMoveChannelCoalesced implements event coalescing (latest-wins per window)
// while preserving approximate inter-window ordering using a first-seen slice.
// This directly addresses the rubber-banding issue described.
func drainMoveChannelCoalesced() {
	// // latest holds only the most recent state for each window
	//coalesceMap := make(map[windows.Handle]WindowMoveData, 8)

	// // order records the first-seen order of windows in this drain batch.
	// // This gives us stable FIFO-like behavior across different windows
	// // without sacrificing per-window coalescing.
	// coalesceOrder := make([]windows.Handle, 0, 8)

	//By reusing the same underlying memory for the map and the slice, Go no longer creates garbage during a window drag. Even if the GC is totally starved by your TIME_CRITICAL thread, memory will not grow because nothing new is being allocated.
	// 0. Clear the map and slice from the previous run WITHOUT reallocating
	for k := range coalesceMap {
		delete(coalesceMap, k)
	}
	coalesceOrder = coalesceOrder[:0]

	// 1. Non-blocking full drain of the channel
	for {
		select {
		case data := <-moveDataChan:
			if _, exists := coalesceMap[data.Hwnd]; !exists {
				coalesceOrder = append(coalesceOrder, data.Hwnd) // record first appearance order
			}
			coalesceMap[data.Hwnd] = data // always overwrite with newest state
		default:
			// Channel is empty → proceed to batch apply
			goto applyBatch
		}
	}

applyBatch:
	// 2. Batch apply in first-seen order
	for _, hwnd := range coalesceOrder {
		data, ok := coalesceMap[hwnd]
		if !ok || hwnd == 0 {
			continue
		}

		if !isWindowValid(hwnd) {
			continue
		}

		// We bypass the execution throttle here. Coalescing guarantees we only
		// apply once per window per batch, and we absolutely MUST NOT drop
		// the user's final intended window coordinates.
		handleActualMoveOrResize(data, true)
	}
}

// Simple helper (add near other utils)
func isWindowValid(hwnd windows.Handle) bool {
	if hwnd == 0 {
		return false
	}
	// Fast check without sending messages
	var rect RECT
	res := procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&rect)))
	return res.Succeeded()
}

func getClassName(hwnd windows.Handle) string {
	buf := make([]uint16, 256)
	res1 := procGetClassName.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	//if ret == 0 {
	if res1.Failed() {
		return ""
	}
	return windows.UTF16ToString(buf[:res1.R1])
}

// TODO: shall we make these toggles in systray? probably not, it's spammy debug!
var shouldLogFocusChanges = false
var shouldLogWindowEvents = false

var (
	// XXX: You already safely manage eventCount using atomic.AddUint64 and atomic.SwapUint64, which is good practice. Because you passed WINEVENT_OUTOFCONTEXT to SetWinEventHook, Windows delivers those callbacks to the message queue of the thread that registered the hook (the Main Thread). Therefore, lastReport and eventCount are technically single-threaded in this specific architecture, but keeping the atomics there is a smart defensive move against future refactors.
	//well, read the comment for winEventProc below (didn't double check it, Geminit 3.5 Flash made)
	eventCount uint64
	lastReport time.Time = time.Now()
)

/*
3. Can winEventProc be called recursively?

Yes, absolutely. Windows hooks and event hooks (winEventProc) are notoriously prone to re-entrancy and recursive callbacks.

When the user opens a system tray menu, Windows enters a modal loop. During this time, Windows continues pumping specialized messages and hooks entirely on the Main Thread. If a window in the background changes focus or generates an event while that menu is open, Windows will immediately interrupt the current thread state to invoke your winEventProc function.
Why your eventCount is safe anyway:

Even though re-entrancy can happen, your counter updates remain secure because you used atomic operations (like atomic.AddUint64 and atomic.SwapUint64).

If you had written it as a standard assignment (eventCount++), a recursive interrupt could result in a lost update:

	The thread reads the current value of eventCount (e.g., 5).

	A recursive event interrupts the thread and executes winEventProc entirely. It reads 5, increments it to 6, and saves it.

	The interrupt finishes, control returns to the original assignment, which still holds the stale value 5 in its CPU register, increments it to 6, and overwrites the variable. The recursive increment is completely lost.

By treating them as atomic assignments, you effectively forced the CPU to perform the increment as an indivisible operation at the hardware level, preventing recursion from causing corrupted calculations.
*/
func winEventProc(hWinEventHook windows.Handle, event uint32, hwnd windows.Handle, idObject, idChild int32, dwEventThread, dwmsEventTime uint32) uintptr {
	_ = hWinEventHook //don't warn me it's unused!
	_ = dwEventThread //don't warn me it's unused!
	_ = dwmsEventTime //don't warn me it's unused!

	// ONLY process if it's the actual window, not a sub-control/caret/item
	if idObject != OBJID_WINDOW { // 0 is OBJID_WINDOW
		return 0 // WinEvent callbacks return 0 (no chaining)
	}

	//fmt.Println("DEBUG: hook called")
	var nowCount uint64
	if shouldLogWindowEvents {
		nowCount = atomic.AddUint64(&eventCount, 1)
	}

	var eventName string = "unclassified&untracked"
	var untrackedEvent bool = false

	switch event {
	case EVENT_SYSTEM_FOREGROUND: //0x0003:
		eventName = "EVENT_SYSTEM_FOREGROUND"
	case EVENT_SYSTEM_CAPTURESTART: //0x0008:
		eventName = "EVENT_SYSTEM_CAPTURESTART"
		// fg := getForegroundWindow()
		// logf("CaptureStart: FG=0x%x eventHWND=0x%x", fg, hwnd)

		// time.AfterFunc(20*time.Millisecond, func() {
		// 	logf("20ms later FG=0x%x", getForegroundWindow())
		// })

		// time.AfterFunc(100*time.Millisecond, func() {
		// 	logf("100ms later FG=0x%x", getForegroundWindow())
		// })
	case EVENT_SYSTEM_CAPTUREEND: //0x0009:
		eventName = "EVENT_SYSTEM_CAPTUREEND"
	case EVENT_CONSOLE_UPDATE_REGION: //0x4002:
		//This fires when an object (window, button, menu item) is made visible. During a Regedit search,
		// it might fire if the UI is dynamically popping elements in and out of the view.
		eventName = "EVENT_CONSOLE_UPDATE_REGION"
		untrackedEvent = true
	case EVENT_CONSOLE_LAYOUT: // 0x4005:
		//It fires every time a window or an element moves or changes size.
		eventName = "EVENT_CONSOLE_LAYOUT"
		untrackedEvent = true
	case EVENT_OBJECT_CREATE: //0x8000:
		eventName = "EVENT_OBJECT_CREATE"
		untrackedEvent = true
	case EVENT_OBJECT_DESTROY: //0x8001:
		eventName = "EVENT_OBJECT_DESTROY"
		untrackedEvent = true
	case EVENT_OBJECT_SHOW: //0x8002:
		eventName = "EVENT_OBJECT_SHOW"
	case EVENT_OBJECT_HIDE: // 0x8003:
		eventName = "EVENT_OBJECT_HIDE"
	case EVENT_OBJECT_REORDER: //0x8004:
		eventName = "EVENT_OBJECT_REORDER"
	case EVENT_OBJECT_FOCUS: // 0x8005:
		eventName = "EVENT_OBJECT_FOCUS"
	default:
		// Return early if it's an event we aren't tracking to keep logs clean
		untrackedEvent = true
	}

	if shouldLogWindowEvents {
		// 1. Monitor Event Frequency (Every 1 second)
		if time.Since(lastReport) > time.Second && nowCount > 160 { //TODO: make it a const; can get 122 events per sec during resizes, or less than 50 during wtw else not-our-gesture events.
			count := atomic.SwapUint64(&eventCount, 0)
			//fmt.Printf
			logf("[DEBUG] Events per second: %d | Last Event: 0x%x(%s)", count, event, eventName)
			lastReport = time.Now()
		}

		// 2. Time the execution of the callback
		start := time.Now()
		defer func() {
			elapsed := time.Since(start)
			if elapsed > 5*time.Millisecond { // TODO: make it a const
				logf("[PERF] Slow Event 0x%x(%s): %v (HWND: 0x%x, ObjId: %d)", event, eventName, elapsed, hwnd, idObject)
			}
		}()
	}

	if untrackedEvent {
		// Return early if it's an event we aren't tracking to keep logs clean
		return 0
	}

	// --- THE RECONCILIATION TRIGGER ---
	// If we are currently locked out by a High-IL window, we hijack other
	// reliable events (like a mouse click causing CAPTURESTART) to manually
	// double-check if the foreground has secretly shifted back to normal.
	//
	// Windows occasionally(the first time always does it! not second+ times apparently)
	//  fails to emit EVENT_SYSTEM_FOREGROUND when returning
	// from a higher-integrity foreground window (observed with some Windows
	// Terminal activation paths). If we're currently waiting for such a
	// transition, use the first reliable mouse-capture event to reconcile the
	// actual foreground window via GetForegroundWindow().
	forceReconcile := foregroundWasHigherIntegrity.Load() &&
		event == EVENT_SYSTEM_CAPTURESTART // || event == EVENT_SYSTEM_CAPTUREEND || event == EVENT_OBJECT_FOCUS) // these two aren't needed, and last one isn't hit anyway!

	var pid uint32
	targetHwnd := hwnd

	if shouldLogFocusChanges || event == EVENT_SYSTEM_FOREGROUND || forceReconcile {
		// If reconciling, the event's 'hwnd' might just be a child element.
		// We want the absolute master foreground window to bypass the glitch.
		if forceReconcile && event != EVENT_SYSTEM_FOREGROUND {
			res1 := procGetForegroundWindow.Call()
			targetHwnd = windows.Handle(res1.R1)
		}

		if targetHwnd == 0 {
			if forceReconcile {
				logf("Reconciliation via %s: GetForegroundWindow() returned NULL; skipping reconciliation.", eventName)
			} else {
				logf("winEventProc's hwnd was 0, this is very undexpected! eventName=%s", eventName)
			}
			return 0 // WinEvent callbacks return 0 (no chaining)
		}

		//pid is needed in one OR two places outside of this 'if' block
		procGetWindowThreadProcessID.Call(uintptr(targetHwnd), uintptr(unsafe.Pointer(&pid)))
		//"Pro-tip: You don't need to check err for this specific API because it doesn't set LastError in the traditional way; you just check if the return value (or the written pid variable) is 0. Your current check if pid == 0 is the correct way to handle it." - gemini 3 Fast
		if pid == 0 {
			//some error or wtw
			logf("Couldn't get pid(it's 0) for HWND=0x%x for event 0x%x(%s)", targetHwnd, event, eventName)
			return 0 // WinEvent callbacks return 0 (no chaining)
		}
	}

	if shouldLogFocusChanges {
		// Get the top-level owner of this HWND to see if it belongs to CMD
		// GA_ROOT (2) gets the "real" parent window
		res1 := procGetAncestor.Call(uintptr(targetHwnd), 2)
		if res1.Failed() {
			logf("failed to get rootHwnd via GetAncestor on HWND=0x%x", targetHwnd)
			return 0 // WinEvent callbacks return 0 (no chaining)
		}
		rootHwnd := windows.Handle(res1.R1)

		title := getWindowTextFast(rootHwnd)
		procName := getProcessNameFast(pid)
		class := getClassName(targetHwnd)
		// if (event == EVENT_SYSTEM_CAPTURESTART) || (event == EVENT_SYSTEM_CAPTUREEND) { // yes it does have focus, even tho EVENT_SYSTEM_FOREGROUND is never sent! see caveats1.txt
		//	focusedHwnd := getForegroundWindow()
		// 	if focusedHwnd == hwnd || focusedHwnd == rootHwnd {
		// 		logf("at %s, the foreground window 0x%X is the same one that caused this event 0x%X or its rootHwnd 0x%X !", eventName, focusedHwnd, hwnd, rootHwnd)
		// 	} else {
		// 		logf("at %s, the foreground window 0x%X is NOT the same one that caused this event 0x%X or its rootHwnd 0x%X !", eventName, focusedHwnd, hwnd, rootHwnd)
		// 	}
		// }

		logf("[%s] HWND=0x%x (Root=0x%x) objId=%d childId=%d [%s] Class=[%s] PID=%d (%s)",
			eventName, targetHwnd, rootHwnd, idObject, idChild, title, class, pid, procName)
	}

	if event == EVENT_SYSTEM_FOREGROUND || forceReconcile {
		if pid == 0 {
			badprogramming("pid is 0 here, code logic was changed!")
		}

		targetIL, err := processIntegrityLevel(pid)

		if err == nil && targetIL > selfIntegrityLevel {
			// 		Quick Cheat Sheet for Levels:
			// 0x0000: Untrusted
			// 0x1000: Low (Browsers / AppContainers)
			// 0x2000: Medium (Standard User)
			// 0x3000: High (Administrator / Elevated)
			// 0x4000: System

			// Only lock the state down if this was a genuine foreground transition
			if event == EVENT_SYSTEM_FOREGROUND {
				logf("Target window HWND=0x%x is higher integrity (0x%x > 0x%x). UIPI will block movement(no key/mouse events will be received while it is focused!thus can't trigger the gesture).", targetHwnd, targetIL, selfIntegrityLevel)
				softReset(true)
				foregroundWasHigherIntegrity.Store(true)
			}
		} else {
			if shouldLogFocusChanges {
				logf("Current foreground PID=%d IL=0x%x ILerr=%v", pid, targetIL, err)
			}
			// We successfully detected a return to a normal window!
			if foregroundWasHigherIntegrity.Swap(false) {
				if missedGestureRecoveryEnabled.Load() {
					checkForMissedGestureOnNextMove.Store(true)

					reason := eventName //it's EVENT_SYSTEM_FOREGROUND
					if forceReconcile {
						reason = "reconciliation via " + reason + "(should only happen once, the first time after just started " + selfName + ")" //TODO: track if this happens more than once and warn in red color or something somehow notify me the dev, maybe write into a new file about it, or I guess the log is enough since it's always append
					}

					logf("Foreground regained a non-blocking integrity level (HWND=0x%x, PID=%d, IL=0x%x) [%s] after previously being blocked by a higher-integrity window; arming missed-gesture recovery check for the next mouse move.", targetHwnd, pid, targetIL, reason)
				} else if shouldLogFocusChanges {
					logf("Foreground regained a non-blocking integrity level (HWND=0x%x, PID=%d, IL=0x%x), but missed-gesture recovery is disabled; not arming.", targetHwnd, pid, targetIL)
				}
			}
		}
	} //endif focus event happened.
	return 0 // WinEvent callbacks return 0 (no chaining)
}

func badprogramming(msg string) {
	panic2(msg)
}

// use badprogramming() instead
func panic2(msg string) {
	//FIXME: once initWincoeLogging() wires the bridge, the wincoe.GetBugLogger().Error(msg) also funnels back into logf() (via slogBridge.Handle), so every panic2() call after that point writes the same message twice (once raw, once prefixed [wincoe])
	logf("%s", msg)
	wincoe.GetBugLogger().Error(msg) // so after initWincoeLogging(), this redirects to logf tho, so it doubles the line!
	panic(msg)
}

func getProcessNameFast(pid uint32) string {
	// PROCESS_QUERY_LIMITED_INFORMATION is very fast and doesn't require a snapshot
	hProc, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "<failed>"
	}
	//defer windows.CloseHandle(hProc)
	defer closeHandleLogged(hProc, "getProcessNameFast:OpenProcess hProc")

	buf := make([]uint16, windows.MAX_PATH)
	// #nosec G115 -- safe: buffer length is a small constant aka windows.MAX_PATH aka 260 which is well within uint32 bounds
	size := uint32(len(buf))
	// QueryFullProcessImageName is significantly faster than Toolhelp snapshots
	err = windows.QueryFullProcessImageName(hProc, 0, &buf[0], &size)
	if err != nil {
		return "<not found>"
	}

	// Just return the base name (regedit.exe)
	fullPath := windows.UTF16ToString(buf[:size])
	return filepath.Base(fullPath)
}

// InternalGetWindowText is a "non-blocking" call. It reads from the Desktop Heap (kernel memory) rather than sending a WM_GETTEXT message.
// This prevents your program from freezing when Regedit is too busy to respond to messages.
func getWindowTextFast(hwnd windows.Handle) string {
	buf := make([]uint16, 512)
	// This API does NOT send a message; it reads from kernel memory.
	res1 := procInternalGetWindowText.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	//if ret == 0 {
	if res1.Failed() {
		return "<failed>"
	}
	return windows.UTF16ToString(buf[:res1.R1])
}

// Package-level. Non-nil = capture is currently held for that session pointer.
var captureHeldForSession atomic.Pointer[dragSession]

// WinEvent hook flags (SetWinEventHook dwFlags argument).
const (
	WINEVENT_OUTOFCONTEXT   uint32 = 0x0000 // callback delivered out-of-context (different process)
	WINEVENT_SKIPOWNPROCESS uint32 = 0x0002 // suppress events originating in our own process
)

// WinEvent event codes.
// The hook registered in runApplication covers EVENT_SYSTEM_FOREGROUND..EVENT_OBJECT_FOCUS,
// which incidentally includes the 0x4xxx console-event band.
const (
	// System events
	EVENT_SYSTEM_FOREGROUND   uint32 = 0x0003
	EVENT_SYSTEM_CAPTURESTART uint32 = 0x0008 // a window acquired mouse capture
	EVENT_SYSTEM_CAPTUREEND   uint32 = 0x0009 // mouse capture was released

	// Console events (received because hook range 0x0003–0x8005 spans 0x4xxx)
	EVENT_CONSOLE_UPDATE_REGION uint32 = 0x4002
	EVENT_CONSOLE_LAYOUT        uint32 = 0x4005

	// Object events
	EVENT_OBJECT_CREATE  uint32 = 0x8000
	EVENT_OBJECT_DESTROY uint32 = 0x8001
	EVENT_OBJECT_SHOW    uint32 = 0x8002
	EVENT_OBJECT_HIDE    uint32 = 0x8003
	EVENT_OBJECT_REORDER uint32 = 0x8004
	EVENT_OBJECT_FOCUS   uint32 = 0x8005
)

// OBJID_WINDOW is the idObject value meaning the event concerns the window itself,
// not a child control, caret, or accessibility item.
const OBJID_WINDOW int32 = 0

// initForegroundIntegrityState seeds foregroundWasHigherIntegrity with the
// integrity level of whatever window currently has the foreground, at the
// moment winEventHook is installed. Without this, missed-gesture recovery
// never arms on the very first switch away from an already-elevated
// foreground window (e.g. Task Manager, if it was already focused before
// winbollocks started) — only from the second time onward, once
// winEventProc had a chance to observe a real transition *into* such a
// window while our hook was already active.
func initForegroundIntegrityState() {
	res1 := procGetForegroundWindow.Call()
	// procGetForegroundWindow is bound with wincoe.CheckNone (no failure signal beyond
	// NULL), so res1.Failed() can never be true; rely on the HWND itself instead.
	hwnd := windows.Handle(res1.R1)
	if hwnd == 0 {
		return // no foreground window right now (or GetForegroundWindow failed — indistinguishable per its docs); nothing to seed
	}

	pid := getWindowPID(hwnd)
	if pid == 0 {
		logf("initForegroundIntegrityState: couldn't get PID for current foreground HWND=0x%X", hwnd)
		return
	}

	il, err := processIntegrityLevel(pid)
	if err != nil {
		logf("initForegroundIntegrityState: processIntegrityLevel failed for PID %d (HWND=0x%X), err: %v", pid, hwnd, err)
		return
	}

	if il > selfIntegrityLevel {
		foregroundWasHigherIntegrity.Store(true)
		logf("initForegroundIntegrityState: current foreground HWND=0x%X (PID=%d, IL=0x%x) is already higher integrity than us (0x%x); seeded foregroundWasHigherIntegrity=true so missed-gesture recovery can arm on the next foreground change.", hwnd, pid, il, selfIntegrityLevel)
	}
}

// injectMouseButtonUp injects a single, bare button-up event (no down, no
// movement) at the current cursor position via SendInput. Used by the
// missed-gesture recovery path to tell whatever window legitimately saw the
// real LMB/RMB-down (because our hook was blind to it — see
// dragSession.viaMissedGestureRecovery) that the button is up now, so it
// stops treating subsequent mouse moves as an extension of its own
// click-drag (e.g. a console extending a text selection) while we drive the
// window move/resize ourselves.
func injectMouseButtonUp(flag uint32) {
	inputs := []INPUT{
		{
			Type: INPUT_MOUSE,
			Ki:   KEYBDINPUT{}, // union placeholder
		},
	}

	(*MOUSEINPUT)(unsafe.Pointer(&inputs[0].Ki)).DwFlags = flag

	res1 := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)

	if res1.Failed() || res1.R1 != uintptr(len(inputs)) {
		logf("SendInput mouse button-up injection (flag=0x%x) failed: ret=%d err=%v", flag, res1.R1, res1.Err)
	}
}

func injectLMBUp() {
	injectMouseButtonUp(MOUSEEVENTF_LEFTUP)
}

func injectRMBUp() {
	injectMouseButtonUp(MOUSEEVENTF_RIGHTUP)
}

// initDarkMode tells Windows this app supports dark mode,
// allowing standard Win32 context menus to follow the system theme.
func initDarkMode() {
	uxtheme := windows.NewLazySystemDLL("uxtheme.dll")
	if err := uxtheme.Load(); err != nil {
		logf("initDarkMode: Failed to load uxtheme.dll for dark mode: %v", err)
		return
	}

	// PreferredAppMode: 0=Default, 1=AllowDark, 2=ForceDark, 3=ForceLight, 4=Max
	// Passing 1 allows it to seamlessly follow the system's active theme.
	const AllowDark uintptr = 1
	const wanted = AllowDark

	// Quick string mapper
	modeStr := func(m uintptr) string {
		switch m {
		case 0:
			return "Default"
		case 1:
			return "AllowDark"
		case 2:
			return "ForceDark"
		case 3:
			return "ForceLight"
		case 4:
			return "Max"
		default:
			return fmt.Sprintf("Unknown(%d)", m)
		}
	}

	// Ordinal 135: SetPreferredAppMode (Windows 10 1903+) / AllowDarkModeForApp (Windows 10 1809)
	if procSetPreferredAppMode, err := windows.GetProcAddressByOrdinal(windows.Handle(uxtheme.Handle()), 135); err == nil {
		r1, _, errno := syscall.SyscallN(procSetPreferredAppMode, wanted)
		if errno != 0 {
			logf("initDarkMode: uxtheme ordinal 135 (SetPreferredAppMode) returned errno: %v", errno)
		} else {
			// SetPreferredAppMode returns the previous state as r1.
			logf("initDarkMode: uxtheme ordinal 135 (SetPreferredAppMode) succeeded, current mode: %s, prev mode: %s", modeStr(wanted), modeStr(r1))
		}
	} else {
		logf("initDarkMode: Failed to find uxtheme ordinal 135: %v", err)
	}

	// Ordinal 136: FlushMenuThemes (forces Windows to refresh the menu rendering cache)
	if procFlushMenuThemes, err := windows.GetProcAddressByOrdinal(windows.Handle(uxtheme.Handle()), 136); err == nil {
		_, _, errno := syscall.SyscallN(procFlushMenuThemes)
		if errno != 0 {
			logf("initDarkMode: uxtheme ordinal 136 (FlushMenuThemes) returned errno: %v", errno)
		}
	} else {
		logf("initDarkMode: Failed to find uxtheme ordinal 136: %v", err)
	}
}

const TPM_RETURNCMD = 0x0100

var startupTerminalHwnd windows.Handle

func closeHandleLogged(h windows.Handle, context2 string) {
	if err := windows.CloseHandle(h); err != nil {
		logf("CloseHandle failed for %s: %v", context2, err)
	}
}

func modifierKeyState() (winDown, shiftDown, ctrlDown, altDown bool) {
	winDown = keyDown(VK_LWIN) || keyDown(VK_RWIN)
	shiftDown = keyDown(VK_SHIFT)
	ctrlDown = keyDown(VK_CONTROL)
	altDown = keyDown(VK_MENU)
	return
}

func markGestureUsedOnce() {
	if !winGestureUsed.Load() { //wasn't set already
		winGestureUsed.Store(true) // we used at least once of our gestures
		injectShiftTapOnly()       // has dual benefits: 1. prevent releasing of winkey later from popping up Start menu! AND 2. allows focusing target window to not be prevented by win11's focus stealing prevention!
	}
}

// enqueueMoveOrResize submits data to moveDataChan and wakes the main thread's
// message loop to drain it. context is only used in the failure log.
func enqueueMoveOrResize(data WindowMoveData, context3 string) {
	// Send to your mover channel
	select {
	case moveDataChan <- data:
		// SUCCESS: The data was copied into the buffered channel.
		// Now we ring the "Doorbell" to wake up the Main Thread.
		// PostThreadMessage(and PostMessage, but not SendMessage!) is an asynchronous "fire and forget" call.
		//the reason we use PostMessage and not PostThreadMessage here is because while systray menu popup is open it runs its own msg loop and calls my wndProc so it will ignore all of these doorbells until popup is closed if i use postThreadMessage!
		if res := procPostMessage.Call(uintptr(mainMsgHwnd), WM_DO_SETWINDOWPOS, 0, 0); res.Failed() {
			logf("PostMessage of WM_DO_SETWINDOWPOS for %s failed: %v", context3, res.Err)
		}
	default:
		// FAIL: The channel (2048 slots) is completely full.
		// This happens if the Main Thread is frozen (e.g., Admin console lag).
		// We MUST NOT block here, or we will freeze the user's entire mouse cursor.
		// We just increment our "shame counter" and move on.
		droppedMoveOrResizeEvents.Add(1) //TODO: use diff. one to keep track of drops due to channel full
	}
}

// setForegroundWindow calls SetForegroundWindow and handles the boilerplate logging if it fails.
func setForegroundWindow(hwnd windows.Handle, failLogPrefix string) bool {
	res := procSetForegroundWindow.Call(uintptr(hwnd))
	if res.Failed() {
		//XXX: you get ret=0 aka res.Err=0 with "err=The operation completed successfully." when Start menu was already open
		logf("%s ret=%d err='%v' callStatus='%v'", failLogPrefix, res.R1, res.Err, res.CallStatus)
		return false
	}
	return true
}

// FlushLogs blocks until the logWorker has written all currently queued messages.
func FlushLogs() {
	if logQuitClosed.Load() {
		return // Worker is already dead/dying
	}
	ack := make(chan struct{})
	select {
	case logFlushChan <- ack:
		<-ack // Wait for logWorker to finish draining
	case <-logWorkerDone:
		// Worker exited before it could process
	}
}

// Version is a global variable that can be overwritten at build time via -ldflags
var Version = ""

// Compute the string exactly once at package startup
var memoizedVersion = func() string {
	var baseVersion string
	var vcsRevision string
	var vcsTime string
	var isModified bool

	// 1. Determine the base version (Release tag / module path)
	if Version != "" {
		baseVersion = Version
	} else if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			baseVersion = info.Main.Version
		}
	}

	// Default base if nothing is found yet
	if baseVersion == "" {
		baseVersion = "dev"
	}

	// 2. Extract the underlying VCS revision if embedded by the compiler
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if setting.Value != "" {
					vcsRevision = setting.Value
					if len(vcsRevision) > 16 {
						vcsRevision = vcsRevision[:16]
					}
				}
			case "vcs.time":
				if setting.Value != "" {
					if t, err := time.Parse(time.RFC3339, setting.Value); err == nil {
						vcsTime = t.Format("20060102150405")
					} else {
						vcsTime = strings.NewReplacer("-", "", "T", "", ":", "", "Z", "").Replace(setting.Value)
					}
				}
			case "vcs.modified":
				if setting.Value == "true" {
					isModified = true
				}
			}
		}
	}

	// 3. Assemble the final version string
	suffix := ""
	if vcsTime != "" {
		suffix += "-0." + vcsTime
	}
	if vcsRevision != "" && !strings.Contains(baseVersion, vcsRevision) {
		suffix += "-" + vcsRevision
	}
	if isModified {
		suffix += "+dirty"
	}

	return baseVersion + suffix
}()

// GetVersion returns the cached build info string directly
func GetVersion() string {
	return memoizedVersion
}
