/* layout.c - 打印 Win32 Job Object 结构体的 sizeof/字段偏移
 * 用途: Go 移植版(jobs.go)用这些实测值写布局断言回归测试
 * 编译: gcc -O2 -o layout.exe layout.c && ./layout.exe
 */
#include <windows.h>
#include <winnt.h>
#include <stddef.h>
#include <stdio.h>

#define P(s, f) printf("  offsetof(%s, %s) = %zu\n", #s, #f, offsetof(s, f))

int main(void)
{
    printf("sizeof(JOBOBJECT_BASIC_LIMIT_INFORMATION)     = %zu\n", sizeof(JOBOBJECT_BASIC_LIMIT_INFORMATION));
    printf("sizeof(IO_COUNTERS)                           = %zu\n", sizeof(IO_COUNTERS));
    printf("sizeof(JOBOBJECT_EXTENDED_LIMIT_INFORMATION)  = %zu\n", sizeof(JOBOBJECT_EXTENDED_LIMIT_INFORMATION));
    printf("sizeof(JOBOBJECT_CPU_RATE_CONTROL_INFORMATION)= %zu\n", sizeof(JOBOBJECT_CPU_RATE_CONTROL_INFORMATION));
    printf("sizeof(JOBOBJECT_BASIC_ACCOUNTING_INFORMATION)= %zu\n", sizeof(JOBOBJECT_BASIC_ACCOUNTING_INFORMATION));
    printf("sizeof(STARTUPINFOW)                          = %zu\n", sizeof(STARTUPINFOW));
    printf("sizeof(PROCESS_INFORMATION)                   = %zu\n", sizeof(PROCESS_INFORMATION));
    printf("sizeof(SIZE_T)                                = %zu\n", sizeof(SIZE_T));
    P(JOBOBJECT_BASIC_LIMIT_INFORMATION, LimitFlags);
    P(JOBOBJECT_BASIC_LIMIT_INFORMATION, MinimumWorkingSetSize);
    P(JOBOBJECT_BASIC_LIMIT_INFORMATION, MaximumWorkingSetSize);
    P(JOBOBJECT_BASIC_LIMIT_INFORMATION, ActiveProcessLimit);
    P(JOBOBJECT_BASIC_LIMIT_INFORMATION, Affinity);
    P(JOBOBJECT_BASIC_LIMIT_INFORMATION, PriorityClass);
    P(JOBOBJECT_BASIC_LIMIT_INFORMATION, SchedulingClass);
    P(JOBOBJECT_EXTENDED_LIMIT_INFORMATION, BasicLimitInformation);
    P(JOBOBJECT_EXTENDED_LIMIT_INFORMATION, IoInfo);
    P(JOBOBJECT_EXTENDED_LIMIT_INFORMATION, ProcessMemoryLimit);
    P(JOBOBJECT_EXTENDED_LIMIT_INFORMATION, JobMemoryLimit);
    P(JOBOBJECT_EXTENDED_LIMIT_INFORMATION, PeakProcessMemoryUsed);
    P(JOBOBJECT_EXTENDED_LIMIT_INFORMATION, PeakJobMemoryUsed);
    P(JOBOBJECT_CPU_RATE_CONTROL_INFORMATION, ControlFlags);
    P(JOBOBJECT_CPU_RATE_CONTROL_INFORMATION, CpuRate);
    P(JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, TotalUserTime);
    P(JOBOBJECT_BASIC_ACCOUNTING_INFORMATION, TotalPageFaultCount);
    printf("STD_ERROR_HANDLE = %d\n", STD_ERROR_HANDLE);
    return 0;
}
