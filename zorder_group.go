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
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/workturnedplay/wincoe"
)

const (
	// zOrderOnlyFlags: change nothing but the z-order, don't activate.
	zOrderOnlyFlags uint32 = wincoe.SWP_NOMOVE | wincoe.SWP_NOSIZE | wincoe.SWP_NOACTIVATE

	// maxZOrderWalkSteps bounds every top-level z-order walk in this file
	// (hidden windows are included in such walks; a few hundred is normal).
	maxZOrderWalkSteps = 1000

	// maxSameProcessChain bounds the run of consecutive same-process windows
	// directly below the target that sameProcessWindowsDirectlyBelow collects.
	maxSameProcessChain = 64

	// wsMinimize is WS_MINIMIZE (not defined in wincoe).
	wsMinimize uint32 = 0x20000000

	// dwmwaCloaked is DWMWA_CLOAKED for DwmGetWindowAttribute.
	dwmwaCloaked uint32 = 14

	// maxBlockersLogged caps how many "visible foreign window still below the
	// target" entries ensureSentToBack lists (and fetches) per verification.
	maxBlockersLogged = 5

	// explorerExeName is the shell process whose desktop host windows
	// (Progman/WorkerW) legitimately sit below every normal window.
	explorerExeName = "explorer.exe"

	// maxBlockersRaised caps how many windows stageRaiseBlockersAboveTarget
	// restacks in one go.
	maxBlockersRaised = 64

	// swMinimize / swShowMinNoActive are ShowWindow's SW_MINIMIZE (minimize
	// and activate the next window in z-order) and SW_SHOWMINNOACTIVE
	// (minimize without touching activation); neither is defined in wincoe.
	swMinimize        int32 = 6
	swShowMinNoActive int32 = 7
)

// procDwmGetWindowAttributeCloaked is DwmGetWindowAttribute bound for the
// DWORD-sized DWMWA_CLOAKED query (wincoe's own binding of the same API is
// private to it).
var procDwmGetWindowAttributeCloaked = wincoe.NewBoundProc4(wincoe.Dwmapi, "DwmGetWindowAttribute", wincoe.CheckHRESULT)

// errSendToBackFailed is wrapped by ensureSentToBack's returned error when the
// window was verified to still NOT be visually at the back after every stage.
// Any other error from it means "couldn't verify", which must not trigger the
// minimize workaround.
var errSendToBackFailed = errors.New("window is still not visually at the back after every fallback stage")

// getRelatedWindowChecked wraps wincoe.GetWindow. A (0, nil) result means "no
// such related window" (e.g. no owner, or no next window); an error means the
// call really failed (e.g. invalid/destroyed handle).
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

// setWindowZOrder issues a position/size-less, non-activating SetWindowPos.
func setWindowZOrder(hwnd, insertAfter windows.Handle) error {
	if res := wincoe.SetWindowPos(hwnd, insertAfter, 0, 0, 0, 0, zOrderOnlyFlags); res.Failed() {
		return fmt.Errorf("SetWindowPos(HWND=0x%X, insertAfter=0x%X) failed: %w", hwnd, insertAfter, res.Err)
	}
	return nil
}

// pingWindowResponsive sends a harmless WM_NULL with SMTO_ABORTIFHUNG and
// reports an error if hwnd's thread doesn't answer within HungWindowTimeout.
// Used before any synchronous cross-process call that could otherwise block
// the calling thread on a hung window.
func pingWindowResponsive(hwnd windows.Handle) error {
	var result uintptr
	if res := wincoe.SendMessageTimeout(hwnd,
		wincoe.WM_NULL, // harmless ping
		0, 0,
		wincoe.SMTO_ABORTIFHUNG,
		HungWindowTimeout,
		&result,
	); res.Failed() {
		return fmt.Errorf("window HWND=0x%X didn't answer a WM_NULL ping within %dms (hung?): %w", hwnd, HungWindowTimeout, res.Err)
	}
	return nil
}

// isWindowMinimized reports whether hwnd has WS_MINIMIZE set.
func isWindowMinimized(hwnd windows.Handle) (bool, error) {
	style, _, styleErr := readStyleAndExStyle(hwnd)
	if styleErr != nil {
		return false, fmt.Errorf("isWindowMinimized: %w", styleErr)
	}
	return style&wsMinimize != 0, nil
}

// restoreIfMinimized un-minimizes hwnd if it's minimized (most likely by our
// own can't-send-to-back workaround, see minimizeUnsendableWindow), so a
// Win+Shift+MMB restore of such a window actually brings it back:
// SetWindowPos(HWND_TOP) and SetForegroundWindow don't un-minimize anything.
func restoreIfMinimized(hwnd windows.Handle) {
	minimized, minErr := isWindowMinimized(hwnd)
	if minErr != nil {
		logf("restoreIfMinimized: %v", minErr)
		return
	}
	if !minimized {
		return
	}
	if pingErr := pingWindowResponsive(hwnd); pingErr != nil {
		logf("restoreIfMinimized: not restoring: %v", pingErr)
		return
	}
	logf("restoreIfMinimized: HWND=0x%X is minimized (probably by our own can't-send-to-back workaround); restoring it", hwnd)
	_ = wincoe.ShowWindow(hwnd, wincoe.SW_RESTORE) // return value is only the prior visibility state
}

// minimizeUnsendableWindow is the last-resort workaround for a window that
// no z-order stage managed to push to the back: minimize it so it at least
// stops covering everything else. Must only run on the main thread.
func minimizeUnsendableWindow(target windows.Handle) {
	if pingErr := pingWindowResponsive(target); pingErr != nil {
		logf("minimizeUnsendableWindow: not minimizing HWND=0x%X: %v", target, pingErr)
		return
	}

	// A still-foreground window needs SW_MINIMIZE so the system hands
	// activation to the next window; otherwise don't touch activation.
	cmd := swShowMinNoActive
	if isWindowForeground(target) {
		cmd = swMinimize
	}
	_ = wincoe.ShowWindow(target, cmd) // return value is only the prior visibility state

	minimized, minErr := isWindowMinimized(target)
	if minErr != nil {
		logf("minimizeUnsendableWindow: couldn't verify whether HWND=0x%X got minimized: %v", target, minErr)
		return
	}
	if !minimized {
		logf("WARNING: minimizeUnsendableWindow: HWND=0x%X did not get minimized either (elevated window? UIPI) -- nothing more can be done for it", target)
		return
	}

	// The window lost foreground by being minimized, so the
	// focused-but-backgrounded marker (if it was set for it) is now stale.
	focusedSentToBackHwnd.CompareAndSwap(uintptr(target), 0)

	procName := getProcessNameFast(getWindowPID(target))
	logf("WARNING: WORKAROUND: couldn't send HWND=0x%X (%s) to the back by any method, so it was MINIMIZED instead (toggle this in the systray).", target, procName)
	showTrayInfo(selfName, fmt.Sprintf("Couldn't send %s to the back (it resists z-order changes), so it was minimized instead.", procName))
}

// verifySentToBackOrMinimize runs ensureSentToBack and, only when it
// definitively failed (errSendToBackFailed), applies the minimize workaround
// if the user left minimizeWhenCantSendToBack enabled. Any other error means
// the check itself couldn't be done and is just logged.
func verifySentToBackOrMinimize(target windows.Handle) {
	err := ensureSentToBack(target, "after the refocus logic")
	if err == nil {
		return
	}
	if !errors.Is(err, errSendToBackFailed) {
		logf("verifySentToBackOrMinimize: %v", err)
		return
	}
	logf("WARNING: HWND=0x%X could not be sent to the very back by any known method: %v | %s", target, err, describeWindow(target, false))
	if !minimizeWhenCantSendToBack.Load() {
		logf("verifySentToBackOrMinimize: the minimize workaround is disabled (systray toggle); leaving HWND=0x%X where it is", target)
		return
	}
	minimizeUnsendableWindow(target)
}

// isDesktopHostWindow reports whether hwnd is one of explorer's desktop host
// windows (Progman/WorkerW), which legitimately stay below everything else even
// after an HWND_BOTTOM. The class name alone is NOT enough: observed,
// TOTALCMD64.EXE creates a top-level window of class "WorkerW", which made a
// class-only check stop the "lowest real window" walk in the middle of the
// z-order. A failed class or process lookup is treated as "no".
func isDesktopHostWindow(hwnd windows.Handle) bool {
	class, res := wincoe.GetClassName(hwnd)
	if res.Failed() {
		return false
	}
	if class != "Progman" && class != "WorkerW" {
		return false
	}
	pid := getWindowPID(hwnd)
	if pid == 0 {
		return false
	}
	return strings.EqualFold(getProcessNameFast(pid), explorerExeName)
}

// isWindowCloaked reports whether DWM cloaks hwnd (e.g. UWP windows that are
// IsWindowVisible but not actually shown, like TextInputHost's CoreWindow).
func isWindowCloaked(hwnd windows.Handle) (bool, error) {
	var cloaked uint32
	res := procDwmGetWindowAttributeCloaked.Call(
		uintptr(hwnd),
		uintptr(dwmwaCloaked),
		uintptr(unsafe.Pointer(&cloaked)),
		unsafe.Sizeof(cloaked),
	)
	if res.Failed() {
		return false, fmt.Errorf("DwmGetWindowAttribute(DWMWA_CLOAKED) on HWND=0x%X failed: %w", hwnd, res.Err)
	}
	return cloaked != 0, nil
}

// isWindowReallyOnScreen is IsWindowVisible minus the windows that report as
// visible but a user can't actually see or interact with: minimized, DWM
// cloaked, or layered click-through overlays. Used so z-order verification
// doesn't count those as "a window that should be above the target".
//
// A window whose styles can't be read is treated as not on screen (it most
// likely vanished); a failed cloak query fails open (counts as on screen).
func isWindowReallyOnScreen(hwnd windows.Handle) bool {
	if !wincoe.IsWindowVisible(hwnd) {
		return false
	}
	style, exStyle, styleErr := readStyleAndExStyle(hwnd)
	if styleErr != nil {
		return false
	}
	if style&wsMinimize != 0 {
		return false
	}
	if exStyle&wincoe.WS_EX_LAYERED != 0 && exStyle&wincoe.WS_EX_TRANSPARENT != 0 {
		return false
	}
	cloaked, cloakErr := isWindowCloaked(hwnd)
	if cloakErr != nil {
		logf("isWindowReallyOnScreen: assuming HWND=0x%X is not cloaked: %v", hwnd, cloakErr)
		return true
	}
	return !cloaked
}

// zOrderIndexOf returns target's 0-based index (0 = topmost) among all
// top-level windows (hidden ones included) and how many were walked; index is
// -1 if target wasn't found.
func zOrderIndexOf(target windows.Handle) (index, total int, err error) {
	index = -1
	hwnd, res := wincoe.GetTopWindow(0)
	if res.Failed() {
		return index, 0, fmt.Errorf("zOrderIndexOf: GetTopWindow(0) failed: %w", res.Err)
	}
	for hwnd != 0 {
		if total >= maxZOrderWalkSteps {
			return index, total, fmt.Errorf("zOrderIndexOf: walk exceeded %d windows", maxZOrderWalkSteps)
		}
		if hwnd == target {
			index = total
		}
		total++
		next, nextErr := getRelatedWindowChecked(hwnd, wincoe.GW_HWNDNEXT)
		if nextErr != nil {
			return index, total, fmt.Errorf("zOrderIndexOf: %w", nextErr)
		}
		hwnd = next
	}
	return index, total, nil
}

func fmtZIndex(index, total int, err error) string {
	if err != nil {
		return fmt.Sprintf("<unknown: %v>", err)
	}
	if index < 0 {
		return fmt.Sprintf("<not found among %d>", total)
	}
	return fmt.Sprintf("%d/%d", index, total)
}

// visibleForeignWindowsBelow walks downward from hwnd and returns up to limit of
// the top-level windows below it that are really on screen and are not part of
// hwnd's own owner group, not one of ours, and not an explorer desktop host
// window. An empty result with a nil error means hwnd is visually at the back.
// On a walk error, whatever was found so far is returned together with the error.
func visibleForeignWindowsBelow(hwnd windows.Handle, limit int) ([]windows.Handle, error) {
	if limit < 1 {
		return nil, fmt.Errorf("visibleForeignWindowsBelow: limit must be >= 1, got %d", limit)
	}
	groupRoot, rootErr := rootOwnerOf(hwnd)
	if rootErr != nil {
		return nil, fmt.Errorf("visibleForeignWindowsBelow: %w", rootErr)
	}

	var found []windows.Handle
	cur := hwnd
	for steps := 0; steps < maxZOrderWalkSteps; steps++ {
		next, nextErr := getRelatedWindowChecked(cur, wincoe.GW_HWNDNEXT)
		if nextErr != nil {
			return found, fmt.Errorf("visibleForeignWindowsBelow: walking below HWND=0x%X: %w", hwnd, nextErr)
		}
		if next == 0 {
			return found, nil // reached the bottom of the z-order
		}
		cur = next

		// Cheapest check first: most windows in the list are hidden.
		if !wincoe.IsWindowVisible(next) {
			continue
		}
		if isOwnWindow(next) || isDesktopHostWindow(next) || isInZOrderGroup(next, groupRoot) || !isWindowReallyOnScreen(next) {
			continue
		}
		found = append(found, next)
		if len(found) >= limit {
			return found, nil
		}
	}
	return found, fmt.Errorf("visibleForeignWindowsBelow: walk below HWND=0x%X exceeded %d steps", hwnd, maxZOrderWalkSteps)
}

// logBlockingWindows logs the visible foreign windows still below target.
func logBlockingWindows(phase string, target windows.Handle, blockers []windows.Handle) {
	capped := ""
	if len(blockers) >= maxBlockersLogged {
		capped = " (at least; list capped)"
	}
	logf("ensureSentToBack(%s): %d visible foreign window(s) still below HWND=0x%X%s:", phase, len(blockers), target, capped)
	for _, b := range blockers {
		logf("ensureSentToBack(%s):   below: %s", phase, describeWindow(b, false))
	}
}

// sameProcessWindowsDirectlyBelow returns the run of consecutive windows
// directly below target in the z-order that belong to target's process
// (hidden ones included; e.g. TMOG's hidden ComboLBox popups).
func sameProcessWindowsDirectlyBelow(target windows.Handle) ([]windows.Handle, error) {
	pid := getWindowPID(target)
	if pid == 0 {
		return nil, fmt.Errorf("sameProcessWindowsDirectlyBelow: couldn't get the PID of HWND=0x%X", target)
	}

	var chain []windows.Handle
	cur := target
	for len(chain) < maxSameProcessChain {
		next, err := getRelatedWindowChecked(cur, wincoe.GW_HWNDNEXT)
		if err != nil {
			return chain, fmt.Errorf("sameProcessWindowsDirectlyBelow: %w", err)
		}
		if next == 0 || getWindowPID(next) != pid {
			return chain, nil
		}
		chain = append(chain, next)
		cur = next
	}
	return chain, fmt.Errorf("sameProcessWindowsDirectlyBelow: more than %d consecutive same-process windows below HWND=0x%X", maxSameProcessChain, target)
}

// ---- send-to-back stages ---------------------------------------------------

// stagePlainBottom is the ordinary SetWindowPos(HWND_BOTTOM). Worth repeating
// after the refocus: observed with Task Manager OG, the same call that does
// nothing while the window is still the foreground window works once it isn't.
func stagePlainBottom(target windows.Handle) error {
	return setWindowZOrder(target, wincoe.HWND_BOTTOM)
}

// stageInsertAfterLowestWindow inserts target directly after the lowest
// eligible top-level window (hidden ones included) instead of using
// HWND_BOTTOM. Eligible means: not in target's own owner group and not an
// explorer desktop host window.
//
// Desktop host windows are SKIPPED, not used as a stop marker: a hidden
// explorer WorkerW can sit in the MIDDLE of the z-order (observed with Task
// Manager OG, with ~100 real windows below it), so stopping the walk at the
// first one picked a window far above the real bottom and moved the target UP.
// Skipping them is still safe: inserting after a lower window never moves the
// target beneath a desktop host that was already below that window.
//
// Refuses (rather than moving the target up) if no eligible window sits
// below the target, and refuses to insert after a topmost window, which would
// make the target topmost.
func stageInsertAfterLowestWindow(target windows.Handle) error {
	groupRoot, rootErr := rootOwnerOf(target)
	if rootErr != nil {
		return fmt.Errorf("stageInsertAfterLowestWindow: %w", rootErr)
	}
	hwnd, res := wincoe.GetTopWindow(0)
	if res.Failed() {
		return fmt.Errorf("stageInsertAfterLowestWindow: GetTopWindow(0) failed: %w", res.Err)
	}

	var lowest windows.Handle
	seenTarget := false
	lowestIsBelowTarget := false
	for steps := 0; hwnd != 0; steps++ {
		if steps >= maxZOrderWalkSteps {
			return fmt.Errorf("stageInsertAfterLowestWindow: walk exceeded %d windows", maxZOrderWalkSteps)
		}
		switch {
		case hwnd == target:
			seenTarget = true
		case isInZOrderGroup(hwnd, groupRoot), isDesktopHostWindow(hwnd):
			// not eligible; keep walking
		default:
			lowest = hwnd
			lowestIsBelowTarget = seenTarget
		}
		next, nextErr := getRelatedWindowChecked(hwnd, wincoe.GW_HWNDNEXT)
		if nextErr != nil {
			return fmt.Errorf("stageInsertAfterLowestWindow: %w", nextErr)
		}
		hwnd = next
	}
	if lowest == 0 {
		return errors.New("stageInsertAfterLowestWindow: no suitable window found to insert after")
	}
	if !lowestIsBelowTarget {
		return fmt.Errorf("stageInsertAfterLowestWindow: HWND=0x%X is already below every eligible window (lowest is HWND=0x%X); inserting after it would move the target UP", target, lowest)
	}

	_, exStyle, styleErr := readStyleAndExStyle(lowest)
	if styleErr != nil {
		return fmt.Errorf("stageInsertAfterLowestWindow: can't verify that HWND=0x%X isn't topmost: %w", lowest, styleErr)
	}
	if exStyle&wincoe.WS_EX_TOPMOST != 0 {
		return fmt.Errorf("stageInsertAfterLowestWindow: the lowest window HWND=0x%X is topmost, inserting after it would make the target topmost", lowest)
	}
	return setWindowZOrder(target, lowest)
}

// insertAfterAboveGroup returns the hwndInsertAfter value that places a window
// directly above target's whole owner group: the first window above the group,
// or HWND_TOP if there is none or it is topmost (inserting a non-topmost
// window after a topmost one would make it topmost; in that case the group is
// already at the top of the non-topmost band, which is exactly what HWND_TOP
// means).
func insertAfterAboveGroup(target, groupRoot windows.Handle) (windows.Handle, error) {
	cur := target
	for steps := 0; steps < maxZOrderWalkSteps; steps++ {
		prev, prevErr := getRelatedWindowChecked(cur, wincoe.GW_HWNDPREV)
		if prevErr != nil {
			return 0, fmt.Errorf("insertAfterAboveGroup: %w", prevErr)
		}
		if prev == 0 {
			return wincoe.HWND_TOP, nil
		}
		if isInZOrderGroup(prev, groupRoot) {
			cur = prev
			continue
		}
		_, prevExStyle, styleErr := readStyleAndExStyle(prev)
		if styleErr != nil {
			return 0, fmt.Errorf("insertAfterAboveGroup: %w", styleErr)
		}
		if prevExStyle&wincoe.WS_EX_TOPMOST != 0 {
			return wincoe.HWND_TOP, nil
		}
		return prev, nil
	}
	return 0, fmt.Errorf("insertAfterAboveGroup: walk above HWND=0x%X exceeded %d steps", target, maxZOrderWalkSteps)
}

// stageRaiseBlockersAboveTarget achieves "target is visually at the back" from
// the other direction: instead of moving the target down (which a stubborn
// target may resist or misplace), it raises every visible foreign window below
// it to directly above the target's owner group, preserving their relative
// order. Doesn't depend on how the target handles its own
// WM_WINDOWPOSCHANGING. Each blocker is pinged first so a hung window can't
// block the main thread.
func stageRaiseBlockersAboveTarget(target windows.Handle) error {
	groupRoot, rootErr := rootOwnerOf(target)
	if rootErr != nil {
		return fmt.Errorf("stageRaiseBlockersAboveTarget: %w", rootErr)
	}
	_, targetExStyle, styleErr := readStyleAndExStyle(target)
	if styleErr != nil {
		return fmt.Errorf("stageRaiseBlockersAboveTarget: %w", styleErr)
	}
	if targetExStyle&wincoe.WS_EX_TOPMOST != 0 {
		return fmt.Errorf("stageRaiseBlockersAboveTarget: HWND=0x%X is topmost; non-topmost windows can't be raised above it", target)
	}
	insertAfter, insertErr := insertAfterAboveGroup(target, groupRoot)
	if insertErr != nil {
		return fmt.Errorf("stageRaiseBlockersAboveTarget: %w", insertErr)
	}

	blockers, walkErr := visibleForeignWindowsBelow(target, maxBlockersRaised)
	var errs []error
	if walkErr != nil {
		errs = append(errs, walkErr)
	} else if len(blockers) == 0 {
		return errors.New("stageRaiseBlockersAboveTarget: no visible foreign window below the target, nothing to raise")
	}

	// blockers is top-down; processing bottom-up with the SAME insertAfter
	// leaves them in their original relative order directly above the target.
	for i := len(blockers) - 1; i >= 0; i-- {
		blocker := blockers[i]
		if pingErr := pingWindowResponsive(blocker); pingErr != nil {
			errs = append(errs, pingErr)
			continue
		}
		if err := setWindowZOrder(blocker, insertAfter); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("stageRaiseBlockersAboveTarget(HWND=0x%X): %w", target, errors.Join(errs...))
}

// stageJumpPastSameProcessChain inserts target directly after the last of the
// same-process windows sitting directly below it, instead of relying on
// HWND_BOTTOM to get past them.
func stageJumpPastSameProcessChain(target windows.Handle) error {
	chain, chainErr := sameProcessWindowsDirectlyBelow(target)
	if chainErr != nil {
		return fmt.Errorf("stageJumpPastSameProcessChain: %w", chainErr)
	}
	if len(chain) == 0 {
		return errors.New("stageJumpPastSameProcessChain: no same-process window directly below the target, nothing to jump past")
	}
	return setWindowZOrder(target, chain[len(chain)-1])
}

// stageBurySameProcessChain sends the same-process windows directly below
// target to the bottom first (they're what target stops above), then sends
// target to the bottom. The windows being moved are the target process's own
// hidden helpers, on the same UI thread the target's own SetWindowPos already
// had to talk to.
func stageBurySameProcessChain(target windows.Handle) error {
	chain, chainErr := sameProcessWindowsDirectlyBelow(target)
	if chainErr != nil {
		return fmt.Errorf("stageBurySameProcessChain: %w", chainErr)
	}
	if len(chain) == 0 {
		return errors.New("stageBurySameProcessChain: no same-process window directly below the target, nothing to bury")
	}

	var errs []error
	for _, w := range chain {
		if err := setWindowZOrder(w, wincoe.HWND_BOTTOM); err != nil {
			errs = append(errs, err)
		}
	}
	if err := setWindowZOrder(target, wincoe.HWND_BOTTOM); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("stageBurySameProcessChain(HWND=0x%X): %w", target, errors.Join(errs...))
}

// sendToBackStages are tried in order by ensureSentToBack until one leaves
// the target visually at the back.
var sendToBackStages = [...]struct {
	name string
	run  func(target windows.Handle) error
}{
	{"plain HWND_BOTTOM", stagePlainBottom},
	{"insert after the lowest real window", stageInsertAfterLowestWindow},
	{"insert after the last same-process window directly below", stageJumpPastSameProcessChain},
	{"bury same-process windows directly below, then HWND_BOTTOM", stageBurySameProcessChain},
	{"raise every visible foreign window below it above it instead", stageRaiseBlockersAboveTarget},
}

// ensureSentToBack verifies that target (already the subject of an
// HWND_BOTTOM SetWindowPos, normally followed by a refocus) is really visually
// at the back, and if not walks sendToBackStages, verifying after each one and
// logging the target's z-index before/after every stage plus which visible
// foreign windows are still below it.
//
// SetWindowPos reporting success proves nothing here: observed with Task
// Manager OG, where HWND_BOTTOM succeeds but leaves the window about 100
// windows above the real bottom, and does nothing at all while the window is
// still the foreground window.
//
// Owned windows (e.g. modal dialogs) are deliberately left on their legacy
// behavior: this is only for windows that are their own root owner.
//
// Returns nil if target is at the back (or this doesn't apply). If it was
// verified to still not be at the back after every stage, the returned error
// wraps errSendToBackFailed; any other error means verification itself
// couldn't be done.
func ensureSentToBack(target windows.Handle, phase string) error {
	root, rootErr := rootOwnerOf(target)
	if rootErr != nil {
		return fmt.Errorf("ensureSentToBack(%s): can't verify HWND=0x%X, not attempting fallbacks: %w", phase, target, rootErr)
	}
	if root != target {
		return nil // owned window: legacy behavior, doesn't apply
	}

	blockers, verifyErr := visibleForeignWindowsBelow(target, maxBlockersLogged)
	if verifyErr != nil {
		return fmt.Errorf("ensureSentToBack(%s): couldn't verify z-order of HWND=0x%X, not attempting fallbacks: %w", phase, target, verifyErr)
	}
	if len(blockers) == 0 {
		return nil
	}
	logf("ensureSentToBack(%s): HWND=0x%X is NOT at the back; trying fallbacks", phase, target)
	logBlockingWindows(phase, target, blockers)

	for _, stage := range sendToBackStages {
		idxBefore, totalBefore, idxBeforeErr := zOrderIndexOf(target)
		runErr := stage.run(target)
		idxAfter, totalAfter, idxAfterErr := zOrderIndexOf(target)
		logf("ensureSentToBack(%s): stage %q: z-index %s -> %s, stage error: %v", phase, stage.name,
			fmtZIndex(idxBefore, totalBefore, idxBeforeErr), fmtZIndex(idxAfter, totalAfter, idxAfterErr), runErr)

		blockers, verifyErr = visibleForeignWindowsBelow(target, maxBlockersLogged)
		if verifyErr != nil {
			return fmt.Errorf("ensureSentToBack(%s): couldn't verify HWND=0x%X after stage %q: %w", phase, target, stage.name, verifyErr)
		}
		if len(blockers) == 0 {
			logf("ensureSentToBack(%s): stage %q was needed and worked for HWND=0x%X", phase, stage.name, target)
			return nil
		}
		logf("ensureSentToBack(%s): after stage %q HWND=0x%X is still not at the back", phase, stage.name, target)
		logBlockingWindows(phase, target, blockers)
	}
	return fmt.Errorf("ensureSentToBack(%s): every stage failed for HWND=0x%X: %w", phase, target, errSendToBackFailed)
}
