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

package wincoe

import (
	"fmt"
	"math"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

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

	_ = windows.FlushConsoleInputBuffer(h)
	return true
}

/*
On Windows, the console input buffer is not a keyboard buffer. It is an event queue.

Conceptually, it’s closer to a GUI message loop than to Unix stdin.

The queue can contain (among others):

KEY_EVENT

key down

key up

MOUSE_EVENT

movement

button presses

WINDOW_BUFFER_SIZE_EVENT

FOCUS_EVENT

MENU_EVENT

So when you hear “console input”, read it as:

“Anything the console subsystem thinks the user did.”

This is not a bug. It dates back to early Windows NT and the idea that the console is a window, not a TTY.
*/
// func clearStdin() (hadKey bool) {
// 	h := windows.Handle(os.Stdin.Fd())

// 	var rec windows.InputRecord //fail here
// 	var read uint32

// 	for {
// 		err := windows.ReadConsoleInput(h, &rec, 1, &read) //fail here
// 		if err != nil || read == 0 {
// 			break
// 		}

// 		if rec.EventType == windows.KEY_EVENT {
// 			ke := rec.KeyEvent()
// 			if ke != nil && ke.BKeyDown {
// 				hadKey = true
// 				break
// 			}
// 		}
// 	}

// 	_ = windows.FlushConsoleInputBuffer(h)
// 	return hadKey
// }

func ReadKeySequence() {
	var b [1]byte
	_, _ = os.Stdin.Read(b[:])
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

var (
	//kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procReadConsoleInputW = Kernel32.NewProc("ReadConsoleInputW")
	procPeekConsoleInputW = Kernel32.NewProc("PeekConsoleInputW")
	//procFlushConsoleInputBuf = kernel32.NewProc("FlushConsoleInputBuffer")
)

// // clearStdin reads and inspects console input events, returns true if any KeyDown was seen.
// // It consumes events as it goes and finally flushes the buffer to leave a clean state.
// func ClearStdin() (hadKey bool) {
// 	hadKey = false //explicit
// 	h := syscall.Handle(os.Stdin.Fd())

// 	var rec inputRecord
// 	var numRead uint32

// 	for {
// 		fmt.Println("foo3")
// 		r1, _, err := procReadConsoleInputW.Call(
// 			uintptr(h),
// 			uintptr(unsafe.Pointer(&rec)),
// 			uintptr(1),
// 			uintptr(unsafe.Pointer(&numRead)),
// 		)
// 		fmt.Println("foo4")
// 		if r1 == 0 {
// 			fmt.Println("ReadConsoleInputW error:", err) //FIXME: bad to do.
// 			break
// 		}
// 		if err != syscall.Errno(0) {
// 			break
// 		}

// 		if numRead == 0 {
// 			break
// 		}

// 		fmt.Println(rec.EventType)
// 		const KEY_EVENT = 0x0001
// 		if rec.EventType == KEY_EVENT {
// 			ke := (*keyEventRecord)(unsafe.Pointer(&rec.Event[0]))

// 			vk := ke.VirtualKeyCode
// 			fmt.Printf(
// 				"KEY  down=%v vk=0x%X rune=%q ctrl=0x%X\n",
// 				ke.BKeyDown != 0,
// 				vk,
// 				rune(ke.UnicodeChar),
// 				ke.ControlKeyState,
// 			)

// 			// Only treat an actual key press as meaningful input.
// 			if ke.BKeyDown != 0 {
// 				//KEY_UP without KEY_DOWN is meaningless for “press any key” or for is there a key/char queued on stdin
// 				hadKey = true
// 				// Ensure the buffer is empty afterwards.
// 				fmt.Println("got key", ke)
// 				_, _, _ = procFlushConsoleInputBuf.Call(uintptr(h))
// 				break
// 			}
// 		}
// 	}

// 	//don't clear buf. here as there are no keys pending
// 	return
// }

// //go:build windows
// package dnsbollocks

// import (
// 	"os"
// 	"syscall"
// 	"unsafe"
// )

// type inputRecord struct {
// 	EventType uint16
// 	_         [2]byte
// 	Event     [16]byte
// }

// type keyEventRecord struct {
// 	BKeyDown        int32
// 	RepeatCount     uint16
// 	VirtualKeyCode  uint16
// 	VirtualScanCode uint16
// 	UnicodeChar     uint16
// 	ControlKeyState uint32
// }

// var (
// 	kernel32                = syscall.NewLazyDLL("kernel32.dll")
// 	procPeekConsoleInputW   = kernel32.NewProc("PeekConsoleInputW")
// 	procReadConsoleInputW   = kernel32.NewProc("ReadConsoleInputW")
// 	// procFlushConsoleInputBuf if you still need it: kernel32.NewProc("FlushConsoleInputBuffer")
// )

const (
	KEY_EVENT = 0x0001

// MOUSE_EVENT = 0x0002
)

// ClearStdin inspects and consumes all pending console input events.
// Returns true if any KEY_EVENT with BKeyDown was observed.
// It peeks first to avoid blocking reads.
func ClearStdin() (hadKey bool) {
	h := syscall.Handle(os.Stdin.Fd())

	hadKey = false // be explicit

	for {
		// Peek a single event (non-destructive, non-blocking).
		var peekRec inputRecord
		var peekCount uint32
		r1, _, err := procPeekConsoleInputW.Call(
			uintptr(h),
			uintptr(unsafe.Pointer(&peekRec)),
			uintptr(1),
			uintptr(unsafe.Pointer(&peekCount)),
		)
		if r1 == 0 {
			// syscall error — be conservative and stop looping
			_ = err
			break
		}
		if peekCount == 0 {
			// no events waiting -> done
			break
		}

		// There's at least one event, now consume one event for real.
		var rec inputRecord
		var read uint32
		r1, _, err = procReadConsoleInputW.Call(
			uintptr(h),
			uintptr(unsafe.Pointer(&rec)),
			uintptr(1),
			uintptr(unsafe.Pointer(&read)),
		)
		if r1 == 0 {
			// read failed; stop
			_ = err
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

	return hadKey
}

// func withRawConsole(fn func()) {
// 	h := syscall.Handle(os.Stdin.Fd())

// 	var oldMode uint32
// 	_ = syscall.GetConsoleMode(h, &oldMode)

// 	newMode := oldMode
// 	newMode &^= syscall.ENABLE_LINE_INPUT
// 	newMode &^= syscall.ENABLE_ECHO_INPUT

// 	_ = syscall.SetConsoleMode(h, newMode)
// 	defer syscall.SetConsoleMode(h, oldMode)

// 	fn()
// }

// func ClearClearStdinIfTermIsRawStdin() bool {
// 	fd := int(os.Stdin.Fd())

// 	hadKey := false

// 	// Put stdin into nonblocking mode
// 	if err := syscall.SetNonblock(fd, true); err != nil {
// 		return false
// 	}
// 	defer syscall.SetNonblock(fd, false)

// 	var buf [64]byte
// 	for {
// 		n, err := os.Stdin.Read(buf[:])
// 		if n > 0 {
// 			hadKey = true
// 		}
// 		if err != nil {
// 			break
// 		}
// 	}

// 	return hadKey
// }

//import "golang.org/x/sys/windows"

// WithConsoleEventRaw
func WithConsoleEventRaw(fn func()) {
	h := windows.Handle(os.Stdin.Fd())

	var oldMode uint32
	if err := windows.GetConsoleMode(h, &oldMode); err != nil {
		return
	}

	newMode := oldMode
	//"Take the current value of newMode and force the ENABLE_LINE_INPUT bit to be 0 (off), while leaving all other bits exactly as they were."
	//so: newMode = newMode AND (NOT windows.ENABLE_LINE_INPUT)
	newMode &^= windows.ENABLE_LINE_INPUT
	newMode &^= windows.ENABLE_ECHO_INPUT

	_ = windows.SetConsoleMode(h, newMode)
	defer windows.SetConsoleMode(h, oldMode)

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

// func IsStdinConsoleInteractive_Flimsy() bool {
// 	h := windows.Handle(os.Stdin.Fd())

// 	var mode uint32
// 	err := windows.GetConsoleMode(h, &mode)
// 	return err == nil
// }

// this is cross-platform, as per Gemini
func IsStdinConsoleInteractive() bool {
	fdPtr := os.Stdin.Fd()
	//fmt.Printf("got fdPtr %d\n", fdPtr)

	// G115 Fix: Ensure the uintptr fits into a signed int
	if fdPtr > math.MaxInt {
		//TODO: should be log this? Logger.slog
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

	// oldState, err := term.MakeRaw(fd)
	// if err != nil {
	// 	fmt.Print("couldn't make the terminal raw, bailing!")
	// 	return // or log, or fail loudly — your call
	// }
	// defer term.Restore(fd, oldState)

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
			//})
			//WithConsoleEventRaw(func() {

			if ClearStdin() { // OS-specific
				fmt.Print("(clrbuf2).")
			}
		})
		done <- struct{}{} // Empty structs occupy zero bytes and are commonly used for signals where no data is needed.
	}()

	// select {
	// case <-done:
	// 	//case <-ctx.Done():  // this bypasses the key wait!
	// }
	<-done // blocks until a value is received from the channel.
	fmt.Println()
}
