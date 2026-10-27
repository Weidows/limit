// jobs.go — Win32 Job Object 绑定(纯标准库 + LazyDLL, 零外部依赖)
//
// 说明:
//   - Go 的 syscall.NewLazyDLL(...).NewProc(...) 内部就是 LoadLibrary/GetProcAddress,
//     C 版因 mingw 导入库缺符号而手写 GetProcAddress 的绕法在这里不需要。
//   - 所有结构体布局与 legacy/layout.c (gcc x86_64-w64-mingw32) 实测逐一核对,
//     回归测试见 jobs_test.go (TestStructLayouts)。
package main

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var kernel32 = syscall.NewLazyDLL("kernel32.dll")

var (
	procCreateJobObjectW          = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject   = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject  = kernel32.NewProc("AssignProcessToJobObject")
	procQueryInformationJobObject = kernel32.NewProc("QueryInformationJobObject")
	procCreateProcessW            = kernel32.NewProc("CreateProcessW")
	procResumeThread              = kernel32.NewProc("ResumeThread")
	procTerminateProcess          = kernel32.NewProc("TerminateProcess")
	procWaitForSingleObject       = kernel32.NewProc("WaitForSingleObject")
	procGetExitCodeProcess        = kernel32.NewProc("GetExitCodeProcess")
	procSetPriorityClass          = kernel32.NewProc("SetPriorityClass")
	procCloseHandle               = kernel32.NewProc("CloseHandle")
	procOpenProcess               = kernel32.NewProc("OpenProcess")
	procGetProcessIoCounters      = kernel32.NewProc("GetProcessIoCounters")

	// Job Object I/O rate control: 运行时解析, Win7 及更早不存在
	procSetIoRateControlInformationJobObject = kernel32.NewProc("SetIoRateControlInformationJobObject")
	procTerminateJobObject                   = kernel32.NewProc("TerminateJobObject")
	procSetConsoleCtrlHandler                = kernel32.NewProc("SetConsoleCtrlHandler")
)

// JobObjectInformationClass(用到的; 不能与结构体同名, Go 单命名空间)
const (
	jobObjectInfoClassBasicAccounting    = 1
	jobObjectInfoClassBasicProcessIdList = 3
	jobObjectInfoClassExtendedLimit      = 9
	jobObjectInfoClassCpuRateControl     = 15
)

// 限制位与开关
const (
	jobObjectLimitActiveProcess    = 0x00000008
	jobObjectLimitAffinity         = 0x00000010
	jobObjectLimitProcessMemory    = 0x00000100
	jobObjectLimitJobMemory        = 0x00000200
	jobObjectLimitKillOnJobClose   = 0x00002000
	jobObjectCpuRateControlEnable  = 0x1
	jobObjectCpuRateControlHardCap = 0x4
	jobObjectIoRateControlEnable   = 0x1
)

// 进程创建/等待
const (
	createSuspended = 0x00000004
	infinite        = 0xFFFFFFFF
)

// 优先级类
const (
	idlePriorityClass        = 0x00000040
	belowNormalPriorityClass = 0x00004000
	normalPriorityClass      = 0x00000020
	aboveNormalPriorityClass = 0x00008000
	highPriorityClass        = 0x00000080
)

// ---------- 结构体(布局与 Win32 SDK 一致, 由 jobs_test.go 断言) ----------

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type jobObjectCpuRateControlInformation struct {
	ControlFlags uint32
	CpuRate      uint32
}

type jobObjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// JOBOBJECT_IO_RATE_CONTROL_INFORMATION
// 注意: 文档称 VolumeName=NULL 时按盘独立配额, 实测(双盘并行拷贝)为整 Job 聚合, 以实测为准
type ioRateControlInformation struct {
	MaxIops         int64
	MaxBandwidth    int64
	ReservationIops int64
	VolumeName      *uint16
	BaseIoSize      uint32
	ControlFlags    uint32
}

type startupInfo struct {
	Cb              uint32
	LpReserved      *uint16
	LpDesktop       *uint16
	LpTitle         *uint16
	DwX             uint32
	DwY             uint32
	DwXSize         uint32
	DwYSize         uint32
	DwXCountChars   uint32
	DwYCountChars   uint32
	DwFillAttribute uint32
	DwFlags         uint32
	WShowWindow     uint16
	CbReserved2     uint16
	LpReserved2     *byte
	HStdInput       uintptr
	HStdOutput      uintptr
	HStdError       uintptr
}

type processInformation struct {
	HProcess    syscall.Handle
	HThread     syscall.Handle
	DwProcessId uint32
	DwThreadId  uint32
}

type processEntry32 struct {
	Size            uint32
	CntUsage        uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	CntThreads      uint32
	ParentProcessID uint32
	PriClassBase    int32
	Flags           uint32
	ExeFile         [260]uint16
}

// ---------- Win32 封装 ----------

func createJobObject() (syscall.Handle, error) {
	r1, _, err := procCreateJobObjectW.Call(0, 0)
	if r1 == 0 {
		return 0, err
	}
	return syscall.Handle(r1), nil
}

func setInformationJobObject(job syscall.Handle, class uint32, info unsafe.Pointer, size uintptr) error {
	r1, _, err := procSetInformationJobObject.Call(uintptr(job), uintptr(class), uintptr(info), size)
	if r1 == 0 {
		return err
	}
	return nil
}

func assignProcessToJobObject(job, process syscall.Handle) error {
	r1, _, err := procAssignProcessToJobObject.Call(uintptr(job), uintptr(process))
	if r1 == 0 {
		return err
	}
	return nil
}

func queryInformationJobObject(job syscall.Handle, class uint32, info unsafe.Pointer, size uintptr) error {
	r1, _, err := procQueryInformationJobObject.Call(uintptr(job), uintptr(class), uintptr(info), size, 0)
	if r1 == 0 {
		return err
	}
	return nil
}

// createProcessSuspended 以挂起状态启动(避免入 Job 前那几毫秒不受限)
func createProcessSuspended(cmdline string) (*processInformation, error) {
	var si startupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi processInformation
	p, err := syscall.UTF16PtrFromString(cmdline)
	if err != nil {
		return nil, err
	}
	r1, _, callErr := procCreateProcessW.Call(
		0,                          // lpApplicationName
		uintptr(unsafe.Pointer(p)), // lpCommandLine(可写)
		0, 0, 0,                    // 安全属性/继承
		createSuspended, // 挂起创建
		0, 0,            // 环境/工作目录
		uintptr(unsafe.Pointer(&si)), // STARTUPINFO
		uintptr(unsafe.Pointer(&pi)), // PROCESS_INFORMATION
	)
	if r1 == 0 {
		return nil, callErr
	}
	return &pi, nil
}

func resumeThread(thread syscall.Handle) error {
	r1, _, err := procResumeThread.Call(uintptr(thread))
	if r1 == 0xFFFFFFFF {
		return err
	}
	return nil
}

func terminateProcess(process syscall.Handle, code uint32) error {
	r1, _, err := procTerminateProcess.Call(uintptr(process), uintptr(code))
	if r1 == 0 {
		return err
	}
	return nil
}

func waitForSingleObject(handle syscall.Handle) error {
	r1, _, err := procWaitForSingleObject.Call(uintptr(handle), infinite)
	if r1 != 0 {
		return err
	}
	return nil
}

func getExitCodeProcess(process syscall.Handle) (uint32, error) {
	var code uint32
	r1, _, err := procGetExitCodeProcess.Call(uintptr(process), uintptr(unsafe.Pointer(&code)))
	if r1 == 0 {
		return 1, err
	}
	return code, nil
}

// consoleCleanup 由 runWindows 注册: 收到 Ctrl+C / 关窗时先把被挂起的子进程恢复,
// 再按 killOnClose 终止整棵树。
//
// 为什么必须显式做: NtSuspendProcess 的挂起计数不会因为父进程退出而回滚,
// 而"先挂起、后终止"在 IO 密集进程上还可能留下卡在内核等待里的僵尸。
var consoleCleanup func()

func consoleCtrlHandler(ctrlType uint32) uintptr {
	if consoleCleanup != nil {
		consoleCleanup()
	}
	os.Exit(130)
	return 0
}

func installConsoleCleanup(fn func()) {
	consoleCleanup = fn
	cb := syscall.NewCallback(consoleCtrlHandler)
	procSetConsoleCtrlHandler.Call(cb, 1)
}

// ioPriorityClass 把 --io-prio 字符串映射成 IO_PRIORITY_HINT(0=VeryLow .. 3=High)。
func ioPriorityClass(p string) uint32 {
	switch p {
	case "verylow":
		return 0
	case "low":
		return 1
	case "normal":
		return 2
	default:
		return 3
	}
}

// setIoPriority 设置进程 IO 优先级(IO_PRIORITY_HINT)。这是**不给进程添风险**的降速/让路手段:
// 低 IO 优先级的请求会被存储栈排到别人后面, 不会像冻结那样把线程挂起。
// 作用于已启动的进程; 其后新建的线程/子进程不保证继承。
func setIoPriority(process syscall.Handle, prio uint32) error {
	const processIoPriority = 33 // PROCESSINFOCLASS.ProcessIoPriority
	v := prio
	r1, _, err := procNtSetInformationProcess.Call(
		uintptr(process), processIoPriority, uintptr(unsafe.Pointer(&v)), unsafe.Sizeof(v))
	if int32(r1) < 0 {
		return err
	}
	return nil
}

// terminateJobObject 终止 Job 内全部进程(超时收工用)。
func terminateJobObject(job syscall.Handle, code uint32) error {
	r1, _, err := procTerminateJobObject.Call(uintptr(job), uintptr(code))
	if r1 == 0 {
		return err
	}
	return nil
}

func setPriorityClass(process syscall.Handle, class uint32) error {
	r1, _, err := procSetPriorityClass.Call(uintptr(process), uintptr(class))
	if r1 == 0 {
		return err
	}
	return nil
}

func closeHandle(handle syscall.Handle) error {
	r1, _, err := procCloseHandle.Call(uintptr(handle))
	if r1 == 0 {
		return err
	}
	return nil
}

// 只设带宽顶(--io)。IOPS 维度不再走这条内核 API: 它按 8KB 归一化计数、和寻道无关,
// --iops 已改为设备层事务率 + 占空比冻结(见下面的 iopsLimiter)。传 0 表示不设该维度。
func setIoRateControl(job syscall.Handle, maxBandwidthBytes, maxIops int64) error {
	// Find() 内部即 GetProcAddress; 不存在时明确报错, 不静默
	if err := procSetIoRateControlInformationJobObject.Find(); err != nil {
		return fmt.Errorf("IO 限速 API 不可用(需 Win8+): %w", err)
	}
	io := ioRateControlInformation{
		MaxBandwidth: maxBandwidthBytes,
		MaxIops:      maxIops,
		ControlFlags: jobObjectIoRateControlEnable,
	}
	r1, _, callErr := procSetIoRateControlInformationJobObject.Call(
		uintptr(job), uintptr(unsafe.Pointer(&io)))
	if r1 == 0 {
		return callErr
	}
	return nil
}

// ---------- 设备层速率限制(--iops: HDD 寻道频率) ----------
//
// 目标只有一个: 保护 HDD 磁头。设备层 `Disk Transfers/sec` 是寻道频率的可用代理:
//
//	顺序 1MB : 1MB 只算 1 次事务(100MB/s ≈ 100 事务/s, 几乎不寻道)
//	随机 4KB : 每 4KB 就是 1 次寻道, 同样字节量的事务数是顺序的 256 倍
//
// 所以「限 事务/s」天然对大文件松、对小文件狠: 吞吐天花板 ≈ 目标 × 平均 IO 大小。
//
// 实现要点:
//  1. PDH 读 `\PhysicalDisk(*)\Disk Transfers/sec` 的**原始累积值**(免管理员),
//     并按物理盘实例名匹配盘符; C:/D: 落在同一块物理盘时只算一次, 不重复计数。
//  2. 每块盘独立记账, **任意一盘超目标**就冻结整棵进程树 —— 这就是
//     「src / dst 谁超标谁触发」的语义, 不需要用户手工指定哪盘是瓶颈。
//  3. 细粒度还债: 20ms 采样, 超额就按 欠账/目标 的时长分块冻结(5ms..250ms),
//     每冻完一块立刻复采样。可达任意占空比 —— 旧实现「单 tick 最多冻 90% + 100ms
//     采样」把地板钉死在 1/10 削减(目标 50 实测只能压到 735/s)。
//  4. 冻结用缓存的进程句柄(300ms 刷新), 不再每次冻结/恢复都全量枚举进程表。
//
// 已知边界:
//   - 计数器是**整盘**口径: 同盘其他程序也计入。背景负载本身高于目标时, 本程序会被
//     自己的控制律饿住(占空比拉满, 打印一次告警) —— 语义上仍是「保整盘交互」。
//   - 冻结作用于整棵树、不分盘: 一盘超标时另一盘的 IO 也一起停(占空比式限流必然会这样)。
//   - 逐 IO 偏移量判定(真寻道)要 ETW Kernel-File, 需管理员, 故不采用。
const (
	th32csSnapProcess    = 0x00000002
	processQueryInfo     = 0x0400 // IsProcessInJob 需要 QUERY_INFORMATION
	processSuspendResume = 0x0800

	iopsTick       = 20 * time.Millisecond  // 采样周期
	iopsChunkMin   = 50 * time.Millisecond  // 单次冻结下限(别调小: 切换太勤容易撞上内核等待)
	iopsChunkMax   = 250 * time.Millisecond // 单次冻结上限
	iopsTickBudget = 900 * time.Millisecond // 单个采样周期内最多冻结多久
	iopsDebtCap    = 2.0                    // 欠账上限(目标×秒), 防止长期冻死
	iopsPidRefresh = 300 * time.Millisecond // 刷新 Job 内进程句柄的间隔
)

var (
	procCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW          = kernel32.NewProc("Process32FirstW")
	procProcess32NextW           = kernel32.NewProc("Process32NextW")
	procIsProcessInJob           = kernel32.NewProc("IsProcessInJob")

	ntdll                       = syscall.NewLazyDLL("ntdll.dll")
	procNtSuspendProcess        = ntdll.NewProc("NtSuspendProcess")
	procNtResumeProcess         = ntdll.NewProc("NtResumeProcess")
	procNtSetInformationProcess = ntdll.NewProc("NtSetInformationProcess")

	pdhDLL              = syscall.NewLazyDLL("pdh.dll")
	procPdhOpenQuery    = pdhDLL.NewProc("PdhOpenQueryW")
	procPdhAddCounter   = pdhDLL.NewProc("PdhAddEnglishCounterW")
	procPdhCollectQuery = pdhDLL.NewProc("PdhCollectQueryData")
	procPdhGetRawArray  = pdhDLL.NewProc("PdhGetRawCounterArrayW")
	procPdhCloseQuery   = pdhDLL.NewProc("PdhCloseQuery")
)

// PDH_RAW_COUNTER
type pdhRawCounter struct {
	CStatus     uint32
	TimeStamp   int64
	FirstValue  int64 // 累积计数
	SecondValue int64
	MultiCount  uint32
	_           uint32
}

// PDH_RAW_COUNTER_ITEM_W: 通配符计数器会用数组返回每个实例(物理盘)的值
type pdhRawCounterItem struct {
	Name *uint16
	Raw  pdhRawCounter
}

// diskWatch 一块被监控的物理盘(设备层实例), name 形如 "1 D:" / "2 E: F:"。
type diskWatch struct {
	name    string
	prev    uint64  // 上次读到的累积事务数
	debt    float64 // 欠账(事务数), 只记正超额
	total   float64 // 全程观测到的事务数
	max     float64 // 峰值速率
	samples int
}

// iopsLimiter 监视若干物理盘, 任一盘设备层事务速率超目标就压整棵进程树。
type iopsLimiter struct {
	job     syscall.Handle
	target  float64
	query   uintptr
	count   uintptr
	letters []string // 用户指定的盘符小写(或 "all")
	watches []*diskWatch
	frz     *freezer

	start       time.Time
	frozenTotal time.Duration
	warned      bool
	warnMu      sync.Mutex

	stopCh chan struct{}
	done   chan struct{}
}

// iopsReport 结束时的实测数据(整盘口径: 同盘其他程序也计入)。
type iopsReport struct {
	elapsed float64
	frozen  float64
	rows    []iopsRow
}

type iopsRow struct {
	name string
	avg  float64
	max  float64
	ops  float64
}

// freezer 缓存 Job 内进程句柄, 冻结/恢复只做 NtSuspendProcess/NtResumeProcess。
// 旧实现对每次冻结/恢复都做一次全进程表快照 + IsProcessInJob, 100 轮/秒时开销爆炸,
// 这也是当初不敢把采样周期调细的原因之一。
type freezer struct {
	job         syscall.Handle
	handles     []syscall.Handle
	lastRefresh time.Time
	frozen      bool
}

func newFreezer(job syscall.Handle) *freezer {
	f := &freezer{job: job}
	f.refresh()
	return f
}

func (f *freezer) refresh() {
	for _, h := range f.handles {
		closeHandle(h)
	}
	f.handles = f.handles[:0]
	for _, pid := range pidsInJob(f.job) {
		h, _, _ := procOpenProcess.Call(processSuspendResume, 0, uintptr(pid))
		if h != 0 {
			f.handles = append(f.handles, syscall.Handle(h))
		}
	}
	f.lastRefresh = time.Now()
}

// set 返回 false 表示手上没有可挂起的进程句柄(目标已退出), 调用方别把它当「已冻结」记账。
func (f *freezer) set(on bool) bool {
	if on == f.frozen {
		return true
	}
	if len(f.handles) == 0 || time.Since(f.lastRefresh) > iopsPidRefresh {
		f.refresh()
	}
	if len(f.handles) == 0 {
		f.frozen = on
		return false
	}
	for _, h := range f.handles {
		if on {
			procNtSuspendProcess.Call(uintptr(h))
		} else {
			procNtResumeProcess.Call(uintptr(h))
		}
	}
	f.frozen = on
	return true
}

func (f *freezer) close() {
	f.set(false)
	for _, h := range f.handles {
		closeHandle(h)
	}
	f.handles = nil
}

// utf16PtrToString 读 PDH 缓冲区里的实例名(不保证以固定长度结尾)。
func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	n := 0
	for ptr := unsafe.Pointer(p); *(*uint16)(ptr) != 0; ptr = unsafe.Add(ptr, 2) {
		n++
	}
	return syscall.UTF16ToString(unsafe.Slice(p, n))
}

// newIopsLimiter 建立 PDH 查询并解析出要监控的物理盘实例。
func newIopsLimiter(job syscall.Handle, target float64, letters []string) (*iopsLimiter, error) {
	if err := procPdhGetRawArray.Find(); err != nil {
		return nil, fmt.Errorf("PDH 数组接口不可用: %w", err)
	}
	for _, l := range letters {
		if l == "all" {
			continue
		}
		// PDH 通配符路径对不存在的盘不报错、只会一直返回 0(限速静默失效) —— 先拦下
		if _, e := os.Stat(l + ":\\"); e != nil {
			return nil, fmt.Errorf("盘 %s: 不存在或不可访问", l)
		}
	}
	s := &iopsLimiter{
		job:     job,
		target:  target,
		letters: letters,
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	var q uintptr
	if r, _, _ := procPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&q))); r != 0 {
		return nil, fmt.Errorf("PdhOpenQuery 失败 (0x%x)", uint32(r))
	}
	s.query = q
	path := `\PhysicalDisk(*)\Disk Transfers/sec`
	p16, e := syscall.UTF16PtrFromString(path)
	if e != nil {
		procPdhCloseQuery.Call(q)
		return nil, e
	}
	var c uintptr
	if r, _, _ := procPdhAddCounter.Call(q, uintptr(unsafe.Pointer(p16)), 0, uintptr(unsafe.Pointer(&c))); r != 0 {
		procPdhCloseQuery.Call(q)
		return nil, fmt.Errorf("找不到 PhysicalDisk 计数器 (0x%x)", uint32(r))
	}
	s.count = c
	procPdhCollectQuery.Call(q) // 建基线
	if err := s.resolve(); err != nil {
		procPdhCloseQuery.Call(q)
		return nil, err
	}
	return s, nil
}

// readAll 读全部物理盘实例的累积事务数。
func (s *iopsLimiter) readAll() (map[string]uint64, bool) {
	if r, _, _ := procPdhCollectQuery.Call(s.query); r != 0 {
		return nil, false
	}
	var size, count uint32
	procPdhGetRawArray.Call(s.count,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
	if size == 0 || count == 0 {
		return nil, false
	}
	buf := make([]byte, size)
	if r, _, _ := procPdhGetRawArray.Call(s.count,
		uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)),
		uintptr(unsafe.Pointer(&buf[0]))); r != 0 {
		return nil, false
	}
	out := make(map[string]uint64, count)
	items := unsafe.Slice((*pdhRawCounterItem)(unsafe.Pointer(&buf[0])), int(count))
	for i := range items {
		if items[i].Raw.CStatus != 0 {
			continue
		}
		out[utf16PtrToString(items[i].Name)] = uint64(items[i].Raw.FirstValue)
	}
	runtime.KeepAlive(buf)
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// resolve 把盘符映射成物理盘实例; 同一物理盘被多个盘符命中只登记一次。
func (s *iopsLimiter) resolve() error {
	cur, ok := s.readAll()
	if !ok {
		return fmt.Errorf("读不到 PhysicalDisk 计数器")
	}
	names := make([]string, 0, len(cur))
	for n := range cur {
		names = append(names, n)
	}
	sort.Strings(names)
	seen := make(map[string]bool, len(names))
	for _, want := range s.letters {
		hit := 0
		for _, n := range names {
			if n == "_Total" || !strings.Contains(n, ":") {
				continue // 无盘符的设备(如 Optane 缓存盘)不参与
			}
			if want != "all" && !strings.Contains(strings.ToLower(n), " "+want+":") {
				continue
			}
			hit++
			if seen[n] {
				continue
			}
			seen[n] = true
			s.watches = append(s.watches, &diskWatch{name: n})
		}
		if hit == 0 {
			return fmt.Errorf("盘 %s: 找不到设备层 IO 计数器(该盘可能不是固定磁盘)", want)
		}
	}
	return nil
}

// run 在子进程存活期间持续限速; 返回的 stop 给出实测数据。
func (s *iopsLimiter) run() func() *iopsReport {
	s.frz = newFreezer(s.job)
	s.start = time.Now()
	go func() {
		defer close(s.done)
		prev, ok := s.readAll()
		if !ok {
			return
		}
		for _, w := range s.watches {
			w.prev = prev[w.name]
		}
		prevT := time.Now()
		tickStart := prevT
		var frozenInTick time.Duration
		frozenRound := false // 上一轮是否以「冻结」收尾
		budgetHits := 0      // 连续多少个采样周期把冻结预算用满
		for {
			select {
			case <-s.stopCh:
				s.frz.set(false)
				return
			default:
			}
			cur, ok := s.readAll()
			now := time.Now()
			dt := now.Sub(prevT).Seconds()
			if !ok || dt < 0.001 {
				time.Sleep(time.Millisecond)
				continue
			}
			prevT = now
			worst := 0.0
			for _, w := range s.watches {
				c, has := cur[w.name]
				if !has {
					continue
				}
				if c < w.prev { // 计数器重置/回绕: 重设基线
					w.prev = c
					continue
				}
				delta := float64(c - w.prev)
				w.prev = c
				rate := delta / dt
				w.total += delta
				w.samples++
				if rate > w.max {
					w.max = rate
				}
				if frozenRound {
					// 冻结期间观测到的增量是「在途 IO 完成 + 别的程序」, 不是我们新发出的
					// 请求。计费会把它算成我们的欠账, 于是 debt 永远还不清 → 占空比拉满、
					// 目标 100/s 时自然率只有 83/s 也照冻(实测 80%+ 占空比、整盘只剩 19/s)。
					// 这一段只按目标预算偿还, 不累加 delta。
					w.debt -= s.target * dt
				} else {
					w.debt += delta - s.target*dt
				}
				if w.debt < 0 {
					w.debt = 0
				} else if cap := s.target * iopsDebtCap; w.debt > cap {
					w.debt = cap
				}
				if t := w.debt / s.target; t > worst {
					worst = t
				}
			}
			if worst < 0.003 {
				budgetHits = 0
				// 达标: 补满本次采样周期, 让目标进程全速跑
				if r := iopsTick - time.Since(tickStart); r > 0 {
					time.Sleep(r)
				}
				tickStart, frozenInTick, frozenRound = time.Now(), 0, false
				continue
			}
			// 还债: 按 need = 欠账/目标 冻结, 分块(5ms..250ms), 冻完立刻复采样
			frz := time.Duration(worst * float64(time.Second))
			if frz < iopsChunkMin {
				frz = iopsChunkMin
			}
			if frz > iopsChunkMax {
				frz = iopsChunkMax
			}
			if left := iopsTickBudget - frozenInTick; frz > left {
				frz = left
			}
			if frz <= 0 {
				// 本周期冻结预算用尽: 让出去一小会再继续; 连续多周期都这样才告警
				if budgetHits++; budgetHits >= 3 {
					s.warnOnce()
				}
				if r := iopsTick - time.Since(tickStart); r > 0 {
					time.Sleep(r)
				}
				tickStart, frozenInTick, frozenRound = time.Now(), 0, false
				continue
			}
			if !s.frz.set(true) {
				// 目标进程已退出(或拿不到句柄): 别空转, 让出去一个采样周期
				time.Sleep(iopsTick)
				tickStart, frozenInTick, frozenRound = time.Now(), 0, false
				continue
			}
			time.Sleep(frz)
			s.frz.set(false)
			frozenInTick += frz
			s.frozenTotal += frz
			frozenRound = true
		}
	}()
	return func() *iopsReport {
		close(s.stopCh)
		<-s.done
		s.frz.close()
		el := time.Since(s.start).Seconds()
		if el <= 0 {
			el = 1e-9
		}
		rep := &iopsReport{elapsed: el, frozen: s.frozenTotal.Seconds() / el}
		for _, w := range s.watches {
			rep.rows = append(rep.rows, iopsRow{
				name: w.name,
				avg:  w.total / el,
				max:  w.max,
				ops:  w.total,
			})
		}
		return rep
	}
}

func (s *iopsLimiter) warnOnce() {
	s.warnMu.Lock()
	defer s.warnMu.Unlock()
	if s.warned {
		return
	}
	s.warned = true
	eprintf("[limit] 提示: 已连续把冻结预算(约 98%%)用满 —— 目标 %.0f/s 低于该负载的自然事务率, "+
		"任务会明显变慢; 想让它跑快点就把目标调高(先跑 --iops 1000000 看自然事务率)\n", s.target)
}

// describe 供启动摘要行使用, 形如 "iops<=200/s@[1 d:, 0 e:]"。
func (s *iopsLimiter) describe() string {
	parts := make([]string, 0, len(s.watches))
	for _, w := range s.watches {
		parts = append(parts, strings.ToLower(w.name))
	}
	return fmt.Sprintf("iops<=%.0f/s@[%s]", s.target, strings.Join(parts, ", "))
}

// pidsInJob 枚举 Job 内全部进程(含孙进程)。
func pidsInJob(job syscall.Handle) []uint32 {
	snap, _, _ := procCreateToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if snap == 0 || snap == uintptr(syscall.InvalidHandle) {
		return nil
	}
	defer closeHandle(syscall.Handle(snap))
	var e processEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var out []uint32
	r, _, _ := procProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(&e)))
	for r != 0 {
		// IsProcessInJob 用 QUERY_LIMITED 会 access denied 而漏掉进程, 必须 QUERY_INFORMATION
		h, _, _ := procOpenProcess.Call(processQueryInfo, 0, uintptr(e.ProcessID))
		if h != 0 {
			var in int32
			if ok, _, _ := procIsProcessInJob.Call(h, uintptr(job), uintptr(unsafe.Pointer(&in))); ok != 0 && in != 0 {
				out = append(out, e.ProcessID)
			}
			closeHandle(syscall.Handle(h))
		}
		r, _, _ = procProcess32NextW.Call(snap, uintptr(unsafe.Pointer(&e)))
	}
	return out
}

// ---------- 执行(仅 Windows) ----------

// prioClassValue 把 --prio 字符串映射为优先级类常量(仅 Windows 路径使用)。
func prioClassValue(p string) uint32 {
	switch p {
	case "idle":
		return idlePriorityClass
	case "below":
		return belowNormalPriorityClass
	case "normal":
		return normalPriorityClass
	case "above":
		return aboveNormalPriorityClass
	default:
		return highPriorityClass
	}
}

// runWindows 创建 Job、施加限制、挂起启动子进程入 Job, 等待并打印统计。
func runWindows(L *limits, cmdArgs []string) int {
	// 组装子进程命令行(按需加引号)
	parts := make([]string, 0, len(cmdArgs))
	for _, a := range cmdArgs {
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

	if L.ioMBps > 0 {
		bw := int64(L.ioMBps * 1024 * 1024)
		if err := setIoRateControl(job, bw, 0); err != nil {
			eprintf("[limit] 设置 IO 限速失败: %v\n", err)
			return 1
		}
		eprintf("[limit] 注意: IO 限速只对无缓冲/直写精确生效; 缓冲写(robocopy/xcopy 默认)会穿透, robocopy 加 /J 可解\n")
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
		setPriorityClass(pi.HProcess, prioClassValue(L.prio))
	}
	if L.haveIoPrio {
		if err := setIoPriority(pi.HProcess, ioPriorityClass(L.ioPrio)); err != nil {
			eprintf("[limit] 设置 IO 优先级失败(已忽略): %v\n", err)
		}
	}
	if err := resumeThread(pi.HThread); err != nil {
		eprintf("[limit] 恢复子进程失败: %v\n", err)
	}

	// --iops: 启动设备层「寻道频率」限制(多物理盘 + 细粒度占空比冻结)
	var stopIops func() *iopsReport
	var iopsLim *iopsLimiter
	if L.ioIops > 0 {
		drives := L.iopsDrives
		if len(drives) == 0 {
			drives = []string{"all"}
		}
		lim, err := newIopsLimiter(job, L.ioIops, drives)
		if err != nil {
			eprintf("[limit] --iops 初始化失败: %v\n", err)
			return 1
		}
		defer procPdhCloseQuery.Call(lim.query)
		iopsLim = lim
		stopIops = lim.run()
	}

	// --timeout: 墙钟计时, 到点 TerminateJobObject 整棵树(子孙一起走)
	var timedOut int32
	timeoutDone := make(chan struct{})
	if L.haveTmout {
		go func() {
			t := time.NewTimer(L.timeout)
			defer t.Stop()
			select {
			case <-t.C:
				atomic.StoreInt32(&timedOut, 1)
				if err := terminateJobObject(job, 124); err != nil {
					eprintf("[limit] 超时终止整棵树失败: %v\n", err)
				}
				// 兜底: 进程若卡在内核等待里(如被挂起时撞上同步 IO), TerminateJobObject 也带不走它,
				// 此时不能陪它一起永远等下去 —— 等 8 秒仍不退出就自己走人。
				time.Sleep(8 * time.Second)
				eprintf("[limit] 目标进程 8 秒内未退出(可能卡在内核等待), limit 强制退出; " +
					"残留进程可能需要重启系统才能清理\n")
				os.Exit(124)
			case <-timeoutDone:
			}
		}()
	}

	// Ctrl+C / 关窗: 先恢复挂起的子进程, 再按 killOnClose 收工 —— 不留挂起态孤儿
	installConsoleCleanup(func() {
		if iopsLim != nil && iopsLim.frz != nil {
			iopsLim.frz.set(false)
		}
		if L.killOnClose {
			terminateJobObject(job, 1)
		}
	})

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
	if iopsLim != nil {
		summary += " " + iopsLim.describe()
	}
	if L.procs != 0 {
		summary += fmt.Sprintf(" procs<=%d", L.procs)
	}
	if L.haveAff {
		summary += fmt.Sprintf(" affinity=%#x", L.affinity)
	}
	if L.havePrio {
		summary += fmt.Sprintf(" prio=%s", L.prio)
	}
	if L.haveIoPrio {
		summary += fmt.Sprintf(" io_prio=%s", L.ioPrio)
	}
	if L.haveTmout {
		summary += fmt.Sprintf(" timeout=%s", L.timeout)
	}
	eprintf("%s\n", summary)

	if err := waitForSingleObject(pi.HProcess); err != nil {
		eprintf("[limit] 等待子进程失败: %v\n", err)
	}
	close(timeoutDone)
	if stopIops != nil {
		rep := stopIops()
		eprintf("[limit] iops 目标 %.0f/s | 设备层实测(整盘口径):\n", L.ioIops)
		for _, r := range rep.rows {
			eprintf("[limit]   %-12s 均值 %.0f/s  峰值 %.0f/s  (共 %.0f 事务)\n",
				strings.ToLower(r.name), r.avg, r.max, r.ops)
		}
		eprintf("[limit]   冻结占空比 %.1f%% | 运行 %.1fs\n", rep.frozen*100, rep.elapsed)
	}
	code, err := getExitCodeProcess(pi.HProcess)
	if err != nil {
		eprintf("[limit] 获取退出码失败: %v\n", err)
	}
	closeHandle(pi.HProcess)
	closeHandle(pi.HThread)

	if atomic.LoadInt32(&timedOut) != 0 {
		eprintf("[limit] 超时 %s: 已终止整棵进程树, 返回 124\n", L.timeout)
	}

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
	if atomic.LoadInt32(&timedOut) != 0 {
		return 124
	}
	return int(code)
}
