//go:build windows
// +build windows

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
	"strings"
	"syscall"
	"unsafe"

	"encoding/binary"
	"golang.org/x/sys/windows"
	"golang.org/x/term"

	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Logger - exported global logger. Defaults to a "do nothing" logger.
// So if this wincoe lib ever wants to log things it uses this Logger to do so, currently it doesn't need to!
//
// Set this in caller(lib user) like:
//
// wincoe.Logger = slog.Default()
//
// this way this wincoe lib will log to where caller wants.
// var Logger *slog.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
// Logger is stored behind an atomic.Pointer because it can be swapped
// concurrently (dnsbollocks.LoggerManager.ApplyConfig does this on every
// config Reload) while other goroutines are reading it via panic2() from
// arbitrary wincoe call paths — DNS/UDP/TCP request handling can reach rare
// defensive panics in this package (e.g. PidAndExeForUDP's bounds check,
// impossibiru() inside callWithRetry) at any time. A plain *slog.Logger
// package var here would be a genuine, -race-detectable data race between
// Reload() and any in-flight request hitting one of those paths.
//
// Set via: wincoe.Logger.Store(someLogger)
// Read via: wincoe.Logger.Load()
var Logger atomic.Pointer[slog.Logger]

func init() {
	Logger.Store(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

var (
	procSetConsoleTextAttribute = NewBoundProc(Kernel32, "SetConsoleTextAttribute", CheckBool)
)

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
	if hStdout == windows.InvalidHandle {
		return errors.New("invalid stdout handle")
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

// WithConsoleColor temporarily changes text attribute, runs fn, then restores original
func WithConsoleColor(outputHandle windows.Handle, color uint16, fn func()) (errRet error) {
	originalColor, err := GetConsoleScreenBufferAttributes(outputHandle)
	if err != nil {
		return fmt.Errorf("GetConsoleScreenBufferInfo failed: %w", err)
	}
	defer func() {
		// Always restore (even on panic inside fn)
		if resetErr := SetConsoleTextAttribute(outputHandle, originalColor); resetErr != nil { //NVM nolint:errcheck // because nothing to do with the error.
			errRet = fmt.Errorf("SetConsoleTextAttribute failed to reset back to original color %d, err: %w", originalColor, resetErr) // Only overwrite if the main logic succeeded
		}
	}()
	// Set new color
	if err := SetConsoleTextAttribute(outputHandle, color); err != nil {
		return fmt.Errorf("SetConsoleTextAttribute failed to set new color %d, err: %w", color, err)
	}

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

/*
Windows:

Console = input events

# Arrow keys are atomic

# FlushConsoleInputBuffer already solves the problem

One read is enough
*/
func ClearStdinIfTermIsNOTRaw() (hadInput bool) {
	h := windows.Handle(os.Stdin.Fd())

	var n uint32
	err := windows.GetNumberOfConsoleInputEvents(h, &n) // FIXME: this means mouse movements too though!
	if err != nil || n == 0 {
		return false
	}

	if flushErr := windows.FlushConsoleInputBuffer(h); flushErr != nil {
		log := Logger.Load() // safe atomic read
		log.Debug("ClearStdinIfTermIsNOTRaw: FlushConsoleInputBuffer failed", SafeErr(flushErr))
	}
	return true
}

func ReadKeySequence() {
	var b [1]byte
	if _, err := os.Stdin.Read(b[:]); err != nil {
		log := Logger.Load() // safe atomic read
		log.Debug("ReadKeySequence: os.Stdin.Read failed", SafeErr(err))
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
	log := Logger.Load() // safe atomic read

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

// WithConsoleEventRaw
func WithConsoleEventRaw(fn func()) {
	h := windows.Handle(os.Stdin.Fd())

	var oldMode uint32
	if err := windows.GetConsoleMode(h, &oldMode); err != nil {
		log := Logger.Load() // safe atomic read
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
		log := Logger.Load() // safe atomic read
		log.Warn("WithConsoleEventRaw: SetConsoleMode failed to enter raw mode, NOT running fn() without raw-mode toggling", SafeErr(err))
		return
	}
	defer func() {
		log := Logger.Load() // safe atomic read
		err2 := windows.SetConsoleMode(h, oldMode)
		if err2 != nil {
			log.Warn("WithConsoleEventRaw: SetConsoleMode failed to restore old mode, ignoring", SafeErr(err2))
		}
	}()

	fn()
}

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

// returns true if waited, false if it's not interactive
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

// whether it is or not a terminal, it attempts to wait for any key, with proper clrbuf(s) before and after!
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
	os.Stderr.Sync() // Tell Windows to flush the file buffers to disk/console
	//fmt.Printf("[GoR:%d] !flushing stdout\n", GoRoutineId())
	os.Stdout.Sync() // Tell Windows to flush the file buffers to disk/console
}

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

// side effect of returning a struct like this is that we don't get warned for not handling the returned Err or CallStatus errors! in a way it's good because CallStatus is rarely checked! just Err is checked usually.
type WinResult struct {
	R1 uintptr
	R2 uintptr
	// exactly the third return value from LazyProc.Call; may contain additional status information even when Err == nil.
	//
	// Raw status returned by LazyProc.Call. Usually ERROR_SUCCESS
	// on successful calls, but some Win32 APIs use it to report
	// additional success information (e.g. ERROR_ALREADY_EXISTS).
	CallStatus error

	Err error // normalized according to the Check*() function that NewBoundProc() got as the last arg!
}

func (r WinResult) Failed() bool {
	return r.Err != nil
}

func (r WinResult) Succeeded() bool {
	return r.Err == nil
}

// ErrIs reports whether Err matches a particular failure value.
//
// Successful calls are represented by Err == nil, so passing
// windows.ERROR_SUCCESS will always return false.
func (r WinResult) ErrIs(target error) bool {
	if target == nil || target == error(windows.ERROR_SUCCESS) { //nolint:errorlint //we're checking for exactly this, not for one of the wrapped ones being ERROR_SUCCESS! aka // exact sentinel check; wrapped errors are intentionally not matched.
		GetBugLogger().Error(
			"BUG: WinResult.ErrIs() cannot be used to test for success; either use WinResult.Succeeded() first then WinResult.ErrIs(target_error) (aka this) to check for exact error, or use WinResult.CallStatusIs(target_error) which allows checking for windows.ERROR_SUCCESS! So, dev. needs to change the callsite!",
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
// Use this only for APIs whose documentation defines meaningful success
// status codes beyond simple success/failure.
func (r WinResult) CallStatusIs(target error) bool {
	return errors.Is(r.CallStatus, target)
}

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

// UnspecifiedWinApi is the string used when empty op name is used
const UnspecifiedWinApi string = "unspecified_winapi"
const THREAD_PRIORITY_ERROR_RETURN int32 = 0x7fffffff

// LazyProcish is the minimal interface that WinCall needs from a LazyProc-like object.
//
// We deliberately avoid the full *windows.LazyProc type to enable mocking.
type LazyProcish interface {
	// Name returns the name of the procedure (used in error messages).
	//Why Name() instead of a field? Because interfaces in Go cannot require fields — only methods
	Name() string

	// Call invokes the Windows procedure with the given arguments.
	// Signature must match windows.LazyProc.Call exactly.
	Call(a ...uintptr) (r1, r2 uintptr, lastErr error)
	Find() error
}

// realLazyProc wraps *windows.LazyProc to satisfy LazyProcish.
//
// Embedding gives us .Call() for free via promotion.
type realLazyProc struct {
	*windows.LazyProc
}

// Name implements LazyProcish.
//
// Returns the procedure name for use in error messages.
func (r *realLazyProc) Name() string {
	return r.LazyProc.Name
}

// RealProc wraps a *windows.LazyProc into the testable interface.
//
// Use this at all production call sites instead of passing *windows.LazyProc directly.
//
// The real production code that previously called WinCall(&proc, ...) now becomes WinCall(&realLazyProc{LazyProc: &proc}, ...) or you use this tiny helper like:
//
// r1, r2, err := WinCall(RealProc(proc), CheckBool, uintptr(unsafe.Pointer(&something)), ...)
func RealProc(p *windows.LazyProc) LazyProcish {
	return &realLazyProc{LazyProc: p}
}

// RealProc2 resolves a procedure from the given DLL and wraps it into a LazyProcish.
//
// It is a thin, validated convenience over dll.NewProc(name) + RealProc(...).
// This function enforces basic invariants early:
//   - dll must be non-nil
//   - name must be non-empty (after trimming whitespace)
//
// The returned LazyProcish is suitable for use with WinCall or higher-level
// binding helpers such as BindFunc.
//
// RealProc2 does NOT attach any failure semantics (WinCheckFunc). Callers must
// explicitly provide the appropriate check strategy (e.g. CheckBool, CheckHandle)
// when invoking the procedure via WinCall or when binding it.
//
// Panics:
//   - if dll is nil
//   - if name is empty or whitespace-only
func RealProc2(dll *windows.LazyDLL, name string) LazyProcish {
	if dll == nil {
		panic2("RealProc2: nil dll")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		panic2("RealProc2: empty proc name")
	}
	return RealProc(dll.NewProc(name))
}

// BoundProc
// By making this a struct with a method, we can apply //go:uintptrescapes to it.

// BoundProc represents a Windows API procedure permanently bound to a
// specific failure-checking strategy.
//
// It wraps a LazyProcish (usually a windows.LazyProc) and a WinCheckFunc.
// By using BoundProc instead of raw Syscall/Call, you centralize error
// handling logic for the specific API while maintaining the ability to
// use //go:uintptrescapes for memory safety.
type BoundProc struct {
	Proc  LazyProcish
	Check WinCheckFunc
}

// Call executes the underlying Windows procedure with the provided arguments.
//
// SECURITY WARNING: This method uses the //go:uintptrescapes compiler directive.
// To ensure memory safety and prevent "0xc0000005 Access Violation" crashes,
// any Go pointer passed as an argument MUST be converted to uintptr using
// uintptr(unsafe.Pointer(&x)) directly within the argument list of the
// call site.
// So //go:uintptrescapes = escape to heap + keep-alive for the duration of the call.
// The compiler inserts the necessary liveness (equivalent to an implicit KeepAlive across the entire function call)
// for any argument passed as uintptr(unsafe.Pointer(...)) to a function marked //go:uintptrescapes.
//
// Example:
//
//	var size uint32
//	proc.Call(handle, uintptr(unsafe.Pointer(&size)))
//
// This direct conversion signals the Go compiler to move the variable to
// the heap, ensuring its memory address remains stable even if the stack grows.
//
//go:uintptrescapes
func (b *BoundProc) Call(args ...uintptr) WinResult {
	return WinCall(b.Proc, b.Check, args...)
}

// Find attempts to locate the procedure in the DLL.
// Returns nil if the procedure is successfully found, or an error if it is not.
func (b *BoundProc) Find() error {
	return b.Proc.Find()
}

// NewBoundProc initializes a BoundProc by resolving a procedure from the
// provided DLL and attaching a result-checking function.
//
// Parameters:
//   - dll: A pointer to a windows.LazyDLL (e.g., kernel32, user32).
//   - name: The exact string name of the procedure (e.g., "GetProcessId").
//   - check: A WinCheckFunc (e.g., CheckBool) used to determine if the
//     API call failed based on its return value.
//
// It panics if the check function is nil.
func NewBoundProc(dll *windows.LazyDLL, name string, check WinCheckFunc) *BoundProc {
	if check == nil {
		panic2("NewBoundProc: nil WinCheckFunc passed as arg")
	}

	return &BoundProc{
		Proc:  RealProc2(dll, name),
		Check: check,
	}
}

// WARNING: you must do the uintptr casting at the args call place (for pointers on stack!) for this to work and not crash randomly because the stack got moved by Go.
// The price of absolute memory safety in Go is that you must write uintptr(unsafe.Pointer(...)) explicitly at the exact call site.
// This tells the compiler, "Pin this variable right now."
//

// WinCall is the low-level engine that executes the syscall and performs
// automated error checking.
//
// It leverages //go:uintptrescapes to signal to the compiler that arguments
// may be pointers converted to integers. It calls the procedure, captures
// the return values (r1, r2) and the system error, then passes them to
// CheckWinResult to produce a clean Go error if the call failed.
//
// Use this directly only if you need to bypass the BoundProc abstraction.
// Otherwise, use BoundProc.Call for better type organization.
//
//go:uintptrescapes
func WinCall(proc LazyProcish, check WinCheckFunc, args ...uintptr) WinResult {
	if proc == nil {
		panic2("WinCall: nil proc")
	}

	op := strings.TrimSpace(proc.Name())
	if op == "" {
		op = UnspecifiedWinApi
	}
	// args is a []uintptr, but because of //go:uintptrescapes, the caller
	// has already pinned the memory safely before we get here.
	r1, r2, callStatus := proc.Call(args...)
	//XXX: don't put anything here, which might call a syscall or it might delete the last error for a potential future GetLastError() call.
	return WinResult{
		R1:         r1,
		R2:         r2,
		CallStatus: callStatus,
		Err:        CheckWinResult(op, check, r1, callStatus),
	}
}

var (
	Iphlpapi                = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedUdpTable = NewBoundProc(Iphlpapi, "GetExtendedUdpTable", CheckErrno)
	procGetExtendedTcpTable = NewBoundProc(Iphlpapi, "GetExtendedTcpTable", CheckErrno)

	// Secure: restricts DLL search path strictly to %SystemRoot%\System32
	Kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	// Note: QueryFullProcessNameW expects 'size' to include the null terminator
	// on input, and returns the length WITHOUT the null terminator on success.
	procQueryFullProcessName     = NewBoundProc(Kernel32, "QueryFullProcessImageNameW", CheckBool)
	procCreateToolhelp32Snapshot = NewBoundProc(Kernel32, "CreateToolhelp32Snapshot", CheckHandle)
	procProcess32First           = NewBoundProc(Kernel32, "Process32FirstW", CheckBool)
	procProcess32Next            = NewBoundProc(Kernel32, "Process32NextW", CheckBool)

	//procWriteConsoleInputW = Kernel32.NewProc("WriteConsoleInputW")
	procWriteConsoleInputW = NewBoundProc(Kernel32, "WriteConsoleInputW", CheckBool)

	//procReadConsoleInputW = Kernel32.NewProc("ReadConsoleInputW")
	//procPeekConsoleInputW = Kernel32.NewProc("PeekConsoleInputW")
	procPeekConsoleInputW = NewBoundProc(Kernel32, "PeekConsoleInputW", CheckBool)
	procReadConsoleInputW = NewBoundProc(Kernel32, "ReadConsoleInputW", CheckBool)
)

// auto runs before main(), loads the DLLs non-lazily.
func init() {
	loadDll(Kernel32)
	loadDll(Iphlpapi)
}

func loadDll(dll *windows.LazyDLL) {
	err := dll.Load()
	if err != nil {
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
				impossibiru("size is bigger than len(buf)")
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
			return nil, err
		}
		// Loop continues, using the updated 'size' from the failed call
		//however:
		// If size didn't increase but we still got an error,
		// we should nudge it upward to prevent an infinite loop.
		// We use uint64 casts to satisfy gosec G115.
		// 1. Convert both to uint64 to compare safely without narrowing (Fixes G115)
		if uint64(size) <= uint64(len(buf)) {
			// 2. Check for overflow before adding 1024
			const increment = 1024
			const MaxInt = math.MaxUint32
			if MaxInt-size < increment {
				return nil, fmt.Errorf("buffer size(%d) would overflow uint32(%d) if adding %d", size, MaxInt, increment)
			}
			size += increment
		}
	}
	return nil, fmt.Errorf("buffer growth exceeded max retries(%d)", MAX_RETRIES)
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
		return "", fmt.Errorf("OpenProcess failedfor PID %d: %w", pid, err0)
	}
	//defer windows.CloseHandle(hProc)
	defer closeHandleLogged(hProc, "QueryFullProcessName:OpenProcess hProc")

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
			//size is just a number the API handed back, so let's not trust it, thus use full 'buf'
			return windows.UTF16ToString(buf), nil
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
func panic2(msg string) {
	GetBugLogger().Error(msg)
	panic(msg)
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
		return "", fmt.Errorf("pid %d not found for %s, errTransient:'%v', err:'%w'", owningPid, remoteAddrStr, err2, err3)
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
	defer closeHandleLogged(snapshot, "GetProcessName:CreateToolhelp32Snapshot snapshot")

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
			Logger.Load().Debug("CloseServiceHandle failed in GetServiceNamesFromPIDUncached:OpenSCManager", slog.String("err", xerr.Error()))
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

	for i := uint32(0); i < servicesReturned; i++ {
		offset := uintptr(i) * entrySize
		if offset+entrySize > uintptr(len(buffer)) {
			return nil, fmt.Errorf("entry %d at offset %d + entrySize %d exceeds buffer len %d",
				i, offset, entrySize, len(buffer))
		}
		data := (*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buffer[offset]))

		// Validate ServiceName pointer is within buffer before dereferencing
		bufStart := uintptr(unsafe.Pointer(&buffer[0]))
		bufEnd := bufStart + uintptr(len(buffer))
		snPtr := uintptr(unsafe.Pointer(data.ServiceName))
		if snPtr < bufStart || snPtr >= bufEnd {
			// pointer outside buffer — skip this entry
			return nil, fmt.Errorf("entry %d at offset %0x which has entrySize %d, in the buffer len %d, "+
				"has a ServiceName ptr outside the buffer=%p area, snPtr=%0x bufStart=%0x bufEnd=%0x",
				i, offset, entrySize, len(buffer),
				buffer, snPtr, bufStart, bufEnd)
		}

		if data.ServiceStatusProcess.ProcessId == targetPID {
			str := windows.UTF16PtrToString(data.ServiceName)
			// We use UTF16PtrToString because ServiceName is a *uint16
			// pointing into the same buffer returned by the API.
			serviceNames = append(serviceNames, str)
		}
	}
	return serviceNames, nil
}

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
	// single SCM enumeration.
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
	fmt.Sscanf(string(buf[:n]), "goroutine %d", &id)
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
	procReplaceFileW = NewBoundProc(Kernel32, "ReplaceFileW", CheckBool)
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
	log.Error(logmsg) //FIXME: ? the errors/args are embedded in the msg
	panic(logmsg)
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
				"Because the file contains non-zero bytes, the next server boot will panic.\n",
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
	log.Error(logmsg)
	panic(logmsg)
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

func closeHandleLogged(h windows.Handle, context string) {
	if err := windows.CloseHandle(h); err != nil {
		//Logged shouldn't ever be nil here due to init()
		Logger.Load().Debug("CloseHandle failed.",
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
		tmpFile, err := os.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
		if err != nil {
			return fmt.Errorf("create failed: %w", err)
		}
		_, writeErr := tmpFile.Write(data)
		syncErr := tmpFile.Sync()
		closeErr := tmpFile.Close()

		if writeErr != nil || syncErr != nil || closeErr != nil {
			return fmt.Errorf("write/sync/close failed (write=%v sync=%v close=%v)", writeErr, syncErr, closeErr)
		}
		return nil
	})
}

// neutralizeOrPanic encapsulates the resilience cleanup logic for an abandoned staging file.
// It attempts deletion first. If deletion fails, it forces a truncation down to 0 bytes
// to neutralize any partial garbage data that would trip up the next boot.
func neutralizeOrPanic(tmpName string, perm os.FileMode, maxAttempts int, backoff time.Duration, log *slog.Logger, actionErr error, title string, extraPanicMsg string) {
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
	log.Error(logmsg)
	panic(logmsg)
}
