---
name: using-limit-cli
description: >-
  Run a command inside a resource container with hard caps on
  CPU, memory (commit), disk IO (bandwidth, plus device-level transfers/sec as
  a seek-rate cap for HDDs), process count, CPU affinity, priority class, and
  wall-clock timeout, applied to the entire process tree. Backend is
  Windows Job Object (linux/macOS builds report unsupported). Use when an
  agent needs to launch a heavy or rowdy command (archive extraction, bulk file
  copying, builds, checksums, runaway tools) without starving the rest of the
  machine — especially on HDDs, where many-small-file workloads collapse
  throughput and need a device-level transfers/sec cap instead of a bandwidth
  cap. Covers option
  semantics, measured caveats (cached-write escape, job-wide aggregate IO,
  metadata ops counted), recipes, and exit stats.
user-invocable: true
metadata: {"openclaw":{"emoji":"🛡️","os":["win32"]}}
---

# using-limit-cli — limit 使用技能

`limit [选项] -- <命令> [参数...]` 把命令及其**整棵子进程树**关进一个资源容器,
硬限制 CPU / 内存 / 磁盘 IO / 进程数。零依赖单文件二进制。
agent 要跑重活(解压、批量拷贝、构建、校验)又不想拖垮整机时,一律套 limit。

**平台**:发布件覆盖 Windows / Linux / macOS,但限制后端目前只有 Windows
(Job Object,免管理员,Win8+);非 Windows 平台执行到施加限制那一步会明确报错退出,
不静默放行。以下选项语义按全平台一致描述,机制细节以 Windows 后端实测为准。

## 0. 先确认可用

```bash
# 二进制在仓库根或 Release 附件(缺就现场构建;纯标准库,秒级)
cd <limit 仓库根> && go build -o limit .   # Windows 下产物为 limit.exe
limit --help
```

- 若报「IO 限速 API 不可用」→ 系统低于 Win8,`--io`/`--iops` 不可用,其余选项仍可用。
- 若报「加入 Job 失败」→ 当前 shell 已在别的 Job/容器里(嵌套 Job 是 Windows 限制),
  换普通终端跑,不要静默放弃。
- 若报「此平台不支持」→ 当前是 linux/macOS,cgroup 后端尚未移植;临时等价手段
  `systemd-run --scope -p CPUQuota=25% -- <命令>`(linux),不要用 limit 假装限制。

## 1. 基本用法

选项在前,`--` 之后是被限制的命令:

```bash
limit --cpu 25% --mem 2G --io 50 -- 7z x big.7z -oD:\out
```

limit 会挂起启动子进程 → 入 Job → 恢复(开头几毫秒也不受限),默认 limit 被杀时
整棵子进程树陪葬(`--no-kill-on-close` 关闭)。退出时打印退出码、CPU 时间、峰值 commit。

## 2. 选项速查

| 选项 | 说明 |
|---|---|
| `--cpu 25%` | CPU 硬顶,**相对整机算力**(20 线程机上 5% = 1 核) |
| `--mem 512M` | 单进程 commit 内存硬顶,撞顶时分配直接失败(≈cgroup memory.max) |
| `--mem-total 4G` | 整棵树 commit 硬顶 |
| `--io 50` | 磁盘速率上限 MB/s,读写合计(是天花板,自然速度低于它时不生效、不会变慢) |
| `--iops 200 [盘...]` | **限 HDD 寻道频率**=物理盘设备层事务/秒(免管理员)。不给盘符=所有固定卷,也可 `--iops 200 D: E:` 只盯这两盘,**任一盘超目标就压**;同盘多盘符只算一次。大文件几乎无感,大量小文件被削得最狠 |
| `--procs 4` | 进程数上限(含整棵树) |
| `--affinity 0x0F` | CPU 亲和性,掩码或 `0,2-5` |
| `--prio idle` | `idle\|below\|normal\|above\|high` |
| `--timeout 2h` | 墙钟超时:`TerminateJobObject` 整棵树,退出码 **124**(`90s`/`10m`/`1h`,纯数字=秒) |
| `--no-kill-on-close` | limit 被杀时不杀子进程 |

## 3. 常用配方

```bash
# 限速解压大压缩包(缓存写回写挡不住,见坑 #1;吃 CPU 的活套 --cpu 当节流阀)
limit --cpu 25% --mem 2G --io 50 -- 7z x big.7z -oD:\out

# HDD 上批量小文件(拷贝/解压/建目录):带宽顶永远撞不上,伤盘的是随机寻道
# → 用 --iops <事务/s> [盘...] 限「设备层事务率」: 大文件几乎不受影响, 小文件被削最狠
# src / dst 都列上(不给盘符 = 所有固定卷),任一盘超目标就压(同物理盘只算一次)
limit --iops 200 D: E: -- robocopy S:\ D:\backup /E
# 目标怎么定: 先 --iops 1000000 D: 跑一遍,看日志里的「均值 N/s」= 该负载自然事务率,
# 取它的 1/2 ~ 1/3。实测:100/s 下小文件慢 6.9x、2GB 大文件只慢 1.7x(200/s 时大文件零影响)

# 长时间跑怕忘记:加墙钟超时,到期整棵树被终止、退出码 124(coreutils timeout 同款)
limit --timeout 2h --iops 200 D: E: -- robocopy S:\ D:\backup /E

# 顺序大块拷贝要限数据速率:缓冲写会穿透 --io,robocopy 必须加 /J 走无缓冲
limit --io 50 -- robocopy S:\ D:\ /E /J

# 顺序大块 + 小文件混合:带宽顶管总量,寻道顶防小文件抖动,双保险(同样要 /J)
limit --io 80 --iops 200 D: E: -- robocopy S:\ D:\backup /E /J

# 后台任务让路 + 限制核数
limit --prio idle --cpu 10% -- ffmpeg -i in.mp4 out.mkv

# 缓存写程序(7z 等)回写限不住?CPU 顶当 IO 节流阀,对所有 IO 类型生效:
# 压缩解压吃 CPU,压住 CPU 即压住数据产出速率;HDD 上还能削平回写爆发
limit --cpu 5% -- 7z x big.7z -oHDD:\out

# 防失控工具吃爆内存(单进程 1G,整棵树 4G)
limit --mem 1G --mem-total 4G -- some_tool.exe
```

## 4. 语义与坑(必读,全部实测结论)

1. **缓冲写会穿透 `--io`/`--iops`(数据侧)**:它们只对无缓冲/直写/写穿透 IO 精确生效。
   robocopy/xcopy 默认模式、7z/Bandizip 等缓冲写程序的 `WriteFile` 只进系统缓存即返回,
   真正落盘由内核回写器在 Job 之外全速发出——实测 `--io 30` 下 robocopy 814 MB/s、
   xcopy 506 MB/s,与不限速无异;robocopy **加 `/J`** 走无缓冲后 `--io` 精确生效
   (`/J --io 50` → 37 MB/s)。xcopy 没有无缓冲模式,换 robocopy `/J` 或用 `--cpu` 节流阀(坑 #6)。
2. **`--io` 是整 Job 聚合口径,不是按盘独立**:Job 内多盘并行 IO 共享同一个配额
   (实测与文档不符,以实测为准)。
3. **`--mem` 是 commit 口径**:RSS/工作集只有软限,Windows 语义如此,别承诺 RSS 硬顶。
4. **`--cpu` 按整机算力算**:不是单核百分比;20 线程机上 5% = 1 核。
5. **`--iops` 限的是「设备层事务数/秒」= HDD 寻道频率**:顺序 1MB 只算 1 次设备事务
   (≈100MB/s 也只有 100 事务/s),随机 4KB 每 4KB 就 1 次寻道。所以同一档位下大文件几乎无感、
   大量小文件被削最狠。不给盘符就盯**所有固定卷**,也可 `--iops 200 D: E:` 只盯这两盘,
   **任一盘超目标就冻结整棵树**(同物理盘只算一次)。
   实测(robocopy `/J`,D:→E: HDD):4000×64KB 小文件 `--iops 200` ×3.6、`--iops 100` ×6.9;
   2GB 单文件 `--iops 200` **零影响**、`--iops 100` ×1.7;实测速率 189/s、97/s(目标 200、100)。
   注意:① 计数器是**整盘**口径(同盘其他程序也计入),无法区分是谁造成的;
   ② **验证必须用 `/J` 无缓冲写**,否则 64GB 内存缓存会让设备事务归零、测不出东西;
   ③ 旧的内核 IOPS 顶(按 8KB 归一化)已移除,`--iops` 不再有"N×8KB/s 带宽顶"的失真;
   ④ **别拿 `du` 量进度**:robocopy 会预分配目标文件到源大小,目录表观大小 1 秒就"满"了,
   要测速度就跑完整拷贝计时;也别在刚写完语料时测(脏页回写会抢盘)。
   ⑤ 大文件负载(几百 MB/个)自然事务率只有几十~一两百/s,**通常不需要 `--iops`**;
   给小文件负载限速才有意义。
6. **`--timeout` 到期会杀整棵树**:`TerminateJobObject` 连子进程一起终止,返回 124;
   限速冻结的时间也计入墙钟(合理:用户等的是它结束)。
7. **缓存写限不住又不想换工具 → 用 `--cpu` 当节流阀**:CPU 顶对所有 IO 类型生效
   (含缓存写),实际 IO 速率 ≈ min(磁盘自然速率, CPU 顶折算速率)。
   前提是负载真的吃 CPU(压缩/解压/转码);纯拷贝是 IO 型负载,CPU 顶管不住。
8. **限速生效的判断**:撞顶时任务变慢是预期行为;若完全没慢,先确认负载是直写 IO
   (坑 #1),再看摘要行是否带上了对应限制。

## 5. 退出统计怎么用

正常退出打印三行:退出码 + CPU 时间(user/kernel)、峰值 commit(单进程/整棵树)。
- 退出码 1455 相关错误 → 内存顶被撞(坑 #3)。
- 进程数顶被撞 → 子进程创建失败,退出码来自被拒的子进程。
- IO 没被限住 → 对照坑 #1/#2 排查,不要盲目调大数值。
- `--iops` 会额外打印每块盘「均值/峰值/事务总数」+ 冻结占空比;占空比长期 >90% 说明目标低于该盘背景负载。
- `--timeout` 命中时退出码是 **124**(不是子进程的原退出码),日志会明确写「超时 Xs: 已终止整棵进程树」。
