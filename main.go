//go:build windows && amd64

// winbollocks targets 64-bit Windows only. Several Win32 ABI details are
// architecture-specific:
//   - The wincoe.KEYANDMOUSE_INPUT / KEYBDINPUT struct layout includes explicit 64-bit padding.
//   - wincoe.WindowFromPointRaw or AncestorWindowFromPoint receives POINT by value packed into a single 64-bit
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
	"sync"
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
	//res := procGetModuleHandle.Call(0) // "If this parameter is NULL, GetModuleHandle returns a handle to the file used to create the calling process (.exe file)."
	res := wincoe.GetModuleHandle(nil)
	if res.Failed() {
		panic(fmt.Sprintf("CRITICAL: GetModuleHandle(0) failed for self instance: %v", res))
	}
	selfHInstance = windows.Handle(res.R1)
}

/* ---------------- DLLs & Procs ---------------- */

// var shellHook windows.Handle
var (
	// The Data Pipe (2048 is plenty for lag spikes)
	moveDataChan = make(chan WindowMoveData, 2048)
	// Tracks if a WM_DO_SETWINDOWPOS is already sitting in the message queue
	doorbellPending atomic.Bool

	// Modern Atomic tracking
	droppedMoveOrResizeEvents   atomic.Uint64
	droppedLogEvents            atomic.Uint64
	maxChannelFillForMoveEvents atomic.Uint64 // To track how "full" it got
	maxChannelFillForLogEvents  atomic.Uint64 // To track how "full" it got

	// logDepthCASFailures counts times logf's high-water-mark CAS loop
	// exhausted all attemptAtomicSwapThisManyTimes retries. Telemetry must
	// never be allowed to crash the app: losing one high-water-mark sample
	// under extreme contention is a rounding error, not a correctness
	// issue, so this is counted (and reported once at shutdown, alongside
	// the two counters above) instead of panicking.
	logDepthCASFailures atomic.Uint64
)

func init() {
	maxChannelFillForMoveEvents.Store(1) // avoid the first message: New Channel Peak: 1 events queued (Dropped: 0)
}

var (
	winEventHook windows.Handle
	//winEventCallback = windows.NewCallback(winEventProc)
)

// lastKnownUserForegroundHwnd tracks the most recent non-shell foreground
// window, updated from winEventProc's EVENT_SYSTEM_FOREGROUND handling.
// Needed because right-clicking the tray icon activates Explorer's taskbar
// (Shell_TrayWnd) as a side effect of the click itself, before WM_MYSYSTRAY
// is even delivered to us -- a GetForegroundWindow() snapshot taken inside
// WM_MYSYSTRAY therefore always sees Explorer, never whatever window the
// user was actually working in. Continuous tracking with a shell-class
// filter sidesteps that.
var lastKnownUserForegroundHwnd atomic.Uintptr

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

/* ---------------- Constants ---------------- */

var (
	overlayHwnd windows.Handle
	overlayText string

	overlayIsShowing atomic.Bool // Tracks overlay visibility to prevent hide spam

	// Reusable GDI brushes
	magentaBrush windows.Handle
	blackBrush   windows.Handle
)

// HungWindowTimeout if target window doesn't respond in 150ms we consider it hung and don't attempt to attach our input thread to it in an attempt to succeed at focusing it because it would also hang us.
const HungWindowTimeout = 150 // ms

const (
	WM_MYSYSTRAY                   = wincoe.WM_USER + 2
	WM_INJECT_SEQUENCE             = wincoe.WM_USER + 100
	WM_FOCUS_TARGET_WINDOW_SOMEHOW = wincoe.WM_USER + 101
	WM_EXIT_VIA_CTRL_C             = wincoe.WM_USER + 150
	WM_DO_SETWINDOWPOS             = wincoe.WM_USER + 200 // arbitrary, just unique
	WM_HIDE_OVERLAY                = wincoe.WM_USER + 205
	WM_BRING_TO_FRONT              = wincoe.WM_USER + 206
	WM_DO_RELEASE_CAPTURE          = wincoe.WM_USER + 215
	WM_CANCEL_GESTURE              = wincoe.WM_USER + 220
	WM_APPLY_SHIFT_MIRROR          = wincoe.WM_USER + 225
)
const (
	MENU_EXIT                                      = 1
	MENU_USE_LMB_TO_FOCUS_AS_FALLBACK              = 2
	MENU_ACTIVATE_MOVE                             = 3
	MENU_RATELIMIT_MOVES                           = 4
	MENU_LOG_RATE_OF_MOVES                         = 5
	MENU_TOGGLE_ASYNC_RESIZE                       = 6
	MENU_TOGGLE_REQUIRE_WINDOWN                    = 7
	MENU_TOGGLE_COALESCE_EVENTS                    = 8
	MENU_TOGGLE_IMMEDIATE_OVERLAY_REPAINT          = 9
	MENU_TOGGLE_MISSED_GESTURE_RECOVERY            = 10
	MENU_TOGGLE_INJECT_BUTTON_UP_ON_RECOVERY       = 11
	MENU_TOGGLE_BRING_TO_FRONT_ON_DRAG             = 12
	MENU_TOGGLE_BYPASS_GESTURES_WHEN_FULLSCREEN    = 13
	MENU_TOGGLE_USE_THREADATTACHINPUT_FOR_FOCUS    = 14
	MENU_TOGGLE_ACTIVATE_RESIZE                    = 15
	MENU_TOGGLE_BRING_TO_FRONT_ON_RESIZE           = 16
	MENU_TOGGLE_BRING_TO_FRONT_ON_BACKGROUND_CLICK = 17
	MENU_TOGGLE_UNFOCUS_SENT_TO_BACK               = 18
	MENU_SHOW_INPUT_STATE                          = 19
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

	// UnfocusAfterSendToBack marks this entry as originating from a
	// winkey+MMB (no shift) send-to-back gesture whose target actually held
	// keyboard focus at the moment of the gesture (see
	// tryPerformMMBGestureAt's !shiftDown branch and its
	// targetWasFocusedBeforeSendToBack local), so handleActualMoveOrResize
	// knows it's safe -- and intended -- to check whether
	// unfocusSentToBackWindow requires shifting focus to whatever is now
	// the top-of-Z-order window afterward. Left false (the zero value) for
	// every other case: ordinary drag-move, resize, the winkey+shift+MMB
	// bring-to-front gesture (which uses its own FocusAfterBringToFront
	// flag below instead, since it always wants target itself refocused,
	// never some OTHER now-topmost window), AND a winkey+MMB send-to-back
	// gesture whose target was NOT already focused -- sending an
	// already-unfocused "side window" to the back must never disturb
	// whatever unrelated window actually holds focus, since SWP_NOACTIVATE
	// already guarantees the OS itself won't touch it either.
	UnfocusAfterSendToBack bool

	// FocusAfterBringToFront marks this entry as originating from a
	// winkey+shift+MMB (bring-to-front) gesture (see
	// tryPerformMMBGestureAt's shiftDown branch), so
	// handleActualMoveOrResize explicitly refocuses Hwnd via
	// forceForeground() once its Z-order change (always issued with
	// SWP_NOACTIVATE for this gesture -- see Flags) succeeds. This gesture
	// always sets this flag rather than relying on SetWindowPos's own
	// implicit activation (omitting SWP_NOACTIVATE and hoping HWND_TOP
	// alone both reorders AND focuses): that implicit path is gated by the
	// same foreground-lock/focus-stealing-prevention rules as a bare
	// SetForegroundWindow call, and empirically both silently fails to
	// steal focus AND declines to fully promote the window to the top of
	// the Z order when the target isn't already foreground -- exactly the
	// situation unfocusSentToBackWindow creates (some other window now
	// holds focus after an earlier send-to-back). forceForeground()
	// already no-ops safely when Hwnd happens to already be foreground
	// (the unfocusSentToBackWindow==false fallback path in
	// tryPerformMMBGestureAt, where the target comes straight from
	// GetForegroundWindow()), so setting this unconditionally for the
	// gesture is correct regardless of that setting.
	FocusAfterBringToFront bool
}

type dragState struct {
	startPt   wincoe.POINT
	startRect wincoe.RECT
}

/* ---------------- Globals ---------------- */

var (
	//The Problem: winGestureUsed, capturing, and resizing manage your state machine. They are flipped by keyboardProc/mouseProc but cleared by hardReset/softReset (which can be triggered by the Main Thread via winEventProc).
	// used exclusively to know when to inject shift key tap (shiftdown+shiftUP) at the point when physical winkeyUP aka winUP is detected //noTODO: maybe remove because we do it(shifttap) now at gesture start
	winGestureUsed atomic.Bool

	// lmbDownSwallowed / rmbDownSwallowed / mmbDownSwallowed track whether we
	// ourselves swallowed the corresponding real WM_*BUTTONDOWN event via our
	// winkey+button gesture-start handling (see mouseProc's WM_LBUTTONDOWN /
	// WM_RBUTTONDOWN / WM_MBUTTONDOWN cases), independent of activeSession's
	// own lifecycle. activeSession can be cleared for reasons unrelated to
	// whether the ORIGINATING down-event was swallowed (winkey released
	// mid-drag triggers hardReset from WM_MOUSEMOVE; a gesture that failed to
	// start at all -- see tryBeginMoveGestureAt's "Invalid window" case --
	// still swallows the down but never creates a session in the first place;
	// a WTS lock/unlock cycle discards a stale session in wndProc). Using
	// activeSession!=nil as a proxy for "should the matching up be swallowed
	// too" is therefore unreliable and can desync a target app's button state
	// (a swallowed down with a passed-through up looks, to that app, exactly
	// like an up with no preceding down -- stuck hover/selection states or
	// spurious clicks). These flags are the single source of truth for that
	// decision instead, and are NOT set for missed-gesture-recovery sessions
	// (viaMissedGestureRecovery): those exist specifically because our hook
	// never SAW the real down (a higher-integrity window had focus at the
	// time), so the real up must reach the target normally, same as today.
	lmbDownSwallowed atomic.Bool
	rmbDownSwallowed atomic.Bool
	mmbDownSwallowed atomic.Bool
)

// resetStaleGestureFlags clears winGestureUsed and lmbDownSwallowed/
// rmbDownSwallowed/mmbDownSwallowed unconditionally.
//
// All four flags record "something about the CURRENT gesture/keypress that
// a LATER event needs to react to" -- winGestureUsed says "the eventual
// winkey-up should be suppressed (Start menu shouldn't pop)", and the three
// *Swallowed flags say "the eventual button-up should be swallowed too, to
// balance the down we already ate". Both kinds of promise are only honored
// if our hook actually gets to SEE that later up/winkey-up event. Two
// situations break that assumption, and both call this helper at exactly
// the moment visibility is lost:
//
//  1. WTS lock/unlock (see WM_WTSSESSION_CHANGE in wndProc): input on the
//     secure desktop is entirely invisible to our hooks.
//  2. The foreground transitioning to a higher-integrity window (see
//     winEventProc's EVENT_SYSTEM_FOREGROUND handling): UIPI blocks
//     delivery of hook callbacks to our (lower-integrity) process for as
//     long as that window -- or anything else at/above its integrity
//     level -- stays foreground. Critically, this can be triggered BY a
//     gesture itself: winkey+MMB's send-to-back, when
//     unfocusSentToBackWindow is enabled, refocuses whatever window is now
//     topmost (see findNewForegroundCandidateAfterSendToBack) WITHOUT
//     checking its integrity level, so a single winkey+MMB (or
//     winkey+shift+MMB) can hand focus straight to an elevated window
//     while its own real MMB-up is still physically pending -- exactly the
//     "MMB stuck down" scenario this was written to fix.
//
// Left stuck true across either situation, the eventual real up/winkey-up
// event -- now invisible to us -- never arrives to naturally flip the flag
// back, so it stays true indefinitely. The NEXT, entirely unrelated
// transition of that same button/key (once we're not blind anymore, on
// some future, unconnected window) then gets incorrectly treated as
// "belongs to a gesture we're tracking" and gets swallowed/suppressed -- for
// the *Swallowed flags this means a real button-up never reaches whatever
// window the user just clicked, desyncing that window's (and potentially
// the shell's) own button-state tracking exactly the way a physically
// stuck button would.
func resetStaleGestureFlags() {
	winGestureUsed.Store(false)
	lmbDownSwallowed.Store(false)
	rmbDownSwallowed.Store(false)
	mmbDownSwallowed.Store(false)
}

var (
	// mouseHook windows.Handle
	// kbdHook   windows.Handle

	trayIcon wincoe.NOTIFYICONDATA
)

// trayIconMu guards all reads/writes to trayIcon (including the
// Shell_NotifyIconW calls that read it by pointer). showTrayInfo can run on
// the hook thread (via startDrag) while initTray/cleanupTray and
// showTrayInfo's other callers (via handleActualMoveOrResize) run on the
// main thread -- without this, concurrent unsynchronized mutation of this
// shared NOTIFYICONDATA struct from two threads risks memory corruption and
// malformed Shell_NotifyIconW calls.
var trayIconMu sync.Mutex

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

	// wasMaximizedAtStart records whether targetWnd was maximized at the
	// moment this gesture began (see startManualDrag / tryBeginResizeGestureAt,
	// both of which SW_RESTORE the window before the drag/resize actually
	// starts). Consulted only by cancelActiveGesture (triggered by pressing
	// ESC mid-gesture -- see tryCancelActiveGestureViaEsc and
	// WM_CANCEL_GESTURE), which re-maximizes the window after undoing the
	// move/resize back to state.startRect, so cancelling a gesture that
	// began on a maximized window leaves it maximized again instead of
	// stranding it at its aligned-under-cursor restored size/position.
	wasMaximizedAtStart bool

	// originalRect/originalPt record the window's rect and the cursor's
	// position at the very moment THIS gesture began -- set once, in
	// startManualDrag (ModeMove) or tryBeginResizeGestureAt (ModeResize),
	// and never touched again for the rest of the gesture's lifetime,
	// unlike state.startRect/startPt (which handleShiftMirrorToggle DOES
	// replace, every time Shift is pressed/released, to rebaseline resize
	// math against the window's live size at each toggle -- see its doc
	// comment). cancelActiveGesture (the ESC-to-undo feature) deliberately
	// reads THESE fields instead of state.startRect/startPt, so an ESC
	// press always restores the window to how it looked before ANY part of
	// the gesture happened, regardless of how many times Shift was toggled
	// in between.
	originalRect wincoe.RECT
	originalPt   wincoe.POINT

	// shiftMirrorActive, mirrorReturnZone, and mirrorReturnPt implement the
	// Shift-held resize accelerator (see handleShiftMirrorToggle, triggered
	// from keyboardProc's real Shift key-down/key-up events via
	// postShiftMirrorToggleIfNeeded/WM_APPLY_SHIFT_MIRROR -- NOT lazily
	// discovered on the next mouse move, so the cursor warp below happens
	// instantly on the key transition with no mouse movement required):
	// holding Shift mid-resize warps the cursor to the point-reflected
	// position in the diametrically-opposite resize zone and replaces
	// activeSession with a fresh *dragSession rebaselined there (size
	// frozen at whatever it currently is), so the resize continues exactly
	// as if it had started fresh from that opposite zone -- letting one
	// gesture push/pull both edges instead of requiring two separate ones.
	// shiftMirrorActive is true only on such a mirrored session (always
	// false for ModeMove, and false for an ordinary non-mirrored
	// ModeResize session). mirrorReturnZone/mirrorReturnPt only have
	// meaningful values when shiftMirrorActive is true: they record the
	// ORIGINAL (pre-mirror) zone and the cursor's exact screen position
	// immediately before the mirror warp, so releasing Shift can warp the
	// cursor back and rebaseline back to that original zone (against the
	// window's live rect at release time, not the pre-mirror rect).
	shiftMirrorActive bool
	mirrorReturnZone  int
	mirrorReturnPt    wincoe.POINT
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

// bringToFrontOnBackgroundClick, when true, restores a focused-but-
// backgrounded window (e.g. one previously sent to the back of the
// Z-order via winkey+MMB) to the top of the Z-order the moment the user
// clicks on it with LMB/MMB/RMB while winkey isn't held -- see
// tryBringForegroundToFrontAt, called from mouseProc's
// WM_LBUTTONDOWN/WM_RBUTTONDOWN/WM_MBUTTONDOWN handling. Toggleable via
// systray.
var bringToFrontOnBackgroundClick atomic.Bool

// unfocusSentToBackWindow, when true, shifts keyboard focus away from a
// window immediately after it's sent to the back of the Z-order via
// winkey+MMB (see tryPerformMMBGestureAt's !shiftDown branch and
// WindowMoveData.UnfocusAfterSendToBack). That SetWindowPos call always
// passes SWP_NOACTIVATE so the reordering itself doesn't disturb focus,
// which means the now-backgrounded window silently keeps keyboard focus
// even though something else is now visually on top -- typing afterward
// goes to the window you just tried to banish, not to whatever you can
// actually see. When enabled, handleActualMoveOrResize walks the Z-order
// (see findNewForegroundCandidateAfterSendToBack) to find whichever window
// is now topmost and gives it focus instead.
// Off by default(Edit:  Switched to on by default!): most people
// don't mind the quirk, and focusing an arbitrary other-process window
// carries the same small residual risk any of this codebase's other focus
// attempts do (e.g. Windows' own focus-stealing prevention could still
// reject it in some state). Toggleable via systray.
var unfocusSentToBackWindow atomic.Bool

// lastSentToBackHwnd remembers the last window pushed to the back of the Z-order
// via winkey+MMB, so winkey+shift+MMB can reliably summon it back even if it lost focus.
var lastSentToBackHwnd atomic.Uintptr

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

func getResizeZone(pt wincoe.POINT, r wincoe.RECT) int {
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

// oppositeResizeZone returns the "opposite" resize zone for zone, used when
// Shift is held during an in-progress drag-resize gesture (see the
// ModeResize case in mouseProc's WM_MOUSEMOVE handling) so the same raw
// cursor movement that would normally drag the original edge/corner instead
// drags the diametrically-opposite one -- e.g. holding Shift while resizing
// from the right edge makes further right-drags now push/pull the LEFT
// edge instead, without requiring the cursor to actually move there.
// ZONE_CENTER has no opposite (it already resizes symmetrically from all
// sides at once) and mirrors to itself.
func oppositeResizeZone(zone int) int {
	switch zone {
	case ZONE_TOP_LEFT:
		return ZONE_BOT_RIGHT
	case ZONE_TOP_CENTER:
		return ZONE_BOT_CENTER
	case ZONE_TOP_RIGHT:
		return ZONE_BOT_LEFT
	case ZONE_MID_LEFT:
		return ZONE_MID_RIGHT
	case ZONE_MID_RIGHT:
		return ZONE_MID_LEFT
	case ZONE_BOT_LEFT:
		return ZONE_TOP_RIGHT
	case ZONE_BOT_CENTER:
		return ZONE_TOP_CENTER
	case ZONE_BOT_RIGHT:
		return ZONE_TOP_LEFT
	default: // ZONE_CENTER, or any unexpected value
		return zone
	}
}

// mirrorPointInRect reflects pt through the center of r -- a combined
// horizontal AND vertical point-reflection (equivalent to a 180-degree
// rotation about r's center). Reflecting the raw cursor position through
// the whole window's center this way is self-similar at any granularity:
// it simultaneously (a) moves the cursor into the diametrically-opposite
// resize zone and (b) mirrors its relative position within that zone --
// both horizontally and vertically -- without needing to reason about
// zones or sub-grids explicitly.
func mirrorPointInRect(pt wincoe.POINT, r wincoe.RECT) wincoe.POINT {
	return wincoe.POINT{
		X: r.Left + r.Right - pt.X,
		Y: r.Top + r.Bottom - pt.Y,
	}
}

// handleShiftMirrorToggle implements the Shift-held resize accelerator: it
// lets a single resize gesture push/pull BOTH edges/corners of a window
// without the cursor ever needing to physically travel between them.
//
// shiftDown is the caller's already-determined target state -- NOT
// re-derived here via keyDown(VK_SHIFT)/GetAsyncKeyState. This is called
// from wndProc's WM_APPLY_SHIFT_MIRROR handler, itself triggered the
// instant keyboardProc sees a real WM_KEYDOWN/WM_KEYUP transition for
// VK_SHIFT/VK_LSHIFT/VK_RSHIFT (see postShiftMirrorToggleIfNeeded) --
// deliberately NOT discovered lazily by polling Shift's state on the next
// WM_MOUSEMOVE, so the cursor warp happens immediately on the key
// transition itself, with no mouse movement required to trigger it. On a
// transition (shiftDown != session.shiftMirrorActive), it:
//
//  1. Reads the window's LIVE rect via GetWindowRect -- deliberately NOT
//     session.state.startRect -- so the window's size at the exact moment
//     of the toggle is preserved unchanged; only the baseline used for
//     FUTURE resize computation changes.
//  2. Point-reflects the current cursor position through that live rect's
//     center (see mirrorPointInRect) and warps the OS cursor there via
//     SetCursorPos.
//  3. Replaces activeSession with a brand-new *dragSession whose
//     state.startRect/startPt are the live rect/warped cursor position and
//     whose resizeZone is the opposite zone (pressing Shift) or the
//     original zone again (releasing Shift) -- so calculateResize treats
//     this exactly as if a fresh resize gesture had just started from
//     there. originalRect/originalPt are carried through UNCHANGED from
//     the prior session (see their doc comment on dragSession -- ESC-
//     cancel depends on these staying fixed across any number of mirror
//     toggles).
//
// Releasing Shift warps the cursor back to mirrorReturnPt (its exact
// screen position immediately before the mirror-triggered warp) and
// rebaselines back to mirrorReturnZone, again against the window's live
// rect at release time -- so resizing continues seamlessly from the
// original edge/corner using whatever size resulted from the mirrored
// phase, rather than snapping back to the pre-mirror size.
//
// Returns the new session if a transition was applied (callers should log
// its target/zone if they need to; the session is already published via
// activeSession.Store internally), or nil if shiftDown already matched
// session.shiftMirrorActive (a redundant call -- e.g. a duplicate
// WM_APPLY_SHIFT_MIRROR queued during Shift's OS key-repeat before the
// first one was processed; see postShiftMirrorToggleIfNeeded's doc
// comment).
func handleShiftMirrorToggle(session *dragSession, cursorPt wincoe.POINT, shiftDown bool) *dragSession {
	if shiftDown == session.shiftMirrorActive {
		return nil // already in the requested state
	}

	var liveRect wincoe.RECT
	if res := wincoe.GetWindowRect(session.targetWnd, &liveRect); res.Failed() {
		logf("handleShiftMirrorToggle: GetWindowRect on HWND=0x%X failed, err: %v; skipping shift-mirror toggle for this event", session.targetWnd, res.Err)
		return nil
	}
	w := liveRect.Right - liveRect.Left
	h := liveRect.Bottom - liveRect.Top
	if w <= 0 || h <= 0 {
		logf("handleShiftMirrorToggle: invalid live window size %dx%d for HWND=0x%X; skipping toggle", w, h, session.targetWnd)
		return nil
	}

	next := &dragSession{
		targetWnd:                session.targetWnd,
		mode:                     ModeResize,
		viaMissedGestureRecovery: session.viaMissedGestureRecovery,
		wasMaximizedAtStart:      session.wasMaximizedAtStart,
		initialAspectRatio:       float64(w) / float64(h),
		originalRect:             session.originalRect, // carried through unchanged, see doc comment
		originalPt:               session.originalPt,
	}

	if shiftDown {
		// Entering mirrored mode: freeze current size, warp cursor to the
		// point-reflection of its current position, resize fresh from the
		// diametrically opposite zone.
		mirroredPt := mirrorPointInRect(cursorPt, liveRect)
		if res := wincoe.SetCursorPos(mirroredPt.X, mirroredPt.Y); res.Failed() {
			logf("handleShiftMirrorToggle: SetCursorPos (mirror warp) failed: %v", res.Err)
		}
		next.state = dragState{startPt: mirroredPt, startRect: liveRect}
		next.resizeZone = oppositeResizeZone(session.resizeZone)
		next.shiftMirrorActive = true
		next.mirrorReturnZone = session.resizeZone
		next.mirrorReturnPt = cursorPt
		logf("Shift held during resize of HWND=0x%X: mirroring from zone %d to zone %d, cursor warped from (%d,%d) to (%d,%d)",
			session.targetWnd, session.resizeZone, next.resizeZone, cursorPt.X, cursorPt.Y, mirroredPt.X, mirroredPt.Y)
	} else {
		// Releasing mirrored mode: warp the cursor back to exactly where it
		// was right before the mirror warp, and rebaseline back to the
		// original zone against the window's live (post-mirror-resize) rect.
		if res := wincoe.SetCursorPos(session.mirrorReturnPt.X, session.mirrorReturnPt.Y); res.Failed() {
			logf("handleShiftMirrorToggle: SetCursorPos (mirror restore) failed: %v", res.Err)
		}
		next.state = dragState{startPt: session.mirrorReturnPt, startRect: liveRect}
		next.resizeZone = session.mirrorReturnZone
		next.shiftMirrorActive = false
		logf("Shift released during resize of HWND=0x%X: returning to zone %d, cursor warped back to (%d,%d)",
			session.targetWnd, next.resizeZone, session.mirrorReturnPt.X, session.mirrorReturnPt.Y)
	}

	activeSession.Store(next)
	return next
}

// postShiftMirrorToggleIfNeeded is keyboardProc's entry point into the
// Shift-mirror resize accelerator (see handleShiftMirrorToggle). Called on
// every real (non-injected) WM_KEYDOWN/WM_SYSKEYDOWN or
// WM_KEYUP/WM_SYSKEYUP transition of a Shift key, with intendedShiftDown
// set directly from which of those two event kinds fired -- true for
// down, false for up -- rather than by reading keyDown(VK_SHIFT)/
// GetAsyncKeyState afterward, which the hook callback can observe as
// stale relative to the very transition currently being handled (the
// identical caveat this file's own winkey-up handling already documents
// for GetAsyncKeyState during a hook callback).
//
// If a ModeResize gesture is active and its shiftMirrorActive state
// doesn't already match intendedShiftDown, posts WM_APPLY_SHIFT_MIRROR to
// the main thread, which performs the actual GetWindowRect/SetCursorPos/
// session-swap work (see wndProc's WM_APPLY_SHIFT_MIRROR case) -- none of
// that runs directly on the hook thread here, consistent with every other
// window-mutating action in this codebase.
//
// The session != nil / mode==ModeResize / shiftMirrorActive-mismatch check
// here is a plain, unsynchronized read of an already-loaded activeSession
// snapshot -- safe per activeSession's own "fields never altered after
// storing, RCU-style" invariant -- and exists purely to avoid spamming
// PostMessage on every auto-repeated WM_KEYDOWN while a physical Shift key
// stays held. It is NOT the authoritative check: wndProc's handler
// reloads activeSession fresh and re-verifies via
// handleShiftMirrorToggle's own comparison, so a benign race here (e.g.
// several auto-repeat events queued before the first WM_APPLY_SHIFT_MIRROR
// is processed) produces at most a couple of harmless no-op duplicate
// posts, never an incorrect or missed toggle.
func postShiftMirrorToggleIfNeeded(intendedShiftDown bool) {
	session := activeSession.Load()
	if session == nil || session.mode != ModeResize {
		return // no active resize gesture; nothing to mirror
	}
	if session.shiftMirrorActive == intendedShiftDown {
		return // already in the requested state, or OS key-repeat; avoid a redundant post
	}

	var shiftFlag uintptr
	if intendedShiftDown {
		shiftFlag = 1
	}
	if res := wincoe.PostMessage(loadMainMsgHwnd(), WM_APPLY_SHIFT_MIRROR, uintptr(session.targetWnd), shiftFlag); res.Failed() {
		logf("postShiftMirrorToggleIfNeeded: PostMessage WM_APPLY_SHIFT_MIRROR (intendedShiftDown=%v) failed: %v", intendedShiftDown, res.Err)
	}
}

func calculateResize(session *dragSession, currentPt wincoe.POINT, zone int) (x, y, w, h int32) {
	drag := session.state
	// zone is passed explicitly (rather than read from session.resizeZone
	// directly) so callers can supply a Shift-mirrored zone (see
	// oppositeResizeZone) without mutating the otherwise-immutable session
	// struct (see dragSession's/activeSession's own "fields are never
	// altered" invariant).

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

const (
	shiftScanCode = 0x1D //Ctrl key actually, not Shift (we switched to avoid an edgecase with RShift)

	shiftFlags = wincoe.KEYEVENTF_SCANCODE | wincoe.KEYEVENTF_EXTENDED // make it RCtrl aka Right Ctrl key(with that KEYEVENTF_EXTENDED) instead of LCtrl
)

var shiftTapInputs = [...]wincoe.KEYANDMOUSE_INPUT{
	{
		Type: wincoe.INPUT_KEYBOARD,
		Ki: wincoe.KEYBDINPUT{
			WScan:   shiftScanCode,
			DwFlags: shiftFlags,
		},
	},
	{
		Type: wincoe.INPUT_KEYBOARD,
		Ki: wincoe.KEYBDINPUT{
			WScan:   shiftScanCode,
			DwFlags: shiftFlags | wincoe.KEYEVENTF_KEYUP,
		},
	},
}

// injectShiftTapOnly uses the RCtrl key tap(keydown then key up) injected when gesture starts
// should prevent Start Menu from popping up, but it still needs this to be done before winkeyUP is injected
// later on when gesture's done.
// Has no known caveats that I'm aware of (except perhaps virtualbox has RCtrl as the "Host Key Combo")
//
// old bad:
// injectShiftTapOnly uses the unassigned vkE8 key to mask the Winkey.
// It is guaranteed to register as a state change, disarming the Start menu,
// even if Shift, Ctrl, or Alt are currently held down.
//
// old info(using RShift tap!):
// this way when winUP happens it won't pop up start menu
// this doesn't work in this one case only: if(in this order!) LShift was held before winkey down then eg. MMB happened(so a gesture triggers) then you release LShift, it pops startmenu!
// but it does work if you release winkey first, or if you hold winkey before shift, then you can release either and works!
//
// bad: fixed now: using "Unassigned virtual key (vkE8)"(instead of RShift) as per Gemini 3.1 Pro 's suggestion did fix the above case ^!
func injectShiftTapOnly() {
	/*
		You are correctly not setting WVk when using KEYEVENTF_SCANCODE. Windows explicitly documents that when SCANCODE is set, WVk is ignored. Mixing them leads to inconsistent behavior on some builds.
	*/
	/* note to self:
	Left Ctrl (Press): WScan: 0x1D, DwFlags: KEYEVENTF_SCANCODE
	Right Ctrl (Press): WScan: 0x1D, DwFlags: KEYEVENTF_SCANCODE | KEYEVENTF_EXTENDED

	Left Alt (Press): WScan: 0x38, DwFlags: KEYEVENTF_SCANCODE
	Right Alt (Press): WScan: 0x38, DwFlags: KEYEVENTF_SCANCODE | KEYEVENTF_EXTENDED
	  via Gemini 3.5 Flash-Lite Extended Thinking
	*/

	// //RShift tap works but if(in this order!) LShift was held before winkey down then eg. MMB happened(so a gesture triggers) then you release LShift, it pops startmenu!
	// // however, it does work if you release winkey first, or if you hold winkey before shift, then you can release either and works!
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

	//vkE8, works but bad caveat: it make the scroll move to bottom! ie. in cmd.exe window that's scrolled up before this!
	// inputs := []INPUT{
	// 	{
	// 		Type: INPUT_KEYBOARD,
	// 		Ki: KEYBDINPUT{
	// 			WVk: 0xE8, // Unassigned virtual key (vkE8)
	// 		},
	// 	},
	// 	{
	// 		Type: INPUT_KEYBOARD,
	// 		Ki: KEYBDINPUT{
	// 			WVk:     0xE8,
	// 			DwFlags: KEYEVENTF_KEYUP,
	// 		},
	// 	},
	// }

	// //let's try on UP, no effect!
	// inputs := []INPUT{
	// 	{
	// 		Type: INPUT_KEYBOARD,
	// 		Ki: KEYBDINPUT{
	// 			WVk:     0xE8,
	// 			DwFlags: KEYEVENTF_KEYUP,
	// 		},
	// 	},
	// }

	// res := procSendInput.Call(
	// 	uintptr(len(shiftTapInputs)),
	// 	uintptr(unsafe.Pointer(&shiftTapInputs[0])),
	// 	unsafe.Sizeof(shiftTapInputs[0]),
	// )
	if res := wincoe.SendInput(shiftTapInputs[:]); res.Failed() {
		logf("SendInput for injectShiftTapOnly failed: %v", res.Err)
	}
}

// this is here to avoid any heap allocations happening before each procSendInput/SendInput function call due to go:uintptrescapes!
var shitTapThenLWinKeyUp = [3]wincoe.KEYANDMOUSE_INPUT{
	shiftTapInputs[0],
	shiftTapInputs[1],
	{
		Type: wincoe.INPUT_KEYBOARD,
		Ki: wincoe.KEYBDINPUT{
			WVk:     wincoe.VK_LWIN,
			DwFlags: wincoe.KEYEVENTF_KEYUP,
		},
	},
}

// this is here to avoid any heap allocations happening before each procSendInput/SendInput function call due to go:uintptrescapes!
var shitTapThenRWinKeyUp = [3]wincoe.KEYANDMOUSE_INPUT{
	shiftTapInputs[0],
	shiftTapInputs[1],
	{
		Type: wincoe.INPUT_KEYBOARD,
		Ki: wincoe.KEYBDINPUT{
			WVk:     wincoe.VK_RWIN,
			DwFlags: wincoe.KEYEVENTF_KEYUP,
		},
	},
}

// injectShiftTapThenWinUp injects RCtrl tap(ie. down then up) [instead of the vkE8(Unassigned virtual key (vkE8)) dummy tap
// (ie. down then up) which had an edge case(it would scroll back to bottom if you were scrolled up in cmd.exe)
// [instead of RShift which had an edge case!]]
// followed by the Win UP event.
// This prevents Start Menu from poping/showing up.
func injectShiftTapThenWinUp(whichWinUp uint16) {
	/*
		You are correctly not setting WVk when using KEYEVENTF_SCANCODE. Windows explicitly documents that when SCANCODE is set, WVk is ignored. Mixing them leads to inconsistent behavior on some builds.
	*/

	var inputs *[3]wincoe.KEYANDMOUSE_INPUT
	switch whichWinUp {
	case wincoe.VK_LWIN:
		inputs = &shitTapThenLWinKeyUp
	case wincoe.VK_RWIN:
		inputs = &shitTapThenRWinKeyUp
	default:
		panic2(fmt.Sprintf("BUG: unexpected non-winkey(left or right) arg passed to injectShiftTapThenWinUp(), passed: %d", whichWinUp))
	}

	// res := procSendInput.Call(
	// 	uintptr(len(inputs)),
	// 	uintptr(unsafe.Pointer(&inputs[0])),
	// 	unsafe.Sizeof(inputs[0]),
	// )

	//Go automatically dereferences pointers to arrays when you index or slice them.
	//so wincoe.SendInput(inputs[:]) becomes wincoe.SendInput((*inputs)[:]) sugarly
	//Slicing the pointer (inputs[:]) does not allocate any heap memory.
	//Slicing the array pointer (inputs[:]) creates a lightweight slice header on the stack.
	//Slicing an array or array pointer (inputs[:]) never allocates heap memory by itself.
	//Slicing is just a zero-cost operation that creates a 24-byte struct (a Slice Header) sitting in stack memory or CPU registers.
	//Whether your code triggers a heap allocation depends entirely on where the Data pointer inside that slice header is pointing.
	//and that points to global array
	//Because inputs holds the memory address of &shitTapThenLWinKeyUp (a global array), inputs[:] constructs
	// a slice header on the stack whose Data pointer points straight to your global array in static memory.
	//When passed down to SendInput and proc.Call:
	//The slice header construction: 0 heap allocs (just stack values).
	//The target memory address: 0 heap allocs (already in global memory).
	//That’s why the entire operation achieves 0 allocations per call!
	if res := wincoe.SendInput(inputs[:]); res.Failed() {
		//Note: that inputs[:] creates a slice header pointing directly to your stack-allocated array without incurring any extra heap allocations.
		logf("SendInput for injectShiftTapThenWinUp failed: %v", res.Err)
	}
}

// mouseInputView reinterprets the union-emulating Ki field of an INPUT as a
// MOUSEINPUT. Ki is declared as the smaller KEYBDINPUT (24 bytes); INPUT
// adds an explicit trailing [8]byte padding field right after it so the
// combined space (Ki + that padding) matches MOUSEINPUT's 32 bytes -- see
// INPUT's own doc comment. This is Go's closest equivalent to reinterpreting
// a C union member, since Go has no native union type. Safety here rests
// entirely on assertStructSizes()'s startup check that unsafe.Sizeof(INPUT{})
// is exactly 40: since Go lays out struct fields in declaration order with
// no reordering, that check guarantees the 32 bytes starting at &input.Ki
// really do extend all the way to INPUT's end, with nothing beyond it. Any
// future edit to INPUT/KEYBDINPUT/MOUSEINPUT's fields that broke this
// invariant would panic there, at startup, before this function is ever
// reached.
func mouseInputView(input *wincoe.KEYANDMOUSE_INPUT) *wincoe.MOUSEINPUT {
	return (*wincoe.MOUSEINPUT)(unsafe.Pointer(&input.Ki))
}

// Package-level global array (initialized once at program startup)
var lmbClickInputs = func() [2]wincoe.KEYANDMOUSE_INPUT {
	var inputs [2]wincoe.KEYANDMOUSE_INPUT

	inputs[0].Type = wincoe.INPUT_MOUSE
	inputs[1].Type = wincoe.INPUT_MOUSE

	// Fill the union as MOUSEINPUT
	mouseInputView(&inputs[0]).DwFlags = wincoe.MOUSEEVENTF_LEFTDOWN
	mouseInputView(&inputs[1]).DwFlags = wincoe.MOUSEEVENTF_LEFTUP

	//Your inject (MOUSEEVENTF_LEFTDOWN/UP): Defaults relative (Dx/Dy=0 = no move, click at current cursor).

	return inputs
}()

func injectLMBClick() {
	if res1 := wincoe.SendInput(lmbClickInputs[:]); res1.Failed() {
		logf("SendInput mouse click failed: %v", res1.Err)
	} else {
		//TODO: remove, temp.
		logf("Used LMB click to focus, caveat: target window got a LMB click at the point where you started the window move so it could've clicked an UI button!")
	}
}

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

	// res1 := procGetSystemMetrics.Call(SM_XVIRTUALSCREEN)
	// var virtualLeft int32 = int32(res1.R1)
	// res1 = procGetSystemMetrics.Call(SM_YVIRTUALSCREEN)
	// var virtualTop int32 = int32(res1.R1)
	// res1 = procGetSystemMetrics.Call(SM_CXVIRTUALSCREEN)
	// var virtualWidth int32 = int32(res1.R1)
	// res1 = procGetSystemMetrics.Call(SM_CYVIRTUALSCREEN)
	// var virtualHeight int32 = int32(res1.R1)

	var virtualLeft int32 = wincoe.GetSystemMetrics(wincoe.SM_XVIRTUALSCREEN)
	var virtualTop int32 = wincoe.GetSystemMetrics(wincoe.SM_YVIRTUALSCREEN)
	var virtualWidth int32 = wincoe.GetSystemMetrics(wincoe.SM_CXVIRTUALSCREEN)
	var virtualHeight int32 = wincoe.GetSystemMetrics(wincoe.SM_CYVIRTUALSCREEN)

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

	inputs := []wincoe.KEYANDMOUSE_INPUT{
		{
			Type: wincoe.INPUT_MOUSE,
		},
		{
			Type: wincoe.INPUT_MOUSE,
		},
	}

	// Move to target location and press LMB.
	// m0 := (*MOUSEINPUT)(unsafe.Pointer(&inputs[0].Ki))
	m0 := mouseInputView(&inputs[0])
	m0.Dx = normalizedX
	m0.Dy = normalizedY
	m0.DwFlags = wincoe.MOUSEEVENTF_ABSOLUTE |
		wincoe.MOUSEEVENTF_VIRTUALDESK |
		wincoe.MOUSEEVENTF_MOVE |
		wincoe.MOUSEEVENTF_LEFTDOWN

	// Release LMB at the same location.
	// m1 := (*MOUSEINPUT)(unsafe.Pointer(&inputs[1].Ki))
	m1 := mouseInputView(&inputs[1])
	m1.Dx = normalizedX
	m1.Dy = normalizedY
	m1.DwFlags =
		wincoe.MOUSEEVENTF_ABSOLUTE |
			wincoe.MOUSEEVENTF_VIRTUALDESK |
			wincoe.MOUSEEVENTF_MOVE |
			wincoe.MOUSEEVENTF_LEFTUP

	//you can "save and restore" the cursor position. Since GetCursorPos and SetCursorPos are extremely fast
	// and don't involve the message queue, this will happen so quickly (sub-millisecond) that the user won't perceive the jump.

	// Save the user's current cursor position.
	//
	// SendInput with MOUSEEVENTF_ABSOLUTE physically moves the cursor.
	// We restore it immediately afterwards so the click appears to happen
	// remotely without visibly teleporting the user's mouse.
	var currentPt wincoe.POINT
	// 1. Capture current physical mouse position to restore it later
	//resGetCursorPos := procGetCursorPos.Call(uintptr(unsafe.Pointer(&currentPt)))
	resGetCursorPos := wincoe.GetCursorPos(&currentPt)
	haveOriginalCursorPos := resGetCursorPos.Succeeded()
	if !haveOriginalCursorPos {
		logf("injectLMBClickAtCoords: GetCursorPos failed, err:%v; will not restore cursor position after the injected click (would otherwise teleport it to (0,0))", resGetCursorPos.Err)
	}
	// 2. Inject the click at the original gesture location

	// res2 := procSendInput.Call(
	// 	uintptr(len(inputs)),
	// 	uintptr(unsafe.Pointer(&inputs[0])),
	// 	unsafe.Sizeof(inputs[0]),
	// )

	//if err != nil || ret != uintptr(len(inputs)) {
	if res2 := wincoe.SendInput(inputs); res2.Failed() || res2.R1 != uintptr(len(inputs)) {
		logf(
			"injectLMBClickAtCoords: SendInput injected %d/%d events: %v",
			res2.R1,
			len(inputs),
			res2.Err,
		)
	}

	if haveOriginalCursorPos {
		// 3. Teleport the mouse back to where the user had it a millisecond ago
		res3 := wincoe.SetCursorPos(currentPt.X, currentPt.Y)
		// res3 := procSetCursorPos.Call(
		// 	//When SetCursorPos(X, Y) is called, Windows expects the X coordinate to be in the RCX register and Y to be in RDX.
		// 	// Even though the arguments are 32-bit integers, Windows expects the entire 64-bit register to be properly sign-extended.
		// 	// If the upper 32 bits contain garbage or are cleared to zero when they shouldn't be, the CPU behavior or the OS wrapper can misinterpret the value.
		// 	// and that's why the 'inf' cast is needed. What inf? It's enough they're int32 cast to uintptr!
		// 	// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
		// 	uintptr(currentPt.X),
		// 	// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
		// 	uintptr(currentPt.Y),
		// )

		//if restoreRet == 0 {
		if res3.Failed() {
			logf("injectLMBClickAtCoords: SetCursorPos failed: %v", res3.Err)
		}
	}
}

func injectLMBDown() {
	// inputs := []wincoe.KEYANDMOUSE_INPUT{
	// 	{
	// 		Type: INPUT_MOUSE,
	// 		Ki:   wincoe.KEYBDINPUT{}, // union placeholder
	// 	},
	// }

	// // Fill the union as MOUSEINPUT
	// // (*MOUSEINPUT)(unsafe.Pointer(&inputs[0].Ki)).DwFlags = MOUSEEVENTF_LEFTDOWN
	// mouseInputView(&inputs[0]).DwFlags = MOUSEEVENTF_LEFTDOWN

	//Your inject (MOUSEEVENTF_LEFTDOWN): Defaults relative (Dx/Dy=0 = no move, click at current cursor).

	//SendInput is synchronous—blocks until inputs queued/processed by system. In WH_MOUSE_LL (global, synchronous chain), this blocks all mouse input until done.
	//SendInput is synchronous — blocks caller until inputs queued to system queue (not processed).

	// res1 := procSendInput.Call(
	// 	uintptr(len(inputs)),
	// 	uintptr(unsafe.Pointer(&inputs[0])),
	// 	unsafe.Sizeof(inputs[0]),
	// )

	//if err != nil || ret == 0 {

	// lmbClickInputs[:1] creates a slice of length 1 containing ONLY the LEFTDOWN event
	if res1 := wincoe.SendInput(lmbClickInputs[:1]); res1.Failed() || res1.R1 != 1 {
		logf("SendInput mouse LMBdown failed: %v", res1.Err)
	} else {
		//TODO: remove, temp.
		logf("Injected LMB down(without the up!), ret=%d err=%v", res1.R1, res1.Err)
	}
}

func getWindowPID(hwnd windows.Handle) uint32 {
	var pid uint32
	// res1 := procGetWindowThreadProcessID.Call(
	// 	uintptr(hwnd),
	// 	uintptr(unsafe.Pointer(&pid)),
	// )
	if _, res1 := wincoe.GetWindowThreadProcessId(hwnd, &pid); res1.Failed() {
		logf("getWindowPID: GetWindowThreadProcessId failed for HWND=0x%X, err: %v", hwnd, res1.Err)
	}

	return pid
}

func isMaximized(hwnd windows.Handle) bool {
	var wp wincoe.WINDOWPLACEMENT
	wp.Length = uint32(unsafe.Sizeof(wp))
	//"GetWindowPlacement is a synchronous query into USER32, but it does not send a message to the target window. It reads window state maintained by the window manager (the same data used by the shell for task switching)." -chatgpt5.2
	// so GetWindowPlacement does not block on a hung window.
	// res1 := procGetWindowPlacement.Call(
	// 	uintptr(hwnd),
	// 	uintptr(unsafe.Pointer(&wp)),
	// )
	//if r == 0 {
	if res1 := wincoe.GetWindowPlacement(hwnd, &wp); res1.Failed() {
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
		return 0, fmt.Errorf("buffer too small: %s", humanBytes(uintptr(lenb)))
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
func appendMenuChecked(hMenu windows.Handle, flags uint32, id uintptr, textStr string) {
	text := mustUTF16(textStr)
	//if res := procAppendMenu.Call(hMenu, flags, id, uintptr(unsafe.Pointer(text))); res.Failed() {
	if res := wincoe.AppendMenu(hMenu, flags, id, text); res.Failed() {
		logf("WM_MYSYSTRAY: AppendMenu failed for item with text %q, err=%v", textStr, res.Err)
	}
}

// copyUTF16Truncated copies s (as UTF-16) into dst, guaranteeing dst ends up
// null-terminated even when s must be truncated to fit. A bare
// copy(dst, windows.StringToUTF16(s)) can silently drop the terminator
// entirely if the encoded string (plus its trailing 0) is >= len(dst):
// copy() only copies min(len(dst), len(src)) elements, chopping off exactly
// the null terminator in that case and leaving Windows to read past dst's
// end into whatever struct field follows it until it happens across a
// stray zero -- garbage tray text at best, explorer.exe misbehaving at
// worst.
func copyUTF16Truncated(dst []uint16, s string) {
	if len(dst) == 0 {
		return // nothing we can safely write
	}
	encoded, err := windows.UTF16FromString(s)
	if err != nil {
		// s contains an embedded NUL byte; UTF16FromString refuses to
		// encode it. Fall back to an explicitly empty, safely
		// null-terminated string rather than leaving dst's previous
		// (possibly stale or un-terminated) contents in place.
		dst[0] = 0
		return
	}
	if len(encoded) > len(dst) {
		// Truncate the STRING content, but always keep the last slot as the
		// terminator -- never truncate away the terminator itself.
		copy(dst, encoded[:len(dst)-1])
		dst[len(dst)-1] = 0
		return
	}
	copy(dst, encoded)
}

func initTray() error {
	msgHwnd := loadMainMsgHwnd()
	if msgHwnd == 0 {
		return fmt.Errorf("main message window is not initialized")
	}
	trayIconMu.Lock()
	defer trayIconMu.Unlock()

	trayIcon.HWnd = msgHwnd //doneFIXME: need to put this in a diff. variable so it doesn't depend on systray being inited! since it's used in other things!
	trayIcon.CbSize = uint32(unsafe.Sizeof(trayIcon))
	trayIcon.UID = 1

	// Just the classic flags. No NIF_SHOWTIP needed.
	trayIcon.UFlags = wincoe.NIF_TIP | wincoe.NIF_ICON | wincoe.NIF_MESSAGE

	// res1 := procLoadIcon.Call(0, wincoe.IDI_APPLICATION)
	// res1 := wincoe.LoadIcon(0, wincoe.IDI_APPLICATION_RESOURCE)

	// // Load the default embedded icon (resource ID #1 created by go-winres)
	// if tempH, res1 := wincoe.LoadIconByID(selfHInstance, 1 /*#1 resource!*/); res1.Failed() {
	// 	logf("LoadIcon of the first resource in the .exe file failed, res: %v", res1)
	// 	//load an icon that looks like the same one from cmd.exe
	// 	if tempHfallback, res1 := wincoe.LoadIconByID(0, wincoe.IDI_APPLICATION); res1.Failed() {
	// 		return fmt.Errorf("LoadIcon IDI_APPLICATION failed, err: %w", res1.Err)
	// 	} else {
	// 		trayIcon.HIcon = tempHfallback
	// 	}
	// } else {
	// 	trayIcon.HIcon = tempH
	// }

	// Fetch the exact dimensions expected for a system tray icon
	cxSmIcon := wincoe.GetSystemMetrics(wincoe.SM_CXSMICON)
	cySmIcon := wincoe.GetSystemMetrics(wincoe.SM_CYSMICON)

	// Load the default embedded icon (resource ID #1 created by go-winres)
	// We use LoadImageByID to explicitly extract the 16x16 (or DPI scaled) variant
	// rather than letting Windows dynamically squash the 32x32 variant.
	if tempH, res1 := wincoe.LoadImageByID(selfHInstance, 1 /*#1 resource!*/, wincoe.IMAGE_ICON, cxSmIcon, cySmIcon, 0); res1.Failed() {
		logf("LoadImageByID of the first resource in the .exe file failed, res: %v", res1)

		// Fallback to the standard application icon if custom load fails
		if tempHfallback, res1 := wincoe.LoadImageByID(0, wincoe.IDI_APPLICATION, wincoe.IMAGE_ICON, cxSmIcon, cySmIcon, wincoe.LR_SHARED); res1.Failed() {
			return fmt.Errorf("LoadImageByID IDI_APPLICATION failed, err: %w", res1.Err)
		} else {
			trayIcon.HIcon = tempHfallback
		}
	} else {
		trayIcon.HIcon = tempH
	}

	trayIcon.UCallbackMessage = WM_MYSYSTRAY

	// Notice: We completely removed trayIcon.UTimeoutOrVersion = NOTIFYICON_VERSION_4
	// Leaving it as 0 defaults to the legacy behavior that handles tooltips automatically.

	tipText := selfName + " " + GetVersion()
	//copy(trayIcon.SzTip[:], windows.StringToUTF16(tipText))
	copyUTF16Truncated(trayIcon.SzTip[:], tipText)

	// 1. Add the tray icon
	//res2 := procShellNotifyIcon.Call(wincoe.NIM_ADD, uintptr(unsafe.Pointer(&trayIcon)))
	res2 := wincoe.ShellNotifyIcon(wincoe.NIM_ADD, &trayIcon)
	if res2.Failed() {
		logf("Failed to add tray icon (real error): '%v'", res2.Err)
	}

	// 2. We are done! No NIM_SETVERSION. No NIM_MODIFY.

	return nil
}

func cleanupTray() {
	if trayIcon.HWnd == 0 {
		// Never initialized or window creation failed — nothing to clean
		return
	}
	trayIconMu.Lock()
	defer trayIconMu.Unlock()

	// Use the same trayIcon struct from initTray
	trayIcon.UFlags = 0 // NIM_DELETE ignores most fields, but set to be safe

	// res1 := procShellNotifyIcon.Call(NIM_DELETE, uintptr(unsafe.Pointer(&trayIcon)))
	//ret is non-zero (success), but err can still be set
	//if ret == 0 {
	if res1 := wincoe.ShellNotifyIcon(wincoe.NIM_DELETE, &trayIcon); res1.Failed() {
		logf("Failed to delete tray icon: %v", res1.Err) // optional, for debug
	} else {
		// Zero out the struct to avoid reuse confusion
		trayIcon = wincoe.NOTIFYICONDATA{}
	}
}

func showTrayInfo(title, msg string) {
	//FIXME: this call should be rate-limited, or the callers of it should be.
	trayIconMu.Lock()
	defer trayIconMu.Unlock()

	logf("systray info: %s", msg)
	//the tray notification shows differently than a tooltip on win11 (didn't test it on anything else tho)
	//and I think you've to turn it on like(this only if you have Do Not Disturn 'on' already) System->Notifications->Set priority notifications, Add Apps(button) and pick winbollocks.exe
	// then you see it slide from the right, on top of systray, as a notifcation rectangle.
	//if you don't have Do not disturb on, it shows the same and you don't have to add it as priority notif. at all.
	// because it is already turned on in System->Notifications, Notifications from apps and other senders
	trayIcon.UFlags |= wincoe.NIF_INFO
	trayIcon.UTimeoutOrVersion = 5000 //5sec, though Win11 ignores it and uses system accessibility settings)
	// copy(trayIcon.SzInfoTitle[:], windows.StringToUTF16(title))
	// copy(trayIcon.SzInfo[:], windows.StringToUTF16(msg))
	copyUTF16Truncated(trayIcon.SzInfoTitle[:], title)
	copyUTF16Truncated(trayIcon.SzInfo[:], msg)
	//if res1 := procShellNotifyIcon.Call(wincoe.NIM_MODIFY, uintptr(unsafe.Pointer(&trayIcon))); res1.Failed() {
	if res1 := wincoe.ShellNotifyIcon(wincoe.NIM_MODIFY, &trayIcon); res1.Failed() {
		logf("Failed to update tray icon info: %v", res1.Err)
	}
}

// formatHeldInputState reports which of the tracked modifier keys
// (left/right Win, Shift, Ctrl, Alt) and mouse buttons (LMB/RMB/MMB)
// GetAsyncKeyState currently reports as held down, as a short
// comma-separated list, or "none" if nothing is held.
//
// Mouse buttons are included alongside the keyboard modifiers because the
// motivating bug (winkey+MMB / winkey+shift+MMB occasionally leaving MMB
// looking stuck down after interacting with a higher-UIPI window -- see
// resetStaleGestureFlags's doc comment) is exactly the kind of thing this
// is meant to help catch: a physically-released button GetAsyncKeyState
// still insists is down is the most direct possible confirmation something
// is actually stuck, versus one of our own internal *Swallowed flags
// merely being stale.
func formatHeldInputState() string {
	type trackedKey struct {
		vk   uintptr
		name string
	}
	keys := [...]trackedKey{
		{wincoe.VK_LWIN, "LWin"},
		{wincoe.VK_RWIN, "RWin"},
		{wincoe.VK_LSHIFT, "LShift"},
		{wincoe.VK_RSHIFT, "RShift"},
		{wincoe.VK_LCONTROL, "LCtrl"},
		{wincoe.VK_RCONTROL, "RCtrl"},
		{wincoe.VK_LMENU, "LAlt"},
		{wincoe.VK_RMENU, "RAlt"},
		{wincoe.VK_LBUTTON, "LMB"},
		{wincoe.VK_RBUTTON, "RMB"},
		{wincoe.VK_MBUTTON, "MMB"},
	}

	var held []string
	for _, k := range keys {
		if keyDown(k.vk) {
			held = append(held, k.name)
		}
	}
	if len(held) == 0 {
		return "none"
	}
	return strings.Join(held, ", ")
}

// lastTrayTooltipInputStateText caches the formatHeldInputState() text most
// recently written into a NIM_MODIFY'd tray-icon tooltip, so
// updateTrayTooltipInputStateIfChanged only calls Shell_NotifyIconW when
// the text has actually changed rather than on every single
// WM_MOUSEMOVE-over-the-tray-icon notification. Read/written exclusively
// from wndProc's WM_MYSYSTRAY handling, which only ever runs on the main
// thread -- unlike trayIcon itself (guarded by trayIconMu because
// showTrayInfo can also run from the hook thread), this var needs no
// synchronization of its own.
var lastTrayTooltipInputStateText string

// updateTrayTooltipInputStateIfChanged refreshes the tray icon's hover
// tooltip with the currently-held modifier keys/mouse buttons (see
// formatHeldInputState), but only issues a Shell_NotifyIconW(NIM_MODIFY)
// call when that text actually differs from what's already showing.
//
// Unlike showTrayInfo's SzInfo/SzInfoTitle (a one-shot balloon/toast), SzTip
// is what the shell displays for the classic hover tooltip, and the shell
// has no "ask the app for fresh text right now" callback for it -- it
// simply redisplays whatever SzTip last held from NIM_ADD/NIM_MODIFY.
// Refreshing it here, from the WM_MOUSEMOVE sub-message WM_MYSYSTRAY
// already receives whenever the cursor is over the icon (see wndProc's
// low==WM_MOUSEMOVE case), means a hover shortly after this call picks up a
// reasonably fresh snapshot instead of whatever initTray() set once at
// startup and nothing ever touched again.
//
// This deliberately builds and modifies a throwaway COPY of trayIcon for
// the NIM_MODIFY call rather than mutating the shared, persistent trayIcon
// struct's own UFlags/SzTip fields in place: showTrayInfo only ever ORs
// NIF_INFO onto trayIcon.UFlags and never clears it again (see its own doc
// comment), so if this function reused that same sticky UFlags value
// as-is, every tooltip refresh after the first showTrayInfo() call anywhere
// in the app's lifetime would carry NIF_INFO (plus whatever stale
// SzInfo/SzInfoTitle content was last set) right along with it -- silently
// re-popping that OLD balloon notification on every single hover-triggered
// tooltip refresh instead of only updating the hover text. Passing a copy
// with UFlags narrowed to just NIF_TIP sidesteps that entirely: NIM_MODIFY
// only touches the fields whose bit is set for THAT specific call, and
// CbSize/HWnd/UID (needed to identify which icon to modify at all) come
// along for free via the copy.
func updateTrayTooltipInputStateIfChanged() {
	stateText := formatHeldInputState()
	if stateText == lastTrayTooltipInputStateText {
		return // nothing changed since we last wrote it; avoid a pointless Shell_NotifyIconW call
	}
	if lastTrayTooltipInputStateText != "" {
		// Skip logging the very first refresh (transitioning from this
		// var's zero-value "") -- that's just normal startup, not a
		// genuine change worth a log line.
		logf("Tray hover tooltip input-state changed: %q -> %q", lastTrayTooltipInputStateText, stateText)
	}
	lastTrayTooltipInputStateText = stateText

	trayIconMu.Lock()
	defer trayIconMu.Unlock()

	if trayIcon.HWnd == 0 {
		return // tray icon not initialized (or already torn down)
	}

	tipUpdate := trayIcon
	tipUpdate.UFlags = wincoe.NIF_TIP
	copyUTF16Truncated(tipUpdate.SzTip[:], selfName+" "+GetVersion()+" | Held: "+stateText)

	//if res := procShellNotifyIcon.Call(NIM_MODIFY, uintptr(unsafe.Pointer(&tipUpdate))); res.Failed() {
	if res := wincoe.ShellNotifyIcon(wincoe.NIM_MODIFY, &tipUpdate); res.Failed() {
		logf("updateTrayTooltipInputStateIfChanged: Shell_NotifyIconW(NIM_MODIFY) failed to refresh tray tooltip, err: %v", res.Err)
	}
}

/* ---------------- Drag Logic ---------------- */

func startManualDrag(hwnd windows.Handle, pt wincoe.POINT, viaMissedGestureRecovery, wasMaximized bool, preRestoreRect wincoe.RECT) bool {
	if cur := activeSession.Load(); cur != nil {
		logf("unexpected startManualDrag while already having an activeSession(either drag-move or resizing) mode:%d", cur.mode)
		return false
	}

	var r wincoe.RECT
	//procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r)))
	if res1 := wincoe.GetWindowRect(hwnd, &r); res1.Failed() {
		logf("GetWindowRect on target HWND=0x%X failed for move startup, err:%v", hwnd, res1.Err)
		return false
	}

	if wasMaximized {
		// Compute a top-left so the cursor sits at the same proportional
		// position within the restored window as it had within the maximized one.
		r = alignRestoredWindowToCursor(pt, preRestoreRect, r)
		// Reposition immediately, before the first WM_MOUSEMOVE arrives.
		// if res := procSetWindowPos.Call(
		// 	uintptr(hwnd),
		// 	0, // ignored due to SWP_NOZORDER
		// 	// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
		// 	uintptr(r.Left),
		// 	// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
		// 	uintptr(r.Top),
		// 	0, 0, // ignored due to SWP_NOSIZE
		// 	SWP_NOSIZE|SWP_NOZORDER|SWP_NOACTIVATE,
		// ); res.Failed() {
		if res := wincoe.SetWindowPos(
			hwnd,
			0, // ignored due to SWP_NOZORDER
			r.Left,
			r.Top,
			0, 0, // ignored due to SWP_NOSIZE
			wincoe.SWP_NOSIZE|wincoe.SWP_NOZORDER|wincoe.SWP_NOACTIVATE,
		); res.Failed() {
			logf("SetWindowPos (post-restore alignment) on HWND=0x%X failed: %v; re-reading rect for consistent drag origin", hwnd, res.Err)
			// Re-read the rect so startPt arithmetic stays consistent with
			// wherever the window actually landed.
			//if res2 := procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r))); res2.Failed() {
			if res2 := wincoe.GetWindowRect(hwnd, &r); res2.Failed() {
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
		wasMaximizedAtStart:      wasMaximized,
		originalRect:             r,
		originalPt:               pt,
	})
	return true
}

func startDrag(hwnd windows.Handle, pt wincoe.POINT, viaMissedGestureRecovery bool) bool {
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

	var preRestoreRect wincoe.RECT
	wasMaximized := isMaximized(hwnd)
	if wasMaximized {
		// Capture the maximized rect before restoring so alignRestoredWindowToCursor
		// can compute the proportional cursor position within the restored window.
		//if res := procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&preRestoreRect))); res.Failed() {
		if res := wincoe.GetWindowRect(hwnd, &preRestoreRect); res.Failed() {
			logf("GetWindowRect (pre-restore) on HWND=0x%X failed: %v; cursor alignment after restore will be skipped", hwnd, res.Err)
			wasMaximized = false // skip alignment rather than use a zero rect
		}
		//_ = procShowWindow.Call(uintptr(hwnd), SW_RESTORE)
		_ = wincoe.ShowWindow(hwnd, wincoe.SW_RESTORE)
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
func applyFocusAndBringToFrontOnGestureStart(targetWnd windows.Handle, pt wincoe.POINT, bringToFront, focus *atomic.Bool, callerName string) {
	msgHwnd := loadMainMsgHwnd()
	if msgHwnd == 0 {
		logf("%s: applyFocusAndBringToFrontOnGestureStart failed due to mainMsgHwnd is 0", callerName)
		return
	}
	if bringToFront.Load() {
		// Post a dedicated bring-to-front message rather than routing through
		// the move channel, which would be coalesced away by move events for
		// the same HWND.
		//if res := wincoe.PostMessage(uintptr(mainMsgHwnd), WM_BRING_TO_FRONT, uintptr(targetWnd), 0); res.Failed() {
		if res := wincoe.PostMessage(msgHwnd, WM_BRING_TO_FRONT, uintptr(targetWnd), 0); res.Failed() {
			logf("%s: PostMessage WM_BRING_TO_FRONT for HWND=0x%X failed: %v", callerName, targetWnd, res.Err)
		}
	}
	if focus.Load() && !isWindowForeground(targetWnd) { //TODO: should I move this in startDrag?
		//doneFIXME: should probably embed the targetWnd into the message instead of using whichever the current dragged window is, otherwise it might miss focusing the clicked window due to delays in processing if a new window was quick-engouh clicked since!

		if res := wincoe.PostMessage(
			msgHwnd,
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
func tryBeginMoveGestureAt(pt wincoe.POINT, viaMissedGestureRecovery bool) (started, bypassed bool) {
	wantTargetWnd, res1 := wincoe.RootWindowFromPoint(pt)
	if wantTargetWnd == 0 {
		logf("Invalid window(tryBeginMoveGestureAt:RootWindowFromPoint res:%v), window-move gesture skipped but LMB eaten and start menu will still be prevented(now even if you LMB on a higher integrity eg. admin window before you release winkey)", res1)
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
	// session := activeSession.Load()
	// if session == nil {
	// 	panic("bad coding: nil session after startDrag returned true")
	// }
	applyFocusAndBringToFrontOnGestureStart(wantTargetWnd, pt, &bringToFrontOnDrag, &focusOnDrag, "tryBeginMoveGestureAt")
	return true, false
}

// tryBeginResizeGestureAt is tryBeginMoveGestureAt's ModeResize counterpart,
// used by both the real WM_RBUTTONDOWN handler and the missed-gesture
// recovery path from WM_MOUSEMOVE. See tryBeginMoveGestureAt's doc comment
// for the (started, bypassed) return-value contract.
func tryBeginResizeGestureAt(pt wincoe.POINT, viaMissedGestureRecovery bool) (started, bypassed bool) {
	wantTargetWnd, res0 := wincoe.RootWindowFromPoint(pt)
	if wantTargetWnd == 0 {
		logf("Invalid window(tryBeginMoveGestureAt:RootWindowFromPoint res:%v), window-resize gesture skipped but RMB eaten and start menu will still be prevented(now even if you RMB on a higher integrity eg. admin window before you release winkey)", res0)
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
	var preRestoreRect wincoe.RECT
	wasMaximized := isMaximized(wantTargetWnd)

	if wasMaximized {
		// if res := procGetWindowRect.Call(uintptr(wantTargetWnd), uintptr(unsafe.Pointer(&preRestoreRect))); res.Failed() {
		if res := wincoe.GetWindowRect(wantTargetWnd, &preRestoreRect); res.Failed() {
			logf("GetWindowRect (pre-restore) on HWND=0x%X failed: %v; cursor alignment after restore will be skipped", wantTargetWnd, res.Err)
			wasMaximized = false // skip alignment rather than use a zero rect
		}

		// Restore the window first so the resize starts from, and is measured
		// against, the non-maximized rect. Without this the OS leaves the window
		// in a mixed state (visually resized but still flagged as maximized).
		//_ = procShowWindow.Call(uintptr(wantTargetWnd), SW_RESTORE)
		_ = wincoe.ShowWindow(wantTargetWnd, wincoe.SW_RESTORE)
	}

	var r wincoe.RECT
	//res1 := procGetWindowRect.Call(uintptr(wantTargetWnd), uintptr(unsafe.Pointer(&r)))
	if res1 := wincoe.GetWindowRect(wantTargetWnd, &r); res1.Failed() {
		logf("GetWindowRect on target HWND=0x%X failed(ret is 0) for resize startup, err:%v", wantTargetWnd, res1.Err)
		return false, false
	}

	// Reposition the restored window under the cursor BEFORE setting up the resize session
	if wasMaximized {
		r = alignRestoredWindowToCursor(pt, preRestoreRect, r)
		// if res := procSetWindowPos.Call(
		// 	uintptr(wantTargetWnd),
		// 	0, // ignored due to SWP_NOZORDER
		// 	// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
		// 	uintptr(r.Left),
		// 	// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
		// 	uintptr(r.Top),
		// 	0, 0, // ignored due to SWP_NOSIZE
		// 	SWP_NOSIZE|SWP_NOZORDER|SWP_NOACTIVATE,
		if res := wincoe.SetWindowPos(
			wantTargetWnd,
			0, // ignored due to SWP_NOZORDER
			r.Left,
			r.Top,
			0, 0, // ignored due to SWP_NOSIZE
			wincoe.SWP_NOSIZE|wincoe.SWP_NOZORDER|wincoe.SWP_NOACTIVATE,
		); res.Failed() {
			logf("SetWindowPos (post-restore alignment) on HWND=0x%X failed: %v; re-reading rect for consistent resize origin", wantTargetWnd, res.Err)
			//if res2 := procGetWindowRect.Call(uintptr(wantTargetWnd), uintptr(unsafe.Pointer(&r))); res2.Failed() {
			if res2 := wincoe.GetWindowRect(wantTargetWnd, &r); res2.Failed() {
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
		wasMaximizedAtStart:      wasMaximized,
		originalRect:             r,
		originalPt:               pt,
	})
	// session := activeSession.Load() //weird way to do this Claude Sonnet 5 Extra Thinking (yes Extra this time), because who needs DRY!?!
	// if session == nil {
	// 	panic("bad coding: nil session after storing new resize session")
	// }
	applyFocusAndBringToFrontOnGestureStart(wantTargetWnd, pt, &bringToFrontOnResize, &focusOnResize, "tryBeginResizeGestureAt")
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
func tryPerformMMBGestureAt(pt wincoe.POINT, shiftDown bool) (started, bypassed bool) {
	var hwnd windows.Handle
	useTracking := unfocusSentToBackWindow.Load()

	// targetWasFocusedBeforeSendToBack records whether hwnd held keyboard
	// focus at the moment this gesture fired, computed unconditionally
	// (independent of useTracking/unfocusSentToBackWindow) so it stays
	// correct even if that setting gets toggled between now and whenever
	// handleActualMoveOrResize actually processes the enqueued data below.
	// Consumed via data.UnfocusAfterSendToBack: sending a window that was
	// NOT already focused to the back of the Z-order must never disturb
	// whatever unrelated window actually holds focus (SWP_NOACTIVATE
	// already guarantees the OS itself won't touch it) -- only a window
	// that genuinely held focus before being sent back needs its focus
	// explicitly reconciled afterward.
	var targetWasFocusedBeforeSendToBack bool

	if !shiftDown {
		// winkey + MMB -> send window under cursor to bottom of Z-order
		var res0 wincoe.WinResult
		hwnd, res0 = wincoe.RootWindowFromPoint(pt) // window under cursor
		if hwnd != 0 {
			targetWasFocusedBeforeSendToBack = isWindowForeground(hwnd)
			if useTracking && targetWasFocusedBeforeSendToBack {
				// ONLY remember this window if unfocusSentToBackWindow is true AND it currently has focus
				lastSentToBackHwnd.Store(uintptr(hwnd))
			}
		} else if res0.Failed() {
			logf("tryPerformMMBGestureAt:RootWindowFromPoint failed, res:%v", res0)
		}
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
		// winkey + shift + MMB -> bring back the originally sent-to-back window
		if useTracking {
			// Atomically consume (swap to 0) the saved handle so it is only restored once
			if saved := lastSentToBackHwnd.Swap(0); saved != 0 {
				candidate := windows.Handle(saved)
				// Verify the window was not closed while in the background
				if wincoe.IsWindow(candidate) {
					hwnd = candidate
					logf("got one 0x%X", hwnd)
				}
			}

			// When tracking is enabled and no valid saved window exists, stop here.
			// Do NOT fall back to GetForegroundWindow(), because the active foreground
			// window is currently sitting on TOP of the Z-order!
			if hwnd == 0 {
				logf("winkey+shift+MMB: no valid previously focused sent-to-back window to restore (or it's focused already from last time)")
				return false, false
			}
		} else {
			// Fallback mode: unfocusSentToBackWindow is FALSE, meaning the window sent to back
			// retained foreground focus. Thus, GetForegroundWindow() points to that window.
			fg := getForegroundWindow()
			if fg == 0 {
				logf("Couldn't get currently focused window for winkey+shift+MMB gesture ergo aborting attempt")
				return false, false
			}
			hwnd = fg
		}

		// // Fallback: if unfocusSentToBackWindow is false, or no window has been saved yet
		// if hwnd == 0 {
		// 	// Fallback if we haven't sent anything to the back yet
		// 	res1 := procGetForegroundWindow.Call() // whichever the currently focused window is, wherever it is
		// 	// procGetForegroundWindow is bound with wincoe.CheckNone (GetForegroundWindow has no
		// 	// real failure signal beyond returning NULL), so res1.Failed() can never be true here;
		// 	// check R1 directly, matching GetForegroundWindow's documented NULL-on-failure contract.
		// 	if res1.R1 == 0 {
		// 		logf("Couldn't get currently focused window for the purposes of bringing it to front for winkey+shift+MMB gesture ergo aborting attempt, err=%v callStatus=%v r1=%v", res1.Err, res1.CallStatus, res1.R1)
		// 		return false, false
		// 	}
		// 	hwnd = windows.Handle(res1.R1)
		// }
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
		if useTracking {
			lastSentToBackHwnd.Store(0)
		}
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
			data.InsertAfter = wincoe.HWND_BOTTOM
			data.Flags = wincoe.SWP_NOMOVE | wincoe.SWP_NOSIZE | wincoe.SWP_NOACTIVATE
			// SWP_NOACTIVATE above means hwnd keeps keyboard focus even
			// though it's now behind everything else -- but only if hwnd
			// actually held focus to begin with (see
			// targetWasFocusedBeforeSendToBack above). Sending an
			// already-unfocused "side window" to the back changes nothing
			// about focus at all, so only flag this entry for
			// handleActualMoveOrResize's post-move refocus reconciliation
			// when there's genuinely something to reconcile.
			data.UnfocusAfterSendToBack = targetWasFocusedBeforeSendToBack
		} else {
			// winkey + shift + MMB → bring focused window to top

			// shift is down too, so winkey_DOWN and shiftDOWN and LMB are down
			// but no other modifiers like ctrl or alt are down
			// then we start the bring focused window to front gesture:
			data.InsertAfter = wincoe.HWND_TOP
			// Always request SWP_NOACTIVATE here and handle activation
			// ourselves afterward via forceForeground() (see
			// FocusAfterBringToFront) instead of relying on SetWindowPos's
			// own implicit activation: when hwnd isn't already the
			// foreground window (exactly the case unfocusSentToBackWindow
			// creates -- some OTHER window now holds focus after an
			// earlier send-to-back), that implicit path is gated by the
			// same foreground-lock/focus-stealing-prevention rules as a
			// bare SetForegroundWindow call, and empirically fails to
			// steal focus AND to fully promote hwnd to the very top of the
			// Z order -- silently doing neither. See FocusAfterBringToFront's
			// doc comment on WindowMoveData.
			data.Flags = wincoe.SWP_NOMOVE | wincoe.SWP_NOSIZE | wincoe.SWP_NOACTIVATE
			data.FocusAfterBringToFront = true
		}
		data.Hwnd = hwnd // window under cursor
		data.X = 0       // int32, full range
		data.Y = 0
		enqueueMoveOrResize(data, "MMB gesture (winkey+MMB or winkey+shift+MMB, direct or recovered)")
	} else { // endif every 10ms or more, else drop it
		if useTracking {
			lastSentToBackHwnd.Store(0)
		}
		droppedMoveOrResizeEvents.Add(1) //TODO: use diff. one to keep track of drops due to too-fast thus not-queued
	}

	return true, false
}

func keyDown(vk uintptr) bool {
	return wincoe.IsKeyDown(int(vk))
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
	msgHwnd := loadMainMsgHwnd()
	/*
		The Problem: If you call it in the hook, you are releasing capture on the Hook Thread. But window capture is thread-specific.
		If your SetCapture was originally called by the Main Thread (which is usually where windows and UI live),
		calling ReleaseCapture from the Hook Thread might not work the way you expect, or could lead to an inconsistent state where the OS
		thinks Thread A has it but Thread B tried to kill it.

		actually it is my hook thread that calls SetCapture in 2 places one for move and one for resize!
	*/
	if releaseCapture {
		if msgHwnd != 0 {
			if res := wincoe.PostMessage(msgHwnd, WM_DO_RELEASE_CAPTURE, 0, 0); res.Failed() {
				logf("softReset: PostMessage WM_DO_RELEASE_CAPTURE failed: %v", res.Err)
			}
		} else {
			// fallback, but should rarely hit
			logf("mainMsgHwnd is 0 in softReset when trying to send a WM_DO_RELEASE_CAPTURE, falling back to calling ReleaseCapture now!")
			if res := wincoe.ReleaseCapture(); res.Failed() {
				logf("softReset: fallback ReleaseCapture failed: %v", res.Err)
			}
		}
	}

	// Instead of calling hideOverlay() synchronously on the hook thread,
	// post it asynchronously to your main thread's message window loop.
	if overlayIsShowing.CompareAndSwap(true, false) {
		//hideOverlay() //doneFIXME: move this to wndProc ! else u hit stutter7 occasionally!
		// Instead of calling hideOverlay() synchronously on the hook thread,
		// post it asynchronously to your main thread's message window loop.
		if msgHwnd != 0 {
			if res := wincoe.PostMessage(msgHwnd, WM_HIDE_OVERLAY, 0, 0); res.Failed() {
				logf("softReset: PostMessage WM_HIDE_OVERLAY failed: %v", res.Err)
			}
		} else {
			logf("softReset: unexpected: failed to hideOverlay due to mainMsgHwnd being 0")
		}
	}
} //softReset

func hardReset(releaseCapture bool) {
	var winDown bool = keyDown(wincoe.VK_LWIN) || keyDown(wincoe.VK_RWIN)
	if winGestureUsed.Load() && winDown {
		injectShiftTapOnly() // this way when winUP happens it won't pop up start menu
		//alreadydoingitTODO: inject shift tap at the time gesture is detected!
		winGestureUsed.Store(false)
	}
	softReset(releaseCapture)
}

// cancelActiveGesture undoes an in-progress winkey+LMB/RMB drag-move or
// drag-resize gesture -- triggered by pressing ESC while it's still active
// (see tryCancelActiveGestureViaEsc and wndProc's WM_CANCEL_GESTURE case) --
// by restoring session.targetWnd to session.state.startRect, re-maximizing
// it if it was maximized when the gesture began
// (session.wasMaximizedAtStart), and moving the cursor back to
// session.state.startPt so the whole action looks like it never happened.
// Must run on the main thread (it calls SetWindowPos/ShowWindow/SetCursorPos
// directly) -- see tryCancelActiveGestureViaEsc's doc comment for why the
// hook thread only ever posts a message here instead of calling this
// directly.
func cancelActiveGesture(session *dragSession) {
	target := session.targetWnd
	// Deliberately session.originalRect, NOT session.state.startRect: the
	// Shift-mirror accelerator (see handleShiftMirrorToggle) replaces
	// state.startRect/startPt every time Shift is toggled mid-resize, but
	// originalRect/originalPt are set once at gesture start and never
	// touched again -- so ESC always undoes all the way back to how the
	// window looked before this gesture began, no matter how many mirror
	// toggles happened since.
	r := session.originalRect
	w := r.Right - r.Left
	h := r.Bottom - r.Top

	logf("Cancelling in-progress %v gesture on HWND=0x%X via ESC; restoring to original rect (%d,%d)-(%d,%d), wasMaximizedAtStart=%v",
		session.mode, target, r.Left, r.Top, r.Right, r.Bottom, session.wasMaximizedAtStart)

	if res := wincoe.SetWindowPos(target, 0, r.Left, r.Top, w, h,
		wincoe.SWP_NOZORDER|wincoe.SWP_NOACTIVATE,
	); res.Failed() {
		logf("cancelActiveGesture: SetWindowPos (restore original rect) on HWND=0x%X failed: %v", target, res.Err)
	}

	if session.wasMaximizedAtStart {
		// See startDrag/tryBeginResizeGestureAt's identical
		// "_ = wincoe.ShowWindow(hwnd, wincoe.SW_RESTORE)" pattern -- the
		// return value only reports the window's PRIOR visibility state,
		// not whether the maximize request itself succeeded, so there's
		// nothing meaningful to check or log here.
		_ = wincoe.ShowWindow(target, wincoe.SW_MAXIMIZE)
	}

	if res := wincoe.SetCursorPos(session.originalPt.X, session.originalPt.Y); res.Failed() {
		logf("cancelActiveGesture: SetCursorPos (restore cursor to gesture-start position) failed: %v", res.Err)
	}

	// End the gesture entirely -- ESC is a hard "undo and stop", not merely
	// a snap-back that keeps tracking further mouse movement.
	softReset(true)
}

// tryCancelActiveGestureViaEsc checks whether a winkey+LMB/RMB drag-move or
// drag-resize gesture is currently in progress and, if so, posts
// WM_CANCEL_GESTURE to the main thread to undo it (see cancelActiveGesture)
// and reports true so the caller (keyboardProc) swallows the originating
// ESC key event. Returns false -- ESC should pass through normally -- if no
// ModeMove/ModeResize session is active.
//
// Deliberately does no Win32 UI work itself: this runs on the hook thread
// (keyboardProc), and SetWindowPos/ShowWindow/SetCursorPos must only ever
// be called from the main thread, same as every other gesture-driven
// window change in this codebase (see e.g. handleActualMoveOrResize).
func tryCancelActiveGestureViaEsc() bool {
	session := activeSession.Load()
	if session == nil {
		return false // no active drag/resize gesture to cancel; let ESC through
	}

	msgHwnd := loadMainMsgHwnd()
	if msgHwnd == 0 {
		logf("tryCancelActiveGestureViaEsc: mainMsgHwnd is 0, can't post WM_CANCEL_GESTURE; letting ESC through instead")
		//FIXME: re-check this, doesn't seem like a good idea to let it thru!
		return false
	}

	if res := wincoe.PostMessage(msgHwnd, WM_CANCEL_GESTURE, uintptr(session.targetWnd), 0); res.Failed() {
		logf("tryCancelActiveGestureViaEsc: PostMessage WM_CANCEL_GESTURE failed: %v; letting ESC through instead", res.Err)
		//FIXME: re-check this, doesn't seem like a good idea to let it thru!
		return false
	}
	return true
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

	var wc wincoe.WNDCLASSEX
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	wc.LpfnWndProc = windows.NewCallback(overlayWndProc)
	wc.LpszClassName = className
	wc.HInstance = selfHInstance
	// Add shadow/background if desired, but we'll paint it

	if res1b := wincoe.RegisterClassEx(&wc); res1b.Failed() {
		return fmt.Errorf("RegisterClassEx failed in initOverlay(), err: %w", res1b.Err)
	} else {
		overlayClassRegistered.Store(true)
	}

	// res2 := procCreateWindowEx.Call(
	// 	WS_EX_LAYERED|WS_EX_TRANSPARENT|WS_EX_TOOLWINDOW|WS_EX_TOPMOST,
	// 	uintptr(unsafe.Pointer(className)),
	// 	0,
	// 	WS_POPUP,
	// 	0, 0, 400, 100, // Size will be updated dynamically
	// 	0, 0,
	// 	uintptr(wc.HInstance),
	// 	0,
	// )

	if res2 := wincoe.CreateWindowEx(
		wincoe.WS_EX_LAYERED|wincoe.WS_EX_TRANSPARENT|wincoe.WS_EX_TOOLWINDOW|wincoe.WS_EX_TOPMOST,
		className,
		nil,
		wincoe.WS_POPUP,
		0, 0, 400, 100, // Size will be updated dynamically
		0, 0,
		wc.HInstance,
		nil,
	); res2.Failed() {
		overlayHwnd = 0
		return fmt.Errorf("failed procCreateWindowEx() in initOverlay(), err: %w", res2.Err)
	} else {
		overlayHwnd = windows.Handle(res2.R1 /*aka hwndRaw*/)
	}

	const TransparentKey = wincoe.ColorMagenta
	const defaultOverlayAlpha = 220 // ~86% opacity
	// Set Magenta (0x00FF00FF) as the transparent color key, and 200/255 opacity for the rest
	//if resLayered := procSetLayeredWindowAttributes.Call(uintptr(overlayHwnd), TransparentKey, defaultOverlayAlpha, LWA_COLORKEY|LWA_ALPHA); resLayered.Failed() {
	if resLayered := wincoe.SetLayeredWindowAttributes(overlayHwnd, TransparentKey, defaultOverlayAlpha,
		wincoe.LWA_COLORKEY|wincoe.LWA_ALPHA); resLayered.Failed() {
		logf("initOverlay: SetLayeredWindowAttributes failed, err: %v; overlay will lack its transparent color-key/opacity, continuing anyway", resLayered.Err)
	}

	// Create our reusable GDI brushes once
	// res3 := procGdiCreateSolidBrush.Call(TransparentKey)
	if brushHandle, res3 := wincoe.GdiCreateSolidBrush(TransparentKey); res3.Failed() {
		magentaBrush = 0
		return fmt.Errorf("failed procGdiCreateSolidBrush() in initOverlay(), err: %w", res3.Err)
	} else {
		magentaBrush = brushHandle // windows.Handle(res3.R1 /*aka hMag*/)
	}

	// res4 := procGdiCreateSolidBrush.Call(wincoe.ColorBlack)
	if brushHandle, res4 := wincoe.GdiCreateSolidBrush(wincoe.ColorBlack); res4.Failed() {
		blackBrush = 0
		return fmt.Errorf("failed procGdiCreateSolidBrush() in initOverlay(), err: %w", res4.Err)
	} else {
		blackBrush = brushHandle //windows.Handle(res4.R1 /*aka hBlk*/)
	}

	return nil
}

func overlayWndProc(hwnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr /*aka LRESULT*/ {
	if msg == wincoe.WM_PAINT {
		var ps wincoe.PAINTSTRUCT
		// res1 := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		hdc, res1 := wincoe.BeginPaint(hwnd, &ps)
		if res1.Failed() {
			logf("WM_PAINT in overlayWndProc, BeginPaint() failed, err: %v, ignoring the rest of the paint.", res1.Err)
			return 0 //handled; BeginPaint itself failed, so there's no DC/update region for EndPaint to release.
		}
		//hdc := res1.R1

		// EndPaint MUST run no matter which step below fails, or the update
		// region never gets validated/cleared and Windows will keep
		// re-posting WM_PAINT the instant the queue is idle -- a 100%-CPU
		// repaint storm on whichever thread pumps this window's messages.
		defer wincoe.EndPaint(hwnd, &ps) // it never fails and never sets GetLastError() hence it's void return!
		// func() {
		// 	//if res7 := procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps))); res7.Failed() {
		// 	if res7 := wincoe.EndPaint(hwnd, &ps); res7.Failed() {
		// 		logf("WM_PAINT in overlayWndProc, EndPaint() failed, err: %v", res7.Err)
		// 	}
		// }()

		var rect wincoe.RECT
		// res2 := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rect)))
		// if res2.Failed() {
		// 	logf("WM_PAINT in overlayWndProc, GetWindowRect() failed, err: %v, ignoring the rest of the paint.", res2.Err)
		// 	return 0 //handled
		// }
		// rect.Right -= rect.Left
		// rect.Left = 0
		// rect.Bottom -= rect.Top
		// rect.Top = 0

		//res2 := procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rect)))
		res2 := wincoe.GetClientRect(hwnd, &rect)
		if res2.Failed() {
			logf("WM_PAINT in overlayWndProc, GetClientRect() failed, err: %v, ignoring the rest of the paint.", res2.Err)
			return 0 //handled
		}

		// 1. Fill background with our global Magenta brush (Transparent Key)
		//res3 := procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rect)), uintptr(magentaBrush))

		if res3 := wincoe.FillRect(hdc, &rect, magentaBrush); res3.Failed() {
			logf("WM_PAINT in overlayWndProc, FillRect() failed, err: %v, ignoring the rest of the paint.", res3.Err)
			return 0 //handled
		}

		// 2. Draw black text box background for visibility with our global Black brush
		//res3 := procFillRect.Call(hdc, uintptr(unsafe.Pointer(&rect)), uintptr(blackBrush))

		if res3 := wincoe.FillRect(hdc, &rect, blackBrush); res3.Failed() {
			logf("WM_PAINT in overlayWndProc, FillRect() failed, err: %v, ignoring the rest of the paint.", res3.Err)
			return 0 //handled
		}

		// 3. Draw Text
		// Green text
		if res4 := wincoe.GdiSetTextColor(hdc, wincoe.ColorGreen); res4.Failed() {
			logf("WM_PAINT in overlayWndProc, GdiSetTextColor() failed, err: %v, ignoring the rest of the paint.", res4.Err)
			return 0 //handled
		}
		// TRANSPARENT background for text
		if res5 := wincoe.GdiSetBkMode(hdc, wincoe.SetBkMode_TRANSPARENT); res5.Failed() {
			logf("WM_PAINT in overlayWndProc, GdiSetBkMode() failed, err: %v, ignoring the rest of the paint.", res5.Err)
			return 0 //handled
		}

		textPtr := mustUTF16(overlayText)
		// res6 := procDrawText.Call(hdc, uintptr(unsafe.Pointer(textPtr)), ^uintptr(0),
		// 	uintptr(unsafe.Pointer(&rect)), 0x24) // DT_CENTER | DT_VCENTER | DT_SINGLELINE
		// if res6.Failed() {
		if res6 := wincoe.DrawText(
			hdc,
			textPtr,
			wincoe.DrawTextLengthNullTerminated, // or -1
			&rect,
			wincoe.DT_CENTER|wincoe.DT_VCENTER|wincoe.DT_SINGLELINE,
		); res6.Failed() {
			logf("WM_PAINT in overlayWndProc, DrawText() failed, err: %v, ignoring the rest of the paint.", res6.Err)
			return 0 //handled
		}

		return 0 //handled; the deferred EndPaint above runs regardless of how we got here.
	} //if WM_PAINT

	//res8 := procDefWindowProc.Call(hwnd, uintptr(msg), wParam, lParam) //DefWindowProcW returns LRESULT.
	res8 := wincoe.DefWindowProc(hwnd, msg, wParam, lParam) //DefWindowProcW returns LRESULT. and is CheckNone
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

	// res1 := procSetWindowPos.Call( //doneTODO: handle errors/returns here
	// 	uintptr(overlayHwnd),
	// 	HWND_TOPMOST,
	// 	// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
	// 	uintptr(ox),
	// 	// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
	// 	uintptr(oy),
	// 	300, 50,
	// 	SWP_NOACTIVATE|0x0040, // SWP_SHOWWINDOW
	// )

	overlayIsShowing.Store(true) // Mark as showing before we ask Windows to show it

	//Combining SWP_SHOWWINDOW and SWP_NOACTIVATE will successfully unhide (display) a window that was hidden using SW_HIDE,
	//  and it will do so without stealing input focus or bringing the window to the foreground.
	//SWP_SHOWWINDOW (0x0040): Overrides the hidden state, turns the WS_VISIBLE style back on, and makes the window appear on screen.
	//SWP_NOACTIVATE (0x0010): Tells Windows: "Show the window, but do not give it keyboard focus and do not make it the active foreground window."
	if res1 := wincoe.SetWindowPos(overlayHwnd, wincoe.HWND_TOPMOST, ox, oy, 300, 50, wincoe.SWP_NOACTIVATE|wincoe.SWP_SHOWWINDOW); res1.Failed() {
		logf("in updateOverlay, failed to SetWindowPos of overlayHwnd:0x%X, err:%v, callStatus:%v", overlayHwnd, res1.Err, res1.CallStatus)
	}

	// Force redraw, well the redraw is queued, whenever Windows gets around to it.
	// res2 := procInvalidateRect.Call(uintptr(overlayHwnd), 0, 1)
	if res2 := wincoe.InvalidateRect(overlayHwnd, nil, true); res2.Failed() {
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
		//res3 := procUpdateWindow.Call(uintptr(overlayHwnd))

		if res3 := wincoe.UpdateWindow(overlayHwnd); res3.Failed() { // <--- Forces immediate synchronous repaint
			logf("in updateOverlay, failed to UpdateWindow aka repaint of overlayHwnd:0x%X, err:%v, callStatus:%v", overlayHwnd, res3.Err, res3.CallStatus)
		}
	}
}

func hideOverlay() {
	if overlayHwnd != 0 {
		//_ = procShowWindow.Call(uintptr(overlayHwnd), SW_HIDE)
		wasShown := wincoe.ShowWindow(overlayHwnd, wincoe.SW_HIDE)
		if !wasShown {
			logf("hideOverlay() executed while overlay was already hidden!")
		}
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
func alignRestoredWindowToCursor(cursorPt wincoe.POINT, maxRect, normRect wincoe.RECT) wincoe.RECT {
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
	return wincoe.RECT{
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
	var r wincoe.RECT
	//if res := procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r))); res.Failed() {
	if res := wincoe.GetWindowRect(hwnd, &r); res.Failed() {
		logf("isWindowFullscreenOnMonitor:GetWindowRect failed, err:%v", res.Err)
		return false
	}

	// 2. Get the Monitor information
	// res := procMonitorFromWindow.Call(uintptr(hwnd), MONITOR_DEFAULTTONEAREST)
	// hMon := res.R1
	hMon := wincoe.MonitorFromWindow(hwnd, wincoe.MONITOR_DEFAULTTONEAREST)
	if hMon == 0 {
		logf("isWindowFullscreenOnMonitor:MonitorFromWindow says no monitor!")
		return false
	}
	var mi wincoe.MONITORINFO
	//mi.CbSize = uint32(unsafe.Sizeof(mi))//if uninited it will be inited to this by wincoe.GetMonitorInfo
	// if res2 := procGetMonitorInfo.Call(hMon, uintptr(unsafe.Pointer(&mi))); res2.Failed() {
	if res2 := wincoe.GetMonitorInfo(hMon, &mi); res2.Failed() {
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
	if style, err := getWindowLongPtr(hwnd, wincoe.GWL_STYLE); err != nil {
		logf("isWindowFullscreenOnMonitor:GetWindowLongPtr GWL_STYLE failed: %v", err)
		// Fallback: if style check fails but it fills the screen, err on the side of caution
		return true
	} else {
		// If it fills the screen AND has a caption, it's just a normal maximized window
		// (likely bleeding over the edges due to an auto-hidden taskbar).
		if (style & wincoe.WS_CAPTION) == wincoe.WS_CAPTION {
			//Checking GetWindowPlacement's ShowCmd (via
			// isMaximized, already used elsewhere in this file) is more reliable
			// than inferring from the WS_CAPTION style bit: some borderless-fullscreen
			// games/apps hide their frame via other means (SetWindowRgn, DWM
			// composition attributes) while still leaving WS_CAPTION set in their
			// style, which would make a WS_CAPTION-only check false-negative --
			// failing to bypass gestures for exactly the games this feature targets.
			// A window that reached SW_MAXIMIZE natively (double-click titlebar,
			// Win+Up, snap-to-maximize) is the "just a normal maximized window"
			// case (its rect often bleeds slightly past the monitor edges due to
			// the invisible resize border, which isSpanningMonitor's >=/<= already
			// tolerates); anything else that still spans the monitor is treated as
			// fullscreen (exclusive or borderless), regardless of its style bits.
			return !isMaximized(hwnd)
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
	//res1 := procGetForegroundWindow.Call()
	hwnd := wincoe.GetForegroundWindow() //it's CheckNone so it never fails via res1.Failed()
	if hwnd == 0 {
		logf("Failed to GetForegroundWindow, got 0 aka \"no window currently holds foreground status\"")
		//return windows.Handle(0)
		//fallthru - returns 0 anyway
	}
	return hwnd
}

// aka in window in my own process?
func isOwnWindow(hwnd windows.Handle) bool {
	if hwnd == 0 {
		return false
	}

	var pid uint32
	// res1 := procGetWindowThreadProcessID.Call(
	// 	uintptr(hwnd),
	// 	uintptr(unsafe.Pointer(&pid)),
	// )
	//if r1 == 0 {
	if _, res1 := wincoe.GetWindowThreadProcessId(hwnd, &pid); res1.Failed() {
		return false
	}

	return pid == selfPID //windows.GetCurrentProcessId()
}

// FIXME: make these two funcs be one and return two bools: (samePID, sameTID) and sameTID would be false if samePID is false!

// is window in the same thread ID as the caller thread ID (could still be two diff. processes tho!)
func isInSameThreadID(hwnd windows.Handle) bool {
	var pid uint32
	// res1 := procGetWindowThreadProcessID.Call(
	// 	uintptr(hwnd),
	// 	uintptr(unsafe.Pointer(&pid)),
	// )
	// if tid == 0 {
	tid, res1 := wincoe.GetWindowThreadProcessId(hwnd, &pid)
	if res1.Failed() {
		return false
	}
	return tid /*aka tid aka thread id*/ == windows.GetCurrentThreadId()
	// // #nosec G115 -- safe: Win32 Thread IDs are 32-bit DWORDs
	// return uint32(res1.R1 /*aka tid aka thread id*/) == windows.GetCurrentThreadId()
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

func getWindowLongPtr(hwnd windows.Handle, index int32) (uintptr, error) {
	if hwnd == 0 {
		return 0, fmt.Errorf("getWindowLongPtr: hwnd is 0")
	}

	// //to prevent preemption and running of another goroutine between procSetLastError and procGetWindowLongPtrW, must LockOsThread()
	// runtime.LockOSThread()
	// defer runtime.UnlockOSThread()
	// /*
	// 		The documented pattern is:

	// 	Clear last error.
	// 	Call GetWindowLongPtrW.
	// 	If return value is 0, call GetLastError.
	// 	If error is non-zero → failure.
	// 	If error is zero → success, because the actual value was legitimately zero.
	// */

	// // Clear last error so we can detect real failure
	// //windows.SetLastError(0)
	// // Clear last error so we can detect real failure
	// _ = procSetLastError.Call(0) //You scrubbed the thread's error state clean. Since lasterror is stored in TLS aka thread local storage
	// //windows.SetLastError(0)

	//as per https://github.com/golang/go/issues/41220 there's no need to call setlasterror because it happens automatically!
	// res1 := procGetWindowLongPtrW.Call( //it's a CheckNone so res1.Err is nil
	// 	uintptr(hwnd),
	// 	// #nosec G115 -- safe: Win32 ABI expects negative offsets to be cast to uintptr
	// 	uintptr(index),
	// ) //Go executes the C code and atomically grabs LastError before anything else can touch it. as the 3rd arg well as res1.CallStatus !
	//ret := res1.R1
	//Do NOT trust the third return from .Call
	//You did the right thing ignoring it. For many Win32 APIs it is unreliable.

	// Important edge case:
	// GetWindowLongPtr can legally return 0 even on success.
	// The only reliable failure signal is GetLastError.
	// if ret == 0 {
	if res1 := wincoe.GetWindowLongPtrW(hwnd, index); res1.Failed() {
		//lastErr := windows.GetLastError() //XXX: so, needed! probably the only case so far! NO, this is always 0/nil because each syscall(which this is) from Go will setlasterr(0) first, as per https://github.com/golang/go/issues/41220
		// lastErr := res1.CallStatus
		/*
				Why windows.GetLastError() is Tricky
			In Go's golang.org/x/sys/windows package, windows.GetLastError() returns an error interface type (under the hood, it’s a windows.Errno).
			If the underlying Windows API reports 0 (which matches ERROR_SUCCESS), Go's windows package translates this to a literal nil error interface, not an error object containing ERROR_SUCCESS.
			Therefore, you will never get an error object where errors.Is(err, windows.ERROR_SUCCESS) evaluates to true, because by the time it reaches your code, a success is just a plain old nil.
		*/
		// GetLastError returns nil if the last error code is 0 (ERROR_SUCCESS)
		// if lastErr != nil { //&& !errors.Is(lastErr, windows.ERROR_SUCCESS) {//
		return 0, fmt.Errorf("GetWindowLongPtrW failed: %w", res1.Err)
		// //nolint:wrapcheck
		// return 0, lastErr
		// }
	} else {
		return res1.R1, nil
	}
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
	style, err := getWindowLongPtr(hwnd, wincoe.GWL_STYLE)
	if err != nil {
		logf("GetWindowLongPtr GWL_STYLE failed: %v", err)
		reason = "GetWindowLongPtr GWL_STYLE failed"
		return
	}

	exStyle, err := getWindowLongPtr(hwnd, wincoe.GWL_EXSTYLE)
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
	if s&wincoe.WS_CHILD != 0 {
		reason = "is child"
		return
	}

	// Tool windows are often menus/popups
	if ex&wincoe.WS_EX_TOOLWINDOW != 0 {
		reason = "is tool window"
		return
	}

	// Explicit no-activate → DO NOT TOUCH
	if ex&wincoe.WS_EX_NOACTIVATE != 0 {
		reason = "has WS_EX_NOACTIVATE (explicit no-activate)"
		return
	}

	ret = false
	reason = "shouldn't skip"
	return
}

// findNewForegroundCandidateAfterSendToBack walks the Z-order from the very
// top (GetTopWindow(0)) downward via GW_HWNDNEXT, looking for the first
// window that's a legitimate refocus target: visible, not one of our own
// windows, not excludeHwnd (the window we just sent to the back), and not
// something shouldSkipFocusingIt() would flag (child window, tool window,
// or explicitly WS_EX_NOACTIVATE). Used by handleActualMoveOrResize's
// winkey+MMB send-to-back handling (see unfocusSentToBackWindow) to decide
// what should receive focus once the backgrounded window has been pushed
// out of view but, due to SWP_NOACTIVATE, still nominally holds it.
//
// Returns 0 if no suitable candidate turns up within maxWalkSteps hops, or
// if GetTopWindow itself returns nothing (empty desktop Z-order).
func findNewForegroundCandidateAfterSendToBack(excludeHwnd windows.Handle) windows.Handle {
	const maxWalkSteps = 500 // defensive bound; a real Z-order is never remotely this deep

	//res1 := procGetTopWindow.Call(0)
	hwnd, res1 := wincoe.GetTopWindow(0)
	if res1.Failed() {
		logf("findNewForegroundCandidateAfterSendToBack:GetTopWindow failed, res:%v", res1)
		return 0 //quicker exit than below
	}
	// hwnd := windows.Handle(res1.R1)

	for i := 0; hwnd != 0 && i < maxWalkSteps; i++ {
		switch {
		case hwnd == excludeHwnd:
			// Shouldn't normally happen -- we just moved it to the bottom
			// via a synchronous SetWindowPos that already returned -- but
			// stay defensive against any OS-level timing surprise.
		case isOwnWindow(hwnd):
			// Never refocus one of our own (hidden/overlay) windows.
		default:
			// if resVis := procIsWindowVisible.Call(uintptr(hwnd)); resVis.R1 != 0 {
			if wincoe.IsWindowVisible(hwnd) {
				if skip, _ := shouldSkipFocusingIt(hwnd); !skip {
					return hwnd
				}
			}
		}

		//res2 := procGetWindow.Call(uintptr(hwnd), GW_HWNDNEXT)
		res2 := wincoe.GetWindow(hwnd, wincoe.GW_HWNDNEXT)
		//okFIXME: handle the case of window.ERROR_INVALID_WINDOW_HANDLE here like when hwnd is 0 or hwnd is possibly not alive anymore?!
		// if res2.R1 == 0 {
		// 	// Check if it's just the normal end of the Z-order vs. a true error
		// 	// (e.g. the window we were querying was destroyed mid-walk).
		// 	// if res2.CallStatus != nil && !errors.Is(res2.CallStatus, windows.ERROR_SUCCESS) {
		// 	if !res2.CallStatusIs(windows.ERROR_SUCCESS) {
		// 		// Optional: log that the walk was cut short due to an invalid handle mid-walk
		// 		logf("DEBUG: findNewForegroundCandidateAfterSendToBack:GetWindow(GW_HWNDNEXT) hit invalid handle mid-walk")
		// 	}
		// 	break // if we don't break here then next 'for' loop iteration will due to hwnd!=0 is inside the 'for' decl.!
		// }
		if res2.Failed() {
			logf("DEBUG: findNewForegroundCandidateAfterSendToBack:GetWindow(GW_HWNDNEXT) hit invalid handle mid-walk, res:%v", res2)
			return 0 //can do 'break' too, but what the heck, wanna be sure that adding code after the 'for' won't be executed from this path!
		}
		hwnd = windows.Handle(res2.R1)
		//ohitsintheloopFIXME: am I even handling the case of hwnd == 0 ?! doesn't seem so! should I then try GW_HWNDNEXT ? I guess it's already doing this then!
	}

	return 0
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
		class, res1 := wincoe.GetClassName(target)
		if res1.Failed() {
			logf("forceForeground:GetClassName failed, res:%v", res1)
			return false //TODO: should we continue instead? unclear if it makes sense; #used2continue
		}
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
			// targetThreadID,res2 := procGetWindowThreadProcessID.Call(uintptr(target), uintptr(unsafe.Pointer(&targetProcessID)))
			//if r1 == 0 {
			targetThreadID, res2 := wincoe.GetWindowThreadProcessId(target, &targetProcessID)
			if res2.Failed() {
				logf("forceForeground:GetWindowThreadProcessId failed: %v", res2)
				return false
			}
			//var targetThreadID uint32 = uint32(res2.R1)

			// XXX: assuming we're used on mainThreadID only! we should remove these checks and just use mainThreadID
			curTid := windows.GetCurrentThreadId()
			if curTid != mainThreadID {
				logf("dev coding error: forceForeground is being called(next) from a threadID(%d) that wasn't mainThreadID(%d)", curTid, mainThreadID)
			}

			// Use SendMessageTimeout to see if the window is alive
			var result uintptr
			// res3 := procSendMessageTimeout.Call(
			// 	uintptr(target),
			// 	WM_NULL, // WM_NULL (harmless ping)
			// 	0,
			// 	0,
			// 	SMTO_ABORTIFHUNG,  //0x0002, // SMTO_ABORTIFHUNG
			// 	HungWindowTimeout, // 150ms timeout
			// 	uintptr(unsafe.Pointer(&result)),
			// )

			//if err2 != nil || ret == 0 {
			if res3 := wincoe.SendMessageTimeout(target,
				wincoe.WM_NULL, // WM_NULL (harmless ping)
				0, 0,
				wincoe.SMTO_ABORTIFHUNG, //0x0002, // SMTO_ABORTIFHUNG
				HungWindowTimeout,       // 150ms timeout
				&result,
			); res3.Failed() {
				logf("forceForeground: Target window HWND 0x%X is HUNG err='%v'. Aborting AttachThreadInput to prevent deadlock.", target, res3.Err)
				return false
			}

			// Only if the window responds do we proceed with the attachment
			// res4 := procAttachThreadInput.Call(uintptr(curTid), uintptr(targetThreadID), uintptr(1))
			// if attachRet == 0 {
			if res4 := wincoe.AttachThreadInput(curTid, targetThreadID, true /*attach!*/); res4.Failed() {
				/*
					The reality: Microsoft explicitly hardcodes AttachThreadInput to fail if the target thread belongs to a classic console window (conhost.exe or cmd.exe). Console windows do not have a standard USER32 message queue in the way GUI apps do; their input is managed by the Client/Server Runtime Subsystem (CSRSS) or the Conhost subsystem.
					When you ask Windows to attach to a console thread, the OS rejects it and returns ERROR_INVALID_PARAMETER (87) — aka "The parameter is incorrect."
						- Gemini 3.1 Pro
				*/
				logf("forceForeground: AttachThreadInput failed: %v", res4.Err)
				return false
			}

			defer func() {
				// if res := procAttachThreadInput.Call(uintptr(curTid), uintptr(targetThreadID), uintptr(0)); res.Failed() {
				if res := wincoe.AttachThreadInput(curTid, targetThreadID, false /*detach!*/); res.Failed() {
					logf("forceForeground: AttachThreadInput detach failed for threadIDs %d/%d: %v", curTid, targetThreadID, res.Err)
				}
			}() // Detach always
		} //was not console
	} // was useThreadAttachInputForFocus

	succeeded := focusThisHwnd(target) // still attached thread here.

	return succeeded //fgRet != 0
} //detached thread at end of function due to 'defer'

func logLMBState(prefix string) {
	// res1 := procGetAsyncKeyState.Call(VK_LBUTTON)
	// state := res1.R1
	// if state&0x8000 != 0 {
	if wincoe.IsKeyDown(wincoe.VK_LBUTTON) {
		logf("%s: LMB is DOWN", prefix) //, state)
	} else {
		logf("%s: LMB is UP", prefix) //, state)
	}
}

/* ---------------- Mouse Hook ---------------- */

const Duration5ms time.Duration = 5 * time.Millisecond // aka 5 million ns aka nanosec

/*
"High-input scenarios (gaming, rapid typing) may queue many events, but your callbacks still run one-by-one — the queue just grows temporarily. If you take too long in a callback (> ~1 second, controlled by LowLevelHooksTimeout registry key), Windows may drop or timeout subsequent calls, but it won't parallelize them." - Grok

"When a qualifying input event occurs (e.g., a mouse move or key press), the system detects installed low-level hooks and posts a special internal message (not a standard WM_ message) to the message queue of the thread that installed the hook. Your message loop then retrieves and dispatches this message, and during dispatch, Windows invokes your hook callback (mouseProc or keyboardProc)." - Grok
*/
//nCode being int32 not int: "Matches the Win32 C Spec: In Microsoft's C header (winuser.h), nCode is defined as a standard C int. On Windows (both 32-bit and 64-bit x64), a C int is strictly 32 bits signed."
func mouseProc(nCode int32, wParam uintptr, lParam unsafe.Pointer) uintptr {
	// Start a timer for the hook itself
	start := time.Now()
	// Standard Win32 Hook practice: If nCode < 0, we must pass it
	// to the next hook immediately and stay out of the way.
	if nCode < 0 {
		//res1 := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(lParam))

		//Why is this safe? Because in mouseProc, that lParam pointer is coming from Windows into your callback. It points to a MSLLHOOKSTRUCT that Windows allocated in its own memory space. The Go garbage collector does not own this memory, does not track it, and cannot move or free it. Therefore, converting it to a plain integer (uintptr) immediately is perfectly safe.
		res1 := wincoe.CallNextHookEx(0, nCode, wParam, uintptr(lParam))
		if nowDiff := time.Since(start); nowDiff > Duration5ms {
			logf("stutter1 %d ns", nowDiff.Nanoseconds())
		}
		return res1.R1
	}

	// nolint:govet //for unsafeptr, has no effect actually, still warns even with settings.json only this works(outside of vscode): go vet -unsafeptr=false
	//info := (*MSLLHOOKSTRUCT)(unsafe.Pointer(lParam)) // XXX: warns without the .\.vscode\settings.json the unsafeptr false part.

	// ✅ Direct conversion from unsafe.Pointer to struct pointer (100% valid Go):
	info := (*wincoe.MSLLHOOKSTRUCT)(lParam)
	// // Trick the linter: convert to pointer via an interface or a helper
	// // that doesn't trigger the "unsafeptr" heuristic.
	// var p interface{} = lParam
	// //nolint:govet,unsafeptr // because
	// info := (*MSLLHOOKSTRUCT)(unsafe.Pointer(p.(uintptr)))

	if info.Flags&wincoe.LLMHF_INJECTED != 0 {
		// This mouse event was generated by SendInput
		// Do NOT treat it as user input
		//res2 := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(lParam))
		res2 := wincoe.CallNextHookEx(0, nCode, wParam, uintptr(lParam))
		if nowDiff := time.Since(start); nowDiff > Duration5ms {
			logf("stutter2 %d ns", nowDiff.Nanoseconds())
		}
		return res2.R1
	}

	switch wParam {
	case wincoe.WM_LBUTTONDOWN: //LMB pressed aka LMBDown or LMB DOWN
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

			lmbDownSwallowed.Store(true) // we're about to eat this down; the matching up must be eaten too, regardless of what happens to activeSession in between.
			return 1                     // swallow LMB
		} else if !winDown {
			tryBringForegroundToFrontAt(info.Pt)
		} // the 'if' in LMB

	case wincoe.WM_MOUSEMOVE:
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
						case !shiftDown && keyDown(wincoe.VK_LBUTTON):
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
						case !shiftDown && keyDown(wincoe.VK_RBUTTON):
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
						case keyDown(wincoe.VK_MBUTTON): //this doesn't get hit, doh! unless you hold it during mouse move, which is unlikely for you to do!
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
				var winDown bool = keyDown(wincoe.VK_LWIN) || keyDown(wincoe.VK_RWIN)
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
					// prepare data & wincoe.PostMessage(...)

					//data := new(WindowMoveData) // Heap-allocated, TODO: avoid heap allocation somehow.
					// Create a local copy of the data.
					// This stays on the STACK, so it's lightning fast.
					data := WindowMoveData{
						Hwnd:        session.targetWnd,
						X:           newX,
						Y:           newY,
						InsertAfter: 0, // this is the value for HWND_TOP but SWP_NOZORDER below makes it unused, supposedly!

						Flags: wincoe.SWP_NOSIZE | wincoe.SWP_NOACTIVATE | wincoe.SWP_NOZORDER | wincoe.SWP_ASYNCWINDOWPOS, // for ModeMove
					}
					//data.Hwnd = targetWnd
					//data.X = newX // int32, full range
					//data.Y = newY
					//data.InsertAfter = 0 // this is the value for HWND_TOP but SWP_NOZORDER below makes it unused, supposedly!

					//data.Flags = SWP_NOSIZE | SWP_NOACTIVATE | SWP_NOZORDER // Or dynamic

					//// Post the move request instead of doing the windows move/drag motion here
					// wincoe.PostMessage(
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
					// 	//wincoe.PostThreadMessage(uintptr(mainThreadId), WM_DO_SETWINDOWPOS, 0, 0)
					// 	//the reason we use PostMessage and not PostThreadMessage here is because while systray menu popup is open it runs its own msg loop and calls my wndProc so it will ignore all of these doorbells until popup is closed if i use postThreadMessage!
					// 	res1 := wincoe.PostMessage(uintptr(mainMsgHwnd), WM_DO_SETWINDOWPOS, 0, 0)
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
				var winDown bool = keyDown(wincoe.VK_LWIN) || keyDown(wincoe.VK_RWIN)
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

			// Shift-held resize mirroring (see handleShiftMirrorToggle) is
			// now triggered directly from keyboardProc's real Shift
			// key-transition events -- posted via WM_APPLY_SHIFT_MIRROR to
			// the main thread the instant the key transitions, with no
			// dependency on a mouse-move event arriving first -- rather
			// than being detected here by polling keyDown(VK_SHIFT) on
			// every move. session.resizeZone always reflects whichever
			// zone (original or mirrored) is currently active by the time
			// any WM_MOUSEMOVE is processed.
			if !ShouldThrottle() {
				nx, ny, nw, nh := calculateResize(session, info.Pt, session.resizeZone) //TODO: move this into wndProc aka into handleActualMove() ?!
				flags := uint32(wincoe.SWP_NOZORDER | wincoe.SWP_NOACTIVATE)
				if asyncResize.Load() {
					flags |= wincoe.SWP_ASYNCWINDOWPOS
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
				// 	res1 := wincoe.PostMessage(uintptr(mainMsgHwnd), WM_DO_SETWINDOWPOS, 0, 0)
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

	case wincoe.WM_LBUTTONUP: //LMB released aka LMBUP aka LMB UP
		if session := activeSession.Load(); session != nil && session.mode == ModeMove {
			// End the drag regardless of whether we owe a swallow below (see
			// lmbDownSwallowed's doc comment): a real LMB-up always ends an
			// active ModeMove session, whether or not its own matching down
			// was one we swallowed (a recovery session's real down reached
			// the target normally, but this real up still ends OUR side of
			// the drag). This also means when winkey goes UP it will make
			// sure from keyboardProc that start menu doesn't pop up!
			softReset(true)
		}
		if !lmbDownSwallowed.CompareAndSwap(true, false) {
			// We never swallowed a matching down for this button (no
			// gesture was in progress, or this is a missed-gesture-recovery
			// session whose real down reached the target normally) -- let
			// this pass through untouched.
			break
		}
		// We swallowed the down; eat this up to keep the target's button
		// state balanced (e.g. hovering a menu item while releasing LMB
		// would otherwise act like a real click on LBUTTONUP).
		return 1

	case wincoe.WM_RBUTTONUP: //RMB released aka RMBUP aka RMB UP
		if session := activeSession.Load(); session != nil && session.mode == ModeResize {
			// See the identical comment in WM_LBUTTONUP: end the resize
			// regardless of whether we owe a swallow below.
			softReset(true)
			if nowDiff := time.Since(start); nowDiff > Duration5ms {
				logf("stutter7 %d ns", nowDiff.Nanoseconds()) // FIXME: hitting only this one! yep it's hideOverlay(), do it in wndProc heh!
			}
		}
		if !rmbDownSwallowed.CompareAndSwap(true, false) {
			// See the identical comment in WM_LBUTTONUP.
			break
		}
		/*
			(alt+z to toggle word wrapping)
			Claude 5 Sonnet High Thinking said:
			"Honest caveat: your app also takes mouse capture (SetCapture(mainMsgHwnd)) once the first move is processed, and releases it via a posted (async) message from softReset. So there's a small theoretical race where, right at button-release, capture might not have been relinquished yet by the time this up-event is routed — in which case it'd go to your hidden window instead of conhost, not fixing the "stuck" state that one time. In practice, for any drag lasting more than a few ms (i.e. essentially all real drags), the posted release will have long since been processed, so this should work correctly the overwhelming majority of the time. If you find it's still occasionally sticky in testing, the more bulletproof fix is to have softReset post the capture-release and then, only for recovery sessions, post a second message that gets processed after it and re-injects a synthetic up-click via SendInput from the main thread (same pattern as WM_INJECT_SEQUENCE) — I didn't implement that since it's meaningfully more invasive and worth validating the simple fix first."
		*/
		return 1 // Swallow

	case wincoe.WM_RBUTTONDOWN: //RMB pressed aka RMBDown aka RMBdrag
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
			rmbDownSwallowed.Store(true) // we're about to eat this down; the matching up must be eaten too, regardless of what happens to activeSession in between.
			return 1                     // Swallow
		} else if !winDown {
			tryBringForegroundToFrontAt(info.Pt)
		} // the 'if' in RMB

	case wincoe.WM_MBUTTONDOWN: //MMB pressed
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
			mmbDownSwallowed.Store(true) // we're about to eat this down; the matching up must be eaten too, since MMB has no persistent session to key off of at all.
			return 1                     // swallow MMB
		} else if !winDown {
			tryBringForegroundToFrontAt(info.Pt)
		} // the 'if' in MMB
	case wincoe.WM_MBUTTONUP: //MMB released aka MMBUP
		if !mmbDownSwallowed.CompareAndSwap(true, false) {
			break // we never swallowed a matching down; let this pass through untouched.
		}
		return 1 // eat it, balancing the down we swallowed earlier. MMB gestures are a single immediate Z-order change with no persistent activeSession, so there's nothing else to reset here.
	} //switch

	if nowDiff := time.Since(start); nowDiff > Duration5ms {
		logf("stutter3 %d ns", nowDiff.Nanoseconds())
	}

	// Always pass the event down the chain so other apps don't break
	//res1111 := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(lParam))
	res1111 := wincoe.CallNextHookEx(0, nCode, wParam, uintptr(lParam))
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

	var wc wincoe.WNDCLASSEX
	wc.CbSize = uint32(unsafe.Sizeof(wc))
	wc.LpfnWndProc = wndProc
	wc.LpszClassName = classNameUTF16
	wc.HInstance = selfHInstance

	//procSetLastError.Call(0) // useless! .Call() already does this, see: https://github.com/golang/go/issues/41220#issuecomment-5072097059
	// Register class — check return value
	// No SetLastError(0) reset needed here: per golang/go#41220, Go's syscall
	// layer already captures GetLastError atomically immediately after
	// RegisterClassExW returns, and that value is normalized into res2.Err
	// below via wincoe.CheckZero regardless of whatever the thread's error
	// state was beforehand — resetting it first accomplished nothing.
	if res2 := wincoe.RegisterClassEx(&wc); res2.Failed() { //err2 != nil || ret == 0 {
		//lastErr := windows.GetLastError()
		return 0, fmt.Errorf("RegisterClassEx failed: %w", res2.Err) //, lastErr) //XXX: multiple %w is legal in Go v1.20+ (Feb 2023)
	} else {
		hiddenClassRegistered.Store(true)
	}

	// res3 := procCreateWindowEx.Call(
	// 	0,
	// 	uintptr(unsafe.Pointer(classNameUTF16)),
	// 	0,
	// 	0,
	// 	0, 0, 0, 0,
	// 	0,
	// 	0,
	// 	uintptr(wc.HInstance),
	// 	0,
	// )
	//if err3 != nil || hwndRaw == 0 {
	if res3 := wincoe.CreateWindowEx(
		0,
		classNameUTF16,
		nil,
		0,
		0, 0, 0, 0,
		0,
		0,
		wc.HInstance,
		nil,
	); res3.Failed() {
		//lastErr := windows.GetLastError()
		return 0, fmt.Errorf("CreateWindowEx failed: %w", res3.Err) // (error code: %w)", err3, lastErr)
	} else {
		return windows.Handle(res3.R1 /*aka hwndRaw*/), nil
	}
}

var (
	hookThreadID atomic.Uint32
	mainThreadID uint32 // this one's guaranteed orderly set/read, as the code stands currently!
	// Optional: prepare a mutex for later when we secure 'currentDrag'
	// dragStateMutex sync.RWMutex
)

// mainMsgHwndAtomic holds the current value of the hidden main message
// window handle (see createMessageWindow / runApplication), guarded by
// atomic ops instead of a plain windows.Handle because it's read from
// three distinct thread contexts -- the main thread itself; the dedicated
// hook thread (hookWorker's mouseProc/keyboardProc, e.g.
// enqueueMoveOrResize, softReset, WM_INJECT_SEQUENCE posting, and the
// panic-bridge's WM_CLOSE post); and the separate OS thread Windows spawns
// for every single invocation of ctrlCHandler (see SetConsoleCtrlHandler's
// documented per-event-new-thread model) -- while it's only ever WRITTEN
// from the main thread (once at startup in runApplication via
// storeMainMsgHwnd, once at shutdown in deinitMainMsgHwnd via the atomic
// Swap).
var mainMsgHwndAtomic atomic.Uintptr

// loadMainMsgHwnd is the sole safe way to read the current main message
// window handle from any thread. Returns 0 before runApplication has
// created the window, or after deinitMainMsgHwnd has torn it down.
func loadMainMsgHwnd() windows.Handle {
	return windows.Handle(mainMsgHwndAtomic.Load())
}

// storeMainMsgHwnd is the sole safe way to set the main message window
// handle. Must only ever be called from the main thread (see deinit()'s
// own "runs only on main()" invariant, which this shares).
func storeMainMsgHwnd(h windows.Handle) {
	mainMsgHwndAtomic.Store(uintptr(h))
}

var hookPanicPayload atomic.Value // We use atomic for thread-safety
var mainAcknowledgedShutdown = make(chan struct{})

// hookWorkerDone is closed by hookWorker's own teardown closure immediately
// before its OS thread is actually about to finish — whether that's a
// genuine no-panic exit (WM_QUIT received normally, no recover() payload at
// all), or after recovering from a panic and completing its shutdown-
// signaling sequence (see the Cross-Thread Panic Bridge closure's two
// select-case branches, which now return instead of hanging in select{}
// forever). In every case both hooks are already unhooked by the time this
// closes. primary_defer() waits on this — after wincoe.WaitAnyKey() — so
// logs don't get flushed and the process doesn't exit before that's true.
// Mirrors the existing logWorkerDone/closeAndFlushLog() pattern.
var hookWorkerDone = make(chan struct{})

// hookWorkerSecondaryDefer is hookWorker's analogue of secondary_defer(): a
// last-resort safety net that only does anything if hookWorker's own
// cross-thread-panic-bridge closure (deferred after this one, so it runs
// first — LIFO) panics again while handling the original panic.
//
// Unlike secondary_defer(), reaching this with recover() == nil is NOT a
// bug here — it's the ordinary path for every one of the panic-bridge
// closure's own exit points: its entirely-panic-free tail case, AND (since
// that closure no longer hangs forever in select{} — see its two
// select-case branches) both of its recovered-panic return points too. In
// all three, the closure's own recover() call already fully absorbed
// whatever panic there was (or there was none to begin with) before it
// returned, so there's nothing left propagating for this defer to catch.
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
			if res := wincoe.PostThreadMessage(mainThreadID, wincoe.WM_QUIT, 0, 0); res.Failed() { //cantbeTODO: investigate if mainThreadID can be unset or 0 here.
				logf("hookWorker panic-bridge: PostThreadMessage(WM_QUIT) to mainThreadID=%d failed, err: %v", mainThreadID, res.Err)
			}
			//doneFIXME: what if main is dead too, and would ignore the signal or what, then we exit here? sure after X seconds

			if msgHwnd := loadMainMsgHwnd(); msgHwnd != 0 {
				// Post to the Window Handle, NOT the Thread ID.
				// This cuts through modal menus like the systray popup menu!
				if res := wincoe.PostMessage(msgHwnd, wincoe.WM_CLOSE, 0, 0); res.Failed() {
					logf("hookWorker panic-bridge: PostMessage(WM_CLOSE) to mainMsgHwnd=0x%X failed, err: %v", msgHwnd, res.Err)
				}
			} else {
				logf("hookWorker panic-bridge: won't PostMessage(WM_CLOSE) because mainMsgHwnd is 0")
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
				logf("main() acknowledged shutdown; hookWorker is finishing its own OS thread now instead of hanging it forever.")
			case <-time.After(waitForMainSeconds * time.Second):
				directLoggerf("CRITICAL: Main thread unresponsive after %d seconds. hookWorker will finish its own OS thread anyway; primary_defer()'s already-bounded wait on hookWorkerDone covers main's side of this.", waitForMainSeconds)
			}
			// Either way, hookWorker's own job is done here: both hooks are
			// already unhooked (their unhook defers were registered right
			// after SetWindowsHookEx succeeded, above -- they always run
			// before this closure during LIFO unwind), and the shutdown
			// signals (WM_QUIT/WM_CLOSE) have been posted. There's nothing
			// left only this specific OS thread is still needed for, so
			// actually finish the goroutine -- and let
			// runtime.UnlockOSThread() release the OS thread
			// runtime.LockOSThread() pinned it to -- instead of leaking
			// that thread for the remaining lifetime of the process by
			// hanging in select{} forever, as this used to do. Returning
			// here is exactly as safe as the entirely-panic-free tail case
			// below: recover() already consumed the original panic, and a
			// deferred function returning normally afterward is standard,
			// well-defined behavior -- hookWorkerSecondaryDefer's own
			// recover() will see nil here, same as it does for that tail
			// case (see its doc comment).
			close(hookWorkerDone)
			return
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
	// mouseCallback = windows.NewCallback(mouseProc)
	// res1 := procSetWindowsHookEx.Call(WH_MOUSE_LL, mouseCallback, 0, 0)
	// if err != nil || h == 0 {
	if theHook, res1 := wincoe.SetWindowsHookEx(wincoe.WH_MOUSE_LL, mouseProc, 0, 0); res1.Failed() {
		exitf(1, "hookWorker:SetWindowsHookEx for mouse failed, res: %v", res1)
		unreachable()
	} else {
		defer func() {
			// prev := mouseHook
			// mouseHook = 0
			// if res := procUnhookWindowsHookEx.Call(uintptr(prev)); res.Failed() {
			if res := wincoe.UnhookWindowsHookEx(theHook); res.Failed() {
				logf("failed to unhook mouseHook: %v", res.Err)
			} else {
				logf("unhooked mouseHook")
			}
		}()
		// mouseHook = theHook //windows.Handle(res1.R1)
	}

	// kbdCB := windows.NewCallback(keyboardProc)
	// res2 := procSetWindowsHookEx.Call(
	// 	WH_KEYBOARD_LL,
	// 	kbdCB,
	// 	0, // hMod = 0 for low-level
	// 	0, // dwThreadId = 0 = global
	// )
	// if err != nil || hk == 0 {
	if tmpKeyHook, res2 := wincoe.SetWindowsHookEx(wincoe.WH_KEYBOARD_LL, keyboardProc, 0, 0); res2.Failed() {
		exitf(1, "hookWorker:SetWindowsHookEx for keyboard failed, res: %v", res2)
		unreachable()
	} else {
		defer func() {
			// kbdHook = 0
			// if res := procUnhookWindowsHookEx.Call(uintptr(kbdHook)); res.Failed() {
			if res := wincoe.UnhookWindowsHookEx(tmpKeyHook); res.Failed() {
				logf("failed to unhook kbdHook: %v", res.Err)
			} else {
				logf("unhooked kbdHook")
			}
		}()
		// kbdHook = tmpKeyHook
	}

	// 4. The Thread's Private Message Loop
	var msg wincoe.MSG
	for {
		//exitf(1, "temp. manual panic")
		// res3 := procGetMessage.Call(
		// 	uintptr(unsafe.Pointer(&msg)),
		// 	0, 0, 0,
		// )

		// const minus1 = ^uintptr(0)
		// ret == 0 means WM_QUIT was received. ret == -1 aka ^uintptr(0) is an error.
		//if ret == 0 || ret == minus1 {

		// GetMessage here calls the hook(s), ie. during this GetMessage call the hooks execute!
		if res3 := wincoe.GetMessage(&msg, 0, 0, 0); res3.Failed() {
			logf("Hook worker thread got GetMessage error res=%v, exiting and unhooking...", res3)
			break
		} else if res3.R1 == 0 /*returns 0 due to receiving WM_QUIT==0x12*/ {
			logf("Hook worker thread received WM_QUIT(so R1==0), exiting and unhooking...")
			break
		}

		// procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		_ = wincoe.TranslateMessage(&msg) // nolint:errcheck // don't care
		// procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
		resDis := wincoe.DispatchMessage(&msg)
		if resDis.CallStatusFailed() {
			logf("DEBUG: in hookWorker, last GetLastError() seen by DispatchMessage is %v", resDis.CallStatus)
		}
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

//var mouseCallback uintptr

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

func handleActualMoveOrResize(data WindowMoveData, bypassThrottle bool) {
	//Top of handleActualMoveOrResize, before the rate-limit check (capture should be set even if we throttle the actual SetWindowPos):
	// Lazy once-per-session SetCapture.
	// We are guaranteed to be on the main thread here (wndProc context).
	if cur := activeSession.Load(); cur != nil && captureHeldForSession.Load() != cur {
		//_ = procSetCapture.Call(uintptr(mainMsgHwnd))
		if msgHwnd := loadMainMsgHwnd(); msgHwnd != 0 {
			_ = wincoe.SetCapture(msgHwnd)
		} else {
			logf("handleActualMoveOrResize:SetCapture couldn't setcapture because mainMsgHwnd was 0")
			droppedMoveOrResizeEvents.Add(1) //TODO: so this was queued but decided not to do the action, maybe we need a diff. counter for each kind of dropped type/reason?
			return
		}
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
	var async bool = (data.Flags & wincoe.SWP_ASYNCWINDOWPOS) != 0
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
		// res1 := procSetWindowPos.Call( //XXX: this is blocking, depends on target window's responsiveness! which is why this happens on wndProc not inside mouseProc btw.
		// 	uintptr(target),
		// 	uintptr(data.InsertAfter),
		// 	0, 0, // X and Y are ignored because of SWP_NOMOVE
		// 	// #nosec G115 -- safe: Win32 dimensions are sign-extended from int32 into uintptr
		// 	uintptr(data.W),
		// 	// #nosec G115 -- safe: Win32 dimensions are sign-extended from int32 into uintptr
		// 	uintptr(data.H),
		// 	uintptr(data.Flags|SWP_NOMOVE),
		// )
		res1 := wincoe.SetWindowPos( //XXX: this is blocking, depends on target window's responsiveness! which is why this happens on wndProc not inside mouseProc btw.
			target, data.InsertAfter,
			0, 0, // X and Y are ignored because of SWP_NOMOVE
			data.W,
			data.H,
			data.Flags|wincoe.SWP_NOMOVE,
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
		var r wincoe.RECT
		// res2 := procGetWindowRect.Call(uintptr(target), uintptr(unsafe.Pointer(&r)))
		/*
			1. Why GetWindowRect Seems Out of Sync

			When you call SetWindowPos without SWP_ASYNCWINDOWPOS (sync mode), it does indeed block until the target window processes the WM_WINDOWPOSCHANGING and WM_WINDOWPOSCHANGED messages.

			However, Windows applications are highly asynchronous internally. When a modern app (especially one using a custom UI framework, WPF, or complex drawing like Defraggler) receives the resize message, it often just updates its internal state and posts a paint message to itself to redraw later. Furthermore, during WM_WINDOWPOSCHANGING, an application can modify the WINDOWPOS structure to enforce its own minimum size.

			If it does this, SetWindowPos returns, but GetWindowRect might briefly return an intermediate state, or the window manager might not have fully reconciled the visual bounds yet.
		*/
		//if ret == 0 {
		if res2 := wincoe.GetWindowRect(target, &r); res2.Failed() {
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

		// res3 := procSetWindowPos.Call(
		// 	uintptr(target),
		// 	uintptr(data.InsertAfter),

		// 	// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
		// 	uintptr(correctedX),
		// 	// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
		// 	uintptr(correctedY),

		// 	0, 0, // W and H are ignored because of SWP_NOSIZE
		// 	uintptr(data.Flags|SWP_NOSIZE),
		// )
		//if ret2 == 0 { //failed
		if res3 := wincoe.SetWindowPos(target, data.InsertAfter, correctedX, correctedY,
			0, 0, // W and H are ignored because of SWP_NOSIZE
			data.Flags|wincoe.SWP_NOSIZE,
		); res3.Failed() {
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

		// res4 := procSetWindowPos.Call(
		// 	uintptr(target),
		// 	uintptr(data.InsertAfter),
		// 	// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
		// 	uintptr(data.X),
		// 	// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
		// 	uintptr(data.Y),
		// 	// #nosec G115 -- safe: Win32 dimensions are sign-extended from int32 into uintptr
		// 	uintptr(data.W),
		// 	// #nosec G115 -- safe: Win32 dimensions are sign-extended from int32 into uintptr
		// 	uintptr(data.H),
		// 	uintptr(data.Flags),
		// )
		//if ret == 0 { //failed
		if res4 := wincoe.SetWindowPos(target, data.InsertAfter, data.X, data.Y, data.W, data.H, data.Flags); res4.Failed() {
			//errCode, _, _ := procGetLastError.Call()
			logf("SetWindowPos/Move-or-AsyncResize failed(from within main message loop): hwnd=0x%x err=%v", target, res4.Err)
			// if errCode == 5 { // Access denied (UIPI likely)
			if res4.ErrIs(windows.ERROR_ACCESS_DENIED) { // ==5 aka Access denied (UIPI likely)
				showTrayInfo(selfName, "Cannot Move-or-AsyncResize elevated window (access denied), you'd have to run as admin.")
			}
		} else if data.UnfocusAfterSendToBack && unfocusSentToBackWindow.Load() {
			// The send-to-back Z-order change above succeeded and this
			// entry came from winkey+MMB's send-to-back gesture; since that
			// call used SWP_NOACTIVATE, target still nominally holds
			// keyboard focus even though it's now behind everything else.
			// Shift focus to whichever window is now genuinely on top.
			if newTop := findNewForegroundCandidateAfterSendToBack(target); newTop != 0 {
				if !forceForeground(newTop) {
					logf("handleActualMoveOrResize: failed to shift focus to new top-of-Z-order HWND=0x%X after sending HWND=0x%X to back", newTop, target)
				}
			} else {
				logf("handleActualMoveOrResize: no suitable window found to refocus after sending HWND=0x%X to back", target)
			}
		} else if data.FocusAfterBringToFront {
			// The bring-to-front Z-order change above succeeded (issued
			// with SWP_NOACTIVATE, so it happened unconditionally
			// regardless of whatever focus-stealing restrictions might
			// otherwise silently veto SetWindowPos's own implicit
			// activation -- see FocusAfterBringToFront's doc comment on
			// WindowMoveData). Now explicitly focus target via the same
			// reliable mechanism the rest of this codebase already relies
			// on to steal focus. Safe no-op if target already happens to
			// be foreground.
			if !forceForeground(target) {
				logf("handleActualMoveOrResize: failed to focus HWND=0x%X after bringing it to front via winkey+shift+MMB", target)
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
	case wincoe.WTS_SESSION_LOCK:
		return "lock"
	case wincoe.WTS_SESSION_UNLOCK:
		return "unlock"
	default:
		return fmt.Sprintf("0x%x", code)
	}
}

var wndProc = windows.NewCallback(func(hwnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case WM_DO_SETWINDOWPOS:
		// Reset the doorbell immediately so new incoming mouse events
		// can queue a fresh wakeup call if they arrive while we are draining.
		doorbellPending.Store(false)

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
		// if res := procSetWindowPos.Call(
		// 	uintptr(target),
		// 	uintptr(HWND_TOP),
		// 	0, 0, 0, 0,
		// 	SWP_NOMOVE|SWP_NOSIZE|SWP_NOACTIVATE,
		if res := wincoe.SetWindowPos(target, wincoe.HWND_TOP, 0, 0, 0, 0,
			wincoe.SWP_NOMOVE|wincoe.SWP_NOSIZE|wincoe.SWP_NOACTIVATE,
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
		prev := wincoe.GetCapture()     //CheckNone
		res1 := wincoe.ReleaseCapture() //CheckBool
		if res1.Failed() {
			logf("in wndProc, WM_DO_RELEASE_CAPTURE: ReleaseCapture failed, err: %v", res1.Err)
		}
		current := wincoe.GetCapture() //CheckNone
		// Normal case (prev=1 or 0, current=0) → completely silent
		if current != 0 {
			logf("in wndProc part2of2, WM_DO_RELEASE_CAPTURE says the current capture (after releasing) is still 0x%X instead of none aka 0", current)
		} else if prev != 0 && prev != hwnd && prev != 1 { // 1 is the common "desktop" value
			//hwnd is mainMsgHwnd as per Claude: "Bonus fix found along the way: wndProc's WM_DO_RELEASE_CAPTURE handler also reads mainMsgHwnd, but since wndProc is only ever invoked as the main message window's own procedure, its hwnd parameter already is that value — no atomic load needed there at all, just use the parameter:"
			// Only log unusual previous owners (debug only)
			logf("in wndProc, WM_DO_RELEASE_CAPTURE: previous owner was unexpected 0x%X (mainMsgHwnd=0x%X)", prev, hwnd)
		}

		return 0

	case WM_CANCEL_GESTURE:
		expectedTarget := windows.Handle(wParam)
		session := activeSession.Load()
		if session == nil {
			logf("WM_CANCEL_GESTURE: no active drag/resize session by the time this was processed (already ended); ignoring")
			return 0
		}
		if session.targetWnd != expectedTarget {
			logf("WM_CANCEL_GESTURE: active session's target HWND=0x%X no longer matches the one ESC was pressed for (0x%X); a new gesture must've started since, ignoring", session.targetWnd, expectedTarget)
			return 0
		}
		cancelActiveGesture(session)
		return 0

	case WM_APPLY_SHIFT_MIRROR:
		expectedTarget := windows.Handle(wParam)
		shiftDown := lParam != 0

		session := activeSession.Load()
		if session == nil || session.mode != ModeResize {
			logf("WM_APPLY_SHIFT_MIRROR: no active ModeResize session by the time this was processed; ignoring (shiftDown=%v)", shiftDown)
			return 0
		}
		if session.targetWnd != expectedTarget {
			logf("WM_APPLY_SHIFT_MIRROR: active session's target HWND=0x%X no longer matches the one this toggle was posted for (0x%X); a new gesture must've started since, ignoring", session.targetWnd, expectedTarget)
			return 0
		}

		var cursorPt wincoe.POINT
		if res := wincoe.GetCursorPos(&cursorPt); res.Failed() {
			logf("WM_APPLY_SHIFT_MIRROR: GetCursorPos failed: %v; skipping this toggle", res.Err)
			return 0
		}

		handleShiftMirrorToggle(session, cursorPt, shiftDown)
		return 0

	case wincoe.WM_WTSSESSION_CHANGE:
		switch wParam {
		case wincoe.WTS_SESSION_LOCK, wincoe.WTS_SESSION_UNLOCK:
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
			// winGestureUsed and lmbDownSwallowed/rmbDownSwallowed/
			// mmbDownSwallowed all need clearing here: real key/button-up
			// events that happen on the secure desktop while locked are
			// invisible to our hooks, so any of these left stuck true
			// would silently misfire against some later, entirely
			// unrelated key/button-up after unlock. See
			// resetStaleGestureFlags's doc comment (shared with the
			// higher-integrity-foreground case in winEventProc).
			resetStaleGestureFlags()
			if session := activeSession.Load(); session != nil {
				logf("WTS session %s detected mid-%v; discarding stale drag/resize session for HWND=0x%X", wtsSessionChangeName(wParam), session.mode, session.targetWnd)
				softReset(true)
			}
		}
		return 0

	case wincoe.WM_QUERYENDSESSION:
		// system is asking permission to end session
		logf("system is asking permission to end session")
		return 1 // allow

	case wincoe.WM_ENDSESSION:
		if wParam != 0 {
			logf("WM_ENDSESSION with wParam!=0 aka system shutdown or restart detected")
			// ensure flush here if buffered
		} else {
			logf("WM_ENDSESSION with wParam == 0 (weird?!)")
		}
		exitf(20, "due to WM_ENDSESSION")
		unreachable()
		return 0

	//TODO: maybe add option in systray if 'true' keep moving the window even after winkey is released, else stop; the latter case would stop it from moving after coming back from unlock screen, if it was moving when lock happened.
	//doneTODO: Add WH_SHELL Hook for Focus Change Detection - in progress.
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
			// ret, _, err := wincoe.PostMessage(uintptr(targetWnd), WM_LBUTTONDOWN, 1, makeLParam(10, 10)) // MK_LBUTTON = 1, safe pos
			// logf("Post WM_LBUTTONDOWN for focus ret=%d err=%v", ret, err)
			// ret, _, err = wincoe.PostMessage(uintptr(targetWnd), WM_LBUTTONUP, 0, makeLParam(10, 10)) // Release to avoid hold
			// logf("Post WM_LBUTTONUP for focus ret=%d err=%v", ret, err)
		}
		return 0

	case WM_MYSYSTRAY:

		// Strip high word to get the low 16-bit message code
		low := uint32(lParam & 0xFFFF)

		// if low != WM_MOUSEMOVE { // any non-mouse_move(0x10200 on v4) events:
		// 	logf("WM_TRAY received with lParam %x, %x", lParam, low)
		// }

		if low == wincoe.WM_MOUSEMOVE {
			// Opportunistic hover-tooltip refresh -- see
			// updateTrayTooltipInputStateIfChanged's doc comment for why
			// this WM_MOUSEMOVE-over-the-tray-icon notification is the only
			// practical hook point for keeping the tooltip's key/button
			// state text reasonably fresh.
			updateTrayTooltipInputStateIfChanged()
		}

		//if ((lParam & 0x0FFFF) == WM_RBUTTONUP) || ((lParam & 0x0FFFF) == WM_CONTEXTMENU) {
		if low == wincoe.WM_RBUTTONUP { // RMB on systray aka RMBUp or RMBUP on systray aka RMB button released
			/*
				Yes — handling WM_RBUTTONUP (after masking with 0xFFFF) alone would work on every Windows version, because:
				  XP → only 0x0205
				  Vista+ → both 0x0205 and 0x007B, but 0x0205 is still sent
			*/
			// Get mouse position early (always do this manually — wParam/lParam don't carry it reliably) - Grok
			var pt wincoe.POINT
			//if res := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))); res.Failed() {
			if res := wincoe.GetCursorPos(&pt); res.Failed() {
				logf("WM_MYSYSTRAY: GetCursorPos failed, menu will appear at (0,0): %v", res.Err)
			}

			//logf("popping tray menu")

			//res1 := procCreatePopupMenu.Call()
			hMenu, res1 := wincoe.CreatePopupMenu()
			if res1.Failed() {
				logf("in wndProc, WM_MYSYSTRAY, failed to CreatePopupMenu, res=%v", res1)
				return 0 // Handled
			}
			// hMenu := res1.R1
			// hMenu := windows.Handle(res1.R1)
			defer func() {
				// if res := procDestroyMenu.Call(hMenu); res.Failed() {
				if res := wincoe.DestroyMenu(hMenu); res.Failed() {
					logf("in wndProc, WM_MYSYSTRAY, failed to DestroyMenu, err=%v", res.Err)
				}
			}()

			{
				var actFlags uint32 = wincoe.MF_STRING // untyped constants can auto-convert, but not untyped vars(in the below call)
				if focusOnDrag.Load() {
					actFlags |= wincoe.MF_CHECKED
				}
				focusText := "Activate(aka focus) window when moved if not in focus."
				appendMenuChecked(hMenu, actFlags, MENU_ACTIVATE_MOVE, focusText)
			}

			{
				var bringToFrontOnDragFlags uint32 = wincoe.MF_STRING
				if bringToFrontOnDrag.Load() {
					bringToFrontOnDragFlags |= wincoe.MF_CHECKED
				}
				// if !focusOnDrag.Load() { //XXX:actually don't, because if it's focused, we can bring it to front
				// 	bringToFrontOnDragFlags |= MF_DISABLED | MF_GRAYED
				// }
				bringToFrontOnDragText := "Bring already-focused(!) window to front of Z-order when starting a drag/move gesture (useful after winkey+MMB sent it to back)"
				appendMenuChecked(hMenu, bringToFrontOnDragFlags,
					MENU_TOGGLE_BRING_TO_FRONT_ON_DRAG, bringToFrontOnDragText)
			}

			{
				var actResizeFlags uint32 = wincoe.MF_STRING
				if focusOnResize.Load() {
					actResizeFlags |= wincoe.MF_CHECKED
				}
				focusResizeText := "Activate(aka focus) window when a resize gesture starts if not already in focus."
				appendMenuChecked(hMenu, actResizeFlags,
					MENU_TOGGLE_ACTIVATE_RESIZE, focusResizeText)
			}

			{
				var bringToFrontOnResizeFlags uint32 = wincoe.MF_STRING
				if bringToFrontOnResize.Load() {
					bringToFrontOnResizeFlags |= wincoe.MF_CHECKED
				}
				bringToFrontOnResizeText := "Bring already-focused(!) window to front of Z-order when starting a resize gesture (independent of the same option for drag/move above)"
				appendMenuChecked(hMenu, bringToFrontOnResizeFlags,
					MENU_TOGGLE_BRING_TO_FRONT_ON_RESIZE, bringToFrontOnResizeText)
			}

			{
				var btfbcFlags uint32 = wincoe.MF_STRING
				if bringToFrontOnBackgroundClick.Load() {
					btfbcFlags |= wincoe.MF_CHECKED
				}
				btfbcText := "Bring backgrounded-but-focused window(ie. winkey+MMB -ed one) to front on LMB/MMB/RMB click"
				appendMenuChecked(hMenu, btfbcFlags,
					MENU_TOGGLE_BRING_TO_FRONT_ON_BACKGROUND_CLICK, btfbcText)
			}

			{
				var unfocusSentToBackFlags uint32 = wincoe.MF_STRING
				if unfocusSentToBackWindow.Load() {
					unfocusSentToBackFlags |= wincoe.MF_CHECKED
				}
				//ie. the meaning of winkey+shift+MMB changes when this is true!
				unfocusSentToBackText := "Switch the focus from the sent-to-back window(but can bring it back with winkey+shift+MMB) to whichever window is now on top"
				appendMenuChecked(hMenu, unfocusSentToBackFlags,
					MENU_TOGGLE_UNFOCUS_SENT_TO_BACK, unfocusSentToBackText)
			}

			{
				var useThreadAttachInputForFocusFlags uint32 = wincoe.MF_STRING
				if useThreadAttachInputForFocus.Load() {
					useThreadAttachInputForFocusFlags |= wincoe.MF_CHECKED
				}
				useThreadAttachInputForFocusText := "(dontuse)Use AttachThreadInput before attempting any window focus (else focus stealing prevention might happen)"
				appendMenuChecked(hMenu, useThreadAttachInputForFocusFlags,
					MENU_TOGGLE_USE_THREADATTACHINPUT_FOR_FOCUS, useThreadAttachInputForFocusText)
			}

			{
				var lmbFlags uint32 = wincoe.MF_STRING
				if doLMBClick2FocusAsFallback.Load() {
					lmbFlags |= wincoe.MF_CHECKED
				}
				if !focusOnDrag.Load() && !focusOnResize.Load() {
					lmbFlags |= wincoe.MF_DISABLED | wincoe.MF_GRAYED
				}
				doLMBClick2FocusAsFallbackText := "Fallback: Use Left Mouse Click to focus (Warning: will click underlying UI elements)."
				appendMenuChecked(hMenu, lmbFlags,
					MENU_USE_LMB_TO_FOCUS_AS_FALLBACK, doLMBClick2FocusAsFallbackText)
			}

			{
				var rlFlags uint32 = wincoe.MF_STRING
				if ratelimitOnMove.Load() {
					rlFlags |= wincoe.MF_CHECKED
				}
				ratelimitText := "Rate-limit window moves(by 5x, uses less CPU but looks choppier so ur subconscious will hate it)"
				appendMenuChecked(hMenu, rlFlags,
					MENU_RATELIMIT_MOVES, ratelimitText)
			}

			{
				var sldrFlags uint32 = wincoe.MF_STRING
				if shouldLogDragRate.Load() {
					sldrFlags |= wincoe.MF_CHECKED
				}
				// Disable (grey) the "Log rate of moves" item when rate-limit is off
				if !ratelimitOnMove.Load() {
					sldrFlags |= wincoe.MF_DISABLED | wincoe.MF_GRAYED
				}
				sldrText := "Log rate of moves(only if rate-limit above is enabled)"
				appendMenuChecked(hMenu, sldrFlags,
					MENU_LOG_RATE_OF_MOVES, sldrText)
			}

			{
				var asyncFlags uint32 = wincoe.MF_STRING
				if asyncResize.Load() {
					asyncFlags |= wincoe.MF_CHECKED
				}
				asyncText := "Use Async Window Positioning for Resizing(bugged for unresizable windows - it moves them)(don't use this)"
				appendMenuChecked(hMenu, asyncFlags,
					MENU_TOGGLE_ASYNC_RESIZE, asyncText)
			}

			{
				var reqWinDownFlags uint32 = wincoe.MF_STRING
				if requireWinDownHeldDuringGesture.Load() {
					reqWinDownFlags |= wincoe.MF_CHECKED
				}
				reqWinDownText := "Require holding down WinKey while performing the gesture(move/resize) - if not you'll hit edge cases" //such as(not anymore this): if you do Winkey+L to lock, then release winkey and LMB(or RMB if resize) then you unlock, the gesture is still in effect(if this is false); actually not anymore now that I got lock/unlock hooks and I reset when winkey+L locks !
				appendMenuChecked(hMenu, reqWinDownFlags,
					MENU_TOGGLE_REQUIRE_WINDOWN, reqWinDownText)
			}

			{
				var coalesceEventsFlags uint32 = wincoe.MF_STRING
				if coalesceMoveResizeEvents.Load() {
					coalesceEventsFlags |= wincoe.MF_CHECKED
				}
				coalesceEventsText := "Coalesce Move/Resize (ignores queue history to keep drag responsive), if off it's rate-limited to 60fps"
				appendMenuChecked(hMenu, coalesceEventsFlags,
					MENU_TOGGLE_COALESCE_EVENTS, coalesceEventsText)
			}

			{
				var immediateOverlayRepaintFlags uint32 = wincoe.MF_STRING
				if immediateOverlayRepaint.Load() {
					immediateOverlayRepaintFlags |= wincoe.MF_CHECKED
				}
				immediateOverlayRepaintText := "Force immediate repaint of the resize overlay (avoids freezing if dragging at a certain constant rate), if off, it repaints when target window repaints"
				appendMenuChecked(hMenu, immediateOverlayRepaintFlags,
					MENU_TOGGLE_IMMEDIATE_OVERLAY_REPAINT, immediateOverlayRepaintText)
			}

			{
				var missedGestureRecoveryFlags uint32 = wincoe.MF_STRING
				if missedGestureRecoveryEnabled.Load() {
					missedGestureRecoveryFlags |= wincoe.MF_CHECKED
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
				var injectButtonUpFlags uint32 = wincoe.MF_STRING
				if injectButtonUpOnMissedGestureRecovery.Load() {
					injectButtonUpFlags |= wincoe.MF_CHECKED
				}
				if !missedGestureRecoveryEnabled.Load() {
					injectButtonUpFlags |= wincoe.MF_DISABLED | wincoe.MF_GRAYED
				}
				injectButtonUpOnMissedGestureRecoveryText := "(dontuse)On missed-gesture recovery, inject a button-release early (Warning: will click LMB or RMB eg. console-paste unexpectedly)"
				appendMenuChecked(hMenu, injectButtonUpFlags,
					MENU_TOGGLE_INJECT_BUTTON_UP_ON_RECOVERY, injectButtonUpOnMissedGestureRecoveryText)
			}

			{
				var bypassWhenFullscreenFlags uint32 = wincoe.MF_STRING
				if bypassGesturesWhenFullscreen.Load() {
					bypassWhenFullscreenFlags |= wincoe.MF_CHECKED
				}
				bypassWhenFullscreenText := "Bypass all gestures when foreground window is fullscreen or borderless-fullscreen (reduces hook overhead while gaming)"

				appendMenuChecked(hMenu, bypassWhenFullscreenFlags,
					MENU_TOGGLE_BYPASS_GESTURES_WHEN_FULLSCREEN, bypassWhenFullscreenText)
			}

			{
				// Read-only diagnostic row, grayed/disabled so it can never
				// be "selected" -- it's informational only. Recomputed
				// fresh every time this menu is popped (this whole hMenu is
				// rebuilt from scratch on each WM_MYSYSTRAY RMB, so there's
				// no separate refresh mechanism needed here the way the
				// hover tooltip needs one).
				var keysHeldFlags uint32 = wincoe.MF_STRING | wincoe.MF_GRAYED | wincoe.MF_DISABLED
				keysHeldText := "Currently held (GetAsyncKeyState): " + formatHeldInputState()
				appendMenuChecked(hMenu, keysHeldFlags, MENU_SHOW_INPUT_STATE, keysHeldText)
			}

			{
				exitText := "Exit"
				appendMenuChecked(hMenu, wincoe.MF_STRING, MENU_EXIT, exitText)
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

			prevForegroundBeforeTrayMenu := windows.Handle(lastKnownUserForegroundHwnd.Load())
			restoreForegroundAfterTrayMenu := func() {
				// FIX: Only restore if our hidden window STILL has focus.
				// If it doesn't, the user dismissed the menu by clicking on (and thus focusing)
				// another window, or they Alt-Tabbed away. We must leave that new window alone!
				currentFg := getForegroundWindow()
				if currentFg != hwnd {
					return
				}

				if prevForegroundBeforeTrayMenu == 0 || prevForegroundBeforeTrayMenu == hwnd {
					return
				}
				// if resIsWin := procIsWindow.Call(uintptr(prevForegroundBeforeTrayMenu)); resIsWin.Failed() {
				// 	return
				// }
				if !wincoe.IsWindow(prevForegroundBeforeTrayMenu) {
					return
				}
				if !wincoe.SetForegroundWindow(prevForegroundBeforeTrayMenu) {
					logf("WM_MYSYSTRAY: failed to restore foreground(aka focus) to the pre-tray-menu window HWND=0x%X", prevForegroundBeforeTrayMenu)
				}
			}

			// This comes from the classic Win32 system tray workaround (Microsoft KB135788).
			// When a popup menu is created from a system tray icon, Windows requires your window to be in the foreground to route mouse/keyboard messages properly. Without the proper sequence, clicking outside the menu won't dismiss it.
			// The exact MSDN-recommended sequence is:
			// 1. Force your window to the foreground before tracking
			setForegroundWindow(hwnd, "WM_MYSYSTRAY: SetForegroundWindow(self) failed")

			//logf("DEBUG: Currently focused window is 0x%X prev:0x%X", hwnd, prevForegroundBeforeTrayMenu)

			// res2 := procTrackPopupMenu.Call(
			// 	hMenu,
			// 	TPM_RETURNCMD, //0x0100, // TPM_RETURNCMD
			// 	uintptr(pt.X),
			// 	uintptr(pt.Y),
			// 	0,
			// 	hwnd,
			// 	0,
			// )
			// res2 := wincoe.TrackPopupMenuCmd(hMenu, wincoe.TPM_RETURNCMD, pt.X, pt.Y, hwnd, nil)
			// if res2.Failed() { // it's CheckNone and
			// 	logf("in wndProc, WM_MYSYSTRAY, failed to TrackPopupMenu, err=%v", res2.Err)
			// 	restoreForegroundAfterTrayMenu()
			// 	return 0 // Handled
			// }
			// cmd := res2.R1

			// 2. Track the popup menu
			cmd, _ := wincoe.TrackPopupMenuCmd(hMenu, wincoe.TPM_RETURNCMD, pt.X, pt.Y, hwnd, nil) //nolint:errcheck // nothing to check in this case as GetLastError aka WinResult.Err might be polluted by whatever other syscalls happen during the blocking of this(ie. while systray is open until it's closed/gone)

			// Required by MSDN to dismiss menu correctly
			// 3. Post (or send) WM_NULL to your window handle
			/*
				If you are following the MSDN KB workaround, Microsoft specifically recommends PostMessage rather than SendMessage:
				PostMessage puts WM_NULL into your window's message queue asynchronously, forcing the thread's message loop to wake up and perform a context switch right as the menu loses focus.
				SendMessage executes synchronously on the spot, which can sometimes bypass the message loop flush that Windows relies on to dismiss the menu popup properly.
			*/
			//SendMessage is 100% synchronous. It does not put a message into the event queue to wait for the message loop; instead, it immediately calls your window's wndProc directly on the current thread and blocks until it returns.
			//PostMessage is asynchronous. It places WM_NULL at the very end of your thread's message queue and returns immediately.
			//so, SendMessage below: // Synchronously flushes message processing on hwnd
			_ = wincoe.SendMessage(hwnd /*yes hwnd, not hMenu!*/, wincoe.WM_NULL, 0, 0) // Send WM_NULL, cannot fail, it's also CheckNone
			restoreForegroundAfterTrayMenu()

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

			case MENU_TOGGLE_BRING_TO_FRONT_ON_BACKGROUND_CLICK:
				bringToFrontOnBackgroundClick.Store(!bringToFrontOnBackgroundClick.Load())

			case MENU_TOGGLE_UNFOCUS_SENT_TO_BACK:
				unfocusSentToBackWindow.Store(!unfocusSentToBackWindow.Load())

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

	case wincoe.WM_CLOSE:
		//exit(0)
		//WM_CLOSE → DestroyWindow() → WM_DESTROY → PostQuitMessage() -> getmessage() -> break loop -> outside of loop continuation...
		if res := wincoe.DestroyWindow(hwnd); res.Failed() {
			logf("in wndProc, WM_CLOSE: DestroyWindow failed for hwnd=0x%X, err: %v", hwnd, res)
		}
		return 0

	case wincoe.WM_DESTROY:
		// _ = procPostQuitMessage.Call(0)
		wincoe.PostQuitMessage(0 /*exit code*/)
		return 0

	case WM_EXIT_VIA_CTRL_C:
		var ctrlType uint32 = uint32(wParam)
		switch ctrlType {
		//case 0, 2: // CTRL_C_EVENT, CTRL_CLOSE_EVENT
		case wincoe.CTRL_C_EVENT:
			exitf(128, "exit via Ctrl+C")

		case wincoe.CTRL_BREAK_EVENT:
			exitf(128, "exit via Ctrl+Break")
		case wincoe.CTRL_CLOSE_EVENT:
			exitf(127, "exit via (Console Window Closed)")
		case wincoe.CTRL_LOGOFF_EVENT:
			exitf(126, "exit via (User Logoff)")
		case wincoe.CTRL_SHUTDOWN_EVENT:
			exitf(125, "exit via (System Shutdown)")
		default:
			exitf(129, "exit via unknown event %d", ctrlType)
		}
		unreachable()
	} //switch

	//let the default window proc handle the rest:
	// res1111 := procDefWindowProc.Call(hwnd, uintptr(msg), wParam, lParam)
	// if res1111.Failed() {//it's CheckNone and no real failure mode to detect!
	// 	logf("in wndProc, DefWindowProc() failed, err: %v, continuing", res1111.Err)
	// }
	// return res1111.R1 //LRESULT
	return wincoe.DefWindowProc(hwnd, msg, wParam, lParam).R1 //LRESULT
})

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
		if res := wincoe.PostThreadMessage(htidcached, wincoe.WM_QUIT, 0, 0); res.Failed() {
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

	// NOTE: deinit() runs from primary_defer(), which executes AFTER
	// runApplication()'s own `for { GetMessage(); ... }` loop has already
	// broken (it always exits via the WM_DESTROY handler's own
	// PostQuitMessage(0), called earlier while that loop was still live --
	// see WM_DESTROY in wndProc), OR via panic(). Calling PostQuitMessage(0) again here,
	// this late, would target a message queue nothing is polling with
	// GetMessage anymore on this thread: a genuine no-op, so it's been
	// removed rather than left as dead code.
	// //This puts a WM_QUIT message in the queue, which causes GetMessage to return 0 and gracefully break the loop.
	// _ = procPostQuitMessage.Call(0)
	/*
		PostThreadMessage(id, WM_QUIT, ...) literally pushes a message into the queue.

		PostQuitMessage(0) doesn't actually "post" a message immediately. It sets a internal "quit" flag in the thread's message queue.
		The next time your GetMessage loop looks for work and finds no other messages, it "synthesizes" a WM_QUIT message on the fly.
	*/
	//however, we used to be singlethreaded and then we were in the same thread that executes that loop so the chances are 0 that we get back to it and more likely that we'll os.Exit
	//but now, hmm... well we're in deinit() of the same thread so it's same thing, heh.
	if winEventHook != 0 { //FIXME: never entered here due to a 'defer' in runApplication that already unhooks it! so remove this?! and thus winEventHook itself shouldn't be needed at all!
		// res1 := procUnhookWinEvent.Call(uintptr(winEventHook))
		// if err9 != nil {
		prev := winEventHook
		winEventHook = 0
		if res1 := wincoe.UnhookWinEvent(prev); res1.Failed() {
			logf("failed UnhookWinEvent, from deinit(), res=%v", res1)
		} else {
			logf("cleaned winEventHook from deinit()")
		}
	}
}

func deinitOverlayClass() {
	if overlayHwnd != 0 {
		// Destroy the overlay window
		if res := wincoe.DestroyWindow(overlayHwnd); res.Failed() {
			logf("deinitOverlayClass: DestroyWindow failed for overlayHwnd=0x%X, res: %v", overlayHwnd, res)
		}
		overlayHwnd = 0
	}

	if magentaBrush != 0 {
		if res := wincoe.GdiDeleteObject(magentaBrush); res.Failed() {
			logf("deinitOverlayClass: DeleteObject failed for magentaBrush=0x%X: %v", magentaBrush, res.Err)
		}
		magentaBrush = 0
	}
	if blackBrush != 0 {
		if res := wincoe.GdiDeleteObject(blackBrush); res.Failed() {
			logf("deinitOverlayClass: DeleteObject failed for blackBrush=0x%X: %v", blackBrush, res.Err)
		}
		blackBrush = 0
	}

	if overlayClassRegistered.Load() { //deinit it only if it was inited ever
		// instance := uintptr(selfHInstance)
		classNamePtr := mustUTF16(winbollocksResizingOverlayClassName)
		if res2 := wincoe.UnregisterClassW(classNamePtr, selfHInstance); res2.Failed() {
			logf("deinitOverlayClass: UnregisterClassW failed for overlay class %s, res: %v", winbollocksHiddenClassName, res2)
		}
	}
}

func deinitMainMsgHwnd() {
	if prev := windows.Handle(mainMsgHwndAtomic.Swap(0)); prev != 0 {
		if res1 := wincoe.DestroyWindow(prev); res1.Failed() {
			logf("DestroyWindow failed of HWND=0x%X, (probably already destroyed or invalid) actual res: %v", prev, res1)
		}
	}

	if hiddenClassRegistered.Load() { //deinit it only if it was inited ever
		// instance := uintptr(selfHInstance)
		classNamePtr := mustUTF16(winbollocksHiddenClassName)
		if res3 := wincoe.UnregisterClassW(classNamePtr, selfHInstance); res3.Failed() {
			logf("deinitMainMsgHwnd: UnregisterClassW failed for our own hidden class named %s, res: %v", winbollocksHiddenClassName, res3)
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

// ctrlCHandlerFired guards ctrlCHandler so that only the FIRST invocation
// posts WM_EXIT_VIA_CTRL_C to the main thread. Per SetConsoleCtrlHandler's
// documented threading model, Windows spawns a brand-new OS thread for
// EVERY console control event delivered to a registered handler, so a
// user hammering Ctrl+C (or Ctrl+C immediately followed by closing the
// console window) can genuinely invoke ctrlCHandler concurrently from
// multiple threads while the first invocation's WM_EXIT_VIA_CTRL_C is
// still sitting in the queue or being processed. Nothing downstream of
// this function actually crashes on repeat invocations -- PostMessage is
// safe to call any number of times, and wndProc's WM_EXIT_VIA_CTRL_C
// handler's exitf() panics and unwinds the main message loop the first
// time it runs, so a second queued copy would never even be dispatched --
// but there's no reason to post (or log) more than once, so this makes
// "only react to the first control event" explicit rather than accidental.
var ctrlCHandlerFired atomic.Bool

// done: keep this for the devbuild.bat mode?! ie. when having console!
func ctrlCHandler(ctrlType uint32) uintptr {
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
	if !ctrlCHandlerFired.CompareAndSwap(false, true) {
		// Already handled a prior control event; this one arrived on its
		// own new OS thread (see doc comment above). Still report
		// "handled" (return 1) so Windows doesn't fall through to default
		// handling -- e.g. an unhandled CTRL_CLOSE_EVENT would terminate
		// the process immediately, bypassing our own graceful shutdown
		// already in flight from the first invocation.
		return 1
	}

	if now := loadMainMsgHwnd(); now != 0 {
		if res := wincoe.PostMessage(
			now,
			WM_EXIT_VIA_CTRL_C,
			uintptr(ctrlType),
			0,
		); res.Failed() {
			logf("ctrlCHandler: PostMessage WM_EXIT_VIA_CTRL_C to main msg hwnd 0x%X failed, err: %v", now, res.Err)
		}
	} else {
		//doneFIMXE: maybe logf is dead? i forget if we close it or just ignore new msgs? but once the worker is done we won't be seeing this then? so use directlogger? this means it's not gonna be logged to file.
		directLoggerf("ctrlCHandler: the main msg hwnd is 0 (shutdown already in progress?); thus didn't re-signal the main hwnd for shutdown!")
	}
	return 1 // 1=true aka i handled this event ie. don't do the default handling which would exit.
}

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
	// logFile is guarded by logFileOnce (ensuring exactly one os.OpenFile
	// call ever happens, avoiding a leaked duplicate file handle) and
	// stored behind an atomic.Pointer (matching wincoe.Logger's identical
	// pattern) because internalLogger can run concurrently from logWorker's
	// own goroutine AND, once logQuitClosed is set, directly from ANY
	// calling goroutine's logf() (hook thread, main thread, timer
	// goroutines) -- a plain *os.File package var here would be a genuine,
	// -race-detectable data race between those callers.
	logFile     atomic.Pointer[os.File]
	logFileOnce sync.Once
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
	logFileOnce.Do(func() {
		// 1. Check if the batch script provided a log file path
		logFilename := os.Getenv(selfName + "_log_file") // aka "winbollocks_log_file" env. var. which depends on readcfg.env 's key value being this! and readcfg.bat reading it and run.bat the top caller thus using it(the env. var.!)
		if logFilename == "" {
			// Fallback just in case it's run directly without the .bat
			logFilename = selfName + "_debug.log"
		}

		// #nosec: G302 // we want 0644 not 0600 because winbollocks runs as admin usually and want user to can read the log without becoming admin to do so.
		f, err := os.OpenFile( // FIXME: G703: Path traversal via taint analysis (gosec)
			logFilename,
			os.O_CREATE|os.O_WRONLY|os.O_APPEND,
			0644,
		)
		if err == nil {
			logFile.Store(f)
		}
		// on error, logFile stays nil; internalLogger's caller already
		// handles that by falling back to a no-op.
	})
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

	// // 2. Note the problem if we exhausted the 100 tries
	// if !wentAccordingToPlan {
	// 	// We failed to record the peak after 100 tries.
	// 	// Increment a "Contention Error" counter
	// 	panic(fmt.Sprintf("Failed(%d times) to set an atomic to int64 value %d. Happened during this log msg: '%s'", attemptAtomicSwapThisManyTimes, currentDepth, finalMsg))
	// }

	// 2. Note the problem if we exhausted the 100 tries. Telemetry must
	// never crash the app (see logDepthCASFailures' doc comment) -- count
	// it and move on instead of panicking.
	if !wentAccordingToPlan {
		logDepthCASFailures.Add(1)
	}
}

func injectLetterE() {
	injectKeyTap('E')
}

func injectKeyTap(vk uint16) {
	inputs := []wincoe.KEYANDMOUSE_INPUT{
		{
			Type: wincoe.INPUT_KEYBOARD,
			Ki: wincoe.KEYBDINPUT{
				WVk: vk,
			},
		},
		{
			Type: wincoe.INPUT_KEYBOARD,
			Ki: wincoe.KEYBDINPUT{
				WVk:     vk,
				DwFlags: wincoe.KEYEVENTF_KEYUP,
			},
		},
	}

	// res1 := procSendInput.Call(
	// 	uintptr(len(inputs)),
	// 	uintptr(unsafe.Pointer(&inputs[0])),
	// 	unsafe.Sizeof(inputs[0]),
	// )
	if res1 := wincoe.SendInput(inputs); res1.Failed() || res1.R1 != uintptr(len(inputs)) {
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
//nCode being int32 not int: "Matches the Win32 C Spec: In Microsoft's C header (winuser.h), nCode is defined as a standard C int. On Windows (both 32-bit and 64-bit x64), a C int is strictly 32 bits signed."
func keyboardProc(nCode int32, wParam uintptr, lParam unsafe.Pointer) uintptr {
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
		//res1 := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(lParam))
		res1 := wincoe.CallNextHookEx(0, nCode, wParam, uintptr(lParam))
		return res1.R1
	}

	//no effect: //nolint:govet,unsafeptr // Win32 hook lParam is OS-owned pointer valid for callback duration
	//k := (*KBDLLHOOKSTRUCT)(unsafe.Pointer(lParam))

	k := (*wincoe.KBDLLHOOKSTRUCT)(lParam)
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
	if k.Flags&wincoe.LLKHF_INJECTED != 0 {
		// This key event was generated by SendInput
		// Do NOT treat it as user input
		//res2 := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(lParam))
		res2 := wincoe.CallNextHookEx(0, nCode, wParam, uintptr(lParam))
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

	// Key DOWN
	if wParam == wincoe.WM_KEYDOWN || wParam == wincoe.WM_SYSKEYDOWN {
		if vk == wincoe.VK_ESCAPE && tryCancelActiveGestureViaEsc() {
			// Swallow ESC entirely: the target window under an in-progress
			// winkey+LMB/RMB gesture never saw the original button-down (it
			// was swallowed at gesture start -- see mouseProc's
			// WM_LBUTTONDOWN/WM_RBUTTONDOWN handling), so letting it see a
			// bare ESC now could trigger unrelated behavior (e.g. canceling
			// a text selection or closing a dialog) despite nothing else
			// about this gesture ever having reached it.
			return 1
		}
		if vk == wincoe.VK_SHIFT || vk == wincoe.VK_LSHIFT || vk == wincoe.VK_RSHIFT {
			// Checking all three: the low-level keyboard hook has, across
			// different Windows versions/input paths, been observed to
			// report either the left/right-specific VK code or the
			// generic VK_SHIFT for a physical Shift press -- react to
			// whichever one actually arrives rather than assuming one.
			postShiftMirrorToggleIfNeeded(true)
		}
	}

	// Key UP
	if wParam == wincoe.WM_KEYUP || wParam == wincoe.WM_SYSKEYUP {
		if vk == wincoe.VK_SHIFT || vk == wincoe.VK_LSHIFT || vk == wincoe.VK_RSHIFT {
			postShiftMirrorToggleIfNeeded(false)
		}
		switch vk {
		case wincoe.VK_LWIN, wincoe.VK_RWIN:
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

				if main := loadMainMsgHwnd(); main != 0 {
					if res := wincoe.PostMessage(
						main,
						WM_INJECT_SEQUENCE,
						uintptr(vk), // VK_LWIN or VK_RWIN,
						0,
					); res.Failed() {
						// Can't recover from inside this low-level keyboard hook; the shift-tap
						// injection that suppresses Start menu simply won't happen this time.
						logf("keyboardProc: PostMessage WM_INJECT_SEQUENCE at end of gesture, failed, err: %v; Start menu may(unlikely tho) pop up, but probably won't because the vkE8 was injected when gesture started", res.Err)
					}
				} else {
					logf("keyboardProc: PostMessage WM_INJECT_SEQUENCE at end of gesture, failed, mainMsgHwnd was 0")
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

	//res1111 := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(lParam))
	res1111 := wincoe.CallNextHookEx(0, nCode, wParam, uintptr(lParam))
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

	if got := unsafe.Sizeof(wincoe.KEYANDMOUSE_INPUT{}); got != expectedINPUT {
		badprogramming(fmt.Sprintf(
			"INPUT ABI size mismatch: Go struct is %d bytes, Win32 x64 expects %d — SendInput will be broken",
			got, expectedINPUT,
		))
	}
	if got := unsafe.Sizeof(wincoe.KEYBDINPUT{}); got != expectedKEYBDINPUT {
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
	focusOnDrag.Store(bringToFrontByDefaultOnGesture)          // focus window when gesture applies on it, ie. on drag-Move
	bringToFrontOnDrag.Store(bringToFrontByDefaultOnGesture)   // bring dragged window to front of Z-order, same default as focus above
	focusOnResize.Store(bringToFrontByDefaultOnGesture)        // resize's independent counterpart to focusOnDrag
	bringToFrontOnResize.Store(bringToFrontByDefaultOnGesture) // resize's independent counterpart to bringToFrontOnDrag
	// Default to true: clicking a focused-but-backgrounded window (e.g. one
	// sent to back via winkey+MMB) should bring it back to the top of the
	// Z-order rather than leaving it stuck behind everything else.
	bringToFrontOnBackgroundClick.Store(true)

	unfocusSentToBackWindow.Store(true) // default on;-- see its doc comment

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

var mutexHandle windows.Handle

func releaseSingleInstance() {
	if mutexHandle != 0 {
		//defer is tied to the function, not to inner scopes, so it happens only when this func. exits!
		//defers do run when a panic happens
		defer func() { mutexHandle = 0 }() //executes third

		//"Failing to call CloseHandle results in a kernel handle leak, which slowly exhausts system resources if repeated."
		defer closeHandleLogged(mutexHandle, "releaseSingleInstance mutexHandle") //executes second
		// func() {
		// 	//If procReleaseMutex.Call somehow panics (unlikely, but possible with corrupted memory), this is in a defer

		// 	// Close handle so other instances can acquire
		// 	//procCloseHandle.Call(mutexHandle)
		// 	res2 := procCloseHandle.Call(mutexHandle)
		// 	// if r2 == 0 {
		// 	if res2.Failed() {
		// 		logf("CloseHandle failed: %v", res2.Err)
		// 	}
		// }()

		//executes first
		// Release ownership if we own it
		//procReleaseMutex.Call(mutexHandle)
		// res1 := procReleaseMutex.Call(mutexHandle)
		res1 := wincoe.ReleaseMutex(mutexHandle)
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
	// res1 := procCreateMutex.Call(0, 1, uintptr(unsafe.Pointer(namePtr)))
	mH, res1 := wincoe.CreateMutex(nil,
		true, //"You must acquire ownership of the mutex before you can release it."
		namePtr)

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
	//didn't fail, succeeded, but GetLastError() aka res1.CallStatus can still be set:
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
	// mutexHandle = windows.Handle(res1.R1) // aka ret
	mutexHandle = mH
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
	// Compile-time assertion: fails to compile if modVal <= 1
	// Uses a division by zero error if modVal <= 1, which halts compilation cleanly.
	const _ = 0 / (modVal - 1) // if compile-error set modVal to be > 1 !

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
	if casFailures := logDepthCASFailures.Load(); casFailures > 0 {
		directLoggerf("High-water-mark CAS loop exhausted its %d retries %s time(s) (never fatal, just an imprecise peak).", attemptAtomicSwapThisManyTimes, withCommas(casFailures))
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

	lf := logFile.Load()
	if lf == nil {
		initLogFile()
		lf = logFile.Load()
		if lf == nil {
			return
		}
	}

	_, err := fmt.Fprintf(lf, "%s", finalMsg)
	if err != nil && canUseConsoleStderr {
		fmt.Fprintf(os.Stderr, "!!! Err:'%v', Couldn't write to logFile %q the logline: %s", err, lf.Name(), finalMsg)
	}
	// --- START SYNC TIMING ---
	syncStart := time.Now()
	err2 := lf.Sync()
	syncDur := time.Since(syncStart)
	// --- END SYNC TIMING ---
	if err2 != nil && canUseConsoleStderr {
		fmt.Fprintf(os.Stderr, "!!! Err:'%v', Couldn't sync logFile %q after writing to it this logline: %s", err2, lf.Name(), finalMsg)
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
		_, err3 := fmt.Fprintf(lf, "%s", warnMsg)
		if err3 != nil {
			fmt.Fprintf(os.Stderr, "!!! failed to write to logFile %q too (err:%v) the warn msg: %q", lf.Name(), err3, warnMsg)
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

// func getConsoleWindow() (windows.HWND, error) {
// 	// res1 := procGetConsoleWindow.Call()
// 	// hwnd := windows.HWND(res1.R1)
// 	hwnd := wincoe.GetConsoleWindow()

// 	if hwnd == 0 {
// 		// syscall wrappers often return err == "The operation completed successfully."
// 		// when no failure occurred, so treat that as nil.
// 		// if err != nil && err != windows.ERROR_SUCCESS {
// 		// if res1.Failed() {//it's CheckNone, so useless to check here!
// 		// 	return 0, fmt.Errorf("in getConsoleWindow, GetConsoleWindow() failed, err=%w", res1.Err)
// 		// }

// 		// No console is a normal state, not an error.
// 		return 0, nil
// 	}

// 	return hwnd, nil
// }

func hasRealConsole() bool {
	// hwnd, err := getConsoleWindow()
	// if err != nil {
	// 	return false
	// }
	return wincoe.GetConsoleWindow() != 0
}

func installCtrlHandlerIfConsole() {
	if !hasRealConsole() {
		logf("No console, not installing Ctrl+C handler")
		return
	} else {
		logf("Installing Ctrl+C handler due to having console.")
	}
	if res := wincoe.RegisterCtrlHandler(ctrlCHandler); res.Failed() { // this doesn't work(ie. has no console) for: go build -mod=vendor -ldflags="-H=windowsgui" .
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
	//resFg := procGetForegroundWindow.Call()
	//windows.Handle(resFg.R1)
	startupTerminalHwnd = getForegroundWindow()

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

	wincoe.InitDPIAwareness(logf) //If you call it after window creation, it does nothing.

	mainThreadID = windows.GetCurrentThreadId() //XXX: it's set before 'go hookWorker()' below
	logf("main loop thread started. ThreadID: %d", mainThreadID)

	hwnd, err3 := createMessageWindow() //doneTODO: how to undo this via defer or something?!
	if err3 != nil {
		//exitf(1, "Failed to create message window: %v", err)
		return fmt.Errorf("failed to create message window: %w", err3)
	}
	storeMainMsgHwnd(hwnd)

	if err4 := initTray(); err4 != nil {
		return fmt.Errorf("failed to init tray: %w", err4)
	}

	// if res := procWTSRegisterSessionNotification.Call(uintptr(mainMsgHwnd), NOTIFY_FOR_THIS_SESSION); res.Failed() {
	if res := wincoe.WTSRegisterSessionNotification(hwnd, wincoe.NOTIFY_FOR_THIS_SESSION); res.Failed() {
		logf("WTSRegisterSessionNotification failed, err: %v; lock/unlock-triggered stale-session cleanup (see WM_WTSSESSION_CHANGE in wndProc) will be unavailable this run.", res.Err)
	} else {
		defer func() {
			// if res2 := procWTSUnRegisterSessionNotification.Call(uintptr(mainMsgHwnd)); res2.Failed() {
			if res2 := wincoe.WTSUnRegisterSessionNotification(hwnd); res2.Failed() {
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

	// if res1 := procSetWinEventHook.Call(
	// 	uintptr(EVENT_SYSTEM_FOREGROUND), //0x0003, // EVENT_SYSTEM_FOREGROUND min
	// 	//0x0003, // max
	// 	uintptr(EVENT_OBJECT_FOCUS), // max; spans 0x4xxx console band too //0x8005, // EVENT_OBJECT_FOCUS (Catch lower-level focus shifts)

	// 	0, // hmod = 0 (out-of-context callback)
	// 	winEventCallback,
	// 	0, // idProcess = 0 (all)
	// 	0, // idThread = 0 (all)
	// 	uintptr(WINEVENT_OUTOFCONTEXT|WINEVENT_SKIPOWNPROCESS), //0x0000|0x0002, // WINEVENT_OUTOFCONTEXT | WINEVENT_SKIPOWNPROCESS
	// ); res1.Failed() { //err != nil || h == 0 {
	if theEvHook, res1 := wincoe.SetWinEventHook(
		wincoe.EVENT_SYSTEM_FOREGROUND, //min
		wincoe.EVENT_OBJECT_FOCUS,      //max
		0,                              // hmod = 0 (out-of-context callback)
		winEventProc,
		0, // idProcess = 0 (all)
		0, // idThread = 0 (all)
		wincoe.WINEVENT_OUTOFCONTEXT|wincoe.WINEVENT_SKIPOWNPROCESS,
	); res1.Failed() {
		logf("SetWinEventHook failed, hooking of winEventHook, from main thread: %v", res1.Err)
	} else {
		// theEvHook := windows.Handle(res1.R1)
		defer func() {
			// prev:=
			winEventHook = 0
			// res2 := procUnhookWinEvent.Call(uintptr(winEventHook))
			// if err2 != nil {
			if res2 := wincoe.UnhookWinEvent(theEvHook); res2.Failed() {
				logf("UnhookWinEvent failed unhooking of winEventHook, from main thread, err: %v", res2.Err)
			}
			logf("normal unhooking of winEventHook, from main thread")
		}()
		winEventHook = theEvHook
		logf("SetWinEventHook: hooked into focus events")
		initForegroundIntegrityState() //"This runs synchronously, single-threaded, before the message loop starts pumping — so there's no race with winEventProc itself (it literally can't fire yet)." - Claude
	}

	if err5 := initOverlay(); err5 != nil {
		return fmt.Errorf("failed to initOverlay which is what's displayed when resizing, err: %w", err5)
	}

	//You should call lockRAM() at the very end of your initialization sequence, but before you enter the main message loop (GetMessage).
	lockRAM()
	var msg wincoe.MSG
	for {
		/* GetMessage is the "Event-Driven" king.
		   It puts this thread to sleep at 0% CPU.
		   It only wakes up if:
		   1. A real Windows message (Key, Exit, Window Move) arrives.
		   2. Our Hook sends the WM_WAKE_UP "Doorbell".
		*/
		// res3 := procGetMessage.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		// if int32(r) <= 0 {
		if res3 := wincoe.GetMessage(&msg, 0, 0, 0); res3.Failed() /*aka res3.Err < 0*/ || res3.R1 == wincoe.WM_QUIT /*aka WM_QUIT*/ {
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

		// procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		_ = wincoe.TranslateMessage(&msg) // nolint:errcheck // don't care
		// procDispatchMessage.Call(uintptr(unsafe.Pointer(&msg)))
		resDis := wincoe.DispatchMessage(&msg)
		if resDis.CallStatusFailed() {
			logf("DEBUG: in runApplication, last GetLastError() seen by DispatchMessage is %v", resDis.CallStatus)
		}
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

// Define this at the top level (global)
var (
	// Ensure it's not optimized away by making it a package-level variable
	integrityCheckVar int64 = 0xDEADC0DE
	/*
			1. Zero Heap Allocations

		If you move integrityCheckVar inside the function, taking its address (&integrityCheckVar) forces the Go compiler's escape analysis to allocate it on the heap every single time verifyMemoryIsLocked() runs.

		By keeping it at the package level:

		    It resides permanently in the executable's static data segment (.data).

		    Invoking verifyMemoryIsLocked() causes 0 bytes of memory allocation and zero GC overhead.

		2. Immutable Memory Location

		A global variable's virtual memory address is fixed at process launch and never changes. This gives you a rock-solid, permanent target to test whether your application's base code/data space is currently paged into physical RAM.
		3. Eliminates Compiler Optimization Risks

		Compilers are aggressive about optimizing unused or read-only local variables. While runtime.KeepAlive stops premature collection, keeping the sentinel variable at package scope guarantees the compiler will treat it as a real, non-elided storage location across all optimization passes.
	*/
)

func verifyMemoryIsLocked() {
	//var testVar int = 42 // Variable we want to check, bad, on stack always hot.
	hProc := wincoe.GetCurrentProcess()

	// PSAPI_WORKING_SET_EX_INFORMATION
	// This tells Windows: "Tell me about the physical state of this specific address"
	// info := wincoe.PSAPI_WORKING_SET_EX_INFORMATION{
	// 	VirtualAddress: uintptr(unsafe.Pointer(&integrityCheckVar)),
	// }
	// Query a single address cleanly:
	entries := []wincoe.PSAPI_WORKING_SET_EX_INFORMATION{
		{VirtualAddress: uintptr(unsafe.Pointer(&integrityCheckVar))},
	}

	// res1 := procQueryWorkingSetEx.Call(
	// 	hProc,
	// 	uintptr(unsafe.Pointer(&info)),
	// 	unsafe.Sizeof(info),
	// )

	//if ret == 0 {
	if res1 := wincoe.QueryWorkingSetEx(hProc, entries); res1.Failed() {
		logf("in verifyMemoryIsLocked, failed QueryWorkingSetEx, res: %v", res1)
		return
	}
	// Explicitly keep the variable alive until after the Win32 call completes
	//runtime.KeepAlive is completely sufficient for this case—even if the variable is a local variable inside a function rather than a package-global.
	runtime.KeepAlive(&integrityCheckVar)

	if !entries[0].VirtualAttributes.IsValid() {
		//		logf("Verification: Memory at 0x%X is currently resident in RAM.", info.VirtualAddress)
		//} else {
		logf("Verification: Memory at 0x%X is currently PAGED OUT. This is unexpected!", entries[0].VirtualAddress)
	}
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
	hProc := wincoe.GetCurrentProcess()

	//To successfully increase your working set, you often need the SE_INC_WORKING_SET_NAME privilege. Simply calling the API might fail silently or return "Access Denied."
	// 1. Enable the Privilege
	var token windows.Token
	// res1 := procOpenProcessToken.Call(hProc, TOKEN_ADJUST_PRIVILEGES|TOKEN_QUERY, uintptr(unsafe.Pointer(&token)))
	res1 := wincoe.OpenProcessToken(hProc, wincoe.TOKEN_ADJUST_PRIVILEGES|wincoe.TOKEN_QUERY, &token)
	//if err == nil || ret != 0 {
	if res1.Succeeded() {
		var luid wincoe.LUID
		lpName, err4 := windows.UTF16PtrFromString(wincoe.SE_INC_WORKING_SET_NAME)
		if err4 != nil {
			logf("failed UTF16PtrFromString on %q, err='%v', continuing tho.", wincoe.SE_INC_WORKING_SET_NAME, err4)
		} else {
			//res2 := procLookupPrivilegeValue.Call(0, uintptr(unsafe.Pointer(lpName)), uintptr(unsafe.Pointer(&luid)))
			if res2 := wincoe.LookupPrivilegeValue(nil, lpName, &luid); res2.Failed() {
				logf("failed procLookupPrivilegeValue %q, err: '%v', continuing tho.", wincoe.SE_INC_WORKING_SET_NAME, res2.Err)
			} else {
				//if err2 == nil || ret2 != 0 {
				//if res2.Succeeded() {
				tp := wincoe.TOKEN_PRIVILEGES{
					PrivilegeCount: 1,
					Privileges: [1]wincoe.LUID_AND_ATTRIBUTES{
						{Luid: luid, Attributes: wincoe.SE_PRIVILEGE_ENABLED},
					},
				}
				// AdjustTokenPrivileges returns success even if it partially fails,
				// so we must check GetLastError (err) specifically.
				// res3 := procAdjustTokenPrivileges.Call(token, 0, uintptr(unsafe.Pointer(&tp)), 0, 0, 0)
				//if err3 != nil || ret3 == 0 || !errors.Is(err3, windows.Errno(0)) {
				if res3 := wincoe.AdjustTokenPrivileges(token, false, &tp, 0, nil, nil); res3.Failed() { // uses CheckAdjustTokenPrivileges
					logf("Warning: Could not enable %q, err: '%v', callStatus: '%v', ret: '%d', continuing tho.",
						wincoe.SE_INC_WORKING_SET_NAME, res3.Err, res3.CallStatus, res3.R1)
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
	var min2 uintptr = 20 * 1024 * 1024
	var max2 uintptr = 50 * 1024 * 1024

	// res4 := procSetProcessWorkingSetSize.Call(hProc, uintptr(min2), uintptr(max2))
	//if ret4 == 0 {
	if res4 := wincoe.SetProcessWorkingSetSize(hProc, min2, max2); res4.Failed() {
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

func humanBytes(bytes uintptr) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}
	const unit uintptr = 1024
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

// func getCurrentThread() (hThread uintptr) {
// 	//Note that GetCurrentThread also returns a pseudo-handle (usually -2), so it doesn't need to be closed either.
// 	// res1 := procGetCurrentThread.Call()
// 	// currThread := res1.R1
// 	hThread = wincoe.GetCurrentThread()
// 	// See the identical comment in getCurrentProcess() above; procGetCurrentThread
// 	// is bound with wincoe.CheckEquals(CURRENT_THREAD_PSEUDO_HANDLE).
// 	// if res1.Failed() {
// 	// 	exitf(1, "Critical: getCurrentThread returned 0x%X, err: %v, callStatus: %v", currThread, res1.Err, res1.CallStatus)
// 	// }
// 	// return currThread
// 	return
// }

// required high prio(normal is stuttering) to avoid mouse stuttering during the whole Gemini AI website version reply in Firefox.
// "By being "High Priority," you tell the Windows Scheduler that your thread should have a longer quantum (more time before being interrupted)
// and a shorter wait time to be re-scheduled. It ensures that when the "Mouse Interrupt" fires, your Go code is ready to answer the door immediately."
func setAndVerifyPriority() {
	hProc := wincoe.GetCurrentProcess()

	// Set to HIGH_PRIORITY_CLASS (0x80)
	const wantedProcessPrio uint32 = wincoe.HighPriorityClass
	// res1 := procSetPriorityClass.Call(hProc, wantedProcessPrio)
	//if ntStatus == 0 {
	if res1 := wincoe.SetPriorityClass(hProc, wantedProcessPrio); res1.Failed() {
		logf("Failed to set process priority class to 0x%x, err:%v", wantedProcessPrio, res1.Err)
		//return
	}

	// Verify it actually changed
	prio, res2 := wincoe.GetPriorityClass(hProc)
	if res2.Failed() {
		logf("Failed to get process priority, err:%v", res2.Err)
	}
	// prio := res2.R1
	if prio == wantedProcessPrio {
		logf("Process priority confirmed: 0x%x where 0x%x is Normal.", wantedProcessPrio, wincoe.NormalPriorityClass)
	} else {
		logf("Priority mismatch! OS returned prio: 0x%x instead of 0x%x and err was: %v, callStatus: %v", prio, wantedProcessPrio, res2.Err, res2.CallStatus)
	}

	const wantedThreadPrio int32 = wincoe.ThreadPriorityTimeCritical
	//By setting the thread prio to 15, you are at the absolute ceiling of the "Dynamic" priority range.
	// Only "Realtime" processes can go higher (16–31). This ensures that even if your Go app's other threads
	// (like the one doing logging or tray icon management) get bogged down, the thread handling the mouse hook has a "VIP pass" at the CPU's door.

	// currThread := getCurrentThread()
	currThread := wincoe.GetCurrentThread()

	//In Go, the Garbage Collector runs on background threads. If your Process Priority is High (13) but your Hook Thread is Time Critical (15),
	// the Hook Thread will actually preempt the Go Garbage Collector if they both want the CPU at the same time.
	//This is the secret sauce for low-latency Go on Windows: you've made the hook more important than the language's own housekeeping.
	// - gemini 3 fast
	//The Process is High, but the Hook Thread (current thread) is "Time Critical." This ensures that even if your Go app starts doing a heavy Garbage Collection on another thread,
	// the Hook Thread gets the absolute maximum "right of way."
	// res3 := procSetThreadPriority.Call(currThread, uintptr(wantedThreadPrio))
	//if tRet == 0 {
	if res3 := wincoe.SetThreadPriority(currThread, wantedThreadPrio); res3.Failed() {
		logf("Failed to set thread priority, res: %v", res3)
	} else {
		// Verify Thread Priority
		// res4 := procGetThreadPriority.Call(currThread)
		tprio, res4 := wincoe.GetThreadPriority(currThread)
		if res4.Failed() {
			logf("setAndVerifyPriority:GetThreadPriority, failed to get thread priority, res:%v", res4)
			//so tprio here is 0x7fffffff aka THREAD_PRIORITY_ERROR_RETURN
		}
		// #nosec G115 -- safe: Win32 thread priorities are small integers that fit in int32
		// tprio := int32(res4.R1)

		// GetThreadPriority returns an int. 15 is TIME_CRITICAL.
		if tprio == wantedThreadPrio {
			logf("Thread Priority confirmed: %d", tprio)
		} else {
			logf("Thread Priority mismatch! OS returned prio: %d instead of %d", tprio, wantedThreadPrio)
		}
	}

	//FIXME: so since memprio and i/o prio below aren't set to anything different than normal, maybe don't try to set them at all ie. remove the code doing it!

	// --- Memory Priority (Using Kernel32) ---
	// this is so we don't get paged out to swap/pagefile
	var wantedMemPrio uint32 = 5 // 6 is Very High(doesn't work, it fails w/ invalid param!), 5 is the value i saw in process explorer if nothing's setting it at all.

	wantedType := wincoe.PROCESS_MEMORY_PRIORITY
	memPrio := wincoe.MEMORY_PRIORITY_INFORMATION{MemoryPriority: wantedMemPrio}

	// res5 := procSetProcessInformation.Call(
	// 	hProc,
	// 	uintptr(wantedType), // 0
	// 	uintptr(unsafe.Pointer(&memPrio)),
	// 	unsafe.Sizeof(memPrio),
	// )

	if res5 := wincoe.SetProcessInformation(hProc, wantedType, unsafe.Pointer(&memPrio), unsafe.Sizeof(memPrio)); res5.Succeeded() {
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
	var ioHint uint32 = wincoe.IO_PRIORITY_NORMAL //aka 2 works as it's the default anyway.
	// Note: NtSetInformationProcess returns an NTSTATUS, where 0 is STATUS_SUCCESS
	// res6 := procNtSetInformationProcess.Call(
	// 	hProc,
	// 	uintptr(wincoe.PROCESS_IO_PRIORITY), //33
	// 	uintptr(unsafe.Pointer(&ioHint)),
	// 	unsafe.Sizeof(ioHint),
	// )
	//if ntStatus != 0 {
	if res6 := wincoe.NtSetInformationProcess(hProc, wincoe.PROCESS_IO_PRIORITY,
		unsafe.Pointer(&ioHint), unsafe.Sizeof(ioHint)); res6.Failed() {
		logf("Failed NtSetInformationProcess (I/O), NTSTATUS is in R1, res: %v", res6)
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
	var rect wincoe.RECT
	//res := procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&rect)))
	res := wincoe.GetWindowRect(hwnd, &rect)
	return res.Succeeded()
}

// TODO: shall we make these toggles in systray? probably not, it's spammy debug!
var shouldLogFocusChanges = false
var shouldLogWindowEvents = false

var (
	// XXX: You already safely manage eventCount using atomic.AddUint64 and atomic.SwapUint64, which is good practice. Because you passed WINEVENT_OUTOFCONTEXT to SetWinEventHook, Windows delivers those callbacks to the message queue of the thread that registered the hook (the Main Thread). Therefore, lastReport and eventCount are technically single-threaded in this specific architecture, but keeping the atomics there is a smart defensive move against future refactors.
	// eventCount uses the typed atomic.Uint64 (matching droppedMoveOrResizeEvents
	// and friends elsewhere in this file) rather than the older
	// atomic.AddUint64/SwapUint64 free-function API, for consistency and to
	// rule out any accidental non-atomic read slipping in later. Because you
	// passed WINEVENT_OUTOFCONTEXT to SetWinEventHook, Windows delivers
	// these callbacks to the message queue of the thread that registered
	// the hook (the Main Thread), so lastReport/eventCount are technically
	// single-threaded here regardless, but the atomic stays a smart
	// defensive move against future refactors.
	//well, read the comment for winEventProc below (didn't double check it, Geminit 3.5 Flash made)
	eventCount atomic.Uint64
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
	if idObject != wincoe.OBJID_WINDOW { // 0 is OBJID_WINDOW
		return 0 // WinEvent callbacks return 0 (no chaining)
	}

	//fmt.Println("DEBUG: hook called")
	var nowCount uint64
	if shouldLogWindowEvents {
		//nowCount = atomic.AddUint64(&eventCount, 1)
		nowCount = eventCount.Add(1)
	}

	var eventName string = "unclassified&untracked"
	var untrackedEvent bool = false

	switch event {
	case wincoe.EVENT_SYSTEM_FOREGROUND: //0x0003:
		eventName = "EVENT_SYSTEM_FOREGROUND"
	case wincoe.EVENT_SYSTEM_CAPTURESTART: //0x0008:
		eventName = "EVENT_SYSTEM_CAPTURESTART"
		// fg := getForegroundWindow()
		// logf("CaptureStart: FG=0x%x eventHWND=0x%x", fg, hwnd)

		// time.AfterFunc(20*time.Millisecond, func() {
		// 	logf("20ms later FG=0x%x", getForegroundWindow())
		// })

		// time.AfterFunc(100*time.Millisecond, func() {
		// 	logf("100ms later FG=0x%x", getForegroundWindow())
		// })
	case wincoe.EVENT_SYSTEM_CAPTUREEND: //0x0009:
		eventName = "EVENT_SYSTEM_CAPTUREEND"
	case wincoe.EVENT_CONSOLE_UPDATE_REGION: //0x4002:
		//This fires when an object (window, button, menu item) is made visible. During a Regedit search,
		// it might fire if the UI is dynamically popping elements in and out of the view.
		eventName = "EVENT_CONSOLE_UPDATE_REGION"
		untrackedEvent = true
	case wincoe.EVENT_CONSOLE_LAYOUT: // 0x4005:
		//It fires every time a window or an element moves or changes size.
		eventName = "EVENT_CONSOLE_LAYOUT"
		untrackedEvent = true
	case wincoe.EVENT_OBJECT_CREATE: //0x8000:
		eventName = "EVENT_OBJECT_CREATE"
		untrackedEvent = true
	case wincoe.EVENT_OBJECT_DESTROY: //0x8001:
		eventName = "EVENT_OBJECT_DESTROY"
		untrackedEvent = true
	case wincoe.EVENT_OBJECT_SHOW: //0x8002:
		eventName = "EVENT_OBJECT_SHOW"
	case wincoe.EVENT_OBJECT_HIDE: // 0x8003:
		eventName = "EVENT_OBJECT_HIDE"
	case wincoe.EVENT_OBJECT_REORDER: //0x8004:
		eventName = "EVENT_OBJECT_REORDER"
	case wincoe.EVENT_OBJECT_FOCUS: // 0x8005:
		eventName = "EVENT_OBJECT_FOCUS"
	default:
		// Return early if it's an event we aren't tracking to keep logs clean
		untrackedEvent = true
	}

	if shouldLogWindowEvents {
		// 1. Monitor Event Frequency (Every 1 second)
		if time.Since(lastReport) > time.Second && nowCount > 160 { //TODO: make it a const; can get 122 events per sec during resizes, or less than 50 during wtw else not-our-gesture events.
			//count := atomic.SwapUint64(&eventCount, 0)
			count := eventCount.Swap(0)
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
		event == wincoe.EVENT_SYSTEM_CAPTURESTART // || event == EVENT_SYSTEM_CAPTUREEND || event == EVENT_OBJECT_FOCUS) // these two aren't needed, and last one isn't hit anyway!

	var pid uint32
	targetHwnd := hwnd

	if shouldLogFocusChanges || event == wincoe.EVENT_SYSTEM_FOREGROUND || forceReconcile {
		// If reconciling, the event's 'hwnd' might just be a child element.
		// We want the absolute master foreground window to bypass the glitch.
		if forceReconcile && event != wincoe.EVENT_SYSTEM_FOREGROUND {
			//res1 := procGetForegroundWindow.Call()
			//targetHwnd = windows.Handle(res1.R1)
			targetHwnd = getForegroundWindow()
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

		// procGetWindowThreadProcessID.Call(uintptr(targetHwnd), uintptr(unsafe.Pointer(&pid)))
		// //"Pro-tip: You don't need to check err for this specific API because it doesn't set LastError in the traditional way; you just check if the return value (or the written pid variable) is 0. Your current check if pid == 0 is the correct way to handle it." - gemini 3 Fast
		_, res := wincoe.GetWindowThreadProcessId(targetHwnd, &pid)
		if res.Failed() || pid == 0 {
			//some error or wtw
			logf("Couldn't get pid(it's %d) for HWND=0x%x for event 0x%x(%s), res:%v", pid, targetHwnd, event, eventName, res)
			return 0 // WinEvent callbacks return 0 (no chaining)
		}
	}

	if shouldLogFocusChanges {
		// Get the top-level owner of this HWND to see if it belongs to CMD
		// GA_ROOT (2) gets the "real" parent window
		// res1 := procGetAncestor.Call(uintptr(targetHwnd), 2)

		rootHwnd, res1 := wincoe.GetAncestor(targetHwnd, wincoe.GA_ROOT)
		if res1.Failed() {
			logf("failed to get rootHwnd via GetAncestor on HWND=0x%x, res:%v", targetHwnd, res1)
			return 0 // WinEvent callbacks return 0 (no chaining)
		}
		// rootHwnd := windows.Handle(res1.R1)

		title := getWindowTextFast(rootHwnd)
		procName := getProcessNameFast(pid)
		class, res2 := wincoe.GetClassName(targetHwnd)
		// if (event == EVENT_SYSTEM_CAPTURESTART) || (event == EVENT_SYSTEM_CAPTUREEND) { // yes it does have focus, even tho EVENT_SYSTEM_FOREGROUND is never sent! see caveats1.txt
		//	focusedHwnd := getForegroundWindow()
		// 	if focusedHwnd == hwnd || focusedHwnd == rootHwnd {
		// 		logf("at %s, the foreground window 0x%X is the same one that caused this event 0x%X or its rootHwnd 0x%X !", eventName, focusedHwnd, hwnd, rootHwnd)
		// 	} else {
		// 		logf("at %s, the foreground window 0x%X is NOT the same one that caused this event 0x%X or its rootHwnd 0x%X !", eventName, focusedHwnd, hwnd, rootHwnd)
		// 	}
		// }

		logf("[%s] HWND=0x%x (Root=0x%x) objId=%d childId=%d [%s] Class=[%s] PID=%d (%s) GetClassName res:%v",
			eventName, targetHwnd, rootHwnd, idObject, idChild, title, class, pid, procName, res2)
	}

	if event == wincoe.EVENT_SYSTEM_FOREGROUND && targetHwnd != 0 && !isOwnWindow(targetHwnd) {
		class, res3 := wincoe.GetClassName(targetHwnd)
		if res3.Failed() {
			logf("winEventProc:GetClassName failed for HWND=0x%X, res: %v", targetHwnd, res3)
			return 0 // WinEvent callbacks return 0 (no chaining) //TODO: should we continue instead? #used2continue
		}
		if class != "Shell_TrayWnd" && class != "Shell_SecondaryTrayWnd" {
			//"Caveat: Shell_TrayWnd/Shell_SecondaryTrayWnd cover the normal taskbar; I'm not fully certain of the class name Win11 uses for the "show hidden icons" overflow flyout if your icon ever lives there, so this may need a tweak if you test it and it still shows Explorer in that case. Flagging this as something to actively decide on rather than silently reinstating." - Claude Sonnet 5 Extra Thinking
			lastKnownUserForegroundHwnd.Store(uintptr(targetHwnd))
		}
	}

	if event == wincoe.EVENT_SYSTEM_FOREGROUND || forceReconcile {
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
			if event == wincoe.EVENT_SYSTEM_FOREGROUND {
				logf("Target window HWND=0x%x is higher integrity (0x%x > 0x%x). UIPI will block movement(no key/mouse events will be received while it is focused!thus can't trigger the gesture).", targetHwnd, targetIL, selfIntegrityLevel)
				softReset(true)
				foregroundWasHigherIntegrity.Store(true)
				// Our hooks are about to go blind to all mouse/keyboard
				// input for as long as this (or any equally-or-more
				// elevated) window stays foreground -- including when this
				// very foreground shift was itself caused by an in-flight
				// gesture. See resetStaleGestureFlags's doc comment.
				resetStaleGestureFlags()
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
	//halfassedFIXME: once initWincoeLogging() wires the bridge, the wincoe.GetBugLogger().Error(msg) also funnels back into logf() (via slogBridge.Handle), so every panic2() call after that point writes the same message twice (once raw, once prefixed [wincoe])
	logf("%s", msg)
	//wincoe.GetBugLogger().Error(msg) // so after initWincoeLogging(), this redirects to logf tho, so it doubles the line!
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

	// // Clear the error state first: InternalGetWindowText returning 0 for a
	// // window with a genuinely empty title does NOT call SetLastError, so
	// // without this, GetLastError() below could just reflect whatever
	// // unrelated Win32 call last touched it. See procInternalGetWindowText's
	// // binding comment.
	// _ = procSetLastError.Call(0)//XXX: don't do this because as per https://github.com/golang/go/issues/41220 there's no need to call setlasterror because it happens automatically on LazyProc.Call() !

	// res1 := procInternalGetWindowText.Call(
	// 	uintptr(hwnd),
	// 	uintptr(unsafe.Pointer(&buf[0])),
	// 	uintptr(len(buf)),
	// )
	// This API does NOT send a message; it reads from kernel memory.
	res1 := wincoe.InternalGetWindowTextRaw(hwnd, &buf[0],
		// #nosec G115: integer overflow conversion int -> int32; it's 512 bytes!
		int32(len(buf)),
	)
	length := res1.R1 // it's CheckNone and returns length!
	if length == 0 {
		//if lastErr := windows.GetLastError(); lastErr != nil {// this is always 0/nil because each syscall(which this windows.GetLastError() is) from Go will setlasterr(0) first, as per https://github.com/golang/go/issues/41220
		if lastErr := res1.CallStatus; lastErr != nil {
			logf("getWindowTextFast: InternalGetWindowText failed for HWND=0x%X, err: %v", hwnd, lastErr)
			return "<failed>"
		}
		return "" // genuinely empty title, not a failure
	}
	return windows.UTF16ToString(buf[:length])
}

// Package-level. Non-nil = capture is currently held for that session pointer.
var captureHeldForSession atomic.Pointer[dragSession]

// initForegroundIntegrityState seeds foregroundWasHigherIntegrity with the
// integrity level of whatever window currently has the foreground, at the
// moment winEventHook is installed. Without this, missed-gesture recovery
// never arms on the very first switch away from an already-elevated
// foreground window (e.g. Task Manager, if it was already focused before
// winbollocks started) — only from the second time onward, once
// winEventProc had a chance to observe a real transition *into* such a
// window while our hook was already active.
func initForegroundIntegrityState() {
	// res1 := procGetForegroundWindow.Call()
	// // procGetForegroundWindow is bound with wincoe.CheckNone (no failure signal beyond
	// // NULL), so res1.Failed() can never be true; rely on the HWND itself instead.
	// hwnd := windows.Handle(res1.R1)
	hwnd := getForegroundWindow()
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
	// inputs := []wincoe.KEYANDMOUSE_INPUT{
	// 	{
	// 		Type: INPUT_MOUSE,
	// 		Ki:   wincoe.KEYBDINPUT{}, // union placeholder
	// 	},
	// }

	// //	(*MOUSEINPUT)(unsafe.Pointer(&inputs[0].Ki)).DwFlags = flag
	// mouseInputView(&inputs[0]).DwFlags = flag

	// res1 := procSendInput.Call(
	// 	uintptr(len(inputs)),
	// 	uintptr(unsafe.Pointer(&inputs[0])),
	// 	unsafe.Sizeof(inputs[0]),
	// )

	// lmbClickInputs[1:] creates a slice of length 1 containing ONLY the LEFTUP event, ie. skips the first one
	if res1 := wincoe.SendInput(lmbClickInputs[1:]); res1.Failed() || res1.R1 != 1 {
		logf("SendInput mouse button-up injection (flag=0x%x) failed: ret=%d err=%v", flag, res1.R1, res1.Err)
	}
}

func injectLMBUp() {
	injectMouseButtonUp(wincoe.MOUSEEVENTF_LEFTUP)
}

func injectRMBUp() {
	injectMouseButtonUp(wincoe.MOUSEEVENTF_RIGHTUP)
}

// initDarkMode tells Windows this app supports dark mode,
// allowing standard Win32 context menus to follow the system theme.
func initDarkMode() {
	uxtheme := windows.NewLazySystemDLL("uxtheme.dll") // kept here on purpose just in case this doesn't exist, for some reason
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

var startupTerminalHwnd windows.Handle

// TODO: change the sig of this to take pointer to handle and set the pointer to 0 after close
// or maybe even before then close with the saved one to avoid some TOCTOU window
func closeHandleLogged(h windows.Handle, context2 string) {
	if err := windows.CloseHandle(h); err != nil {
		logf("CloseHandle failed for %s: %v", context2, err)
	}
}

func modifierKeyState() (winDown, shiftDown, ctrlDown, altDown bool) {
	winDown = keyDown(wincoe.VK_LWIN) || keyDown(wincoe.VK_RWIN)
	shiftDown = keyDown(wincoe.VK_SHIFT)
	ctrlDown = keyDown(wincoe.VK_CONTROL)
	altDown = keyDown(wincoe.VK_MENU)
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
		// Only ring the doorbell if it hasn't been rung yet
		if doorbellPending.CompareAndSwap(false, true) {
			// Now we ring the "Doorbell" to wake up the Main Thread.
			// PostThreadMessage(and PostMessage, but not SendMessage!) is an asynchronous "fire and forget" call.
			//the reason we use PostMessage and not PostThreadMessage here is because while systray menu popup is open it runs its own msg loop and calls my wndProc so it will ignore all of these doorbells until popup is closed if i use postThreadMessage!

			if main := loadMainMsgHwnd(); main != 0 {
				if res := wincoe.PostMessage(main, WM_DO_SETWINDOWPOS, 0, 0); res.Failed() {
					logf("enqueueMoveOrResize:PostMessage of WM_DO_SETWINDOWPOS for %s failed: %v", context3, res.Err)
				}
			} else {
				logf("enqueueMoveOrResize:PostMessage of WM_DO_SETWINDOWPOS for %s failed because mainMsgHwnd was 0", context3)
			}
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
	//res := procSetForegroundWindow.Call(uintptr(hwnd))
	if !wincoe.SetForegroundWindow(hwnd) {
		//XXX: you get ret=0 aka res.Err=0 with "err=The operation completed successfully." when Start menu was already open
		logf("%s", failLogPrefix)
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

// tryBringForegroundToFrontAt checks if the mouse click was over the window that
// already has foreground focus. If so, it posts an async WM_BRING_TO_FRONT message
// to restore its Z-order position (e.g. if it was previously sent to bottom via Win+MMB).
func tryBringForegroundToFrontAt(pt wincoe.POINT) {
	if !bringToFrontOnBackgroundClick.Load() {
		return
	}

	// --- NEW FIX: Bring focused-but-backgrounded window to front ---
	// If the user clicks the window that already has focus, Windows normally
	// skips Z-order promotion. If we sent it to the back previously via
	// winkey+MMB, it stays stuck there. We detect this and explicitly bring it up.
	fg := getForegroundWindow()
	if fg == 0 {
		return
	}

	clickedHwnd, res0 := wincoe.RootWindowFromPoint(pt)
	if clickedHwnd != 0 && clickedHwnd == fg {
		// It's the foreground window. Fire an async message to bring it to top.
		// We DO NOT swallow the click (we let it fall through to CallNextHookEx)
		// so the target window still receives the actual mouse click!
		if main := loadMainMsgHwnd(); main != 0 {
			if res := wincoe.PostMessage(main, WM_BRING_TO_FRONT, uintptr(fg), 0); res.Failed() {
				logf("mouseProc: PostMessage WM_BRING_TO_FRONT failed: %v", res.Err)
			}
		} else {
			logf("mouseProc: PostMessage WM_BRING_TO_FRONT failed because mainMsgHwnd is 0")
		}
	} else if res0.Failed() {
		logf("tryBringForegroundToFrontAt:RootWindowFromPoint failed, res:%v", res0)
	}
}
