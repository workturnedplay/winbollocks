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

package wincoe

import (
	"errors"
	"fmt"
	//"strings"
	"encoding/binary"
	"math"
	"net"
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
	callGetExtendedUdpTable = NewBoundProc(Iphlpapi, "GetExtendedUdpTable", CheckErrno)
	callGetExtendedTcpTable = NewBoundProc(Iphlpapi, "GetExtendedTcpTable", CheckErrno)

	Kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	//procQueryFullProcessName = Kernel32.NewProc("QueryFullProcessImageNameW")
	//
	// Note: QueryFullProcessNameW expects 'size' to include the null terminator
	// on input, and returns the length WITHOUT the null terminator on success.
	callQueryFullProcessName = NewBoundProc(Kernel32, "QueryFullProcessImageNameW", CheckBool)
	// procCreateToolhelp32Snapshot = Kernel32.NewProc("CreateToolhelp32Snapshot")
	callCreateToolhelp32Snapshot = NewBoundProc(Kernel32, "CreateToolhelp32Snapshot", CheckHandle)
	// procProcess32First           = Kernel32.NewProc("Process32FirstW")
	callProcess32First = NewBoundProc(Kernel32, "Process32FirstW", CheckBool)
	// procProcess32Next            = Kernel32.NewProc("Process32NextW")
	callProcess32Next = NewBoundProc(Kernel32, "Process32NextW", CheckBool)
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
func callWithRetry(initialSize uint32, call func(p uintptr, s *uint32) error) ([]byte, error) {
	size := initialSize
	const MAX_RETRIES = 10
	for tries := 0; tries < MAX_RETRIES; tries++ {
		// If size is 0, we're just probing. If > 0, we're allocating.
		var buf []byte
		var p uintptr
		if size > 0 {
			buf = make([]byte, size)
			p = uintptr(unsafe.Pointer(&buf[0]))
		}

		err := call(p, &size)
		if err == nil {
			return buf, nil
		}

		// Windows uses both INSUFFICIENT_BUFFER and MORE_DATA
		// to signal that we need a bigger boat.
		//GetExtendedUdpTable usually returns ERROR_INSUFFICIENT_BUFFER when the buffer is too small.
		//EnumServicesStatusEx (and many Enumeration APIs) returns ERROR_MORE_DATA.
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) &&
			!errors.Is(err, windows.ERROR_MORE_DATA) {
			return nil, err
		}
		// Loop continues, using the updated 'size' from the failed call
		//however:
		// If size didn't increase but we still got an error,
		// we should nudge it upward to prevent an infinite loop.
		if size <= uint32(len(buf)) {
			size += 1024
		}
	}
	return nil, fmt.Errorf("buffer growth exceeded max retries(%d)", MAX_RETRIES)
}

// boolToUintptr converts a Go bool to a uintptr (1 for true, 0 for false)
// for use in Windows syscalls.
//
// boolToUintptr performs an explicit conversion from a Go bool to a
// Windows-compatible BOOL (uintptr(1) for true, uintptr(0) for false).
// This is required because Go bools cannot be directly cast to numeric types.
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
	return callWithRetry(0, func(p uintptr, s *uint32) error {
		_, _, err := callGetExtendedUdpTable(
			p,
			uintptr(unsafe.Pointer(s)),
			boolToUintptr(order),
			uintptr(family),
			uintptr(UDP_TABLE_OWNER_PID),
			0,
		)
		return err
	})
}

// GetExtendedTCPTable retrieves the system TCP table.
// It follows the same contract as GetExtendedUDPTable.
func GetExtendedTCPTable(order bool, family uint32) ([]byte, error) {
	return callWithRetry(0, func(p uintptr, s *uint32) error {
		_, _, err := callGetExtendedTcpTable(
			p,
			uintptr(unsafe.Pointer(s)),
			boolToUintptr(order),
			uintptr(family),
			uintptr(TCP_TABLE_OWNER_PID_ALL), // Value 5: Get all states + PID
			0,
		)
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
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
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
		_, _, err = callQueryFullProcessName(
			uintptr(h),
			0,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(unsafe.Pointer(&size)),
		)

		if err == nil {
			// Success! Convert the returned size to string
			//UTF16ToString is a function that looks for a 0x0000 (null).
			//size is just a number the API handed back, so let's not trust it, thus use full 'buf'
			return windows.UTF16ToString(buf), nil
		}

		// Check if the error is specifically "Buffer too small"
		// syscall.ERROR_INSUFFICIENT_BUFFER = 0x7A
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
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
	snapshot, err := CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	err = Process32First(snapshot, &entry)
	for err == nil {
		//TODO: make a hard limit here, so it doesn't loop infinitely just in case.
		if entry.ProcessID == pid {
			return windows.UTF16ToString(entry.ExeFile[:]), nil
		}
		err = Process32Next(snapshot, &entry)
	}

	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return "", err
	}
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
	r1, _, err := callCreateToolhelp32Snapshot(
		uintptr(dwFlags),
		uintptr(th32ProcessID),
	)
	if err != nil {
		return 0, err
	}
	return windows.Handle(r1), nil
}

// // CreateProcessSnapshot is a convenience wrapper for creating a snapshot of all processes.
// //
// // Internally calls CreateToolhelp32Snapshot with TH32CS_SNAPPROCESS and PID 0.
// func (l *Exported) CreateProcessSnapshot() (windows.Handle, error) {

// 	return l.CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0)
// }

// Process32First wraps callProcess32First.
func Process32First(snapshot windows.Handle, entry *windows.ProcessEntry32) error {
	_, _, err := callProcess32First(uintptr(snapshot), uintptr(unsafe.Pointer(entry)))
	return err
}

// Process32Next wraps callProcess32Next.
func Process32Next(snapshot windows.Handle, entry *windows.ProcessEntry32) error {
	_, _, err := callProcess32Next(uintptr(snapshot), uintptr(unsafe.Pointer(entry)))
	return err
}

// GetServiceNamesFromPID queries the Service Control Manager to find all service
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
func GetServiceNamesFromPID(targetPID uint32) ([]string, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return nil, fmt.Errorf("OpenSCManager failed: %w", err)
	}
	defer windows.CloseServiceHandle(scm)

	// We'll need these to persist across the closure calls
	var servicesReturned uint32

	// Use our retry helper to handle the buffer growth logic
	// We use callWithRetry because the service list is highly volatile.
	buffer, err := callWithRetry(0, func(p uintptr, s *uint32) error {
		// Reset these for each attempt to ensure a fresh enumeration if it retries
		servicesReturned = 0
		// Note: we usually keep resumeHandle at 0 for a fresh start on each retry
		// unless we are specifically doing paged enumeration.
		var currentResumeHandle uint32

		errEnum := windows.EnumServicesStatusEx(
			scm,
			windows.SC_ENUM_PROCESS_INFO,
			windows.SERVICE_WIN32,
			windows.SERVICE_STATE_ALL,
			(*byte)(unsafe.Pointer(p)),
			*s,
			s, // bytesNeeded
			&servicesReturned,
			&currentResumeHandle,
			nil,
		)
		return errEnum
	})

	if err != nil {
		return nil, fmt.Errorf("EnumServicesStatusEx failed: %w", err)
	}

	// Parsing logic remains the same, but now it's protected by the retry logic
	var serviceNames []string
	entrySize := unsafe.Sizeof(windows.ENUM_SERVICE_STATUS_PROCESS{})

	for i := uint32(0); i < servicesReturned; i++ {
		offset := uintptr(i) * entrySize
		data := (*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buffer[offset]))

		if data.ServiceStatusProcess.ProcessId == targetPID {
			// We use UTF16PtrToString because ServiceName is a *uint16
			// pointing into the same buffer returned by the API.
			serviceNames = append(serviceNames, windows.UTF16PtrToString(data.ServiceName))
		}
	}

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
	for i := uint32(0); i < num; i++ {
		if offset+rowSize > len(buf) {
			break
		}
		localAddr := binary.LittleEndian.Uint32(buf[offset : offset+4])
		localPortRaw := binary.LittleEndian.Uint32(buf[offset+4 : offset+8])
		owningPid := binary.LittleEndian.Uint32(buf[offset+8 : offset+12])
		offset += rowSize

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
				// found PID
				exe, err := ExePathFromPID(owningPid)
				if err != nil {
					//fmt.Println(err)
					// got error due to permissions needed for abs. path? this will work but it's just the .exe:
					//exe, err2 := wincoe.GetProcessName(owningPid) // shadowing is only a warning here, major footgun otherwise.

					var err2 error // Declare err2 so we don't have to use :=
					exe, err2 = GetProcessName(owningPid)

					if err2 != nil {
						return 0, "", fmt.Errorf("pid %d not found for %s, errTransient:'%v', err:'%w'", num, clientAddr.String(), err, err2)
					}

					//_ = exe // enable when trying for shadowing
				}
				return owningPid, exe, nil
			}
		}
	}

	return 0, "", fmt.Errorf("pid %d not found for %s", num, clientAddr.String())
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
