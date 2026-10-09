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
	"sync"

	"golang.org/x/sys/windows"

	"github.com/workturnedplay/wincoe"
)

const (
	// maxZOrderWalkSteps bounds every top-level z-order walk in this file
	// (hidden windows are included in such walks; a few hundred is normal).
	maxZOrderWalkSteps = 1000

	// maxBlockersLogged caps how many "visible foreign window still below the
	// target" entries ensureSentToBack lists (and fetches) per verification.
	maxBlockersLogged = 5

	// maxBlockersRaised caps how many windows stageRaiseBlockersAboveTarget
	// restacks in one go.
	maxBlockersRaised = 64

	// maxBuriedHelperWindows caps how many hidden same-process windows
	// buryHiddenSameProcessWindows sends to the bottom in one go.
	maxBuriedHelperWindows = 64

	// describeTitleMaxRunes keeps one-line window descriptions readable.
	describeTitleMaxRunes = 48

	// explorerExeName is the shell process whose desktop host windows
	// (Progman/WorkerW) legitimately sit below every normal window.
	explorerExeName = "explorer.exe"
)

// errSendToBackFailed is wrapped by ensureSentToBack's returned error when the
// window was verified to still NOT be visually at the back after every stage.
// Any other error from it means "couldn't verify", which must not trigger the
// minimize workaround.
var errSendToBackFailed = errors.New("window is still not visually at the back after every fallback stage")

// stubbornSendToBackExes remembers (lowercased exe base names) processes for
// which a plain HWND_BOTTOM was NOT enough but burying their hidden helper
// windows first was (observed with Task Manager OG, whose z-position seems to
// be tied to its hidden ComboLBox windows: moving the main window alone gets
// undone, producing visible flicker and ~1s delays while ensureSentToBack
// walks its stages). For such processes preBuryIfKnownStubborn buries the
// helpers BEFORE the first HWND_BOTTOM. Not persisted: it's relearned once per
// run.
var (
	stubbornSendToBackMu   sync.Mutex
	stubbornSendToBackExes = make(map[string]struct{})
)

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// describeWindow renders a one-line description of hwnd for logs.
func describeWindow(hwnd windows.Handle) string {
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
	title := truncateRunes(getWindowTextFast(hwnd), describeTitleMaxRunes)

	pid := getWindowPID(hwnd)
	exe := "<unknown>"
	if pid != 0 {
		exe = getProcessNameFast(pid)
	}
	return fmt.Sprintf("HWND=0x%X class=%q title=%q exe=%s pid=%d", hwnd, class, title, exe, pid)
}

// exeKeyOfWindow returns the lowercased exe base name of hwnd's process, the
// key used by stubbornSendToBackExes.
func exeKeyOfWindow(hwnd windows.Handle) (string, error) {
	pid := getWindowPID(hwnd)
	if pid == 0 {
		return "", fmt.Errorf("exeKeyOfWindow: couldn't get the PID of HWND=0x%X", hwnd)
	}
	name := getProcessNameFast(pid)
	if strings.HasPrefix(name, "<") { // getProcessNameFast's "<failed>"/"<not found>"
		return "", fmt.Errorf("exeKeyOfWindow: couldn't resolve the exe name of PID %d (HWND=0x%X): %s", pid, hwnd, name)
	}
	return strings.ToLower(name), nil
}

func markStubbornSendToBack(target windows.Handle) {
	key, err := exeKeyOfWindow(target)
	if err != nil {
		logf("markStubbornSendToBack: not remembering: %v", err)
		return
	}
	stubbornSendToBackMu.Lock()
	_, already := stubbornSendToBackExes[key]
	stubbornSendToBackExes[key] = struct{}{}
	stubbornSendToBackMu.Unlock()
	if !already {
		logf("markStubbornSendToBack: %q resists a plain HWND_BOTTOM; from now on its hidden helper windows get buried BEFORE the first HWND_BOTTOM", key)
	}
}

func isStubbornSendToBack(target windows.Handle) bool {
	key, err := exeKeyOfWindow(target)
	if err != nil {
		return false // can't tell; behave like an ordinary window
	}
	stubbornSendToBackMu.Lock()
	defer stubbornSendToBackMu.Unlock()
	_, ok := stubbornSendToBackExes[key]
	return ok
}

// isDesktopHostWindow reports whether hwnd is one of explorer's desktop host
// windows (Progman/WorkerW), which legitimately stay below everything else even
// after an HWND_BOTTOM. The class name alone is NOT enough: observed,
// TOTALCMD64.EXE creates a top-level window of class "WorkerW". A failed class
// or process lookup is treated as "no".
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

// isWindowReallyOnScreen is IsWindowVisible minus the windows that report as
// visible but a user can't actually see or interact with: minimized, DWM
// cloaked, or layered click-through overlays. Used so z-order verification and
// refocus selection don't count those as real windows.
//
// A window whose styles can't be read is treated as not on screen (it most
// likely vanished); a failed cloak query fails open (counts as on screen).
func isWindowReallyOnScreen(hwnd windows.Handle) bool {
	if !wincoe.IsWindowVisible(hwnd) || wincoe.IsIconic(hwnd) {
		return false
	}
	_, exStyle, styleErr := wincoe.GetWindowStyleAndExStyle(hwnd)
	if styleErr != nil {
		return false
	}
	if exStyle&wincoe.WS_EX_LAYERED != 0 && exStyle&wincoe.WS_EX_TRANSPARENT != 0 {
		return false
	}
	cloaked, cloakErr := wincoe.DwmIsWindowCloaked(hwnd)
	if cloakErr != nil {
		logf("isWindowReallyOnScreen: assuming HWND=0x%X is not cloaked: %v", hwnd, cloakErr)
		return true
	}
	return !cloaked
}

// restoreIfMinimized un-minimizes hwnd if it's minimized (most likely by our
// own can't-send-to-back workaround, see minimizeUnsendableWindow), so a
// Win+Shift+MMB restore of such a window actually brings it back:
// SetWindowPos(HWND_TOP) and SetForegroundWindow don't un-minimize anything.
func restoreIfMinimized(hwnd windows.Handle) {
	if !wincoe.IsIconic(hwnd) {
		return
	}
	if pingErr := wincoe.PingWindow(hwnd, HungWindowTimeout); pingErr != nil {
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
	if pingErr := wincoe.PingWindow(target, HungWindowTimeout); pingErr != nil {
		logf("minimizeUnsendableWindow: not minimizing HWND=0x%X: %v", target, pingErr)
		return
	}

	// A still-foreground window needs SW_MINIMIZE so the system hands
	// activation to the next window; otherwise don't touch activation.
	var cmd int32 = wincoe.SW_SHOWMINNOACTIVE
	if isWindowForeground(target) {
		cmd = wincoe.SW_MINIMIZE
	}
	_ = wincoe.ShowWindow(target, cmd) // return value is only the prior visibility state

	if !wincoe.IsIconic(target) {
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
	logf("WARNING: HWND=0x%X could not be sent to the very back by any known method: %v | %s", target, err, describeWindow(target))
	if !minimizeWhenCantSendToBack.Load() {
		logf("verifySentToBackOrMinimize: the minimize workaround is disabled (systray toggle); leaving HWND=0x%X where it is", target)
		return
	}
	minimizeUnsendableWindow(target)
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
		next, nextErr := wincoe.GetRelatedWindow(hwnd, wincoe.GW_HWNDNEXT)
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
	groupRoot, rootErr := wincoe.GetRootOwner(hwnd)
	if rootErr != nil {
		return nil, fmt.Errorf("visibleForeignWindowsBelow: %w", rootErr)
	}

	var found []windows.Handle
	cur := hwnd
	for steps := 0; steps < maxZOrderWalkSteps; steps++ {
		next, nextErr := wincoe.GetRelatedWindow(cur, wincoe.GW_HWNDNEXT)
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
		logf("ensureSentToBack(%s):   below: %s", phase, describeWindow(b))
	}
}

// ---- hidden same-process helper windows ------------------------------------

// hiddenSameProcessWindows returns, top-down, up to limit top-level windows
// that belong to target's process, are NOT visible, are not in target's own
// owner group (those follow their owner anyway) and are not topmost (sending a
// topmost window to HWND_BOTTOM would silently strip its topmost status).
// Refuses for explorer.exe, whose hidden windows include the desktop hosts
// that must stay at the very bottom.
//
// Observed with Task Manager OG: these are its hidden ComboLBox popups, and
// its main window can't be sent to the back until they've been moved first.
// On a walk error, whatever was found so far is returned with the error.
func hiddenSameProcessWindows(target windows.Handle, limit int) ([]windows.Handle, error) {
	if limit < 1 {
		return nil, fmt.Errorf("hiddenSameProcessWindows: limit must be >= 1, got %d", limit)
	}
	pid := getWindowPID(target)
	if pid == 0 {
		return nil, fmt.Errorf("hiddenSameProcessWindows: couldn't get the PID of HWND=0x%X", target)
	}
	if strings.EqualFold(getProcessNameFast(pid), explorerExeName) {
		return nil, fmt.Errorf("hiddenSameProcessWindows: refusing for %s (its hidden windows include the desktop hosts)", explorerExeName)
	}
	groupRoot, rootErr := wincoe.GetRootOwner(target)
	if rootErr != nil {
		return nil, fmt.Errorf("hiddenSameProcessWindows: %w", rootErr)
	}
	hwnd, res := wincoe.GetTopWindow(0)
	if res.Failed() {
		return nil, fmt.Errorf("hiddenSameProcessWindows: GetTopWindow(0) failed: %w", res.Err)
	}

	var found []windows.Handle
	for steps := 0; hwnd != 0; steps++ {
		if steps >= maxZOrderWalkSteps {
			return found, fmt.Errorf("hiddenSameProcessWindows: walk exceeded %d windows", maxZOrderWalkSteps)
		}
		// Cheapest checks first.
		if hwnd != target && !wincoe.IsWindowVisible(hwnd) && getWindowPID(hwnd) == pid && !isInZOrderGroup(hwnd, groupRoot) {
			_, exStyle, styleErr := wincoe.GetWindowStyleAndExStyle(hwnd)
			if styleErr == nil && exStyle&wincoe.WS_EX_TOPMOST == 0 {
				found = append(found, hwnd)
				if len(found) >= limit {
					return found, nil
				}
			}
		}
		next, nextErr := wincoe.GetRelatedWindow(hwnd, wincoe.GW_HWNDNEXT)
		if nextErr != nil {
			return found, fmt.Errorf("hiddenSameProcessWindows: walk cut short: %w", nextErr)
		}
		hwnd = next
	}
	return found, nil
}

// sendWindowsToBottom sends each window to HWND_BOTTOM, in order, so they end
// up at the bottom in their original relative order. Each distinct UI thread
// is pinged once first so a hung one can't block the (main) thread inside a
// synchronous cross-process SetWindowPos. Best effort: returns every error
// encountered.
func sendWindowsToBottom(hwnds []windows.Handle) []error {
	var errs []error
	threadHealth := make(map[uint32]error) // tid -> nil if responsive, else the ping error
	for _, h := range hwnds {
		var pid uint32
		tid, res := wincoe.GetWindowThreadProcessId(h, &pid)
		if res.Failed() {
			errs = append(errs, fmt.Errorf("sendWindowsToBottom: GetWindowThreadProcessId(HWND=0x%X) failed: %w", h, res.Err))
			continue
		}
		health, seen := threadHealth[tid]
		if !seen {
			health = wincoe.PingWindow(h, HungWindowTimeout)
			threadHealth[tid] = health
		}
		if health != nil {
			errs = append(errs, health)
			continue
		}
		if err := wincoe.SetWindowZOrder(h, wincoe.HWND_BOTTOM); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// buryHiddenSameProcessWindows sends target's process's hidden helper windows
// (see hiddenSameProcessWindows) to the bottom. Returns how many were found
// (and attempted); a nil error with 0 means there was nothing to bury.
func buryHiddenSameProcessWindows(target windows.Handle) (int, error) {
	helpers, listErr := hiddenSameProcessWindows(target, maxBuriedHelperWindows)
	errs := sendWindowsToBottom(helpers)
	if listErr != nil {
		errs = append(errs, listErr)
	}
	if len(errs) == 0 {
		return len(helpers), nil
	}
	return len(helpers), fmt.Errorf("buryHiddenSameProcessWindows(HWND=0x%X): %w", target, errors.Join(errs...))
}

// preBuryIfKnownStubborn buries target's hidden helper windows ahead of the
// first HWND_BOTTOM if its exe was previously found to need that (see
// stubbornSendToBackExes), so such windows go to the back in one clean step
// instead of flickering through ensureSentToBack's fallback stages. Main
// thread only. Owned windows (modal dialogs etc.) keep their legacy behavior.
func preBuryIfKnownStubborn(target windows.Handle) {
	if !isStubbornSendToBack(target) {
		return
	}
	root, rootErr := wincoe.GetRootOwner(target)
	if rootErr != nil {
		logf("preBuryIfKnownStubborn: not burying for HWND=0x%X: %v", target, rootErr)
		return
	}
	if root != target {
		return
	}
	buried, err := buryHiddenSameProcessWindows(target)
	if err != nil {
		logf("preBuryIfKnownStubborn: %v", err)
	}
	logf("preBuryIfKnownStubborn: buried %d hidden helper window(s) of HWND=0x%X ahead of its HWND_BOTTOM", buried, target)
}

// ---- send-to-back stages ---------------------------------------------------

// stagePlainBottom is the ordinary SetWindowPos(HWND_BOTTOM). Worth repeating
// after the refocus: observed with Task Manager OG, the same call that does
// nothing while the window is still the foreground window works once it isn't.
func stagePlainBottom(target windows.Handle) error {
	return wincoe.SetWindowZOrder(target, wincoe.HWND_BOTTOM)
}

// stageBuryHelpersThenBottom sends target's process's hidden helper windows to
// the bottom first (see hiddenSameProcessWindows), then sends target itself to
// the bottom. Observed with Task Manager OG: this is the only stage that ever
// worked for it, apparently because it undoes a plain HWND_BOTTOM of its main
// window unless its hidden ComboLBox windows were moved first.
func stageBuryHelpersThenBottom(target windows.Handle) error {
	buried, buryErr := buryHiddenSameProcessWindows(target)
	if buryErr == nil && buried == 0 {
		return errors.New("stageBuryHelpersThenBottom: no hidden same-process window to bury")
	}
	var errs []error
	if buryErr != nil {
		errs = append(errs, buryErr)
	}
	if err := wincoe.SetWindowZOrder(target, wincoe.HWND_BOTTOM); err != nil {
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("stageBuryHelpersThenBottom(HWND=0x%X): %w", target, errors.Join(errs...))
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
		prev, prevErr := wincoe.GetRelatedWindow(cur, wincoe.GW_HWNDPREV)
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
		_, prevExStyle, styleErr := wincoe.GetWindowStyleAndExStyle(prev)
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
// WM_WINDOWPOSCHANGING. Each blocker's thread is pinged first so a hung window
// can't block the main thread.
func stageRaiseBlockersAboveTarget(target windows.Handle) error {
	groupRoot, rootErr := wincoe.GetRootOwner(target)
	if rootErr != nil {
		return fmt.Errorf("stageRaiseBlockersAboveTarget: %w", rootErr)
	}
	_, targetExStyle, styleErr := wincoe.GetWindowStyleAndExStyle(target)
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
		if pingErr := wincoe.PingWindow(blocker, HungWindowTimeout); pingErr != nil {
			errs = append(errs, pingErr)
			continue
		}
		if err := wincoe.SetWindowZOrder(blocker, insertAfter); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("stageRaiseBlockersAboveTarget(HWND=0x%X): %w", target, errors.Join(errs...))
}

// sendToBackStages are tried in order by ensureSentToBack until one leaves
// the target visually at the back. marksStubborn: if this stage was the one
// that finally worked, remember the target's exe (see stubbornSendToBackExes).
var sendToBackStages = [...]struct {
	name          string
	run           func(target windows.Handle) error
	marksStubborn bool
}{
	{"plain HWND_BOTTOM", stagePlainBottom, false},
	{"bury hidden same-process windows, then HWND_BOTTOM", stageBuryHelpersThenBottom, true},
	{"raise every visible foreign window below it above it instead", stageRaiseBlockersAboveTarget, false},
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
	root, rootErr := wincoe.GetRootOwner(target)
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
			if stage.marksStubborn {
				markStubbornSendToBack(target)
			}
			return nil
		}
		logf("ensureSentToBack(%s): after stage %q HWND=0x%X is still not at the back", phase, stage.name, target)
		logBlockingWindows(phase, target, blockers)
	}
	return fmt.Errorf("ensureSentToBack(%s): every stage failed for HWND=0x%X: %w", phase, target, errSendToBackFailed)
}
