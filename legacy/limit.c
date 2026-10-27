/* limit.c - 通用程序资源限制器 (Windows 版 cgroup, 零依赖单文件)
 *
 * 原理: Job Object 是 Windows 原生的"进程组资源容器"(Windows 容器本身就用它实现)。
 *       子进程及其所有后代自动同属一个 Job, 整棵树被整体限制。
 * 支持: CPU 硬顶 / 内存 commit 上限 / 磁盘 IO 速率 / 进程数 / CPU 亲和性 / 优先级类
 *
 * 编译: gcc -O2 -Wall -static -municode -o limit.exe limit.c
 * 用法: limit.exe [--cpu 25%] [--mem 512M] [--io 50MB/s] [--procs 8]
 *                [--affinity 0,2-5] [--prio below] [--no-kill-on-close] -- 7z x a.7z -oout\
 *
 * 边界(重要, 详见 README):
 *  - --io 只对"无缓冲/写穿透"IO 精确生效; 默认缓存写(7z/Bandizip 等)的回写会逃逸,
 *    磁盘仍会爆发。要稳定限速请用 io7z 的分段节流: https://github.com/Weidows/io7z
 *  - --mem 是 commit(虚拟内存提交)口径的硬顶; RSS(工作集)只有软限, Windows 语义如此。
 *  - 网络带宽/统一磁盘 IO 限速需要 WFP/minifilter 签名驱动, 用户态做不到。
 */
#ifndef _WIN32_WINNT
#define _WIN32_WINNT 0x0602
#endif
#include <windows.h>
#include <wchar.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifndef JOB_OBJECT_CPU_RATE_CONTROL_ENABLE
#define JOB_OBJECT_CPU_RATE_CONTROL_ENABLE 1
#define JOB_OBJECT_CPU_RATE_CONTROL_HARD_CAP 4
#endif
#ifndef JobObjectCpuRateControlInformation
#define JobObjectCpuRateControlInformation ((JOBOBJECTINFOCLASS)15)
#endif

/* mingw 导入库不含此符号, 运行时加载 */
typedef struct { LONG64 MaxIops; LONG64 MaxBandwidth; LONG64 ReservationIops;
                 PCWSTR VolumeName; ULONG BaseIoSize; ULONG ControlFlags; } IO_RATE;
typedef DWORD (WINAPI *FN_SET_IO_RATE)(HANDLE, IO_RATE *);

/* ---------------- 解析 ---------------- */

/* "512M"/"4G"/"1024K"/"1048576" -> 字节 */
static int parse_size(const wchar_t *s, SIZE_T *out)
{
    wchar_t *end = NULL;
    double v = wcstod(s, &end);
    if (v <= 0) return 0;
    SIZE_T mult = 1;
    if (end && *end) {
        if (!_wcsicmp(end, L"K") || !_wcsicmp(end, L"KB")) mult = 1024ULL;
        else if (!_wcsicmp(end, L"M") || !_wcsicmp(end, L"MB")) mult = 1024ULL * 1024;
        else if (!_wcsicmp(end, L"G") || !_wcsicmp(end, L"GB")) mult = 1024ULL * 1024 * 1024;
        else if (!_wcsicmp(end, L"T") || !_wcsicmp(end, L"TB")) mult = 1024ULL * 1024 * 1024 * 1024;
        else return 0;
    }
    *out = (SIZE_T)(v * (double)mult);
    return 1;
}

/* "25%"/"25" -> CpuRate (百分数*100, 合法区间 1..10000) */
static int parse_cpu(const wchar_t *s, DWORD *out)
{
    const wchar_t *pct = wcschr(s, L'%');
    wchar_t buf[32];
    size_t n = pct ? (size_t)(pct - s) : wcslen(s);
    if (n >= 32) return 0;
    wmemcpy(buf, s, n); buf[n] = 0;
    wchar_t *end = NULL;
    double v = wcstod(buf, &end);
    if (v <= 0 || (end && *end)) return 0;
    double rate = v * 100.0;
    if (rate < 1 || rate > 10000) return 0;
    *out = (DWORD)(rate + 0.5);
    return 1;
}

/* "0x0F" 或 "0,2-5,7" -> 掩码 (手写分词, 不依赖 wcstok 三参数版) */
static int parse_affinity(const wchar_t *s, DWORD_PTR *out)
{
    DWORD_PTR mask = 0;
    if (!wcsncmp(s, L"0x", 2) || !wcsncmp(s, L"0X", 2)) {
        mask = (DWORD_PTR)wcstoul(s + 2, NULL, 16);
    } else {
        const wchar_t *p = s;
        while (*p) {
            while (*p == L',') p++;
            if (!*p) break;
            int a = 0, b = -1;
            a = _wtoi(p);
            const wchar_t *dash = p;
            while (*dash && *dash != L',') dash++;
            const wchar_t *d = p;
            while (d < dash && *d != L'-') d++;
            if (d < dash) b = _wtoi(d + 1);
            if (b < a) b = a;
            for (int k = a; k <= b && k < 64; k++) mask |= (DWORD_PTR)1 << k;
            p = dash;
        }
    }
    if (!mask) return 0;
    *out = mask;
    return 1;
}

static wchar_t *quote_arg(const wchar_t *a)
{
    if (!wcspbrk(a, L" \t\"")) return _wcsdup(a);
    size_t n = wcslen(a), q = 0, i;
    for (i = 0; i < n; i++) if (a[i] == L'"') q++;
    wchar_t *out = (wchar_t *)malloc((n + q + 3) * sizeof(wchar_t));
    wchar_t *p = out;
    *p++ = L'"';
    for (i = 0; i < n; i++) { if (a[i] == L'"') *p++ = L'\\'; *p++ = a[i]; }
    *p++ = L'"';
    *p = 0;
    return out;
}

static void usage(const wchar_t *me)
{
    fwprintf(stderr, L"limit - 通用程序资源限制器 (Job Object, 整棵进程树生效)\n\n");
    fwprintf(stderr, L"用法: %ls [选项] -- <命令> [参数...]\n\n", me);
    fwprintf(stderr, L"选项:\n");
    fwprintf(stderr, L"  --cpu <百分比>      CPU 硬顶, 如 25%% (1%%..100%%)\n");
    fwprintf(stderr, L"  --mem <大小>        单进程 commit 内存硬顶, 如 512M / 4G\n");
    fwprintf(stderr, L"  --mem-total <大小>  整棵树 commit 内存硬顶\n");
    fwprintf(stderr, L"  --io <MB/s>         磁盘 IO 速率(读写合计; 直写精确, 缓存写不保证)\n");
    fwprintf(stderr, L"  --iops <次数/s>     IO 每秒操作数上限(可与 --io 并用, 谁先撞顶谁生效)\n");
    fwprintf(stderr, L"  --procs <个数>      进程数上限\n");
    fwprintf(stderr, L"  --affinity <掩码>   CPU 亲和性, 如 0x0F 或 0,2-5\n");
    fwprintf(stderr, L"  --prio <级别>       idle|below|normal|above|high\n");
    fwprintf(stderr, L"  --no-kill-on-close  limit 被杀时不让子进程陪葬(默认陪葬)\n");
    fwprintf(stderr, L"\n示例: %ls --cpu 25%% --mem 2G -- 7z x big.7z -oD:\\out\n", me);
    fwprintf(stderr, L"      %ls --io 50 -- robocopy S:\\ D:\\ /E /J\n", me);
}

/* ---------------- main ---------------- */

int wmain(int argc, wchar_t **argv)
{
    int have_cpu = 0; DWORD cpu_rate = 0;
    SIZE_T mem = 0, mem_total = 0;
    double io_mbps = 0;
    double io_iops = 0;
    DWORD procs = 0;
    DWORD_PTR affinity = 0; int have_aff = 0;
    DWORD prio = 0; int have_prio = 0;
    int kill_on_close = 1;

    int i = 1;
    for (; i < argc; i++) {
        wchar_t *a = argv[i];
        if (!wcscmp(a, L"--")) { i++; break; }
        else if (!wcscmp(a, L"--cpu") && i + 1 < argc) {
            if (!parse_cpu(argv[++i], &cpu_rate)) { fwprintf(stderr, L"[limit] --cpu 值无效: %ls\n", argv[i]); return 2; }
            have_cpu = 1;
        }
        else if (!wcscmp(a, L"--mem") && i + 1 < argc) {
            if (!parse_size(argv[++i], &mem)) { fwprintf(stderr, L"[limit] --mem 值无效: %ls\n", argv[i]); return 2; }
        }
        else if (!wcscmp(a, L"--mem-total") && i + 1 < argc) {
            if (!parse_size(argv[++i], &mem_total)) { fwprintf(stderr, L"[limit] --mem-total 值无效: %ls\n", argv[i]); return 2; }
        }
        else if (!wcscmp(a, L"--io") && i + 1 < argc) {
            io_mbps = _wtof(argv[++i]);
            if (io_mbps <= 0) { fwprintf(stderr, L"[limit] --io 值无效: %ls\n", argv[i]); return 2; }
        }
        else if (!wcscmp(a, L"--iops") && i + 1 < argc) {
            io_iops = _wtof(argv[++i]);
            if (io_iops <= 0) { fwprintf(stderr, L"[limit] --iops 值无效: %ls\n", argv[i]); return 2; }
        }
        else if (!wcscmp(a, L"--procs") && i + 1 < argc) {
            procs = (DWORD)_wtoi(argv[++i]);
            if (procs < 1) { fwprintf(stderr, L"[limit] --procs 值无效: %ls\n", argv[i]); return 2; }
        }
        else if (!wcscmp(a, L"--affinity") && i + 1 < argc) {
            if (!parse_affinity(argv[++i], &affinity)) { fwprintf(stderr, L"[limit] --affinity 值无效: %ls\n", argv[i]); return 2; }
            have_aff = 1;
        }
        else if (!wcscmp(a, L"--prio") && i + 1 < argc) {
            const wchar_t *v = argv[++i];
            if (!_wcsicmp(v, L"idle")) prio = IDLE_PRIORITY_CLASS;
            else if (!_wcsicmp(v, L"below")) prio = BELOW_NORMAL_PRIORITY_CLASS;
            else if (!_wcsicmp(v, L"normal")) prio = NORMAL_PRIORITY_CLASS;
            else if (!_wcsicmp(v, L"above")) prio = ABOVE_NORMAL_PRIORITY_CLASS;
            else if (!_wcsicmp(v, L"high")) prio = HIGH_PRIORITY_CLASS;
            else { fwprintf(stderr, L"[limit] --prio 值无效: %ls\n", v); return 2; }
            have_prio = 1;
        }
        else if (!wcscmp(a, L"--no-kill-on-close")) kill_on_close = 0;
        else if (!wcscmp(a, L"-h") || !wcscmp(a, L"--help")) { usage(argv[0]); return 0; }
        else break;  /* 第一个非选项: 命令开始 */
    }
    if (i >= argc) { usage(argv[0]); return 2; }

    /* 组装子进程命令行 */
    int nargs = argc - i;
    wchar_t **parts = (wchar_t **)calloc((size_t)nargs, sizeof(wchar_t *));
    size_t total = 1;
    for (int k = 0; k < nargs; k++) {
        parts[k] = quote_arg(argv[i + k]);
        total += wcslen(parts[k]) + 1;
    }
    wchar_t *cmdline = (wchar_t *)malloc(total * sizeof(wchar_t));
    cmdline[0] = 0;
    for (int k = 0; k < nargs; k++) {
        if (k) wcscat(cmdline, L" ");
        wcscat(cmdline, parts[k]);
    }

    /* ---- 创建 Job 并施加限制 ---- */
    HANDLE job = CreateJobObjectW(NULL, NULL);
    if (!job) { fwprintf(stderr, L"[limit] CreateJobObject 失败: %lu\n", GetLastError()); return 1; }

    JOBOBJECT_EXTENDED_LIMIT_INFORMATION eli;
    ZeroMemory(&eli, sizeof(eli));
    if (mem) {
        eli.BasicLimitInformation.LimitFlags |= JOB_OBJECT_LIMIT_PROCESS_MEMORY;
        eli.ProcessMemoryLimit = mem;
    }
    if (mem_total) {
        eli.BasicLimitInformation.LimitFlags |= JOB_OBJECT_LIMIT_JOB_MEMORY;
        eli.JobMemoryLimit = mem_total;
    }
    if (procs) {
        eli.BasicLimitInformation.LimitFlags |= JOB_OBJECT_LIMIT_ACTIVE_PROCESS;
        eli.BasicLimitInformation.ActiveProcessLimit = procs;
    }
    if (have_aff) {
        eli.BasicLimitInformation.LimitFlags |= JOB_OBJECT_LIMIT_AFFINITY;
        eli.BasicLimitInformation.Affinity = affinity;
    }
    if (kill_on_close) eli.BasicLimitInformation.LimitFlags |= JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
    if (!SetInformationJobObject(job, JobObjectExtendedLimitInformation, &eli, sizeof(eli))) {
        fwprintf(stderr, L"[limit] 设置扩展限制失败: %lu\n", GetLastError());
        return 1;
    }

    if (have_cpu) {
        JOBOBJECT_CPU_RATE_CONTROL_INFORMATION c;
        ZeroMemory(&c, sizeof(c));
        c.ControlFlags = JOB_OBJECT_CPU_RATE_CONTROL_ENABLE | JOB_OBJECT_CPU_RATE_CONTROL_HARD_CAP;
        c.CpuRate = cpu_rate;
        if (!SetInformationJobObject(job, JobObjectCpuRateControlInformation, &c, sizeof(c))) {
            fwprintf(stderr, L"[limit] 设置 CPU 硬顶失败: %lu\n", GetLastError());
            return 1;
        }
    }

    if (io_mbps > 0 || io_iops > 0) {
        FN_SET_IO_RATE pSet = (FN_SET_IO_RATE)
            GetProcAddress(GetModuleHandleW(L"kernel32.dll"), "SetIoRateControlInformationJobObject");
        if (!pSet) {
            fwprintf(stderr, L"[limit] IO 限速 API 不可用(需 Win8+)\n");
            return 1;
        }
        IO_RATE io; ZeroMemory(&io, sizeof(io));
        io.MaxBandwidth = (LONG64)(io_mbps * 1024.0 * 1024.0);
        io.MaxIops = (LONG64)(io_iops + 0.5);
        io.ControlFlags = 1; /* JOB_OBJECT_IO_RATE_CONTROL_ENABLE */
        if (pSet(job, &io) == 0) {
            fwprintf(stderr, L"[limit] 设置 IO 限速失败: %lu\n", GetLastError());
            return 1;
        }
        fwprintf(stderr, L"[limit] 注意: IO 限速对缓存写不保证(回写会爆发), 直写/写穿透 IO 才精确生效\n");
    }

    /* ---- 挂起启动 -> 入 Job -> 恢复(避免开头几毫秒不受限) ---- */
    STARTUPINFOW si; ZeroMemory(&si, sizeof(si)); si.cb = sizeof(si);
    PROCESS_INFORMATION pi; ZeroMemory(&pi, sizeof(pi));
    if (!CreateProcessW(NULL, cmdline, NULL, NULL, TRUE, CREATE_SUSPENDED, NULL, NULL, &si, &pi)) {
        fwprintf(stderr, L"[limit] 启动失败(%lu): %ls\n", GetLastError(), cmdline);
        return 1;
    }
    if (!AssignProcessToJobObject(job, pi.hProcess)) {
        DWORD err = GetLastError();
        fwprintf(stderr, L"[limit] 加入 Job 失败: %lu (进程可能已在别的 Job/容器里)\n", err);
        TerminateProcess(pi.hProcess, 1);
        CloseHandle(pi.hProcess); CloseHandle(pi.hThread);
        return 1;
    }
    if (have_prio) SetPriorityClass(pi.hProcess, prio);
    ResumeThread(pi.hThread);

    /* ---- 摘要行 ---- */
    {
        wchar_t buf[512];
        int off = _snwprintf(buf, 512, L"[limit] 运行中 (pid %lu):", pi.dwProcessId);
        if (have_cpu)  off += _snwprintf(buf + off, (size_t)(512 - off), L" cpu<=%.2f%%", cpu_rate / 100.0);
        if (mem)       off += _snwprintf(buf + off, (size_t)(512 - off), L" mem<=%.0fMB", mem / 1e6);
        if (mem_total) off += _snwprintf(buf + off, (size_t)(512 - off), L" mem_total<=%.0fMB", mem_total / 1e6);
        if (io_mbps)   off += _snwprintf(buf + off, (size_t)(512 - off), L" io<=%.0fMB/s", io_mbps);
        if (io_iops)   off += _snwprintf(buf + off, (size_t)(512 - off), L" iops<=%.0f/s", io_iops);
        if (procs)     off += _snwprintf(buf + off, (size_t)(512 - off), L" procs<=%lu", procs);
        if (have_aff)  off += _snwprintf(buf + off, (size_t)(512 - off), L" affinity=%llx", (unsigned long long)affinity);
        if (have_prio) off += _snwprintf(buf + off, (size_t)(512 - off), L" prio=%lu", prio);
        fwprintf(stderr, L"%ls\n", buf);
    }

    WaitForSingleObject(pi.hProcess, INFINITE);
    DWORD code = 1;
    GetExitCodeProcess(pi.hProcess, &code);
    CloseHandle(pi.hProcess); CloseHandle(pi.hThread);

    /* ---- 退出统计 ---- */
    {
        JOBOBJECT_BASIC_ACCOUNTING_INFORMATION acc;
        JOBOBJECT_EXTENDED_LIMIT_INFORMATION q;
        ZeroMemory(&acc, sizeof(acc));
        ZeroMemory(&q, sizeof(q));
        if (QueryInformationJobObject(job, JobObjectBasicAccountingInformation, &acc, sizeof(acc), NULL)) {
            double user = (double)acc.TotalUserTime.QuadPart / 1e7;
            double kern = (double)acc.TotalKernelTime.QuadPart / 1e7;
            fwprintf(stderr, L"[limit] 退出码 %lu | CPU 时间 user %.2fs + kernel %.2fs\n",
                     code, user, kern);
        }
        if (QueryInformationJobObject(job, JobObjectExtendedLimitInformation, &q, sizeof(q), NULL)) {
            fwprintf(stderr, L"[limit] 峰值 commit: 单进程 %.0f MB / 整棵树 %.0f MB\n",
                     q.PeakProcessMemoryUsed / 1e6, q.PeakJobMemoryUsed / 1e6);
        }
    }
    CloseHandle(job);
    return (int)code;
}
