// jobs_test.go — 结构体布局断言 + 解析器单测
//
// 布局期望值来自 legacy/layout.c (gcc x86_64-w64-mingw32 在 Win11 x64 上的实测),
// 改 jobs.go 的结构体前先重跑 layout.exe 核对, 不要凭记忆改字段。
package main

import (
	"testing"
	"unsafe"
)

func TestStructLayouts(t *testing.T) {
	sizes := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"jobObjectBasicLimitInformation", unsafe.Sizeof(jobObjectBasicLimitInformation{}), 64},
		{"ioCounters", unsafe.Sizeof(ioCounters{}), 48},
		{"jobObjectExtendedLimitInformation", unsafe.Sizeof(jobObjectExtendedLimitInformation{}), 144},
		{"jobObjectCpuRateControlInformation", unsafe.Sizeof(jobObjectCpuRateControlInformation{}), 8},
		{"jobObjectBasicAccountingInformation", unsafe.Sizeof(jobObjectBasicAccountingInformation{}), 48},
		{"ioRateControlInformation", unsafe.Sizeof(ioRateControlInformation{}), 40},
		{"startupInfo", unsafe.Sizeof(startupInfo{}), 104},
		{"processInformation", unsafe.Sizeof(processInformation{}), 24},
	}
	for _, c := range sizes {
		if c.got != c.want {
			t.Errorf("sizeof(%s) = %d, want %d", c.name, c.got, c.want)
		}
	}

	var b jobObjectBasicLimitInformation
	var e jobObjectExtendedLimitInformation
	var c jobObjectCpuRateControlInformation
	var a jobObjectBasicAccountingInformation
	offs := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"basic.LimitFlags", unsafe.Offsetof(b.LimitFlags), 16},
		{"basic.MinimumWorkingSetSize", unsafe.Offsetof(b.MinimumWorkingSetSize), 24},
		{"basic.MaximumWorkingSetSize", unsafe.Offsetof(b.MaximumWorkingSetSize), 32},
		{"basic.ActiveProcessLimit", unsafe.Offsetof(b.ActiveProcessLimit), 40},
		{"basic.Affinity", unsafe.Offsetof(b.Affinity), 48},
		{"basic.PriorityClass", unsafe.Offsetof(b.PriorityClass), 56},
		{"basic.SchedulingClass", unsafe.Offsetof(b.SchedulingClass), 60},
		{"extended.BasicLimitInformation", unsafe.Offsetof(e.BasicLimitInformation), 0},
		{"extended.IoInfo", unsafe.Offsetof(e.IoInfo), 64},
		{"extended.ProcessMemoryLimit", unsafe.Offsetof(e.ProcessMemoryLimit), 112},
		{"extended.JobMemoryLimit", unsafe.Offsetof(e.JobMemoryLimit), 120},
		{"extended.PeakProcessMemoryUsed", unsafe.Offsetof(e.PeakProcessMemoryUsed), 128},
		{"extended.PeakJobMemoryUsed", unsafe.Offsetof(e.PeakJobMemoryUsed), 136},
		{"cpu.ControlFlags", unsafe.Offsetof(c.ControlFlags), 0},
		{"cpu.CpuRate", unsafe.Offsetof(c.CpuRate), 4},
		{"acc.TotalUserTime", unsafe.Offsetof(a.TotalUserTime), 0},
		{"acc.TotalPageFaultCount", unsafe.Offsetof(a.TotalPageFaultCount), 32},
	}
	for _, o := range offs {
		if o.got != o.want {
			t.Errorf("offsetof(%s) = %d, want %d", o.name, o.got, o.want)
		}
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		in   string
		want uintptr
		ok   bool
	}{
		{"512M", 512 * 1024 * 1024, true},
		{"4G", 4 * 1024 * 1024 * 1024, true},
		{"1024K", 1024 * 1024, true},
		{"1048576", 1048576, true},
		{"2MB", 2 * 1024 * 1024, true},
		{"1.5G", uintptr(1.5 * 1024 * 1024 * 1024), true},
		{"0M", 0, false},
		{"-5M", 0, false},
		{"abc", 0, false},
		{"12X", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := parseSize(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseSize(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseCPU(t *testing.T) {
	cases := []struct {
		in   string
		want uint32
		ok   bool
	}{
		{"25%", 2500, true},
		{"25", 2500, true},
		{"2.5%", 250, true},
		{"100%", 10000, true},
		{"0.005%", 1, true},  // 0.5 四舍五入到 1(=0.01% API 下限), 与 C 版 (DWORD)(x+0.5) 一致
		{"0.001%", 0, false}, // 0.1 舍入后为 0, 低于下限
		{"150%", 0, false},
		{"abc", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := parseCPU(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseCPU(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseAffinity(t *testing.T) {
	cases := []struct {
		in   string
		want uintptr
		ok   bool
	}{
		{"0x0F", 0x0F, true},
		{"0x1", 1, true},
		{"0", 1, true},
		{"0,2-5", 1 | 4 | 8 | 16 | 32, true},
		{"1", 2, true},
		{"7", 128, true},
		{"0x0", 0, false},
		{"abc", 0, false},
		{"", 0, false},
		{"5-2", 1 << 5, true}, // 反向区间按单核处理
	}
	for _, c := range cases {
		got, ok := parseAffinity(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseAffinity(%q) = (%#x, %v), want (%#x, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseDriveList(t *testing.T) {
	cases := []struct {
		args []string
		want []string
		n    int
	}{
		{[]string{"D:"}, []string{"d"}, 1},
		{[]string{"D:", "E:"}, []string{"d", "e"}, 2},
		{[]string{"c", "f:"}, []string{"c", "f"}, 2},
		{[]string{"all"}, []string{"all"}, 1},
		{[]string{"ALL", "D:"}, []string{"all", "d"}, 2},
		{[]string{"D:", "--", "robocopy"}, []string{"d"}, 1}, // 遇到 -- 停
		{[]string{"D:", "x"}, []string{"d", "x"}, 2},         // 单字母也算盘符(命令名一般不会只有一个字母)
		{[]string{"robocopy"}, nil, 0},                       // 根本不是盘符
		{[]string{""}, nil, 0},
	}
	for _, c := range cases {
		got, n := parseDriveList(c.args)
		if n != c.n || len(got) != len(c.want) {
			t.Errorf("parseDriveList(%q) = (%q, %d), want (%q, %d)", c.args, got, n, c.want, c.n)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseDriveList(%q)[%d] = %q, want %q", c.args, i, got[i], c.want[i])
			}
		}
	}
}

func TestQuoteArg(t *testing.T) {
	cases := []struct{ in, want string }{
		{"simple.exe", "simple.exe"},
		{`C:\Program Files\7-Zip\7z.exe`, `"C:\Program Files\7-Zip\7z.exe"`},
		{`a"b`, `"a\"b"`},
		{"", ""},
	}
	for _, c := range cases {
		if got := quoteArg(c.in); got != c.want {
			t.Errorf("quoteArg(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
