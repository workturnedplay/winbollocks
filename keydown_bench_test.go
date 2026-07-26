package main

import (
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

	b.ResetTimer()
	for b.Loop() {
		_ = procGetAsyncKeyState.Call(vk)
	}
}

func BenchmarkDirectSyscallNKeyDown(b *testing.B) {
	b.ReportAllocs()

	const vk = uintptr(0x10) // VK_SHIFT

	addr := rawGetAsyncKeyState.Addr()

	b.ResetTimer()
	for b.Loop() {
		r1, _, err := syscall.SyscallN(addr, vk)
		_ = r1
		_ = err
	}
}
