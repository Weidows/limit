// main.go — limit: Windows 通用程序资源限制器 (Job Object 版 cgroup)
//
// 用法:
//
//	limit [--cpu 25%] [--mem 512M] [--io 50MB/s] [--iops 100] [--procs 8]
//	      [--affinity 0,2-5] [--prio below] [--no-kill-on-close] -- 7z x a.7z -oout\
//
// 边界(与 C 版一致, README 有详细说明):
//   - --io 只对无缓冲/写穿透 IO 精确生效; 默认缓存写(7z 等)的回写会逃逸,
//     磁盘仍会爆发。缓存写场景可用 --cpu 顶当 IO 节流阀(见 README)。
//   - --mem 是 commit(虚拟内存提交)口径硬顶; RSS 只有软限。
//   - 网络带宽/统一磁盘 IO 需要签名内核驱动, 用户态做不到。
package main

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"unsafe"
)

type limits struct {
	haveCPU     bool
	cpuRate     uint32
	mem         uintptr
	memTotal    uintptr
	ioMBps      float64
	ioIops      float64
	procs       uint32
	affinity    uintptr
	haveAff     bool
	prio        uint32
	havePrio    bool
	killOnClose bool
}

// ---------- 解析(与 C 版语义一致) ----------

func parseSize(s string) (uintptr, bool) {
	num, mult := s, 1.0
	up := strings.ToUpper(strings.TrimSpace(s))
	// 两字母后缀要先匹配(KB 先于 K)
	for _, p := range []struct {
		suf string
		m   float64
	}{
		{"KB", 1024}, {"MB", 1024 * 1024}, {"GB", 1024 * 1024 * 1024}, {"TB", 1024 * 1024 * 1024 * 1024},
		{"K", 1024}, {"M", 1024 * 1024}, {"G", 1024 * 1024 * 1024}, {"T", 1024 * 1024 * 1024 * 1024},
	} {
		if strings.HasSuffix(up, p.suf) {
			mult = p.m
			num = strings.TrimSpace(s[:len(s)-len(p.suf)])
			break
		}
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return uintptr(v * mult), true
}

// parseCPU: "25%"/"25" -> CpuRate(百分数*100, 合法区间 1..10000)
func parseCPU(s string) (uint32, bool) {
	num := strings.TrimSuffix(strings.TrimSpace(s), "%")
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	rate := math.Round(v * 100)
	if rate < 1 || rate > 10000 {
		return 0, false
	}
	return uint32(rate), true
}

// parseAffinity: "0x0F" 或 "0,2-5,7" -> 掩码
func parseAffinity(s string) (uintptr, bool) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		v, err := strconv.ParseUint(s[2:], 16, 64)
		if err != nil || v == 0 {
			return 0, false
		}
		return uintptr(v), true
	}
	var mask uintptr
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if i := strings.IndexByte(tok, '-'); i >= 0 {
			a, e1 := strconv.Atoi(tok[:i])
			b, e2 := strconv.Atoi(tok[i+1:])
			if e1 != nil || e2 != nil || a < 0 {
				return 0, false
			}
			if b < a {
				b = a
			}
			for k := a; k <= b && k < 64; k++ {
				mask |= 1 << uint(k)
			}
		} else {
			k, err := strconv.Atoi(tok)
			if err != nil || k < 0 || k >= 64 {
				return 0, false
			}
			mask |= 1 << uint(k)
		}
	}
	if mask == 0 {
		return 0, false
	}
	return mask, true
}

func quoteArg(a string) string {
	if !strings.ContainsAny(a, " \t\"") {
		return a
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range a {
		if r == '"' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

func usage(me string) {
	eprintf("limit - 通用程序资源限制器 (Job Object, 整棵进程树生效)\n\n")
	eprintf("用法: %s [选项] -- <命令> [参数...]\n\n", me)
	eprintf("选项:\n")
	eprintf("  --cpu <百分比>      CPU 硬顶, 如 25%% (1%%..100%%, 相对整机算力)\n")
	eprintf("  --mem <大小>        单进程 commit 内存硬顶, 如 512M / 4G\n")
	eprintf("  --mem-total <大小>  整棵树 commit 内存硬顶\n")
	eprintf("  --io <MB/s>         磁盘 IO 速率(读写合计; 直写精确, 缓存写不保证)\n")
	eprintf("  --iops <次数/s>     IO 每秒操作数上限(读写合计; 可与 --io 并用, 谁先撞顶谁生效)\n")
	eprintf("                      小文件场景 HDD 掉速时用它: 顺序大块走 --io, 随机小 IO 撞 --iops\n")
	eprintf("  --procs <个数>      进程数上限\n")
	eprintf("  --affinity <掩码>   CPU 亲和性, 如 0x0F 或 0,2-5\n")
	eprintf("  --prio <级别>       idle|below|normal|above|high\n")
	eprintf("  --no-kill-on-close  limit 被杀时不让子进程陪葬(默认陪葬)\n")
	eprintf("\n示例: %s --cpu 25%% --mem 2G -- 7z x big.7z -oD:\\out\n", me)
	eprintf("      %s --io 50 -- xcopy.exe S:\\ D:\\ /E\n", me)
}

// ---------- main ----------

func main() {
	os.Exit(run())
}

func run() int {
	args := os.Args[1:]
	var L limits
	L.killOnClose = true

	i := 0
optLoop:
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--":
			i++
			break optLoop
		case a == "--cpu" && i+1 < len(args):
			v, ok := parseCPU(args[i+1])
			if !ok {
				eprintf("[limit] --cpu 值无效: %s\n", args[i+1])
				return 2
			}
			L.cpuRate, L.haveCPU = v, true
			i += 2
		case a == "--mem" && i+1 < len(args):
			v, ok := parseSize(args[i+1])
			if !ok {
				eprintf("[limit] --mem 值无效: %s\n", args[i+1])
				return 2
			}
			L.mem = v
			i += 2
		case a == "--mem-total" && i+1 < len(args):
			v, ok := parseSize(args[i+1])
			if !ok {
				eprintf("[limit] --mem-total 值无效: %s\n", args[i+1])
				return 2
			}
			L.memTotal = v
			i += 2
		case a == "--io" && i+1 < len(args):
			v, err := strconv.ParseFloat(args[i+1], 64)
			if err != nil || v <= 0 {
				eprintf("[limit] --io 值无效: %s\n", args[i+1])
				return 2
			}
			L.ioMBps = v
			i += 2
		case a == "--iops" && i+1 < len(args):
			v, err := strconv.ParseFloat(args[i+1], 64)
			if err != nil || v <= 0 {
				eprintf("[limit] --iops 值无效: %s\n", args[i+1])
				return 2
			}
			L.ioIops = v
			i += 2
		case a == "--procs" && i+1 < len(args):
			v, err := strconv.Atoi(args[i+1])
			if err != nil || v < 1 {
				eprintf("[limit] --procs 值无效: %s\n", args[i+1])
				return 2
			}
			L.procs = uint32(v)
			i += 2
		case a == "--affinity" && i+1 < len(args):
			v, ok := parseAffinity(args[i+1])
			if !ok {
				eprintf("[limit] --affinity 值无效: %s\n", args[i+1])
				return 2
			}
			L.affinity, L.haveAff = v, true
			i += 2
		case a == "--prio" && i+1 < len(args):
			switch strings.ToLower(args[i+1]) {
			case "idle":
				L.prio = idlePriorityClass
			case "below":
				L.prio = belowNormalPriorityClass
			case "normal":
				L.prio = normalPriorityClass
			case "above":
				L.prio = aboveNormalPriorityClass
			case "high":
				L.prio = highPriorityClass
			default:
				eprintf("[limit] --prio 值无效: %s\n", args[i+1])
				return 2
			}
			L.havePrio = true
			i += 2
		case a == "--no-kill-on-close":
			L.killOnClose = false
			i++
		case a == "-h" || a == "--help":
			usage(os.Args[0])
			return 0
		default:
			break optLoop // 第一个非选项: 命令开始
		}
	}
	if i >= len(args) {
		usage(os.Args[0])
		return 2
	}

	// 组装子进程命令行(按需加引号)
	parts := make([]string, 0, len(args)-i)
	for _, a := range args[i:] {
		parts = append(parts, quoteArg(a))
	}
	cmdline := strings.Join(parts, " ")

	// ---- 创建 Job 并施加限制 ----
	job, err := createJobObject()
	if err != nil {
		eprintf("[limit] CreateJobObject 失败: %v\n", err)
		return 1
	}
	defer closeHandle(job)

	var eli jobObjectExtendedLimitInformation
	if L.mem != 0 {
		eli.BasicLimitInformation.LimitFlags |= jobObjectLimitProcessMemory
		eli.ProcessMemoryLimit = L.mem
	}
	if L.memTotal != 0 {
		eli.BasicLimitInformation.LimitFlags |= jobObjectLimitJobMemory
		eli.JobMemoryLimit = L.memTotal
	}
	if L.procs != 0 {
		eli.BasicLimitInformation.LimitFlags |= jobObjectLimitActiveProcess
		eli.BasicLimitInformation.ActiveProcessLimit = L.procs
	}
	if L.haveAff {
		eli.BasicLimitInformation.LimitFlags |= jobObjectLimitAffinity
		eli.BasicLimitInformation.Affinity = L.affinity
	}
	if L.killOnClose {
		eli.BasicLimitInformation.LimitFlags |= jobObjectLimitKillOnJobClose
	}
	if err := setInformationJobObject(job, jobObjectInfoClassExtendedLimit,
		unsafe.Pointer(&eli), unsafe.Sizeof(eli)); err != nil {
		eprintf("[limit] 设置扩展限制失败: %v\n", err)
		return 1
	}

	if L.haveCPU {
		c := jobObjectCpuRateControlInformation{
			ControlFlags: jobObjectCpuRateControlEnable | jobObjectCpuRateControlHardCap,
			CpuRate:      L.cpuRate,
		}
		if err := setInformationJobObject(job, jobObjectInfoClassCpuRateControl,
			unsafe.Pointer(&c), unsafe.Sizeof(c)); err != nil {
			eprintf("[limit] 设置 CPU 硬顶失败: %v\n", err)
			return 1
		}
	}

	if L.ioMBps > 0 || L.ioIops > 0 {
		bw := int64(L.ioMBps * 1024 * 1024)
		iops := int64(math.Round(L.ioIops))
		if err := setIoRateControl(job, bw, iops); err != nil {
			eprintf("[limit] 设置 IO 限速失败: %v\n", err)
			return 1
		}
		eprintf("[limit] 注意: IO 限速对缓存写不保证(回写会爆发), 直写/写穿透 IO 才精确生效\n")
	}

	// ---- 挂起启动 -> 入 Job -> 恢复 ----
	pi, err := createProcessSuspended(cmdline)
	if err != nil {
		eprintf("[limit] 启动失败(%v): %s\n", err, cmdline)
		return 1
	}
	if err := assignProcessToJobObject(job, pi.HProcess); err != nil {
		eprintf("[limit] 加入 Job 失败: %v (进程可能已在别的 Job/容器里)\n", err)
		terminateProcess(pi.HProcess, 1)
		closeHandle(pi.HProcess)
		closeHandle(pi.HThread)
		return 1
	}
	if L.havePrio {
		setPriorityClass(pi.HProcess, L.prio)
	}
	if err := resumeThread(pi.HThread); err != nil {
		eprintf("[limit] 恢复子进程失败: %v\n", err)
	}

	// ---- 摘要行 ----
	summary := fmt.Sprintf("[limit] 运行中 (pid %d):", pi.DwProcessId)
	if L.haveCPU {
		summary += fmt.Sprintf(" cpu<=%.2f%%", float64(L.cpuRate)/100)
	}
	if L.mem != 0 {
		summary += fmt.Sprintf(" mem<=%.0fMB", float64(L.mem)/1e6)
	}
	if L.memTotal != 0 {
		summary += fmt.Sprintf(" mem_total<=%.0fMB", float64(L.memTotal)/1e6)
	}
	if L.ioMBps > 0 {
		summary += fmt.Sprintf(" io<=%.0fMB/s", L.ioMBps)
	}
	if L.ioIops > 0 {
		summary += fmt.Sprintf(" iops<=%.0f/s", L.ioIops)
	}
	if L.procs != 0 {
		summary += fmt.Sprintf(" procs<=%d", L.procs)
	}
	if L.haveAff {
		summary += fmt.Sprintf(" affinity=%#x", L.affinity)
	}
	if L.havePrio {
		summary += fmt.Sprintf(" prio=%#x", L.prio)
	}
	eprintf("%s\n", summary)

	if err := waitForSingleObject(pi.HProcess); err != nil {
		eprintf("[limit] 等待子进程失败: %v\n", err)
	}
	code, err := getExitCodeProcess(pi.HProcess)
	if err != nil {
		eprintf("[limit] 获取退出码失败: %v\n", err)
	}
	closeHandle(pi.HProcess)
	closeHandle(pi.HThread)

	// ---- 退出统计 ----
	var acc jobObjectBasicAccountingInformation
	if err := queryInformationJobObject(job, jobObjectInfoClassBasicAccounting,
		unsafe.Pointer(&acc), unsafe.Sizeof(acc)); err == nil {
		eprintf("[limit] 退出码 %d | CPU 时间 user %.2fs + kernel %.2fs\n",
			code, float64(acc.TotalUserTime)/1e7, float64(acc.TotalKernelTime)/1e7)
	}
	var q jobObjectExtendedLimitInformation
	if err := queryInformationJobObject(job, jobObjectInfoClassExtendedLimit,
		unsafe.Pointer(&q), unsafe.Sizeof(q)); err == nil {
		eprintf("[limit] 峰值 commit: 单进程 %.0f MB / 整棵树 %.0f MB\n",
			float64(q.PeakProcessMemoryUsed)/1e6, float64(q.PeakJobMemoryUsed)/1e6)
	}
	return int(code)
}
