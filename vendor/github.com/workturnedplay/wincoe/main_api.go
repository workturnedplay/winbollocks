//go:build windows && amd64

// wincoe targets 64-bit Windows only. Several Win32 ABI details are
// architecture-specific:
//   - The wincoe.KEYANDMOUSE_INPUT / KEYBDINPUT struct layout includes explicit 64-bit padding.
//   - wincoe.WindowFromPointRaw or AncestorWindowFromPoint receives POINT by value packed into a single 64-bit
//     register (the amd64 calling convention); on x86 it would be two stack args.
//   - assertStructSizes() validates the 64-bit ABI layout at startup.
// Add a separate build target (and struct definitions) before enabling x86.

//go:generate go run gen_boundproc.go

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

// Package wincoe aka winco(r)e, are common functions I use across my projects to keep things DRY.
package wincoe

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"unsafe"

	"encoding/binary"
	"golang.org/x/sys/windows"
	"golang.org/x/term"

	"net"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// logger is the process-wide fallback logger used by package-level helpers
// (e.g. panic2, GetBugLogger) that cannot receive a logger explicitly. Set it
// via SetLogger; read it via getLogger. Defaults to a "do nothing" (discard)
// logger until the caller (lib user) calls wincoe.SetLogger(...).
//
// Logger is stored behind an atomic.Pointer because it can be swapped
// concurrently (dnsbollocks.LoggerManager.ApplyConfig does this on every
// config Reload) while other goroutines are reading it via panic2() from
// arbitrary wincoe call paths — DNS/UDP/TCP request handling can reach rare
// defensive panics in this package (e.g. PidAndExeForUDP's bounds check,
// impossibiru() inside callWithRetry) at any time. A plain *slog.Logger
// package var here would be a genuine, -race-detectable data race between
// Reload() and any in-flight request hitting one of those paths.
//
// logger stores the process-wide fallback logger used by package-level
// helpers that cannot receive a logger explicitly.
//
// Keep the atomic pointer private so callers cannot store nil and violate
// the package invariant. Use SetLogger and getLogger instead.
var (
	logger        atomic.Pointer[slog.Logger]
	discardLogger = slog.New(slog.NewTextHandler(io.Discard, nil))
)

func init() {
	// Guarantee baseline initialization before any goroutines spawn.
	logger.Store(discardLogger)
}

// SetLogger enforces the non-nil invariant at the mutation boundary.
func SetLogger(l *slog.Logger) {
	if l == nil {
		// Defense-in-depth: Never allow a nil pointer into the atomic storage.
		// Immediately swap to the discard logger to prevent downstream dereference panics.
		logger.Store(discardLogger)
		return
	}
	logger.Store(l)
}

// getLogger provides branchless, atomic read access.
// By enforcing the invariant in SetLogger, the read path is stripped of overhead.
func getLogger() *slog.Logger {
	// Guaranteed non-nil by init() and SetLogger().
	return logger.Load()
}

const PROCESS_QUERY_LIMITED_INFORMATION = 0x1000

const (
	// TH32CS_SNAPHEAPLIST includes all heap lists of the process in the snapshot.
	TH32CS_SNAPHEAPLIST = 0x00000001

	// TH32CS_SNAPPROCESS includes all processes in the system in the snapshot.
	TH32CS_SNAPPROCESS = 0x00000002

	// TH32CS_SNAPTHREAD includes all threads in the system in the snapshot.
	TH32CS_SNAPTHREAD = 0x00000004

	// TH32CS_SNAPMODULE includes all modules of the process in the snapshot.
	//TH32CS_SNAPMODULE enumerates all modules for the process, but on a 64-bit process, it only includes modules of the same bitness as the caller (so a 64-bit process sees 64-bit modules).
	//If you only pass TH32CS_SNAPMODULE in a 64-bit process, you will not see 32-bit modules of a 32-bit process, ergo you need TH32CS_SNAPMODULE32 too.
	TH32CS_SNAPMODULE = 0x00000008

	// TH32CS_SNAPMODULE32 includes 32-bit modules of the process in the snapshot.
	//TH32CS_SNAPMODULE32 explicitly requests 32-bit modules, which is only relevant if your process is 64-bit and you want to see 32-bit modules of a 32-bit process.
	TH32CS_SNAPMODULE32 = 0x00000010

	// TH32CS_SNAPALL is a convenience constant to include all object types.
	TH32CS_SNAPALL = TH32CS_SNAPHEAPLIST | TH32CS_SNAPPROCESS | TH32CS_SNAPTHREAD | TH32CS_SNAPMODULE | TH32CS_SNAPMODULE32

	// TH32CS_INHERIT indicates that the snapshot handle is inheritable.
	TH32CS_INHERIT = 0x80000000
)

const (
	// STD_OUTPUT_HANDLE to be used with windows.GetStdHandle(STD_OUTPUT_HANDLE) only!
	STD_OUTPUT_HANDLE = uint32(-11 & 0xFFFFFFFF) // cast to uint32
	// STD_ERROR_HANDLE to be used with windows.GetStdHandle(STD_OUTPUT_HANDLE) only!
	STD_ERROR_HANDLE = uint32(-12 & 0xFFFFFFFF)

	FOREGROUND_RED       uint16 = 0x0004
	FOREGROUND_GREEN     uint16 = 0x0002
	FOREGROUND_BLUE      uint16 = 0x0001
	FOREGROUND_NORMAL    uint16 = 0x0007
	FOREGROUND_INTENSITY uint16 = 0x0008
	FOREGROUND_GRAY      uint16 = FOREGROUND_INTENSITY // dark gray / bright black

	// derived colors
	FOREGROUND_YELLOW        uint16 = FOREGROUND_RED | FOREGROUND_GREEN
	FOREGROUND_BRIGHT_YELLOW uint16 = FOREGROUND_YELLOW | FOREGROUND_INTENSITY

	FOREGROUND_MAGENTA        uint16 = FOREGROUND_RED | FOREGROUND_BLUE
	FOREGROUND_BRIGHT_MAGENTA uint16 = FOREGROUND_MAGENTA | FOREGROUND_INTENSITY

	FOREGROUND_CYAN        uint16 = FOREGROUND_GREEN | FOREGROUND_BLUE
	FOREGROUND_BRIGHT_CYAN uint16 = FOREGROUND_CYAN | FOREGROUND_INTENSITY

	FOREGROUND_WHITE        uint16 = FOREGROUND_RED | FOREGROUND_GREEN | FOREGROUND_BLUE
	FOREGROUND_BRIGHT_WHITE uint16 = FOREGROUND_WHITE | FOREGROUND_INTENSITY

	FOREGROUND_BRIGHT_RED uint16 = FOREGROUND_RED | FOREGROUND_INTENSITY

	FOREGROUND_BRIGHT_GREEN uint16 = FOREGROUND_GREEN | FOREGROUND_INTENSITY
)

const (
	AF_INET  = 2
	AF_INET6 = 23

	UDP_TABLE_OWNER_PID     = 1 // MIB_UDPTABLE_OWNER_PID
	TCP_TABLE_OWNER_PID_ALL = 5
)

// MaxExtendedPath is the maximum character count supported by the Unicode (W) versions of Windows API functions when using the \\?\ prefix, and it's the limit for QueryFullProcessNameW.
// don't set a type so it can be compared with other types without error-ing about mismatched types!
const MaxExtendedPath = 32767

// Static assertions to ensure constants are "stern" enough.
// This block will fail to compile if the conditions are not met.
const (
	// Ensure MaxExtendedPath isn't accidentally set higher than what a uint32 can hold.
	_ = uint32(MaxExtendedPath)
)

// Ensure MaxExtendedPath is at least as large as the legacy MAX_PATH (260).
var _ = [MaxExtendedPath - 260]byte{}

const ENABLE_VIRTUAL_TERMINAL_PROCESSING uint32 = 0x0004

func EnableVirtualTerminalProcessing() error {
	hStdout, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return fmt.Errorf("GetStdHandle failed: %w", err)
	}
	// hStdout is 0 (NULL) when no console is attached at all (e.g. a
	// -H=windowsgui build, or after FreeConsole) and no stdio redirection
	// was configured for this process; windows.InvalidHandle (-1) indicates
	// an actual GetStdHandle error. Either way there's nothing to enable VT
	// processing on.
	if hStdout == windows.InvalidHandle || hStdout == 0 {
		return errors.New("invalid stdout handle (no console attached)")
	}

	var mode uint32
	if err := windows.GetConsoleMode(hStdout, &mode); err != nil {
		return fmt.Errorf("GetConsoleMode failed: %w", err)
	}

	mode |= ENABLE_VIRTUAL_TERMINAL_PROCESSING
	if err2 := windows.SetConsoleMode(hStdout, mode); err2 != nil {
		return fmt.Errorf("EnableVirtualTerminalProcessing failed: %w", err2)
	} else {
		return nil
	}
}

// WithConsoleColor temporarily changes the console text attributes, invokes
// fn, and restores the original attributes before returning.
//
// If fn panics, the original panic is preserved. A restoration failure during
// panic unwinding is logged because there is no ordinary error return path
// available without replacing or obscuring the panic.
func WithConsoleColor(
	outputHandle windows.Handle,
	color uint16,
	fn func(),
) (errRet error) {
	if fn == nil {
		return errors.New("WithConsoleColor: nil callback")
	}

	originalColor, err := GetConsoleScreenBufferAttributes(outputHandle)
	if err != nil {
		return fmt.Errorf("WithConsoleColor: get original console attributes: %w", err)
	}

	if err := SetConsoleTextAttribute(outputHandle, color); err != nil {
		return fmt.Errorf(
			"WithConsoleColor: set console color %d: %w",
			color,
			err,
		)
	}

	defer func() {
		resetErr := SetConsoleTextAttribute(outputHandle, originalColor)
		if resetErr == nil {
			return
		}

		if recovered := recover(); recovered != nil {
			getLogger().Error(
				"WithConsoleColor: failed to restore console color while unwinding panic",
				slog.Uint64("original_color", uint64(originalColor)),
				SafeErr(resetErr),
			)
			panic(recovered)
		}

		errRet = fmt.Errorf(
			"WithConsoleColor: restore original console color %d: %w",
			originalColor,
			resetErr,
		)
	}()

	fn()
	return nil
}

// GetConsoleScreenBufferAttributes returns the current console text attribute so we can restore it after colored output.
// This is the missing piece you mentioned.
// NOTE: outputHandle must be gotten via windows.GetStdHandle(STD_OUTPUT_HANDLE) or via windows.Stdout or windows.Stderr but NOT directly using STD_OUTPUT_HANDLE
func GetConsoleScreenBufferAttributes(outputHandle windows.Handle) (uint16, error) {
	if outputHandle == windows.InvalidHandle {
		return 0, errors.New("invalid console handle")
	}

	var csbi windows.ConsoleScreenBufferInfo
	//XXX: don't use STD_OUTPUT_HANDLE for this call, it won't work!
	if err := windows.GetConsoleScreenBufferInfo(outputHandle, &csbi); err != nil {
		return 0, fmt.Errorf("GetConsoleScreenBufferInfo failed: %w", err)
	}
	return csbi.Attributes, nil
}

// SetConsoleTextAttribute used to set the color for the text next printed on console
func SetConsoleTextAttribute(h windows.Handle, color uint16) error {
	res := procSetConsoleTextAttribute.Call(uintptr(h), uintptr(color))
	return res.Err
}

// ClearStdinIfTermIsNOTRaw drains and discards any console input events
// currently queued for os.Stdin, using FlushConsoleInputBuffer. Intended for
// callers running in normal (cooked) console mode who want to make sure a
// later blocking read isn't immediately satisfied by stale buffered input
// (e.g. leftover keystrokes typed while some earlier operation was busy).
//
// NOTE: this also discards queued mouse-movement input records if
// ENABLE_MOUSE_INPUT is on for the console, not just key events (see the
// FIXME on the internal GetNumberOfConsoleInputEvents call).
//
// Returns true if there was any pending input to clear (n > 0 at the time
// of the check), false if the console handle couldn't be queried, there was
// nothing pending, or the flush itself failed (failures are logged via
// wincoe.Logger but otherwise swallowed — this is a best-effort cleanup
// helper, not a hard requirement for correctness).
func ClearStdinIfTermIsNOTRaw() (hadInput bool) {
	/*
	   Windows:

	   Console = input events

	   # Arrow keys are atomic

	   # FlushConsoleInputBuffer already solves the problem

	   One read is enough
	*/
	h := windows.Handle(os.Stdin.Fd())

	var n uint32
	err := windows.GetNumberOfConsoleInputEvents(h, &n) // FIXME: this means mouse movements too though!
	if err != nil || n == 0 {
		return false
	}

	if flushErr := windows.FlushConsoleInputBuffer(h); flushErr != nil {
		getLogger().Debug("ClearStdinIfTermIsNOTRaw: FlushConsoleInputBuffer failed", SafeErr(flushErr))
	}
	return true
}

func ReadKeySequence() {
	var b [1]byte
	if _, err := os.Stdin.Read(b[:]); err != nil {
		getLogger().Debug("ReadKeySequence: os.Stdin.Read failed", SafeErr(err))
	}
}

// Minimal local copies of the Win32 structs we need.
type inputRecord struct {
	EventType uint16
	_         [2]byte
	Event     [16]byte
}

type keyEventRecord struct {
	BKeyDown        int32 // BOOL
	RepeatCount     uint16
	VirtualKeyCode  uint16
	VirtualScanCode uint16
	UnicodeChar     uint16
	ControlKeyState uint32
}

const (
	KEY_EVENT = 0x0001
	VK_RETURN = 0x0D // Virtual Key Code for Enter/Carriage Return
)

// ClearStdin inspects and consumes all pending console input events.
// Returns true if any KEY_EVENT with BKeyDown was observed.
// It peeks first to avoid blocking reads.
//
// This is best-effort: failures are logged (via wincoe.Logger) but do not
// abort the program — we still return whatever partial state we collected.
// Thread-safety note: console handle access is inherently racy if other
// goroutines manipulate stdin mode concurrently. Callers (WaitAnyKey etc.)
// already wrap this in WithConsoleEventRaw which does its own mode protection.
func ClearStdin() (hadKey bool) {
	h := syscall.Handle(os.Stdin.Fd())
	log := getLogger() // safe atomic read

	for {
		// Peek a single event (non-destructive, non-blocking).
		var peekRec inputRecord
		var peekCount uint32

		res1 := procPeekConsoleInputW.Call(
			uintptr(h),
			uintptr(unsafe.Pointer(&peekRec)),
			uintptr(1),
			uintptr(unsafe.Pointer(&peekCount)),
		)
		if res1.Failed() { //err != nil {
			// Failure on Peek — log and stop. This is usually transient or
			// indicates stdin is no longer a console.
			log.Warn("ClearStdin: PeekConsoleInputW failed",
				slog.String("operation", "PeekConsoleInputW"),
				SafeErr(res1.Err))
			break
		}
		if peekCount == 0 {
			// no events waiting -> done
			break
		}

		// There's at least one event, now consume one event for real.
		var rec inputRecord
		var read uint32

		res2 := procReadConsoleInputW.Call(
			uintptr(h),
			uintptr(unsafe.Pointer(&rec)),
			uintptr(1),
			uintptr(unsafe.Pointer(&read)),
		)
		if res2.Failed() { // != nil {
			log.Warn("ClearStdin: ReadConsoleInputW failed",
				slog.String("operation", "ReadConsoleInputW"),
				SafeErr(res2.Err))
			break
		}
		if read == 0 {
			// no event read — stop to avoid blocking
			break
		}

		// Inspect consumed event
		if rec.EventType == KEY_EVENT {
			ke := (*keyEventRecord)(unsafe.Pointer(&rec.Event[0]))
			if ke.BKeyDown != 0 {
				if !hadKey {
					hadKey = true
				}
				// continue draining the rest
				continue
			}
		}
		// otherwise keep looping until no events left
	}

	return hadKey // explicit return for clarity (though bare "return" also works)
}

// WithConsoleEventRaw temporarily switches os.Stdin's console into
// "event-raw" mode — disabling ENABLE_LINE_INPUT and ENABLE_ECHO_INPUT so
// individual key events are delivered immediately instead of being buffered
// until a full line + Enter is typed — runs fn, and unconditionally restores
// the original console mode afterward via defer (even if fn panics).
//
// If GetConsoleMode or SetConsoleMode fails at the point of ENTERING raw
// mode, fn is NOT called at all (the failure is logged via wincoe.Logger and
// this function returns immediately) rather than running fn in an unknown/
// uncontrolled mode. If restoring the original mode afterward fails, that
// failure is only logged — by that point fn has already run and there's
// nothing more useful to do about it.
//
// See ClearStdin/ReadKeySequence for the two OS-specific helpers this is
// typically paired with inside fn.
func WithConsoleEventRaw(fn func()) {
	h := windows.Handle(os.Stdin.Fd())

	var oldMode uint32
	if err := windows.GetConsoleMode(h, &oldMode); err != nil {
		log := getLogger() // safe atomic read
		log.Warn("WithConsoleEventRaw: GetConsoleMode failed; NOT running fn() without raw-mode toggling", SafeErr(err))
		// fn()
		return
	}

	newMode := oldMode
	//"Take the current value of newMode and force the ENABLE_LINE_INPUT bit to be 0 (off), while leaving all other bits exactly as they were."
	//so: newMode = newMode AND (NOT windows.ENABLE_LINE_INPUT)
	newMode &^= windows.ENABLE_LINE_INPUT
	newMode &^= windows.ENABLE_ECHO_INPUT

	if err := windows.SetConsoleMode(h, newMode); err != nil {
		log := getLogger() // safe atomic read
		log.Warn("WithConsoleEventRaw: SetConsoleMode failed to enter raw mode, NOT running fn() without raw-mode toggling", SafeErr(err))
		return
	}
	defer func() {
		log := getLogger() // safe atomic read
		err2 := windows.SetConsoleMode(h, oldMode)
		if err2 != nil {
			log.Warn("WithConsoleEventRaw: SetConsoleMode failed to restore old mode, ignoring", SafeErr(err2))
		}
	}()

	fn()
}

// IsStdinConsoleInteractive reports whether os.Stdin is connected to a real,
// interactive terminal — as opposed to a pipe, redirect, or non-console file
// (e.g. `echo foo | program.exe` or CI/CD environments that provide no
// attached console at all).
//
// This distinguishes three cases term.IsTerminal alone would not: cooked
// line-mode consoles, event-raw consoles, and byte-raw/VT consoles are all
// still "a terminal" for this check's purposes — the only thing that
// matters here is whether it's safe to block waiting for a keypress at all.
//
// Returns false (without querying anything further) if the underlying file
// descriptor value doesn't fit in a platform int (an essentially impossible
// defensive guard, logged via wincoe.GetBugLogger() if ever hit), or if
// term.IsTerminal reports the descriptor isn't a real character-device
// console.
/*
On Windows there are three distinct modes, not two:

Cooked line mode
– keys buffered until Enter
– no KEY_EVENT until line completes

Event-raw mode
– immediate KEY_EVENTs
– arrow keys are single events
– ReadConsoleInputW works

VT / byte-raw mode
– escape sequences
– os.Stdin.Read works
– no console events
*/
// this is cross-platform, as per Gemini
func IsStdinConsoleInteractive() bool {
	fdPtr := os.Stdin.Fd()
	//fmt.Printf("got fdPtr %d\n", fdPtr)

	// G115 Fix: Ensure the uintptr fits into a signed int
	if fdPtr > math.MaxInt {
		//doneTODO: should we log this? Logger.slog
		GetBugLogger().Warn("fdPtr exceeded math.MaxInt", slog.Uint64("fdPtr", uint64(fdPtr)))
		return false
	}

	// Skip waiting if stdin isn't a terminal
	// term.IsTerminal does more than just check GetConsoleMode. On Windows, it specifically handles the nuances of whether the file descriptor
	// is a character device (like a real console) or a pipe (like a CI/CD environment or a redirect).
	if !term.IsTerminal(int(fdPtr)) {
		return false
	}
	return true
}

// WaitAnyKeyIfInteractive calls WaitAnyKey only if os.Stdin is connected to
// a real interactive terminal (see IsStdinConsoleInteractive). This avoids
// blocking forever on a keypress that can never arrive when the process was
// launched with stdin piped, redirected, or otherwise non-interactive (e.g.
// `echo foo | program.exe`, or a CI/CD runner with no attached console).
//
// Returns true if it actually waited (i.e. stdin was interactive), false if
// it skipped waiting entirely because stdin was not interactive.
//
// WaitAnyKeyIfInteractive returns true if waited, false if it's not interactive
// implied before&after clrbuf(s)
func WaitAnyKeyIfInteractive() bool {
	//find out which variant is best here:
	if !IsStdinConsoleInteractive() {
		// don't wait if eg. echo foo | program.exe
		return false
	}
	WaitAnyKey()
	return true
}

// WaitAnyKey prints a "Press any key to exit..." prompt and blocks until a
// single key is pressed, working correctly whether os.Stdin is currently in
// cooked line-mode, event-raw mode, or byte-raw/VT mode (see the "three
// distinct modes" doc comment on IsStdinConsoleInteractive above).
//
// Before waiting, it clears any input events that were already buffered
// (logging "(clrbuf)..." if it found any) so a stale keystroke typed earlier
// doesn't immediately satisfy the wait without the user actually pressing
// anything now. It then spawns a goroutine that reads exactly one key
// sequence in event-raw mode (via ReadKeySequence, itself wrapped in
// WithConsoleEventRaw) and clears any input left buffered afterward too
// (logging "(clrbuf2)." if so), and blocks the calling goroutine on a
// channel until that goroutine signals completion.
//
// Callers should generally prefer WaitAnyKeyIfInteractive unless they've
// already independently confirmed stdin is interactive — calling this
// directly against non-interactive stdin can hang forever with no key ever
// arriving to satisfy the read.
//
// WaitAnyKey, whether it is or not a terminal, it attempts to wait for any key, with proper clrbuf(s) before and after!
func WaitAnyKey() {
	fmt.Print("Press any key to exit...")

	var hadKey bool
	WithConsoleEventRaw(func() {
		hadKey = ClearStdin() // OS-specific
	})

	if hadKey {
		fmt.Print("(clrbuf)...")
	}

	done := make(chan struct{}, 1)

	go func() {
		WithConsoleEventRaw(func() {
			ReadKeySequence() // OS-specific
			if ClearStdin() { // OS-specific
				fmt.Print("(clrbuf2).")
			}
		})
		done <- struct{}{} // Empty structs occupy zero bytes and are commonly used for signals where no data is needed.
	}()

	<-done // blocks until a value is received from the channel.
	fmt.Println()
}

func Flush() {
	//fmt.Printf("[GoR:%d] !flushing stderr\n", GoRoutineId())
	// Errors here are expected and harmless when no console is attached
	// (-H=windowsgui build, or after FreeConsole), hence Debug not Warn/Error.
	if err := os.Stderr.Sync(); err != nil {
		getLogger().Debug("Flush: os.Stderr.Sync failed (harmless if no console is attached)", SafeErr(err))
	}
	//fmt.Printf("[GoR:%d] !flushing stdout\n", GoRoutineId())
	if err := os.Stdout.Sync(); err != nil {
		getLogger().Debug("Flush: os.Stdout.Sync failed (harmless if no console is attached)", SafeErr(err))
	}
}

// UnspecifiedWinApi is the string used when empty op name is used
// const UnspecifiedWinApi string = "unspecified_winapi"

const THREAD_PRIORITY_ERROR_RETURN int32 = 0x7fffffff

// WinCheckFunc defines a predicate used to determine if a Windows API call failed
// based on its primary return value (r1). So true means it failed.
type WinCheckFunc func(r1 uintptr, callErr error) bool

var (
	// CheckBool identifies a failure for functions returning a Windows BOOL in r1.
	// In the Windows API, a 0 (FALSE) indicates that the function failed.
	CheckBool WinCheckFunc = func(r1 uintptr, _ error) bool { return r1 == 0 }

	// CheckHandle identifies a failure for functions returning a HANDLE in r1.
	// Many Windows APIs return INVALID_HANDLE_VALUE (all bits set to 1) on failure.
	// ^uintptr(0) is the Go-idiomatic way to represent -1 as an unsigned pointer.
	CheckHandle WinCheckFunc = func(r1 uintptr, _ error) bool { return r1 == ^uintptr(0) }

	// CheckNull identifies a failure for functions returning a pointer or a handle in r1
	// where a NULL value (0) indicates the operation could not be completed.
	CheckNull WinCheckFunc = func(r1 uintptr, _ error) bool { return r1 == 0 }

	// CheckNullWithLastError returns true (meaning a failure occurred) only if
	// r1 is 0 AND the Windows API actually set a non-zero last error code.
	// because r1 can be 0 and still be a success: GetLastError() aka callErr aka CallStatus is 0 aka SUCCESS.
	// was used for procTrackPopupMenu = NewBoundProc7(User32, "TrackPopupMenu", CheckNullWithLastError)
	// but it's only good for the variant that doesn't use TPM_RETURNCMD in uFlags and then CheckBool is better!
	/*
		done(we are not using CheckNullWithLastError for TrackPopupMenu anymore)doneFIXME: problem:
		When you pass TPM_RETURNCMD to TrackPopupMenu:

		Return value 0 is overloaded: It means "User pressed Esc / clicked away" (100% normal) OR "The API call failed" (actual error).

		The Modal Loop Trap: TrackPopupMenu enters a blocking modal message loop to process mouse and keyboard events. While that loop is running, Windows pumps messages, draws tooltips, updates cursors, and invokes shell hooks.

		Stale/Incidental Errors: Any harmless API call happening under the hood inside that modal loop (or even prior to calling TrackPopupMenu) can set a non-zero GetLastError() code (like ERROR_FILE_NOT_FOUND when trying to load a system cursor/theme).

		False Alarm: When the user presses Esc, TrackPopupMenu exits and returns 0. Go's proc.Call() immediately grabs GetLastError(), sees the leftover non-zero error from the message loop, and CheckNullWithLastError falsely concludes: "The function returned 0 and GetLastError() is non-zero, so this must be a failure!"

		Scenario,Return Value (r1),Win32 Behavior
		User selects an item,Non-zero (TRUE),"Posts a WM_COMMAND message to hwnd with the item ID, then returns TRUE."
		User dismisses menu (Esc / Click away),Non-zero (TRUE),"Simply closes the menu without posting WM_COMMAND, then returns TRUE."
		API Failure,Zero (FALSE),Fails to show the menu (e.g. invalid HMENU or HWND).
	*/
	CheckNullWithLastError WinCheckFunc = func(r1 uintptr, callErr error) bool {
		if r1 != 0 {
			return false // r1 is non-zero, definitely a success
		}

		// r1 is 0. Now we must inspect callErr (GetLastError).
		if callErr == nil {
			return false
		}

		// In Go's syscall package, callErr is always a concrete syscall.Errno
		// (even if its value is 0 / ERROR_SUCCESS).
		// r1 is 0. A zero return from GetWindowLongPtrW is only a failure
		// if the OS actually set a non-zero last error.

		// var errno syscall.Errno
		// if errors.As(callErr, &errno) {
		if errno, ok := errors.AsType[syscall.Errno](callErr); ok {
			return errno != 0 // True failure only if the error code is non-zero
		}

		// Fallback if it's some other error type
		panic2("BUG: in CheckNullWithLastError: unexpected non- syscall.Errno type for the 3rd returned arg of syscall.SyscallN(..); this means we made some wrong assumptions here?!")
		return true
	}

	// CheckHRESULT identifies a failure for functions that return an HRESULT in r1.
	// An HRESULT is a 32-bit value where a negative number (high bit set)
	// indicates an error, while 0 or positive values indicate success.
	/*
			HRESULT (COM / User-mode Win32)

		HRESULT is used by COM (Component Object Model) and high-level user-mode APIs. It only allocates 1 bit for Severity:

		    0 (Success): S_OK (0x00000000) or S_FALSE (0x00000001)

		    1 (Failure): E_FAIL (0x80004005)
	*/
	CheckHRESULT WinCheckFunc = func(r1 uintptr, _ error) bool { return int32(r1) < 0 }

	// CheckErrno identifies a failure for Win32 APIs that return a DWORD error code in r1.
	// In this convention, 0 (ERROR_SUCCESS) means success, any non-zero value is a failure.
	CheckErrno WinCheckFunc = func(r1 uintptr, _ error) bool { return r1 != 0 }

	// CheckAdjustTokenPrivileges handles both FALSE returns and the partial-success
	// state where some privileges could not be assigned (ERROR_NOT_ALL_ASSIGNED).
	CheckAdjustTokenPrivileges WinCheckFunc = func(r1 uintptr, callErr error) bool {
		// Layer 1: If the API returned FALSE (0), the entire call failed.
		if r1 == 0 {
			return true
		}

		// Layer 2: The API returned TRUE, but check if it partially failed.
		// Go's syscall/windows wrappers always return a non-nil error tracking GetLastError().
		if callErr != nil && errors.Is(callErr, windows.ERROR_NOT_ALL_ASSIGNED) {
			return true // Treat partial assignment as a failure state
		}

		return false
	}

	// CheckZero indicates failure if the API returns 0 (useful for counts, lengths, IDs)
	CheckZero WinCheckFunc = func(r1 uintptr, _ error) bool { return r1 == 0 }

	// CheckMinusOne indicates failure if the API returns -1 (specifically for GetMessage)
	CheckMinusOne WinCheckFunc = func(r1 uintptr, _ error) bool { return r1 == ^uintptr(0) }

	// CheckNone never fails. Used for VOID returns or LRESULTs that require manual checking.
	// XXX: Using anything other than CheckNone and an error outside the 0..255 range happens,
	// it will cause the .Call(..) to alloc due to Errno becoming the 'error' interface
	// and possibly more allocs due to fmt.Errorf formatting
	//  within the CheckWinResult() call which happens internally when error is sensed!
	// "Go's runtime uses its static lookup table for integers between 0 and 255.
	// Returning error codes within this range costs zero heap allocations.
	// Only error codes >= 256 incur one 8-byte heap allocation to box the syscall.Errno integer into the error interface."
	//
	//If an API can return 0 as a legitimate "No" or "Nothing found" without setting an error code, it must be bound as CheckNone.
	CheckNone WinCheckFunc = func(_ uintptr, _ error) bool { return false }

	// CheckNTSTATUS indicates failure if the NTSTATUS code is negative
	/*
			NTSTATUS (Kernel / Native API)

		NTSTATUS is used by the NT Kernel and native APIs (found in ntdll.dll). Its top 2 bits represent Severity:

		    00 (Success): STATUS_SUCCESS (0x00000000)

		    01 (Informational): STATUS_PENDING (0x00000103)

		    10 (Warning): STATUS_BUFFER_OVERFLOW (0x80000005)

		    11 (Error): STATUS_ACCESS_DENIED (0xC0000022)

		Because Warnings (10...) and Errors (11...) both have the highest bit (bit 31) set, they both evaluate as negative integers (< 0).
	*/
	//XXX: not collapsing it to same impl. as CheckHRESULT on purpose!
	CheckNTSTATUS WinCheckFunc = func(r1 uintptr, _ error) bool { return int32(r1) < 0 }

	//for GetThreadPriority which returns r1 as int,
	CheckThreadPriority WinCheckFunc = func(r1 uintptr, _ error) bool {
		return int32(r1) == THREAD_PRIORITY_ERROR_RETURN // aka 0x7fffffff // aka THREAD_PRIORITY_ERROR_RETURN
	}

	CheckCLRInvalid WinCheckFunc = func(r1 uintptr, _ error) bool {
		return uint32(r1) == CLR_INVALID // aka 0xffffffff
	}

	CheckGDIError WinCheckFunc = func(r1 uintptr, _ error) bool {
		return uint32(r1) == GDIError // aka 0xffffffff
	}

	// CheckStringLength returns true (failure) only if r1 is 0 AND an actual error is set.
	CheckStringLength WinCheckFunc = func(r1 uintptr, callErr error) bool {
		if r1 == 0 {
			if callErr != nil && !errors.Is(callErr, windows.ERROR_SUCCESS) {
				return true // It's a real failure
			}
		}
		return false // It's just an empty string
	}
)

// CheckEquals returns a WinCheckFunc that treats any r1 not equal to
// expected as a failure. This is the right tool specifically for APIs that
// combine two properties: (1) documented as unable to fail through ordinary
// means, and (2) documented to always return exactly one fixed, known
// value on success (e.g. GetCurrentProcess always returns the pseudo-handle
// -1; GetCurrentThread always returns -2). For those, the interesting
// failure mode isn't "the API reported an error" but "the OS/ABI returned
// something other than the single value it's contractually guaranteed to
// return" — which would itself indicate a much deeper problem (calling
// convention mismatch, corrupted stack, etc.) worth surfacing through the
// normal WinResult.Failed()/.Err plumbing instead of a bespoke inline check
// re-derived at every call site.
//
// Don't reach for this for APIs that can validly return more than one
// value on success (e.g. CreateToolhelp32Snapshot, GetPriorityClass) —
// CheckEquals would incorrectly treat every valid value except one as a
// failure.
func CheckEquals(expected uintptr) WinCheckFunc {
	return func(r1 uintptr, _ error) bool { return r1 != expected }
}

const CLR_INVALID uint32 = 0xffffffff
const GDIError = uint32(0xffffffff)

// WinResult is the way to check for syscall's state after running it
// WinResult.CallStatus is the raw GetLastError() of the call, guaranteed to be it(and it was reset to 0 before each call by Go) by Go runtime / SyscallN()
// but you should use WinResult.Failed() or .Success() first, to know if an error was afoot, then WinResult.Err is that error, else it's nil!
// when success though, WinResult.Err will be nil but if you expect GetLastError() to be non-zero then it's gonna be in WinResult.CallStatus !
// side effect of returning a struct like this is that we don't get warned for not handling the returned Err or CallStatus errors! in a way it's good because CallStatus is rarely checked! just Err is checked usually.
type WinResult struct {
	R1 uintptr
	R2 uintptr
	// exactly the third return value from LazyProc.Call; may contain additional status information even when Err == nil.
	//
	// Raw status returned by LazyProc.Call. Usually ERROR_SUCCESS
	// on successful calls, but some Win32 APIs use it to report
	// additional success information (e.g. ERROR_ALREADY_EXISTS).
	//this is actually the GetLastError() done atomically(ie. unpreemptible) by Go/asm (think syscall.SyscallN(..) done) right after the windows syscall returned
	CallStatus error

	Err error // normalized according to the Check*() function that NewBoundProc() got as the last arg!
}

func (r WinResult) Failed() bool {
	return r.Err != nil
}

func (r WinResult) Succeeded() bool {
	return r.Err == nil
}

// Error , once this method is defined, fmt.Sprintf("%v", res) or %w will automatically invoke Error() and output a clean,
//
//	human-readable string instead of the raw struct layout.
func (r WinResult) Error() string {
	if r.Err != nil {
		return fmt.Sprintf("R1: %v, CallStatus(aka GetLastError): %v, actual error(based on CheckFunc and CallStatus): %v", r.R1, r.CallStatus, r.Err)
	}
	return fmt.Sprintf("R1: %v, CallStatus(aka GetLastError): %v", r.R1, r.CallStatus)
}

// ErrIs reports whether Err matches a particular failure value.
//
// Successful calls are represented by Err == nil, so passing
// windows.ERROR_SUCCESS will always return false.
func (r WinResult) ErrIs(target error) bool {
	if target == nil || target == error(windows.ERROR_SUCCESS) { //nolint:errorlint //we're checking for exactly this, not for one of the wrapped ones being ERROR_SUCCESS! aka // exact sentinel check; wrapped errors are intentionally not matched.
		GetBugLogger().Warn(
			"BUG: WinResult.ErrIs() cannot be used to test for success; either use [!WinResult.Succeeded() or WinResult.Failed()] first then WinResult.ErrIs(target_error) (aka this) to check for exact error, or use WinResult.CallStatusIs(target_error) which allows checking for windows.ERROR_SUCCESS! So, the dev. needs to change the callsite! ",
			slog.Any("stack", debug.Stack()),
		)
	}
	return errors.Is(r.Err, target)
}

// CallStatusIs reports whether the raw call status returned by LazyProc.Call
// matches target.
//
// Unlike Err, CallStatus is not normalized and may contain additional
// information even when the call succeeded. For example, some Win32 APIs
// report ERROR_ALREADY_EXISTS on successful calls.
//
// A nil CallStatus is safely treated as ERROR_SUCCESS for matching purposes.
//
// Use this only for APIs whose documentation defines meaningful success
// status codes beyond simple success/failure.
func (r WinResult) CallStatusIs(target error) bool {
	err := r.CallStatus
	if err == nil {
		// A nil CallStatus means no error occurred, which is equivalent to ERROR_SUCCESS
		err = windows.ERROR_SUCCESS
	}
	return errors.Is(err, target)
}

// CallStatusFailed returns true if CallStatus contains a real OS error.
// It is immune to the typed-nil interface trap and handles ERROR_SUCCESS (0).
func (r WinResult) CallStatusFailed() bool {
	if r.CallStatus == nil {
		return false
	}

	// 1. Handle ERROR_SUCCESS (0) explicitly via errors.Is
	if errors.Is(r.CallStatus, windows.ERROR_SUCCESS) {
		return false
	}

	// 2. Extract underlying Errno integer to check if it's 0
	if errno, ok := errors.AsType[syscall.Errno](r.CallStatus); ok {
		return errno != 0
	}

	// It's a non-nil error that isn't Errno 0
	panic2("BUG: in CallStatusFailed(): unexpected non- syscall.Errno type for the 3rd returned arg of syscall.SyscallN(..); this means we made some wrong assumptions here?!")
	return true
}

func (r WinResult) CallStatusSucceeded() bool {
	return !r.CallStatusFailed()
}

// Static assertion: CheckWinResult's r1-recovery path (treating a non-zero
// r1 as an errno when callErr is unavailable) assumes ERROR_SUCCESS == 0,
// since it only takes that path when r1 != 0. If ERROR_SUCCESS were ever
// redefined to something else, this fails to compile instead of silently
// letting CheckWinResult misclassify a success code as a failure.
var _ = [0 - int(windows.ERROR_SUCCESS)]byte{}

// CheckWinResult processes a Windows API result.
//
// It returns nil on success (when isFailure is false).
//
// On failure, it returns a wrapped error.
// /
// Use errors.Is whenever you want to check whether an error matches a particular sentinel value, like windows.ERROR_ACCESS_DENIED
//
// This works even if the error was wrapped with %w in fmt.Errorf, which is exactly what this helper does.
//
// callErr will never be windows.ERROR_SUCCESS but instead it would be nil or an error if r1 indicates an error but callErr didn't.
//
// operationNameToIncludeInErrorMessages can be empty, unlike for WinCall, it's not converted into a predefined string.
func CheckWinResult(
	//can be empty
	operationNameToIncludeInErrorMessages string,
	isFailure WinCheckFunc,
	//onFail func(err error),
	r1 uintptr,
	callErr error,
) error {
	if !isFailure(r1, callErr) {
		// Success: return nil so 'if err != nil' behaves normally.
		return nil
	}

	// Normalize callErr: treat ERROR_SUCCESS as nil
	if callErr != nil && errors.Is(callErr, windows.ERROR_SUCCESS) {
		callErr = nil
	}

	// If callErr is missing/useless, try to recover from r1
	if callErr == nil {
		// Many Win32 APIs (e.g. GetExtendedUdpTable) return the error in r1.
		// Only treat r1 as an errno if it's non-zero.
		if r1 != 0 { // 0 here is exactly windows.ERROR_SUCCESS
			errno := windows.Errno(r1) //doneTODO: see how we can match against this, I doubt errors.Is still works for this! actually, it seems to, based on the below!

			//I keep the redundancy of these 2 compile-time asserts(type and var) here, on purpose(and the package level one before this function there as well):

			// Local compile-time assertion trap(to avoid that inner 'if'):
			type _ [0 - int(windows.ERROR_SUCCESS)]byte

			// on-purpose-redundant Compile-time assertion that ERROR_SUCCESS is 0.
			// If it is NOT 0, this evaluates to [-1]int, which causes a compiler error.
			var _ [0 - int(windows.ERROR_SUCCESS)]int

			//// Defensive: avoid ever wrapping ERROR_SUCCESS
			//if !errors.Is(errno, windows.ERROR_SUCCESS) {
			// since r1 != 0 already, this is bound to never be ERROR_SUCCESS here, unless r1 != 0 can ever be ERROR_SUCCESS, unsure.
			return fmt.Errorf("%q windows call failed with error: %w", operationNameToIncludeInErrorMessages, errno)
			//}
		}

		//fmt.Printf("[GoR:%d] !ending   CheckWinResult for %s with truly unknown failure: ret=%d\n", GoRoutineId(), operationNameToIncludeInErrorMessages, r1)
		// Fallback: truly unknown failure
		return fmt.Errorf(
			"%q windows call reported failure (ret=%d) but no usable error was provided",
			operationNameToIncludeInErrorMessages,
			r1,
		)
	}

	// Normal path: we have a meaningful callErr
	return fmt.Errorf("%q windows call failed with error: %w", operationNameToIncludeInErrorMessages, callErr)
}

// // LazyProcishWrapperForMocksN is the minimal interface that WinCall needs from a windows.LazyProc-like object.
// //
// // We deliberately avoid the full *windows.LazyProc type to enable mocking.
// type LazyProcishWrapperForMocksN interface { //formerly named LazyProcish
// 	// Name returns the name of the procedure (used in error messages).
// 	//Why Name() instead of a field? Because interfaces in Go cannot require fields — only methods
// 	Name() string

// 	// Call invokes the Windows procedure with the given arguments.
// 	// Signature must match windows.LazyProc.Call exactly.
// 	Call(a ...uintptr) (r1, r2 uintptr, lastErr error)
// 	Find() error
// 	Addr() uintptr
// }

// // realLazyProc wraps *windows.LazyProc to satisfy LazyProcish.
// //
// // Embedding gives us .Call() for free via promotion.
// type realLazyProc struct {
// 	*windows.LazyProc
// }

// // Name implements LazyProcish.
// //
// // Returns the procedure name for use in error messages.
// func (r *realLazyProc) Name() string {
// 	return r.LazyProc.Name
// }

// // RealProc wraps a *windows.LazyProc into the testable interface.
// //
// // Use this at all production call sites instead of passing *windows.LazyProc directly.
// //
// // The real production code that previously called WinCall(&proc, ...) now becomes WinCall(&realLazyProc{LazyProc: &proc}, ...) or you use this tiny helper like:
// //
// // r1, r2, err := WinCall(RealProc(proc), CheckBool, uintptr(unsafe.Pointer(&something)), ...)
// func RealProc(p *windows.LazyProc) LazyProcishWrapperForMocksN {
// 	return &realLazyProcN{LazyProc: p}
// }

// // MustLoadProc eagerly resolves a procedure from the given DLL and wraps it into a LazyProcish.
// // it loads the DLL and resolves the proc, so it unlazifies the whole thing, thus it can panic if DLL or proc cannot be loaded or found
// //
// // It is a thin, validated convenience over dll.NewProc(name) + RealProc(...).
// // This function enforces basic invariants early:
// //   - dll must be non-nil
// //
// // The returned LazyProcish is suitable for use with WinCall or higher-level
// // binding helpers such as BindFunc.
// //
// // MustLoadProc does NOT attach any failure semantics (WinCheckFunc). Callers must
// // explicitly provide the appropriate check strategy (e.g. CheckBool, CheckHandle)
// // when invoking the procedure via WinCall or when binding it.
// //
// // Panics:
// //   - if dll is nil
// func MustLoadProc(dll *windows.LazyDLL, name string) LazyProcishWrapperForMocksN {
// 	if dll == nil {
// 		panic2("MustLoadProc: nil dll")
// 	}
// 	loadDll(dll) // make it non-lazy, load it now if not loaded! or panic if loading fails!
// 	// name = strings.TrimSpace(name)
// 	// if name == "" {
// 	// 	panic2("MustLoadProc: empty proc name")
// 	// }
// 	lp := dll.NewProc(name)
// 	// Force resolution now. Find() returns the address or an error.
// 	if err := lp.Find(); err != nil {
// 		panic2(fmt.Sprintf("MustLoadProc: unable to find windows API function named %q, err: %v", name, err.Error()))
// 	}
// 	return RealProc(lp)
// }

// // BoundProc
// // By making this a struct with a method, we can apply //go:uintptrescapes to it.

// // BoundProcN represents a Windows API procedure permanently bound to a
// // specific failure-checking strategy.
// //
// // It wraps a LazyProcish (usually a windows.LazyProc) and a WinCheckFunc.
// // By using BoundProcN instead of raw Syscall/Call, you centralize error
// // handling logic for the specific API while maintaining the ability to
// // use //go:uintptrescapes for memory safety.
// type BoundProcN struct {
// 	Proc  LazyProcishWrapperForMocksN
// 	Check WinCheckFunc
// }

// // Call executes the underlying Windows procedure with the provided arguments.
// //
// // SECURITY WARNING: This method uses the //go:uintptrescapes compiler directive.
// // To ensure memory safety and prevent "0xc0000005 Access Violation" crashes,
// // any Go pointer passed as an argument MUST be converted to uintptr using
// // uintptr(unsafe.Pointer(&x)) directly within the argument list of the
// // call site.
// // So //go:uintptrescapes = escape to heap + keep-alive for the duration of the call.
// // The compiler inserts the necessary liveness (equivalent to an implicit KeepAlive across the entire function call)
// // for any argument passed as uintptr(unsafe.Pointer(...)) to a function marked //go:uintptrescapes.
// //
// // Example:
// //
// //	var size uint32
// //	proc.Call(handle, uintptr(unsafe.Pointer(&size)))
// //
// // This direct conversion signals the Go compiler to move the variable to
// // the heap, ensuring its memory address remains stable even if the stack grows.
// //
// //go:uintptrescapes
// func (b *BoundProcN) Call(args ...uintptr) WinResult {
// 	return WinCallN(b.Proc, b.Check, args...)
// }

// // Find attempts to locate the procedure in the DLL.
// // Returns nil if the procedure is successfully found, or an error if it is not.
// func (b *BoundProcN) Find() error {
// 	err := b.Proc.Find()
// 	if err == nil {
// 		return nil
// 	} else {
// 		return fmt.Errorf("BoundProc:Find says that LazyProcish/LaziProc.Find() failed, err: %w", err)
// 	}
// }

// // NewBoundProcN initializes a BoundProc by resolving a procedure from the
// // provided DLL and attaching a result-checking function.
// // It eagerly resolves the DLL and proc's address so it won't have to do it on the first .Call(..)
// // Each .Call(..) causes 1 heap alloc of 8 bytes due to variadic args, just like LazyProc.Call(..) does!
// //
// // Parameters:
// //   - dll: A pointer to a windows.LazyDLL (e.g., kernel32, user32).
// //   - name: The exact string name of the procedure (e.g., "GetProcessId").
// //   - check: A WinCheckFunc (e.g., CheckBool) used to determine if the
// //     API call failed based on its return value.
// //
// // It panics if the check function is nil.
// func NewBoundProcN(dll *windows.LazyDLL, name string, check WinCheckFunc) *BoundProcN {
// 	if check == nil {
// 		panic2("NewBoundProc: nil WinCheckFunc passed as arg")
// 	}

// 	return &BoundProcN{
// 		Proc:  MustLoadProc(dll, name),
// 		Check: check,
// 	}
// }

// // WinCallN is the low-level engine that executes the syscall and performs
// // automated error checking.
// //
// // It leverages //go:uintptrescapes to signal to the compiler that arguments
// // may be pointers converted to integers. It calls the procedure, captures
// // the return values (r1, r2) and the system error, then passes them to
// // CheckWinResult to produce a clean Go error if the call failed.
// //
// // WARNING: you must do the uintptr casting at the args call place (for pointers on stack!) for this to work and not crash randomly because the stack got moved by Go.
// // The price of absolute memory safety in Go is that you must write uintptr(unsafe.Pointer(...)) explicitly at the exact call site.
// // This tells the compiler, "Pin this variable right now."
// //
// // Use this directly only if you need to bypass the BoundProc abstraction.
// // Otherwise, use BoundProc.Call for better type organization.
// //
// //go:uintptrescapes
// func WinCallN(proc LazyProcishWrapperForMocksN, check WinCheckFunc, args ...uintptr) WinResult {
// 	op := validateAndGetOp(proc)
// 	// if proc == nil {
// 	// 	panic2("WinCall: nil proc")
// 	// }

// 	// //op := strings.TrimSpace(proc.Name())
// 	// op := proc.Name()
// 	// if op == "" {
// 	// 	//op = UnspecifiedWinApi
// 	// 	panic2("BUG: impossible to have empty name in proc/LazyProc/LazyProcish/BoundProc, unless it was overwritten after which shouldn't have been!")
// 	// }
// 	// return internalWinCall(op, proc, check, args...)

// 	// args is a []uintptr, but because of //go:uintptrescapes, the caller
// 	// has already pinned the memory safely before we get here.
// 	//"Go's proc.Call() always atomcially captures LastError immediately after the C code finishes." as 3rd arg aka CallStatus(should rename to lastError ?! or not it's more confusing!)
// 	r1, r2, callStatus := proc.Call(args...) // this is one more wrapper ie. windows.LazyProc.Call() but I need it to can use mocked tests
// 	//r1, r2, callStatus := syscall.SyscallN(proc.Addr(), args...) // this is one less wrapper but only 1ns faster than above but fails all tests since they're not mocked!
// 	//XXX: don't put anything here, which might call a syscall or it might delete the last error for a potential future GetLastError() call, actually it won't because each syscall does atomically setlasterror(0),callapi,getlasterror as 3rd return arg!
// 	// return WinResult{
// 	// 	R1:         r1,
// 	// 	R2:         r2,
// 	// 	CallStatus: callStatus,
// 	// 	Err:        CheckWinResult(op, check, r1, callStatus),
// 	// }
// 	return makeWinResult(op, check, r1, r2, callStatus)
// }

// validateProcName handles the nil check and empty name check for any proc wrapper.
// Any type with a Name() string method satisfies this interface implicitly.
func validateProcName(proc interface{ Name() string }) { //string {
	/*
		An interface is only equal to nil (proc == nil) if both the Type and the Value are nil:
		 (nil, nil) --> proc == nil is true
		However, if someone passes a nil pointer of a concrete struct into your function:
		 var p *realLazyProc0 = nil
		 WinCall0(p, check) // 'p' gets implicitly converted to interface{ Name() string }
		Inside validateAndGetOp(proc interface{ Name() string }):
		 Interface representation: (*realLazyProc0, nil)
		 proc == nil evaluates to false because the Type field is *realLazyProc0, not nil.
		If you only used proc == nil, execution would continue, and as soon as you tried calling proc.Name(),
		Go would panic with a nil pointer dereference.
	*/
	if isNilInterfaceValue(proc) {
		panic2(fmt.Sprintf("validateProcName: nil proc (type: %T)", proc))
	}
	pname := proc.Name()
	op := strings.TrimSpace(pname)
	if op == "" {
		panic2(fmt.Sprintf("validateProcName: shouldn't have empty name in proc (type: %T) unless you set it afterwards by mistake directly via `LazyProc.Name=`, you had %q", proc, pname))
		//panic2("BUG: impossible to have empty name in proc/LazyProc/LazyProcish/BoundProc, unless it was overwritten after which shouldn't have been!")
	}
	if pname != op {
		panic2(fmt.Sprintf("BUG: in validateProcName: proc name %q is different than validated proc name %q and it didn't fail earlier!",
			pname, op))
	}
	// return //op
}

// validateProcNameString validates a procedure name before windows.NewProc
// receives it. This catches invalid constructor input at the earliest point.
func validateProcNameString(name string) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		panic2("validateProcNameString: empty procedure name")
	}
	if trimmed != name {
		panic2(fmt.Sprintf(
			"validateProcNameString: procedure name contains leading or trailing whitespace: %q",
			name,
		))
	}
}

// isNilInterfaceValue reports whether v is either a nil interface or contains
// a typed nil value of any nil-capable kind.
//
// Calling reflect.Value.IsNil for a non-nil-capable kind panics, so the kind
// must always be checked first.
func isNilInterfaceValue(v any) bool {
	if v == nil {
		return true
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() { //nolint:exhaustive // we check only these, for wtw reason.
	case reflect.Chan,
		reflect.Func,
		reflect.Interface,
		reflect.Map,
		reflect.Pointer,
		reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

// makeWinResult packages raw call outputs into a WinResult using your check function.
func makeWinResult(op string, check WinCheckFunc, r1, r2 uintptr, callStatus error) WinResult {
	return WinResult{
		R1:         r1,
		R2:         r2,
		CallStatus: callStatus,
		Err:        CheckWinResult(op, check, r1, callStatus),
	}
}

var (
	Kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	User32   = windows.NewLazySystemDLL("user32.dll")
	Iphlpapi = windows.NewLazySystemDLL("iphlpapi.dll")
	Shell32  = windows.NewLazySystemDLL("shell32.dll")
	Shcore   = windows.NewLazySystemDLL("shcore.dll")
	Gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	Psapi    = windows.NewLazySystemDLL("psapi.dll")
	Advapi32 = windows.NewLazySystemDLL("advapi32.dll")
	Ntdll    = windows.NewLazySystemDLL("ntdll.dll")
	Wtsapi32 = windows.NewLazySystemDLL("wtsapi32.dll")
	Setupapi = windows.NewLazySystemDLL("setupapi.dll")

	procGetExtendedUdpTable = NewBoundProc6(Iphlpapi, "GetExtendedUdpTable", CheckErrno)
	procGetExtendedTcpTable = NewBoundProc6(Iphlpapi, "GetExtendedTcpTable", CheckErrno)

	// Secure: restricts DLL search path strictly to %SystemRoot%\System32

	// Note: QueryFullProcessNameW expects 'size' to include the null terminator
	// on input, and returns the length WITHOUT the null terminator on success.
	procQueryFullProcessName     = NewBoundProc4(Kernel32, "QueryFullProcessImageNameW", CheckBool)
	procCreateToolhelp32Snapshot = NewBoundProc2(Kernel32, "CreateToolhelp32Snapshot", CheckHandle)
	procProcess32First           = NewBoundProc2(Kernel32, "Process32FirstW", CheckBool)
	procProcess32Next            = NewBoundProc2(Kernel32, "Process32NextW", CheckBool)

	//procWriteConsoleInputW = Kernel32.NewProc("WriteConsoleInputW")
	procWriteConsoleInputW = NewBoundProc4(Kernel32, "WriteConsoleInputW", CheckBool)

	//procReadConsoleInputW = Kernel32.NewProc("ReadConsoleInputW")
	//procPeekConsoleInputW = Kernel32.NewProc("PeekConsoleInputW")
	procPeekConsoleInputW = NewBoundProc4(Kernel32, "PeekConsoleInputW", CheckBool)
	procReadConsoleInputW = NewBoundProc4(Kernel32, "ReadConsoleInputW", CheckBool)

	//procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
	procSetConsoleCtrlHandler = NewBoundProc2(Kernel32, "SetConsoleCtrlHandler", CheckBool)

	// procNtSetInformationProcess = ntdll.NewProc("NtSetInformationProcess")
	procNtSetInformationProcess = NewBoundProc4(Ntdll, "NtSetInformationProcess", CheckNTSTATUS) // NTSTATUS (0 == success)

	// procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	// procSetWindowsHookEx    = user32.NewProc("SetWindowsHookExW")
	// procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	// procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	// procGetMessage          = user32.NewProc("GetMessageW")
	// procTranslateMessage    = user32.NewProc("TranslateMessage")
	// procDispatchMessage     = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = NewBoundProc1(User32, "PostQuitMessage", CheckNone) // void-ish, but safe
	procSetWindowsHookEx    = NewBoundProc4(User32, "SetWindowsHookExW", CheckNull)
	procCallNextHookEx      = NewBoundProc4(User32, "CallNextHookEx", CheckNone) // returns next hook result
	procUnhookWindowsHookEx = NewBoundProc1(User32, "UnhookWindowsHookEx", CheckBool)
	procGetMessage          = NewBoundProc4(User32, "GetMessageW", CheckMinusOne) // -1 on error, 0 on WM_QUIT
	procSendMessage         = NewBoundProc4(User32, "SendMessageW", CheckNone)    // LRESULT
	procTranslateMessage    = NewBoundProc1(User32, "TranslateMessage", CheckNone)
	procDispatchMessage     = NewBoundProc1(User32, "DispatchMessageW", CheckNone) // returns value from window proc

	// procGetAsyncKeyState    = user32.NewProc("GetAsyncKeyState")
	// procWindowFromPoint     = user32.NewProc("WindowFromPoint")
	// procGetAncestor         = user32.NewProc("GetAncestor")
	// procReleaseCapture      = user32.NewProc("ReleaseCapture") // Releases mouse capture if any window has it
	// procSendMessage         = user32.NewProc("SendMessageW")
	// procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procGetAsyncKeyState = NewBoundProc1(User32, "GetAsyncKeyState", CheckNone) // returns short

	procWindowFromPoint     = NewBoundProc1(User32, "WindowFromPoint", CheckNone)          //changed from CheckNull
	procGetAncestor         = NewBoundProc2(User32, "GetAncestor", CheckNullWithLastError) //was CheckNone //changed from CheckNull
	procSetForegroundWindow = NewBoundProc1(User32, "SetForegroundWindow", CheckNone)      //changed from CheckBool

	procReleaseCapture = NewBoundProc0(User32, "ReleaseCapture", CheckBool) // Releases mouse capture if any window has it

	// procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")
	// procDestroyWindow   = user32.NewProc("DestroyWindow")
	procShellNotifyIcon = NewBoundProc2(Shell32, "Shell_NotifyIconW", CheckBool)
	procDestroyWindow   = NewBoundProc1(User32, "DestroyWindow", CheckBool)

	//procSendMessageTimeout = user32.NewProc("SendMessageTimeoutW")
	procSendMessageTimeout = NewBoundProc7(User32, "SendMessageTimeoutW", CheckZero) // or CheckErrno depending on usage

	// procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	// procGetWindowPlacement       = user32.NewProc("GetWindowPlacement")
	// procGetWindowRect            = user32.NewProc("GetWindowRect")
	// procShowWindow               = user32.NewProc("ShowWindow")
	// procSetWindowPos             = user32.NewProc("SetWindowPos")
	procGetWindowThreadProcessID = NewBoundProc2(User32, "GetWindowThreadProcessId", CheckZero)
	procGetWindowPlacement       = NewBoundProc2(User32, "GetWindowPlacement", CheckBool)
	procGetWindowRect            = NewBoundProc2(User32, "GetWindowRect", CheckBool)
	procGetClientRect            = NewBoundProc2(User32, "GetClientRect", CheckBool)

	//If the window was previously visible, the return value is nonzero.
	//If the window was previously hidden, the return value is zero.
	// so it's technically a CheckBool but it's not an error, it's just an fyi
	procShowWindow = NewBoundProc2(User32, "ShowWindow", CheckNone)

	procSetWindowPos = NewBoundProc7(User32, "SetWindowPos", CheckBool)

	// procDefWindowProc   = user32.NewProc("DefWindowProcW")
	// procRegisterClassEx = user32.NewProc("RegisterClassExW")
	// procCreateWindowEx  = user32.NewProc("CreateWindowExW")
	procDefWindowProc   = NewBoundProc4(User32, "DefWindowProcW", CheckNone)   // LRESULT
	procRegisterClassEx = NewBoundProc1(User32, "RegisterClassExW", CheckZero) // atom / 0 on fail
	procCreateWindowEx  = NewBoundProc12(User32, "CreateWindowExW", CheckNull)

	// procGetModuleHandle = kernel32.NewProc("GetModuleHandleW")
	procGetModuleHandle = NewBoundProc1(Kernel32, "GetModuleHandleW", CheckNull)

	// procSetCapture = user32.NewProc("SetCapture")
	// procSetConsoleCtrlHandler = kernel32.NewProc("SetConsoleCtrlHandler")
	// procGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	procSetCapture          = NewBoundProc1(User32, "SetCapture", CheckNone)
	procGetCapture          = NewBoundProc0(User32, "GetCapture", CheckNone)
	procGetForegroundWindow = NewBoundProc0(User32, "GetForegroundWindow", CheckNone)

	// procCreatePopupMenu = user32.NewProc("CreatePopupMenu")
	// procAppendMenu      = user32.NewProc("AppendMenuW")
	// procTrackPopupMenu  = user32.NewProc("TrackPopupMenu")
	// procGetCursorPos    = user32.NewProc("GetCursorPos")
	procCreatePopupMenu = NewBoundProc0(User32, "CreatePopupMenu", CheckNull)
	procAppendMenu      = NewBoundProc4(User32, "AppendMenuW", CheckBool)

	//"This API returns BOOL only if TPM_RETURNCMD is specified. Otherwise it returns nonzero merely because the menu was displayed.If you don't always pass TPM_RETURNCMD, CheckBool is fine. If you do always pass TPM_RETURNCMD, then returning 0 may simply mean the user dismissed the menu without choosing anything." - chatgpt 5.5
	//well CheckNullWithLastError is bad too! because it blocks and during that time other syscalls can setlasterror thus the seen one when this one's done/dismissed is the last one that setlasterror which is unrelated to this syscall!
	// procTrackPopupMenu = NewBoundProc7(User32, "TrackPopupMenu", CheckNullWithLastError) //was CheckNone
	procTrackPopupMenuBool = NewBoundProc7(User32, "TrackPopupMenu", CheckBool) //for the caller who avoids the use of TPM_RETURNCMD
	procTrackPopupMenuCmd  = NewBoundProc7(User32, "TrackPopupMenu", CheckNone) //for the caller that forces the use of TPM_RETURNCMD

	procDestroyMenu  = NewBoundProc1(User32, "DestroyMenu", CheckBool)
	procGetCursorPos = NewBoundProc1(User32, "GetCursorPos", CheckBool)

	// procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	// procSetProcessDpiAwareness        = shcore.NewProc("SetProcessDpiAwareness")
	procSetProcessDpiAwarenessContext = NewLazyBoundProc1(User32, "SetProcessDpiAwarenessContext", CheckBool)
	procSetProcessDpiAwareness        = NewLazyBoundProc1(Shcore, "SetProcessDpiAwareness", CheckHRESULT)

	// procAttachThreadInput = user32.NewProc("AttachThreadInput")
	procAttachThreadInput = NewBoundProc3(User32, "AttachThreadInput", CheckBool)

	// procPostMessage       = user32.NewProc("PostMessageW")
	// procPostThreadMessage = user32.NewProc("PostThreadMessageW")
	procPostMessage       = NewBoundProc4(User32, "PostMessageW", CheckBool)
	procPostThreadMessage = NewBoundProc4(User32, "PostThreadMessageW", CheckBool)

	// procGetLastError = kernel32.NewProc("GetLastError")

	// // procGetLastError holds the lazy proc handle for kernel32.dll's GetLastError.
	// //
	// // Deprecated: Do not call procGetLastError.Call() directly. Go's runtime wipes
	// // LastError prior to executing DLL calls, causing it to return 0. Use the 3rd
	// // return argument (err) from other proc.Call() invocations instead.
	// procGetLastError = NewBoundProc(Kernel32, "GetLastError", CheckNone) //don't use, see: https://github.com/golang/go/issues/41220

	// procSendInput = user32.NewProc("SendInput")
	procSendInput = NewBoundProc3(User32, "SendInput", CheckZero) // UINT (count)

	procSetConsoleTextAttribute = NewBoundProc2(Kernel32, "SetConsoleTextAttribute", CheckBool)

	procUpdateWindow = NewBoundProc1(User32, "UpdateWindow", CheckBool)

	// procLoadIcon  = user32.NewProc("LoadIconW")
	procLoadIcon = NewBoundProc2(User32, "LoadIconW", CheckNull)

	// procLoadImage = user32.NewProc("LoadImageW")
	procLoadImageW = NewBoundProc6(User32, "LoadImageW", CheckNull)

	// procUnregisterClassW = user32.NewProc("UnregisterClassW")
	procUnregisterClassW = NewBoundProc2(User32, "UnregisterClassW", CheckBool)

	// Priority / process
	// procSetPriorityClass  = kernel32.NewProc("SetPriorityClass")
	// procGetPriorityClass  = kernel32.NewProc("GetPriorityClass")
	// procGetCurrentProcess = kernel32.NewProc("GetCurrentProcess")
	// procGetCurrentThread  = kernel32.NewProc("GetCurrentThread")
	// procSetThreadPriority = kernel32.NewProc("SetThreadPriority")
	// procGetThreadPriority = kernel32.NewProc("GetThreadPriority")
	procSetPriorityClass  = NewBoundProc2(Kernel32, "SetPriorityClass", CheckBool)
	procGetPriorityClass  = NewBoundProc1(Kernel32, "GetPriorityClass", CheckZero)
	procGetCurrentProcess = NewBoundProc0(Kernel32, "GetCurrentProcess", CheckEquals(CURRENT_PROCESS_PSEUDO_HANDLE))
	procGetCurrentThread  = NewBoundProc0(Kernel32, "GetCurrentThread", CheckEquals(CURRENT_THREAD_PSEUDO_HANDLE))
	procSetThreadPriority = NewBoundProc2(Kernel32, "SetThreadPriority", CheckBool)
	procGetThreadPriority = NewBoundProc1(Kernel32, "GetThreadPriority", CheckThreadPriority)

	// procSetProcessInformation    = kernel32.NewProc("SetProcessInformation")
	// procSetProcessWorkingSetSize = kernel32.NewProc("SetProcessWorkingSetSize")
	procSetProcessInformation    = NewBoundProc4(Kernel32, "SetProcessInformation", CheckBool)
	procSetProcessWorkingSetSize = NewBoundProc3(Kernel32, "SetProcessWorkingSetSize", CheckBool)

	// GDI / layered
	// procSetLayeredWindowAttributes = user32.NewProc("SetLayeredWindowAttributes")
	// procBeginPaint                 = user32.NewProc("BeginPaint")
	// procEndPaint                   = user32.NewProc("EndPaint")
	// procDrawText                   = user32.NewProc("DrawTextW")
	// procFillRect                   = user32.NewProc("FillRect")
	procSetLayeredWindowAttributes = NewBoundProc4(User32, "SetLayeredWindowAttributes", CheckBool)
	procBeginPaint                 = NewBoundProc2(User32, "BeginPaint", CheckNull)

	//(Note on procEndPaint: You bound it with CheckBool. MSDN explicitly dictates that EndPaint ALWAYS returns a non-zero value. Because it can never return 0, CheckBool will simply never trigger an error, making it perfectly safe).
	procEndPaint = NewBoundProc2(User32, "EndPaint", CheckNone) //changed from CheckBool

	procDrawText = NewBoundProc5(User32, "DrawTextW", CheckZero)
	procFillRect = NewBoundProc3(User32, "FillRect", CheckZero)

	// procGdiSetTextColor     = gdi32.NewProc("SetTextColor")
	// procGdiSetBkMode        = gdi32.NewProc("SetBkMode")
	// procGdiCreateSolidBrush = gdi32.NewProc("CreateSolidBrush")
	// procGdiDeleteObject     = gdi32.NewProc("DeleteObject")
	procGdiSetTextColor     = NewBoundProc2(Gdi32, "SetTextColor", CheckGDIError)
	procGdiSetBkMode        = NewBoundProc2(Gdi32, "SetBkMode", CheckZero)
	procGdiCreateSolidBrush = NewBoundProc1(Gdi32, "CreateSolidBrush", CheckNull)
	procGdiDeleteObject     = NewBoundProc1(Gdi32, "DeleteObject", CheckBool)

	// procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	// procSetCursorPos     = user32.NewProc("SetCursorPos")

	//Per Win32 docs, GetSystemMetrics returns 0 for both "the queried value is legitimately 0" and "the index is invalid/unsupported" — it does not set GetLastError in any meaningful way for these system-metric indices.
	procGetSystemMetrics = NewBoundProc1(User32, "GetSystemMetrics", CheckNone) // returns int, 0 on failure for most indices
	procSetCursorPos     = NewBoundProc2(User32, "SetCursorPos", CheckBool)
	// LoadCursorW returns a shared system cursor handle; failure is NULL.
	// SetCursor returns the previous HCURSOR (0 is a legitimate prior value
	// and does not set GetLastError), so CheckNone — the public wrapper
	// returns that previous handle as windows.Handle, matching SetCapture.
	procLoadCursorW = NewBoundProc2(User32, "LoadCursorW", CheckNull)
	procSetCursor   = NewBoundProc1(User32, "SetCursor", CheckNone)

	// procInvalidateRect = user32.NewProc("InvalidateRect")
	procInvalidateRect = NewBoundProc3(User32, "InvalidateRect", CheckBool)

	// GetWindowLongPtrW returns LONG_PTR (can be 0 legitimately); we treat non-zero as "success" for most usages
	//Microsoft’s documentation for GetWindowLongPtrW explicitly states: "If the function fails, the return value is zero." It never returns a non-zero value when an error occurs.
	procGetWindowLongPtrW = NewBoundProc2(User32, "GetWindowLongPtrW", CheckNullWithLastError) //was CheckNone and CheckNull is not enough!
	//procSetLastError      = NewBoundProc(Kernel32, "SetLastError", CheckNone) // void-like, useless call, don't use! it's always nil on beginning of each .Call() anyway, as per: https://github.com/golang/go/issues/41220
	// procGetWindowLongPtrW = user32.NewProc("GetWindowLongPtrW")
	// procSetLastError      = kernel32.NewProc("SetLastError")

	// procCreateMutex  = kernel32.NewProc("CreateMutexW")
	// procReleaseMutex = kernel32.NewProc("ReleaseMutex")
	// procCloseHandle  = kernel32.NewProc("CloseHandle")
	procCreateMutex  = NewBoundProc3(Kernel32, "CreateMutexW", CheckNull)
	procReleaseMutex = NewBoundProc1(Kernel32, "ReleaseMutex", CheckBool)
	procCloseHandle  = NewBoundProc1(Kernel32, "CloseHandle", CheckBool)

	// procQueryWorkingSetEx = psapi.NewProc("QueryWorkingSetEx")
	procQueryWorkingSetEx = NewBoundProc3(Psapi, "QueryWorkingSetEx", CheckBool)

	// procOpenProcessToken      = advapi32.NewProc("OpenProcessToken")
	// procLookupPrivilegeValue  = advapi32.NewProc("LookupPrivilegeValueW")
	// procAdjustTokenPrivileges = advapi32.NewProc("AdjustTokenPrivileges")
	procOpenProcessToken     = NewBoundProc3(Advapi32, "OpenProcessToken", CheckBool)
	procLookupPrivilegeValue = NewBoundProc3(Advapi32, "LookupPrivilegeValueW", CheckBool)
	// AdjustTokenPrivileges is special: returns BOOL but sets LastError even on partial success (ERROR_NOT_ALL_ASSIGNED)
	procAdjustTokenPrivileges = NewBoundProc6(Advapi32, "AdjustTokenPrivileges", CheckAdjustTokenPrivileges)

	// procGetClassName = user32.NewProc("GetClassNameW")
	procGetClassName = NewBoundProc3(User32, "GetClassNameW", CheckZero) // returns length

	// procInternalGetWindowText = user32.NewProc("InternalGetWindowText")
	// procInternalGetWindowText = user32.NewProc("InternalGetWindowText")
	// Bound with CheckNone rather than CheckStringLength: InternalGetWindowText
	// can legitimately return 0 for an empty title WITHOUT calling
	// SetLastError, so WinCall's automatically-captured callErr may just be
	// stale cruft left over from some unrelated earlier call -- CheckStringLength
	// would then misreport a normal empty title as a failure. getWindowTextFast
	// instead clears the error state itself immediately before the call and
	// checks GetLastError() freshly afterward, the same pattern getWindowLongPtr
	// already uses for the identical class of "0 is valid, GetLastError is the
	// only real signal" API.
	procInternalGetWindowText = NewBoundProc3(User32, "InternalGetWindowText", CheckNone) // returns length

	// procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleWindow = NewBoundProc0(Kernel32, "GetConsoleWindow", CheckNone)

	procFreeConsole = NewBoundProc0(Kernel32, "FreeConsole", CheckBool)

	// procSetWinEventHook = user32.NewProc("SetWinEventHook")
	// procUnhookWinEvent  = user32.NewProc("UnhookWinEvent")
	procSetWinEventHook = NewBoundProc7(User32, "SetWinEventHook", CheckNull)
	procUnhookWinEvent  = NewBoundProc1(User32, "UnhookWinEvent", CheckBool)

	procWTSRegisterSessionNotification   = NewBoundProc2(Wtsapi32, "WTSRegisterSessionNotification", CheckBool)
	procWTSUnRegisterSessionNotification = NewBoundProc1(Wtsapi32, "WTSUnRegisterSessionNotification", CheckBool)

	procMonitorFromWindow = NewBoundProc2(User32, "MonitorFromWindow", CheckNone) // returns HMONITOR; 0 means no monitor
	procGetMonitorInfo    = NewBoundProc2(User32, "GetMonitorInfoW", CheckBool)

	// procGetTopWindow/procGetWindow: like procGetForegroundWindow, 0 is a
	// legitimate "no such window in that relationship" result (empty
	// desktop, no more siblings, etc.), not an unambiguous failure signal.
	// Bound CheckNullWithLastError rather than CheckNone: a genuine failure
	// (e.g. an invalid/destroyed hwnd) does set GetLastError, so this lets
	// WinResult.Failed() distinguish "no window in that relationship" (r1==0,
	// no error) from "the call actually failed" (r1==0, error set) — callers
	// should still treat r1==0 as "nothing found" whenever .Succeeded().
	procGetTopWindow = NewBoundProc1(User32, "GetTopWindow", CheckNullWithLastError) // was CheckNone
	procGetWindow    = NewBoundProc2(User32, "GetWindow", CheckNullWithLastError)    // was CheckNone

	// procIsWindowVisible's BOOL return is the actual meaningful result
	// (visible or not), not a failure indicator -- also CheckNone.
	procIsWindowVisible = NewBoundProc1(User32, "IsWindowVisible", CheckNone)
	procIsWindow        = NewBoundProc1(User32, "IsWindow", CheckNone)

	// Iphlpapi routing procs
	procGetBestInterface     = NewBoundProc2(Iphlpapi, "GetBestInterface", CheckErrno)
	procGetIPForwardTable    = NewBoundProc3(Iphlpapi, "GetIpForwardTable", CheckErrno)
	procCreateIPForwardEntry = NewBoundProc1(Iphlpapi, "CreateIpForwardEntry", CheckErrno)
	procDeleteIPForwardEntry = NewBoundProc1(Iphlpapi, "DeleteIpForwardEntry", CheckErrno)
	procGetIfTable           = NewBoundProc3(Iphlpapi, "GetIfTable", CheckErrno)
	procGetIPAddrTable       = NewBoundProc3(Iphlpapi, "GetIpAddrTable", CheckErrno)

	procSetupDiGetClassDevs              = NewBoundProc4(Setupapi, "SetupDiGetClassDevsW", CheckHandle)
	procSetupDiEnumDeviceInfo            = NewBoundProc3(Setupapi, "SetupDiEnumDeviceInfo", CheckBool)
	procSetupDiDestroyDeviceInfoList     = NewBoundProc1(Setupapi, "SetupDiDestroyDeviceInfoList", CheckBool)
	procSetupDiGetDeviceInstanceId       = NewBoundProc5(Setupapi, "SetupDiGetDeviceInstanceIdW", CheckBool)
	procSetupDiGetDeviceRegistryProperty = NewBoundProc7(Setupapi, "SetupDiGetDeviceRegistryPropertyW", CheckBool)
	procSetupDiSetClassInstallParams     = NewBoundProc4(Setupapi, "SetupDiSetClassInstallParamsW", CheckBool)
	procSetupDiCallClassInstaller        = NewBoundProc3(Setupapi, "SetupDiCallClassInstaller", CheckBool)
)

// auto runs before main(), loads the DLLs non-lazily.
func init() {
	loadDll(Kernel32)
	loadDll(Iphlpapi)
}

func loadDll(dll *windows.LazyDLL) {
	if dll == nil {
		panic2("BUG: you passed a nil dll to loadDll(dll)")
		panic(nil)
	}
	err := dll.Load()
	if err != nil {
		//wekeepitsoTODO: technically not a "BUG: " yet using panic2 means it will use GetBugLogger() to log it!
		panic2("critical system dll " + dll.Name + " not found, error: " + err.Error())
	}
}

// callWithRetry is a generic internal helper that manages the "query size,
// allocate, fetch data" pattern common in Windows network APIs.
//
// It handles the race condition where the required buffer size grows between
// the query and the fetch by retrying up to MAX_RETRIES times.
//
// Arguments:
//   - initialSize: The size to use for the first attempt (0 to query first).
//   - call: A closure that wraps the actual Windows syscall.
//
// Returns the populated byte slice on success, or an error if the API fails
// for reasons other than buffer size, or if it fails to stabilize after retries.
func callWithRetry(who string, initialSize uint32, call func(bufPtr *byte, s *uint32) error) ([]byte, error) {
	size := initialSize
	const MAX_RETRIES = 10
	for tries := 1; tries <= MAX_RETRIES; tries++ { // tries will be 1, 2, 3, ..., MAX_RETRIES
		// If size is 0, we're just probing. If > 0, we're allocating.
		var buf []byte
		var ptr *byte = nil //implied anyway
		if size > 0 {
			buf = make([]byte, size) //+8) // 8 extra bytes
			ptr = &buf[0]            // Keep it as a real, GC-visible pointer
			/*
				fmt.Printf with the %p verb treats a slice value specially: for a slice,
					%p prints the address of the first element (the Data pointer), not the address of the slice descriptor.
					The slice variable itself is a three-word header (pointer, len, cap) stored on the stack (or heap).
					The header's address is &buf; the header's Data field (pointer to element 0) is what fmt prints for %p when given a slice.

				So:

				    buf (the slice) ≠ &buf (address of the header).
				    fmt.Printf("%p", buf) prints buf's Data pointer (same as &buf[0] when len>0).
				    To print the header address use fmt.Printf("%p", &buf). To print the Data pointer explicitly
					use fmt.Printf("%p", unsafe.Pointer(&buf[0])) (only when len>0).

			*/
		}
		err := call(ptr, &size)

		if err == nil {
			if uint64(size) > uint64(len(buf)) {
				impossibiru(who + ":callWithRetry: size is bigger than len(buf)")
			}
			return buf, nil // epic fail here if returning buf[:size] because size is 0 even tho servicesReturned is > 0
			//return buf[:size], nil // fixed one issue! nope this "fix" was wrong because: The size parameter is only reliable when the API returns ERROR_MORE_DATA or ERROR_INSUFFICIENT_BUFFER. On success it is frequently set to 0, even when the buffer contains real data.
		}

		// Windows uses both INSUFFICIENT_BUFFER and MORE_DATA
		// to signal that we need a bigger boat.
		//GetExtendedUdpTable usually returns ERROR_INSUFFICIENT_BUFFER when the buffer is too small.
		//EnumServicesStatusEx (and many Enumeration APIs) returns ERROR_MORE_DATA.
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) &&
			!errors.Is(err, windows.ERROR_MORE_DATA) {
			// Wrap with %w so errors.Is/As against the underlying sentinel
			// (e.g. windows.ERROR_ACCESS_DENIED) still works for callers,
			// while making clear in the message that the failure surfaced
			// through callWithRetry's size-query/allocate/fetch loop rather
			// than directly from the caller's own call site.
			return nil, fmt.Errorf("%s:callWithRetry: underlying call failed: %w", who, err)
		}
		// 	// Loop continues, using the updated 'size' from the failed call
		// 	//however:
		// 	// If size didn't increase but we still got an error,
		// 	// we should nudge it upward to prevent an infinite loop.
		// 	// We use uint64 casts to satisfy gosec G115.
		// 	// 1. Convert both to uint64 to compare safely without narrowing (Fixes G115)
		// 	if uint64(size) <= uint64(len(buf)) {
		// 		// 2. Check for overflow before adding 1024
		// 		const increment = 1024
		// 		const MaxInt = math.MaxUint32
		// 		if MaxInt-size < increment {
		// 			return nil, fmt.Errorf("%s:callWithRetry: buffer size(%d) would overflow uint32(%d) if adding %d", who, size, MaxInt, increment)
		// 		}
		// 		size += increment
		// 	}

		// Loop continues, using the updated 'size' from the failed call
		//however:
		// If size (the API's own newly-reported value) isn't meaningfully
		// bigger than what we JUST tried, we should nudge it upward
		// ourselves to prevent an infinite loop. We grow relative to our own
		// last-attempted buffer (len(buf)), NOT relative to 'size' itself:
		// for paginated/resumable enumeration APIs (e.g. EnumServicesStatusEx,
		// used by GetServiceNamesFromPIDUncached), the documented "bytes
		// needed" value means "bytes needed for the REMAINING entries only"
		// once some entries already fit in a partially-successful call, not
		// "total bytes needed from scratch" -- and because every retry here
		// restarts enumeration fresh (GetServiceNamesFromPIDUncached resets
		// its resume handle to 0 on every attempt), a fresh retry genuinely
		// needs the FULL total again. Trusting a possibly-"remaining-only"
		// value directly could undersize the next buffer and repeat the
		// same partial-fill/ERROR_MORE_DATA cycle. Doubling our own last
		// size (matching QueryFullProcessName's identical growth strategy)
		// sidesteps that ambiguity entirely and also reaches multi-megabyte
		// sizes within only a handful of retries for a heavily loaded table,
		// instead of a flat +1KB per retry potentially exhausting all
		// MAX_RETRIES attempts while still coming up short.
		// We use uint64 casts throughout to satisfy gosec G115.
		if uint64(size) <= uint64(len(buf)) {
			const minIncrement uint64 = 1024 // floor, in case len(buf) is currently 0 or tiny
			next := uint64(len(buf)) * 2
			if grown := uint64(len(buf)) + minIncrement; next < grown {
				next = grown
			}
			if next > math.MaxUint32 {
				return nil, fmt.Errorf("%s:callWithRetry: buffer size(%d) would overflow uint32(%d) if grown to %d", who, len(buf), uint32(math.MaxUint32), next)
			}
			size = uint32(next)
		}
	}
	return nil, fmt.Errorf("%s:callWithRetry: buffer growth exceeded max retries(%d)", who, MAX_RETRIES)
}

// boolToUintptr converts a Go bool to a uintptr (1 for true, 0 for false)
// for use in Windows syscalls.
//
// boolToUintptr performs an explicit conversion from a Go bool to a
// Windows-compatible BOOL (uintptr(1) for true, uintptr(0) for false).
// This is required because Go bools cannot be directly cast to numeric types.
//
//go:inline
func boolToUintptr(b bool) uintptr {
	if b {
		return 1
	}
	return 0
}

// GetExtendedUDPTable retrieves the system UDP table using the Windows
// GetExtendedUdpTable API and returns the raw buffer containing the table data.
//
// This is a higher-level wrapper over the low-level bound call
// (callGetExtendedUdpTable). It encapsulates:
//
//   - the two-call pattern required by the API (size query + data fetch)
//   - conversion of Win32 error codes into Go errors via wincall.CheckErrno
//   - handling of ERROR_INSUFFICIENT_BUFFER as part of normal control flow
//
// The returned []byte contains a MIB_UDPTABLE_OWNER_PID (or related) structure,
// depending on the flags used internally. Callers are responsible for parsing
// the buffer according to the expected Windows structure layout.
//
// Guarantees:
//   - returns a non-nil error if the underlying API reports failure
//   - never requires callers to inspect r1 or perform manual error checks
//
// Edge cases handled:
//   - initial size query returning ERROR_INSUFFICIENT_BUFFER
//   - empty table responses (size 0) returning (nil, nil)
//   - propagation of underlying Windows errors with errors.Is compatibility
//
// Note:
//   - this function intentionally operates on raw bytes to avoid committing
//     to a specific struct layout; build a typed parser on top if needed.
func GetExtendedUDPTable(order bool, family uint32) ([]byte, error) {
	return callWithRetry("GetExtendedUDPTable", 0, func(bufPtr *byte, s *uint32) error {
		res1 := procGetExtendedUdpTable.Call(
			uintptr(unsafe.Pointer(bufPtr)),
			uintptr(unsafe.Pointer(s)),
			boolToUintptr(order),
			uintptr(family),
			uintptr(UDP_TABLE_OWNER_PID),
			0,
		)
		return res1.Err
	})
}

// GetExtendedTCPTable retrieves the system TCP table.
// It follows the same contract as GetExtendedUDPTable.
func GetExtendedTCPTable(order bool, family uint32) ([]byte, error) {
	return callWithRetry("GetExtendedTCPTable", 0, func(bufPtr *byte, s *uint32) error {
		res1 := procGetExtendedTcpTable.Call(
			uintptr(unsafe.Pointer(bufPtr)),
			uintptr(unsafe.Pointer(s)),
			boolToUintptr(order),
			uintptr(family),
			uintptr(TCP_TABLE_OWNER_PID_ALL), // Value 5: Get all states + PID
			0,
		)
		return res1.Err
	})
}

// QueryFullProcessName retrieves the full executable path of a process given its PID.
//
// This is a higher-level wrapper over callQueryFullProcessName.
// It encapsulates:
//
//   - opening the process handle with PROCESS_QUERY_LIMITED_INFORMATION
//   - preparing a buffer for the UTF16 path
//   - calling the Windows API
//   - converting UTF16 to Go string
//
// Returns a non-empty string and nil error on success, or an empty string with error on failure.
func QueryFullProcessName(pid uint32) (string, error) {
	hProc, err0 := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err0 != nil {
		return "", fmt.Errorf("OpenProcess failed for PID %d: %w", pid, err0)
	}
	//defer windows.CloseHandle(hProc)
	defer CloseHandleLogged(&hProc, "QueryFullProcessName:OpenProcess hProc")

	// Start with MAX_PATH (260)
	//Yes, size remains a uint32 on both x86 and x64. This is because the Windows API function QueryFullProcessImageNameW
	// explicitly defines that parameter as a PDWORD (a pointer to a 32-bit unsigned integer), regardless of the processor architecture.
	size := uint32(windows.MAX_PATH)
	var tries uint64 = 1
	for {
		buf := make([]uint16, size)
		currentCap := uint64(len(buf))
		if currentCap != uint64(size) { // must cast else compile error!
			impossibiru(fmt.Sprintf("currentCap(%d) != size(%d), after %d tries", currentCap, size, tries))
		}

		// Note: QueryFullProcessNameW expects 'size' to include the null terminator
		// on input, and returns the length WITHOUT the null terminator on success.

		res1 := procQueryFullProcessName.Call(
			uintptr(hProc),
			0,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&size)),
		)
		if res1.Succeeded() { //err == nil {
			// Success! Convert the returned size to string
			//UTF16ToString is a function that looks for a 0x0000 (null).
			//Go's windows.UTF16ToString is safely implemented to stop at the first \x00 it finds, OR the end of the slice provided to it.
			//size is just a number the API handed back, so let's not trust it, thus use full 'buf'
			// return windows.UTF16ToString(buf), nil
			// Previously, the code distrusted 'size' and passed the full 'buf'.
			// However, if the path perfectly hits the buffer boundary (e.g., exactly MaxExtendedPath),
			// Windows might omit the null terminator entirely.
			// Slicing to `[:size]` is much safer: it forces `UTF16ToString` to process exactly
			// the characters Windows explicitly claims to have written, preventing silent
			// truncation bugs or unnecessary scanning of trailing nulls.
			// limit := size
			// if limit > uint32(len(buf)) {
			// 	limit = uint32(len(buf)) // Defense-in-depth: never out-of-bounds slice if the OS lies
			// }
			limit := min(int(size), len(buf))
			return windows.UTF16ToString(buf[:limit]), nil
		}

		// Check if the error is specifically "Buffer too small"
		// syscall.ERROR_INSUFFICIENT_BUFFER = 0x7A
		//if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
		if !res1.ErrIs(windows.ERROR_INSUFFICIENT_BUFFER) {
			return "", fmt.Errorf("QueryFullProcessNameW failed after %d tries, err: '%w'", tries, res1.Err)
		}
		//else the desired 'size' now includes the nul terminator, so no need to +1 it

		// currentCap is what we just allocated; nextSize is what the API told us it wants.
		nextSize := uint64(size) //this is api suggested size now! ie. modified! so it's not same as currentCap!

		// If API didn't suggest a larger size, we manually double.
		if nextSize <= currentCap {
			nextSize = currentCap * 2
		}

		if currentCap < MaxExtendedPath && nextSize > MaxExtendedPath {
			// cap it once! in case we doubled it or (unlikely)api suggested more!(in the latter case it will fail the next syscall)
			nextSize = MaxExtendedPath
		}

		// Stern check against the Windows limit (32767) and the uint32 limit.
		if nextSize > MaxExtendedPath || nextSize > math.MaxUint32 {
			return "", fmt.Errorf("buffer size %d exceeds limit, after %d tries", nextSize, tries)
		}

		size = uint32(nextSize)
		tries += 1
	} // infinite 'for'
}

func impossibiru(msg string) {
	msg2 := fmt.Sprintf("Impossible: '%s'", msg)
	panic2(msg2)
}

// logCriticalThenPanic logs msg through log (which may be asynchronous/
// buffered -- e.g. bridged into a caller's own async log pipeline, as
// winbollocks does via initWincoeLogging) AND, defensively, writes the
// exact same message directly and synchronously to os.Stderr before
// panicking. This guarantees the diagnostic explaining WHY the process is
// about to crash is never solely dependent on some caller's async logger
// successfully draining its buffer before the panic unwinds: since wincoe
// is a general-purpose library, a panic originating here could just as
// easily be on a goroutine with no relevant defer/recover chain at all, in
// which case an async logger's buffered message could be lost forever when
// the process dies almost immediately afterward.
func logCriticalThenPanic(log *slog.Logger, msg string) {
	log.Error(msg)
	// Best-effort synchronous fallback in case log (which may be async/
	// buffered by the caller) doesn't flush before the panic unwinds — see
	// this function's own doc comment above.
	//
	// If the process has no console at all (built with -H=windowsgui, no
	// console subsystem attached at creation), os.Stderr's underlying
	// handle is invalid; Write simply returns an error in that case rather
	// than panicking or blocking, and that error is intentionally ignored
	// below (we're about to panic regardless, and there's no console for
	// the message to reach even if we retried).
	fmt.Fprintf(os.Stderr, "%s\n", msg) //nolint:errcheck // best-effort; we're about to panic regardless
	panic(msg)
}
func panic2(msg string) {
	logCriticalThenPanic(GetBugLogger(), msg)
}

// bugLogger is a package-level fallback logger used only by free functions
// (not methods on Server/AdminUI) that need to log a BUG-class invariant
// violation immediately before panicking, but have no logger threaded to them.
// Kept in sync with the active logger via applyLogger. Falls back to
// slog.Default() before logging is initialized (mirrors Server.getLogger()'s
// own fallback behavior).
var bugLogger atomic.Pointer[slog.Logger]

func SetBugLogger(newLogger *slog.Logger) {
	bugLogger.Store(newLogger)
}

func GetBugLogger() *slog.Logger {
	if l := bugLogger.Load(); l != nil {
		return l
	}
	//def := slog.Default()
	def := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	return def
}

// GetLoggerOrFallback is the single owner of the "load the current logger
// from a shared, hot-swappable atomic.Pointer[slog.Logger], falling back to
// the process-wide bug logger when it hasn't been initialized yet" behavior.
//
// Every type that holds a live, reloadable logger reference — Server (via
// Runtime/LoggerManager), AdminUI, UpstreamManager, Upstream,
// FailoverSelector, LoggerManager itself, GenericSafeFileWriter, and
// win11SafeFileWriter — used to hand-roll this exact nil-check-then-fallback
// dance in its own getLogger() method, each with slightly different guards
// and message text. This function is now the one place that logic lives;
// every such getLogger() should be a one-line delegate to it.
//
// ptr may itself be nil (some callers hold *atomic.Pointer[slog.Logger] as
// an optional field, e.g. before full initialization); that is treated
// identically to a non-nil ptr that hasn't had Store() called on it yet.
// ownerDesc identifies the calling type/field (e.g. "AdminUI.liveLogger") for
// the diagnostic message logged through the fallback logger.
func GetLoggerOrFallback(ptr *atomic.Pointer[slog.Logger], ownerDesc string) *slog.Logger {
	if ptr != nil {
		if l := ptr.Load(); l != nil {
			return l
		}
	}
	log := GetBugLogger()
	log.Error("BUG: " + ownerDesc + " wasn't initialized before use; using fallback bug logger")
	return log
}

// resolveExeName resolves the executable name/path for owningPid, trying the
// fast path (ExePathFromPID, via QueryFullProcessImageNameW) first and
// falling back to GetProcessName (a Toolhelp32 snapshot walk) if that fails.
// remoteAddrStr is purely diagnostic context folded into the returned error
// if both lookups fail (the remote UDP/TCP address that triggered the pid lookup).
func resolveExeName(owningPid uint32, remoteAddrStr string) (string, error) {
	exe, err2 := ExePathFromPID(owningPid)
	if err2 == nil {
		return exe, nil
	}
	exe, err3 := GetProcessName(owningPid)
	if err3 != nil {
		return "", fmt.Errorf("pid %d not found for %s, errTransient:'%w', err:'%w'", owningPid, remoteAddrStr, err2, err3) //fine, wrap both then!
	}
	return exe, nil
}

// ExePathFromPID returns process image path for pid or an error.
// Uses QueryFullProcessImageNameW. May fail if insufficient privilege.
//
// ExePathFromPID retrieves the full executable path of a process by PID.
//
// This is a higher-level wrapper over callQueryFullProcessName.
// It handles buffer sizing and UTF16 conversion.
//
// it's a wrapper-alias around QueryFullProcessName
func ExePathFromPID(pid uint32) (string, error) {
	return QueryFullProcessName(pid)
}

func GetProcessName(pid uint32) (string, error) {
	snapshot, err := CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return "", err
	}
	//defer windows.CloseHandle(snapshot)
	defer CloseHandleLogged(&snapshot, "GetProcessName:CreateToolhelp32Snapshot snapshot")

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	const maxProcessEntries = 10000
	count := 0
	err = Process32First(snapshot, &entry)
	for err == nil {
		if count > maxProcessEntries {
			return "", fmt.Errorf("Process32 enumeration exceeded safety limit of %d active processes currently running", maxProcessEntries)
		}
		count++
		//doneTODO: make a hard limit here, so it doesn't loop infinitely just in case.
		if entry.ProcessID == pid {
			return windows.UTF16ToString(entry.ExeFile[:]), nil
		}
		err = Process32Next(snapshot, &entry)
	}

	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return "", err
	}
	return "", fmt.Errorf("not found, err: %w", err)
}

// CreateToolhelp32Snapshot creates a snapshot of the specified processes, threads,
// modules, or heaps in the system. The snapshot can then be used with functions
// like Process32First/Next or Module32First/Next to enumerate the captured entries.
//
// In short: it’s a system-wide “frozen view” of processes or other kernel objects, enabling safe enumeration without interference from runtime changes.
//
// Parameters:
//
//	flagdwFlagss - a bitmask specifying what to include in the snapshot (e.g., TH32CS_SNAPPROCESS).
//	th32ProcessID   - for some snapshots, a process ID to restrict the snapshot to a particular process. (0 = all processes)
//
// Returns:
//
//	A handle to the snapshot, which must be closed with CloseHandle when done.
//	INVALID_HANDLE_VALUE indicates failure, with GetLastError providing details.
//
// Typical usage:
//
//	hSnap, err := CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0)
//	if err != nil { ... }
//	defer CloseHandle(hSnap)
//	// enumerate processes with Process32First/Next
//
// Returns a valid windows.Handle on success, or a non-nil error on failure.
//
// Notes:
//
// These flags are bitwise combinable. For example, TH32CS_SNAPPROCESS | TH32CS_SNAPTHREAD captures both processes and threads.
// If a flag isn’t used (e.g., you don’t include TH32CS_SNAPPROCESS), CreateToolhelp32Snapshot will not include that object type in the snapshot.
// TH32CS_SNAPPROCESS specifically tells the API to include all processes in the snapshot. Without it, Process32First/Process32Next won’t enumerate any processes.
func CreateToolhelp32Snapshot(dwFlags, th32ProcessID uint32) (windows.Handle, error) {
	res1 := procCreateToolhelp32Snapshot.Call(
		uintptr(dwFlags),
		uintptr(th32ProcessID),
	)
	if res1.Failed() { //err != nil {
		return 0, res1.Err
	}
	return windows.Handle(res1.R1), nil
}

// Process32First wraps callProcess32First.
func Process32First(snapshot windows.Handle, entry *windows.ProcessEntry32) error {
	if entry == nil {
		return errors.New("Process32First: nil entry")
	}
	res1 := procProcess32First.Call(uintptr(snapshot), uintptr(unsafe.Pointer(entry)))
	return res1.Err
}

// Process32Next wraps callProcess32Next.
func Process32Next(snapshot windows.Handle, entry *windows.ProcessEntry32) error {
	if entry == nil {
		return errors.New("Process32Next: nil entry")
	}
	res1 := procProcess32Next.Call(uintptr(snapshot), uintptr(unsafe.Pointer(entry)))
	return res1.Err
}

// GetServiceNamesFromPIDUncached queries the Service Control Manager to find all service
// names currently associated with a specific Process ID (PID).
//
// This function encapsulates:
//   - opening a remote handle to the SCM with SC_MANAGER_ENUMERATE_SERVICE rights
//   - utilizing callWithRetry to handle the "snapshot" race condition where the
//     number of services changes between the size query and the data fetch
//   - parsing the resulting ENUM_SERVICE_STATUS_PROCESS structure array
//
// Returns a slice of service display names associated with the PID. If no
// services are found for the given PID, it returns (nil, nil).
//
// Guarantees:
//   - returns a non-nil error if SCM access is denied or the RPC call fails
//   - handles ERROR_INSUFFICIENT_BUFFER internally via the retry loop
//   - ensures the SCM handle is closed via defer, even on internal retry failure
//
// Edge cases handled:
//   - services starting/stopping mid-enumeration (handled by 10-try retry logic)
//   - PIDs with zero associated services (returns nil slice, no error)
//   - stale resume handles (reset to 0 on each retry for a fresh full snapshot)
//   - race conditions where the service list grows mid-call (handled by treating ERROR_MORE_DATA as a retry signal)
//
// Note:
//   - This performs a full enumeration of all Win32 services to filter by PID;
//     on systems with hundreds of services, this may involve a ~100KB+ buffer.
func GetServiceNamesFromPIDUncached(targetPID uint32) ([]string, error) {
	//doneTODO: since makeClientInfoContext is called on every single packet, and GetServiceNamesFromPID opens the SCM, enumerates all services, and does unsafe parsing — all under high concurrent load — you might consider caching the PID→services mapping with a short TTL to reduce both the performance impact and the attack surface of concurrent unsafe calls.
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return nil, fmt.Errorf("OpenSCManager failed: %w", err)
	}
	defer func() {
		if xerr := windows.CloseServiceHandle(scm); xerr != nil {
			getLogger().Debug("CloseServiceHandle failed in GetServiceNamesFromPIDUncached:OpenSCManager", slog.String("err", xerr.Error()))
		}
	}()

	// We'll need these to persist across the closure calls
	var servicesReturned uint32

	// Use our retry helper to handle the buffer growth logic
	// We use callWithRetry because the service list is highly volatile.
	buffer, err := callWithRetry("GetServiceNamesFromPIDUncached", 0, func(bufPtr *byte, s *uint32) error {
		// Reset these for each attempt to ensure a fresh enumeration if it retries
		servicesReturned = 0
		// Note: we usually keep resumeHandle at 0 for a fresh start on each retry
		// unless we are specifically doing paged enumeration.
		var currentResumeHandle uint32
		errEnum := windows.EnumServicesStatusEx(
			scm,
			windows.SC_ENUM_PROCESS_INFO,
			windows.SERVICE_WIN32,
			windows.SERVICE_STATE_ALL,
			bufPtr,
			*s,
			s, // bytesNeeded
			&servicesReturned,
			&currentResumeHandle,
			nil,
		)
		if errEnum == nil {
			return nil
		} else {
			return fmt.Errorf("EnumServicesStatusEx failed0: %w", errEnum)
		}
	})

	if err != nil {
		return nil, fmt.Errorf("EnumServicesStatusEx wrapped by callWithRetry, failed: %w", err)
	}
	if buffer == nil {
		return nil, fmt.Errorf("nil buffer from callWithRetry, no error though")
	}
	if len(buffer) == 0 {
		return nil, fmt.Errorf("non-nil buffer with 0 length, from callWithRetry, no error though")
	}

	// Parsing logic remains the same, but now it's protected by the retry logic
	var serviceNames []string
	entrySize := unsafe.Sizeof(windows.ENUM_SERVICE_STATUS_PROCESS{})

	//this 'if' suggested by Claude Sonnet 4.6: (i DRY-ed the 'foo')
	if partialLen := uint64(servicesReturned) * uint64(entrySize); partialLen > uint64(len(buffer)) { // unlikely to ever be hit!
		return nil, fmt.Errorf("servicesReturned(%d) * entrySize(%d) = %d exceeds buffer len(%d): API invariant violated",
			servicesReturned, entrySize, partialLen, len(buffer))
	}
	// DON'T: Trim the buffer to the actual data written, bad Grok 4.20! because data.ServiceName is a pointer past this size, still in the buffer tho!
	//buffer = buffer[:realLen] // BAD! don't do this!

	// Define buffer endpoints as unsafe.Pointers safely using unsafe.Add
	bufStart := unsafe.Pointer(&buffer[0])
	// Point to the exact LAST byte of the buffer, not past it.
	// This prevents the checkptr boundary-crossing panic.
	bufLastByte := unsafe.Add(bufStart, len(buffer)-1)

	bufStartAddr := uintptr(bufStart)
	bufLastByteAddr := uintptr(bufLastByte)

	for i := uint32(0); i < servicesReturned; i++ {
		offset := uintptr(i) * entrySize
		if offset+entrySize > uintptr(len(buffer)) {
			return nil, fmt.Errorf("entry %d at offset %d + entrySize %d exceeds buffer len %d",
				i, offset, entrySize, len(buffer))
		}
		//data := (*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buffer[offset]))
		data := (*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Add(bufStart, offset))

		// Validate ServiceName pointer is within buffer before dereferencing
		// bufStart := uintptr(unsafe.Pointer(&buffer[0]))
		// bufEnd := bufStart + uintptr(len(buffer))
		// snPtr := uintptr(unsafe.Pointer(data.ServiceName))
		//if snPtr < bufStart || snPtr >= bufEnd {

		// Validate ServiceName pointer is within buffer before dereferencing
		snPtr := data.ServiceName // type is *uint16
		snAddr := uintptr(unsafe.Pointer(snPtr))
		// Change the condition to > instead of >= because bufLastByteAddr is inclusive
		if snAddr < bufStartAddr || snAddr > bufLastByteAddr {
			// pointer outside buffer — skip this entry

			// return nil, fmt.Errorf("entry %d at offset %0x which has entrySize %d, in the buffer len %d, "+
			// 	"has a ServiceName ptr outside the buffer=%p area, snPtr=%0x bufStart=%0x bufEnd=%0x",
			// 	i, offset, entrySize, len(buffer),
			// 	buffer, snPtr, bufStart, bufEnd)
			return nil, fmt.Errorf("entry %d at offset %0x which has entrySize %d, in the buffer len %d, "+
				"has a ServiceName ptr outside the buffer=%p area, snPtr=%0x bufStart=%0x bufEnd=%0x",
				i, offset, entrySize, len(buffer),
				buffer, snAddr, bufStartAddr, bufLastByteAddr)
		}

		if data.ServiceStatusProcess.ProcessId == targetPID {
			// Bounded decode instead of windows.UTF16PtrToString: the check
			// above only confirms the string's START address lies within the
			// buffer, not that a null terminator is guaranteed to appear
			// before bufEnd. A corrupted/malformed SCM response could
			// otherwise send UTF16PtrToString scanning past the end of this
			// allocation into unmapped memory.
			str, ok := utf16StringWithinBounds(snPtr, bufLastByte)
			if !ok {
				return nil, fmt.Errorf("entry %d: ServiceName string has no null terminator before bufEnd (snPtr=%0x bufLastByte=%0x)", i, snPtr, bufLastByte)
			}
			serviceNames = append(serviceNames, str)
		}
	}
	return serviceNames, nil
}

// utf16StringWithinBounds decodes a null-terminated UTF-16 string starting
// at startAddr, refusing to read past endAddr. Used instead of
// windows.UTF16PtrToString when the caller only knows the string's start
// address lies within a fixed-size buffer, not that a null terminator is
// guaranteed to appear before the buffer's end.
func utf16StringWithinBounds(strPtr *uint16, bufLastByte unsafe.Pointer) (string, bool) { // by Gemini 3.1 Pro
	if strPtr == nil || bufLastByte == nil {
		return "", false
	}

	startAddr := uintptr(unsafe.Pointer(strPtr))
	lastByteAddr := uintptr(bufLastByte)

	if startAddr > lastByteAddr {
		return "", false
	}

	// Calculate maximum allowed uint16 elements before the inclusive end
	// Formula: (LastByte - StartByte + 1) / 2
	availableBytes := (lastByteAddr - startAddr) + 1
	maxUnits := int(availableBytes / 2)

	if maxUnits <= 0 {
		return "", false
	}

	// unsafe.Slice accepts *uint16 directly
	units := unsafe.Slice(strPtr, maxUnits)

	for i, u := range units {
		if u == 0 {
			return windows.UTF16ToString(units[:i]), true
		}
	}

	return "", false // no null terminator found before buffer end
}

// func utf16StringWithinBounds4(strPtr *uint16, bufEnd unsafe.Pointer) (string, bool) { // by Gemini 3.6 Thinking
// 	if strPtr == nil || bufEnd == nil {
// 		return "", false
// 	}

// 	startAddr := uintptr(unsafe.Pointer(strPtr))
// 	endAddr := uintptr(bufEnd)

// 	if startAddr >= endAddr {
// 		return "", false
// 	}

// 	// Calculate maximum allowed uint16 elements before bufEnd
// 	maxUnits := int((endAddr - startAddr) / 2)
// 	if maxUnits <= 0 {
// 		return "", false
// 	}

// 	// unsafe.Slice accepts *uint16 directly—no unsafe.Pointer conversion needed!
// 	units := unsafe.Slice(strPtr, maxUnits)

// 	for i, u := range units {
// 		if u == 0 {
// 			return windows.UTF16ToString(units[:i]), true
// 		}
// 	}

// 	return "", false // no null terminator found before bufEnd
// }

// func utf16StringWithinBounds0(startAddr, endAddr uintptr) (string, bool) { //this was made by Claude Sonnet 5 Extra Thinking
// 	var units []uint16
// 	for addr := startAddr; addr+1 < endAddr; addr += 2 {
// 		//main_api.go:1470:19: possible misuse of unsafe.Pointer
// 		u := *(*uint16)(unsafe.Pointer(addr)) //nolint:gosec // bounds-checked by the loop condition above
// 		if u == 0 {
// 			return windows.UTF16ToString(units), true
// 		}
// 		units = append(units, u)
// 	}
// 	return "", false // no null terminator found before endAddr
// }

// func utf16StringWithinBounds1(startAddr, endAddr uintptr) (string, bool) { // by Gemini 3.6 Thinking
// 	var units []uint16
// 	basePtr := unsafe.Pointer(startAddr) //possible misuse of unsafe.Pointer

// 	for offset := uintptr(0); startAddr+offset+1 < endAddr; offset += 2 {
// 		// unsafe.Add performs single-expression pointer arithmetic
// 		u := *(*uint16)(unsafe.Add(basePtr, offset))
// 		if u == 0 {
// 			return windows.UTF16ToString(units), true
// 		}
// 		units = append(units, u)
// 	}

// 	return "", false // no null terminator found before endAddr
// }

// func utf16StringWithinBounds2(startAddr, endAddr uintptr) (string, bool) { // by Gemini 3.6 Thinking
// 	if startAddr >= endAddr {
// 		return "", false
// 	}

// 	// Calculate the max possible uint16 elements in the memory range
// 	count := (endAddr - startAddr) / 2
// 	if count == 0 {
// 		return "", false
// 	}

// 	// Create a slice referencing the memory range
// 	units := unsafe.Slice((*uint16)(unsafe.Pointer(startAddr)), count) //possible misuse of unsafe.Pointer

// 	// Search for null terminator
// 	for i, u := range units {
// 		if u == 0 {
// 			return windows.UTF16ToString(units[:i]), true
// 		}
// 	}

// 	return "", false // no null terminator found before endAddr
// }

// PidAndExeForUDP returns (pid, exePath_or_exeName, error).
// clientAddr should be the remote UDP address observed on the server side.
func PidAndExeForUDP(clientAddr *net.UDPAddr) (uint32, string, error) {
	//capital P in PidAndExeForUDP means exported, apparently!
	if clientAddr == nil {
		return 0, "", errors.New("nil clientAddr")
	}

	ip := clientAddr.IP
	if clientAddr.Port < 0 || clientAddr.Port > 65535 {
		return 0, "", fmt.Errorf("invalid network port: %d", clientAddr.Port)
	}
	port := uint16(clientAddr.Port)

	isIPv4 := ip.To4() != nil
	family := uint32(AF_INET)
	if !isIPv4 {
		family = AF_INET6
	}

	buf, err := GetExtendedUDPTable(false, family)
	if err != nil {
		return 0, "", fmt.Errorf("GetExtendedUDPTable failed while resolving pid/exe for UDP client %s: %w", clientAddr, err)
	}

	if buf == nil {
		return 0, "", errors.New("GetExtendedUdpTable returned empty buffer which means there were no UDP entries in the table")
	}

	// Buffer layout: DWORD dwNumEntries; then array of MIB_UDPROW_OWNER_PID entries.
	if len(buf) < 4 {
		return 0, "", errors.New("GetExtendedUdpTable returned too small buffer")
	}

	num := binary.LittleEndian.Uint32(buf[:4])
	offset := 4

	//for i := uint32(0); i < num; i++ {
	for i := range num {
		if isIPv4 {
			// MIB_UDPROW_OWNER_PID (12 bytes)
			const rowSize = 12 // MIB_UDPROW_OWNER_PID has 3 DWORDs = 12 bytes
			if offset+rowSize > len(buf) {
				// Defense-in-depth: reached on every incoming UDP DNS packet(in dnsbollocks), so never panic on
				// OS-returned telemetry here — mirrors PidAndExeForTCP's handling of the
				// identical situation below in this same file. A transient race between the size
				// query and the data fetch can occasionally yield a count*rowSize that doesn't
				// fit; treat it as "no more entries to scan" instead of crashing the resolver.
				GetBugLogger().Error(fmt.Sprintf("attempted to read beyond buffer in buf=%p len(buf)=%d offset=%d rowSize=%d i=%d\n", buf, len(buf), offset, rowSize, i))
				break
			}
			localAddr := binary.LittleEndian.Uint32(buf[offset : offset+4])
			localPortRaw := binary.LittleEndian.Uint32(buf[offset+4 : offset+8])
			owningPid := binary.LittleEndian.Uint32(buf[offset+8 : offset+12])
			//prepare for next entry
			offset += rowSize
			// localPortRaw stores port in network byte order in low 16 bits.
			localPort := uint16(localPortRaw & 0xFFFF)
			localPort = (localPort>>8)&0xFF | (localPort&0xFF)<<8 // convert to host order

			// convert DWORD IP (little-endian) to net.IP
			ipb := []byte{
				byte(localAddr & 0xFF),
				byte((localAddr >> 8) & 0xFF),
				byte((localAddr >> 16) & 0xFF),
				byte((localAddr >> 24) & 0xFF),
			}
			entryIP := net.IPv4(ipb[0], ipb[1], ipb[2], ipb[3])

			if localPort == port && (entryIP.Equal(net.IPv4zero) || entryIP.Equal(ip.To4())) { // treat 0.0.0.0 as wildcard match
				exe, err2 := resolveExeName(owningPid, clientAddr.String())
				if err2 != nil {
					return 0, "", err2
				}
				return owningPid, exe, nil
			}
		} else {
			// MIB_UDP6ROW_OWNER_PID (28 bytes)
			// ucLocalAddr[16], dwLocalScopeId, dwLocalPort, dwOwningPid
			const rowSize = 28
			if offset+rowSize > len(buf) {
				//See the identical comment in the isIPv4 branch above.

				//panic2(fmt.Sprintf("attempted to read beyond buffer in buf=%p len(buf)=%d offset=%d rowSize=%d i=%d\n", buf, len(buf), offset, rowSize, i))
				GetBugLogger().Error(fmt.Sprintf("attempted to read beyond buffer in buf=%p len(buf)=%d offset=%d rowSize=%d i=%d\n", buf, len(buf), offset, rowSize, i))
				break
			}

			localIPBytes := buf[offset : offset+16]
			// offset+16 to offset+20 is dwLocalScopeId (skipped)
			localPortRaw := binary.LittleEndian.Uint32(buf[offset+20 : offset+24])
			owningPid := binary.LittleEndian.Uint32(buf[offset+24 : offset+28])
			offset += rowSize

			localPort := uint16(localPortRaw & 0xFFFF)
			localPort = (localPort>>8)&0xFF | (localPort&0xFF)<<8

			entryIP := net.IP(localIPBytes)

			if localPort == port && (entryIP.Equal(net.IPv6zero) || entryIP.Equal(ip)) {
				exe, err2 := resolveExeName(owningPid, clientAddr.String())
				if err2 != nil {
					return 0, "", err2
				}
				return owningPid, exe, nil
			}
		}
	} //for

	return 0, "", fmt.Errorf("no matching UDP socket entry found for %s (ephemeral port reuse or socket already closed by kernel) thus dno who sent it", clientAddr.String())
}

// PidAndExeForTCP resolves the PID/Exe for a given client TCP connection.
// clientAddr should be the remote TCP address observed on the server side.
func PidAndExeForTCP(clientAddr *net.TCPAddr) (uint32, string, error) {
	if clientAddr == nil {
		return 0, "", errors.New("nil clientAddr")
	}

	ip := clientAddr.IP
	if clientAddr.Port < 0 || clientAddr.Port > 65535 {
		return 0, "", fmt.Errorf("invalid network port: %d", clientAddr.Port)
	}
	port := uint16(clientAddr.Port)

	isIPv4 := ip.To4() != nil
	family := uint32(AF_INET)
	if !isIPv4 {
		family = AF_INET6
	}

	// Fetch the table using the dynamic address family
	buf, err := GetExtendedTCPTable(false, family)
	if err != nil {
		return 0, "", fmt.Errorf("GetExtendedTCPTable failed while resolving pid/exe for TCP client %s: %w", clientAddr, err)
	}
	if buf == nil {
		return 0, "", errors.New("GetExtendedTcpTable returned empty buffer")
	}

	if len(buf) < 4 {
		return 0, "", errors.New("GetExtendedTcpTable buffer too small for header")
	}

	num := binary.LittleEndian.Uint32(buf[:4])
	offset := 4

	//for i := uint32(0); i < num; i++ {
	for i := range num {
		if isIPv4 {
			// MIB_TCPROW_OWNER_PID (24 bytes)
			// MIB_TCPROW_OWNER_PID structure:
			// 0: dwState (4 bytes)
			// 4: dwLocalAddr (4 bytes)
			// 8: dwLocalPort (4 bytes)
			// 12: dwRemoteAddr (4 bytes)
			// 16: dwRemotePort (4 bytes)
			// 20: dwOwningPid (4 bytes)
			const rowSize = 24
			if offset+rowSize > len(buf) {
				GetBugLogger().Error(fmt.Sprintf("attempted to read beyond buffer in buf=%p len(buf)=%d offset=%d rowSize=%d i=%d\n", buf, len(buf), offset, rowSize, i))
				break
			}

			// Extract fields based on the 24-byte MIB_TCPROW_OWNER_PID layout
			localAddrRaw := binary.LittleEndian.Uint32(buf[offset+4 : offset+8])
			localPortRaw := binary.LittleEndian.Uint32(buf[offset+8 : offset+12])
			owningPid := binary.LittleEndian.Uint32(buf[offset+20 : offset+24])
			// Advance offset for next iteration
			offset += rowSize

			// Port conversion (Network Byte Order in low 16 bits)
			localPort := uint16(localPortRaw & 0xFFFF)
			localPort = (localPort>>8)&0xFF | (localPort&0xFF)<<8

			if localPort == port {
				// Convert DWORD IP (little-endian) to net.IP
				entryIP := net.IPv4(
					byte(localAddrRaw&0xFF),
					byte((localAddrRaw>>8)&0xFF),
					byte((localAddrRaw>>16)&0xFF),
					byte((localAddrRaw>>24)&0xFF),
				)
				// Match logic (Wildcard 0.0.0.0 or specific IP)
				if entryIP.Equal(net.IPv4zero) || entryIP.Equal(ip.To4()) {
					exe, err2 := resolveExeName(owningPid, clientAddr.String())
					if err2 != nil {
						return 0, "", err2
					}
					return owningPid, exe, nil
				}
			}
		} else {
			// MIB_TCP6ROW_OWNER_PID (56 bytes)
			// ucLocalAddr[16], dwLocalScopeId, dwLocalPort, ucRemoteAddr[16], dwRemoteScopeId, dwRemotePort, dwState, dwOwningPid
			const rowSize = 56
			if offset+rowSize > len(buf) {
				GetBugLogger().Error(fmt.Sprintf("attempted to read beyond buffer in buf=%p len(buf)=%d offset=%d rowSize=%d i=%d\n", buf, len(buf), offset, rowSize, i))
				break
			}

			localIPBytes := buf[offset : offset+16]
			// offset+16 to offset+20 is dwLocalScopeId (skipped)
			localPortRaw := binary.LittleEndian.Uint32(buf[offset+20 : offset+24])
			// offset+24 to offset+52 contains remote info and state (skipped)
			owningPid := binary.LittleEndian.Uint32(buf[offset+52 : offset+56])
			offset += rowSize

			localPort := uint16(localPortRaw & 0xFFFF)
			localPort = (localPort>>8)&0xFF | (localPort&0xFF)<<8

			if localPort == port {
				entryIP := net.IP(localIPBytes)

				if entryIP.Equal(net.IPv6zero) || entryIP.Equal(ip) {
					exe, err2 := resolveExeName(owningPid, clientAddr.String())
					if err2 != nil {
						return 0, "", err2
					}
					return owningPid, exe, nil
				}
			}
		}
	}

	return 0, "", fmt.Errorf("no TCP owner found for %s", clientAddr.String())
}

// serviceNameCache caches PID→service-names with a short TTL to avoid
// hammering EnumServicesStatusEx on every packet under high concurrency.
// This also eliminates the concurrent unsafe-buffer pressure that caused
// the STATUS_ACCESS_VIOLATION crash under -race load. No, the cause was this: https://github.com/golang/go/issues/77975
type serviceCacheEntry struct {
	names     []string
	expiresAt time.Time
}

var (
	serviceCache    = make(map[uint32]serviceCacheEntry)
	serviceCacheMu  sync.Mutex
	serviceCacheTTL = 60 * time.Second
)

// serviceLookupInFlight coordinates a single in-progress
// GetServiceNamesFromPIDUncached call for a given PID, so concurrent cache
// misses for the SAME PID (e.g. a burst of UDP/TCP packets from one process
// arriving faster than the 60s cache TTL) coalesce into one SCM enumeration
// instead of a thundering herd of duplicate EnumServicesStatusEx calls.
//
// done is closed exactly once, by the single "leader" goroutine that
// actually performed the lookup, only after it has written names/err.
// Closing a channel happens-after any writes made before the close and
// happens-before any receive of that close completes, so waiters reading
// names/err after <-done observe a fully-published result with no
// additional synchronization needed.
type serviceLookupInFlight struct {
	done  chan struct{}
	names []string
	err   error
}

var (
	serviceInFlightMu sync.Mutex
	serviceInFlight   = make(map[uint32]*serviceLookupInFlight)
)

func GetServiceNamesFromPIDCached(targetPID uint32) ([]string, error) {
	// Fast path: check cache under lock.
	serviceCacheMu.Lock()
	if entry, ok := serviceCache[targetPID]; ok && time.Now().Before(entry.expiresAt) {
		names := entry.names
		serviceCacheMu.Unlock()
		return names, nil
	}
	serviceCacheMu.Unlock()

	// Slow path: coalesce concurrent cache misses for this PID into a
	// single SCM enumeration. Note there's a narrow, benign window right
	// here between the unlock above and the lock below: another goroutine
	// could finish an in-flight lookup and populate the cache in that exact
	// gap, in which case we'll redundantly join or start a lookup instead of
	// noticing the fresh cache entry. This is the same class of accepted
	// tradeoff already documented at the end of this function (the
	// delete-then-close(inFlight.done) race on the failure path) -- rare,
	// benign, and categorically better than adding a lock that spans both
	// maps just to close it.
	serviceInFlightMu.Lock()
	if inFlight, ok := serviceInFlight[targetPID]; ok {
		// Someone else is already fetching this PID; wait for their result
		// instead of starting a duplicate, expensive enumeration ourselves.
		serviceInFlightMu.Unlock()
		<-inFlight.done
		return inFlight.names, inFlight.err
	}
	inFlight := &serviceLookupInFlight{done: make(chan struct{})}
	serviceInFlight[targetPID] = inFlight
	serviceInFlightMu.Unlock()

	names, err := GetServiceNamesFromPIDUncached(targetPID)
	inFlight.names = names
	inFlight.err = err

	if err == nil {
		serviceCacheMu.Lock()
		serviceCache[targetPID] = serviceCacheEntry{
			names:     names,
			expiresAt: time.Now().Add(serviceCacheTTL),
		}
		serviceCacheMu.Unlock()
	}

	// Remove ourselves from the in-flight map before broadcasting, so that
	// as soon as waiters wake up, a fresh caller either sees the (already
	// updated, on success) cache entry or is free to start a brand-new
	// lookup rather than incorrectly rejoining a completed one.
	//
	// NOTE: there's a narrow, benign race on the failure path (err != nil,
	// so the cache is intentionally NOT updated): a caller arriving between
	// the delete below and the close(inFlight.done) broadcast finds neither
	// a fresh cache entry nor an in-flight lookup, and starts its own
	// redundant GetServiceNamesFromPIDUncached call. That's an acceptable,
	// rare duplicate call — the same trade-off any singleflight-style
	// "forget" step makes — and is categorically better than every
	// concurrent caller stampeding the SCM on every miss.
	serviceInFlightMu.Lock()
	delete(serviceInFlight, targetPID)
	serviceInFlightMu.Unlock()

	close(inFlight.done)

	return names, err
}

func GoRoutineId() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// "goroutine 17 [running]:\n..."
	var id int64 = -1
	_, err := fmt.Sscanf(string(buf[:n]), "goroutine %d", &id)
	if err != nil {
		GetBugLogger().Warn("BUG: unexpected fail from fmt.Sscanf", SafeErr(err))
	}
	return id
}

// InjectConsoleEnter synthesizes a dummy Carriage Return (Enter) key event
// and writes it directly into the system's console input buffer.
// This safely unblocks threads stuck on synchronous reads like term.ReadPassword.
// InjectConsoleEnter sends a synthetic Carriage Return (Enter) to the console stream
func InjectConsoleEnter() error {
	return InjectConsoleKey(VK_RETURN, 0x1C, '\r')
}

// InjectConsoleKey synthesizes a single virtual key down event
// and writes it directly into the system's console input buffer.
func InjectConsoleKey(vkCode, scanCode uint16, char rune) error {
	h := syscall.Handle(os.Stdin.Fd())

	// Validate that the rune fits into a single UTF-16 code unit (Basic Multilingual Plane)
	if char < 0 || char > 65535 {
		return fmt.Errorf("character %U cannot fit into a single uint16 code unit", char)
	}

	var rec inputRecord
	rec.EventType = KEY_EVENT

	ke := (*keyEventRecord)(unsafe.Pointer(&rec.Event[0]))
	ke.BKeyDown = 1 // Key Down
	ke.RepeatCount = 1
	ke.VirtualKeyCode = vkCode
	ke.VirtualScanCode = scanCode
	ke.UnicodeChar = uint16(char) // Safe now, gosec will be happy
	ke.ControlKeyState = 0

	var written uint32

	// Execute via your custom BoundProc architecture wrapper
	// WARNING: We must do the uintptr casting explicitly right here inside
	// the arguments list to comply with //go:uintptrescapes memory pinning safety bounds.
	res1 := procWriteConsoleInputW.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&rec)),
		uintptr(1),
		uintptr(unsafe.Pointer(&written)),
	)
	if res1.Failed() || written != 1 {
		return fmt.Errorf("InjectConsoleKey failed, written %d, err: %w", written, res1.Err)
	}

	return nil
}

var (
	procReplaceFileW = NewBoundProc6(Kernel32, "ReplaceFileW", CheckBool)
)

// Add this to your wincoe bindings / main_api.go
const (
	REPLACEFILE_WRITE_THROUGH       = 0x00000001
	REPLACEFILE_IGNORE_MERGE_ERRORS = 0x00000002
	REPLACEFILE_IGNORE_ACL_ERRORS   = 0x00000004
)

// ReplaceFile atomically replaces 'replaced' with 'replacement', creating an optional backup.
func ReplaceFile(replaced, replacement, backup string, flags uint32) error {
	replacedPtr, err := windows.UTF16PtrFromString(replaced)
	if err != nil {
		return fmt.Errorf("convert replaced path %q to UTF-16: %w", replaced, err)
	}
	replacementPtr, err := windows.UTF16PtrFromString(replacement)
	if err != nil {
		return fmt.Errorf("convert replacement path %q to UTF-16: %w", replacement, err)
	}

	var backupPtr *uint16
	if backup != "" {
		backupPtr, err = windows.UTF16PtrFromString(backup)
		if err != nil {
			return fmt.Errorf("convert backup path %q to UTF-16: %w", backup, err)
		}
	}

	// Utilizing your existing //go:uintptrescapes architecture
	res1 := procReplaceFileW.Call(
		uintptr(unsafe.Pointer(replacedPtr)),
		uintptr(unsafe.Pointer(replacementPtr)),
		uintptr(unsafe.Pointer(backupPtr)),
		uintptr(flags),
		0,
		0,
	)
	return res1.Err
}

// fileWriteLocks serializes concurrent writers targeting the SAME file path
// (keyed by its cleaned, lowercased form — NTFS path comparison is
// case-insensitive) while letting writes to DIFFERENT files (e.g.
// config.json vs query_whitelist.json vs hosts2ip.json) proceed fully in
// parallel instead of serializing through one FileWriter-instance-wide
// mutex. Entries are never removed: the set of distinct file paths this
// process ever writes to is small and fixed by construction.
var fileWriteLocks sync.Map // map[string]*sync.Mutex

// lockFileForWrite acquires (creating on first use) the per-filename mutex
// for filename and returns a function that releases it. Callers should
// `defer` the returned function immediately.
func lockFileForWrite(filename string) func() {
	key := strings.ToLower(filepath.Clean(filename))
	muIface, _ := fileWriteLocks.LoadOrStore(key, &sync.Mutex{})
	mu, ok := muIface.(*sync.Mutex)
	if !ok {
		panic2("BUG: fileWriteLocks contained a non-*sync.Mutex value")
	}
	mu.Lock()
	return mu.Unlock
}

// FileWriter is the persistence contract.

// FileWriter is the persistence contract.
// Extracted from Server so saves can be intercepted in tests without
// touching the filesystem, and so fileWriteMu is an implementation detail
// rather than a Server concern.
type FileWriter interface {
	SafeWriteFile(filename string, data []byte, perm os.FileMode) error
	CheckPowerLossFile(filename string)
	SetExtraSafety(enabled bool)
	SetRetryParams(maxRetries, retryBackoffMs int)
}

// GenericSafeFileWriter is the production FileWriter.
// Writes to the SAME file path are serialised via fileWriteLocks (replacing
// the old single instance-wide Server.fileWriteMu); writes to DIFFERENT file
// paths proceed fully in parallel. It conditionally uses a staging file when
// cfg.ExtraSafety is true.
// cfg is a pointer to Server.config so ExtraSafety is always read at call time.
type GenericSafeFileWriter struct {
	// paramsMu guards extraSafety/maxRetries/retryBackoffMs only. Actual file
	// writes are serialized per-filename via fileWriteLocks instead of this
	// mutex, so concurrent writes to different files never block each other.
	paramsMu       sync.Mutex
	extraSafety    bool
	maxRetries     int
	retryBackoffMs int
	liveLogger     *atomic.Pointer[slog.Logger]
}

func NewGenericSafeFileWriter(extraSafety bool, maxRetries, retryBackoffMs int, liveLogger *atomic.Pointer[slog.Logger]) FileWriter {
	return &GenericSafeFileWriter{
		extraSafety:    extraSafety,
		maxRetries:     maxRetries,
		retryBackoffMs: retryBackoffMs,
		liveLogger:     liveLogger,
	}
}

func (fw *GenericSafeFileWriter) getLogger() *slog.Logger {
	return GetLoggerOrFallback(fw.liveLogger, "GenericSafeFileWriter.liveLogger")
}

func (fw *GenericSafeFileWriter) SetExtraSafety(enabled bool) {
	fw.paramsMu.Lock()
	defer fw.paramsMu.Unlock()
	fw.extraSafety = enabled
}

// CheckPowerLossFile implements FileWriter.
// Panics if a non-empty staging file exists for filename, signalling a
// mid-write crash on a previous run.
// old:
// checkPowerLossFile inspects the file system for a lingering commit file.
// If found, it halts execution to prevent the application from overwriting
// or loading potentially corrupted state.
func (fw *GenericSafeFileWriter) CheckPowerLossFile(filename string) {
	if filename == "" {
		return
	}
	log := fw.getLogger()

	tmpName := filename + PowerlossFileExtension
	fi, err := OsStatFunc(tmpName)
	if err != nil {
		// File doesn't exist (or is completely inaccessible), safe to proceed
		return
	}
	// -> THE FIX: If the file is 0 bytes, cleanup failed on a previous successful run.

	if fi.Size() == 0 {
		log.Warn("ExtraSafety: Found an empty power-loss staging file. Previous write succeeded, "+
			"but the temporary file could not be deleted (likely due to directory permissions).",
			slog.String("tempfilename", tmpName))
		return
	}
	logmsg := fmt.Sprintf(
		"\n========================================================================\n"+
			"CRITICAL SAFETY PANIC: Power loss or crash detected!\n"+
			"The safety file %q exists and contains uncommitted data (%d bytes).\n\n"+
			"This indicates the server aborted mid-write while updating %q.\n"+
			"The main file may be corrupted, truncated, or empty (0 bytes).\n\n"+
			"ACTION REQUIRED:\n"+
			"1. Manually inspect both files.\n"+
			"2. The %s file contains your last valid saved data.\n"+
			"3. Restore the data to the main file, then DELETE the %s file.\n"+
			"========================================================================\n",
		tmpName, fi.Size(), filename,
		PowerlossFileExtension, PowerlossFileExtension,
	)
	logCriticalThenPanic(log, logmsg) //FIXME: ? the errors/args are embedded in the msg
	panic(nil)
}

// SafeWriteFile attempts a crash-safe file update without using os.Rename,
// preserving Windows ACLs and falling back gracefully if directory permissions
// block the creation of temporary files.
// Writes to the SAME file path are serialised via fileWriteLocks (replacing
// the old Server.fileWriteMu); writes to DIFFERENT file paths proceed fully
// in parallel instead of contending on one instance-wide mutex.
//
// By writing the complete payload to [filename].powergotlost first, flushing it
// to hardware, and only then truncating the target file, you create a cryptographic-like commit phase.
//
// old:
// SafeWriteFile implements FileWriter.
// All writes are serialised through fw.mu (replacing the old Server.fileWriteMu).
// When cfg.ExtraSafety is true, data is first written to a staging file
// (filename + ".powergotlost") so a power-loss mid-write is detectable on
// the next boot via CheckPowerLossFile.
// older:
// SafeWriteFile attempts a crash-safe file update without using os.Rename,
// preserving Windows ACLs and falling back gracefully if directory permissions
// block the creation of temporary files.
//
// By writing the complete payload to [filename].powergotlost first, flushing it to hardware, and only then truncating the target file, you create a cryptographic-like commit phase.
func (fw *GenericSafeFileWriter) SafeWriteFile(filename string, data []byte, perm os.FileMode) error {
	log := fw.getLogger()

	fw.paramsMu.Lock()
	maxAttempts := 1 + fw.maxRetries
	backoffDuration := time.Duration(fw.retryBackoffMs) * time.Millisecond
	extraSafety := fw.extraSafety
	fw.paramsMu.Unlock()

	// Serialize actual disk I/O per-filename rather than per-instance, so a
	// slow write to one file never blocks an unrelated write to a different
	// file — see fileWriteLocks's doc comment.
	unlockFile := lockFileForWrite(filename)
	defer unlockFile()

	if extraSafety {
		tmpName := filename + PowerlossFileExtension

		// step1. Try to write to a temp file first to ensure disk space and data integrity.
		stagingErr := writeStagingFileWithRetry(tmpName, data, perm, maxAttempts, backoffDuration)

		if stagingErr == nil {
			// --- SUCCESS BRANCH ---
			// Temp file is safely on disk. Overwrite the target file directly
			// so we don't alter its existing Windows permissions/ACLs.
			log.Debug("ExtraSafety: Staged recovery file on disk", slog.String("tempfilename", tmpName))

			// Queue cleanup. If we crash/lose power after this point,
			// this defer never runs, leaving the safe copy intact.
			defer neutralizeOrPanic(
				tmpName, perm, maxAttempts, backoffDuration, log, nil,
				"Staging file cleanup failed completely!",
				"Because the file contains non-zero bytes, the next server boot will panic.",
			)
		} else {
			// --- FAILURE BRANCH ---
			log.Warn("ExtraSafety: Can't create temp staging file before writing the actual file (lacking directory write permissions?), using fallback which means if power-loss occurs in a very tiny window here then the file is lost", SafeErr(stagingErr))

			// FIX FOR THE ELSE BRANCH: The staging write itself failed or was cut short.
			// Attempt deletion or force a truncation down to 0 bytes to neutralize any partial garbage data.
			neutralizeOrPanic(
				tmpName, perm, maxAttempts, backoffDuration, log, stagingErr,
				"Failed staging write left un-neutralized garbage bytes!",
				"",
			)
		}
	}

	// 2. Fallback: If we couldn't create the .tmp file (likely folder permissions),
	// do a direct write but enforce a hardware sync to minimize the corruption window.
	// step2. Overwrite the target file directly (Retains Windows ACLs)
	if err := RetryFileOp(maxAttempts, backoffDuration, func() error {
		return WriteSyncedFile(filename, data, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	}); err == nil {
		return nil
	} else {
		return fmt.Errorf("failed to open/write/sync/close the file %q, err: %w", filename, err)
	}
}

// RetryFileOp attempts fn up to maxAttempts times with a short backoff
// between attempts, to absorb transient Windows file locks (Defender,
// Search Indexer, backup agents) that typically release within milliseconds.
// Returns the last error if every attempt fails.
func RetryFileOp(maxAttempts int, backoff time.Duration, fn func() error) error {
	if maxAttempts < 1 {
		panic2("BUG: dev fail: retryFileOp called with maxAttempts < 1")
	}
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		if lastErr = fn(); lastErr == nil {
			//succeeded
			return nil
		}
		if i < maxAttempts-1 {
			time.Sleep(backoff)
		}
	}
	//failed
	return lastErr
}

// TruncateStagingFileToZero opens the staging file with O_TRUNC (destroying
// its contents in-place, no delete/rename required), syncs the 0-byte state
// to disk, and closes it. This is the fallback path when the staging file
// can't be deleted outright but must not be left containing non-zero bytes,
// since CheckPowerLossFile treats any non-empty staging file as evidence of
// a crash mid-write on the next boot.
func TruncateStagingFileToZero(tmpName string, perm os.FileMode) error {
	truncFile, openErr := os.OpenFile(tmpName, os.O_WRONLY|os.O_TRUNC, perm)
	if openErr != nil {
		return fmt.Errorf("open for truncate failed: %w", openErr)
	}

	syncErr := truncFile.Sync() // Ensure the 0-byte state hits disk
	closeErr := truncFile.Close()

	if syncErr != nil && closeErr != nil {
		return fmt.Errorf("sync after truncate failed: %w (close also failed: %w)", syncErr, closeErr)
	}
	if syncErr != nil {
		return fmt.Errorf("sync after truncate failed: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close after truncate+sync failed: %w", closeErr)
	}

	return nil
}

// WriteSyncedFile opens filename with the given flags, writes data, syncs,
// and closes as a single retryable unit. A mid-write failure from a
// transient Windows file lock is exactly as retryable as an open failure,
// so this covers both together rather than only guarding OpenFile.
func WriteSyncedFile(filename string, data []byte, flags int, perm os.FileMode) error {
	f, err := os.OpenFile(filename, flags, perm)
	if err != nil {
		return fmt.Errorf("open failed: %w", err)
	}
	n, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()

	if writeErr != nil {
		return fmt.Errorf("write failed after %d/%d bytes: %w", n, len(data), writeErr)
	}
	if syncErr != nil {
		return fmt.Errorf("sync failed: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close failed: %w", closeErr)
	}
	return nil
}

// SafeErr converts an error to a primitive string attribute safely.
// If the error is nil, it gracefully logs it as "<nil>" without panicking.
func SafeErr(err error) slog.Attr {
	return SafeErr2("err", err)
}

// SafeErr2 converts an error to a primitive string attribute safely.
// If the error is nil, it gracefully logs it as "<nil>" without panicking.
func SafeErr2(msg string, err error) slog.Attr {
	if err == nil {
		return slog.String(msg, "<nil>")
	}
	return slog.String(msg, err.Error())
}

// PowerlossFileExtension any saved file with this extension means power-loss (or panic in code?) occurred in a very tiny window and thus this is your potentially safe config and should be manually investigated for restoration purposes esp. if the main file is 0 bytes.
const PowerlossFileExtension string = ".powergotlost"
const BackupFileExtension string = ".bak"

// win11SafeFileWriter is the production FileWriter for Windows.
// Writes to the SAME file path are serialised via fileWriteLocks; writes to
// DIFFERENT file paths proceed fully in parallel. It always attempts a
// transactional swap via ReplaceFileW to gain atomic updates and automated backups.
// If the Win32 transaction is blocked by directory/file permissions or ACL limits,
// it gracefully falls back to an in-place truncate write.
type win11SafeFileWriter struct {
	// paramsMu guards maxRetries/retryBackoffMs (and extraSafety, though it's
	// currently unread by SafeWriteFile) only. Actual file writes are
	// serialized per-filename via fileWriteLocks instead of this mutex.
	paramsMu       sync.Mutex
	extraSafety    bool // Kept for interface alignment; always runs staging on Windows
	maxRetries     int
	retryBackoffMs int
	liveLogger     *atomic.Pointer[slog.Logger]
}

func NewWin11SafeFileWriter(extraSafety bool, maxRetries, retryBackoffMs int, liveLogger *atomic.Pointer[slog.Logger]) FileWriter {
	return &win11SafeFileWriter{
		extraSafety:    extraSafety,
		maxRetries:     maxRetries,
		retryBackoffMs: retryBackoffMs,
		liveLogger:     liveLogger,
	}
}

func (fw *win11SafeFileWriter) getLogger() *slog.Logger {
	return GetLoggerOrFallback(fw.liveLogger, "win11SafeFileWriter.liveLogger")
}

func (fw *win11SafeFileWriter) SetExtraSafety(enabled bool) {
	fw.paramsMu.Lock()
	defer fw.paramsMu.Unlock()
	fw.extraSafety = enabled
}

// CheckPowerLossFile implements FileWriter.
// Inspects the filesystem for a lingering, non-empty staging file. If detected,
// it means a prior run crashed mid-transaction, and it halts to protect system state.
func (fw *win11SafeFileWriter) CheckPowerLossFile(filename string) {
	if filename == "" {
		return
	}
	log := fw.getLogger()

	tmpName := filename + PowerlossFileExtension
	fi, err := OsStatFunc(tmpName)
	if err != nil {
		// File doesn't exist or is completely inaccessible, safe to proceed
		return
	}

	// If the file is 0 bytes, a previous cleanup attempt dropped to truncation but
	// couldn't erase the file record. This is safe to bypass.
	if fi.Size() == 0 {
		log.Warn("Windows FileWriter: Found an empty power-loss staging file. Previous write succeeded, but the temporary file record could not be unlinked.",
			slog.String("tempfilename", tmpName))
		return
	}

	logmsg := fmt.Sprintf(
		"\n========================================================================\n"+
			"CRITICAL SAFETY PANIC: Power loss or crash detected!\n"+
			"The safety file %q exists and contains uncommitted data (%d bytes).\n\n"+
			"This indicates the server aborted mid-write while updating %q.\n"+
			"The main file may be corrupted, truncated, or empty (0 bytes).\n\n"+
			"ACTION REQUIRED:\n"+
			"1. Manually inspect both files.\n"+
			"2. The %s file contains your last valid saved data.\n"+
			"3. Restore the data to the main file, then DELETE the %s file.\n"+
			"========================================================================\n",
		tmpName, fi.Size(), filename,
		PowerlossFileExtension, PowerlossFileExtension,
	)
	logCriticalThenPanic(log, logmsg)
	panic(nil)
}

// SafeWriteFile implements FileWriter.
// Always attempts to write a staging file and run an atomic ReplaceFileW swap.
// If ReplaceFileW aborts (e.g. because it cannot copy the original file's ACLs
// due to lacking WRITE_DAC permissions), the staging file is erased and the write
// falls back to a direct in-place truncation, preserving target security configurations.
func (fw *win11SafeFileWriter) SafeWriteFile(filename string, data []byte, perm os.FileMode) error {
	log := fw.getLogger()

	fw.paramsMu.Lock()
	maxAttempts := 1 + fw.maxRetries
	backoffDuration := time.Duration(fw.retryBackoffMs) * time.Millisecond
	fw.paramsMu.Unlock()

	// Serialize actual disk I/O per-filename rather than per-instance, so a
	// slow write to one file never blocks an unrelated write to a different
	// file — see fileWriteLocks's doc comment.
	unlockFile := lockFileForWrite(filename)
	defer unlockFile()

	tmpName := filename + PowerlossFileExtension
	backupName := filename + BackupFileExtension

	// Step 1: Always try to write and flush the staging payload to disk first
	stagingErr := writeStagingFileWithRetry(tmpName, data, perm, maxAttempts, backoffDuration)
	stagingSuccess := stagingErr == nil

	if stagingSuccess {
		log.Debug("Windows FileWriter: Staged recovery file written and flushed successfully", slog.String("tempfilename", tmpName))
	} else {
		log.Warn("Windows FileWriter: Directory ACLs or disk issues blocked staging file creation; skipping to in-place fallback", SafeErr(stagingErr))
	}

	// Step 2: Attempt native atomic Win32 transaction
	if stagingSuccess {
		// ReplaceFileW requires the target destination to exist. If this is a first-boot
		// scenario and the target is missing, we bypass ReplaceFileW and perform a clean rename.
		if _, statErr := OsStatFunc(filename); os.IsNotExist(statErr) {
			log.Info("Windows FileWriter: Destination file does not exist, committing via initial rename", slog.String("path", filename))

			renameErr := OsRenameFunc(tmpName, filename)
			if renameErr == nil {
				return nil // Done with first-boot save
			}

			log.Warn("Windows FileWriter: Staging file rename failed; clearing and using truncate fallback", SafeErr(renameErr))
			neutralizeOrPanic(
				tmpName, perm, maxAttempts, backoffDuration, log, renameErr,
				"Staging file rename failed and cleanup failed completely!",
				"",
			)
		} else {
			// We intentionally omit REPLACEFILE_IGNORE_ACL_ERRORS. If Windows can't
			// guarantee full ACL preservation, we WANT ReplaceFileW to fail so that
			// we drop down to truncation instead of altering file permissions.
			flags := uint32(REPLACEFILE_IGNORE_MERGE_ERRORS)

			replaceErr := ReplaceFileFunc(filename, tmpName, backupName, flags)
			if replaceErr == nil {
				// Win32 documentation states the replacement staging file is automatically unlinked.
				// Run a defensive validation check to guarantee it was eliminated.
				if _, statErr := OsStatFunc(tmpName); statErr == nil {
					log.Error("BUG: ReplaceFileW reported success but staging file still exists on disk(tho it's possible something else created it this fast). Force removing.", slog.String("filename", tmpName))
					if removeErr := OsRemoveFunc(tmpName); removeErr != nil {
						log.Error("BUG: failed to remove the staging file that somehow ReplaceFileW still left on disk just now. Continuing anyway.", SafeErr(removeErr), slog.String("filename", tmpName))
					}
				}
				log.Debug("Windows FileWriter: file backed up and replaced atomically",
					slog.String("existing_file", filename),
					slog.String("backup_file", backupName),
				)
				return nil // Success! Transaction fully committed and rolled to .bak
			}

			// ReplaceFileW transaction wholly aborted (e.g., locked backup file, or no WRITE_DAC right)
			log.Warn("Windows FileWriter: ReplaceFileW transaction aborted; clearing staging file and falling back", SafeErr(replaceErr))

			// Resilience cleanup: neutralize the abandoned staging file right now so it doesn't cause a false reboot panic
			neutralizeOrPanic(
				tmpName, perm, maxAttempts, backoffDuration, log, replaceErr,
				"ReplaceFileW failed and staging file cannot be neutralized!",
				"",
			)
		}
	}

	// Step 3: FALLBACK PHASE — In-place Truncation Write
	// Truncating modifies file blocks directly on the underlying MFT record.
	// This ensures the file's original explicit ACL security context is completely untouched.
	log.Info("Windows FileWriter: Executing in-place truncation fallback write to preserve existing file ACLs", slog.String("path", filename))

	if err := RetryFileOp(maxAttempts, backoffDuration, func() error {
		return WriteSyncedFile(filename, data, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	}); err == nil {
		return nil
	} else {
		return fmt.Errorf("windows safe file writer completely failed: fallback open/write/sync/close on %q failed: %w", filename, err)
	}
}

func (fw *GenericSafeFileWriter) SetRetryParams(maxRetries, retryBackoffMs int) {
	fw.paramsMu.Lock()
	defer fw.paramsMu.Unlock()
	fw.maxRetries = maxRetries
	fw.retryBackoffMs = retryBackoffMs
}

func (fw *win11SafeFileWriter) SetRetryParams(maxRetries, retryBackoffMs int) {
	fw.paramsMu.Lock()
	defer fw.paramsMu.Unlock()
	fw.maxRetries = maxRetries
	fw.retryBackoffMs = retryBackoffMs
}

// Hooks for testing. In production, these point to the standard OS/wincoe functions.
var (
	OsStatFunc      = os.Stat
	OsRenameFunc    = os.Rename
	OsRemoveFunc    = os.Remove
	ReplaceFileFunc = ReplaceFile
)

// func closeHandleLogged(h windows.Handle, context string) {
// 	if err := windows.CloseHandle(h); err != nil {
// 		//Logged shouldn't ever be nil here due to init()
// 		Logger.Load().Debug("CloseHandle failed.")
// 	}
// }

// CloseHandleLogged closes *h (if non-zero), zeroing *h immediately
// beforehand so no caller can ever observe or reuse a handle value that's
// already been handed to CloseHandle -- whether or not the close itself
// succeeds. Logs (via logf) if CloseHandle fails, but never returns an
// error: callers already treat handle cleanup as best-effort throughout
// this codebase.
//
// Taking *windows.Handle rather than windows.Handle closes the
// TOCTOU-style window the previous value-taking signature left open: a
// caller's own handle variable could still be read (and potentially
// reused, e.g. in another defer further up the same function, or on some
// later error path) after this function had already closed it, since the
// caller's copy was never told the handle was gone.
//
// A nil h or a zero *h is treated as "nothing to close" and is a silent
// no-op.
func CloseHandleLogged(h *windows.Handle, context string) {
	if h == nil || *h == 0 {
		return
	}
	saved := *h
	*h = 0 // zero first -- see doc comment above.
	if err := windows.CloseHandle(saved); err != nil {
		getLogger().Debug("CloseHandle failed.",
			//"context", context, "err", err, //XXX: yeah this works, doesn't need slog.String("context", context) and the other for err! but I'm not gonna use this pattern!
			slog.String("context", context),
			SafeErr(err),
		)
	}
}

// writeStagingFileWithRetry handles the initial attempt to write to a temp file first
// to ensure disk space and data integrity before touching the main file.
func writeStagingFileWithRetry(tmpName string, data []byte, perm os.FileMode, maxAttempts int, backoff time.Duration) error {
	return RetryFileOp(maxAttempts, backoff, func() error {
		tmpFile, err := openStagingFileSafely(tmpName, perm)
		if err != nil {
			return fmt.Errorf("create failed: %w", err)
		}
		_, writeErr := tmpFile.Write(data)
		syncErr := tmpFile.Sync()
		closeErr := tmpFile.Close()

		if writeErr != nil || syncErr != nil || closeErr != nil {
			return fmt.Errorf("write/sync/close failed (write=%w sync=%w close=%w)", writeErr, syncErr, closeErr)
		}
		return nil
	})
}

// openStagingFileSafely opens (creating if needed) the staging file used for
// crash-safe atomic writes, refusing to blindly follow whatever might
// already exist at that path. Because CheckPowerLossFile already runs at
// process boot and panics if a NON-EMPTY staging file is found there, the
// only pre-existing staging file this should ever encounter mid-run is
// either (a) a benign leftover — a previous write succeeded but its own
// cleanup couldn't unlink the now-empty (0-byte) staging file — or (b)
// something planted by a lower-privileged local attacker: a symlink or a
// hard link aliasing this name onto a file the attacker cannot write to
// directly, hoping this process (running with equal or higher privilege)
// will overwrite the real target through the alias. Opening with O_EXCL
// (fails outright if anything already exists at tmpName) forces that
// distinction to be made explicitly instead of silently following whatever
// is already there via O_TRUNC.
func openStagingFileSafely(tmpName string, perm os.FileMode) (*os.File, error) {
	// 1. First attempt: atomic exclusive creation.
	f, err := os.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err == nil {
		return f, nil
	}
	if !os.IsExist(err) {
		return nil, fmt.Errorf("openStagingFileSafely says that os.OpenFile failed(and not because it doesn't exist) to create %q staging file, with err:%w", tmpName, err) // some other failure (permissions, path issues, etc.) — propagate as-is
	}

	// 2. Something already exists at tmpName. Inspect it before taking any action.
	// Only ever safe to reclaim it if it is a plain, empty, regular file with a single hard link.

	// Something already exists at tmpName. Only ever safe to reclaim it if
	// it is a plain, empty, regular file with a single hard link — exactly
	// what CheckPowerLossFile's own "empty staging file" tolerance
	// describes. Refuse anything else outright rather than following it.
	safe, reason, inspectErr := inspectExistingStagingFile(tmpName)
	if inspectErr != nil {
		return nil, fmt.Errorf("staging file %q exists but could not be safely inspected: %w", tmpName, inspectErr)
	}
	if !safe {
		return nil, fmt.Errorf("refusing to use staging file %q: %s (possible attack or genuine corruption)", tmpName, reason)
	}

	// // Confirmed benign: remove the empty leftover and retry the exclusive
	// // create so we never write through a pre-existing name.
	// if rmErr := OsRemoveFunc(tmpName); rmErr != nil {
	// 	return nil, fmt.Errorf("failed to remove confirmed-benign empty staging file %q before recreating it: %w", tmpName, rmErr)
	// }
	// if fil, ofErr := os.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm); ofErr != nil {
	// 	return nil, fmt.Errorf("failed to create staging file %q after removed the existing file (so, directory permissions doesn't allow creating new files? and we shoulda just truncated the existing one to keep ACLs?!), err: %w", tmpName, ofErr)
	// } else {
	// 	return fil, nil
	// }

	// 3. Confirmed benign: reuse the existing empty file directly by opening with O_TRUNC.
	// We DO NOT delete and recreate it (via OsRemoveFunc + O_EXCL) because:
	//   a) Deleting throws away existing file-level ACLs.
	//   b) Deleting requires directory-level file creation permissions, which might be restricted.
	//   c) Re-opening directly avoids an unnecessary delete-recreate window.
	// 3. Re-assigning 'f' here overwrites a nil pointer, not an open handle.
	f, err = os.OpenFile(tmpName, os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return nil, fmt.Errorf("failed to open confirmed-benign staging file %q for writing: %w", tmpName, err)
	}

	// 4. Post-open safety check: verify the handle itself (closing the tiny TOCTOU race window
	// between inspectExistingStagingFile and os.OpenFile).
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("failed to stat open staging file handle %q: %w", tmpName, err)
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("staging file handle %q is not a regular file after open (mode: %v)", tmpName, fi.Mode())
	}

	return f, nil
}

// inspectExistingStagingFile opens path WITHOUT following any reparse point
// (symlink/junction) that might be there, and reports whether it is safe to
// reclaim as a benign, previously-written-but-not-yet-cleaned-up staging
// file: specifically, a plain regular file, zero bytes long, with exactly
// one hard link (itself).
//
// FILE_FLAG_OPEN_REPARSE_POINT is essential here: without it, CreateFile
// transparently follows a symlink/junction planted at path and reports
// information about whatever it points AT instead of the link itself, which
// would make this whole check trivially bypassable by an attacker.
func inspectExistingStagingFile(path string) (safeToReclaim bool, reason string, err error) {
	pathPtr, cerr := windows.UTF16PtrFromString(path)
	if cerr != nil {
		return false, "", fmt.Errorf("convert path %q to UTF-16: %w", path, cerr)
	}
	h, cerr := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if cerr != nil {
		return false, "", fmt.Errorf("CreateFile failed: %w", cerr)
	}
	defer CloseHandleLogged(&h, "inspectExistingStagingFile:CreateFile h")

	var info windows.ByHandleFileInformation
	if gerr := windows.GetFileInformationByHandle(h, &info); gerr != nil {
		return false, "", fmt.Errorf("GetFileInformationByHandle failed: %w", gerr)
	}

	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return false, "it is a symlink/junction/reparse point, not a plain regular file", nil
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return false, "it is a directory, not a regular file", nil
	}
	if info.NumberOfLinks > 1 {
		return false, "it has multiple hard links", nil
	}
	if info.FileSizeHigh != 0 || info.FileSizeLow != 0 {
		size := (uint64(info.FileSizeHigh) << 32) | uint64(info.FileSizeLow)
		return false, fmt.Sprintf("it already contains data (%d byte(s))", size), nil
	}
	return true, "", nil
}

// neutralizeOrPanic encapsulates the resilience cleanup logic for an abandoned staging file.
// It attempts deletion first. If deletion fails, it forces a truncation down to 0 bytes
// to neutralize any partial garbage data that would trip up the next boot.
func neutralizeOrPanic(tmpName string, perm os.FileMode, maxAttempts int, backoff time.Duration, log *slog.Logger, actionErr error, title, extraPanicMsg string) {
	ondeleteErr := RetryFileOp(maxAttempts, backoff, func() error { return OsRemoveFunc(tmpName) })
	if ondeleteErr == nil {
		log.Debug("ExtraSafety: successfully neutralized staging file by deleting it", slog.String("tempfilename", tmpName))
		// Successful deletion, nothing more to do
		return
	}

	// aside: Trying to rename the file as an intermediary step (e.g. to .trash) usually fails
	// under the exact same security context as a deletion. Wiping it to 0 bytes bypasses the
	// directory management layer entirely and works purely on file-level write access,
	// making it the most robust fallback option available.
	log.Warn("ExtraSafety: failed to delete staging file(possibly due to directory permissions?), attempting truncation fallback", SafeErr(ondeleteErr))

	// Fallback: If we can't delete it, truncate it to 0 bytes.
	// Since we already have write handle permissions to this file, this is highly likely to succeed.
	truncErr := RetryFileOp(maxAttempts, backoff, func() error {
		return TruncateStagingFileToZero(tmpName, perm)
	})

	if truncErr == nil {
		log.Warn("ExtraSafety: successfully truncated staging file to 0 bytes as a fallback preservation step", slog.String("tempfilename", tmpName))
		return
	}

	if extraPanicMsg == "" || !strings.HasSuffix(extraPanicMsg, "\n") {
		extraPanicMsg += "\n"
	}

	// Absolute worst case scenario: Can't delete AND can't write/truncate an open file.
	// The file is stuck on disk with data, making a future boot panic inevitable.
	// Crash immediately while the administrator is interacting with the system.
	logmsg := fmt.Sprintf(
		"\n========================================================================\n"+
			"CRITICAL SAFETY PANIC: %s\n"+
			"The temporary staging file %q cannot be deleted or truncated.\n\n"+
			"Original error: %v\n"+
			"Delete error: %v\n"+
			"Truncation error: %v\n\n"+
			"%s"+
			"Halting execution immediately to prevent a corrupted filesystem operation.\n"+
			"========================================================================\n",
		title, tmpName, actionErr, ondeleteErr, truncErr, extraPanicMsg,
	)
	logCriticalThenPanic(log, logmsg)
	panic(nil)
}

// GetLastError wraps windows.GetLastError.
//
// Deprecated: Do not call this. Go erases thread-local error state prior to
// syscall execution. Always read the 3rd return value (err) from LazyProc.Call().
// don't use, see: https://github.com/golang/go/issues/41220
func GetLastError() error {
	panic2("BUG: don't use GetLastError because it's ran after each syscall and is what 3rd arg of LazyProc.Call(..) returns or if using wrapper it's in WinResult.CallStatus!")
	return windows.GetLastError() //nolint:wrapcheck //on-purpose wrapper over this! kept here on purpose but never reached!
}

// SetLastError wraps windows.GetLastError.
//
// Deprecated: Do not call this. Go erases thread-local error state prior to
// syscall execution(ie. runs SetLastError(0)). Always read the 3rd return value (err) from LazyProc.Call().
// if using wincoe's wrappers like BoundProcN then read it from WinResult.CallStatus
// don't use, see: https://github.com/golang/go/issues/41220
func SetLastError() {
	panic2("BUG: don't use SetLastError because it's ran and set to 0 before each syscall and GetLastError() is what 3rd arg of LazyProc.Call(..) returns or if using wrapper it's in WinResult.CallStatus!")
}

const (
	CTRL_C_EVENT     = windows.CTRL_C_EVENT     //0
	CTRL_BREAK_EVENT = windows.CTRL_BREAK_EVENT //1
	//clicked the close button on top right:
	CTRL_CLOSE_EVENT = windows.CTRL_CLOSE_EVENT //2
	//if win11 wants to restart/shutdown:
	CTRL_LOGOFF_EVENT   = windows.CTRL_LOGOFF_EVENT   //5
	CTRL_SHUTDOWN_EVENT = windows.CTRL_SHUTDOWN_EVENT //6
)

// ConsoleCtrlHandler is the required signature for Windows console control handlers.
// Return 1 (TRUE) if the event was handled, or 0 (FALSE) to pass it to the next handler.
type ConsoleCtrlHandler func(ctrlType uint32) uintptr

// RegisterCtrlHandler registers or unregisters a custom Windows console control handler.
// Pass true to add the handler (equivalent to passing 1), or false to remove it.
//
// The handler argument is expected to be a function with one uintptr-sized result. The function must not have arguments with size larger than the size of uintptr.
//
// Return 1 (TRUE) from the handler if the event was handled, or 0 (FALSE)
// to pass the signal down the OS handler chain.
//
// NOTE: Always pass top-level named functions to this API.
// Do NOT pass inline closures (e.g., anonymous func() values created dynamically inside a loop),
// because Go allocates a permanent assembly trampoline for each unique closure instance that
// is never garbage-collected. Passing dynamic closures will eventually exhaust Go's internal
// callback pool (~2,000 max) and panic the runtime. Named functions are cached and memory-safe.
func setCtrlHandler(handler ConsoleCtrlHandler, add bool) WinResult {
	var addVal uintptr = 0
	if add {
		addVal = 1
	}

	return procSetConsoleCtrlHandler.Call(windows.NewCallback(handler), addVal)
}

// RegisterCtrlHandler registers a custom Windows console control handler.
//
// The handler argument is expected to be a function with one uintptr-sized result. The function must not have arguments with size larger than the size of uintptr.
//
// If you hit Ctrl+C a second time while your handler is still executing, it will run again concurrently on a new thread.
// Windows does not queue or lock control handler events for you—it handles them by spawning threads.
// If your shutdown logic is not idempotent (safe to run multiple times), re-entrant calls will cause subtle bugs or crashes.
func RegisterCtrlHandler(handler ConsoleCtrlHandler) WinResult {
	return setCtrlHandler(handler, true)
}

// UnregisterCtrlHandler unregisters a previously registered Windows console control handler.
//
// The handler argument is expected to be a function with one uintptr-sized result. The function must not have arguments with size larger than the size of uintptr.
func UnregisterCtrlHandler(handler ConsoleCtrlHandler) WinResult {
	return setCtrlHandler(handler, false)
}

// CURRENT_PROCESS_PSEUDO_HANDLE is what GetCurrentProcess returns a valid pseudo-handle which happens to be -1.
// In Go, ^uintptr(0) (all bits set) is the numeric representation of -1
const CURRENT_PROCESS_PSEUDO_HANDLE = ^uintptr(0) // All bits set to 1

// CURRENT_THREAD_PSEUDO_HANDLE is what GetCurrentThread returns, a valid pseudo-handle, in uintptr fashion (64-bit), -2 is: 0xFFFFFFFFFFFFFFFE aka ^uintptr(1)
const CURRENT_THREAD_PSEUDO_HANDLE uintptr = ^uintptr(1)

// PostMessage sends a Win32 message to a window's message queue.
//
//go:uintptrescapes
func PostMessage(hwnd windows.Handle, msg uint32, wParam, lParam uintptr) WinResult {
	return procPostMessage.Call(
		uintptr(hwnd),
		uintptr(msg),
		wParam,
		lParam,
	)
}

// PostThreadMessage sends a Win32 message to a thread's message queue.
//
//go:uintptrescapes
func PostThreadMessage(threadID, msg uint32, wParam, lParam uintptr) WinResult {
	return procPostThreadMessage.Call(
		uintptr(threadID),
		uintptr(msg),
		wParam,
		lParam,
	)
}

// KEYBDINPUT is used for synthesizing inputs via SendInput
type KEYBDINPUT struct {
	WVk         uint16
	WScan       uint16
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

// MOUSEINPUT is used for synthesizing inputs via SendInput
type MOUSEINPUT struct {
	Dx          int32
	Dy          int32
	MouseData   uint32
	DwFlags     uint32
	Time        uint32
	DwExtraInfo uintptr
}

// KEYANDMOUSE_INPUT represent any of the above 2
// "The Union Problem: In C, an INPUT structure is a union where the largest member dictates the overall size of the structure.
// MOUSEINPUT is larger than KEYBDINPUT. To prevent buffer overflows or misalignment when passing an array of INPUT structures to native APIs like SendInput,
// Go structs representing C unions must explicitly pad out any remaining bytes so that Go's unsafe.Sizeof() matches the C header size precisely."
type KEYANDMOUSE_INPUT struct {
	Type uint32
	_    uint32 // explicit padding for 64-bit alignment
	Ki   KEYBDINPUT
	_    [8]byte // tail padding to make union 32 bytes, because Ki should be MOUSEINPUT(32) not KEYBDINPUT(24 bytes) because the former's the biggest member of the union.
}

// SendInput synthesizes keystrokes, mouse motions, and button clicks.
//
// If `inputs` isn't already on heap or global, so if it's on stack it will be escaping to heap due to go:uintptrescapes being in effect!
func SendInput(inputs []KEYANDMOUSE_INPUT) WinResult {
	howMany := len(inputs)
	if howMany <= 0 {
		//return WinResult{} // Return early to avoid indexing an empty slice
		panic2("BUG: you passed empty slice of inputs, this indicates bad coding and should be fixed at call site, ie. if expected to be empty then check for that and don't call this at all; else assuming you mistakenly passed empty slice due to some bug")
		panic(nil)
	}

	return procSendInput.Call(
		uintptr(howMany),
		uintptr(unsafe.Pointer(&inputs[0])), // Or unsafe.Pointer(unsafe.SliceData(inputs)) in Go 1.20+
		unsafe.Sizeof(inputs[0]),
	)
}

// GetAsyncKeyState retrieves the status of the specified virtual key.
// Returns a 16-bit integer where bit 15 (0x8000) indicates if the key is down.
func GetAsyncKeyState(vKey int) uint16 {
	res := procGetAsyncKeyState.Call(uintptr(vKey))
	return uint16(res.R1 & 0xFFFF)
}

// IsKeyDown returns true if the specified virtual key is currently pressed.
func IsKeyDown(vKey int) bool {
	ret := GetAsyncKeyState(vKey)
	return (ret & 0x8000) != 0
	// ret := procGetAsyncKeyState.Call(uintptr(vKey))
	// return (uint16(ret.R1&0xFFFF) & uint16(0x8000)) != 0
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

type MSG struct {
	HWnd    windows.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      POINT
}

type PAINTSTRUCT struct {
	Hdc         windows.Handle
	FErase      int32
	RcPaint     RECT
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

type MONITORINFO struct {
	//In the Win32 API, GetMonitorInfo expects cbSize to equal the exact byte size of the structure being passed to it.
	// In fact, Windows uses cbSize to check whether you are passing a standard MONITORINFO (40 bytes)
	// or an extended MONITORINFOEX (72 bytes, which includes szDevice). If cbSize is 0 or invalid,
	//  GetMonitorInfo will fail and return FALSE.
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

const WM_QUIT = 0x0012

// GetMessage retrieves a message from the calling thread's message queue.
// It dispatches no incoming sent messages, waiting for a message to be posted
// within the range specified by msgFilterMin and msgFilterMax.
//
// # Window Handle Parameter (hwnd)
//
//   - 0 (NULL): Retrieves messages for any window belonging to the current thread,
//     as well as thread-specific messages posted to the thread's message queue
//     via PostThreadMessage (where hwnd is NULL).
//   - -1 (windows.Handle(^uintptr(0))): Retrieves ONLY thread-specific messages
//     posted to the thread's message queue via PostThreadMessage. It completely
//     ignores messages intended for any windows.
//
// # Return Value(in WinResult.R1) & Error Behavior
//
//   - Nonzero: A message was successfully retrieved (other than WM_QUIT).
//   - Zero (0): The WM_QUIT message was retrieved, signaling the application to exit.
//   - -1 (or WinResult.Failed()): An error occurred (e.g., an invalid window handle).
//     WinResult.Err will contain the Windows system error code.
func GetMessage(msg *MSG, hwnd windows.Handle, msgFilterMin, msgFilterMax uint32) WinResult {
	return procGetMessage.Call(uintptr(unsafe.Pointer(msg)), uintptr(hwnd), uintptr(msgFilterMin), uintptr(msgFilterMax))
}

// TranslateMessage translates virtual-key messages into character messages.
//
// R1 (Return Value) aka WinResult.R1:
//   - Non-zero (TRUE): The message was successfully translated (meaning a character message like WM_CHAR was posted to the thread's message queue), or the message was a key-down/key-up message (WM_KEYDOWN, WM_KEYUP, etc.) regardless of whether it translated into a character.
//   - Zero (FALSE): The message was not translated (no character message was posted). This is a common, completely normal occurrence (e.g., when pressing modifier keys like Ctrl, Shift, or system keys that don't map to text output).
//
// GetLastError() (aka WinResult.Err) Behavior:
//   - Microsoft's documentation does not specify that TranslateMessage sets an extended error code via GetLastError(). Because returning 0 is a normal state (meaning "not a text-generating key"), treating 0 as a system failure would cause massive false-positive errors.
func TranslateMessage(msg *MSG) WinResult {
	return procTranslateMessage.Call(uintptr(unsafe.Pointer(msg)))
}

// DispatchMessage dispatches a message to a window procedure.
//
// R1 (Return Value):
//   - LRESULT: The return value specifies the value returned by the window procedure (WindowProc) that processed the message. Its meaning entirely depends on which window message was dispatched (e.g., WM_CREATE returns 0 on success or -1 on failure; other messages ignore it entirely).
//
// GetLastError() Behavior:
//   - Microsoft's documentation states: "Although its meaning depends on the message being dispatched, the return value generally is ignored." It does not reliably set a thread-level last-error code upon dispatch. If a window procedure inside your app crashes or fails, the error originates from your user-defined code, not a standard OS failure code tracked by GetLastError().
//
// a DispatchMessage call could do:
//   - Go clears the error -> calls DispatchMessage.
//   - DispatchMessage enters the OS message pump and synchronously executes your window procedure (WndProc).
//   - If an API call inside your WndProc (or an internal OS control handler triggered by the message) fails and calls SetLastError(...), that error is still active on the thread when DispatchMessage finishes.
//   - Go immediately captures GetLastError(), which now holds the error produced by that internal execution path!
func DispatchMessage(msg *MSG) WinResult {
	return procDispatchMessage.Call(uintptr(unsafe.Pointer(msg)))
}

// DefWindowProc calls the default window procedure to provide default processing.
//
//go:uintptrescapes
func DefWindowProc(hwnd windows.Handle, msg uint32, wParam, lParam uintptr) WinResult {
	//doneTODO: for all wrappers like this(eg. CallNextHookEx) that use uintptr args, put a //go:uintptrescapes just in case, it doesn't apply in this case because it uses wtw value/pointer Windows gave it, but you never know if user decides to uintptr() case their own pointer the crash like hell due to lack of go:uintptrescapes
	return procDefWindowProc.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
}

// PostQuitMessage indicates to the system that a thread has made a request to terminate.
func PostQuitMessage(exitCode int32) {
	//"In any non-constant conversion between integer types, if the value is a signed integer,
	//  it is sign-extended to implicit infinite precision; otherwise it is zero-extended.
	// It is then truncated to fit in the result type's size." - Go Spec (Conversions between integer types), via Gemini 3.6 Flash Extended Thinking
	//so, because Go bases sign extension on the source type (int32 being signed), sign extension occurs automatically during the conversion to uintptr.
	// #nosec G115 -- Intentional Win32 low-level sign-bit preservation -- intentional Win32 signed-to-unsigned conversion
	_ = procPostQuitMessage.Call(uintptr(exitCode)) // nolint:errcheck // it's CheckNone and res.R1 is always 0 aka useless!
}

// GetWindowRect retrieves the dimensions of the bounding rectangle of the specified window.
func GetWindowRect(hwnd windows.Handle, rect *RECT) WinResult {
	return procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(rect)))
}

// GetClientRect retrieves the coordinates of a window's client area.
func GetClientRect(hwnd windows.Handle, rect *RECT) WinResult {
	return procGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(rect)))
}

const (
	SW_RESTORE  = 9
	SW_MAXIMIZE = 3

	SWP_NOSIZE         = 0x0001
	SWP_NOMOVE         = 0x0002
	SWP_NOZORDER       = 0x0004
	SWP_NOREDRAW       = 0x0008
	SWP_NOACTIVATE     = 0x0010
	SWP_SHOWWINDOW     = 0x0040
	SWP_HIDEWINDOW     = 0x0080
	SWP_ASYNCWINDOWPOS = 0x4000
)

const (
	WM_MBUTTONDOWN = 0x0207
	WM_MBUTTONUP   = 0x0208

	HWND_TOP    = windows.Handle(uintptr(0)) // good
	HWND_BOTTOM = windows.Handle(uintptr(1)) // good

	HWND_TOPMOST   = windows.Handle(^uintptr(0)) // (HWND)-1
	HWND_NOTOPMOST = windows.Handle(^uintptr(1)) // (HWND)-2

)

// SetWindowPos changes the size, position, and Z order of a child, pop-up, or top-level window.
func SetWindowPos(hwnd, hwndInsertAfter windows.Handle, x, y, cx, cy int32, flags uint32) WinResult {
	return procSetWindowPos.Call(
		uintptr(hwnd),
		uintptr(hwndInsertAfter),
		// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
		uintptr(x),
		// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
		uintptr(y),
		// #nosec G115 -- safe: Win32 dimensions are sign-extended from int32 into uintptr
		uintptr(cx),
		// #nosec G115 -- safe: Win32 dimensions are sign-extended from int32 into uintptr
		uintptr(cy),
		uintptr(flags),
	)
}

// ShowWindowRaw sets the specified window's show state.
// It returns a WinResult where R1 is non-zero if the window was previously visible,
// or zero if the window was previously hidden.
//
// ShowWindowRaw sets the specified window's show state.
// If the window was previously visible(eg. WS_VISIBLE), the return value WinResult.R1 is nonzero.
// If the window was previously hidden(eg. SW_HIDE), the return value WinResult.R1 is zero.
func ShowWindowRaw(hwnd windows.Handle, nCmdShow int32) WinResult {
	// If the window was previously visible, the return value is nonzero.
	// If the window was previously hidden, the return value is zero.

	// #nosec G115 -- Win32 nCmdShow int32 converted to uintptr for syscall
	return procShowWindow.Call(uintptr(hwnd), uintptr(nCmdShow)) // it's CheckNone but can still use ret.R1 since it's of BOOL type
}

// ShowWindow sets the specified window's show state.
// It returns true if the window was previously visible, or false if it was previously hidden.
//
// ShowWindow sets the specified window's show state.
// If the window was previously visible(eg. WS_VISIBLE), the return value is true.
// If the window was previously hidden(eg. SW_HIDE), the return value is false.
func ShowWindow(hwnd windows.Handle, nCmdShow int32) (wasShownBefore bool) {
	if res := ShowWindowRaw(hwnd, nCmdShow); res.R1 != 0 {
		return true
	}
	return false
}

// DestroyWindow destroys the specified window.
func DestroyWindow(hwnd windows.Handle) WinResult {
	return procDestroyWindow.Call(uintptr(hwnd))
}

// GetWindowPlacement retrieves the show state and the restored, minimized, and maximized positions.
func GetWindowPlacement(hwnd windows.Handle, wp *WINDOWPLACEMENT) WinResult {
	return procGetWindowPlacement.Call(uintptr(hwnd), uintptr(unsafe.Pointer(wp)))
}

// --- Hook Structs ---

// KBDLLHOOKSTRUCT is used by low-level keyboard hooks (SetWindowsHookEx with WH_KEYBOARD_LL)
type KBDLLHOOKSTRUCT struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

// MSLLHOOKSTRUCT is used by low-level mouse hooks (SetWindowsHookEx with WH_MOUSE_LL)
type MSLLHOOKSTRUCT struct {
	Pt          POINT
	MouseData   uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

const (
	WH_KEYBOARD_LL = 13
	WH_MOUSE_LL    = 14
)

// HookProc represents the callback signature required by Windows for SetWindowsHookEx.
type HookProc func(nCode int32, wParam uintptr, lParam unsafe.Pointer) (LRESULT uintptr)

// SetWindowsHookEx installs an application-defined hook procedure into a hook chain.
//
// as `lpfn` arg, don't pass the windows.NewCallback() of it, but the actual Go function itself, because windows.NewCallback(func) will be called on it inside it!
//
// hMod = 0 for low-level
//
// dwThreadId = 0 = global
//
// NOTE: Always pass top-level named functions to this API.
// Do NOT pass inline closures (e.g., anonymous func() values created dynamically inside a loop),
// because Go allocates a permanent assembly trampoline for each unique closure instance that
// is never garbage-collected. Passing dynamic closures will eventually exhaust Go's internal
// callback pool (~2,000 max) and panic the runtime. Named functions are cached and memory-safe.
func SetWindowsHookEx(idHook int32, lpfn HookProc, hmod windows.Handle, dwThreadId uint32) (windows.Handle, WinResult) {
	res := procSetWindowsHookEx.Call(
		// #nosec G115
		uintptr(idHook),
		windows.NewCallback(lpfn),
		uintptr(hmod),
		uintptr(dwThreadId))
	return windows.Handle(res.R1), res
}

// UnhookWindowsHookEx removes a hook procedure installed in a hook chain.
func UnhookWindowsHookEx(hhk windows.Handle) WinResult {
	return procUnhookWindowsHookEx.Call(uintptr(hhk))
}

// CallNextHookEx passes the hook information to the next hook procedure.
//
// use uintptr(lParam) at call site even though you declared it as lParam unsafe.Pointer
//
// Why is this safe? Because in mouseProc/keyboardProc,
// that lParam pointer is coming from Windows into your callback.
// It points to a MSLLHOOKSTRUCT that Windows allocated in its own memory space.
// The Go garbage collector does not own this memory, does not track it, and cannot move or free it.
// Therefore, converting it to a plain integer (uintptr) immediately(at call site) is perfectly safe.
//
//go:uintptrescapes
func CallNextHookEx(hhk windows.Handle, nCode int32, wParam, lParam uintptr) WinResult {
	return procCallNextHookEx.Call(uintptr(hhk),
		// #nosec G115
		uintptr(nCode),
		wParam, lParam)
}

// GetCursorPos retrieves the position of the mouse cursor, in screen coordinates.
func GetCursorPos(pt *POINT) WinResult {
	return procGetCursorPos.Call(uintptr(unsafe.Pointer(pt)))
}

// SetCursorPos moves the cursor to the specified screen coordinates.
func SetCursorPos(x, y int32) WinResult {
	return procSetCursorPos.Call(
		//When SetCursorPos(X, Y) is called, Windows expects the X coordinate to be in the RCX register and Y to be in RDX.
		// Even though the arguments are 32-bit integers, Windows expects the entire 64-bit register to be properly sign-extended.
		// If the upper 32 bits contain garbage or are cleared to zero when they shouldn't be, the CPU behavior or the OS wrapper can misinterpret the value.
		// and that's why the 'inf' cast is needed. What inf? It's enough they're int32 cast to uintptr!
		// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
		uintptr(x),
		// #nosec G115 -- safe: Win32 coordinates are sign-extended from int32 into uintptr
		uintptr(y))
}

// // WindowFromPointRaw retrieves a handle to the window that contains the specified point.
// //
// // uses the raw procWindowFromPoint
// //XXX: don't need a WindowFromPointRaw because GetLastError() is never set!
// func WindowFromPointRaw(pt POINT) WinResult {
// 	/* doneFIXME:
// 		There are a few specific mappings where Windows might return 0 or NULL without setting GetLastError(), which will cause your CheckWinResult to hit the fallback path ("windows call reported failure (ret=X) but no usable error was provided").
// 		2. procWindowFromPoint & procGetAncestor (CheckNull)
// 	    The Nuance: If a point is over an empty desktop space, or a window has no ancestor, these return NULL. They do not explicitly set GetLastError(). Your CheckNull logic will interpret this as a failure and generate the fallback error.
// 	    Why it currently works anyway: In your wrapper functions, you do this:
// 	    Go
// 	    res1 := WindowFromPointRaw(pt)
// 	    if res1.Failed() { return 0 }
// 	    Because you immediately swallow the .Failed() state and return 0, the dirty fallback error string never escapes to the console. Furthermore, your panic2 checks specifically expect .Failed() to be true if .R1 == 0.
// 	    Verdict: Leave them as CheckNull to satisfy your panic2 safeguards, but be aware of the invisible fallback error being generated under the hood.
// 	*/
// 	// Packs the X/Y into a single 64-bit register for the AMD64 calling convention
// 	/*
// 		A POINT struct consists of two 32-bit integers (LONG x and LONG y).
// 		Because $32 \text{ bits} + 32 \text{ bits} = 64 \text{ bits}$ ($8 \text{ bytes}$),
// 		the entire struct fits neatly inside a single 64-bit register (like RCX) on 64-bit Windows.
// 		Under the Microsoft x64 ABI, structs that are 8 bytes or smaller must be passed by value
// 			in a register, not as a pointer to memory.
// 			Why Go Needs This Trick
// 		Go's syscall / proc.Call mechanism only accepts arguments of type uintptr (representing raw
// 		register slots or stack values).
// 		You cannot pass a composite struct like POINT directly into proc.Call(pt).
// 		If you pass a pointer (&pt), you are passing a memory address, which violates what WindowFromPoint
// 		expects (it wants the raw 64-bit data value, not a pointer to it).
// 		&pt: Takes the memory address of the local POINT struct (which lives safely on the stack).
// 		unsafe.Pointer(&pt): Erases Go's type system restrictions, turning it into a generic unsafe pointer.
// 		(*uintptr)(...): Casts that pointer type into a pointer to a uintptr (which is 64 bits wide on 64-bit systems).
// 		*... (Dereference): Reads the 8 bytes of memory starting at &pt as if they were a single uintptr
// 			scalar value instead of two separate int32 fields.
// 	*/
// 	//This passes the exact 64-bit bit pattern of X and Y packed together into the CPU register (RCX),
// 	//  perfectly matching the C ABI expectation without triggering any heap allocations.
// 	res := procWindowFromPoint.Call(*(*uintptr)(unsafe.Pointer(&pt)))
// 	return res
// }

// WindowFromPoint retrieves a handle to the window that contains the specified point.
// Returns 0 if no window exists at the given point (e.g., over empty desktop space).
func WindowFromPoint(pt POINT) windows.Handle {
	/* doneFIXME:
	There are a few specific mappings where Windows might return 0 or NULL without setting GetLastError(), which will cause your CheckWinResult to hit the fallback path ("windows call reported failure (ret=X) but no usable error was provided").
	2. procWindowFromPoint & procGetAncestor (CheckNull)
	The Nuance: If a point is over an empty desktop space, or a window has no ancestor, these return NULL. They do not explicitly set GetLastError(). Your CheckNull logic will interpret this as a failure and generate the fallback error.
	Why it currently works anyway: In your wrapper functions, you do this:
	Go
	res1 := WindowFromPointRaw(pt)
	if res1.Failed() { return 0 }
	Because you immediately swallow the .Failed() state and return 0, the dirty fallback error string never escapes to the console. Furthermore, your panic2 checks specifically expect .Failed() to be true if .R1 == 0.
	Verdict: Leave them as CheckNull to satisfy your panic2 safeguards, but be aware of the invisible fallback error being generated under the hood.
	*/

	// Packs the X/Y into a single 64-bit register for the AMD64 calling convention
	/*
		A POINT struct consists of two 32-bit integers (LONG x and LONG y).
		Because $32 \text{ bits} + 32 \text{ bits} = 64 \text{ bits}$ ($8 \text{ bytes}$),
		the entire struct fits neatly inside a single 64-bit register (like RCX) on 64-bit Windows.
		Under the Microsoft x64 ABI, structs that are 8 bytes or smaller must be passed by value
			in a register, not as a pointer to memory.

			Why Go Needs This Trick

		Go's syscall / proc.Call mechanism only accepts arguments of type uintptr (representing raw
		register slots or stack values).
		You cannot pass a composite struct like POINT directly into proc.Call(pt).
		If you pass a pointer (&pt), you are passing a memory address, which violates what WindowFromPoint
		expects (it wants the raw 64-bit data value, not a pointer to it).

		&pt: Takes the memory address of the local POINT struct (which lives safely on the stack).
		unsafe.Pointer(&pt): Erases Go's type system restrictions, turning it into a generic unsafe pointer.
		(*uintptr)(...): Casts that pointer type into a pointer to a uintptr (which is 64 bits wide on 64-bit systems).
		*... (Dereference): Reads the 8 bytes of memory starting at &pt as if they were a single uintptr
			scalar value instead of two separate int32 fields.
	*/
	//This passes the exact 64-bit bit pattern of X and Y packed together into the CPU register (RCX),
	//  perfectly matching the C ABI expectation without triggering any heap allocations.
	res := procWindowFromPoint.Call(*(*uintptr)(unsafe.Pointer(&pt))) //it's CheckNone (for good reason) so you can't use res.Failed() here.
	//XXX: don't need a WindowFromPointRaw because GetLastError() is never set! "According to Microsoft documentation, WindowFromPoint does not set a last-error code on failure."

	return windows.Handle(res.R1)
}

const (
	GA_ROOT      = 2
	GA_ROOTOWNER = 3
)

// RootWindowFromPoint retrieves the top-level window handle located at the specified
// screen coordinates.
//
// It first locates the deepest child window under the point using WindowFromPoint.
// If a window is found, it walks up the window hierarchy using GetAncestor with
// GA_ROOT to find its absolute top-level ancestor.
//
// "Top-level window" is the standard terminology used by the Win32 API to describe
// a window that has no parent. If the child window is already a top-level window,
// its own handle is returned.
//
// Returns the non-zero window handle (HWND) of the root window, or 0 if no window
// is present at the point (e.g., clicking on empty desktop space) or if the lookup fails.
//
// Note: A return handle of 0 with res.Failed() == false indicates no window
// exists at the given coordinates (e.g., empty desktop space).
// Callers must check `hwnd != 0` in addition to `!res.Failed()`.
func RootWindowFromPoint(pt POINT) (windows.Handle, WinResult) {
	hwnd := WindowFromPoint(pt) // it doesn't set GetLastError()

	if hwnd == 0 { // Expected behavior: clicked on empty desktop space.
		return 0, WinResult{}
	}

	// Get the top-level owner of this HWND.
	// GA_ROOT (2) retrieves the top-level parent window.
	hwndRoot, res := GetAncestor(hwnd, GA_ROOT)
	// If hwndRoot is 0, GetAncestor failed (e.g., window was destroyed concurrently).
	// Otherwise, it returns either the root ancestor or the window itself if already top-level.
	return hwndRoot, res //whenever res.Failed() is true, r1 (the handle) is guaranteed to be 0 here. Due to how GetAncestor works atm.
}

const WM_PAINT = 0x000F

// BeginPaint prepares the specified window for painting.
func BeginPaint(hwnd windows.Handle, ps *PAINTSTRUCT) (windows.Handle, WinResult) {
	res := procBeginPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(ps)))
	return windows.Handle(res.R1), res // it's CheckNull so res.R1 is 0 if res.Failed()
}

// EndPaint marks the end of painting in the specified window.
//
// it never fails and never sets GetLastError() hence it's void return!
func EndPaint(hwnd windows.Handle, ps *PAINTSTRUCT) /*void*/ {
	//"When invoked via LazyProc.Call / SyscallN, R1 will always evaluate to 1 (representing TRUE). EndPaint never returns 0."
	//"Does it set GetLastError()? No. Because EndPaint is contractually guaranteed never to fail, Windows does not set or modify GetLastError()."
	//"Is checking LastError useless? Yes, completely useless. Any value present in CallStatus / GetLastError() after calling EndPaint is purely "stale" residue left over from some earlier API call made on the same thread."
	_ = procEndPaint.Call(uintptr(hwnd), uintptr(unsafe.Pointer(ps))) //nolint:errcheck //no point in checking something that's always TRUE and no GetLastError() is ever set!
}

// FillRect fills a rectangle by using the specified brush.
func FillRect(hdc windows.Handle, rect *RECT, hbr windows.Handle) WinResult {
	return procFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(rect)), uintptr(hbr))
}

// used as the format arg for DrawText() OR-ed!
const (
	DT_TOP        uint32 = 0x00000000
	DT_LEFT       uint32 = 0x00000000
	DT_CENTER     uint32 = 0x00000001
	DT_RIGHT      uint32 = 0x00000002
	DT_VCENTER    uint32 = 0x00000004
	DT_BOTTOM     uint32 = 0x00000008
	DT_WORDBREAK  uint32 = 0x00000010
	DT_SINGLELINE uint32 = 0x00000020
	DT_NOCLIP     uint32 = 0x00000100
)

// DrawTextLengthNullTerminated tells DrawText to assume text is null-terminated.
// this is used as the length arg for DrawText!
const DrawTextLengthNullTerminated int32 = -1

// DrawText draws formatted text in the specified rectangle.
// Note: caller should pass a pointer to a UTF-16 encoded string to prevent allocation hiding.
func DrawText(hdc windows.Handle, text *uint16, length int32, rect *RECT, format uint32) WinResult {
	return procDrawText.Call(
		uintptr(hdc),
		uintptr(unsafe.Pointer(text)), //uintptr cast must be inline here to cause it to escape to heap due to go:uintptrescapes
		// #nosec G115
		uintptr(length),
		uintptr(unsafe.Pointer(rect)), //uintptr cast must be inline here to cause it to escape to heap due to go:uintptrescapes
		uintptr(format))
}

// InvalidateRect adds a rectangle to the specified window's update region.
//
// When you pass 0 instead of a pointer to a RECT struct, Windows interprets it as: "Invalidate the entire client area of the window." If you wanted to only redraw a specific button or a small box, you would pass a pointer to a RECT defining those coordinates. Passing 0 forces the whole window content area to be marked as dirty and scheduled for a repaint.
//
// `erase`
//
// This specifies whether the background within the invalidated area should be erased (cleared/wiped out using the window's background brush) before the WM_PAINT message is processed.
// 1 (TRUE) tells Windows: "Clear the old drawing/background first so we can draw a fresh frame."
// * 0 (FALSE) would mean leave whatever is currently drawn on the screen intact underneath, which is typically only used if you are drawing over it completely or doing custom double-buffered rendering where you manage every single pixel yourself.
func InvalidateRect(hwnd windows.Handle, rect *RECT, erase bool) WinResult {
	var erasePtr uintptr
	if erase {
		erasePtr = 1
	}
	return procInvalidateRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(rect)), erasePtr)
}

// UpdateWindow updates the client area of the specified window.
func UpdateWindow(hwnd windows.Handle) WinResult {
	return procUpdateWindow.Call(uintptr(hwnd))
}

/*
In the Windows API, colors are passed as a COLORREF integer, which follows a strict hexadecimal byte layout: 0x00bbggrr (Blue, Green, Red).

	0x00: Reserved padding byte (must always be zero in COLORREF).

	FF: Blue intensity (255 / max).

	00: Green intensity (0 / none).

	FF: Red intensity (255 / max).

	// Example: Creating a solid brush for a specific color

// 0x000000FF = Pure Red (Blue=00, Green=00, Red=FF)
// 0x0000FF00 = Pure Green (Blue=00, Green=FF, Red=00)
// 0x00FF0000 = Pure Blue (Blue=FF, Green=00, Red=00)
*/
const (
	ColorMagenta uint32 = 0x00FF00FF
	ColorBlack   uint32 = 0x00000000
	ColorGreen   uint32 = 0x0000FF00
)

// SetLayeredWindowAttributes sets the opacity and transparency color key of a layered window.
func SetLayeredWindowAttributes(hwnd windows.Handle, crKey uint32, bAlpha byte, dwFlags uint32) WinResult {
	return procSetLayeredWindowAttributes.Call(uintptr(hwnd), uintptr(crKey), uintptr(bAlpha), uintptr(dwFlags))
}

// GDI Helpers

func GdiCreateSolidBrush(color uint32) (windows.Handle, WinResult) {
	res := procGdiCreateSolidBrush.Call(uintptr(color))
	return windows.Handle(res.R1), res
}

func GdiDeleteObject(hObject windows.Handle) WinResult {
	return procGdiDeleteObject.Call(uintptr(hObject))
}

func GdiSetTextColor(hdc windows.Handle, color uint32) WinResult {
	return procGdiSetTextColor.Call(uintptr(hdc), uintptr(color))
}

// SetBkMode_TRANSPARENT TRANSPARENT = 1 (The background remains unchanged when text or shapes are drawn)
const SetBkMode_TRANSPARENT = 1

// SetBkMode_OPAQUE OPAQUE = 2 (The background is filled with the current background color before drawing)
const SetBkMode_OPAQUE = 2

func GdiSetBkMode(hdc windows.Handle, mode int32) WinResult {
	// #nosec G115
	return procGdiSetBkMode.Call(uintptr(hdc), uintptr(mode))
}

// SetForegroundWindow puts the thread that created the specified window into the foreground.
//
// ie. this will focus the window (think of it like you LMB-ing a window, it gains focus - this is what foreground means)
//
// Returns true if focus was successfully granted, or false if denied by OS rules.
func SetForegroundWindow(hwnd windows.Handle) bool {
	/*
		doneFIXME:
		There are a few specific mappings where Windows might return 0 or NULL without setting GetLastError(), which will cause your CheckWinResult to hit the fallback path ("windows call reported failure (ret=X) but no usable error was provided").

		1. procSetForegroundWindow (CheckBool)

		The Nuance: If Windows denies the focus-stealing request (due to anti-stealing rules in modern Windows), the API simply returns 0. It often does not set GetLastError().

		Result: CheckBool will catch the 0, look for an error, find nothing, and generate your fallback error string. If you consider "denied focus" to be a hard error, this is fine. If you want to check it manually without generating an error struct, switch to CheckNone.
	*/
	//great so now that the above fixme is done, this is CheckNone here:
	return procSetForegroundWindow.Call(uintptr(hwnd)).R1 != 0
}

// GetForegroundWindow retrieves a handle to the foreground window.
//
// A return value of 0 means there is currently no foreground window (e.g.
// the workstation is locked, a screen saver is active, or the system is
// mid window-switch) — not a failure. GetForegroundWindow never sets
// GetLastError(aka WinResult.CallStatus), not even on a 0 return, so there is no meaningful WinResult
// to surface here; only R1 is ever informative for this API which is what's returned only.
func GetForegroundWindow() windows.Handle {
	return windows.Handle(procGetForegroundWindow.Call().R1)
}

// SetCaptureRaw sets the mouse capture to the specified window.
//
// What SetCapture Returns
//   - Success (With Previous Capture): Returns the HWND handle of the window that previously held the mouse capture.
//   - Success (No Previous Capture): Returns NULL (0) if no other window was holding the capture.
func SetCaptureRaw(hwnd windows.Handle) WinResult {
	res := procSetCapture.Call(uintptr(hwnd))
	//previousCapture = windows.Handle(res.R1)
	return res
}

// SetCapture sets the mouse capture to the specified window belonging to the current thread.
//
// Returns the handle of the window that previously had mouse capture, or 0 (NULL) if no
// other window was holding capture.
//
// Note: SetCapture does not set GetLastError(). To verify whether capture was actually
// acquired, call wincoe.GetCapture() and verify it matches the target hwnd.
func SetCapture(hwnd windows.Handle) (prevCapture windows.Handle) {
	res := SetCaptureRaw(hwnd)
	return windows.Handle(res.R1)
}

// ReleaseCapture releases the mouse capture.
func ReleaseCapture() WinResult {
	return procReleaseCapture.Call()
}

// GetCapture retrieves a handle to the window that has captured the mouse.
func GetCapture() windows.Handle {
	res := procGetCapture.Call()
	return windows.Handle(res.R1)
}

// GetWindowThreadProcessId retrieves the identifier of the thread that created the specified window.
//
// pid = process ID
// tid = thread ID
func GetWindowThreadProcessId(hwnd windows.Handle, pid *uint32) (tid uint32, res WinResult) {
	res = procGetWindowThreadProcessID.Call(uintptr(hwnd), uintptr(unsafe.Pointer(pid)))
	// #nosec G115
	tid = uint32(res.R1)
	return
}

// GetAncestorRaw retrieves the handle to the ancestor of the specified window
// as a raw WinResult.
//
// If hwnd is already a top-level window (has no parent), Win32 returns hwnd itself.
// If the call fails (e.g., invalid handle), WinResult.R1 will be 0 and WinResult.Err
// will contain the error.
func GetAncestorRaw(hwnd windows.Handle, flags uint32) WinResult {
	return procGetAncestor.Call(uintptr(hwnd), uintptr(flags))
}

// GetAncestor retrieves the handle to the ancestor of the specified window.
//
// If hwnd is already a top-level window (has no parent), it returns hwnd itself.
// Returns 0 if the handle is invalid or the API call fails.
func GetAncestor(hwnd windows.Handle, flags uint32) (windows.Handle, WinResult) {
	// return windows.Handle(GetAncestorRaw(hwnd, flags).R1)

	//paranoid checks
	res := GetAncestorRaw(hwnd, flags)
	// handle := res.R1
	// if res.Failed() && handle != 0 {
	// 	panic2(fmt.Sprintf("BUG: GetAncestorRaw failed but R1 wasn't 0 but %d, res:%v", handle, res))
	// }
	// return windows.Handle(handle)
	return windows.Handle(res.R1), res
}

// GetTopWindow examines the Z-order of the child windows associated with the
// specified parent window and retrieves a handle to the top child window.
//
// Passing an hwnd of 0 (NULL) retrieves the top-level window at the top of
// the system-wide Z-order.
//
// # Return Value & Error Behavior
//
// A return value of 0 (NULL) in WinResult.R1 means:
//  1. Benign ("No Children"): The specified window has no child windows.
//     WinResult.Err will be nil.
//  2. Failure: An error occurred (e.g., invalid parent handle). WinResult.Err
//     will contain the Windows system error code.
func GetTopWindow(hwnd windows.Handle) (windows.Handle, WinResult) {
	res := procGetTopWindow.Call(uintptr(hwnd))
	return windows.Handle(res.R1), res
}

// GW_HWNDNEXT is GetWindow's uCmd code for "next window in Z-order".
const GW_HWNDNEXT = 2

// GW_HWNDPREV is GetWindow's uCmd code for "previous window in Z-order" --
// i.e. the window immediately ABOVE (in front of) the specified window. A
// result of R1==0 with WinResult.Succeeded() true means the specified
// window has no predecessor and is therefore already the topmost top-level
// window in the system Z-order.
const GW_HWNDPREV = 3

// GW_OWNER is GetWindow's uCmd code for "the window's owner" (set via
// GWL_HWNDPARENT / CreateWindowEx's hWndParent on an owned, non-child
// WS_POPUP-style window). A result of R1==0 with WinResult.Succeeded()
// true means the specified window has no owner.
const GW_OWNER = 4

// // GetWindow retrieves a handle to a window that has the specified relationship (Z-Order or owner) to the specified window.
// func GetWindow(hwnd windows.Handle, uCmd uint32) windows.Handle {
// 	res := procGetWindow.Call(uintptr(hwnd), uintptr(uCmd))
// 	return windows.Handle(res.R1)
// }

// // GetWindow retrieves the handle of a window that has the specified relationship
// // (such as Z-order position or owner) to the specified window.
// //
// // # Return Value & Error Ambiguity
// //
// // A return value of 0 (NULL) can mean two completely different things:
// //  1. Benign / Expected ("Not Found"): The window search completed successfully,
// //     but no window exists in the specified direction (e.g., you reached the end
// //     of the Z-order or the window has no child windows). In this case, no actual
// //     system error occurred.
// //  2. True Failure: An error occurred during execution — most commonly because
// //     the passed hwnd handle was invalid, destroyed, or belongs to a different state.
// //
// // Because 0 is a valid architectural result rather than a guaranteed failure,
// // callers must inspect the associated error (or WinResult) if distinguishing
// // between "end of list" and "invalid handle" is required.
// func GetWindow(hwnd windows.Handle, uCmd uint32) (windows.Handle, error) {
// 	res1 := procGetWindow.Call(uintptr(hwnd), uintptr(uCmd))
// 	handle := windows.Handle(res1.R1)

// 	if handle == 0 {
// 		lastErr := res1.CallStatus
// 		// If lastErr is non-nil and not ERROR_SUCCESS, a true failure occurred
// 		// (e.g., ERROR_INVALID_WINDOW_HANDLE). If it's nil or ERROR_SUCCESS,
// 		// it's simply a benign "no window found" result.
// 		if lastErr != nil && !errors.Is(lastErr, windows.ERROR_SUCCESS) {
// 			return 0, lastErr
// 		}
// 	}

// 	return handle, nil
// }

// GetWindow retrieves the handle of a window that has the specified relationship
// (such as Z-order position or owner) to the specified window.
//
// Passing an hwnd of 0 (NULL) fails and sets an error (e.g., ERROR_INVALID_WINDOW_HANDLE).
//
// # Return Value & Error Behavior
//
// A handle value of 0 (NULL) in WinResult.R1 can mean two things:
//  1. Benign ("Not Found"): Reached the end of the Z-order or no window exists in
//     that direction. WinResult.Err will be nil.
//  2. Failure: An error occurred (e.g., invalid/destroyed handle). WinResult.Err
//     will contain the Windows system error code.
func GetWindow(hwnd windows.Handle, uCmd uint32) WinResult {
	return procGetWindow.Call(uintptr(hwnd), uintptr(uCmd))
}

// IsWindowVisibleRaw determines the visibility state of the specified window.
//
// passing hwnd==0 will return R1==0 aka FALSE because "Windows recognizes that it is not a valid window handle."
func IsWindowVisibleRaw(hwnd windows.Handle) WinResult {
	return procIsWindowVisible.Call(uintptr(hwnd))
}

// IsWindowVisible determines the visibility state of the specified window.
//
// a hwnd of 0 will return false (by Windows! so the syscall will still be called!)
func IsWindowVisible(hwnd windows.Handle) bool {
	res := IsWindowVisibleRaw(hwnd)
	return res.R1 != 0
}

// IsWindowRaw determines whether the specified window handle identifies an existing window.
//
// Because 0 (NULL) is reserved by Windows to denote an invalid/absent handle, IsWindow(0) immediately determines that no such window exists and returns 0 (FALSE).
func IsWindowRaw(hwnd windows.Handle) WinResult {
	return procIsWindow.Call(uintptr(hwnd))
	//return res.R1 != 0
}

// IsWindow returns false for hwnd==0 (the syscall would've done it too, but there's a special 'if' for it)
func IsWindow(hwnd windows.Handle) bool {
	if hwnd == 0 {
		return false
	}
	// res := procIsWindow.Call(uintptr(hwnd))
	// //return res.R1 != 0 && !res.Failed()
	// return !res.Failed()
	res := IsWindowRaw(hwnd)
	return res.R1 != 0
}

// GetSystemMetrics retrieves the specified system metric or system configuration setting.
func GetSystemMetrics(nIndex int32) int32 {
	res := procGetSystemMetrics.Call(
		// #nosec G115
		uintptr(nIndex),
	)
	// #nosec G115
	return int32(res.R1)
}

const MONITOR_DEFAULTTONEAREST = 2

// MonitorFromWindow retrieves a handle to the display monitor that has the largest area of intersection with the bounding rectangle of a specified window.
func MonitorFromWindow(hwnd windows.Handle, dwFlags uint32) windows.Handle {
	res := procMonitorFromWindow.Call(uintptr(hwnd), uintptr(dwFlags))
	return windows.Handle(res.R1)
}

// GetMonitorInfo retrieves information about a display monitor.
func GetMonitorInfo(hMonitor windows.Handle, lpmi *MONITORINFO) WinResult {
	if lpmi == nil {
		return WinResult{R1: 0, Err: windows.ERROR_INVALID_PARAMETER}
	}

	// Windows requires CbSize to be pre-set to sizeof(MONITORINFO)
	shouldBe := uint32(unsafe.Sizeof(*lpmi))
	now := lpmi.CbSize
	if now != 0 && now != shouldBe {
		GetBugLogger().Warn(fmt.Sprintf("BUG: you passed a MONITORINFO struct with wrongly set CbSize! Should be: mi.CbSize = uint32(unsafe.Sizeof(mi)) not %d", now))
		//continue tho
	}
	lpmi.CbSize = shouldBe
	return procGetMonitorInfo.Call(uintptr(hMonitor), uintptr(unsafe.Pointer(lpmi)))
}

// NtSetInformationProcess sets configuration information for a specified process
// using the low-level NT Native API.
//
// Because this function is polymorphic, the expected payload structure and size
// depend entirely on the provided processInformationClass enum. The caller must supply
// a valid pointer to the matching payload structure and its exact size in bytes.
//
// On success, it returns an NTSTATUS success code wrapped in a WinResult (check with .Failed()).
// On failure, it returns the NTSTATUS error code.
//
// Parameters:
//   - hProcess: A handle to the target process.
//   - processInformationClass: An NT core information class enum (e.g., ProcessIoPriority).
//   - processInformation: A pointer to the structure or variable containing the new settings.
//   - processInformationLength: The exact size in bytes of the payload data.
func NtSetInformationProcess(hProcess windows.Handle, processInformationClass uint32, processInformation unsafe.Pointer, processInformationLength uintptr) WinResult {
	// Optional runtime check (though usually redundant if callers use unsafe.Sizeof)
	if processInformationLength > math.MaxUint32 {
		panic2(fmt.Sprintf("BUG: NtSetInformationProcess:processInformationLength %d exceeds max uint32 size of %d", processInformationLength, math.MaxUint32))
	}
	return procNtSetInformationProcess.Call(
		uintptr(hProcess),
		uintptr(processInformationClass),
		uintptr(processInformation),
		processInformationLength,
	)
}

// SendMessage sends the specified message to a window or windows.
//
// if you pass Go pointers as wParam or lParam, use uintptr(unsafe.Pointer(THEPOINTER)) as the arg
// the conversion to uintptr MUST happen inline when calling this function.
//
// # The return is LRESULT as uintptr
//
// What LRESULT Means
// The value depends completely on the specific message (msg) you sent to the window. The window's procedure (WndProc) decides what to return based on that message:
//
//	For some messages (e.g., WM_CREATE): It returns 0 if the window was successfully created, or -1 if creation failed.
//	For query messages (e.g., WM_GETTEXT): It returns the number of characters copied into a buffer.
//	For command messages (e.g., WM_COMMAND): It often returns 0 if the message was processed, or a custom value defined by the application.
//	For many notification messages: The return value is completely ignored, and it just returns 0.
//
//go:uintptrescapes
func SendMessage(hwnd windows.Handle, msg uint32, wParam, lParam uintptr) uintptr {
	res := procSendMessage.Call(
		uintptr(hwnd),
		uintptr(msg),
		wParam,
		lParam,
	)
	return res.R1
}

const (
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

// ShellNotifyIcon sends a message to the taskbar's status area.
func ShellNotifyIcon(dwMessage uint32, lpData *NOTIFYICONDATA) WinResult {
	return procShellNotifyIcon.Call(
		uintptr(dwMessage),
		uintptr(unsafe.Pointer(lpData)),
	)
}

// SendMessageTimeout sends the specified message to one or more windows.
//
//go:uintptrescapes
func SendMessageTimeout(hwnd windows.Handle, msg uint32, wParam, lParam uintptr, fuFlags, uTimeout uint32, lpdwResult *uintptr) WinResult {
	return procSendMessageTimeout.Call(
		uintptr(hwnd),
		uintptr(msg),
		wParam,
		lParam,
		uintptr(fuFlags),
		uintptr(uTimeout),
		uintptr(unsafe.Pointer(lpdwResult)),
	)
}

// RegisterClassEx registers a window class for subsequent use in calls to CreateWindowEx.
func RegisterClassEx(wcx *WNDCLASSEX) WinResult {
	return procRegisterClassEx.Call(uintptr(unsafe.Pointer(wcx)))
}

const (
	WS_EX_LAYERED     = 0x00080000
	WS_EX_TRANSPARENT = 0x00000020
	LWA_COLORKEY      = 0x00000001
	LWA_ALPHA         = 0x00000002
)

const (
	WS_CHILD         = 0x40000000
	WS_POPUP         = 0x80000000
	WS_CAPTION       = 0x00C00000
	WS_EX_NOACTIVATE = 0x08000000
	WS_EX_TOPMOST    = 0x00000008
	WS_EX_TOOLWINDOW = 0x00000080
)

// CreateWindowEx creates an overlapped, pop-up, or child window with an extended window style.
func CreateWindowEx(
	dwExStyle uint32,
	lpClassName *uint16,
	lpWindowName *uint16,
	dwStyle uint32,
	x, y, nWidth, nHeight int32,
	hWndParent windows.Handle,
	hMenu windows.Handle,
	hInstance windows.Handle,
	lpParam unsafe.Pointer,
) WinResult {
	res := procCreateWindowEx.Call(
		uintptr(dwExStyle),
		uintptr(unsafe.Pointer(lpClassName)),
		uintptr(unsafe.Pointer(lpWindowName)),
		uintptr(dwStyle),
		// #nosec G115
		uintptr(x),
		// #nosec G115
		uintptr(y),
		// #nosec G115
		uintptr(nWidth),
		// #nosec G115
		uintptr(nHeight),
		uintptr(hWndParent),
		uintptr(hMenu),
		uintptr(hInstance),
		uintptr(lpParam),
	)
	return res //windows.Handle(res.R1)
}

// GetModuleHandle retrieves a module handle for the specified module.
// "If this parameter is NULL, GetModuleHandle returns a handle to the file used to create the calling process (.exe file)."
// so nil lpModuleName will get you the handle to the current process
//
// GetModuleHandle(nil) returns an HMODULE: This is the base memory address where your .exe file is loaded
//
//	into your process's address space. You use this when you need to load resources (like icons or dialogs)
//
// embedded in your binary or query information about the executable itself.
func GetModuleHandle(lpModuleName *uint16) WinResult {
	res := procGetModuleHandle.Call(uintptr(unsafe.Pointer(lpModuleName)))
	return res //windows.Handle(res.R1)
}

// CreatePopupMenuRaw exposes the WinResult so TrackPopupMenu logic can catch creation failures
func CreatePopupMenuRaw() WinResult {
	return procCreatePopupMenu.Call()
}

// CreatePopupMenu creates a drop-down menu, submenu, or shortcut menu.
func CreatePopupMenu() (windows.Handle, WinResult) {
	res := CreatePopupMenuRaw()
	return windows.Handle(res.R1), res
}

// AppendMenu appends a new item to the end of the specified menu.
//
//go:uintptrescapes
func AppendMenu(hMenu windows.Handle, uFlags uint32, uIDNewItem uintptr, lpNewItem *uint16) WinResult {
	return procAppendMenu.Call(
		uintptr(hMenu),
		uintptr(uFlags),
		uIDNewItem,
		uintptr(unsafe.Pointer(lpNewItem)),
	)
}

const TPM_RETURNCMD = 0x0100

// // TrackPopupMenu displays a shortcut menu at the specified screen coordinates
// // and tracks item selection.
// //
// // Returns the command ID selected by the user (or non-zero success status depending
// // on uFlags). If the user cancels the menu (e.g., clicks away or presses Esc),
// // WinResult.R1 will be 0 and WinResult.Err will be nil.
// // If an actual system error occurs, WinResult.Err will contain the error.
// func TrackPopupMenu(hMenu windows.Handle, uFlags uint32, x, y int32, hwnd windows.Handle, prcRect *RECT) WinResult {
// 	return procTrackPopupMenu.Call(
// 		uintptr(hMenu),
// 		uintptr(uFlags),
// 		// #nosec G115
// 		uintptr(x),
// 		// #nosec G115
// 		uintptr(y),
// 		0, // Reserved, must be 0
// 		uintptr(hwnd),
// 		uintptr(unsafe.Pointer(prcRect)),
// 	)
// }

// Bound to procTrackPopupMenu with CheckBool

// TrackPopupMenuWithoutReturnCmd displays a shortcut menu and posts WM_COMMAND messages
// to the specified window when an item is selected.
//
// Returns a successful WinResult if the menu was displayed and tracked,
// or an error if the API call failed.
func TrackPopupMenuWithoutReturnCmd(
	hMenu windows.Handle,
	uFlags uint32,
	x, y int32,
	hwnd windows.Handle,
	prcRect *RECT,
) WinResult {
	if uFlags&TPM_RETURNCMD != 0 {
		panic2("BUG: you called TrackPopupMenuWithoutReturnCmd with TPM_RETURNCMD!")
		panic(nil)
	}
	// Ensure TPM_RETURNCMD is NOT set
	uFlags &^= TPM_RETURNCMD

	return procTrackPopupMenuBool.Call(
		uintptr(hMenu),
		uintptr(uFlags),
		// #nosec G115
		uintptr(x),
		// #nosec G115
		uintptr(y),
		0, // Reserved, must be 0
		uintptr(hwnd),
		uintptr(unsafe.Pointer(prcRect)),
	)
}

// Bound to procTrackPopupMenu with CheckNone

// TrackPopupMenuCmd displays a shortcut menu and synchronously returns the ID
// of the item selected by the user.
//
// Automatically includes TPM_RETURNCMD in uFlags. Returns 0 if the user dismissed
// the menu without making a selection (or if a system error occurred).
func TrackPopupMenuCmd(
	hMenu windows.Handle,
	uFlags uint32,
	x, y int32,
	hwnd windows.Handle,
	prcRect *RECT,
) (uint32, WinResult) {
	// Automatically force TPM_RETURNCMD
	uFlags |= TPM_RETURNCMD

	res := procTrackPopupMenuCmd.Call(
		uintptr(hMenu),
		uintptr(uFlags),
		// #nosec G115
		uintptr(x),
		// #nosec G115
		uintptr(y),
		0, // Reserved, must be 0
		uintptr(hwnd),
		uintptr(unsafe.Pointer(prcRect)),
	)

	if res.R1 > math.MaxUint32 {
		panic2(fmt.Sprintf("BUG: TrackPopupMenuCmd returned R1 %d exceeding MaxUint32 %d", res.R1, math.MaxUint32))
	}
	return uint32(res.R1), res
}

// DestroyMenu destroys the specified menu and frees allocated memory.
func DestroyMenu(hMenu windows.Handle) WinResult {
	return procDestroyMenu.Call(uintptr(hMenu))
	//return res.R1 != 0
}

// SetProcessDpiAwarenessContext sets the current process DPI awareness context.
//
// Modern API (Win10 1607+)
//
//go:uintptrescapes
func SetProcessDpiAwarenessContext(value uintptr) WinResult {
	return procSetProcessDpiAwarenessContext.Call(value)
}

// SetProcessDpiAwareness sets the process-default DPI awareness level.
//
// Fallback: Windows 8.1+ shcore API
func SetProcessDpiAwareness(value uint32) WinResult {
	return procSetProcessDpiAwareness.Call(uintptr(value))
}

const (
	// DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = (HANDLE)-4
	DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = ^uintptr(3)

	// PROCESS_PER_MONITOR_DPI_AWARE = 2
	PROCESS_PER_MONITOR_DPI_AWARE = 2
)

// InitDPIAwareness If you call it after window creation(ie. CreateWindowEx), it does nothing.
func InitDPIAwareness(loggerFunction func(format string, args ...any)) {
	// Try the modern API first (Win10 1607+).
	if procSetProcessDpiAwarenessContext.Find() == nil {
		// res1 := procSetProcessDpiAwarenessContext.Call(
		res1 := SetProcessDpiAwarenessContext(
			DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2,
		)
		if res1.Succeeded() {
			return
		}
		// ERROR_ACCESS_DENIED means DPI awareness was already locked before main()
		// ran — most likely by an embedded application manifest in a .syso file.
		// This is expected and benign; log informationally and skip the fallback.
		if res1.ErrIs(windows.ERROR_ACCESS_DENIED) {
			if loggerFunction != nil {
				loggerFunction("DPI awareness already set before main() (application manifest?); runtime initialization skipped.")
			}
			return
		}
		if loggerFunction != nil {
			loggerFunction("SetProcessDpiAwarenessContext failed (not manifest-locked), err:'%v'; trying shcore fallback.", res1.Err)
		}
	}

	// Fallback: Windows 8.1+ shcore API.
	if procSetProcessDpiAwareness.Find() == nil {
		//res2 := procSetProcessDpiAwareness.Call(PROCESS_PER_MONITOR_DPI_AWARE)
		res2 := SetProcessDpiAwareness(PROCESS_PER_MONITOR_DPI_AWARE)
		if res2.Failed() {
			// uint32, not uintptr: on 64-bit Windows the upper 32 bits of a
			// 32-bit HRESULT return value can be sign-extension garbage
			// (the exact same reasoning CheckHRESULT elsewhere in this
			// codebase already applies via int32(r1) < 0) -- comparing the
			// full-width res2.R1 directly against this constant would
			// silently miss the match if 0x80070005 came back sign-extended
			// as 0xFFFFFFFF80070005.
			const hresultEAccessDenied uint32 = 0x80070005
			//The hresultEAccessDenied constant is intentionally local to the function rather than a package-level constant, since it's a HRESULT interpretation of an API that's an exception to the project's general use of CheckBool/CheckErrno.
			if uint32(res2.R1) == hresultEAccessDenied { // #nosec: G115
				if loggerFunction != nil {
					loggerFunction("DPI awareness (shcore fallback) already set before main() (application manifest?); skipping.")
				}
				return
			}
			if loggerFunction != nil {
				loggerFunction("SetProcessDpiAwareness PROCESS_PER_MONITOR_DPI_AWARE failed, err:'%v'", res2.Err)
			}
		}
	}
}

// AttachThreadInput attaches or detaches the input processing mechanism of one thread to another.
//
// if fAttach is false it means detach.
func AttachThreadInput(idAttach, idAttachTo uint32, fAttach bool) WinResult {
	var attach uintptr
	if fAttach {
		attach = 1
	}
	return procAttachThreadInput.Call(
		uintptr(idAttach),
		uintptr(idAttachTo),
		attach,
	)
}

const IDI_APPLICATION = 32512

// LoadIcon is a low-level wrapper around User32 LoadIconW that accepts a pre-allocated
// null-terminated UTF-16 string pointer (*uint16).
//
// WARNING: lpIconName MUST be a valid memory pointer to a UTF-16 string (e.g., created via
// windows.UTF16PtrFromString). Do NOT pass integer resource IDs cast to pointers here, as doing so
// will trigger runtime panics under Go's '-d=checkptr' validation. For numeric IDs, use LoadIconByID.
//
// Parameters:
//   - hInstance: Handle to the module whose executable file contains the icon resource.
//   - lpIconName: Pointer to a null-terminated UTF-16 string specifying the resource name.
//
// Return values:
//   - windows.Handle: Handle to the loaded icon (HICON).
//   - WinResult: Call status and error details if the call fails.
//
// "LoadIconW defaults to loading the standard icon size (SM_CXICON / SM_CYICON, which is usually 32x32). However, the system tray uses the small icon size (SM_CXSMICON / SM_CYSMICON, usually 16x16)." - Gemini 3.1 Pro
// so use LoadImage instead. "LoadImageW fixes this by letting you explicitly ask for the 16x16 size, which makes Windows pull the correct sub-image directly from your multi-resolution resource."
func LoadIcon(hInstance windows.Handle, lpIconName *uint16) (windows.Handle, WinResult) {
	res := procLoadIcon.Call(
		uintptr(hInstance),
		uintptr(unsafe.Pointer(lpIconName)),
	)
	return windows.Handle(res.R1), res
}

// LoadIconByName loads an icon resource using a standard Go UTF-8 string name.
//
// It automatically converts the string into a null-terminated UTF-16 pointer and invokes LoadIcon.
//
// Parameters:
//   - hInstance: Handle to the module whose executable file contains the icon resource.
//   - name: The named identifier of the icon resource in the PE executable resource table.
//
// Return values:
//   - windows.Handle: Handle to the loaded icon (HICON).
//   - WinResult: Call status and error details if string conversion or Win32 loading fails.
func LoadIconByName(hInstance windows.Handle, name string) (windows.Handle, WinResult) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, WinResult{Err: err}
	}
	return LoadIcon(hInstance, namePtr)
}

// LoadIconByID loads an icon resource specified by a numeric integer ID.
//
// Parameters:
//   - hInstance: Handle to the module whose executable file contains the icon resource.
//     Pass 0 (NULL) to load standard built-in Windows system icons (e.g., wincoe.IDI_APPLICATION).
//     Pass your application's module handle (selfHInstance) to load custom embedded resources.
//   - resourceID: The 16-bit numeric identifier of the icon resource (e.g., 1 or IDI_APPLICATION).
//
// Return values:
//   - windows.Handle: Handle to the loaded icon (HICON).
//   - WinResult: Call status and error details if the call fails.
func LoadIconByID(hInstance windows.Handle, resourceID uint16) (windows.Handle, WinResult) {
	res := procLoadIcon.Call(
		uintptr(hInstance),
		uintptr(resourceID),
	)
	return windows.Handle(res.R1), res
}

const (
	IMAGE_BITMAP = 0
	IMAGE_ICON   = 1
	IMAGE_CURSOR = 2

	LR_DEFAULTCOLOR     = 0x00000000
	LR_MONOCHROME       = 0x00000001
	LR_COLOR            = 0x00000002
	LR_COPYRETURNORG    = 0x00000004
	LR_COPYDELETEORG    = 0x00000008
	LR_LOADFROMFILE     = 0x00000010
	LR_LOADTRANSPARENT  = 0x00000020
	LR_DEFAULTSIZE      = 0x00000040
	LR_VGACOLOR         = 0x00000080
	LR_LOADMAP3DCOLORS  = 0x00001000
	LR_CREATEDIBSECTION = 0x00002000
	LR_COPYFROMRESOURCE = 0x00004000
	LR_SHARED           = 0x00008000

	SM_CXICON   = 11
	SM_CYICON   = 12
	SM_CXSMICON = 49
	SM_CYSMICON = 50
)

// LoadImageByID loads an image (icon, cursor, or bitmap) using a numeric integer ID.
//
// Parameters:
//   - hInstance: Handle to the module whose executable file contains the resource.
//   - resourceID: The 16-bit numeric identifier of the resource.
//   - uType: The type of image to be loaded (e.g., wincoe.IMAGE_ICON).
//   - cx, cy: The desired width and height in pixels.
//   - fuLoad: Load flags (e.g., wincoe.LR_SHARED).
//
// Return values:
//   - windows.Handle: Handle to the loaded image.
//   - WinResult: Call status and error details if the call fails.
func LoadImageByID(hInstance windows.Handle, resourceID uint16, uType uint32, cx, cy int32, fuLoad uint32) (windows.Handle, WinResult) {
	res := procLoadImageW.Call(
		uintptr(hInstance),
		uintptr(resourceID),
		uintptr(uType),
		// #nosec G115 -- safe: Win32 dimensions are sign-extended from int32 into uintptr
		uintptr(cx),
		// #nosec G115 -- safe: Win32 dimensions are sign-extended from int32 into uintptr
		uintptr(cy),
		uintptr(fuLoad),
	)
	return windows.Handle(res.R1), res
}

// Standard system cursor resource IDs for LoadCursorW(NULL, MAKEINTRESOURCE(id)).
// These are shared OS cursors owned by USER32 — never DestroyCursor or CloseHandle them.
const (
	IDC_ARROW       uintptr = 32512
	IDC_IBEAM       uintptr = 32513
	IDC_WAIT        uintptr = 32514
	IDC_CROSS       uintptr = 32515
	IDC_UPARROW     uintptr = 32516
	IDC_SIZENWSE    uintptr = 32642 // diagonal \ (top-left / bottom-right)
	IDC_SIZENESW    uintptr = 32643 // diagonal / (top-right / bottom-left)
	IDC_SIZEWE      uintptr = 32644 // horizontal
	IDC_SIZENS      uintptr = 32645 // vertical
	IDC_SIZEALL     uintptr = 32646 // four-way (move / omnidirectional)
	IDC_NO          uintptr = 32648
	IDC_HAND        uintptr = 32649
	IDC_APPSTARTING uintptr = 32650
	IDC_HELP        uintptr = 32651
)

// LoadCursor loads a cursor resource. Pass hInstance=0 and one of the IDC_*
// constants to obtain a shared system cursor.
//
// Ownership: cursors loaded from a system resource (hInstance=0 + IDC_*) or
// from a module via LoadCursorW are shared and owned by the system. Do not
// pass them to DestroyCursor, and do not pass them to CloseHandle either —
// HCURSOR is a USER object, not a kernel HANDLE. DestroyCursor is only valid
// for cursors you created yourself (CreateCursor / LoadCursorFromFile /
// LoadImage without LR_SHARED).
func LoadCursor(hInstance windows.Handle, resourceID uintptr) (windows.Handle, WinResult) {
	res := procLoadCursorW.Call(uintptr(hInstance), resourceID)
	return windows.Handle(res.R1), res
}

// SetCursor sets the cursor shape for the calling thread's input queue.
//
// Returns the handle of the previous cursor, or 0 if there was none.
// SetCursor does not set GetLastError and treats a NULL previous cursor as a
// normal outcome, so there is no WinResult — same pattern as SetCapture /
// GetCapture. Callers that only need to force a shape (and do not restore the
// previous one) can ignore the return value.
func SetCursor(hCursor windows.Handle) (prevCursor windows.Handle) {
	res := procSetCursor.Call(uintptr(hCursor))
	return windows.Handle(res.R1)
}

// UnregisterClassW unregisters a window class.
func UnregisterClassW(lpClassName *uint16, hInstance windows.Handle) WinResult {
	return procUnregisterClassW.Call(
		uintptr(unsafe.Pointer(lpClassName)),
		uintptr(hInstance),
	)
}

const (
	// IdlePriorityClass indicates a process that runs only when the system is idle.
	IdlePriorityClass uint32 = 0x00000040

	// BelowNormalPriorityClass indicates a process with priority above Idle, but below Normal.
	BelowNormalPriorityClass uint32 = 0x00004000

	// NormalPriorityClass indicates a standard priority process (the default).
	NormalPriorityClass uint32 = 0x00000020

	// AboveNormalPriorityClass indicates a process with priority above Normal, but below High.
	AboveNormalPriorityClass uint32 = 0x00008000

	// HighPriorityClass indicates a process that performs time-critical tasks.
	HighPriorityClass uint32 = 0x00000080

	// RealtimePriorityClass indicates a process that has the highest possible priority.
	// Use with extreme caution as it can starve system threads.
	RealtimePriorityClass uint32 = 0x00000100
)

// SetPriorityClass sets the priority class for the specified process.
func SetPriorityClass(hProcess windows.Handle, dwPriorityClass uint32) WinResult {
	return procSetPriorityClass.Call(
		uintptr(hProcess),
		uintptr(dwPriorityClass),
	)
}

// GetPriorityClass retrieves the priority class for the specified process.
//
// It wraps the Win32 GetPriorityClass API. On success, it returns the process
// priority class constant (e.g., NormalPriorityClass, HighPriorityClass) and a
// successful WinResult. On failure, it returns 0 and a WinResult containing the
// system error code captured via GetLastError() as WinResult.Err
//
// Parameters:
//   - hProcess: A handle to the process. To query the priority of your own
//     running application, pass windows.CurrentProcess() pseudohandle instead of 0.
func GetPriorityClass(hProcess windows.Handle) (uint32, WinResult) {
	res := procGetPriorityClass.Call(uintptr(hProcess))
	if res.R1 > math.MaxUint32 {
		panic2(fmt.Sprintf("BUG: procGetPriorityClass returned an R1 %d which is bigger than MaxUint32 which is %d, thus wouldn't fit in the returned uint32 type", res.R1, math.MaxUint32))
		panic(nil)
	}

	return uint32(res.R1), res
}

// GetCurrentProcess retrieves a pseudo handle for the current process which is CURRENT_PROCESS_PSEUDO_HANDLE
func GetCurrentProcess() windows.Handle {
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
		panic2(fmt.Sprintf("BUG: procGetCurrentProcess should've returned a fixed handle 0x%X but it returned 0x%X, err: %v, callStatus: %v",
			CURRENT_PROCESS_PSEUDO_HANDLE,
			hProcLocal,
			res1.Err, res1.CallStatus,
		))
		panic(nil)
	}
	return windows.Handle(hProcLocal)
}

// GetCurrentThread retrieves a pseudo handle for the calling thread.
func GetCurrentThread() windows.Handle {
	res := procGetCurrentThread.Call()
	if res.Failed() { // paranoid check
		panic2(fmt.Sprintf("BUG: GetCurrentThread should always return same value, res:%v", res))
		panic(nil)
	}
	return windows.Handle(res.R1)
}

const (
	// ThreadPriorityIdle sets a base priority of 1 for standard processes,
	// or 16 for REALTIME_PRIORITY_CLASS processes.
	ThreadPriorityIdle int32 = -15

	// ThreadPriorityLowest sets the thread priority 2 points below the process priority class.
	ThreadPriorityLowest int32 = -2

	// ThreadPriorityBelowNormal sets the thread priority 1 point below the process priority class.
	ThreadPriorityBelowNormal int32 = -1

	// ThreadPriorityNormal sets the thread priority equal to the process priority class (the default for all new threads).
	ThreadPriorityNormal int32 = 0

	// ThreadPriorityAboveNormal sets the thread priority 1 point above the process priority class.
	ThreadPriorityAboveNormal int32 = 1

	// ThreadPriorityHighest sets the thread priority 2 points above the process priority class.
	ThreadPriorityHighest int32 = 2

	// ThreadPriorityTimeCritical sets a base priority of 15 for standard processes,
	// or 31 for REALTIME_PRIORITY_CLASS processes.
	ThreadPriorityTimeCritical int32 = 15
)

// SetThreadPriority sets the priority value for the specified thread.
func SetThreadPriority(hThread windows.Handle, nPriority int32) WinResult {
	return procSetThreadPriority.Call(
		uintptr(hThread),
		// #nosec G115
		uintptr(nPriority),
	)
}

// GetThreadPriority retrieves the priority value for the specified thread.
func GetThreadPriority(hThread windows.Handle) (int32, WinResult) {
	res := procGetThreadPriority.Call(uintptr(hThread))
	// if res.Failed() { // paranoid check
	// 	panic2(fmt.Sprintf("BUG: GetThreadPriority should always return same value, res:%v", res))
	// 	panic(nil)
	// }
	// #nosec G115
	return int32(res.R1), res
}

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

// SetProcessInformation sets information for a specified process.
//
// It wraps the Win32 SetProcessInformation API. Because this function accepts
// different structure types depending on the processInformationClass, the caller
// must supply a pointer to the appropriate configuration struct and its size in bytes.
//
// On success, it returns a successful WinResult. On failure, it returns a WinResult
// containing the system error code captured via GetLastError().
//
// Parameters:
//   - hProcess: A handle to the process (must have PROCESS_SET_INFORMATION access).
//   - processInformationClass: An enum specifying the type of information being set
//     (e.g., PROCESS_MEMORY_PRIORITY).
//   - processInformation: A pointer to the specific configuration structure.
//   - processInformationSize: The exact size in bytes of the structure being passed.
//
// Depending on the processInformationClass you pass as the second argument, Windows expects completely different payload structures with entirely different byte sizes:
//   - ProcessMemoryPriority expects a MEMORY_PRIORITY_INFORMATION struct.
//   - ProcessPowerThrottling expects a PROCESS_POWER_THROTTLING_STATE struct.
//   - ProcessLeapSecondInfo expects a PROCESS_LEAP_SECOND_INFO struct.
//   - ProcessOverrideSubsequentPrefetchParameter expects an OVERRIDE_PREFETCH_PARAMETER struct.
func SetProcessInformation(hProcess windows.Handle, processInformationClass uint32, processInformation unsafe.Pointer, processInformationSize uintptr) WinResult {
	if processInformationSize > math.MaxUint32 {
		panic2(fmt.Sprintf("BUG: SetProcessInformation:processInformationSize %d exceeds max uint32 size of %d", processInformationSize, math.MaxUint32))
	}
	return procSetProcessInformation.Call(
		uintptr(hProcess),
		uintptr(processInformationClass),
		uintptr(processInformation),
		processInformationSize,
	)
}

// SetProcessWorkingSetSize sets the minimum and maximum working set sizes for the specified process.
// The Windows API defines SIZE_T as a pointer-sized integer so it can scale automatically between 32-bit and 64-bit architectures.
//
//go:uintptrescapes
func SetProcessWorkingSetSize(hProcess windows.Handle, dwMinimumWorkingSetSize, dwMaximumWorkingSetSize uintptr) WinResult {
	return procSetProcessWorkingSetSize.Call(
		uintptr(hProcess),
		dwMinimumWorkingSetSize,
		dwMaximumWorkingSetSize,
	)
}

const (
	WS_DISABLED = 0x08000000
	WS_VISIBLE  = 0x10000000
)

// GetWindowLongPtrW retrieves information about the specified window.
func GetWindowLongPtrW(hwnd windows.Handle, nIndex int32) WinResult { //uintptr {
	//as per https://github.com/golang/go/issues/41220 there's no need to call setlasterror because it happens automatically!
	res := procGetWindowLongPtrW.Call( // it's CheckNullWithLastError
		uintptr(hwnd),
		// #nosec G115
		uintptr(nIndex),
	) //Go executes the C code and atomically grabs LastError before anything else can touch it. as the 3rd arg well as res1.CallStatus !
	return res //res.R1
}

// CreateMutex creates or opens a named or unnamed mutex object.
//
// bInitialOwner should be true aka "You must acquire ownership of the mutex before you can release it." otherwise you cannot call ReleaseMutex on it, unless you use WaitForSingleObject (currently not defined in wincoe!)
func CreateMutex(lpMutexAttributes unsafe.Pointer, bInitialOwner bool, lpName *uint16) (windows.Handle, WinResult) {
	var initialOwner uintptr
	if bInitialOwner {
		initialOwner = 1
	}
	res := procCreateMutex.Call(
		uintptr(lpMutexAttributes),
		initialOwner,
		uintptr(unsafe.Pointer(lpName)),
	)
	return windows.Handle(res.R1), res
}

// ReleaseMutex releases ownership of the specified mutex object.
func ReleaseMutex(hMutex windows.Handle) WinResult {
	return procReleaseMutex.Call(uintptr(hMutex))
}

// CloseHandle closes an open object handle.
func CloseHandle(hObject windows.Handle) bool {
	res := procCloseHandle.Call(uintptr(hObject))
	return res.R1 != 0
}

type PSAPI_WORKING_SET_EX_BLOCK struct {
	Flags uintptr
}

type PSAPI_WORKING_SET_EX_INFORMATION struct {
	VirtualAddress    uintptr
	VirtualAttributes PSAPI_WORKING_SET_EX_BLOCK
}

func (b *PSAPI_WORKING_SET_EX_BLOCK) IsValid() bool {
	// Bit 0 of VirtualAttributes (the 'Valid' bit) indicates if the page
	// is currently resident in physical RAM.
	return b.Flags&1 == 1 // Bit 0 is the 'Valid' (resident) bit
}

// QueryWorkingSetExRaw is the low-level wrapper for the Win32 QueryWorkingSetEx API.
// It accepts a direct pointer to a buffer of PSAPI_WORKING_SET_EX_INFORMATION structures
// and the total size of the buffer in bytes.
//
// On success, it returns a successful WinResult. On failure, it returns a WinResult
// containing the system error code captured via GetLastError() seen as WinResult.Err
func QueryWorkingSetExRaw(hProcess windows.Handle, pv *PSAPI_WORKING_SET_EX_INFORMATION, cb uint32) WinResult {
	return procQueryWorkingSetEx.Call(
		uintptr(hProcess),
		uintptr(unsafe.Pointer(pv)),
		uintptr(cb),
	)
}

// QueryWorkingSetEx retrieves extended information about pages in the virtual address
// space of a specified process.
//
// It is a slice-safe wrapper around QueryWorkingSetExRaw that automatically calculates
// the total buffer size in bytes from the length of the slice. Passing an empty slice
// triggers a panic to prevent unsafe memory operations.
//
// On success, it returns a successful WinResult. On failure, it returns a WinResult
// containing the system error code captured via GetLastError().
func QueryWorkingSetEx(hProcess windows.Handle, entries []PSAPI_WORKING_SET_EX_INFORMATION) WinResult {
	if len(entries) == 0 {
		//The len(entries) == 0 check is crucial: If you didn't check for a zero-length slice and tried to do &entries[0], Go would throw an index-out-of-range panic anyway. Catching it explicitly with your panic2 helper makes the bug message much clearer for whoever calls it.
		panic2("BUG: you passed a slice of len 0 to QueryWorkingSetEx")
		// return WinResult{}
	}

	l := len(entries)
	if l > math.MaxUint32 {
		panic2(fmt.Sprintf("BUG: slice length %d exceeds maximum uint32 size of %d", l, math.MaxUint32))
	}

	// Automatically calculate total size in bytes for the entire slice
	cb := uint32(l) * uint32(unsafe.Sizeof(entries[0]))

	res := QueryWorkingSetExRaw(
		hProcess,
		//uintptr(unsafe.Pointer(&entries[0])),
		&entries[0],
		cb,
	)

	return res
}

const (
	TOKEN_ADJUST_PRIVILEGES uint32 = 0x0020
	TOKEN_QUERY             uint32 = 0x0008
	SE_PRIVILEGE_ENABLED           = 0x00000002
	SE_INC_WORKING_SET_NAME        = "SeIncreaseWorkingSetPrivilege" // not: "SeIncrementWorkingSetPrivilege"
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

// OpenProcessToken opens the access token associated with a process.
func OpenProcessToken(processHandle windows.Handle, desiredAccess uint32, tokenHandle *windows.Token) WinResult {
	return procOpenProcessToken.Call(
		uintptr(processHandle),
		uintptr(desiredAccess),
		uintptr(unsafe.Pointer(tokenHandle)),
	)
}

// LookupPrivilegeValue retrieves the locally unique identifier (LUID) for a specified privilege name.
func LookupPrivilegeValue(lpSystemName, lpName *uint16, lpLuid *LUID) WinResult {
	return procLookupPrivilegeValue.Call(
		uintptr(unsafe.Pointer(lpSystemName)),
		uintptr(unsafe.Pointer(lpName)),
		uintptr(unsafe.Pointer(lpLuid)),
	)
}

// AdjustTokenPrivileges enables or disables privileges in the specified access token.
func AdjustTokenPrivileges(
	tokenHandle windows.Token,
	disableAllPrivileges bool,
	newState *TOKEN_PRIVILEGES,
	bufferLength uint32,
	previousState *TOKEN_PRIVILEGES,
	returnLength *uint32,
) WinResult {
	var disableAll uintptr
	if disableAllPrivileges {
		disableAll = 1
	}
	return procAdjustTokenPrivileges.Call(
		uintptr(tokenHandle),
		disableAll,
		uintptr(unsafe.Pointer(newState)),
		uintptr(bufferLength),
		uintptr(unsafe.Pointer(previousState)),
		uintptr(unsafe.Pointer(returnLength)),
	)
}

// GetClassNameRaw returns the full WinResult so callers can check .Failed() and .R1
func GetClassNameRaw(hwnd windows.Handle, lpClassName *uint16, nMaxCount int32) WinResult {
	return procGetClassName.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(lpClassName)),
		// #nosec G115
		uintptr(nMaxCount),
	)
}

// // GetClassName retrieves the name of the class to which the specified window belongs.
// func GetClassName(hwnd windows.Handle, lpClassName *uint16, nMaxCount int32) int32 {
// 	res := GetClassNameRaw(hwnd, lpClassName, nMaxCount)
// 	// #nosec G115
// 	return int32(res.R1)
// }

// GetClassName retrieves the name of the class to which the specified window belongs.
//
// R1 is the length of the string copied (0 means failure and WinResult.Err is set)
func GetClassName(hwnd windows.Handle) (string, WinResult) {
	//"The maximum length for a window class name in the Windows API is 256 characters (including the null-terminating character, or 255 usable characters for the name itself, though 256 is universally used for buffer allocation)."
	buf := make([]uint16, 256)
	//res1 := procGetClassName.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	res1 := GetClassNameRaw(hwnd, &buf[0],
		// #nosec G115
		int32(len(buf)),
	)
	//if ret == 0 {
	if res1.Failed() {
		return "", res1
	}
	// R1 is the length of the string copied (0 means failure)
	return windows.UTF16ToString(buf[:res1.R1]), res1
}

// InternalGetWindowTextRaw returns the full WinResult for empty-title vs true-failure checks
//
// This API does NOT send a message; it reads from kernel memory.
func InternalGetWindowTextRaw(hwnd windows.Handle, pString *uint16, cchMaxCount int32) WinResult {
	return procInternalGetWindowText.Call(
		uintptr(hwnd),
		uintptr(unsafe.Pointer(pString)),
		// #nosec G115
		uintptr(cchMaxCount),
	)
}

// InternalGetWindowText copies the text of the specified window's title bar into a buffer.
func InternalGetWindowText(hwnd windows.Handle, pString *uint16, cchMaxCount int32) int32 {
	res := InternalGetWindowTextRaw(hwnd, pString, cchMaxCount)
	// #nosec G115
	return int32(res.R1)
}

// GetConsoleWindow retrieves the window handle used by the console associated with the calling process.
func GetConsoleWindow() windows.HWND {
	res := procGetConsoleWindow.Call()
	return windows.HWND(res.R1)
}

// HasConsole reports whether this process currently has a console attached.
// Returns false for GUI-subsystem builds (-H=windowsgui), which never have
// one, and for any build (console or GUI subsystem) after a successful
// FreeConsole call. This is the single source of truth callers should use
// to decide whether console-dependent work (VT processing, interactive
// prompts, the colored console log handler) is worth attempting at all.
func HasConsole() bool {
	return GetConsoleWindow() != 0
}

// FreeConsole detaches the calling process from its console, if any.
// After this returns successfully, HasConsole reports false, GetStdHandle
// for the standard streams returns invalid/NULL handles, and any further
// reads from os.Stdin or writes to os.Stdout/os.Stderr fail gracefully
// (returning an error) rather than blocking or panicking -- exactly the
// same behavior a process built with -H=windowsgui exhibits from the
// start. If this process was the sole one attached to its console, the
// console window itself closes as a side effect.
//
// Safe to call even when no console is attached (e.g. redundantly on a
// -H=windowsgui build); FreeConsole simply fails in that case. Callers
// should treat a failure as a warning, not fatal.
func FreeConsole() error {
	res := procFreeConsole.Call()
	if res.Failed() {
		return fmt.Errorf("FreeConsole failed: %w", res.Err)
	}
	return nil
}

// WinEventProc represents the callback signature required by Windows for SetWinEventHook.
type WinEventProc func(
	hWinEventHook windows.Handle,
	event uint32,
	hwnd windows.Handle,
	idObject, idChild int32,
	dwEventThread, dwmsEventTime uint32,
) uintptr

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

// SetWinEventHook sets an event hook function for a range of events.
//
// It wraps the Win32 SetWinEventHook API, returning a WinResult containing
// the event hook handle (HWINEVENTHOOK) in R1 upon success, or 0 on failure.
//
// Parameters:
//   - eventMin: The low value of the event range handled by the hook.
//   - eventMax: The high value of the event range handled by the hook.
//   - hmodWinEventProc: Handle to the DLL containing the hook function (or 0 if out-of-context).
//   - pfnWinEventProc: Pass the event hook function of type WinEventProc, windows.NewCallback(pfnWinEventProc) will be called internally.
//   - idProcess: Process ID to monitor (0 for all processes on the desktop).
//   - idThread: Thread ID to monitor (0 for all threads on the desktop).
//   - dwFlags: Flags specifying hook location and filtering options (e.g., WINEVENT_OUTOFCONTEXT).
//
// NOTE: Always pass top-level named functions to this API.
// Do NOT pass inline closures (e.g., anonymous func() values created dynamically inside a loop),
// because Go allocates a permanent assembly trampoline for each unique closure instance that
// is never garbage-collected. Passing dynamic closures will eventually exhaust Go's internal
// callback pool (~2,000 max) and panic the runtime. Named functions are cached and memory-safe.
//
// //go:uintptrescapes //this is not actually needed here because it's a pointer to a function but I used 'any' for pfnWinEventProc
func SetWinEventHook(
	eventMin, eventMax uint32,
	hmodWinEventProc windows.Handle,
	// pfnWinEventProc any, //bad for safety!
	pfnWinEventProc WinEventProc,
	idProcess, idThread, dwFlags uint32,
) (windows.Handle, WinResult) {
	res := procSetWinEventHook.Call(
		uintptr(eventMin),
		uintptr(eventMax),
		uintptr(hmodWinEventProc),
		//For a function like SetWinEventHook, Windows expects a raw function pointer (uintptr).
		// syscall.NewCallback bridges the gap between Go functions and Windows callbacks.
		windows.NewCallback(pfnWinEventProc),
		uintptr(idProcess),
		uintptr(idThread),
		uintptr(dwFlags),
	)
	return windows.Handle(res.R1), res
}

// UnhookWinEvent removes an event hook function created by SetWinEventHook.
func UnhookWinEvent(hWinEventHook windows.Handle) WinResult {
	res := procUnhookWinEvent.Call(uintptr(hWinEventHook))
	return res
}

const NOTIFY_FOR_THIS_SESSION = 0

// WTSRegisterSessionNotification registers the specified window to receive session change notifications.
func WTSRegisterSessionNotification(hwnd windows.Handle, dwFlags uint32) WinResult {
	return procWTSRegisterSessionNotification.Call(
		uintptr(hwnd),
		uintptr(dwFlags),
	)
}

// WTSUnRegisterSessionNotification unregisters the specified window from receiving session change notifications.
func WTSUnRegisterSessionNotification(hwnd windows.Handle) WinResult {
	return procWTSUnRegisterSessionNotification.Call(uintptr(hwnd))
}

const (
	WM_MOUSEMOVE   = 0x0200
	WM_LBUTTONDOWN = 0x0201
	WM_LBUTTONUP   = 0x0202
	WM_RBUTTONDOWN = 0x0204 //guessed
	WM_RBUTTONUP   = 0x0205 // even winxp would have this
	WM_CONTEXTMENU = 0x007B // winxp won't have this tho

	WM_NCLBUTTONDOWN = 0x00A1

	HTCAPTION = 2
)

const (
	WM_KEYDOWN    = 0x0100
	WM_KEYUP      = 0x0101
	WM_SYSKEYDOWN = 0x0104
	WM_SYSKEYUP   = 0x0105
)

/*
WM_DESTROY Breakdown

	Constant Value: 0x0002

	What triggers it: It is sent by the system to a window after the window has been removed from the screen, but before the child windows are destroyed.
	Specifically, calling procDestroyWindow.Call(hwnd) is what triggers the WM_DESTROY message to be sent to that hwnd's wndProc.

	The Flow: User clicks Exit (or Hook panics) → WM_CLOSE → DestroyWindow() → WM_DESTROY → PostQuitMessage().
*/
const WM_DESTROY = 0x0002

// Win32 message constants missing from x/sys/windows
const (
	WM_CLOSE = 0x0010

	WM_NULL = 0
	WM_USER = 0x0400
)

const (
	WM_QUERYENDSESSION = 0x0011
	WM_ENDSESSION      = 0x0016
)

const (
	WM_WTSSESSION_CHANGE = 0x02B1

	WTS_SESSION_LOCK   = 0x7
	WTS_SESSION_UNLOCK = 0x8
)

const (
	WM_SYSCOMMAND = 0x0112
	SC_MOVE       = 0xF010
)

const (
	PM_NOREMOVE = 0x0000
	PM_REMOVE   = 0x0001
	PM_NOYIELD  = 0x0002
)

const (
	GWL_STYLE   = -16 // We could use ^uintptr(15) to represent -16 (GWL_STYLE) to prevent Go constant overflow errors.
	GWL_EXSTYLE = -20
)

const SW_HIDE = 0

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

	VK_E      = 0x45
	VK_F      = 0x46
	VK_F12    = 0x7B // F12
	VK_ESCAPE = 0x1B
)

const WS_THICKFRAME = 0x00040000 // or WS_SIZEBOX which has same value (as per chatgpt 5.5)

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
	SMTO_NORMAL      = 0x0000
	SMTO_ABORTIFHUNG = 0x0002

	MF_STRING = 0x0000

	MF_GRAYED   = 0x00000001
	MF_DISABLED = 0x00000002
	MF_CHECKED  = 0x00000008
)

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

// --- Routing & Interface Structs ---

type MIB_IPFORWARDROW struct {
	ForwardDest      uint32
	ForwardMask      uint32
	ForwardPolicy    uint32
	ForwardNextHop   uint32
	ForwardIfIndex   uint32
	ForwardType      uint32
	ForwardProto     uint32
	ForwardAge       uint32
	ForwardNextHopAS uint32
	ForwardMetric1   uint32
	ForwardMetric2   uint32
	ForwardMetric3   uint32
	ForwardMetric4   uint32
	ForwardMetric5   uint32
}

type MIB_IFROW struct {
	WszName         [256]uint16
	Index           uint32
	Type            uint32
	Mtu             uint32
	Speed           uint32
	PhysAddrLen     uint32
	PhysAddr        [8]byte
	AdminStatus     uint32
	OperStatus      uint32
	LastChange      uint32
	InOctets        uint32
	InUcastPkts     uint32
	InNUcastPkts    uint32
	InDiscards      uint32
	InErrors        uint32
	InUnknownProtos uint32
	OutOctets       uint32
	OutUcastPkts    uint32
	OutNUcastPkts   uint32
	OutDiscards     uint32
	OutErrors       uint32
	OutQLen         uint32
	DescrLen        uint32
	Descr           [256]byte
}

type MIB_IPFORWARDTABLE struct {
	NumEntries uint32
	Table      [1]MIB_IPFORWARDROW // placeholder for dynamic allocation
}

type MIB_IPADDRROW struct {
	Addr      uint32
	Index     uint32
	Mask      uint32
	BCastAddr uint32
	ReasmSize uint32
	Unused1   uint16
	Unused2   uint16
}

type MIB_IPADDRTABLE struct {
	NumEntries uint32
	Table      [1]MIB_IPADDRROW // Anchor for the array
}

// --- Routing API Wrappers ---

// GetBestInterface retrieves the index of the interface that has the best route to the specified IPv4 address.
func GetBestInterface(dwDestAddr uint32, pdwBestIfIndex *uint32) WinResult {
	return procGetBestInterface.Call(uintptr(dwDestAddr), uintptr(unsafe.Pointer(pdwBestIfIndex)))
}

// GetIpForwardTable retrieves the IPv4 routing table.
func GetIpForwardTable(pIpForwardTable unsafe.Pointer, pdwSize *uint32, bOrder bool) WinResult {
	return procGetIPForwardTable.Call(uintptr(pIpForwardTable), uintptr(unsafe.Pointer(pdwSize)), boolToUintptr(bOrder))
}

// CreateIpForwardEntry creates a route in the local computer's IPv4 routing table.
func CreateIpForwardEntry(pRoute unsafe.Pointer) WinResult {
	return procCreateIPForwardEntry.Call(uintptr(pRoute))
}

// DeleteIpForwardEntry deletes an existing route in the local computer's IPv4 routing table.
func DeleteIpForwardEntry(pRoute unsafe.Pointer) WinResult {
	return procDeleteIPForwardEntry.Call(uintptr(pRoute))
}

// GetIfTable retrieves the MIB-II interface table.
func GetIfTable(pIfTable unsafe.Pointer, pdwSize *uint32, bOrder bool) WinResult {
	return procGetIfTable.Call(uintptr(pIfTable), uintptr(unsafe.Pointer(pdwSize)), boolToUintptr(bOrder))
}

// GetIpAddrTable retrieves the interface-to-IPv4 address mapping table.
func GetIpAddrTable(pIpAddrTable unsafe.Pointer, pdwSize *uint32, bOrder bool) WinResult {
	return procGetIPAddrTable.Call(uintptr(pIpAddrTable), uintptr(unsafe.Pointer(pdwSize)), boolToUintptr(bOrder))
}

// --- SetupAPI Structs and Constants ---

type SP_DEVINFO_DATA struct {
	CbSize    uint32
	ClassGuid windows.GUID
	DevInst   uint32
	Reserved  uintptr
}

type SP_CLASSINSTALL_HEADER struct {
	CbSize          uint32
	InstallFunction uint32
}

type SP_PROPCHANGE_PARAMS struct {
	ClassInstallHeader SP_CLASSINSTALL_HEADER
	StateChange        uint32
	Scope              uint32
	HwProfile          uint32
}

const (
	DIGCF_DEFAULT         = 0x00000001
	DIGCF_PRESENT         = 0x00000002
	DIGCF_ALLCLASSES      = 0x00000004
	DIGCF_PROFILE         = 0x00000008
	DIGCF_DEVICEINTERFACE = 0x00000010

	SPDRP_DEVICEDESC = 0x00000000

	DIF_PROPERTYCHANGE = 0x00000012
	DICS_PROPCHANGE    = 0x00000003
	DICS_FLAG_GLOBAL   = 0x00000001

	HC_ACTION = 0
)

// --- SetupAPI Wrappers ---

func SetupDiGetClassDevs(classGuid *windows.GUID, enumerator *uint16, hwndParent windows.Handle, flags uint32) (windows.Handle, WinResult) {
	res := procSetupDiGetClassDevs.Call(
		uintptr(unsafe.Pointer(classGuid)),
		uintptr(unsafe.Pointer(enumerator)),
		uintptr(hwndParent),
		uintptr(flags),
	)
	return windows.Handle(res.R1), res
}

func SetupDiEnumDeviceInfo(deviceInfoSet windows.Handle, memberIndex uint32, deviceInfoData *SP_DEVINFO_DATA) WinResult {
	return procSetupDiEnumDeviceInfo.Call(
		uintptr(deviceInfoSet),
		uintptr(memberIndex),
		uintptr(unsafe.Pointer(deviceInfoData)),
	)
}

func SetupDiDestroyDeviceInfoList(deviceInfoSet windows.Handle) WinResult {
	return procSetupDiDestroyDeviceInfoList.Call(uintptr(deviceInfoSet))
}

func SetupDiGetDeviceInstanceId(deviceInfoSet windows.Handle, deviceInfoData *SP_DEVINFO_DATA, deviceInstanceId *uint16, deviceInstanceIdSize uint32, requiredSize *uint32) WinResult {
	return procSetupDiGetDeviceInstanceId.Call(
		uintptr(deviceInfoSet),
		uintptr(unsafe.Pointer(deviceInfoData)),
		uintptr(unsafe.Pointer(deviceInstanceId)),
		uintptr(deviceInstanceIdSize),
		uintptr(unsafe.Pointer(requiredSize)),
	)
}

func SetupDiGetDeviceRegistryProperty(deviceInfoSet windows.Handle, deviceInfoData *SP_DEVINFO_DATA, property uint32, propertyRegDataType *uint32, propertyBuffer *byte, propertyBufferSize uint32, requiredSize *uint32) WinResult {
	return procSetupDiGetDeviceRegistryProperty.Call(
		uintptr(deviceInfoSet),
		uintptr(unsafe.Pointer(deviceInfoData)),
		uintptr(property),
		uintptr(unsafe.Pointer(propertyRegDataType)),
		uintptr(unsafe.Pointer(propertyBuffer)),
		uintptr(propertyBufferSize),
		uintptr(unsafe.Pointer(requiredSize)),
	)
}

func SetupDiSetClassInstallParams(deviceInfoSet windows.Handle, deviceInfoData *SP_DEVINFO_DATA, classInstallParams *SP_PROPCHANGE_PARAMS, classInstallParamsSize uint32) WinResult {
	return procSetupDiSetClassInstallParams.Call(
		uintptr(deviceInfoSet),
		uintptr(unsafe.Pointer(deviceInfoData)),
		uintptr(unsafe.Pointer(classInstallParams)),
		uintptr(classInstallParamsSize),
	)
}

func SetupDiCallClassInstaller(installFunction uint32, deviceInfoSet windows.Handle, deviceInfoData *SP_DEVINFO_DATA) WinResult {
	return procSetupDiCallClassInstaller.Call(
		uintptr(installFunction),
		uintptr(deviceInfoSet),
		uintptr(unsafe.Pointer(deviceInfoData)),
	)
}

var ReservedFileNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

// IsWindowsReservedFileName checks if a filename uses a Windows reserved device name.
func IsWindowsReservedFileName(name string) bool {
	// filepath.Base handles any directory prefix; TrimRight strips trailing
	// dots and spaces that Windows itself strips before resolving the name.
	baseName := strings.ToUpper(strings.TrimRight(filepath.Base(name), ". "))

	// Windows device names remain reserved when followed by an extension:
	// CON.txt, COM1.log, NUL.foo, etc. Only the device-name stem matters.
	stem := baseName
	if dot := strings.IndexByte(stem, '.'); dot >= 0 {
		stem = stem[:dot]
	}

	_, reserved := ReservedFileNames[stem]
	return reserved
}
