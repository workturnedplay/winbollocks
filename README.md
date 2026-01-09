
## winbollocks

Originally located here: https://github.com/workturnedplay/winbollocks  
(unless you got it from a fork, try `git remote -v` to check)  

`winbollocks` is a small Windows utility that I use on win11, it adds a couple of **Win-key mouse gestures for window movement and z-order control**, independent of where the mouse cursor is inside the window.

It is designed to replicate common X11 / Wayland window-manager behaviors on Windows such as, for now only:  
winkey+LMB drag to move a window and winkey+MMB to send window to back behind all others.  

It was build with the help of AI (chatgpt 5.2 specifically). It works right now as stated below, but the code is a mess(for now) on purpose.

---

### What it does

`winbollocks` installs **low-level mouse and keyboard hooks** and reacts only to a small, fixed set of input combinations.

The following behaviors are implemented:

**1. Win + Left Mouse Button (drag anywhere)**
While the Windows key(it's between Ctrl key and Alt key) is held down:

* Pressing and holding **LMB** over any point inside a window starts a manual move of that window.
* The window follows the mouse until LMB is released.
* The click does **not** need to be on the title bar.
* The click is **not** passed through to the target window.

This allows moving/repositioning a window by dragging it from any point inside its bounds, not just from the title bar.

---

**2. Win + Middle Mouse Button (send to back)**
While the Windows key is held:

* Pressing **MMB** over a window sends that window to the bottom of the z-order.
* The window is not activated.
* Focus is preserved.

This is a pure z-order operation.

---

**3. Win + Shift + Middle Mouse Button (bring focused window to front)**
While the Windows key and **Shift** are held:

* Pressing **MMB** brings the **currently focused window** to the top of the z-order.
* This applies only to the focused window; non-focused windows are not affected(windows wouldn't allow it anyway, it'd just blink their taskbar buttons instead).
* This is intended for windows that were previously sent to the back using **Win + MMB** and remain focused but partially obscured and you want it back to the front again without clicking on another window first to then be able to click on the original window to bring it to front.
* The operation happens without requiring refocusing or clicking another window.

---

**4. Start menu suppression for these gestures**
If any of the above Win-key mouse gestures occur:

* Releasing the Windows key does **not** open the Start menu.

If no gesture occurs:

* Releasing the Windows key behaves normally.

This suppression applies **only** to gestures handled by `winbollocks`.

---

### Known Limitations

**Interaction with elevated (Administrator) windows**

When winbollocks is run without administrative privileges, it cannot reliably interact with windows that are running elevated (as Administrator). This is a Windows security boundary.

If a Win-key gesture is initiated while the cursor is over an elevated window:

* Keyboard and mouse events may stop being delivered to WinBollocks mid-gesture.
* The program may not receive the corresponding **Win-key release** event.
* As a result, WinBollocks may temporarily believe the Win key is still held down.

**Recovery:**
Pressing and releasing the Win key once more resets the internal state.

This occurs because Windows does not forward low-level input events from elevated windows to non-elevated global hooks. There is currently no reliable way for a non-admin process to detect that input delivery has been cut off or to infer the missing key-up event.

**Workaround:**
Running WinBollocks with administrative privileges avoids this limitation.

* Bringing a window to the front(winkey+shift+MMB) only works on the currently focused window.

### What it explicitly does *not* do

* It does **not** modify system keyboard mappings.
* It does **not** require administrator privileges.

The keyboard hook exists **solely** to:

* Track modifier key state (Win / Shift / Ctrl / Alt)
* Suppress Start menu activation(which Win key release triggers) after handled gestures (by swallowing the winkey_UP(ie. released) event then injecting/queueing a right_shift_DOWN(ie. right side Shift key pressed) then right_Shift_UP (ie. released) key sequence and that winkey_UP)

No other key combinations are acted upon.

---

### Modifier handling rules

Window movement gestures are triggered **only when**:

* The Windows key is down
* No other modifiers are active (unless explicitly part of the gesture)

Specifically:

* Win + LMB works only if Shift, Ctrl, and Alt are not pressed.
* Win + Shift + MMB (that's Midle Mouse Button) is explicitly allowed and handled.
* Other modifier combinations are ignored.

This avoids accidental activation during unrelated shortcuts.

---

### Focus and activation behavior

* Manual window moves do not require prior activation.
* Z-order changes(ie. send window to back) do not steal focus.
* Bringing a window to the front does not forcibly activate any windows and only works on the currently focused window.
* No foreground-window bypass tricks are used.

All behavior respects standard Windows focus rules.

---

### Implementation notes (technical)

* Uses `WH_MOUSE_LL` and `WH_KEYBOARD_LL`.
* Window movement is performed via explicit position updates.
* Z-order changes are performed with `SetWindowPos`.
* No shell extensions or DLL injection are used.
* Input suppression relies on documented hook return semantics.
* Start menu suppression is achieved without registry changes.

---

### Known limitations

* This utility relies on Windows’ global input model.
* Some behavior is constrained by Windows focus-stealing prevention.
* It does not attempt to emulate a full window manager.
* Behavior is tested on modern Windows 10/11 desktops.

---

### Build

#### Requirements

You need `go.exe` of Go language to compile this code into a standalone exe.  
No internet required to compile, if you have Go already installed.  

#### Compile into .exe

Standard Go build (if you want console messages visible):

```
go build
```

GUI-subsystem build (no console window, recommended):

```
go build -ldflags="-H=windowsgui"
```

Or try `build.bat`(no console) or `.\devbuild.bat`(yes console).  
That gives you an `.exe`, you can run it and it has a systray icon, RMB->Exit to stop it.  
Or, you can try `run.bat` which does the same thing(but Ctrl+C works too, to stop it), wrapped.  

---

### License

Licensed under the **Apache License, Version 2.0**.
See `LICENSE` for details.

---

## Third-party code

This repository includes vendored third-party Go modules under the `vendor/` directory so it can be built without internet access.

Those components are licensed under their respective licenses.
Individual license texts and notices are preserved alongside the vendored code.

