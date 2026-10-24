/* alloc.c - 内存分配测试工具: alloc <MB>
 * 逐 MB VirtualAlloc(MEM_COMMIT)+touch; 撞到 commit 上限时打印失败点 */
#include <windows.h>
#include <stdio.h>
#include <stdlib.h>

int wmain(int argc, wchar_t **argv)
{
    int mb = argc > 1 ? _wtoi(argv[1]) : 512;
    size_t chunk = 1 << 20;
    for (int i = 0; i < mb; i++) {
        void *p = VirtualAlloc(NULL, chunk, MEM_COMMIT | MEM_RESERVE, PAGE_READWRITE);
        if (!p) {
            wprintf(L"ALLOC_FAILED at %d MB (GetLastError=%lu)\n", i, GetLastError());
            return 3;
        }
        memset(p, 1, chunk);   /* touch, 真正计入工作集 */
        if (i % 128 == 0) wprintf(L"committed %d MB\n", i);
    }
    wprintf(L"ALLOC_OK total %d MB\n", mb);
    return 0;
}
