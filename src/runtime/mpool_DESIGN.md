# mpool / typed span 设计文档

Go runtime 内的手动内存分配器：C 风格 malloc/free 语义——确定性复用、
按路径分级、空闲内存主动归还。内部实现位于 `src/runtime/mpool*.go`，
公共 API 为标准库包 `src/mempool`（经 linkname 绑定）。

## 0. 外部使用接口

### 0.1 公共 API（src/mempool，用户代码直接使用）

| 公共函数 | 内部实现 | 签名 |
|---|---|---|
| `mempool.Malloc` | `mpMalloc` | `func(size uintptr) unsafe.Pointer` |
| `mempool.Calloc` | `mpCalloc` | `func(n, size uintptr) unsafe.Pointer` |
| `mempool.Realloc` | `mpRealloc` | `func(p unsafe.Pointer, size uintptr) unsafe.Pointer` |
| `mempool.Free` | `mpFree` | `func(p unsafe.Pointer)` |
| `mempool.UsableSize` | `mpUsableSize` | `func(p unsafe.Pointer) uintptr` |
| `mempool.AllocSpan` | `mpAllocLargeNoscan` | `func(size uintptr) unsafe.Pointer` |
| `mempool.FreeSpan` | `mpFreeLargeNoscan` | `func(p unsafe.Pointer)` |
| `mempool.AllocSpanTyped[T]` | `mpTypedAllocLarge`（T 无指针时自动降级 `mpAllocLargeNoscan`） | `func() *T` |
| `mempool.FreeSpanTyped[T]` | `mpTypedFreeLarge` | `func(p *T)` |

另有 `export_test.go` 钩子（`runtime.MPMalloc` 等）供 runtime 包自身测试使用，
签名与语义同上；观测钩子：`MPChunkCount()`（当前 chunk 数）、
`MPFlushAllPs()`（冲刷 per-P 缓存回 central）。

### 0.2 调用约定（C 语义，违反即未定义行为 / debug 版 panic 或 throw）

- `Malloc`：复用块**不清零**；`size==0` 按 1 处理；返回 8B 对齐指针。
  **内存是 noscan 的，不得存放 Go 指针**（指向的对象会被 GC 回收）。
- `Calloc`：同 Malloc 但清零。
- `Free(nil)` 是 no-op；Free 后再访问 / 重复 Free / Free 非本包指针，未定义行为
  （`mpDebug=true` 构建下 throw）。
- `Realloc`：保留 min(旧,新) 内容；`Realloc(nil, size)` ≡ `Malloc(size)`。
- `AllocSpanTyped[T]`：一对象一 span；**T 含指针**时内存按 T 的位图被 GC 扫描
  （可含 Go 指针），**T 不含指针**时自动走 noscan 路径（免位图保留区与写入，
  仿 `userArenaNextFree` 的 `typ.Pointers()` 分派）；返回的内存已清零。
  Free 后访问**会段错误**（sysFault 的 fault 页，这是特性）。
- `AllocSpan`：一对象一 span，语义同上但 **noscan，不得存指针**；
  适用于 >1KB 的纯字节数据；同样已清零、Free 后访问会段错误。

### 0.3 使用示例

```go
import "mempool"

// 字节层
p := mempool.Malloc(128)
unsafe.Slice((*byte)(p), 128)[0] = 1
mempool.Free(p)

// typed span（含指针对象）
type Node struct{ next *Node; pad [64 << 10]byte }
n := mempool.AllocSpanTyped[Node]()
n.next = &Node{}          // 安全：GC 会扫描
mempool.FreeSpanTyped(n)

// noscan span（纯字节大 buffer）
b := mempool.AllocSpan(1 << 20)
unsafe.Slice((*byte)(b), 1<<20)[100] = 1
mempool.FreeSpan(b)
```

### 0.4 公共包的 linkname 机制

标准库包 `src/mempool` 经 `//go:linkname` 绑定 runtime 内部函数
（runtime 侧包装在 `mpoollink.go`，发射符号名 `mempool.runtime_mempool_*`；
公共函数是普通包装，与 `src/arena` 同款握手；`AllocSpanTyped[T]` 内部用
`abi.TypeOf((*T)(nil)).Elem()` 取类型描述符，并按 `t.Pointers()` 在
typed/noscan 两条分配路径间分派）。

登记文件：`src/go/build/deps_test.go`（`internal/abi, unsafe < mempool;`）、
`api/next/mempool.txt`、`doc/next/6-stdlib/mempool.md`。
测试：`src/mempool/mempool_test.go`（公共路径端到端，含 typed span 指针保活）。

## 1. 三层体系总览

| 层 | 公共 API | 指针安全 | 释放路径 | 适用 |
|---|---|---|---|---|
| 字节 mpool | `Malloc/Calloc/Realloc/Free/UsableSize` | ❌ noscan | 复用；chunk 全空 → GC/scavenger | 高频小对象 ≤32KB |
| typed span | `AllocSpanTyped[T]/FreeSpanTyped[T]` | ✅ GC 位图 | **Free 立即 sysFault** | 大对象、含指针 |
| noscan span | `AllocSpan/FreeSpan` | ❌ noscan | **Free 立即 sysFault** | 大对象、纯字节 |
| 小对象含指针 | （用户侧 sync.Pool 即可） | ✅ | GC | 常规对象 |

大对象定义：**> 32KB**（= `maxSmallSize`，与 runtime 一致）。32KB 是分界线的原因：
span 页粒度下，更大对象切 size class 的碎片和管理成本超过复用收益。

## 2. 字节层 mpool（mpool.go / mpoolclass.go / mpoolcentral.go / mpoolarena.go）

### 2.1 架构

```
per-P 缓存（p.mpcache，acquirem 钉住，无锁快路径）
  → per-class central（一把锁/class，批量 32 块交换）
  → arena（4MB chunk，单 class 服务，基址有序注册表）
```

- size class：34 级，8B 起步每档约 +25%，最大 33904；≤1024 直查表，以上手写二分
- 块 header 8B：`(cls<<1)|1`（最低位 1）；大对象另走 2 幂分桶（64KB~4MB 共 7 桶，
  每桶缓存 8 个，flag=`size<<2` 低两位 00）
- chunk：`mallocgc(4MB, nil, true)` noscan 分配；MemStats 记账自动正确

### 2.2 chunk 全空回收（核心机制）

```go
type mpChunk struct { total, inCentral int32 }
```

- `central.take` → inCentral--；`central.give` → inCentral++
- `inCentral == total` ⟹ 所有块都在 central（不在用户/任何 P 缓存）→ 同临界区内
  剔除空闲表中的块、注销 chunk、断引用 → GC/scavenger 回收
- **临界区纪律**：判定-剔除-注销在同一次 central 锁内完成，take 同锁互斥，
  否则会把已回收内存发出去
- **per-P 残留块**由 GC 解决：`mpFlushAll` 挂在 `gcStart` 的 `clearpools()` 旁
  （STW 段），把每个 P 的缓存冲回 central——等价于 mcache 的 GC 冲刷节奏

### 2.3 runtime 适配要点

- `sync.Mutex` → runtime `mutex` + lockRank 登记（`NONE < mpCentral < mpArena`，
  两锁均在 MALLOC 清单，持锁期间会 mallocgc/append）
- `sync.Pool` 亲和 → 真 per-P（p struct 的 `mpcache` 字段，零值可用）
- `sort` 不可用 → 手写二分；包级 var 初始化表（init 期分配安全的固定数组）
- debug 护栏为 const 门控（`mpDebug`/`mpDebugExt` 翻值重编）：double-free 登记、
  0xDD/0xCD 毒化、尾部 canary；生产零开销

### 2.4 硬约束

chunk/桶内存是 **noscan**：header 只存整数，**用户区禁止存 Go 指针**
（GC 不扫描，指向的对象会被回收）。

## 3. span 层（mpooltyped.go）：一对象一 span，立即释放

typed 与 noscan 共用 `(*mheap).allocUserArenaSpan(npages, noscan)`——
官方 userArena chunk 机制的可变大小泛化：

### 3.1 分配

1. npages 计算：scan 版需为位图预留（spanBytes/64 + dummy type），
   数据区需满足 `spanBytes - spanBytes/64 - sizeof(_type) >= size`，
   即 spanBytes ≥ need×64/63；noscan 版无保留，整 span 可用
2. readyList 精确尺寸复用；否则 `h.sysAlloc` 新分配（尾部余量拆成独立 span 回 readyList）
3. `initSpan` 建模为大对象堆 span；scan 版写 `largeType` 假类型 +
   `userArenaHeapBitsSetType(typ, x, s)` 写 GC 位图
4. GC 相位检查（marktermination 禁入）、assist credit、`gcmarknewobject`、
   sanitizer 钩子、heapLive 记账全套对齐 userArena

### 3.2 释放与延迟 fault 缓存

**缓存路径（热点）**：`mpTypedFreeLarge` 优先把 span 放进 `mpSpanCache`
（容量 32，按 `npages|noscan` 分键），不做任何页操作；同尺寸 Alloc 直接命中——
只剩位图清零 + 对象区 memset + 记账，**~1.6µs/op（对比无缓存 196µs/op，122×）**。

**正确性关键**：缓存的 span 仍是 mSpanInUse 且不被应用引用，sweeper 本应将
其回收（并要求它在 quarantineList 上，否则 throw）——但缓存用 Go map 持有
span 基址（GC 扫描容器），GC 每轮都会标记它，sweep 见到 nalloc>0 即保留，
与 userArena 的 `refs` 保活机制同构。`mpSpanCacheFlush` 在 **gcStart**（STW，
`mpFlushAll` 旁）drain 全部缓存 span 走真正释放，保证新一轮 sweep 之前没有
"活着但无引用"的缓存 span。其它细节：缓存命中跳过 profilealloc（避免
profile bucket 冲突，内存 profiler 对命中分配不可见，属已文档化的近似）。

**真正释放路径**（缓存满 / drain 时）：`freeUserArenaChunk` →
`setUserArenaChunkToFault`：
- GC 不在标记期：立即 `sysFault`——**物理内存马上还 OS**，地址空间保留
- GC 标记期：进 fault 队列，GC 结束时批量 fault
- span 进 quarantine → sweeper 确认无引用后移入 readyList（地址空间回收复用）

对 arena.go 的泛化改动（3 处）：两个固定 8MB 尺寸检查移除、
`allocUserArenaChunk` 的 readyList 跳过非匹配尺寸 span。

### 3.3 实现中踩过的坑（记录在案）

1. **`largeType` 是指针**，指向 span 尾保留区里的假 `_type`——漏初始化即 nil 解引用；
   noscan 版无保留区则必须跳过并置 nil（mspan 结构体复用，防残留指针）
2. **位图保留区按 spanBytes 算**，按 dataSize 算会让 elemsize 小于请求尺寸
3. noscan 版 elemsize=spanBytes 时 `initHeapBits` 的位图区为 0 长度，清零即越界

## 4. 性能数据

微基准（i7-1165G7，并行）：

| 操作 | 耗时 |
|---|---|
| mpMalloc/mpFree (128B) | **4.4 ns/op** |
| 原模块版（sync.Pool 亲和） | 12 ns/op |
| sync.Pool Get/Put | ~4-19 ns/op |
| span alloc+free（缓存命中） | **~1.6 µs/op**（memset + 记账） |
| span alloc+free（缓存未命中/直接释放） | ~196 µs/op（页系统调用） |

高并发压测（32 workers，64KB~2MB 随机，ring 驻留，逐页写入，2s/模式）：

| 模式 | 吞吐 | 峰值 HeapInuse |
|---|---|---|
| **sync.Pool**（2 幂分桶） | **949K ops/s** | 365MB |
| make | 22.4K ops/s | 1102MB（GC 节奏回收） |
| typed span | 8.3K ops/s | **168MB**（≈活集合 128MB） |
| noscan span | 7.1K ops/s | **171MB** |

结论：
- **sync.Pool 在稳态复用负载下吞吐碾压**（桶命中时纯复用，无分配无页操作），
  但峰值内存取决于池内缓存量且 GC 清池后需重建——它是缓存，不是分配契约
- **span 层用页系统调用换内存地板**：峰值即活水位，Free 立即物理释放，
  适合内存敏感/突发型负载
- mpool 字节层用 chunk 高水位换 4ns 级的确定性分配
- 选型：要吞吐选 sync.Pool，要内存地板选 span，要小对象确定性高频选 mpool

## 5. 验证

- 功能测试：`TestMP*`（mpool）+ `TestMPTyped*` + `TestMPNoscan*`，
  含 GC 扫描保活（指针目标跨 GC 存活）、chunk 回收、并发压测
- 公共包测试：`src/mempool/mempool_test.go`（linkname 端到端）
- `TestLockRank`（lockrank 一致性）、`go test go/build`（deps 白名单）
- `go test runtime -short` 全套通过
- debug 护栏（mpDebug=true）单独验证

## 6. 构建与测试命令

```bash
# Windows（git-bash 无法直接跑 make.bat，直接调 dist）
cd src
GO111MODULE=off GOENV=off /d/go1.25.8.windows-amd64/go/bin/go.exe build -o cmd/dist/dist.exe ./cmd/dist
GOROOT='D:\code\go\github_go\go' GOROOT_BOOTSTRAP='D:\go1.25.8.windows-amd64\go' ./cmd/dist/dist.exe bootstrap -a

# 日常迭代（go test 自动重建 runtime）
GOROOT='D:\code\go\github_go\go' GOTOOLCHAIN=local ../bin/go.exe test runtime -run 'TestMP' -v
```

## 7. 文件索引

runtime 内部（`src/runtime/`）：

| 文件 | 职责 |
|---|---|
| `mpool.go` | 字节层入口 API + per-P 快路径 |
| `mpoolclass.go` | size class 表（固定数组，包级 var 生成） |
| `mpoolcentral.go` | central 缓存 + per-P 缓存 + mpFlushAll |
| `mpoolarena.go` | chunk 注册表/引用计数回收 + 大对象桶 |
| `mpooldebug.go` | const 门控 debug 护栏 |
| `mpooltyped.go` | typed/noscan span 分配释放 + allocUserArenaSpan |
| `mpoollink.go` | linkname 包装（对 src/mempool 发射符号） |
| `mpool_test.go` / `mpooltyped_test.go` / `mpoolnoscan_test.go` | runtime 侧测试与压测 |
| `arena.go`（改） | userArena 机制可变大小泛化（3 处） |
| `mgc.go`（改） | gcStart 挂 mpFlushAll |
| `runtime2.go`（改） | p struct 加 mpcache 字段 |
| `mklockrank.go`/`lockrank.go`（改） | mpCentral/mpArena rank 登记 |

公共包与登记：

| 文件 | 职责 |
|---|---|
| `src/mempool/mempool.go` | 公共 API（linkname 绑定 + 泛型 typed span） |
| `src/mempool/mempool_test.go` | 公共路径端到端测试 |
| `src/go/build/deps_test.go`（改） | 依赖白名单登记 |
| `api/next/mempool.txt` | 导出 API 列表 |
| `doc/next/6-stdlib/mempool.md` | 发行说明 |

## 8. 已知限制与后续方向

- 延迟 fault 缓存按精确 npages 命中，随机尺寸负载命中率低（真实 buffer 池
  尺寸聚簇时效果好）；缓存命中对内存 profiler 不可见（近似）
- mpool 字节层 GC 版驻留 = chunk 高水位（arena 语义固有）
- 公共包目前只暴露基础 API；观测接口（ChunkCount 等）仅在 runtime 测试钩子层
- `-race` 需 cgo（本机 Windows 不可用），并发正确性靠压测覆盖
