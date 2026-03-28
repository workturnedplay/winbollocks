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
	if !isFailure(r1) {
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
			errno := windows.Errno(r1)

			// Defensive: avoid ever wrapping ERROR_SUCCESS
			if !errors.Is(errno, windows.ERROR_SUCCESS) {
				// since r1 != 0 already, this is bound to never be ERROR_SUCCESS here, unless r1 != 0 can ever be ERROR_SUCCESS, unsure.
				return fmt.Errorf("%q windows call failed with error: %w", operationNameToIncludeInErrorMessages, errno)
			}
		}

		// Fallback: truly unknown failure
		return fmt.Errorf(
			"%q windows call reported failure (ret=%d) but no usable error was provided",
			operationNameToIncludeInErrorMessages,
			r1,
		)
	}

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
func WinCall(proc LazyProcish, check WinCheckFunc, args ...uintptr) (uintptr, uintptr, error) {
	op := strings.TrimSpace(proc.Name())
	if op == "" {
		op = UnspecifiedWinApi
	}
	r1, r2, callErr := proc.Call(args...)
	err := CheckWinResult(op, check, r1, callErr)
	return r1, r2, err
}

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

// WinCall will handle the error type properly, wrap it nicely so if err != nil you can then always do errors.Is on it!
// BoundFunc represents a bound Windows procedure call with fixed failure semantics.
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
type BoundFunc func(args ...uintptr) (uintptr, uintptr, error)

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
func BindFunc(proc LazyProcish, check WinCheckFunc) BoundFunc {
	if proc == nil {
		panic("BindFunc: nil proc")
	}
	if check == nil {
		panic("BindFunc: nil check")
	}

	return func(args ...uintptr) (uintptr, uintptr, error) {
		return WinCall(proc, check, args...)
	}
}

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
func NewBoundProc(
	dll *windows.LazyDLL,
	name string,
	check WinCheckFunc,
) BoundFunc {
	if dll == nil {
		panic("NewBoundProc: nil dll")
	}

	name = strings.TrimSpace(name)
	if name == "" {
		panic("NewBoundProc: empty proc name")
	}

	if check == nil {
		panic("NewBoundProc: nil check")
	}

	proc := RealProc(dll.NewProc(name))

	return func(args ...uintptr) (uintptr, uintptr, error) {
		return WinCall(proc, check, args...)
	}
}
