// seccomp_linux.go: the system call filter
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

//go:build linux

package sandbox

import (
	"encoding/binary"
	"os"
	"runtime"

	"github.com/agilira/go-errors"
	"golang.org/x/sys/unix"
)

func auditArch() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return unix.AUDIT_ARCH_X86_64, nil
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64, nil
	}
	return 0, errors.New(ErrUnavailable, "the sandbox supports Linux amd64 and arm64")
}

// denied are system calls an ordinary program does not need and that open
// kernel attack surface: keyrings, io_uring (which bypasses this filter),
// eBPF, perf events, userfaultfd (a race amplifier for kernel exploits),
// kernel modules and kexec, and hard links (which could pin a protected
// file under a writable name). The same choices as Docker's default
// profile, plus io_uring.
func denied() []uint32 {
	calls := []uint32{
		unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
		unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
		unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD,
		unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE,
		unix.SYS_KEXEC_LOAD, unix.SYS_KEXEC_FILE_LOAD,
		unix.SYS_LINKAT,
	}
	if runtime.GOARCH == "amd64" {
		// link(2) and modify_ldt(2) exist on amd64 only.
		calls = append(calls, 86, 154) // numbers, not unix constants: the arm64 build has no SYS_MODIFY_LDT
	}
	return calls
}

// seccompFilter builds the classic BPF program bwrap installs. Unknown
// architectures are killed, x32 system calls refused, the denied list
// answers EPERM. Without network, socket(2) is refused entirely; with it,
// only IPv4 and IPv6 sockets are allowed, so a mounted Unix socket cannot
// reach a host service.
func seccompFilter(network bool) (*os.File, error) {
	arch, err := auditArch()
	if err != nil {
		return nil, err
	}
	deny := uint32(unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM))
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: arch, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K, K: 0x40000000, Jf: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: deny},
	}
	calls := denied()
	if !network {
		calls = append(calls, unix.SYS_SOCKET)
	}
	for _, nr := range calls {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: nr, Jf: 1},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny})
	}
	if network {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_SOCKET, Jf: 4},
			unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.AF_INET, Jt: 2},
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.AF_INET6, Jt: 1},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: deny})
	}
	filter = append(filter, socketpairRule(deny)...)
	filter = append(filter, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	return programFile(filter)
}

// socketpairRule allows socketpair(2) for stream and seqpacket pairs only.
// A datagram pair can be re-aimed with sendto or connect at any Unix
// socket on a path the sandbox can see, a host service included; stream
// pairs cannot, and they are what event loops (Python's asyncio) need.
func socketpairRule(deny uint32) []unix.SockFilter {
	return []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_SOCKETPAIR, Jf: 6},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: argLow(1)},
		{Code: unix.BPF_ALU | unix.BPF_AND | unix.BPF_K, K: 0xf}, // the type, without SOCK_NONBLOCK/CLOEXEC
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SOCK_STREAM, Jt: 3},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SOCK_SEQPACKET, Jt: 2},
		{Code: unix.BPF_RET | unix.BPF_K, K: deny},
		{Code: unix.BPF_RET | unix.BPF_K, K: deny}, // padding, never reached
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
	}
}

// argLow is the offset of the low 32 bits of argument n in seccomp_data
// (both supported architectures are little-endian).
func argLow(n uint32) uint32 { return 16 + 8*n }

// programFile writes the filter to an unlinked file whose descriptor bwrap
// reads.
func programFile(filter []unix.SockFilter) (*os.File, error) {
	f, err := os.CreateTemp("", "ringil-seccomp-")
	if err != nil {
		return nil, errors.Wrap(err, ErrStart, "cannot create the seccomp file")
	}
	if err := os.Remove(f.Name()); err != nil {
		return nil, closeWith(errors.Wrap(err, ErrStart, "cannot unlink the seccomp file"), f)
	}
	if err := binary.Write(f, binary.NativeEndian, filter); err != nil {
		return nil, closeWith(errors.Wrap(err, ErrStart, "cannot write the seccomp filter"), f)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, closeWith(errors.Wrap(err, ErrStart, "cannot rewind the seccomp file"), f)
	}
	return f, nil
}
