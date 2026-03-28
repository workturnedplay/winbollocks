//go:build !windows

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

// TODO: untested, as this whole lib is meant for Windows only.
package wincoe

import (
	"os"
	"syscall"
	//"golang.org/x/sys/unix"
)

/*
Unix:

Terminal = byte stream

Escape sequences = multi-byte

Leftovers are real

Draining matters
*/

// func clearStdin() {
// 	fd := int(os.Stdin.Fd())

// 	var n int
// 	n, err := unix.IoctlGetInt(fd, unix.FIONREAD) // complains in vscode due to gopls(?) while on Windows.
// 	if err != nil || n <= 0 {
// 		return
// 	}

// 	buf := make([]byte, n)
// 	_, _ = os.Stdin.Read(buf)
// }

func ClearStdin() (hadInput bool) {
	fd := int(os.Stdin.Fd())

	syscall.SetNonblock(fd, true)
	defer syscall.SetNonblock(fd, false)

	var buf [64]byte
	for {
		_, err := os.Stdin.Read(buf[:])
		if err != nil {
			hadInput = false
			break
		} else {
			if !hadInput {
				hadInput = true
			}
		}
	}
	return
}

func ReadKeySequence() {
	fd := int(os.Stdin.Fd())

	var b [1]byte
	os.Stdin.Read(b[:]) // block for first byte

	if b[0] != 0x1b {
		return
	}

	syscall.SetNonblock(fd, true)
	defer syscall.SetNonblock(fd, false)

	var buf [8]byte
	for {
		_, err := os.Stdin.Read(buf[:])
		if err != nil {
			break
		}
	}
}
