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

// constants used for winapi calls.
// crap(won't re-export): By using a dot import (import . "..."), you effectively "dump" the entire contents of the winconsts package into the local namespace of both winapi and main_api.go.
package wincoe

// const (
// 	WinConsts_Is_Now_USED = 0 // so compiler doesn't complain about not being used! Don't actually use this anywhere! Unfortunately I've to export it which means it's also wincoe exported!
// )

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

//MaxExtendedPath is the maximum character count supported by the Unicode (W) versions of Windows API functions when using the \\?\ prefix, and it's the limit for QueryFullProcessNameW.
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
