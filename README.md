## winbollocks

Originally located here: [https://github.com/workturnedplay/winbollocks](https://github.com/workturnedplay/winbollocks)
(unless you got it from a fork, try `git remote -v` to check)

`winbollocks` is a small Windows utility that I used to use on win11 (before I switched to Linux); it adds a couple of **Win-key mouse gestures for window movement, resizing, and z-order control**, independent of where the mouse cursor is inside the window.

It is designed to replicate common X11 / Wayland window-manager behaviors on Windows such as: winkey+LMB drag to move a window, winkey+RMB drag to resize, and winkey+MMB to send a window to back behind all others.

It was built with the help of AI (chatgpt 5.2 and 5.1Mini, Grok 4 and 4.1, Gemini 3 and 3.1 Pro), non-agentic via website/chat only.
It works right now as stated below, but the code is a mess (for now) on purpose.
It's not actively developed anymore (I'm switching to Linux which already has these).

---

### What it does

`winbollocks` installs **low-level mouse and keyboard hooks** and reacts only to a small, fixed set of input combinations. It also places an icon in the System Tray for quick configuration.

The following behaviors are implemented:

**1. Win + Left Mouse Button (drag anywhere to move)**

* Pressing and holding **LMB** over any point inside a window starts a manual move of that window.
* The window follows the mouse until LMB is released.
* Pressing **ESC** mid-drag cancels the gesture and snaps the window back to its original position.
* The click does **not** need to be on the title bar and is **not** passed through to the target window.

**2. Win + Right Mouse Button (drag anywhere to resize)**

* Pressing and holding **RMB** over a window initiates a resize operation.
* The screen is divided into a 9-zone grid. Dragging from the edges or corners resizes the window in that specific direction.
* Dragging from the **center zone** resizes the window uniformly while respecting its initial aspect ratio.
* **Shift-Mirroring:** Holding **Shift** while resizing warps the cursor to the opposite side, allowing you to push/pull both opposite edges/corners in a single continuous gesture.
* Pressing **ESC** mid-resize cancels the gesture and restores the original size.
* A helpful green-on-black overlay appears on screen, displaying the live dimensions and pixel delta.

**3. Win + Middle Mouse Button (send to back)**

* Pressing **MMB** over a window sends that window to the bottom of the z-order.
* The window is not activated, but you can configure it to automatically shift keyboard focus to whichever window is now on top.

**4. Win + Shift + Middle Mouse Button (restore sent-to-back window)**

* Pressing **Win + Shift + MMB** restores a window that was previously sent to the back.
* This operates on a LIFO (Last-In, First-Out) stack, meaning repeated uses will bring back multiple previously backgrounded windows in the reverse order you sent them away.

**5. Start menu suppression for these gestures**

* Releasing the Windows key after a handled gesture does **not** open the Start menu.
* This is achieved by injecting a quick Right-Ctrl (`VK_RCONTROL`) tap to disarm the shell.

**6. Missed Gesture Recovery**

* Automatically detects if a Win-key gesture was "missed" because a higher-integrity (elevated) window temporarily blinded the hooks.
* It recovers the drag or resize action on the next mouse move once focus returns to a normal window.

---

### System Tray Configuration

Right-clicking the `winbollocks` icon in the system tray provides a massive list of live toggles to customize the app's behavior:

| Feature | Description |
| --- | --- |
| **Activate on Move/Resize** | Brings the target window into focus when a drag or resize gesture starts. |
| **Bring to Front** | Promotes the window to the top of the Z-order automatically on drag or resize. |
| **Fallback LMB Focus** | Injects a physical Left Mouse Click to force focus if standard activation fails. |
| **Unfocus Sent to Back** | Automatically shifts focus to the new top window when you send one to the back. |
| **Coalesce Events** | Ignores historical queue data to keep drags highly responsive, overriding the standard 60fps rate limit. |
| **Bypass Fullscreen** | Ignores gestures entirely if the foreground app is running in exclusive or borderless fullscreen (great for gaming). |
| **Require WinKey Held** | Instantly stops any active move or resize gesture if the Windows key is released mid-action. |
| **Immediate Overlay Repaint** | Forces the resize overlay to repaint synchronously, preventing freezes during rapid resizing. |
| **Missed Gesture Recovery** | Arms the recovery system to catch gestures lost to Admin windows. |
| **Diagnostic Input State** | A read-only menu item showing exactly what modifier keys the app currently thinks are held down. |

---

### Known Limitations

**Interaction with elevated (Administrator) windows**

When `winbollocks` is run without administrative privileges, it cannot reliably interact with windows that are running elevated (as Administrator). This is a Windows security boundary (UIPI).

* If a Win-key gesture is initiated over an elevated window, Windows blocks the hook.
* `winbollocks` detects this Integrity Level mismatch and arms the **Missed Gesture Recovery** feature to catch your input once your cursor leaves the elevated window.
* **Workaround:** Running `winbollocks` with administrative privileges (via `runasadmin.bat`) avoids this limitation entirely.

---

### What it explicitly does *not* do

* It does **not** modify system keyboard mappings.
* It does **not** require administrator privileges (unless you want to manipulate admin windows like Task Manager).

The keyboard hook exists **solely** to track modifier key state (Win / Shift / Ctrl / Alt) and suppress Start menu activation after handled gestures (by swallowing the `winkey_UP` event and injecting a quick RShift tap). No other key combinations are acted upon.

---

### Implementation notes (technical)

* Uses `WH_MOUSE_LL` and `WH_KEYBOARD_LL`.
* Includes an `EVENT_SYSTEM_FOREGROUND` hook via `SetWinEventHook` to track window integrity levels in real-time.
* Locks its own working set memory into RAM to prevent being paged out, ensuring zero-lag hook responses.
* Pins its hook processor to a Time Critical thread (`THREAD_PRIORITY_TIME_CRITICAL`) and High Priority process class.
* Window movement is performed via explicit position updates (`SetWindowPos`).
* No shell extensions or DLL injection are used.

---

### Build

#### Requirements

You need `go.exe` of Go language to compile this code into a standalone exe.
No internet required to compile, if you have Go already installed.

#### Compile into .exe

There are several batch scripts provided for convenience:

* **`build.bat`**: GUI-subsystem build (no console window, silent in background). Recommended for daily use.
* **`devbuild.bat`**: Standard build with a console window attached. Great for debugging and seeing real-time logs.
* **`run.bat`**: Wrapper to run the compiled executable safely.
* **`runasadmin.bat`**: Wrapper to request UAC elevation and run the executable as an Administrator (required to drag/resize Admin windows).

---

### License

Licensed under the **Apache License, Version 2.0**.
See `LICENSE` for details.

---

## Third-party code

This repository includes vendored third-party Go modules under the `vendor/` directory so it can be built without internet access. Those components are licensed under their respective licenses.