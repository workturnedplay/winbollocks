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
package wincall

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
	// CheckBool identifies a failure for functions returning a Windows BOOL.
	// In the Windows API, a 0 (FALSE) indicates that the function failed.
	CheckBool WinCheckFunc = func(r1 uintptr) bool { return r1 == 0 }

	// CheckHandle identifies a failure for functions returning a HANDLE.
	// Many Windows APIs return INVALID_HANDLE_VALUE (all bits set to 1) on failure.
	// ^uintptr(0) is the Go-idiomatic way to represent -1 as an unsigned pointer.
	CheckHandle WinCheckFunc = func(r1 uintptr) bool { return r1 == ^uintptr(0) }

	// CheckNull identifies a failure for functions returning a pointer or a handle
	// where a NULL value (0) indicates the operation could not be completed.
	CheckNull WinCheckFunc = func(r1 uintptr) bool { return r1 == 0 }

	// CheckHRESULT identifies a failure for functions that return an HRESULT.
	// An HRESULT is a 32-bit value where a negative number (high bit set)
	// indicates an error, while 0 or positive values indicate success.
	CheckHRESULT WinCheckFunc = func(r1 uintptr) bool { return int32(r1) < 0 }
)

// CheckWinResult processes a Windows API result.
// It returns nil on success (when isFailure is false).
// On failure, it returns a wrapped error.
// Use errors.Is whenever you want to check whether an error matches a particular sentinel value, like windows.ERROR_ACCESS_DENIED or windows.ERROR_SUCCESS.
// This works even if the error was wrapped with %w in fmt.Errorf, which is exactly what this helper does.
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

	var finalErr error

	// If the system says failure but the error code is 0/nil,
	// we return a concrete error message WITHOUT wrapping ERROR_SUCCESS.
	if callErr == nil || errors.Is(callErr, windows.ERROR_SUCCESS) {
		finalErr = fmt.Errorf("%q windows call reported failure (ret=%d) but LastError(aka callErr) was 0", operationNameToIncludeInErrorMessages, r1)
	} else {
		//finalErr = callErr //unwrapped
		// We only use %w when there is a REAL error to wrap.
		finalErr = fmt.Errorf("%q windows call failed with error: %w", operationNameToIncludeInErrorMessages, callErr)
	}

	return finalErr
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
