# limit — Windows 通用程序资源限制器（Job Object 版 cgroup）

`limit [选项] -- <命令> [参数...]`：把命令及其**所有子进程**整体关进一个 Job Object，
像 Docker 一样限制 CPU / 内存 / 磁盘 IO / 进程数。Go 实现，零依赖单文件 exe，不需要管理员。

Job Object 就是 Windows 原生的「进程组资源容器」——Windows 容器本身正是用它实现的，
所以这不是山寨 Docker，而是用对了 Windows 自己的机制。

## 实测验证（i5-14600KF 14C/20T，Windows 11，2026-10）

| 限制 | 测试 | 结果 |
|---|---|---|
| `--cpu 5%` | 4 线程烧 10 秒 | CPU 10.55s ≈ 正好 1 核（5% × 20 逻辑 CPU）✅ |
| `--cpu 10%` | 4 线程烧 6 秒 | CPU 11.14s ≈ 2 核 ✅ |
| `--cpu 25%` | 4 线程烧 6 秒 | C/Go 交替 A/B 同分布（17.7–19.6 vs 18.1–18.9），与不限速基线无差异——5 核对 4 线程不触顶 ✅ |
| `--mem 256M` | 分配 1024MB | ALLOC_FAILED at 254MB，错误码 1455 `ERROR_COMMITMENT_LIMIT` ✅ |
| `--mem 512M --mem-total 700M` | 分配 1024MB | 510MB 处失败（单进程顶先触发）✅ |
| `--io 50` | 直写拷贝 254MB | 10.07s（单向 26.5MB/s，读写合计 53MB/s ≈ 上限）✅ |
| `--affinity 0x1` | 2 线程烧 6 秒 | CPU 5.55s（绑 1 核）✅ |
| `--procs 1` | cmd 里再起 cmd | 孙进程创建被拒，退出码 1 ✅ |

## 构建与测试

```console
go build -o limit.exe .        # CGO_ENABLED=0, 纯标准库零依赖
go test ./...                  # 结构体布局断言 + 解析器单测
```

## 用法

```console
limit.exe --cpu 25% --mem 2G -- 7z x big.7z -oD:\out
limit.exe --io 50 --mem 1G --procs 4 -- xcopy.exe S:\ D:\ /E
limit.exe --cpu 10% --prio below --affinity 0,2-5 -- ffmpeg -i in.mp4 out.mkv
```

| 选项 | 说明 |
|---|---|
| `--cpu <百分比>` | CPU 硬顶（调度器级强制执行） |
| `--mem <大小>` | 单进程 commit 内存硬顶（`512M`/`4G`） |
| `--mem-total <大小>` | 整棵树 commit 内存硬顶 |
| `--io <MB/s>` | 磁盘 IO 速率上限（读写合计口径） |
| `--procs <个数>` | 进程数上限（含子进程树） |
| `--affinity <掩码>` | CPU 亲和性（`0x0F` 或 `0,2-5`） |
| `--prio <级别>` | `idle\|below\|normal\|above\|high` |
| `--no-kill-on-close` | limit 被杀时不拉子进程陪葬（默认陪葬） |

退出时打印：退出码、CPU 时间（user+kernel）、峰值 commit（单进程/整棵树）。

## 语义要点（必读）

1. **`--cpu` 百分比相对整机算力**：100% = 全部逻辑 CPU。20 线程机器上 5% = 1 个核；
   想限制单线程程序只用半核，写 `--cpu 2.5%`。上限高于线程需求时不触顶（实测 25% 对 4 线程无感）。
2. **`--mem` 是 commit 口径的硬顶**：撞顶时 `VirtualAlloc` 直接失败
   （`ERROR_COMMITMENT_LIMIT` 1455），行为类似 cgroup 的 `memory.max`。
   **RSS（工作集）Windows 只有软限**——Job 的 WorkingSet 限制不保证硬顶，这是 OS 语义。
3. **`--io` 是读写合计口径**，且只对「无缓冲/写穿透」IO 精确生效；
   7z/Bandizip 这类默认缓存写的程序，进程退出后由系统 lazy writer 全速回写，**会逃过限制**
   （实测磁盘仍 188MB/s 爆发 + 长尾）。解压/拷贝要稳定限速请用
   [io7z](https://github.com/Weidows/io7z) 的分段节流方案。
4. **整棵进程树生效**：子进程自动继承 Job。启动采用「挂起创建 → 入 Job → 恢复」，
   开头几毫秒也不漏。
5. **不能在已有 Job/容器里跑 limit**（嵌套 Job 限制）：`AssignProcessToJobObject` 会失败，
   工具会明确报错并终止子进程，不会静默放过。

## 实现说明

- **纯标准库 + `syscall.NewLazyDLL` 直调 kernel32**：Go 的 `NewProc` 内部就是
  LoadLibrary/GetProcAddress，C 版因 mingw 导入库缺符号而手写 GetProcAddress 的绕法
  在这里不需要；IO 限速 API 用 `Find()` 预检存在性，不存在时明确报错。
- **结构体布局有回归测试**：`jobs_test.go` 的 `TestStructLayouts` 与
  `legacy/layout.c`（gcc 实测的 sizeof/offsetof）逐一断言——改结构体前先重跑 layout.exe，
  不要凭记忆改字段。
- **控制台中文输出**：`eprintf` 在 stderr 是控制台时走 `WriteConsoleW`（UTF-16），
  被重定向时写 UTF-8——直接 `fmt.Fprintf` 中文到 GBK 控制台会乱码。
- `legacy/limit.c` 是最初的 C 实现（13.5KB，行为与本版逐项对齐），留作布局参考。
- `tests/` 下是三个 C 写的验证工具（burn=CPU 燃烧+自计时，alloc=逐 MB 分配，
  diotest=直写拷贝+计时）——验证工具与限制器语言无关，无需移植。

## 做不到的（需要签名内核驱动）

| 需求 | 为什么用户态做不到 |
|---|---|
| 网络带宽限速 | 需要 WFP callout 驱动（NetLimiter 就是这么干的） |
| 覆盖缓存写的统一磁盘 IO 限速 | 需要 minifilter 驱动 |
| RSS 硬顶 | Windows 语义只给软限 |
| 磁盘空间配额 | NTFS quota 按**用户**不按进程 |

Docker 的「完整」来自 Linux cgroup 在内核里的统一执行点；Windows 的执行点是散的：
CPU/内存/进程数在 Job Object（用户态可设，本文全部实测生效），网络和磁盘 IO 在驱动层。

## 相关

- [io7z](https://github.com/Weidows/io7z) — 机械盘限速解压（IO 限速的可靠落地形态）
