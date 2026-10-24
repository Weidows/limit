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
	"syscall"
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
	procGetStdHandle              = kernel32.NewProc("GetStdHandle")
	procGetConsoleMode            = kernel32.NewProc("GetConsoleMode")
	procWriteConsoleW             = kernel32.NewProc("WriteConsoleW")

	// Job Object I/O rate control: 运行时解析, Win7 及更早不存在
	procSetIoRateControlInformationJobObject = kernel32.NewProc("SetIoRateControlInformationJobObject")
)

// JobObjectInformationClass(用到的三个; 不能与结构体同名, Go 单命名空间)
const (
	jobObjectInfoClassBasicAccounting = 1
	jobObjectInfoClassExtendedLimit   = 9
	jobObjectInfoClassCpuRateControl  = 15
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
	stdErrorHandle  = 0xFFFFFFF4 // (DWORD)-12, GetStdHandle 参数
)

// 优先级类
const (
	idlePriorityClass         = 0x00000040
	belowNormalPriorityClass  = 0x00004000
	normalPriorityClass       = 0x00000020
	aboveNormalPriorityClass  = 0x00008000
	highPriorityClass         = 0x00000080
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
	ReadOperationCount      uint64
	WriteOperationCount     uint64
	OtherOperationCount     uint64
	ReadTransferCount       uint64
	WriteTransferCount      uint64
	OtherTransferCount      uint64
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
		0,                                     // lpApplicationName
		uintptr(unsafe.Pointer(p)),            // lpCommandLine(可写)
		0, 0, 0,                               // 安全属性/继承
		createSuspended,                       // 挂起创建
		0, 0,                                  // 环境/工作目录
		uintptr(unsafe.Pointer(&si)),          // STARTUPINFO
		uintptr(unsafe.Pointer(&pi)),          // PROCESS_INFORMATION
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

func setIoRateControl(job syscall.Handle, maxBandwidthBytes int64) error {
	// Find() 内部即 GetProcAddress; 不存在时明确报错, 不静默
	if err := procSetIoRateControlInformationJobObject.Find(); err != nil {
		return fmt.Errorf("IO 限速 API 不可用(需 Win8+): %w", err)
	}
	io := ioRateControlInformation{
		MaxBandwidth: maxBandwidthBytes,
		ControlFlags: jobObjectIoRateControlEnable,
	}
	r1, _, callErr := procSetIoRateControlInformationJobObject.Call(
		uintptr(job), uintptr(unsafe.Pointer(&io)))
	if r1 == 0 {
		return callErr
	}
	return nil
}

// ---------- 控制台输出(中文不乱码) ----------

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
