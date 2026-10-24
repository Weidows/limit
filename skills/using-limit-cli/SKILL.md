---
name: using-limit-cli
description: >-
  Run Windows commands inside a Job Object resource container with hard caps on
  CPU, memory (commit), disk IO (bandwidth and IOPS), process count, CPU
  affinity, and priority class, applied to the entire process tree. Use when an
  agent needs to launch a heavy or rowdy command (archive extraction, bulk file
  copying, builds, checksums, runaway tools) without starving the rest of the
  machine — especially on HDDs, where many-small-file workloads collapse
  throughput and need an IOPS cap instead of a bandwidth cap. Covers option
  semantics, measured caveats (cached-write escape, job-wide aggregate IO,
  metadata ops counted), recipes, and exit stats.
user-invocable: true
metadata: {"openclaw":{"emoji":"🛡️","os":["win32"]}}
---

# using-limit-cli — limit.exe 使用技能

`limit.exe [选项] -- <命令> [参数...]` 把命令及其**整棵子进程树**关进一个 Windows
Job Object,硬限制 CPU / 内存 / 磁盘 IO / 进程数。零依赖单文件 exe(Win8+,免管理员)。
agent 要跑重活(解压、批量拷贝、构建、校验)又不想拖垮整机时,一律套 limit。

## 0. 先确认可用

```bash
# exe 在仓库根(gitignore 了,缺就现场构建;纯标准库,秒级)
cd <limit 仓库根> && go build -o limit.exe .   # 若 limit.exe 不存在
limit.exe --help
```

- 若报「IO 限速 API 不可用」→ 系统低于 Win8,`--io`/`--iops` 不可用,其余选项仍可用。
- 若报「加入 Job 失败」→ 当前 shell 已在别的 Job/容器里(嵌套 Job 是 Windows 限制),
  换普通终端跑,不要静默放弃。

## 1. 基本用法

选项在前,`--` 之后是被限制的命令:

```bash
limit.exe --cpu 25% --mem 2G --io 50 -- 7z x big.7z -oD:\out
```

limit 会挂起启动子进程 → 入 Job → 恢复(开头几毫秒也不受限),默认 limit 被杀时
整棵子进程树陪葬(`--no-kill-on-close` 关闭)。退出时打印退出码、CPU 时间、峰值 commit。

## 2. 选项速查

| 选项 | 说明 |
|---|---|
| `--cpu 25%` | CPU 硬顶,**相对整机算力**(20 线程机上 5% = 1 核) |
| `--mem 512M` | 单进程 commit 内存硬顶,撞顶时分配直接失败(≈cgroup memory.max) |
| `--mem-total 4G` | 整棵树 commit 硬顶 |
| `--io 50` | 磁盘速率上限 MB/s,读写合计 |
| `--iops 60` | 每秒 IO 操作数上限,读写合计,可与 `--io` 并用(谁先撞顶谁生效) |
| `--procs 4` | 进程数上限(含整棵树) |
| `--affinity 0x0F` | CPU 亲和性,掩码或 `0,2-5` |
| `--prio idle` | `idle\|below\|normal\|above\|high` |
| `--no-kill-on-close` | limit 被杀时不杀子进程 |

## 3. 常用配方

```bash
# 限速解压大压缩包(默认挡不住缓存写回写,见坑 #1;要稳定限速用 io7z)
limit.exe --cpu 25% --mem 2G --io 50 -- 7z x big.7z -oD:\out

# HDD 上批量小文件(拷贝/解压/建目录):带宽顶永远撞不上,伤盘的是随机寻道
# → 用 IOPS 顶,60/s 左右既能干活又不卡机;元数据操作(create/close)也计数,一并管住
limit.exe --iops 60 -- xcopy.exe S:\ D:\ /E

# 顺序大块拷贝:带宽顶管总量,IOPS 顶防小文件抖动,双保险
limit.exe --io 80 --iops 100 -- robocopy S:\ D:\backup /E

# 后台任务让路 + 限制核数
limit.exe --prio idle --cpu 10% -- ffmpeg -i in.mp4 out.mkv

# 缓存写程序(7z 等)回写限不住?CPU 顶当 IO 节流阀,对所有 IO 类型生效:
# 压缩解压吃 CPU,压住 CPU 即压住数据产出速率;HDD 上还能削平回写爆发
limit.exe --cpu 5% -- 7z x big.7z -oHDD:\out

# 防失控工具吃爆内存(单进程 1G,整棵树 4G)
limit.exe --mem 1G --mem-total 4G -- some_tool.exe
```

## 4. 语义与坑(必读,全部实测结论)

1. **缓存写会逃逸**:`--io`/`--iops` 只对无缓冲/写穿透 IO 精确生效。7z/Bandizip
   等默认缓存写的程序,退出后系统 lazy writer 全速回写,限不住。
   解压要稳定限速用 io7z 的分段节流(本地项目,与 limit 同级目录)。
2. **`--io` 是整 Job 聚合口径,不是按盘独立**:Job 内多盘并行 IO 共享同一个配额
   (实测与文档不符,以实测为准)。
3. **`--mem` 是 commit 口径**:RSS/工作集只有软限,Windows 语义如此,别承诺 RSS 硬顶。
4. **`--cpu` 按整机算力算**:不是单核百分比;20 线程机上 5% = 1 核。
5. **小文件场景选 `--iops` 而不是 `--io`**:HDD 被小文件拖到远低于标称速度时,
   带宽顶形同虚设;IOPS 顶直接限制操作数/寻道风暴。
6. **缓存写限不住又不想换工具 → 用 `--cpu` 当节流阀**:CPU 顶对所有 IO 类型生效
   (含缓存写),实际 IO 速率 ≈ min(磁盘自然速率, CPU 顶折算速率)。
   前提是负载真的吃 CPU(压缩/解压/转码);纯拷贝是 IO 型负载,CPU 顶管不住。
7. **限速生效的判断**:撞顶时任务变慢是预期行为;若完全没慢,先确认负载是直写 IO
   (坑 #1),再看摘要行是否带上了对应限制。

## 5. 退出统计怎么用

正常退出打印三行:退出码 + CPU 时间(user/kernel)、峰值 commit(单进程/整棵树)。
- 退出码 1455 相关错误 → 内存顶被撞(坑 #3)。
- 进程数顶被撞 → 子进程创建失败,退出码来自被拒的子进程。
- IO 没被限住 → 对照坑 #1/#2 排查,不要盲目调大数值。
