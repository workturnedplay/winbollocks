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
	"errors"
	"fmt"
	//"github.com/workturnedplay/wincoe/internal/wincall"
	"golang.org/x/sys/windows"
)

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	//procSetConsoleTextAttribute  = wincall.RealProc(kernel32.NewProc("SetConsoleTextAttribute"))
	//procSetConsoleTextAttribute2 = wincall.BindFunc(wincall.RealProc2(kernel32, "SetConsoleTextAttribute"), wincall.CheckBool)
	callSetConsoleTextAttribute3 = NewBoundProc(kernel32, "SetConsoleTextAttribute", CheckBool)
)

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

// // Quick one-liners for common cases (optional, but cleaner usage)
// func WithInfoColor(fn func()) error {
// 	return WithConsoleColor(FOREGROUND_WHITE, fn)
// }

// func WithWarnColor(fn func()) error {
// 	return WithConsoleColor(FOREGROUND_BRIGHT_MAGENTA, fn)
// }

// func WithErrorColor(fn func()) error {
// 	return WithConsoleColor(FOREGROUND_RED|FOREGROUND_INTENSITY, fn) // bright red
// }

// func WithDebugColor(fn func()) error {
// 	return WithConsoleColor(FOREGROUND_GRAY, fn) // dim gray
// }

// SetConsoleTextAttribute used to set the color for the text next printed on console
func SetConsoleTextAttribute(h windows.Handle, color uint16) error {
	// We use CheckBool because the docs say this returns a BOOL.
	// We pass nil for onFail because we just want to return the error to the caller.
	// Execute the syscall
	// //works:
	// r1, _, callErr := procSetConsoleTextAttribute.Call(uintptr(h), uintptr(color))
	// err := CheckWinResult(CheckBool, r1, callErr)

	// //works:
	// _, _, err := wincall.WinCall(procSetConsoleTextAttribute, wincall.CheckBool, uintptr(h), uintptr(color))

	// //works too:
	// _, _, err := procSetConsoleTextAttribute2(uintptr(h), uintptr(color))

	//works too:
	_, _, err := callSetConsoleTextAttribute3(uintptr(h), uintptr(color))

	return err
}
