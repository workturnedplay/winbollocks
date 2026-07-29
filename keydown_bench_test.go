package main

//GetAsyncKeyState does not need runtime.LockOSThread()

import (
	"github.com/workturnedplay/wincoe"
	"golang.org/x/sys/windows"
	"syscall"
	"testing"
)

// BenchmarkKeyDown measures both execution time and heap allocations.
func BenchmarkKeyDown(b *testing.B) {
	b.ReportAllocs()

	// VK_SHIFT is arbitrary; any valid virtual-key code works.
	const vk = uintptr(0x10)

	b.ResetTimer()
	for b.Loop() {
		_ = keyDown(vk)
	}
}

// TestKeyDownAllocs reports the average number of heap allocations
// performed by a single keyDown call.
func TestKeyDownAllocs(t *testing.T) {
	const vk = uintptr(0x10)

	allocs := testing.AllocsPerRun(100000, func() {
		_ = keyDown(vk)
	})

	t.Logf("keyDown: %.6f allocs/call", allocs)
}

func BenchmarkBoundProcKeyDown(b *testing.B) {
	b.ReportAllocs()

	const vk = uintptr(0x10)
	var procGetAsyncKeyStateN = wincoe.NewBoundProcN(wincoe.User32, "GetAsyncKeyState", wincoe.CheckNone) // returns short

	b.ResetTimer()
	for b.Loop() {
		_ = procGetAsyncKeyStateN.Call(vk)
	}
}

func BenchmarkBoundProcGetAsyncStateArity1(b *testing.B) {
	b.ReportAllocs()

	const vk = uintptr(0x10)
	var procGetAsyncKeyState1 = wincoe.NewBoundProc1(wincoe.User32, "GetAsyncKeyState", wincoe.CheckNone) // returns short

	b.ResetTimer()
	for b.Loop() {
		_ = procGetAsyncKeyState1.Call(vk)
	}
}

func BenchmarkDirectSyscallNKeyDown(b *testing.B) {
	b.ReportAllocs()

	const vk = uintptr(0x10) // VK_SHIFT

	var rawGetAsyncKeyState = wincoe.User32.NewProc("GetAsyncKeyState")
	addr := rawGetAsyncKeyState.Addr()

	b.ResetTimer()
	for b.Loop() {
		r1, _, err := syscall.SyscallN(addr, vk)
		_ = r1
		_ = err
	}
}

//go:uintptrescapes
func winCall1(proc *windows.LazyProc, a1 uintptr) uintptr {
	r1, _, _ := syscall.SyscallN(proc.Addr(), a1)
	return r1
}

func BenchmarkWinCall1KeyDown(b *testing.B) {
	b.ReportAllocs()

	const vk = uintptr(0x10)
	var rawGetAsyncKeyState = wincoe.User32.NewProc("GetAsyncKeyState")

	b.ResetTimer()
	for b.Loop() {
		_ = winCall1(rawGetAsyncKeyState, vk)
	}
}
