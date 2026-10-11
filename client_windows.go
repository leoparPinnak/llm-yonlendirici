package main

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	iphlpapi                   = syscall.NewLazyDLL("iphlpapi.dll")
	getExtendedTcpTable        = iphlpapi.NewProc("GetExtendedTcpTable")
	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	openProcess                = kernel32.NewProc("OpenProcess")
	queryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	closeHandle                = kernel32.NewProc("CloseHandle")
)

const (
	afInet                         = 2
	tcpTableOwnerPIDAll            = 5
	processQueryLimitedInformation = 0x1000
)

type mibTcpRowOwnerPID struct {
	State      uint32
	LocalAddr  uint32
	LocalPort  uint32
	RemoteAddr uint32
	RemotePort uint32
	OwningPid  uint32
}

// resolveClientPID returns the Windows PID that owns the given local TCP port.
func resolveClientPID(port int) uint32 {
	var size uint32
	getExtendedTcpTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, afInet, tcpTableOwnerPIDAll, 0)
	if size == 0 {
		return 0
	}

	buf := make([]byte, size)
	ret, _, _ := getExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, afInet, tcpTableOwnerPIDAll, 0)
	if ret != 0 {
		return 0
	}

	numEntries := binary.LittleEndian.Uint32(buf[:4])
	rowSize := int(unsafe.Sizeof(mibTcpRowOwnerPID{}))
	offset := 4

	for i := uint32(0); i < numEntries; i++ {
		if offset+rowSize > len(buf) {
			break
		}
		row := (*mibTcpRowOwnerPID)(unsafe.Pointer(&buf[offset]))
		offset += rowSize

		localPort := int(binary.BigEndian.Uint16((*[2]byte)(unsafe.Pointer(&row.LocalPort))[:]))
		if localPort == port {
			return row.OwningPid
		}
	}
	return 0
}

// resolveProcessName returns the executable name (e.g. "node.exe", "chrome.exe") for a given PID.
func resolveProcessName(pid uint32) string {
	if pid == 0 {
		return ""
	}
	h, _, _ := openProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return fmt.Sprintf("PID:%d", pid)
	}
	defer closeHandle.Call(h)

	var buf [1024]uint16
	size := uint32(len(buf))
	ret, _, _ := queryFullProcessImageNameW.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if ret != 0 {
		fullPath := syscall.UTF16ToString(buf[:size])
		return filepath.Base(fullPath)
	}
	return fmt.Sprintf("PID:%d", pid)
}

// resolveClientInfo looks up both PID and Process name for a client port.
func resolveClientInfo(port int) (pid uint32, procName string) {
	pid = resolveClientPID(port)
	if pid != 0 {
		procName = resolveProcessName(pid)
	}
	return pid, procName
}
