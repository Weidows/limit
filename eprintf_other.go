// eprintf_other.go — 非 Windows 平台的 stderr 输出(UTF-8 直写)

//go:build !windows

package main

import (
	"fmt"
	"os"
)

func eprintf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
}
