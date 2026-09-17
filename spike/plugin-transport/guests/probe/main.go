//go:build wasm

// The sandbox probe guest: a hostile plugin, as far as this toolchain lets one be
// written. It tries to open a socket, read a file, spawn a process and listen for
// connections, and reports what the runtime told it. It is compiled by exactly the
// command a real guest is, with no special flags — the point is that a plugin
// author does not have to be stopped from writing this, they just get nothing.
//
// Each export returns (ptr<<32 | len) of a plain-text line, using the same
// allocator the bare guest uses.
package main

import (
	"net"
	"os"
	"os/exec"
	"unsafe"
)

func main() {}

var pinned = map[uint32][]byte{}

//go:wasmexport obelo_alloc
func alloc(size uint32) uint32 {
	if size == 0 {
		size = 1
	}
	buf := make([]byte, size)
	ptr := uint32(uintptr(unsafe.Pointer(unsafe.SliceData(buf))))
	pinned[ptr] = buf
	return ptr
}

//go:wasmexport obelo_free
func free(ptr uint32) {
	delete(pinned, ptr)
}

func emit(s string) uint64 {
	b := []byte(s)
	ptr := alloc(uint32(len(b)))
	copy(pinned[ptr], b)
	return uint64(ptr)<<32 | uint64(len(b))
}

func report(what string, err error) uint64 {
	if err == nil {
		return emit(what + " => NO ERROR")
	}
	return emit(what + " => " + err.Error())
}

// probe_net tries to reach a public address with a raw TCP dial.
//
//go:wasmexport probe_net
func probeNet() uint64 {
	conn, err := net.Dial("tcp", "93.184.216.34:80")
	if err == nil {
		_ = conn.Close()
	}
	return report("net.Dial tcp 93.184.216.34:80", err)
}

// probe_file tries to read a file the host process can certainly read.
//
//go:wasmexport probe_file
func probeFile() uint64 {
	_, err := os.ReadFile("/etc/passwd")
	return report(`os.ReadFile("/etc/passwd")`, err)
}

// probe_dir tries to list the directory the plugin's own module would live in.
//
//go:wasmexport probe_dir
func probeDir() uint64 {
	_, err := os.ReadDir("/")
	return report(`os.ReadDir("/")`, err)
}

// probe_exec tries to spawn a process.
//
//go:wasmexport probe_exec
func probeExec() uint64 {
	err := exec.Command("/bin/sh", "-c", "echo pwned").Run()
	return report(`exec.Command("/bin/sh").Run()`, err)
}

var listener net.Listener

// probe_listen tries the other direction: accepting a connection. It reports the
// address it thinks it bound, which the host then checks for on the real machine —
// see TestAGuestCannotReachTheNetworkOrFilesystem. Go's wasip1 port ships an
// IN-PROCESS FAKE network stack (net/net_fake.go, build tag js || wasip1), so this
// call can succeed while binding nothing whatsoever on the host.
//
//go:wasmexport probe_listen
func probeListen() uint64 {
	ln, err := net.Listen("tcp", "127.0.0.1:34517")
	if err != nil {
		return report(`net.Listen("tcp", "127.0.0.1:34517")`, err)
	}
	listener = ln
	return emit(`net.Listen("tcp", "127.0.0.1:34517") => NO ERROR, Addr()=` + ln.Addr().String())
}

// probe_selfdial listens and then dials its own listener. It SUCCEEDS, and that
// success is the proof that the stack is a fake one living inside the guest's own
// linear memory rather than the host's: nothing the host can see is involved.
//
//go:wasmexport probe_selfdial
func probeSelfdial() uint64 {
	ln, err := net.Listen("tcp", "127.0.0.1:34518")
	if err != nil {
		return report("selfdial listen", err)
	}
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		return report("selfdial dial", err)
	}
	_ = conn.Close()
	return emit("net.Dial to the guest's OWN listener => NO ERROR (the stack is in-guest)")
}
