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

// Package wincoe aka winco(r)e, are common functions I use across my projects to keep things DRY.
package wincoe

import (
// "github.com/workturnedplay/wincoe/internal/consolecolors"
// "github.com/workturnedplay/wincoe/internal/waitanykey"
// "github.com/workturnedplay/wincoe/internal/winapi"
// "github.com/workturnedplay/wincoe/internal/wincall"
// // dot import these so I don't have to prefix them and thus are exported (if capitalized in winconsts) as they are - NO they're not re-exported, crap! Gemini leading me astray.
// . "github.com/workturnedplay/wincoe/internal/winconsts"
)

// nvmFIXME: all these 'var' must be replaced with globalAPI (see below) because they lose the top-level docs this way.
// var SetConsoleTextAttribute = consolecolors.SetConsoleTextAttribute
// var WithConsoleColor = consolecolors.WithConsoleColor
// var GetConsoleScreenBufferAttributes = consolecolors.GetConsoleScreenBufferAttributes

// var FOREGROUND_RED = consolecolors.FOREGROUND_RED
// var FOREGROUND_GREEN = consolecolors.FOREGROUND_GREEN
// var FOREGROUND_BLUE = consolecolors.FOREGROUND_BLUE
// var FOREGROUND_NORMAL = consolecolors.FOREGROUND_NORMAL
// var FOREGROUND_INTENSITY = consolecolors.FOREGROUND_INTENSITY
// var FOREGROUND_GRAY = consolecolors.FOREGROUND_GRAY
// var FOREGROUND_YELLOW = consolecolors.FOREGROUND_YELLOW
// var FOREGROUND_BRIGHT_YELLOW = consolecolors.FOREGROUND_BRIGHT_YELLOW
// var FOREGROUND_MAGENTA = consolecolors.FOREGROUND_MAGENTA
// var FOREGROUND_BRIGHT_MAGENTA = consolecolors.FOREGROUND_BRIGHT_MAGENTA
// var FOREGROUND_CYAN = consolecolors.FOREGROUND_CYAN
// var FOREGROUND_BRIGHT_CYAN = consolecolors.FOREGROUND_BRIGHT_CYAN
// var FOREGROUND_WHITE = consolecolors.FOREGROUND_WHITE
// var FOREGROUND_BRIGHT_WHITE = consolecolors.FOREGROUND_BRIGHT_WHITE
// var FOREGROUND_BRIGHT_RED = consolecolors.FOREGROUND_BRIGHT_RED
// var FOREGROUND_BRIGHT_GREEN = consolecolors.FOREGROUND_BRIGHT_GREEN

// var ClearStdin = waitanykey.ClearStdin
// var WithConsoleEventRaw = waitanykey.WithConsoleEventRaw
// var ReadKeySequence = waitanykey.ReadKeySequence
// var WaitAnyKeyIfInteractive = waitanykey.WaitAnyKeyIfInteractive
// var WaitAnyKey = waitanykey.WaitAnyKey
// var IsStdinConsoleInteractive = waitanykey.IsStdinConsoleInteractive

// var RealProc = wincall.RealProc
// var WinCall = wincall.WinCall
// var RealProc2 = wincall.RealProc2
// var BindFunc = wincall.BindFunc
// var NewBoundProc = wincall.NewBoundProc

// var GetExtendedUdpTable = winapi.GetExtendedUDPTable

// doneFIXME: their top-level doc is lost this way!
//var AF_INET = winapi.AF_INET
// var UDP_TABLE_OWNER_PID = winapi.UDP_TABLE_OWNER_PID

// const (
// 	_ = WinConsts_Is_Now_USED // to silence compiler error which happens due to dot import(intended for re-exporting) and not using anything from it inhere.

// 	// AF_INET = winapi.AF_INET
// 	// // UDP_TABLE_OWNER_PID is re-exported from winapi.
// 	// //
// 	// // See winapi.UDP_TABLE_OWNER_PID.
// 	// //
// 	// // See github.com/workturnedplay/wincoe/internal/winapi.UDP_TABLE_OWNER_PID.
// 	// UDP_TABLE_OWNER_PID = winapi.UDP_TABLE_OWNER_PID
// )

// // globalAPI combines all sub-modules, necessary to inherit the top-level documentation of each function(well, method), keeping it DRY.
// // ie. method promotion via embedding
// // Methods are the only thing in Go that can be "promoted" through embedding while keeping their documentation linked to the original source.
// type globalAPI struct {
// 	//winapi.Exported // Pulls in GetExtendedUdpTable, etc.
// }

// // A is a way to access the public API thru such that the submodules within DRY their top-level docs as they would if A weren't the middle-man.
// var A = globalAPI{
// 	//winapi.Exported{},
// 	// ...
// }
