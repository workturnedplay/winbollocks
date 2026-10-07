//go:build windows && amd64

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

package main

import (
	"fmt"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/workturnedplay/wincoe"
)

// shouldLogZOrderDiagnostics gates all "[zdiag]" logging (Win+MMB / Win+Shift+MMB
// z-order investigation). Flip to false once the investigation is done; every
// entry point below returns immediately when it is false.
//
// Log volume is roughly 50-70 lines per z-order gesture, so don't leave it on
// permanently.
var shouldLogZOrderDiagnostics = true

const (
	// zdiagMaxWalkSteps bounds the top-level z-order walk (hidden windows are
	// included in the walk, so real-world counts of a few hundred are normal).
	zdiagMaxWalkSteps = 1000
	// zdiagMaxOwnedListed caps how many directly-owned windows get listed.
	zdiagMaxOwnedListed = 16
	// zdiagTitleMaxRunes keeps one-line window descriptions readable.
	zdiagTitleMaxRunes = 48

	// gaParent is GetAncestor's GA_PARENT (not defined in wincoe).
	gaParent uint32 = 1

	// Window style bits not defined in wincoe.
	wsMinimize    uint32 = 0x20000000
	wsExAppWindow uint32 = 0x00040000
)

// zdiagRecheckDelays are the delays after a z-order change at which the
// z-order is sampled again, to catch a window re-raising itself (or being
// re-raised by a refocus) shortly after we sent it to the back.
var zdiagRecheckDelays = [...]time.Duration{
	30 * time.Millisecond,
	120 * time.Millisecond,
	400 * time.Millisecond,
	1500 * time.Millisecond,
}

// String makes zOrderAction readable in logs (%v / %s).
func (a zOrderAction) String() string {
	switch a {
	case zOrderActionNone:
		return "none"
	case zOrderActionSendToBack:
		return "send-to-back"
	case zOrderActionRestoreStackEntry:
		return "restore-stack-entry"
	case zOrderActionRestoreFocused:
		return "restore-focused"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(a))
	}
}

// runDiagSafely runs fn and swallows (but reports) any panic: diagnostics must
// never be able to take the process down, and some of them run on timer/helper
// goroutines that have no primary_defer() safety net above them.
func runDiagSafely(what string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			directLoggerf("[zdiag] %s panicked (diagnostics only, ignoring): %v", what, r)
		}
	}()
	fn()
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// fmtHandleOrErr renders a handle lookup result for logs.
func fmtHandleOrErr(h windows.Handle, err error) string {
	if err != nil {
		return fmt.Sprintf("<error: %v>", err)
	}
	if h == 0 {
		return "none"
	}
	return fmt.Sprintf("0x%X", h)
}

// getRelatedWindowChecked wraps wincoe.GetWindow. A (0, nil) result means "no
// such related window" (e.g. no owner); an error means the call really failed.
func getRelatedWindowChecked(hwnd windows.Handle, uCmd uint32) (windows.Handle, error) {
	res := wincoe.GetWindow(hwnd, uCmd)
	if res.Failed() {
		return 0, fmt.Errorf("GetWindow(HWND=0x%X, uCmd=%d) failed: %w", hwnd, uCmd, res.Err)
	}
	return windows.Handle(res.R1), nil
}

// readStyleAndExStyle reads GWL_STYLE and GWL_EXSTYLE as 32-bit masks.
func readStyleAndExStyle(hwnd windows.Handle) (style, exStyle uint32, err error) {
	s, err1 := getWindowLongPtr(hwnd, wincoe.GWL_STYLE)
	if err1 != nil {
		return 0, 0, fmt.Errorf("read GWL_STYLE of HWND=0x%X: %w", hwnd, err1)
	}
	e, err2 := getWindowLongPtr(hwnd, wincoe.GWL_EXSTYLE)
	if err2 != nil {
		return 0, 0, fmt.Errorf("read GWL_EXSTYLE of HWND=0x%X: %w", hwnd, err2)
	}
	// #nosec G115 -- safe: Win32 window styles are 32-bit bitmasks
	return uint32(s), uint32(e), nil
}

func decodeWindowFlags(s, ex uint32) string {
	var f []string
	add := func(cond bool, name string) {
		if cond {
			f = append(f, name)
		}
	}
	add(s&wincoe.WS_VISIBLE != 0, "VISIBLE")
	add(s&wincoe.WS_DISABLED != 0, "DISABLED")
	add(s&wsMinimize != 0, "MINIMIZED")
	add(s&wincoe.WS_POPUP != 0, "POPUP")
	add(s&wincoe.WS_CHILD != 0, "CHILD")
	add(s&wincoe.WS_CAPTION == wincoe.WS_CAPTION, "CAPTION")
	add(ex&wincoe.WS_EX_TOPMOST != 0, "TOPMOST")
	add(ex&wincoe.WS_EX_TOOLWINDOW != 0, "TOOLWINDOW")
	add(ex&wincoe.WS_EX_NOACTIVATE != 0, "NOACTIVATE")
	add(ex&wsExAppWindow != 0, "APPWINDOW")
	add(ex&wincoe.WS_EX_LAYERED != 0, "LAYERED")
	add(ex&wincoe.WS_EX_TRANSPARENT != 0, "TRANSPARENT")
	if len(f) == 0 {
		return "-"
	}
	return strings.Join(f, "|")
}

// describeWindow renders a one-line description of hwnd. verbose adds
// parent/root, raw style masks, rect and maximized state.
func describeWindow(hwnd windows.Handle, verbose bool) string {
	if hwnd == 0 {
		return "HWND=0x0 (none)"
	}
	if !wincoe.IsWindow(hwnd) {
		return fmt.Sprintf("HWND=0x%X (no longer a valid window)", hwnd)
	}

	class, resClass := wincoe.GetClassName(hwnd)
	if resClass.Failed() {
		class = fmt.Sprintf("<GetClassName failed: %v>", resClass.Err)
	}
	title := truncateRunes(getWindowTextFast(hwnd), zdiagTitleMaxRunes)

	pid := getWindowPID(hwnd)
	exe := "<unknown>"
	if pid != 0 {
		exe = getProcessNameFast(pid)
	}

	style, exStyle, styleErr := readStyleAndExStyle(hwnd)
	var flags string
	if styleErr != nil {
		flags = fmt.Sprintf("<unreadable: %v>", styleErr)
	} else {
		flags = decodeWindowFlags(style, exStyle)
	}

	owner, ownerErr := getRelatedWindowChecked(hwnd, wincoe.GW_OWNER)
	rootOwner, rootOwnerErr := getAncestorChecked(hwnd, wincoe.GA_ROOTOWNER)

	var b strings.Builder
	fmt.Fprintf(&b, "HWND=0x%X class=%q title=%q exe=%s pid=%d flags=%s owner=%s rootOwner=%s",
		hwnd, class, title, exe, pid, flags,
		fmtHandleOrErr(owner, ownerErr), fmtHandleOrErr(rootOwner, rootOwnerErr))
	if isOwnWindow(hwnd) {
		b.WriteString(" (SELF)")
	}
	if !verbose {
		return b.String()
	}

	parent, parentErr := getAncestorChecked(hwnd, gaParent)
	root, rootErr := getAncestorChecked(hwnd, wincoe.GA_ROOT)
	fmt.Fprintf(&b, " parent=%s root=%s", fmtHandleOrErr(parent, parentErr), fmtHandleOrErr(root, rootErr))
	if styleErr == nil {
		fmt.Fprintf(&b, " style=0x%08X exStyle=0x%08X", style, exStyle)
	}
	var r wincoe.RECT
	if res := wincoe.GetWindowRect(hwnd, &r); res.Failed() {
		fmt.Fprintf(&b, " rect=<GetWindowRect failed: %v>", res.Err)
	} else {
		fmt.Fprintf(&b, " rect=(%d,%d)-(%d,%d)", r.Left, r.Top, r.Right, r.Bottom)
	}
	fmt.Fprintf(&b, " maximized=%v", isMaximized(hwnd))
	return b.String()
}

// zOrderWalk is the result of one top-to-bottom walk of the top-level z-order
// (hidden windows included).
type zOrderWalk struct {
	index               int // target's 0-based index (0 = topmost), -1 if not found
	total               int // how many top-level windows were walked
	above, below        windows.Handle
	top                 []windows.Handle
	owned               []windows.Handle // windows whose GW_OWNER == target (capped)
	ownedTotal          int
	ownerLookupFailures int
	truncated           bool
	err                 error
}

func walkZOrderForDiag(target windows.Handle, topCount int) zOrderWalk {
	w := zOrderWalk{index: -1}

	hwnd, res := wincoe.GetTopWindow(0)
	if res.Failed() {
		w.err = fmt.Errorf("GetTopWindow(0) failed: %w", res.Err)
		return w
	}

	var prev windows.Handle
	for steps := 0; hwnd != 0; steps++ {
		if steps >= zdiagMaxWalkSteps {
			w.truncated = true
			break
		}
		if len(w.top) < topCount {
			w.top = append(w.top, hwnd)
		}
		if target != 0 && prev == target && w.below == 0 {
			w.below = hwnd
		}
		if hwnd == target {
			w.index = w.total
			w.above = prev
		} else {
			o, oErr := getRelatedWindowChecked(hwnd, wincoe.GW_OWNER)
			switch {
			case oErr != nil:
				w.ownerLookupFailures++
			case target != 0 && o == target:
				w.ownedTotal++
				if len(w.owned) < zdiagMaxOwnedListed {
					w.owned = append(w.owned, hwnd)
				}
			}
		}
		w.total++
		prev = hwnd

		next := wincoe.GetWindow(hwnd, wincoe.GW_HWNDNEXT)
		if next.Failed() {
			w.err = fmt.Errorf("GetWindow(HWND=0x%X, GW_HWNDNEXT) failed mid-walk after %d windows: %w", hwnd, w.total, next.Err)
			break
		}
		hwnd = windows.Handle(next.R1)
	}
	return w
}

// logZOrderSnapshot logs where target currently sits in the top-level z-order,
// its neighbours, its directly-owned windows, the foreground window and the
// topCount topmost windows.
func logZOrderSnapshot(label string, target windows.Handle, topCount int, verboseTarget bool) {
	if !shouldLogZOrderDiagnostics {
		return
	}
	w := walkZOrderForDiag(target, topCount)
	fg := wincoe.GetForegroundWindow()

	logf("[zdiag] %s: target=%s", label, describeWindow(target, verboseTarget))
	logf("[zdiag] %s: foreground=%s", label, describeWindow(fg, false))
	if w.err != nil {
		logf("[zdiag] %s: z-order walk incomplete: %v", label, w.err)
	}
	if w.truncated {
		logf("[zdiag] %s: z-order walk truncated at %d windows", label, zdiagMaxWalkSteps)
	}

	if w.index < 0 {
		logf("[zdiag] %s: target NOT found among the %d top-level windows walked", label, w.total)
	} else {
		bottomNote := ""
		if w.index == w.total-1 && !w.truncated && w.err == nil {
			bottomNote = " -> target IS bottom-most"
		}
		logf("[zdiag] %s: target z-index=%d of %d (0=topmost)%s", label, w.index, w.total, bottomNote)
		logf("[zdiag] %s:   directly ABOVE target: %s", label, describeWindow(w.above, false))
		logf("[zdiag] %s:   directly BELOW target: %s", label, describeWindow(w.below, false))
	}

	logf("[zdiag] %s: windows directly owned by target (GW_OWNER==target): %d", label, w.ownedTotal)
	for _, h := range w.owned {
		logf("[zdiag] %s:   owned: %s", label, describeWindow(h, false))
	}
	if w.ownerLookupFailures > 0 {
		logf("[zdiag] %s: GW_OWNER lookup failed for %d window(s) during the walk (probably destroyed mid-walk)", label, w.ownerLookupFailures)
	}

	for i, h := range w.top {
		marks := ""
		if h == target {
			marks += " <== TARGET"
		}
		if h == fg {
			marks += " [FOREGROUND]"
		}
		logf("[zdiag] %s:   z[%d] %s%s", label, i, describeWindow(h, false), marks)
	}
}

// scheduleZOrderRechecks samples the z-order again after each of
// zdiagRecheckDelays, so a window that re-raises itself (or gets re-raised by
// our refocus) shortly after being sent to the back shows up in the log.
func scheduleZOrderRechecks(label string, target windows.Handle) {
	for _, delay := range zdiagRecheckDelays {
		time.AfterFunc(delay, func() {
			runDiagSafely("scheduleZOrderRechecks", func() {
				logZOrderSnapshot(fmt.Sprintf("%s +%dms", label, delay.Milliseconds()), target, 5, false)
			})
		})
	}
}

// diagZOrderBefore must be called right before SetWindowPos is issued for a
// queued z-order command (main thread, handleActualMoveOrResize).
func diagZOrderBefore(data WindowMoveData) {
	if !shouldLogZOrderDiagnostics || data.ZOrderAction == zOrderActionNone {
		return
	}
	runDiagSafely("diagZOrderBefore", func() {
		logf("[zdiag] ===== about to apply %v to HWND=0x%X: InsertAfter=0x%X Flags=0x%X wasForegroundBefore=%v unfocusAfter=%v restoreID=%d =====",
			data.ZOrderAction, data.Hwnd, data.InsertAfter, data.Flags,
			data.TargetWasForegroundBeforeSendToBack, data.UnfocusAfterSuccessfulSendToBack, data.SentToBackRestoreID)
		logZOrderSnapshot("BEFORE", data.Hwnd, 8, true)
	})
}

// diagZOrderAfterSetWindowPos must be called right after a successful
// SetWindowPos for a z-order command, before any refocus logic runs: it shows
// whether the OS itself honored the request.
func diagZOrderAfterSetWindowPos(data WindowMoveData) {
	if !shouldLogZOrderDiagnostics || data.ZOrderAction == zOrderActionNone {
		return
	}
	runDiagSafely("diagZOrderAfterSetWindowPos", func() {
		logZOrderSnapshot("AFTER SetWindowPos (before any refocus)", data.Hwnd, 6, false)
	})
}

// diagZOrderSettled must be called after the ZOrderAction switch (i.e. after
// any refocus): logs the final immediate state and schedules delayed rechecks.
func diagZOrderSettled(data WindowMoveData) {
	if !shouldLogZOrderDiagnostics || data.ZOrderAction == zOrderActionNone {
		return
	}
	runDiagSafely("diagZOrderSettled", func() {
		logZOrderSnapshot("AFTER refocus logic", data.Hwnd, 6, false)
		scheduleZOrderRechecks("RECHECK", data.Hwnd)
	})
}

// diagLogRefocusCandidate logs which window is about to receive focus after
// target was sent to the back, and whether it's related to target (same
// process / same owner group): activating a related window can pull the whole
// group, target included, back to the top.
func diagLogRefocusCandidate(target, candidate windows.Handle) {
	if !shouldLogZOrderDiagnostics {
		return
	}
	runDiagSafely("diagLogRefocusCandidate", func() {
		targetRoot, targetRootErr := rootOwnerOf(target)
		candidateRoot, candidateRootErr := rootOwnerOf(candidate)
		targetPID := getWindowPID(target)
		candidatePID := getWindowPID(candidate)
		sameGroup := targetRootErr == nil && candidateRootErr == nil && targetRoot == candidateRoot
		logf("[zdiag] refocus candidate after sending HWND=0x%X to back: %s | samePID=%v sameOwnerGroup=%v (targetRootOwner=%s candidateRootOwner=%s)",
			target, describeWindow(candidate, false),
			targetPID != 0 && targetPID == candidatePID, sameGroup,
			fmtHandleOrErr(targetRoot, targetRootErr), fmtHandleOrErr(candidateRoot, candidateRootErr))
	})
}

// diagLogMMBTargetResolution is called from the hook thread, so it only does
// one cheap getter there (WindowFromPoint) and hands everything expensive
// (process-name lookups etc.) to a helper goroutine.
func diagLogMMBTargetResolution(pt wincoe.POINT, resolvedRoot windows.Handle) {
	if !shouldLogZOrderDiagnostics {
		return
	}
	rawHwnd := wincoe.WindowFromPoint(pt)
	go runDiagSafely("diagLogMMBTargetResolution", func() {
		logf("[zdiag] Win+MMB at (%d,%d): raw WindowFromPoint=%s", pt.X, pt.Y, describeWindow(rawHwnd, false))
		logf("[zdiag] Win+MMB resolved top-level target (via GA_ROOT): %s", describeWindow(resolvedRoot, true))
	})
}