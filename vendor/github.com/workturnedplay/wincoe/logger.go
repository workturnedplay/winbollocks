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
	"io"
	"log/slog"
)

// Logger - exported global logger. Defaults to a "do nothing" logger.
// So if this wincoe lib ever wants to log things it uses this Logger to do so, currently it doesn't need to!
//
// Set this in caller(lib user) like:
//
// wincoe.Logger = slog.Default()
//
// this way this wincoe lib will log to where caller wants.
var Logger *slog.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
