//go:build windows
// +build windows

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

// If you ever add a new Windows API call to winapi.go, you must remember to do the uintptr(unsafe.Pointer(&myVar))
// conversion directly inside the procName.Call(...) argument list. If you assign it to a variable first and pass that variable,
// the compiler shield(//go:uintptrescapes) drops.
package wincoe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	//"os"
	"runtime"
	"sync"
	"time"
	"unsafe"

	//"github.com/workturnedplay/wincoe/internal/wincall"
	"golang.org/x/sys/windows"
	// crap: dot import these so I don't have to prefix them!
	//. "github.com/workturnedplay/wincoe/internal/winconsts"
)

//type Exported struct{}

var (
	Iphlpapi = windows.NewLazySystemDLL("iphlpapi.dll")
	//procGetExtendedUdpTable = Iphlpapi.NewProc("GetExtendedUdpTable")
	procGetExtendedUdpTable = NewBoundProc(Iphlpapi, "GetExtendedUdpTable", CheckErrno)
	//procGetExtendedTcpTable = Iphlpapi.NewProc("GetExtendedTcpTable")
	procGetExtendedTcpTable = NewBoundProc(Iphlpapi, "GetExtendedTcpTable", CheckErrno)

	Kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	//procQueryFullProcessName = Kernel32.NewProc("QueryFullProcessImageNameW")
	//
	// Note: QueryFullProcessNameW expects 'size' to include the null terminator
	// on input, and returns the length WITHOUT the null terminator on success.
	procQueryFullProcessName = NewBoundProc(Kernel32, "QueryFullProcessImageNameW", CheckBool)
	// procCreateToolhelp32Snapshot = Kernel32.NewProc("CreateToolhelp32Snapshot")
	procCreateToolhelp32Snapshot = NewBoundProc(Kernel32, "CreateToolhelp32Snapshot", CheckHandle)
	// procProcess32First           = Kernel32.NewProc("Process32FirstW")
	procProcess32First = NewBoundProc(Kernel32, "Process32FirstW", CheckBool)
	// procProcess32Next            = Kernel32.NewProc("Process32NextW")
	procProcess32Next = NewBoundProc(Kernel32, "Process32NextW", CheckBool)
)

// auto runs before main(), loads the DLLs non-lazily.
func init() {
	loadDll(Kernel32)
	loadDll(Iphlpapi)
}

func loadDll(dll *windows.LazyDLL) {
	err := dll.Load()
	if err != nil {
		panic("critical system dll " + dll.Name + " not found, error: " + err.Error())
	}
}

// GetExtendedUDPTable retrieves the system UDP table using the Windows
// GetExtendedUdpTable API and returns the raw buffer containing the table data.
//
// This is a higher-level wrapper over the low-level bound call
// (callGetExtendedUdpTable). It encapsulates:
//
//   - the two-call pattern required by the API (size query + data fetch)
//   - conversion of Win32 error codes into Go errors via wincall.CheckErrno
//   - handling of ERROR_INSUFFICIENT_BUFFER as part of normal control flow
//
// The returned []byte contains a MIB_UDPTABLE_OWNER_PID (or related) structure,
// depending on the flags used internally. Callers are responsible for parsing
// the buffer according to the expected Windows structure layout.
//
// Guarantees:
//   - returns a non-nil error if the underlying API reports failure
//   - never requires callers to inspect r1 or perform manual error checks
//
// Edge cases handled:
//   - initial size query returning ERROR_INSUFFICIENT_BUFFER
//   - zero-sized buffer responses treated as error
//   - propagation of underlying Windows errors with errors.Is compatibility
//
// Note:
//   - this function intentionally operates on raw bytes to avoid committing
//     to a specific struct layout; build a typed parser on top if needed.
// func GetExtendedUDPTable() ([]byte, error) {
// 	var bufSize uint32

// 	// First call to GetExtendedUdpTable to get required buffer size.
// 	_, _, err := callGetExtendedUdpTable(
// 		0,
// 		uintptr(unsafe.Pointer(&bufSize)),
// 		0,
// 		uintptr(AF_INET),
// 		uintptr(UDP_TABLE_OWNER_PID),
// 		0,
// 	)

// 	if err != nil && !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
// 		return nil, err
// 	}

// 	if bufSize == 0 {
// 		return nil, errors.New("GetExtendedUdpTable returned size 0")
// 	}

// 	buf := make([]byte, bufSize)

// 	_, _, err = callGetExtendedUdpTable(
// 		uintptr(unsafe.Pointer(&buf[0])),
// 		uintptr(unsafe.Pointer(&bufSize)),
// 		0,
// 		uintptr(AF_INET),
// 		uintptr(UDP_TABLE_OWNER_PID),
// 		0,
// 	)

// 	if err != nil {
// 		return nil, err
// 	}

// 	return buf, nil
// }

// callWithRetry is a generic internal helper that manages the "query size,
// allocate, fetch data" pattern common in Windows network APIs.
//
// It handles the race condition where the required buffer size grows between
// the query and the fetch by retrying up to MAX_RETRIES times.
//
// Arguments:
//   - initialSize: The size to use for the first attempt (0 to query first).
//   - call: A closure that wraps the actual Windows syscall.
//
// Returns the populated byte slice on success, or an error if the API fails
// for reasons other than buffer size, or if it fails to stabilize after retries.
func callWithRetry(who string, initialSize uint32, call func(bufPtr *byte, s *uint32) error) ([]byte, error) {
	size := initialSize
	const MAX_RETRIES = 10
	for tries := 1; tries <= MAX_RETRIES; tries++ { // tries will be 1, 2, 3, ..., MAX_RETRIES
		//for tries := 0; tries < MAX_RETRIES; tries++ { // tries will be 0, 1, 2, ..., MAX_RETRIES-1
		//for tries := range MAX_RETRIES { // tries will be 0, 1, 2, ..., MAX_RETRIES-1
		//fmt.Printf("[GoR:%d]!%s before6 try %d, initialSize=%d size=%d\n", GoRoutineId(), who, tries, initialSize, size)
		// If size is 0, we're just probing. If > 0, we're allocating.
		var buf []byte
		//var p uintptr
		var ptr *byte = nil //implied anyway
		// const canary uint64 = 0xDEADBEEFCAFEBABE // it doesn't smash it, so no point in keeping it, thus commented out!
		// var canaryOffset int
		if size > 0 {
			buf = make([]byte, size) //+8) // 8 extra bytes
			//canaryOffset = len(buf) - 8
			// write canary at the end
			//binary.LittleEndian.PutUint64(buf[canaryOffset:], canary)
			ptr = &buf[0] // Keep it as a real, GC-visible pointer
			/*
				fmt.Printf with the %p verb treats a slice value specially: for a slice,
					%p prints the address of the first element (the Data pointer), not the address of the slice descriptor.
					The slice variable itself is a three-word header (pointer, len, cap) stored on the stack (or heap).
					The header's address is &buf; the header's Data field (pointer to element 0) is what fmt prints for %p when given a slice.

				So:

				    buf (the slice) ≠ &buf (address of the header).
				    fmt.Printf("%p", buf) prints buf's Data pointer (same as &buf[0] when len>0).
				    To print the header address use fmt.Printf("%p", &buf). To print the Data pointer explicitly
					use fmt.Printf("%p", unsafe.Pointer(&buf[0])) (only when len>0).

			*/
			//fmt.Printf("[GoR:%d]!%s middle7(created buf) try %d, buf=%p ptr=%p size=%d len(buf)=%d\n", GoRoutineId(), who, tries, buf, ptr, size, len(buf))
		}
		//fmt.Printf("[GoR:%d]!%s before7 try %d, ptr=%p &size=%p size=%d\n", GoRoutineId(), who, tries, ptr, &size, size)
		err := call(ptr, &size)
		//runtime.KeepAlive(buf)   // probably not needed but hey, ChatGPT.
		//runtime.KeepAlive(&size) // to satisfy Gemini 3.1 Thinking, no effect, still crashed. (cause was this https://github.com/golang/go/issues/77975 )
		//fmt.Printf("[GoR:%d]!%s after7 try %d, ptr=%p &size=%p size=%d\n", GoRoutineId(), who, tries, ptr, &size, size)
		// //check canary immediately after
		// if buf != nil { // guard for first iteration when size==0
		// 	if binary.LittleEndian.Uint64(buf[canaryOffset:]) != canary {
		// 		panic(fmt.Sprintf("CANARY SMASHED in callWithRetry after call, allocSize=%d, apiReportedSize=%d", canaryOffset, size))
		// 	}
		// }
		if err == nil {
			//fmt.Printf("[GoR:%d]!%s middle7(ret ok) try %d, buf=%p len(buf)=%d size=%d\n", GoRoutineId(), who, tries, buf, len(buf), size)
			if uint64(size) > uint64(len(buf)) {
				panic("impossible: size is bigger than len(buf)")
			}
			return buf, nil // epic fail here if returning buf[:size] because size is 0 even tho servicesReturned is > 0
			//return buf[:size], nil // fixed one issue! nope! because: The size parameter is only reliable when the API returns ERROR_MORE_DATA or ERROR_INSUFFICIENT_BUFFER. On success it is frequently set to 0, even when the buffer contains real data.
		}

		// Windows uses both INSUFFICIENT_BUFFER and MORE_DATA
		// to signal that we need a bigger boat.
		//GetExtendedUdpTable usually returns ERROR_INSUFFICIENT_BUFFER when the buffer is too small.
		//EnumServicesStatusEx (and many Enumeration APIs) returns ERROR_MORE_DATA.
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) &&
			!errors.Is(err, windows.ERROR_MORE_DATA) {
			//fmt.Printf("[GoR:%d]!%s middle7_2(ret err) try %d, err=%v\n", GoRoutineId(), who, tries, err)
			return nil, err
		}
		// Loop continues, using the updated 'size' from the failed call
		//however:
		// If size didn't increase but we still got an error,
		// we should nudge it upward to prevent an infinite loop.
		// We use uint64 casts to satisfy gosec G115.
		// 1. Convert both to uint64 to compare safely without narrowing (Fixes G115)
		if uint64(size) <= uint64(len(buf)) {
			//fmt.Printf("[GoR:%d]!%s before8 try %d, size=%d buf=%p len(buf)=%d\n", GoRoutineId(), who, tries, size, buf, len(buf))
			// 2. Check for overflow before adding 1024
			const increment = 1024
			const MaxInt = math.MaxUint32
			if MaxInt-size < increment {
				//fmt.Printf("[GoR:%d]!%s middle8 try %d\n", GoRoutineId(), who, tries)
				return nil, fmt.Errorf("buffer size(%d) would overflow uint32(%d) if adding %d", size, MaxInt, increment)
			}
			size += increment
			//fmt.Printf("[GoR:%d]!%s after8 try %d, new size=%d\n", GoRoutineId(), who, tries, size)
		}
		//fmt.Printf("[GoR:%d]!%s after6(end of for) try %d\n", GoRoutineId(), who, tries)
	}
	return nil, fmt.Errorf("buffer growth exceeded max retries(%d)", MAX_RETRIES)
}

// boolToUintptr converts a Go bool to a uintptr (1 for true, 0 for false)
// for use in Windows syscalls.
//
// boolToUintptr performs an explicit conversion from a Go bool to a
// Windows-compatible BOOL (uintptr(1) for true, uintptr(0) for false).
// This is required because Go bools cannot be directly cast to numeric types.
//
//go:inline
func boolToUintptr(b bool) uintptr {
	if b {
		return 1
	}
	return 0
}

// GetExtendedUDPTable retrieves the system UDP table using the Windows
// GetExtendedUdpTable API and returns the raw buffer containing the table data.
//
// This is a higher-level wrapper over the low-level bound call
// (callGetExtendedUdpTable). It encapsulates:
//
//   - the two-call pattern required by the API (size query + data fetch)
//   - conversion of Win32 error codes into Go errors via wincall.CheckErrno
//   - handling of ERROR_INSUFFICIENT_BUFFER as part of normal control flow
//
// The returned []byte contains a MIB_UDPTABLE_OWNER_PID (or related) structure,
// depending on the flags used internally. Callers are responsible for parsing
// the buffer according to the expected Windows structure layout.
//
// Guarantees:
//   - returns a non-nil error if the underlying API reports failure
//   - never requires callers to inspect r1 or perform manual error checks
//
// Edge cases handled:
//   - initial size query returning ERROR_INSUFFICIENT_BUFFER
//   - empty table responses (size 0) returning (nil, nil)
//   - propagation of underlying Windows errors with errors.Is compatibility
//
// Note:
//   - this function intentionally operates on raw bytes to avoid committing
//     to a specific struct layout; build a typed parser on top if needed.
func GetExtendedUDPTable(order bool, family uint32) ([]byte, error) {
	return callWithRetry("GetExtendedUDPTable", 0, func(bufPtr *byte, s *uint32) error {
		_, _, err := procGetExtendedUdpTable.Call(
			uintptr(unsafe.Pointer(bufPtr)),
			uintptr(unsafe.Pointer(s)),
			boolToUintptr(order),
			uintptr(family),
			uintptr(UDP_TABLE_OWNER_PID),
			0,
		)
		//err = CheckWinResult("LazyProc.Call() for procGetExtendedUdpTable", CheckErrno, r1, err)
		//these keepalives are probably not needed but hey, ChatGPT. they're not because go:uintptrescapes implies go:uintptrkeepalive plus escapes(makes them be, automatically) them to heap!
		//runtime.KeepAlive(bufPtr)
		//runtime.KeepAlive(s)
		return err
	})
}

// GetExtendedTCPTable retrieves the system TCP table.
// It follows the same contract as GetExtendedUDPTable.
func GetExtendedTCPTable(order bool, family uint32) ([]byte, error) {
	return callWithRetry("GetExtendedTCPTable", 0, func(bufPtr *byte, s *uint32) error {
		_, _, err := procGetExtendedTcpTable.Call(
			uintptr(unsafe.Pointer(bufPtr)),
			uintptr(unsafe.Pointer(s)),
			boolToUintptr(order),
			uintptr(family),
			uintptr(TCP_TABLE_OWNER_PID_ALL), // Value 5: Get all states + PID
			0,
		)
		//err = CheckWinResult("LazyProc.Call() for GetExtendedTCPTable", CheckErrno, r1, err)
		//these keepalives are probably not needed but hey, ChatGPT.
		//runtime.KeepAlive(bufPtr)
		//runtime.KeepAlive(s)
		return err
	})
}

// QueryFullProcessName retrieves the full executable path of a process given its PID.
//
// This is a higher-level wrapper over callQueryFullProcessName.
// It encapsulates:
//
//   - opening the process handle with PROCESS_QUERY_LIMITED_INFORMATION
//   - preparing a buffer for the UTF16 path
//   - calling the Windows API
//   - converting UTF16 to Go string
//
// Returns a non-empty string and nil error on success, or an empty string with error on failure.
func QueryFullProcessName(pid uint32) (string, error) {
	//fmt.Printf("[GoR:%d] !starting QueryFullProcessName pid=%d\n", GoRoutineId(), pid)
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		//fmt.Printf("[GoR:%d] !returning from QueryFullProcessName err=%v\n", GoRoutineId(), err)
		return "", fmt.Errorf("OpenProcess failedfor PID %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)

	// Start with MAX_PATH (260)
	//Yes, size remains a uint32 on both x86 and x64. This is because the Windows API function QueryFullProcessImageNameW
	// explicitly defines that parameter as a PDWORD (a pointer to a 32-bit unsigned integer), regardless of the processor architecture.
	size := uint32(windows.MAX_PATH)
	//size := uint32(3) // for tests
	var tries uint64 = 1
	for {
		buf := make([]uint16, size)
		currentCap := uint64(len(buf))
		if currentCap != uint64(size) { // must cast else compile error!
			impossibiru(fmt.Sprintf("currentCap(%d) != size(%d), after %d tries", currentCap, size, tries))
		}

		// Note: QueryFullProcessNameW expects 'size' to include the null terminator
		// on input, and returns the length WITHOUT the null terminator on success.

		// this works too:
		// err = windows.QueryFullProcessImageName(h, 0, &buf[0], &size)
		// r1 := (bool)(err == nil)
		// err = CheckWinResult("windows.QueryFullProcessImageName", CheckBool, boolToUintptr(r1), err)
		_, _, err = procQueryFullProcessName.Call(
			uintptr(h),
			0,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&size)),
		)
		//These 2 are not needed because implied by BoundProc.Call()'s //go:uintptrescapes
		// runtime.KeepAlive(&size)
		// runtime.KeepAlive(size)

		if err == nil {
			// Success! Convert the returned size to string
			//UTF16ToString is a function that looks for a 0x0000 (null).
			//size is just a number the API handed back, so let's not trust it, thus use full 'buf'
			//fmt.Printf("[GoR:%d] !returning from QueryFullProcessName OK\n", GoRoutineId())
			return windows.UTF16ToString(buf), nil
		}

		// Check if the error is specifically "Buffer too small"
		// syscall.ERROR_INSUFFICIENT_BUFFER = 0x7A
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			//fmt.Printf("[GoR:%d] !returning from QueryFullProcessName err=failed after %d tries\n", GoRoutineId(), tries)
			return "", fmt.Errorf("QueryFullProcessNameW failed after %d tries, err: '%w'", tries, err)
		}
		//else the desired 'size' now includes the nul terminator, so no need to +1 it

		// currentCap is what we just allocated; nextSize is what the API told us it wants.
		nextSize := uint64(size) //this is api suggested size now! ie. modified! so it's not same as currentCap!

		// If API didn't suggest a larger size, we manually double.
		if nextSize <= currentCap {
			nextSize = currentCap * 2
		}

		if currentCap < MaxExtendedPath && nextSize > MaxExtendedPath {
			// cap it once! in case we doubled it or (unlikely)api suggested more!(in the latter case it will fail the next syscall)
			nextSize = MaxExtendedPath
		}

		// Stern check against the Windows limit (32767) and the uint32 limit.
		if nextSize > MaxExtendedPath || nextSize > math.MaxUint32 {
			//fmt.Printf("[GoR:%d] !returning from QueryFullProcessName err=buffer size %d exceeds limit, after %d tries\n", GoRoutineId(), nextSize, tries)
			return "", fmt.Errorf("buffer size %d exceeds limit, after %d tries", nextSize, tries)
		}

		size = uint32(nextSize)
		tries += 1
	} // infinite 'for'
}

func impossibiru(msg string) {
	panic(fmt.Sprintf("Impossible: '%s'", msg))
}

// exePathFromPID returns process image path for pid or an error.
// Uses QueryFullProcessImageNameW. May fail if insufficient privilege.
//
// ExePathFromPID retrieves the full executable path of a process by PID.
//
// This is a higher-level wrapper over callQueryFullProcessName.
// It handles buffer sizing and UTF16 conversion.
//
// it's a wrapper-alias around QueryFullProcessName
func ExePathFromPID(pid uint32) (string, error) {
	return QueryFullProcessName(pid)
}

func GetProcessName(pid uint32) (string, error) {
	//fmt.Printf("[GoR:%d] !starting GetProcessName pid=%d\n", GoRoutineId(), pid)
	snapshot, err := CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0)
	if err != nil {
		//fmt.Printf("[GoR:%d] !returning from GetProcessName err=%v\n", GoRoutineId(), err)
		return "", err
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	const maxProcessEntries = 10000
	count := 0
	err = Process32First(snapshot, &entry)
	for err == nil {
		if count > maxProcessEntries {
			//fmt.Printf("[GoR:%d] !returning from GetProcessName err=limit(%d)! count=%d\n", GoRoutineId(), maxProcessEntries, count)
			return "", fmt.Errorf("Process32 enumeration exceeded safety limit")
		}
		count++
		//doneTODO: make a hard limit here, so it doesn't loop infinitely just in case.
		if entry.ProcessID == pid {
			// fmt.Printf("[GoR:%d] !returning from GetProcessName all good\n", GoRoutineId())
			return windows.UTF16ToString(entry.ExeFile[:]), nil
		}
		err = Process32Next(snapshot, &entry)
	}

	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		// fmt.Printf("[GoR:%d] !returning from GetProcessName err=%v\n", GoRoutineId(), err)
		return "", err
	}
	// fmt.Printf("[GoR:%d] !returning from GetProcessName err=not found\n", GoRoutineId())
	return "", fmt.Errorf("not found, err: %w", err)
}

// CreateToolhelp32Snapshot creates a snapshot of the specified processes, threads,
// modules, or heaps in the system. The snapshot can then be used with functions
// like Process32First/Next or Module32First/Next to enumerate the captured entries.
//
// In short: it’s a system-wide “frozen view” of processes or other kernel objects, enabling safe enumeration without interference from runtime changes.
//
// Parameters:
//
//	flagdwFlagss - a bitmask specifying what to include in the snapshot (e.g., TH32CS_SNAPPROCESS).
//	th32ProcessID   - for some snapshots, a process ID to restrict the snapshot to a particular process. (0 = all processes)
//
// Returns:
//
//	A handle to the snapshot, which must be closed with CloseHandle when done.
//	INVALID_HANDLE_VALUE indicates failure, with GetLastError providing details.
//
// Typical usage:
//
//	hSnap, err := CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0)
//	if err != nil { ... }
//	defer CloseHandle(hSnap)
//	// enumerate processes with Process32First/Next
//
// Returns a valid windows.Handle on success, or a non-nil error on failure.
//
// Notes:
//
// These flags are bitwise combinable. For example, TH32CS_SNAPPROCESS | TH32CS_SNAPTHREAD captures both processes and threads.
// If a flag isn’t used (e.g., you don’t include TH32CS_SNAPPROCESS), CreateToolhelp32Snapshot will not include that object type in the snapshot.
// TH32CS_SNAPPROCESS specifically tells the API to include all processes in the snapshot. Without it, Process32First/Process32Next won’t enumerate any processes.
func CreateToolhelp32Snapshot(dwFlags, th32ProcessID uint32) (windows.Handle, error) {
	// fmt.Printf("[GoR:%d] !starting CreateToolhelp32Snapshot\n", GoRoutineId())
	r1, _, err := procCreateToolhelp32Snapshot.Call(
		uintptr(dwFlags),
		uintptr(th32ProcessID),
	)
	// r1, err := windows.CreateToolhelp32Snapshot(dwFlags, th32ProcessID)
	// err = CheckWinResult("windows.CreateToolhelp32Snapshot", CheckHandle, 0, err)
	if err != nil {
		// fmt.Printf("[GoR:%d] !ending CreateToolhelp32Snapshot err=%v\n", GoRoutineId(), err)
		return 0, err
	}
	// fmt.Printf("[GoR:%d] !ending CreateToolhelp32Snapshot OK\n", GoRoutineId())
	return windows.Handle(r1), nil
	//return r1, nil
}

// // CreateProcessSnapshot is a convenience wrapper for creating a snapshot of all processes.
// //
// // Internally calls CreateToolhelp32Snapshot with TH32CS_SNAPPROCESS and PID 0.
// func (l *Exported) CreateProcessSnapshot() (windows.Handle, error) {

// 	return l.CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0)
// }

// Process32First wraps callProcess32First.
func Process32First(snapshot windows.Handle, entry *windows.ProcessEntry32) error {
	// fmt.Printf("[GoR:%d] !starting Process32First\n", GoRoutineId())
	if entry == nil {
		// fmt.Printf("[GoR:%d] !ending Process32First err=nil entry\n", GoRoutineId())
		return errors.New("Process32First: nil entry")
	}
	_, _, err := procProcess32First.Call(uintptr(snapshot), uintptr(unsafe.Pointer(entry)))
	// err := windows.Process32First(snapshot, entry)
	// var r1 bool = err == nil // true means r1 is 1, but when r1 is 0 it means error happened!
	// err = CheckWinResult("windows.Process32First", CheckBool, boolToUintptr(r1), err)

	// THIS is the anchor, says Gemini
	// It ensures 'entry' is considered "live" by the GC
	// until this specific line is reached.
	// runtime.KeepAlive(entry) // no need due to go:uintptrescapes implying go:uintptrkeepalive
	// fmt.Printf("[GoR:%d] !ending Process32First err=%v\n", GoRoutineId(), err)
	return err
}

// Process32Next wraps callProcess32Next.
func Process32Next(snapshot windows.Handle, entry *windows.ProcessEntry32) error {
	// fmt.Printf("[GoR:%d] !starting Process32Next\n", GoRoutineId())
	if entry == nil {
		// fmt.Printf("[GoR:%d] !ending Process32Next err=nil entry\n", GoRoutineId())
		return errors.New("Process32Next: nil entry")
	}
	_, _, err := procProcess32Next.Call(uintptr(snapshot), uintptr(unsafe.Pointer(entry)))
	// err := windows.Process32Next(snapshot, entry)
	// var r1 bool = err == nil
	// err = CheckWinResult("windows.Process32Next", CheckBool, boolToUintptr(r1), err)
	// THIS is the anchor, says Gemini
	// It ensures 'entry' is considered "live" by the GC
	// until this specific line is reached.
	// runtime.KeepAlive(entry) // no need due to go:uintptrescapes implying go:uintptrkeepalive
	// fmt.Printf("[GoR:%d] !ending Process32Next err=%v\n", GoRoutineId(), err)
	return err
}

// GetServiceNamesFromPIDUncached queries the Service Control Manager to find all service
// names currently associated with a specific Process ID (PID).
//
// This function encapsulates:
//   - opening a remote handle to the SCM with SC_MANAGER_ENUMERATE_SERVICE rights
//   - utilizing callWithRetry to handle the "snapshot" race condition where the
//     number of services changes between the size query and the data fetch
//   - parsing the resulting ENUM_SERVICE_STATUS_PROCESS structure array
//
// Returns a slice of service display names associated with the PID. If no
// services are found for the given PID, it returns (nil, nil).
//
// Guarantees:
//   - returns a non-nil error if SCM access is denied or the RPC call fails
//   - handles ERROR_INSUFFICIENT_BUFFER internally via the retry loop
//   - ensures the SCM handle is closed via defer, even on internal retry failure
//
// Edge cases handled:
//   - services starting/stopping mid-enumeration (handled by 10-try retry logic)
//   - PIDs with zero associated services (returns nil slice, no error)
//   - stale resume handles (reset to 0 on each retry for a fresh full snapshot)
//   - race conditions where the service list grows mid-call (handled by treating ERROR_MORE_DATA as a retry signal)
//
// Note:
//   - This performs a full enumeration of all Win32 services to filter by PID;
//     on systems with hundreds of services, this may involve a ~100KB+ buffer.
func GetServiceNamesFromPIDUncached(targetPID uint32) ([]string, error) {
	//TODO: since makeClientInfoContext is called on every single packet, and GetServiceNamesFromPID opens the SCM, enumerates all services, and does unsafe parsing — all under high concurrent load — you might consider caching the PID→services mapping with a short TTL to reduce both the performance impact and the attack surface of concurrent unsafe calls.
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return nil, fmt.Errorf("OpenSCManager failed: %w", err)
	}
	defer windows.CloseServiceHandle(scm)

	// We'll need these to persist across the closure calls
	var servicesReturned uint32

	//fmt.Printf("[GoR:%d] !before2(before callWithRetry in GetServiceNamesFromPIDUncached)\n", GoRoutineId())
	// Use our retry helper to handle the buffer growth logic
	// We use callWithRetry because the service list is highly volatile.
	buffer, err := callWithRetry("GetServiceNamesFromPIDUncached", 0, func(bufPtr *byte, s *uint32) error {
		// Reset these for each attempt to ensure a fresh enumeration if it retries
		servicesReturned = 0
		// Note: we usually keep resumeHandle at 0 for a fresh start on each retry
		// unless we are specifically doing paged enumeration.
		var currentResumeHandle uint32
		// fmt.Printf("[GoR:%d] !before5(before windows.EnumServicesStatusEx) servicesReturned=%d\n", GoRoutineId(), servicesReturned)
		errEnum := windows.EnumServicesStatusEx(
			scm,
			windows.SC_ENUM_PROCESS_INFO,
			windows.SERVICE_WIN32,
			windows.SERVICE_STATE_ALL,
			//(*byte)(unsafe.Pointer(p)),
			bufPtr,
			*s,
			s, // bytesNeeded
			&servicesReturned,
			&currentResumeHandle,
			nil,
		)
		//these keepalives are very likely not needed here, but hey, ChatGPT.
		//runtime.KeepAlive(bufPtr)
		//runtime.KeepAlive(s)

		// fmt.Printf("[GoR:%d] !after5(after windows.EnumServicesStatusEx) servicesReturned=%d\n", GoRoutineId(), servicesReturned)
		return errEnum
	})
	// fmt.Printf("[GoR:%d] !after2(after callWithRetry in GetServiceNamesFromPIDUncached)\n", GoRoutineId())

	if err != nil {
		return nil, fmt.Errorf("EnumServicesStatusEx failed: %w", err)
	}
	if buffer == nil {
		return nil, fmt.Errorf("nil buffer from callWithRetry, no error though")
	}
	if len(buffer) == 0 {
		return nil, fmt.Errorf("non-nil buffer with 0 length, from callWithRetry, no error though")
	}

	// Parsing logic remains the same, but now it's protected by the retry logic
	var serviceNames []string
	entrySize := unsafe.Sizeof(windows.ENUM_SERVICE_STATUS_PROCESS{})

	//this 'if' suggested by Claude Sonnet 4.6: (i DRY-ed the 'foo')
	if partialLen := uint64(servicesReturned) * uint64(entrySize); partialLen > uint64(len(buffer)) { // unlikely to ever be hit!
		// fmt.Printf("[GoR:%d] !middle3\n", GoRoutineId())
		return nil, fmt.Errorf("servicesReturned(%d) * entrySize(%d) = %d exceeds buffer len(%d): API invariant violated",
			servicesReturned, entrySize, partialLen, len(buffer))
	}
	// Trim the buffer to the actual data written, bad Grok 4.20! because data.ServiceName is a pointer past this size, still in the buffer tho!
	//buffer = buffer[:realLen] // safe slice header adjustment, no reallocation

	// fmt.Printf("[GoR:%d] !before3(a 'for' listing servicesReturned in GetServiceNamesFromPIDUncached)\n", GoRoutineId())
	for i := uint32(0); i < servicesReturned; i++ {
		offset := uintptr(i) * entrySize
		if offset+entrySize > uintptr(len(buffer)) {
			return nil, fmt.Errorf("entry %d at offset %d + entrySize %d exceeds buffer len %d",
				i, offset, entrySize, len(buffer))
		}
		data := (*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buffer[offset]))
		//runtime.GC() //Grok says this will crash because of 'data' being a pointer into buffer, wrong, it doesn't crash!

		// Validate ServiceName pointer is within buffer before dereferencing
		bufStart := uintptr(unsafe.Pointer(&buffer[0]))
		bufEnd := bufStart + uintptr(len(buffer))
		snPtr := uintptr(unsafe.Pointer(data.ServiceName))
		if snPtr < bufStart || snPtr >= bufEnd {
			//continue // pointer outside buffer — skip this entry
			return nil, fmt.Errorf("entry %d at offset %0x which has entrySize %d, in the buffer len %d, "+
				"has a ServiceName ptr outside the buffer=%p area, snPtr=%0x bufStart=%0x bufEnd=%0x",
				i, offset, entrySize, len(buffer),
				buffer, snPtr, bufStart, bufEnd)
		}

		if data.ServiceStatusProcess.ProcessId == targetPID {
			// fmt.Printf("[GoR:%d] !before4\n", GoRoutineId())
			str := windows.UTF16PtrToString(data.ServiceName)
			// fmt.Printf("[GoR:%d] !after4\n", GoRoutineId())
			// We use UTF16PtrToString because ServiceName is a *uint16
			// pointing into the same buffer returned by the API.
			serviceNames = append(serviceNames, str)
		}
	}
	// fmt.Printf("[GoR:%d] !after3(end of 'for' listing servicesReturned in GetServiceNamesFromPIDUncached)\n", GoRoutineId())

	//runtime.KeepAlive(buffer)     // keep buffer alive until all ServiceName pointer dereferences are done, because: windows.UTF16PtrToString(data.ServiceName) dereferences an absolute pointer written by the API into the buffer.
	//runtime.KeepAlive(&buffer[0]) //just in case
	//On KeepAlive: &buffer[offset] inside the loop is a live reference to buffer's backing array. The compiler can see that. buffer cannot be collected while the loop body executes because the loop body literally holds a pointer into it. KeepAlive there is redundant — it would only matter if you had extracted the pointer before the loop and used it after the last visible reference to buffer. That's not the case here. Drop it.
	return serviceNames, nil
}

// pidAndExeForUDP returns (pid, exePath_or_exeName, error).
// clientAddr should be the remote UDP address observed on the server side (e.g., 127.0.0.1:49936).
func PidAndExeForUDP(clientAddr *net.UDPAddr) (uint32, string, error) {
	//capital P in PidAndExeForUDP means exported, apparently!
	if clientAddr == nil {
		return 0, "", errors.New("nil clientAddr")
	}
	ip4 := clientAddr.IP.To4()
	if ip4 == nil {
		return 0, "", errors.New("only IPv4 supported")
	}
	port := uint16(clientAddr.Port)

	buf, err := GetExtendedUDPTable(false, AF_INET)
	if err != nil {
		return 0, "", err
	}

	if buf == nil {
		return 0, "", errors.New("GetExtendedUdpTable returned empty buffer which means there were no UDP entries in the table")
	}

	// Buffer layout: DWORD dwNumEntries; then array of MIB_UDPROW_OWNER_PID entries.
	if len(buf) < 4 {
		return 0, "", errors.New("GetExtendedUdpTable returned too small buffer")
	}
	num := binary.LittleEndian.Uint32(buf[:4])
	const rowSize = 12 // MIB_UDPROW_OWNER_PID has 3 DWORDs = 12 bytes
	offset := 4
	//var owningPid uint32
	for i := uint32(0); i < num; i++ {
		if offset+rowSize > len(buf) {
			panic(fmt.Sprintf("attempted to read beyond buffer in buf=%p len(buf)=%d offset=%d rowSize=%d i=%d\n", buf, len(buf), offset, rowSize, i))
			//break
		}
		localAddr := binary.LittleEndian.Uint32(buf[offset : offset+4])
		localPortRaw := binary.LittleEndian.Uint32(buf[offset+4 : offset+8])

		// localPortRaw stores port in network byte order in low 16 bits.
		localPort := uint16(localPortRaw & 0xFFFF)
		localPort = (localPort>>8)&0xFF | (localPort&0xFF)<<8 // convert to host order

		// convert DWORD IP (little-endian) to net.IP
		ipb := []byte{
			byte(localAddr & 0xFF),
			byte((localAddr >> 8) & 0xFF),
			byte((localAddr >> 16) & 0xFF),
			byte((localAddr >> 24) & 0xFF),
		}
		entryIP := net.IPv4(ipb[0], ipb[1], ipb[2], ipb[3])

		//fmt.Println("Checking:",entryIP,ip4, localPort, port)

		if localPort == port {
			// treat 0.0.0.0 as wildcard match
			if entryIP.Equal(net.IPv4zero) || entryIP.Equal(ip4) {
				// found PID for our IP:port tuple
				owningPid := binary.LittleEndian.Uint32(buf[offset+8 : offset+12])
				exe, err := ExePathFromPID(owningPid)
				if err != nil {
					//fmt.Println(err)
					// got error due to permissions needed for abs. path? this will work but it's just the .exe:
					//exe, err2 := wincoe.GetProcessName(owningPid) // shadowing is only a warning here, major footgun otherwise.

					var err2 error // Declare err2 so we don't have to use :=
					exe, err2 = GetProcessName(owningPid)

					if err2 != nil {
						return 0, "", fmt.Errorf("pid %d not found for %s, errTransient:'%v', err:'%w'", owningPid, clientAddr.String(), err, err2)
					}

					//_ = exe // enable when trying for shadowing
				}
				return owningPid, exe, nil
			}
		}

		//prepare for next entry
		offset += rowSize
	} //for

	return 0, "", fmt.Errorf("no matching UDP socket entry found for %s (ephemeral port reuse or socket already closed by kernel) thus dno who sent it", clientAddr.String())
}

// clientAddr should be the remote TCP address observed on the server side (e.g., 127.0.0.1:49936).
func PidAndExeForTCP(clientAddr *net.TCPAddr) (uint32, string, error) {
	if clientAddr == nil {
		return 0, "", errors.New("nil clientAddr")
	}
	ip4 := clientAddr.IP.To4()
	if ip4 == nil {
		return 0, "", errors.New("only IPv4 supported")
	}
	port := uint16(clientAddr.Port)

	// Fetch the table
	buf, err := GetExtendedTCPTable(false, AF_INET) //FIXME: do I need here to include the AF_INET6 ?! probably, and for UDP func too!
	if err != nil {
		return 0, "", err
	}
	if buf == nil {
		return 0, "", errors.New("GetExtendedTcpTable returned empty buffer")
	}

	if len(buf) < 4 {
		return 0, "", errors.New("GetExtendedTcpTable buffer too small for header")
	}

	num := binary.LittleEndian.Uint32(buf[:4])

	// MIB_TCPROW_OWNER_PID structure:
	// 0: dwState (4 bytes)
	// 4: dwLocalAddr (4 bytes)
	// 8: dwLocalPort (4 bytes)
	// 12: dwRemoteAddr (4 bytes)
	// 16: dwRemotePort (4 bytes)
	// 20: dwOwningPid (4 bytes)
	const rowSize = 24
	offset := 4

	for i := uint32(0); i < num; i++ {
		if offset+rowSize > len(buf) {
			break
		}

		// Extract fields based on the 24-byte MIB_TCPROW_OWNER_PID layout
		localAddrRaw := binary.LittleEndian.Uint32(buf[offset+4 : offset+8])
		localPortRaw := binary.LittleEndian.Uint32(buf[offset+8 : offset+12])
		owningPid := binary.LittleEndian.Uint32(buf[offset+20 : offset+24])

		// Advance offset for next iteration
		offset += rowSize

		// Port conversion (Network Byte Order in low 16 bits)
		localPort := uint16(localPortRaw & 0xFFFF)
		localPort = (localPort>>8)&0xFF | (localPort&0xFF)<<8

		if localPort == port {
			// Convert DWORD IP (little-endian) to net.IP
			entryIP := net.IPv4(
				byte(localAddrRaw&0xFF),
				byte((localAddrRaw>>8)&0xFF),
				byte((localAddrRaw>>16)&0xFF),
				byte((localAddrRaw>>24)&0xFF),
			)

			// Match logic (Wildcard 0.0.0.0 or specific IP)
			if entryIP.Equal(net.IPv4zero) || entryIP.Equal(ip4) {
				exe, err := ExePathFromPID(owningPid)
				if err != nil {
					// Fallback to process name if path is inaccessible
					var err2 error
					exe, err2 = GetProcessName(owningPid)
					if err2 != nil {
						return 0, "", fmt.Errorf("pid %d found but exe lookup failed: %w", owningPid, err2)
					}
				}
				return owningPid, exe, nil
			}
		}
	}

	return 0, "", fmt.Errorf("no TCP owner found for %s", clientAddr.String())
}

// serviceNameCache caches PID→service-names with a short TTL to avoid
// hammering EnumServicesStatusEx on every packet under high concurrency.
// This also eliminates the concurrent unsafe-buffer pressure that caused
// the STATUS_ACCESS_VIOLATION crash under -race load. No, the cause was this: https://github.com/golang/go/issues/77975
type serviceCacheEntry struct {
	names     []string
	expiresAt time.Time
}

var (
	serviceCache    = make(map[uint32]serviceCacheEntry)
	serviceCacheMu  sync.Mutex
	serviceCacheTTL = 60 * time.Second
)

func GetServiceNamesFromPIDCached(targetPID uint32) ([]string, error) {
	// Fast path: check cache under lock.
	serviceCacheMu.Lock()
	if entry, ok := serviceCache[targetPID]; ok && time.Now().Before(entry.expiresAt) {
		names := entry.names
		serviceCacheMu.Unlock()
		return names, nil
	}
	serviceCacheMu.Unlock()

	// Slow path: do the actual SCM enumeration.
	names, err := GetServiceNamesFromPIDUncached(targetPID)
	if err != nil {
		return nil, err
	}

	serviceCacheMu.Lock()
	serviceCache[targetPID] = serviceCacheEntry{
		names:     names,
		expiresAt: time.Now().Add(serviceCacheTTL),
	}
	serviceCacheMu.Unlock()

	return names, nil
}

func GoRoutineId() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// "goroutine 17 [running]:\n..."
	var id int64 = -1
	fmt.Sscanf(string(buf[:n]), "goroutine %d", &id)
	return id
}

// //made(wrongly) by Claude Sonnet 4.6
// func InstallCrashSink() {
// 	f, err := os.OpenFile("crash.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
// 	if err != nil {
// 		return
// 	}
// 	// Redirect stderr — Go runtime writes its panic output there
// 	// On Windows with CGO+race, stderr is where the runtime dumps stacks.
// 	// We dup2 it so both console AND file get it.
// 	if err := RedirectStderrToFile(f); err != nil {
// 		f.Close()
// 	}
// }

// func RedirectStderrToFile(f *os.File) error {
// 	return windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd()))
// }
