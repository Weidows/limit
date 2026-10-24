// run_other.go — 非 Windows 平台的执行桩

//go:build !windows

package main

import "runtime"

// runWindows 在非 Windows 平台不应被调用: run() 在到达这里之前已明确报错退出。
// 保留一个编译期可见的兜底定义, 保证 linux/darwin 等平台可正常编译出二进制。
func runWindows(L *limits, cmdArgs []string) int {
	eprintf("[limit] 未实现: 此平台 (%s) 不支持 Job Object 限制。\n", runtime.GOOS)
	return 1
}
