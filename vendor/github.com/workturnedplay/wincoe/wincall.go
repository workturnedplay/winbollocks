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

// Package wincoe aka winco(r)e, are common functions I use across my projects do keep things DRY.
package wincoe

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"golang.org/x/sys/windows"
)

// WinCheckFunc defines a predicate used to determine if a Windows API call failed
// based on its primary return value (r1).
type WinCheckFunc func(r1 uintptr) bool

var (
	// CheckBool identifies a failure for functions returning a Windows BOOL in r1.
	// In the Windows API, a 0 (FALSE) indicates that the function failed.
	CheckBool WinCheckFunc = func(r1 uintptr) bool { return r1 == 0 }

	// CheckHandle identifies a failure for functions returning a HANDLE in r1.
	// Many Windows APIs return INVALID_HANDLE_VALUE (all bits set to 1) on failure.
	// ^uintptr(0) is the Go-idiomatic way to represent -1 as an unsigned pointer.
	CheckHandle WinCheckFunc = func(r1 uintptr) bool { return r1 == ^uintptr(0) }

	// CheckNull identifies a failure for functions returning a pointer or a handle in r1
	// where a NULL value (0) indicates the operation could not be completed.
	CheckNull WinCheckFunc = func(r1 uintptr) bool { return r1 == 0 }

	// CheckHRESULT identifies a failure for functions that return an HRESULT in r1.
	// An HRESULT is a 32-bit value where a negative number (high bit set)
	// indicates an error, while 0 or positive values indicate success.
	CheckHRESULT WinCheckFunc = func(r1 uintptr) bool { return int32(r1) < 0 }

	// CheckErrno identifies a failure for Win32 APIs that return a DWORD error code in r1.
	// In this convention, 0 (ERROR_SUCCESS) means success, any non-zero value is a failure.
	CheckErrno WinCheckFunc = func(r1 uintptr) bool { return r1 != 0 }
)

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
	//fmt.Printf("[GoR:%d] !starting CheckWinResult for %s\n", GoRoutineId(), operationNameToIncludeInErrorMessages)
	//Smashy(true)
	if !isFailure(r1) {
		//fmt.Printf("[GoR:%d] !ending   CheckWinResult for %s with SUCCESS.\n", GoRoutineId(), operationNameToIncludeInErrorMessages)
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
		if r1 != 0 {
			errno := windows.Errno(r1) //TODO: see how we can match against this, I doubt errors.Is still works for this! actually, it seems to based on the below!

			// Defensive: avoid ever wrapping ERROR_SUCCESS
			if !errors.Is(errno, windows.ERROR_SUCCESS) {
				//fmt.Printf("[GoR:%d] !ending   CheckWinResult for %s with Errno: %v\n", GoRoutineId(), operationNameToIncludeInErrorMessages, errno)
				// since r1 != 0 already, this is bound to never be ERROR_SUCCESS here, unless r1 != 0 can ever be ERROR_SUCCESS, unsure.
				return fmt.Errorf("%q windows call failed with error: %w", operationNameToIncludeInErrorMessages, errno)
			}
		}

		//fmt.Printf("[GoR:%d] !ending   CheckWinResult for %s with truly unknown failure: ret=%d\n", GoRoutineId(), operationNameToIncludeInErrorMessages, r1)
		// Fallback: truly unknown failure
		return fmt.Errorf(
			"%q windows call reported failure (ret=%d) but no usable error was provided",
			operationNameToIncludeInErrorMessages,
			r1,
		)
	}

	//fmt.Printf("[GoR:%d] !ending   CheckWinResult for %s with normal callErr: %v\n", GoRoutineId(), operationNameToIncludeInErrorMessages, callErr)
	// Normal path: we have a meaningful callErr
	return fmt.Errorf("%q windows call failed with error: %w", operationNameToIncludeInErrorMessages, callErr)

	// // If the system says failure but the error code is 0/nil,
	// // we return a concrete error message WITHOUT wrapping ERROR_SUCCESS.
	// if callErr == nil || errors.Is(callErr, windows.ERROR_SUCCESS) {
	// 	finalErr = fmt.Errorf("%q windows call reported failure (ret=%d) but LastError(aka callErr) was 0", operationNameToIncludeInErrorMessages, r1)
	// } else {
	// 	//finalErr = callErr //unwrapped
	// 	// We only use %w when there is a REAL error to wrap.
	// 	finalErr = fmt.Errorf("%q windows call failed with error: %w", operationNameToIncludeInErrorMessages, callErr)
	// }

	// return finalErr
}

// UnspecifiedWinApi is the string used when empty op name is used
const UnspecifiedWinApi string = "unspecified_winapi"

// WinCall does r1,r2,err:=proc.Call(args...) and returns them, but
// err is guaranteed to be non-nil when check(r1) indicates failure.
//
// Use errors.Is whenever you want to check whether an error matches a particular sentinel value, like windows.ERROR_ACCESS_DENIED or windows.ERROR_SUCCESS.
// This works even if the error was wrapped with %w in fmt.Errorf, which is exactly what this helper does.
//
// WinCall invokes the Windows procedure and processes the result using the provided
// failure checker. It returns r1, r2, and an error that is never nil when r1 indicates failure.
//
// This version accepts any type that implements LazyProcish, which allows easy mocking in tests.
// func WinCall(proc LazyProcish, check WinCheckFunc, args ...any) (uintptr, uintptr, error) {
// 	if proc == nil {
// 		//return 0, 0,
// 		panic(fmt.Errorf("WinCall: nil proc"))
// 	}

// 	op := strings.TrimSpace(proc.Name())
// 	if op == "" {
// 		op = UnspecifiedWinApi
// 	}

// 	//FIXME: this is shit! so either get back to their .Call() or switch to Rust.
// 	//XXX: to be clear, THIS isn't good enough either! I'd have to cast to uintptr in the .Call() args instead! which means separate func for each, no WinCall possible! Or maybe there's a way via //go:uintptrescapes
// 	// Prepare the raw uintptr arguments for the real syscall
// 	uintArgs := make([]uintptr, len(args))
// 	keepAlive := make([]any, len(args))

// 	for i, arg := range args {
// 		switch v := arg.(type) {
// 		case uintptr:
// 			//uintArgs[i] = v
// 			panic(fmt.Errorf("WinCall: No uintptr(s) allowed, at index %d for %q windows call", i, op))
// 		case unsafe.Pointer:
// 			keepAlive[i] = v
// 			uintArgs[i] = uintptr(v) // Convert at the LAST possible moment

// 		case int:
// 			keepAlive[i] = v
// 			uintArgs[i] = uintptr(v)
// 		case int16:
// 			keepAlive[i] = v
// 			uintArgs[i] = uintptr(v)
// 		case int32:
// 			keepAlive[i] = v
// 			uintArgs[i] = uintptr(v)
// 		case int64:
// 			keepAlive[i] = v
// 			uintArgs[i] = uintptr(v)

// 		case uint:
// 			keepAlive[i] = v
// 			uintArgs[i] = uintptr(v)
// 		case uint32:
// 			keepAlive[i] = v
// 			uintArgs[i] = uintptr(v)
// 		case uint16:
// 			keepAlive[i] = v
// 			uintArgs[i] = uintptr(v)
// 		case uint64:
// 			keepAlive[i] = v
// 			uintArgs[i] = uintptr(v)

// 		case windows.Handle:
// 			keepAlive[i] = v
// 			uintArgs[i] = uintptr(v)
// 		case bool:
// 			keepAlive[i] = v
// 			if v {
// 				uintArgs[i] = 1
// 			} else {
// 				uintArgs[i] = 0
// 			}

// 		case *uint8:
// 			keepAlive[i] = v
// 			uintArgs[i] = uintptr(unsafe.Pointer(v))
// 		case *uint16:
// 			keepAlive[i] = v
// 			uintArgs[i] = uintptr(unsafe.Pointer(v))
// 		case *uint32:
// 			keepAlive[i] = v
// 			uintArgs[i] = uintptr(unsafe.Pointer(v))
// 		case *uint64:
// 			keepAlive[i] = v
// 			uintArgs[i] = uintptr(unsafe.Pointer(v))

// 		// If you have a custom struct, you'll need its pointer type here too
// 		case *windows.ProcessEntry32:
// 			keepAlive[i] = v
// 			uintArgs[i] = uintptr(unsafe.Pointer(v))

// 		default:
// 			// If it's a pointer but not cast to unsafe.Pointer yet
// 			// we can use reflection as a fallback(but we shouldn't as it will trigger GC), but unsafe.Pointer is faster
// 			panic(fmt.Errorf("TODO: WinCall: unsupported argument type %T at index %d for %q windows call", arg, i, op))
// 		}
// 	}

// 	// This is the "Danger Zone" - we have converted to uintptr.
// 	// We must call proc.Call IMMEDIATELY.
// 	//When you call proc.Call(uintArgs...), you are passing a slice header. The compiler looks at the uintptrescapes directive and says:
// 	//  "Okay, I need to protect the arguments. Oh, look, the argument is a slice. Slices are just a pointer and two integers.
// 	// None of those are uintptr conversions from pointers, so I have nothing to pin."
// 	//The compiler does not look inside your uintArgs slice to see what those numbers represent.
// 	// The "Shield" of the LazyProc.Call directive is protecting the slice itself, not the memory addresses inside the slice.
// 	//If args[0] is a pointer to a variable on the stack (e.g., &target), and the Go runtime decides to grow the stack between your for loop
// 	// and the proc.Call (which could happen during the interface method dispatch or the p.mustFind() call inside LazyProc),
// 	// the uintptr in your slice will point to a "Ghost" address on the old stack.
// 	//workaround (in callers): If you want to keep this architecture and be as close to Rust-level safety as possible, there is one rule you must follow when using your WinCall:
// 	//    "Never pass a pointer to a local stack variable."
// 	//If you always ensure the pointers you pass into WinCall come from the Heap (e.g., use new(uint32) or make([]byte, n)),
// 	//  the stack-move problem disappears. Objects on the Go Heap never move. They stay at the same memory address until they are collected.
// 	r1, r2, callErr := proc.Call(uintArgs...)
// 	//runtime.KeepAlive prevents the memory from being deleted, but it does not prevent the stack from being moved.
// 	runtime.KeepAlive(keepAlive) //Using runtime.KeepAlive(args) at the very end of WinCall is the standard Go way to say: "Don't you dare reap these pointers until this line is reached."
// 	err := CheckWinResult(op, check, r1, callErr)
// 	return r1, r2, err
// }

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
		panic("RealProc2: nil dll")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		panic("RealProc2: empty proc name")
	}
	return RealProc(dll.NewProc(name))
}

// BoundFunc represents a bound Windows procedure call with fixed failure semantics.
// WinCall will handle the error type properly, wrap it nicely so if err != nil you can then always do errors.Is on it!
//
// It behaves like a preconfigured syscall wrapper:
//   - accepts raw uintptr arguments
//   - returns (r1, r2, error) consistent with WinCall
//
// A BoundFunc is typically created via BindFunc and should be treated as a
// low-level primitive. Higher-level, type-safe wrappers are recommended for
// production use to avoid repetitive uintptr conversions and to enforce argument
// correctness.
//
// BoundFunc makes no guarantees about argument arity or type safety beyond what
// the underlying Windows API expects.
//type BoundFunc func(args ...any) (uintptr, uintptr, error)

// BindFunc binds a LazyProcish together with a WinCheckFunc into a callable function.
//
// The returned BoundFunc encapsulates:
//   - the procedure to call
//   - the failure detection strategy (check)
//
// When invoked, the BoundFunc delegates to WinCall, ensuring that:
//   - r1, r2 are returned unchanged
//   - the error is non-nil whenever check(r1) indicates failure
//   - Windows errors are wrapped consistently (see CheckWinResult)
//
// This removes the need to repeatedly pass the same WinCheckFunc at each call site.
//
// BindFunc does NOT perform any argument validation; callers are responsible for
// providing the correct number and type (uintptr-convertible) arguments.
//
// Panics:
//   - if proc is nil
//   - if check is nil
// func BindFunc(proc LazyProcish, check WinCheckFunc) BoundFunc {
// 	if proc == nil {
// 		panic("BindFunc: nil proc")
// 	}
// 	if check == nil {
// 		panic("BindFunc: nil check")
// 	}

// 	return func(args ...any) (uintptr, uintptr, error) {
// 		return WinCall(proc, check, args...)
// 	}
// }

// NewBoundProc resolves a procedure from the given DLL and binds it with a
// WinCheckFunc into a ready-to-call BoundFunc.
//
// It is a convenience helper that composes RealProc2 and BindFunc into a single
// step, producing a callable that:
//   - invokes the resolved Windows procedure
//   - applies the provided failure detection strategy (check)
//   - returns (r1, r2, error) with WinCall semantics
//
// This eliminates the need to separately call RealProc2 and BindFunc at the
// declaration site, while still preserving their behavior.
//
// The returned BoundFunc is a low-level wrapper accepting raw uintptr arguments.
// Callers are responsible for passing the correct number and types of arguments.
// For safer and more ergonomic usage, prefer building typed wrappers on top.
//
// Panics:
//   - if dll is nil
//   - if name is empty or whitespace-only
//   - if check is nil
// func NewBoundProc(
// 	dll *windows.LazyDLL,
// 	name string,
// 	check WinCheckFunc,
// ) BoundFunc {
// 	if dll == nil {
// 		panic("NewBoundProc: nil dll")
// 	}

// 	name = strings.TrimSpace(name)
// 	if name == "" {
// 		panic("NewBoundProc: empty proc name")
// 	}

// 	if check == nil {
// 		panic("NewBoundProc: nil check")
// 	}

// 	proc := RealProc(dll.NewProc(name))

// 	return func(args ...any) (uintptr, uintptr, error) {
// 		return WinCall(proc, check, args...)
// 	}
// }

// BoundProc replaces the BoundFunc closure.
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
func (b *BoundProc) Call(args ...uintptr) (uintptr, uintptr, error) {
	return WinCall(b.Proc, b.Check, args...)
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
	// if dll == nil {
	// 	panic("NewBoundProc: nil dll")
	// }
	// name = strings.TrimSpace(name)
	// if name == "" {
	// 	panic("NewBoundProc: empty proc name")
	// }
	if check == nil {
		panic("NewBoundProc: nil WinCheckFunc passed as arg")
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
func WinCall(proc LazyProcish, check WinCheckFunc, args ...uintptr) (uintptr, uintptr, error) {
	if proc == nil {
		panic(fmt.Errorf("WinCall: nil proc"))
	}

	op := strings.TrimSpace(proc.Name())
	if op == "" {
		op = UnspecifiedWinApi
	}
	//fmt.Printf("[GoR:%d] !starting WinCall: %s\n", GoRoutineId(), op)
	//Smashy(true)
	// args is a []uintptr, but because of //go:uintptrescapes, the caller
	// has already pinned the memory safely before we get here.
	r1, r2, callErr := proc.Call(args...)
	//fmt.Printf("[GoR:%d] !checking WinCall: %s\n", GoRoutineId(), op)
	err := CheckWinResult(op, check, r1, callErr)
	//fmt.Printf("[GoR:%d] !ending   WinCall: %s\n", GoRoutineId(), op)
	return r1, r2, err
}

func flush() {
	//fmt.Printf("[GoR:%d] !flushing stderr\n", GoRoutineId())
	os.Stderr.Sync() // Tell Windows to flush the file buffers to disk/console
	//fmt.Printf("[GoR:%d] !flushing stdout\n", GoRoutineId())
	os.Stdout.Sync() // Tell Windows to flush the file buffers to disk/console
}
func Smashy(log bool) {
	if _, exists := os.LookupEnv("WINCOE_SMASHY_TEST"); exists { // only for deliberate stress testing
		if log {
			fmt.Printf("[GoR:%d] !starting Smashy\n", GoRoutineId())
			fmt.Printf("[GoR:%d] ! starting churn()\n", GoRoutineId())
			flush()
		}
		Churn()
		if log {
			fmt.Printf("[GoR:%d] ! ending churn()\n", GoRoutineId())
			fmt.Printf("[GoR:%d] ! starting stackChurn(64)\n", GoRoutineId())
			flush()
		}

		stackChurn(64) // grow stack
		if log {
			fmt.Printf("[GoR:%d] ! ending stackChurn(64)\n", GoRoutineId())
			flush()
		}

		if _, exists2 := os.LookupEnv("WINCOE_SMASHY_RUNGC"); exists2 {
			if log {
				fmt.Printf("[GoR:%d] ! before runtime.GC()\n", GoRoutineId())
				flush()
			}
			runtime.GC() // encourage shrink afterwards
			if log {
				fmt.Printf("[GoR:%d] ! after runtime.GC()\n", GoRoutineId())
				flush()
			}
			if log {
				fmt.Printf("[GoR:%d] ! before runtime.Gosched()\n", GoRoutineId())
				flush()
			}
			runtime.Gosched()
			if log {
				fmt.Printf("[GoR:%d] ! after runtime.Gosched()\n", GoRoutineId())
				flush()
			}
		} //if

		if log {
			fmt.Printf("[GoR:%d] ! starting smashStack\n", GoRoutineId())
			flush()
		}
		smashStack()
		if log {
			fmt.Printf("[GoR:%d] ! ending smashStack\n", GoRoutineId())
			fmt.Printf("[GoR:%d] !ending Smashy\n", GoRoutineId())
			flush()
		}
	}
}

func smashStack() {
	var big [65536]byte
	for i := range big {
		big[i] = 0xCC
	}
}

func Churn() {
	var sink any
	// Force GC + stack pressure
	for i := 0; i < 100; i++ {
		b := make([]byte, 1<<20) // 1MB
		sink = b
	}
	_ = sink
}
func Churn2() {
	var sink any
	// Force GC + stack pressure
	for i := 0; i < 1000; i++ {
		b := make([]byte, 1<<20) // 1MB
		sink = b
	}
	_ = sink
}

func stackChurn(depth int) {
	if depth == 0 {
		return
	}

	// Force a large stack frame (~8KB)
	var buf [8192]byte
	buf[0] = byte(depth) // prevent optimization

	stackChurn(depth - 1)

	// use again to avoid dead-store elimination
	if buf[0] == 255 {
		panic("impossible")
	}
}
