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
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"

	"github.com/workturnedplay/wincoe"
)

// shouldLogZOrderDiagnostics gates all "[zdiag]" logging (Win+MMB / Win+Shift+MMB
// z-order investigation). Flip to false once the investigation is done; every
// entry point below returns immediately when it is false.
var shouldLogZOrderDiagnostics = true

const (
	// zdiagMaxOwnedListed caps how many directly-owned windows get listed.
	zdiagMaxOwnedListed = 16
	// zdiagTitleMaxRunes keeps one-line window descriptions readable.
	zdiagTitleMaxRunes = 48
	// zdiagNeighbors is how many z-order neighbours above AND below the target
	// get listed.
	zdiagNeighbors = 4
	// zdiagBottomCount is how many of the bottom-most windows get listed.
	zdiagBottomCount = 3

	// zdiagEventWindow is how long after a z-order command WinEvents get logged.
	zdiagEventWindow = 2 * time.Second
	// zdiagEventMaxLogged caps how many WinEvents get logged per armed window.
	zdiagEventMaxLogged int32 = 200

	// gaParent is GetAncestor's GA_PARENT (not defined in wincoe).
	gaParent uint32 = 1

	// Window style bits not defined in wincoe.
	wsExAppWindow uint32 = 0x00040000
)

// zdiagRecheckDelays are the delays after a z-order change at which the
// z-order is sampled again, to catch a window re-raising itself (or being
// re-raised by a refocus) shortly after we sent it to the back.
var zdiagRecheckDelays = [...]time.Duration{
	30 * time.Millisecond,
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
	all                 []windows.Handle // every walked window, index == z-index (0 = topmost)
	index               int              // target's z-index, -1 if not found
	topmostBandSize     int              // how many leading windows are WS_EX_TOPMOST
	owned               []windows.Handle // windows whose GW_OWNER == target (capped)
	ownedIdx            []int            // z-index of each entry in owned
	ownedTotal          int
	ownerLookupFailures int
	truncated           bool
	err                 error
}

func walkZOrderForDiag(target windows.Handle) zOrderWalk {
	w := zOrderWalk{index: -1}

	hwnd, res := wincoe.GetTopWindow(0)
	if res.Failed() {
		w.err = fmt.Errorf("GetTopWindow(0) failed: %w", res.Err)
		return w
	}

	inTopmostBand := true
	for steps := 0; hwnd != 0; steps++ {
		if steps >= maxZOrderWalkSteps {
			w.truncated = true
			break
		}
		idx := len(w.all)
		w.all = append(w.all, hwnd)

		if inTopmostBand {
			ex, exErr := getWindowLongPtr(hwnd, wincoe.GWL_EXSTYLE)
			// #nosec G115 -- safe: Win32 extended window styles are 32-bit bitmasks
			if exErr == nil && uint32(ex)&wincoe.WS_EX_TOPMOST != 0 {
				w.topmostBandSize++
			} else {
				inTopmostBand = false
			}
		}

		if hwnd == target {
			w.index = idx
		} else {
			o, oErr := getRelatedWindowChecked(hwnd, wincoe.GW_OWNER)
			switch {
			case oErr != nil:
				w.ownerLookupFailures++
			case target != 0 && o == target:
				w.ownedTotal++
				if len(w.owned) < zdiagMaxOwnedListed {
					w.owned = append(w.owned, hwnd)
					w.ownedIdx = append(w.ownedIdx, idx)
				}
			}
		}

		next, nextErr := getRelatedWindowChecked(hwnd, wincoe.GW_HWNDNEXT)
		if nextErr != nil {
			w.err = fmt.Errorf("z-order walk cut short after %d windows: %w", len(w.all), nextErr)
			break
		}
		hwnd = next
	}
	return w
}

// logZOrderSnapshot logs where target currently sits in the top-level z-order:
// its z-index, the size of the topmost band, its neighbours, its directly-owned
// windows (with their own z-indices), the bottom-most windows and the
// foreground window.
func logZOrderSnapshot(label string, target windows.Handle, verboseTarget bool) {
	if !shouldLogZOrderDiagnostics {
		return
	}
	w := walkZOrderForDiag(target)
	fg := wincoe.GetForegroundWindow()
	total := len(w.all)

	logf("[zdiag] %s: target=%s", label, describeWindow(target, verboseTarget))
	logf("[zdiag] %s: foreground=%s", label, describeWindow(fg, false))
	if w.err != nil {
		logf("[zdiag] %s: z-order walk incomplete: %v", label, w.err)
	}
	if w.truncated {
		logf("[zdiag] %s: z-order walk truncated at %d windows", label, maxZOrderWalkSteps)
	}
	logf("[zdiag] %s: %d top-level windows walked; the first %d are WS_EX_TOPMOST, so the non-topmost band starts at z-index %d",
		label, total, w.topmostBandSize, w.topmostBandSize)

	mark := func(h windows.Handle) string {
		s := ""
		if h == target {
			s += " <== TARGET"
		}
		if h == fg {
			s += " [FOREGROUND]"
		}
		return s
	}

	if w.index < 0 {
		logf("[zdiag] %s: target NOT found among the %d top-level windows walked", label, total)
	} else {
		bottomNote := ""
		if w.index == total-1 && !w.truncated && w.err == nil {
			bottomNote = " -> target IS bottom-most"
		}
		logf("[zdiag] %s: target z-index=%d of %d (0=topmost)%s", label, w.index, total, bottomNote)
		for i := max(0, w.index-zdiagNeighbors); i <= min(total-1, w.index+zdiagNeighbors); i++ {
			logf("[zdiag] %s:   near z[%d] %s%s", label, i, describeWindow(w.all[i], false), mark(w.all[i]))
		}
	}

	logf("[zdiag] %s: windows directly owned by target (GW_OWNER==target): %d", label, w.ownedTotal)
	for i, h := range w.owned {
		logf("[zdiag] %s:   owned z[%d] %s", label, w.ownedIdx[i], describeWindow(h, false))
	}
	if w.ownerLookupFailures > 0 {
		logf("[zdiag] %s: GW_OWNER lookup failed for %d window(s) during the walk (probably destroyed mid-walk)", label, w.ownerLookupFailures)
	}

	for i := max(0, total-zdiagBottomCount); i < total; i++ {
		logf("[zdiag] %s:   bottom z[%d] %s%s", label, i, describeWindow(w.all[i], false), mark(w.all[i]))
	}
}

// scheduleZOrderRechecks samples the z-order again after each of
// zdiagRecheckDelays, so a window that re-raises itself (or gets re-raised by
// our refocus) shortly after being sent to the back shows up in the log.
func scheduleZOrderRechecks(label string, target windows.Handle) {
	for _, delay := range zdiagRecheckDelays {
		time.AfterFunc(delay, func() {
			runDiagSafely("scheduleZOrderRechecks", func() {
				logZOrderSnapshot(fmt.Sprintf("%s +%dms", label, delay.Milliseconds()), target, false)
			})
		})
	}
}

// ---- WinEvent logging around a z-order command -----------------------------

var (
	// zdiagEventArmedUntilUnixNano is 0 when WinEvent logging is off.
	zdiagEventArmedUntilUnixNano atomic.Int64
	zdiagEventBudget             atomic.Int32
	zdiagEventTargetPID          atomic.Uint32
	zdiagEventTargetTID          atomic.Uint32
)

// armZOrderEventLogging makes winEventProc log relevant WinEvents for
// zdiagEventWindow: REORDER and FOREGROUND from anyone, plus SHOW/HIDE/CREATE/
// DESTROY/FOCUS from the target's process. Our own process's events are never
// delivered (the hook uses WINEVENT_SKIPOWNPROCESS), so anything logged here
// was caused by someone else -- most interestingly, by the target itself.
func armZOrderEventLogging(target windows.Handle) {
	var pid uint32
	tid, res := wincoe.GetWindowThreadProcessId(target, &pid)
	if res.Failed() {
		logf("[zdiag] armZOrderEventLogging: GetWindowThreadProcessId(HWND=0x%X) failed, only PID-independent events will be logged: %v", target, res.Err)
		pid, tid = 0, 0
	}
	zdiagEventTargetPID.Store(pid)
	zdiagEventTargetTID.Store(tid)
	zdiagEventBudget.Store(zdiagEventMaxLogged)
	zdiagEventArmedUntilUnixNano.Store(time.Now().Add(zdiagEventWindow).UnixNano())
}

func winEventName(event uint32) string {
	switch event {
	case wincoe.EVENT_SYSTEM_FOREGROUND:
		return "SYSTEM_FOREGROUND"
	case wincoe.EVENT_OBJECT_CREATE:
		return "OBJECT_CREATE"
	case wincoe.EVENT_OBJECT_DESTROY:
		return "OBJECT_DESTROY"
	case wincoe.EVENT_OBJECT_SHOW:
		return "OBJECT_SHOW"
	case wincoe.EVENT_OBJECT_HIDE:
		return "OBJECT_HIDE"
	case wincoe.EVENT_OBJECT_REORDER:
		return "OBJECT_REORDER"
	case wincoe.EVENT_OBJECT_FOCUS:
		return "OBJECT_FOCUS"
	default:
		return fmt.Sprintf("0x%X", event)
	}
}

// diagLogWinEvent is called from the very top of winEventProc (main thread).
// It is one atomic load when not armed.
func diagLogWinEvent(event uint32, hwnd windows.Handle, idObject, idChild int32, eventThread, eventTime uint32) {
	if !shouldLogZOrderDiagnostics {
		return
	}
	until := zdiagEventArmedUntilUnixNano.Load()
	if until == 0 || time.Now().UnixNano() > until {
		return
	}

	switch event {
	case wincoe.EVENT_SYSTEM_FOREGROUND:
		// always relevant
	case wincoe.EVENT_OBJECT_SHOW, wincoe.EVENT_OBJECT_HIDE, wincoe.EVENT_OBJECT_CREATE,
		wincoe.EVENT_OBJECT_DESTROY, wincoe.EVENT_OBJECT_FOCUS, wincoe.EVENT_OBJECT_REORDER:
		targetPID := zdiagEventTargetPID.Load()
		if hwnd == 0 || targetPID == 0 {
			return
		}
		var pid uint32
		if _, res := wincoe.GetWindowThreadProcessId(hwnd, &pid); res.Failed() || pid != targetPID {
			return
		}
	default:
		return
	}

	if left := zdiagEventBudget.Add(-1); left < 0 {
		if left == -1 {
			logf("[zdiag] WINEVENT logging budget (%d) exhausted for this z-order command; suppressing the rest", zdiagEventMaxLogged)
		}
		return
	}

	runDiagSafely("diagLogWinEvent", func() {
		logf("[zdiag] WINEVENT %s idObject=%d idChild=%d eventTime=%d eventThread=%d (target UI thread=%d, our main thread=%d) hwnd: %s",
			winEventName(event), idObject, idChild, eventTime, eventThread,
			zdiagEventTargetTID.Load(), windows.GetCurrentThreadId(), describeWindow(hwnd, false))
	})
}

// ---- entry points called from handleActualMoveOrResize / mouseProc ---------

// diagZOrderBefore must be called right before SetWindowPos is issued for a
// queued z-order command (main thread, handleActualMoveOrResize).
func diagZOrderBefore(data WindowMoveData) {
	if !shouldLogZOrderDiagnostics || data.ZOrderAction == zOrderActionNone {
		return
	}
	runDiagSafely("diagZOrderBefore", func() {
		armZOrderEventLogging(data.Hwnd)
		logf("[zdiag] ===== about to apply %v to HWND=0x%X: InsertAfter=0x%X Flags=0x%X wasForegroundBefore=%v unfocusAfter=%v restoreID=%d =====",
			data.ZOrderAction, data.Hwnd, data.InsertAfter, data.Flags,
			data.TargetWasForegroundBeforeSendToBack, data.UnfocusAfterSuccessfulSendToBack, data.SentToBackRestoreID)
		logZOrderSnapshot("BEFORE", data.Hwnd, true)
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
		logZOrderSnapshot("AFTER SetWindowPos (before any refocus)", data.Hwnd, false)
	})
}

// diagZOrderSettled must be called after the ZOrderAction switch (i.e. after
// any refocus): logs the final immediate state and schedules delayed rechecks.
func diagZOrderSettled(data WindowMoveData) {
	if !shouldLogZOrderDiagnostics || data.ZOrderAction == zOrderActionNone {
		return
	}
	runDiagSafely("diagZOrderSettled", func() {
		logZOrderSnapshot("AFTER refocus logic", data.Hwnd, false)
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
