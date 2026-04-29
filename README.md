## winbollocks

Originally located here: [https://github.com/workturnedplay/winbollocks](https://github.com/workturnedplay/winbollocks)
(unless you got it from a fork, try `git remote -v` to check)

`winbollocks` is a small Windows utility that I use on win11, it adds a couple of **Win-key mouse gestures for window movement, resizing, and z-order control**, independent of where the mouse cursor is inside the window.

It is designed to replicate common X11 / Wayland window-manager behaviors on Windows such as:
winkey+LMB drag to move a window, winkey+RMB drag to resize, and winkey+MMB to send a window to back behind all others.

It was built with the help of AI (chatgpt 5.2 and 5.1Mini, Grok 4 and 4.1, Gemini 3 and 3.1 Pro), non-agentic via website/chat only.
It works right now as stated below, but the code is a mess(for now) on purpose.

---

### What it does

`winbollocks` installs **low-level mouse and keyboard hooks** and reacts only to a small, fixed set of input combinations. It also places an icon in the System Tray for quick configuration.

The following behaviors are implemented:

**1. Win + Left Mouse Button (drag anywhere to move)**
While the Windows key (between Ctrl and Alt) is held down:
* Pressing and holding **LMB** over any point inside a window starts a manual move of that window.
* The window follows the mouse until LMB is released.
* The click does **not** need to be on the title bar.
* The click is **not** passed through to the target window.

**2. Win + Right Mouse Button (drag anywhere to resize)**
While the Windows key is held down:
* Pressing and holding **RMB** over a window initiates a resize operation.
* The screen is divided into a 9-zone grid. Dragging from the edges or corners resizes the window in that specific direction.
* Dragging from the **center zone** resizes the window uniformly while respecting its initial aspect ratio.
* While resizing, a helpful green-on-black overlay appears on screen, displaying the live dimensions and pixel delta.

**3. Win + Middle Mouse Button (send to back)**
While the Windows key is held:
* Pressing **MMB** over a window sends that window to the bottom of the z-order.
* The window is not activated. Focus is preserved.

**4. Win + Shift + Middle Mouse Button (bring focused window to front)**
While the Windows key and **Shift** are held:
* Pressing **MMB** brings the **currently focused window** to the top of the z-order.
* This applies only to the focused window; non-focused windows are not affected.
* This is intended for windows that were previously sent to the back using **Win + MMB** and remain focused but partially obscured.

**5. Start menu suppression for these gestures**
If any of the above Win-key mouse gestures occur:
* Releasing the Windows key does **not** open the Start menu.
* If no gesture occurs, releasing the Windows key behaves normally.
* This suppression applies **only** to gestures handled by `winbollocks`.

---

### System Tray Configuration

Right-clicking the `winbollocks` icon in the system tray provides a few live toggles:

* **Activate window when moved:** Tries to bring the window into focus when you drag it (uses a thread-attaching focus method to bypass Windows' focus-stealing prevention).
* **Fallback: Use Left Mouse Click to focus:** If standard activation fails, it injects a physical LMB click to force focus. *(Warning: this will click underlying UI elements where your cursor is).*
* **Rate-limit window moves:** Throttles the move events to save CPU usage. It drops events that happen faster than ~10ms apart. This saves CPU but might feel slightly choppier.
* **Log rate of moves:** Logs telemetry about dropped vs. actual moves (only selectable if Rate-limiting is enabled).

---

### Known Limitations

**Interaction with elevated (Administrator) windows**

When `winbollocks` is run without administrative privileges, it cannot reliably interact with windows that are running elevated (as Administrator). This is a Windows security boundary (UIPI).

* If a Win-key gesture is initiated over an elevated window, Windows blocks the hook. `winbollocks` will now detect this Integrity Level mismatch and display a Windows Notification (toast/balloon) warning you that access is denied.
* The program may miss the **Win-key release** event.
* **Recovery:** Pressing and releasing the Win key once more resets the internal state.
* **Workaround:** Running `winbollocks` with administrative privileges (via `runasadmin.bat`) avoids this limitation entirely.

**Focusing limits:** Bringing a window to the front (winkey+shift+MMB) only works on the currently focused window.

---

### What it explicitly does *not* do

* It does **not** modify system keyboard mappings.
* It does **not** require administrator privileges (unless you want to manipulate admin windows like Task Manager's window).

The keyboard hook exists **solely** to track modifier key state (Win / Shift / Ctrl / Alt) and suppress Start menu activation after handled gestures (by swallowing the `winkey_UP` event and injecting a quick RShift tap). No other key combinations are acted upon.

---

### Modifier handling rules

Window movement gestures are triggered **only when**:
* The Windows key is down
* No other modifiers are active (unless explicitly part of the gesture, like Shift for bringing to front)

This avoids accidental activation during unrelated system shortcuts.

---

### Implementation notes (technical)

* Uses `WH_MOUSE_LL` and `WH_KEYBOARD_LL`.
* Includes an `EVENT_SYSTEM_FOREGROUND` hook via `SetWinEventHook` to track window integrity levels.
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

