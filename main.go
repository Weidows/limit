// main.go — limit: 通用程序资源限制器 (Windows: Job Object 版 cgroup)
//
// 用法:
//
//	limit [--cpu 25%] [--mem 512M] [--io 50MB/s] [--iops 200 D: E:] [--timeout 2h]
//	      [--procs 8]
//	      [--affinity 0,2-5] [--prio below] [--no-kill-on-close] -- 7z x a.7z -oout\
//
// 平台: 限制机制是 Windows Job Object(整棵进程树生效)。本程序也提供
// linux/macOS 二进制, 但执行到限制那一步会明确报错退出(见 run_other.go)——
// 不做静默放行, 也不假装支持; cgroup/rlimit 移植见 README「平台」一节。
//
// 边界(与 C 版一致, README 有详细说明):
//   - --io 只对无缓冲/写穿透 IO 精确生效; 默认缓存写(7z 等)的回写会逃逸,
//     磁盘仍会爆发。缓存写场景可用 --cpu 顶当 IO 节流阀(见 README)。
//   - --mem 是 commit(虚拟内存提交)口径硬顶; RSS 只有软限。
//   - 网络带宽/统一磁盘 IO 需要签名内核驱动, 用户态做不到。
//   - --iops 面向 HDD 保护磁头: 限的是「物理盘设备层事务数/秒」(其他程序也计入),
//     大文件事务少几乎不受影响, 大量小文件被削得最狠。冻结不分盘: 任一盘超标时
//     整棵树(含另一盘的 IO)一起停。目标值按「能接受的顺序吞吐 ÷ 平均 IO 大小」倒推。
//   - --timeout 按墙钟计时(限速冻结的时间也算), 到期 TerminateJobObject 整棵树,
//     退出码 124(与 coreutils timeout 一致)。
package main

import (
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type limits struct {
	haveCPU     bool
	cpuRate     uint32
	mem         uintptr
	memTotal    uintptr
	ioMBps      float64
	ioIops      float64
	iopsDrives  []string // 只盯这些盘(小写盘符); 空 = 所有固定卷
	timeout     time.Duration
	haveTmout   bool
	procs       uint32
	affinity    uintptr
	haveAff     bool
	prio        string
	havePrio    bool
	ioPrio      string
	haveIoPrio  bool
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

// parseDuration 解析 --timeout 的时长: 90s / 10m / 1h / 1h30m / 500ms, 纯数字按秒。
func parseDuration(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		if v <= 0 {
			return 0, false
		}
		return time.Duration(v * float64(time.Second)), true
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// parseDriveList 吃掉参数流里连续的盘符 token(形如 D: / D / all), 返回小写盘符和消耗个数。
// 多盘监控靠它: --iops 200 D: E: 表示「这两盘任一超 200 事务/s 就压」。
func parseDriveList(args []string) ([]string, int) {
	var out []string
	n := 0
	for _, a := range args {
		t := strings.TrimSpace(a)
		if strings.EqualFold(t, "all") {
			out = append(out, "all")
			n++
			continue
		}
		d := strings.TrimSuffix(t, ":")
		if len(d) != 1 || !((d[0] >= 'a' && d[0] <= 'z') || (d[0] >= 'A' && d[0] <= 'Z')) {
			break
		}
		out = append(out, strings.ToLower(d))
		n++
	}
	return out, n
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
	eprintf("limit - 通用程序资源限制器 (Windows: Job Object, 整棵进程树生效)\n\n")
	eprintf("用法: %s [选项] -- <命令> [参数...]\n\n", me)
	eprintf("选项:\n")
	eprintf("  --cpu <百分比>      CPU 硬顶, 如 25%% (1%%..100%%, 相对整机算力)\n")
	eprintf("  --mem <大小>        单进程 commit 内存硬顶, 如 512M / 4G\n")
	eprintf("  --mem-total <大小>  整棵树 commit 内存硬顶\n")
	eprintf("  --io <MB/s>         磁盘 IO 速率(读写合计; 无缓冲/直写精确生效, 缓冲写穿透)\n")
	eprintf("                      缓冲写程序(robocopy/xcopy 默认)的数据写不受限, robocopy 加 /J 走无缓冲\n")
	eprintf("  --iops <次数/s> [盘...]  限「寻道频率」= 物理盘设备层事务数/秒(免管理员)\n")
	eprintf("                      不给盘符 = 所有固定卷; 也可 --iops 200 D: E: 只盯这两盘\n")
	eprintf("                      任一被盯的盘超目标就冻结整棵树; 同一物理盘只算一次\n")
	eprintf("                      顺序大文件事务少 → 几乎无感; 大量小文件事务多 → 压得最狠\n")
	eprintf("                      注意: 靠占空比挂起/恢复进程实现, 极小概率让目标卡在内核等待(实测出现过);\n")
	eprintf("                      大文件负载本来就不需要它, 只想让路请用 --io-prio verylow(不挂起)\n")
	eprintf("                      吞吐天花板 ≈ 目标 × 平均 IO 大小(200/s: 4KB≈0.8MB/s, 1MB≈200MB/s)\n")
	eprintf("  --procs <个数>      进程数上限\n")
	eprintf("  --affinity <掩码>   CPU 亲和性, 如 0x0F 或 0,2-5\n")
	eprintf("  --prio <级别>       idle|below|normal|above|high\n")
	eprintf("  --io-prio <级别>    IO 优先级 verylow|low|normal|high —— 让路给其他程序, 不挂起进程(最安全)\n")
	eprintf("  --timeout <时长>    超时后终止整棵树并返回 124, 如 90s / 10m / 1h(纯数字=秒)\n")
	eprintf("  --no-kill-on-close  limit 被杀时不让子进程陪葬(默认陪葬)\n")
	eprintf("\n示例: %s --cpu 25%% --mem 2G -- 7z x big.7z -oD:\\out\n", me)
	eprintf("      %s --io 50 -- robocopy S:\\ D:\\ /E /J\n", me)
	eprintf("      %s --iops 200 D: E: -- robocopy S:\\ D:\\backup /E   (任一盘超 200 事务/s 就压)\n", me)
	eprintf("      %s --timeout 2h -- robocopy S:\\ D:\\backup /E /J    (2 小时后强制收工)\n", me)
	eprintf("      %s --io-prio verylow -- robocopy S:\\ D:\\backup /E /J (后台慢慢拷, 不抢前台 IO)\n", me)
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
			// 可选: 跟一串盘符, 只盯这些盘; 不写就盯所有固定卷
			if drives, n := parseDriveList(args[i+2:]); n > 0 {
				L.iopsDrives = append(L.iopsDrives, drives...)
				i += n
			}
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
			p := strings.ToLower(args[i+1])
			switch p {
			case "idle", "below", "normal", "above", "high":
				L.prio = p
			default:
				eprintf("[limit] --prio 值无效: %s\n", args[i+1])
				return 2
			}
			L.havePrio = true
			i += 2
		case a == "--timeout" && i+1 < len(args):
			v, ok := parseDuration(args[i+1])
			if !ok {
				eprintf("[limit] --timeout 值无效: %s (示例: 90s / 10m / 1h / 3600)\n", args[i+1])
				return 2
			}
			L.timeout, L.haveTmout = v, true
			i += 2
		case a == "--io-prio" && i+1 < len(args):
			p := strings.ToLower(args[i+1])
			switch p {
			case "verylow", "low", "normal", "high":
				L.ioPrio, L.haveIoPrio = p, true
			default:
				eprintf("[limit] --io-prio 值无效: %s (verylow|low|normal|high)\n", args[i+1])
				return 2
			}
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

	// 非 Windows: 选项照常解析(--help 可用), 到执行这一步明确报错。
	if runtime.GOOS != "windows" {
		eprintf("[limit] 此平台 (%s/%s) 不支持: 限制机制是 Windows Job Object, 暂无 cgroup/rlimit 移植。\n", runtime.GOOS, runtime.GOARCH)
		eprintf("[limit] 未执行目标命令(不静默放行): %s\n", strings.Join(args[i:], " "))
		return 1
	}
	return runWindows(&L, args[i:])
}
