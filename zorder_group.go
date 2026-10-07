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

	"golang.org/x/sys/windows"

	"github.com/workturnedplay/wincoe"
)

const (
	// swpNoSendChanging is SWP_NOSENDCHANGING: the window doesn't receive
	// WM_WINDOWPOSCHANGING, so an app that vetoes/rewrites z-order changes
	// there (e.g. by adding SWP_NOZORDER while it's the active window) can't.
	// Not defined in wincoe.
	swpNoSendChanging uint32 = 0x0400

	// zOrderOnlyFlags: change nothing but the z-order, don't activate.
	zOrderOnlyFlags uint32 = wincoe.SWP_NOMOVE | wincoe.SWP_NOSIZE | wincoe.SWP_NOACTIVATE

	// maxZOrderWalkSteps bounds every top-level z-order walk in this file
	// (hidden windows are included in such walks; a few hundred is normal).
	maxZOrderWalkSteps = 1000
)

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

// setWindowZOrder issues a position/size-less, non-activating SetWindowPos.
func setWindowZOrder(hwnd, insertAfter windows.Handle, extraFlags uint32) error {
	if res := wincoe.SetWindowPos(hwnd, insertAfter, 0, 0, 0, 0, zOrderOnlyFlags|extraFlags); res.Failed() {
		return fmt.Errorf("SetWindowPos(HWND=0x%X, insertAfter=0x%X, extraFlags=0x%X) failed: %w", hwnd, insertAfter, extraFlags, res.Err)
	}
	return nil
}

// isDesktopHostWindow reports whether hwnd is one of the shell's desktop host
// windows, which legitimately stay below everything else even after an
// HWND_BOTTOM. A failed class lookup is treated as "no".
func isDesktopHostWindow(hwnd windows.Handle) bool {
	class, res := wincoe.GetClassName(hwnd)
	if res.Failed() {
		return false
	}
	return class == "Progman" || class == "WorkerW"
}

// firstVisibleForeignWindowBelow walks downward from hwnd and returns the
// first VISIBLE top-level window below it that is not part of hwnd's own
// owner group, not one of ours, and not a desktop host window. Returns
// (0, nil) if there is none, i.e. hwnd is visually at the back.
func firstVisibleForeignWindowBelow(hwnd windows.Handle) (windows.Handle, error) {
	groupRoot, rootErr := rootOwnerOf(hwnd)
	if rootErr != nil {
		return 0, fmt.Errorf("firstVisibleForeignWindowBelow: %w", rootErr)
	}

	cur := hwnd
	for steps := 0; steps < maxZOrderWalkSteps; steps++ {
		next, nextErr := getRelatedWindowChecked(cur, wincoe.GW_HWNDNEXT)
		if nextErr != nil {
			return 0, fmt.Errorf("firstVisibleForeignWindowBelow: walking below HWND=0x%X: %w", hwnd, nextErr)
		}
		if next == 0 {
			//nolint:nilnil // return both a `nil` error and an invalid value: use a sentinel error instead (nilnil)
			return 0, nil // reached the bottom of the z-order
		}
		cur = next

		// Cheapest checks first.
		if !wincoe.IsWindowVisible(next) || isOwnWindow(next) || isInZOrderGroup(next, groupRoot) || isDesktopHostWindow(next) {
			continue
		}
		return next, nil
	}
	return 0, fmt.Errorf("firstVisibleForeignWindowBelow: walk below HWND=0x%X exceeded %d steps", hwnd, maxZOrderWalkSteps)
}

// collectZOrderGroupMembers returns every top-level window (hidden ones
// included) whose GA_ROOTOWNER is root, excluding root itself.
func collectZOrderGroupMembers(root windows.Handle) ([]windows.Handle, error) {
	hwnd, topRes := wincoe.GetTopWindow(0)
	if topRes.Failed() {
		return nil, fmt.Errorf("collectZOrderGroupMembers: GetTopWindow(0) failed: %w", topRes.Err)
	}

	var members []windows.Handle
	for steps := 0; hwnd != 0; steps++ {
		if steps >= maxZOrderWalkSteps {
			return members, fmt.Errorf("collectZOrderGroupMembers: walk exceeded %d steps", maxZOrderWalkSteps)
		}
		if hwnd != root && isInZOrderGroup(hwnd, root) {
			members = append(members, hwnd)
		}
		next, nextErr := getRelatedWindowChecked(hwnd, wincoe.GW_HWNDNEXT)
		if nextErr != nil {
			return members, fmt.Errorf("collectZOrderGroupMembers: %w", nextErr)
		}
		hwnd = next
	}
	return members, nil
}

// sendOwnerGroupToBottom sends root (which must be its own GA_ROOTOWNER) to
// the bottom without letting it see WM_WINDOWPOSCHANGING, then does the same
// for every window it owns so they land right above it, at the bottom, instead
// of staying up on top of everything (e.g. TMOG's visible CaptionOverlay).
// Individual failures are collected and returned joined, but never stop the
// remaining windows from being processed.
func sendOwnerGroupToBottom(root windows.Handle) error {
	members, collectErr := collectZOrderGroupMembers(root)

	var errs []error
	if collectErr != nil {
		errs = append(errs, collectErr) // still process whatever was collected
	}
	if err := setWindowZOrder(root, wincoe.HWND_BOTTOM, swpNoSendChanging); err != nil {
		errs = append(errs, err)
	}
	for _, m := range members {
		if err := setWindowZOrder(m, wincoe.HWND_BOTTOM, swpNoSendChanging); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("sendOwnerGroupToBottom(HWND=0x%X): %w", root, errors.Join(errs...))
}

// ensureSentToBack verifies that target (already the subject of an
// HWND_BOTTOM SetWindowPos) is really visually at the back, and if not, walks
// a ladder of increasingly forceful fallbacks, verifying after each one:
//
//  1. plain HWND_BOTTOM (skipped if skipPlainStage -- the caller just did it),
//  2. HWND_BOTTOM with SWP_NOSENDCHANGING,
//  3. the whole owner group to the bottom.
//
// SetWindowPos reporting success proves nothing here: observed with Task
// Manager OG, where it succeeds but the z-index doesn't change at all.
//
// Owned windows (e.g. modal dialogs) are deliberately left on their legacy
// behavior: the ladder is only for windows that are their own root owner.
// Returns true if target is at the back (or the ladder doesn't apply).
func ensureSentToBack(target windows.Handle, skipPlainStage bool, phase string) bool {
	root, rootErr := rootOwnerOf(target)
	if rootErr != nil {
		logf("ensureSentToBack(%s): can't verify HWND=0x%X, not attempting fallbacks: %v", phase, target, rootErr)
		return false
	}
	if root != target {
		return true // owned window: legacy behavior, ladder doesn't apply
	}

	blocker, verifyErr := firstVisibleForeignWindowBelow(target)
	if verifyErr != nil {
		logf("ensureSentToBack(%s): couldn't verify z-order of HWND=0x%X, not attempting fallbacks: %v", phase, target, verifyErr)
		return false
	}
	if blocker == 0 {
		return true
	}
	logf("ensureSentToBack(%s): HWND=0x%X is NOT at the back (visible foreign window still below it: %s); trying fallbacks", phase, target, describeWindow(blocker, false))

	stages := [...]struct {
		name string
		run  func() error
	}{
		{"plain HWND_BOTTOM", func() error { return setWindowZOrder(target, wincoe.HWND_BOTTOM, 0) }},
		{"HWND_BOTTOM + SWP_NOSENDCHANGING", func() error { return setWindowZOrder(target, wincoe.HWND_BOTTOM, swpNoSendChanging) }},
		{"owner group to bottom (+SWP_NOSENDCHANGING)", func() error { return sendOwnerGroupToBottom(target) }},
	}
	for i, stage := range stages {
		if skipPlainStage && i == 0 {
			continue
		}
		if runErr := stage.run(); runErr != nil {
			logf("ensureSentToBack(%s): stage %q reported an error for HWND=0x%X: %v", phase, stage.name, target, runErr)
			continue
		}
		blocker, verifyErr = firstVisibleForeignWindowBelow(target)
		if verifyErr != nil {
			logf("ensureSentToBack(%s): couldn't verify HWND=0x%X after stage %q: %v", phase, target, stage.name, verifyErr)
			return false
		}
		if blocker == 0 {
			logf("ensureSentToBack(%s): stage %q was needed and worked for HWND=0x%X", phase, stage.name, target)
			return true
		}
		logf("ensureSentToBack(%s): stage %q ran but HWND=0x%X is still not at the back (still below it: %s)", phase, stage.name, target, describeWindow(blocker, false))
	}
	logf("ensureSentToBack(%s): every fallback stage failed for HWND=0x%X", phase, target)
	return false
}
