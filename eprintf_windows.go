// eprintf_windows.go — 控制台输出(中文不乱码, 仅 Windows)

//go:build windows

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	procGetStdHandle   = kernel32.NewProc("GetStdHandle")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procWriteConsoleW  = kernel32.NewProc("WriteConsoleW")

	stdErrorHandle = ^uintptr(11) // (DWORD)-12, GetStdHandle 参数
)

// eprintf 写到 stderr: 是控制台时走 WriteConsoleW(UTF-16), 被重定向时写 UTF-8 字节。
// C 版靠 fwprintf 的代码页转换; Go 若直接写 UTF-8 到 GBK 控制台会乱码, 故需此函数。
func eprintf(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	h, _, _ := procGetStdHandle.Call(stdErrorHandle)
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(h, uintptr(unsafe.Pointer(&mode))); r != 0 && h != 0 {
		u16, err := syscall.UTF16FromString(s)
		if err == nil {
			var written uint32
			procWriteConsoleW.Call(h, uintptr(unsafe.Pointer(&u16[0])),
				uintptr(len(u16)), uintptr(unsafe.Pointer(&written)), 0)
			return
		}
	}
	fmt.Fprint(os.Stderr, s)
}
