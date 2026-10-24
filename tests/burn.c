/* burn.c - CPU 燃烧测试工具: burn <秒> <线程数>
 * 退出时打印自己消耗的 CPU 秒数(GetProcessTimes), 供验证 --cpu 硬顶 */
#include <windows.h>
#include <stdio.h>
#include <stdlib.h>

static DWORD WINAPI thr(LPVOID p)
{
    (void)p;
    volatile LONG64 x = 0;
    for (;;) x++;
    return 0;
}

int wmain(int argc, wchar_t **argv)
{
    int secs = argc > 1 ? _wtoi(argv[1]) : 5;
    int n = argc > 2 ? _wtoi(argv[2]) : 1;
    if (secs < 1) secs = 1;
    if (n < 1) n = 1;
    for (int i = 0; i < n; i++) CreateThread(NULL, 0, thr, NULL, 0, NULL);
    Sleep(secs * 1000);
    FILETIME c, e, k, u;
    GetProcessTimes(GetCurrentProcess(), &c, &e, &k, &u);
    ULARGE_INTEGER uk, uu;
    uk.LowPart = k.dwLowDateTime; uk.HighPart = k.dwHighDateTime;
    uu.LowPart = u.dwLowDateTime; uu.HighPart = u.dwHighDateTime;
    double cpu = (double)(uk.QuadPart + uu.QuadPart) / 1e7;
    wprintf(L"BURN_DONE wall=%ds threads=%d cpu_seconds=%.2f\n", secs, n, cpu);
    ExitProcess(0);
}
