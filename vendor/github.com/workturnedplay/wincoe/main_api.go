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
	"github.com/workturnedplay/wincoe/internal/consolecolors"
	"github.com/workturnedplay/wincoe/internal/waitanykey"
)

var SetConsoleTextAttribute = consolecolors.SetConsoleTextAttribute
var WithConsoleColor = consolecolors.WithConsoleColor
var GetConsoleScreenBufferAttributes = consolecolors.GetConsoleScreenBufferAttributes
var FOREGROUND_RED = consolecolors.FOREGROUND_RED
var FOREGROUND_GREEN = consolecolors.FOREGROUND_GREEN
var FOREGROUND_BLUE = consolecolors.FOREGROUND_BLUE
var FOREGROUND_NORMAL = consolecolors.FOREGROUND_NORMAL
var FOREGROUND_INTENSITY = consolecolors.FOREGROUND_INTENSITY
var FOREGROUND_GRAY = consolecolors.FOREGROUND_GRAY
var FOREGROUND_YELLOW = consolecolors.FOREGROUND_YELLOW
var FOREGROUND_BRIGHT_YELLOW = consolecolors.FOREGROUND_BRIGHT_YELLOW
var FOREGROUND_MAGENTA = consolecolors.FOREGROUND_MAGENTA
var FOREGROUND_BRIGHT_MAGENTA = consolecolors.FOREGROUND_BRIGHT_MAGENTA
var FOREGROUND_CYAN = consolecolors.FOREGROUND_CYAN
var FOREGROUND_BRIGHT_CYAN = consolecolors.FOREGROUND_BRIGHT_CYAN
var FOREGROUND_WHITE = consolecolors.FOREGROUND_WHITE
var FOREGROUND_BRIGHT_WHITE = consolecolors.FOREGROUND_BRIGHT_WHITE
var FOREGROUND_BRIGHT_RED = consolecolors.FOREGROUND_BRIGHT_RED
var FOREGROUND_BRIGHT_GREEN = consolecolors.FOREGROUND_BRIGHT_GREEN

var ClearStdin = waitanykey.ClearStdin
var WithConsoleEventRaw = waitanykey.WithConsoleEventRaw
var ReadKeySequence = waitanykey.ReadKeySequence
var WaitAnyKeyIfInteractive = waitanykey.WaitAnyKeyIfInteractive
var WaitAnyKey = waitanykey.WaitAnyKey
var IsStdinConsoleInteractive = waitanykey.IsStdinConsoleInteractive
