# Go 内存分配与回收全流程分析

## 一、核心概念与层次结构

### 1.1 五层内存模型

Go 运行时将内存管理分为五个层次，从低到高：

```
┌─────────────────────────────────────────────────────────────┐
│  层次 5: Object (对象)                                       │
│  mallocgc 返回给用户的指针                                   │
│  大小: 任意 (由 size class 决定)                              │
├─────────────────────────────────────────────────────────────┤
│  层次 4: mspan (跨度)                                        │
│  管理一组连续 page，是分配/释放的基本单位                     │
│  大小: npages × pageSize (通常 8KB)                          │
├─────────────────────────────────────────────────────────────┤
│  层次 3: page (页)                                           │
│  页分配器管理的最小单元                                       │
│  大小: pageSize = 8KB (固定)                                 │
├─────────────────────────────────────────────────────────────┤
│  层次 2: pallocChunk + heapArena                             │
│  页分配器的位图 chunk: 4MB (512 pages)                        │
│  heap arena: 64MB (8192 pages)，存 span 元数据               │
│  两者重叠但不相同: pallocChunk 管位图, heapArena 管元数据     │
├─────────────────────────────────────────────────────────────┤
│  层次 1: 虚拟地址空间 + 物理内存                              │
│  sysReserve/sysMap 向 OS 申请的地址范围                       │
│  physPageSize = 4KB, physHugePageSize = 2MB                  │
└─────────────────────────────────────────────────────────────┘
```

### 1.2 关键尺寸常量

| 常量 | 64-bit Linux | 说明 |
|------|-------------|------|
| `physPageSize` | 4KB | OS 物理页大小 |
| `physHugePageSize` | 2MB | 透明大页 (THP) 大小 |
| `pageSize` | 8KB | Go 运行时页大小 (固定) |
| `pallocChunkBytes` | 4MB | 页分配器位图 chunk (512 pages) |
| `heapArenaBytes` | 64MB | Heap arena 大小 (8192 pages) |
| `pallocChunkPages` | 512 | 每个 chunk 包含的 page 数 |
| `pagesPerArena` | 8192 | 每个 arena 包含的 page 数 |

**关键关系**：
- `heapArenaBytes / pallocChunkBytes = 16`（一个 arena 包含 16 个 chunk）
- `pageSize / physPageSize = 2`（一个 Go page 覆盖 2 个物理页）
- `physHugePageSize / pageSize = 256`（一个大页覆盖 256 个 Go page）

---

## 二、地址空间的三级索引结构

### 2.1 arena 索引：虚拟地址 → heapArena

Go 通过两级映射将虚拟地址映射到 `heapArena` 元数据：

```
虚拟地址 p
    │
    ▼
arenaIndex(p) = (p - arenaBaseOffset) / heapArenaBytes
    │
    ├── arenaIdx.l1() → mheap_.arenas[l1]     (L1 数组, 64-bit Linux 上只有 1 个元素)
    │       │
    │       └── mheap_.arenas[l1][l2] → *heapArena  (L2 数组, 每个元素是一个 arena 的元数据)
    │
    └── arenaIdx.l2() → L2 索引
```

**64-bit Linux 上的配置**（`src/runtime/malloc.go:229-288`）：

```
arenaL1Bits = 0    → L1 只有 1 个桶
arenaL2Bits = 48 - 26 - 0 = 22  → L2 有 4M 个条目 (占用 32MB)
heapArenaBytes = 64MB
```

**Windows 64-bit 上的配置**：

```
arenaL1Bits = 6    → L1 有 64 个桶
arenaL2Bits = 48 - 22 - 6 = 20  → L2 有 1M 个条目 (占用 8MB)
heapArenaBytes = 4MB   (更小的 arena 降低提交成本)
```

### 2.2 chunk 索引：虚拟地址 → pallocData

页分配器使用另一套独立的两级索引来管理位图：

```
虚拟地址 p
    │
    ▼
chunkIndex(p) = (p - arenaBaseOffset) / pallocChunkBytes
    │
    ├── chunkIdx.l1() → pageAlloc.chunks[l1]    (L1 数组)
    │       │
    │       └── pageAlloc.chunks[l1][l2] → pallocData  (512 bits × 4 = 2048 bits = 一个 chunk 的位图)
    │
    └── chunkIdx.l2() → L2 索引
```

**64-bit Linux 上的配置**：

```
pallocChunksL1Bits = 13   → L1 有 8192 个条目
pallocChunksL2Bits = 13   → L2 有 8192 个条目, 每个 L2 条目 = 1 个 pallocData (约 256 字节)
```

### 2.3 arena 与 chunk 的关系

```
64MB heapArena
┌────────────────────────────────────────────────────────────────┐
│  chunk[0]     │  chunk[1]     │  ...  │  chunk[15]              │
│  4MB          │  4MB          │       │  4MB                    │
│  512 pages    │  512 pages    │       │  512 pages              │
└────────────────────────────────────────────────────────────────┘
│                                                                  │
└── 同一个 heapArena 管理这 16 个 chunk 对应的 span 元数据 ──────┘

arena.spans[]: 8192 个 *mspan 指针 (每个 page 一个)
chunk.pallocData: 512 bits 的分配位图 (每个 page 一个 bit)
```

**关键区别**：
- `heapArena.spans[pageIdx]` 告诉你这个 page 属于哪个 `*mspan`
- `pallocData.pallocBits[pageIdx]` 告诉你这个 page 是否被分配（1=已分配, 0=空闲）

---

## 三、mspan 如何在物理内存上分配

### 3.1 完整流程：从 mallocgc 到物理内存

```
用户调用 make([]byte, 100)
  │
  ▼
mallocgc(100, ...)                         // malloc.go:1119
  │
  ▼
  size > 32KB ?
  ├── 否 → mcache.alloc[sizeClass]        // 小对象：从 mcache 获取
  │         │
  │         ├── span 有空闲槽位 → 直接分配对象，返回
  │         └── span 满了 → refill → mcentral.cacheSpan
  │                                │
  │                                ├── 有空闲 span → 返回
  │                                └── 无空闲 span → mcentral.grow
  │                                                   │
  └── 是 → mcache.allocLarge → mheap.allocSpan       │
                                                      │
                                    ◄──────────────────┘
                                    │
                                    ▼
                              mheap.allocSpan(npages)
                                    │
                                    ├── pcache.alloc (每 P 的页面缓存, 无锁)
                                    │     │
                                    │     ├── 命中 → 返回 base 地址
                                    │     └── 为空 → pcache = h.pages.allocToCache()
                                    │                  │
                                    │                  └── 从页分配器取 64 页到缓存
                                    │
                                    ├── h.pages.alloc(npages)      (基数树搜索)
                                    │     │
                                    │     ├── 找到空闲页 → 标记为已分配, 返回 scav 字节数
                                    │     └── 未找到 → h.grow(npages)
                                    │                    │
                                    │                    ▼
                                    │              h.sysAlloc(ask)   (向 OS 申请内存)
                                    │                    │
                                    │                    ├── h.arena.alloc (从预保留空间)
                                    │                    ├── sysReserve (mmap PROT_NONE) → Reserved
                                    │                    └── sysMap (mmap PROT_READ|PROT_WRITE|MAP_FIXED) → Prepared
                                    │
                                    ▼
                              分配完成，得到 base + scav
                                    │
                                    ├── sysUsed(base, nbytes, scav)  → 将 scavenged 页恢复为 Ready
                                    │
                                    ▼
                              h.initSpan(s, base, npages, scav)
                                    │
                                    ├── s.init(base, npages)         → 设置 span 的 startAddr/npages
                                    ├── h.setSpans(base, npages, s)  → 将 span 写入 arena.spans[]
                                    └── arena.pageInUse |= mask      → 标记 arena 位图
```

### 3.2 sysAlloc 详细步骤

当页分配器发现没有足够的空闲页时，`mheap.grow` 调用 `mheap.sysAlloc`：

```
h.sysAlloc(ask)                               // ask 对齐到 pallocChunkBytes (4MB)
  │
  ├── 尝试 1: 从预保留的线性地址分配
  │   h.arena.alloc(n, heapArenaBytes, ...)
  │   成功 → goto mapped
  │
  ├── 尝试 2: 遍历 arenaHints 列表
  │   for each hint:
  │     v = sysReserve(hint.addr, n)          // mmap(addr, n, PROT_NONE, MAP_ANON|MAP_PRIVATE)
  │     成功 → break                          // 状态: None → Reserved
  │     失败 → sysFreeOS(v, n) → 下一个 hint  // munmap
  │
  ├── 尝试 3: 所有 hint 都失败
  │   v, size = sysReserveAligned(nil, n, heapArenaBytes)
  │   // mmap(nil, n+64MB, PROT_NONE, ...) → 然后 trim 掉不对齐的部分 (munmap)
  │
  ▼
mapped:
  │
  ├── 创建 arena 元数据
  │   for each arena in [v, v+size):
  │     l2 = h.arenas[ri.l1()]               // 需要时分配 L2 数组
  │     if l2 == nil:
  │       l2 = sysAllocOS(32MB)              // mmap 匿名内存 → None → Ready
  │       sysHugePage(l2, 32MB)              // MADV_HUGEPAGE (如果 arenasHugePages)
  │     r = new(heapArena)                   // 分配 heapArena 结构体
  │     l2[ri.l2()] = r                      // 注册到 L2 数组
  │
  └── 返回 v, size                            // 内存仍为 Reserved 状态
```

**重要**：此时返回的内存是 `Reserved` 状态（`PROT_NONE`），还不能访问。后续由 `mheap.grow` 调用 `sysMap` 将其转为 `Prepared`。

### 3.3 sysMap：将地址空间从 Reserved → Prepared

```
h.grow() 中:
  │
  ├── sysMap(unsafe.Pointer(v), nBase-v, &gcController.heapReleased, "heap")
  │     │
  │     ├── sysMapOS(v, n)
  │     │   // mmap(v, n, PROT_READ|PROT_WRITE, MAP_ANON|MAP_FIXED|MAP_PRIVATE)
  │     │   // 状态: Reserved → Prepared
  │     │
  │     └── if debug.disablethp != 0:
  │           sysNoHugePageOS(v, n)          // MADV_NOHUGEPAGE
  │
  └── h.pages.grow(v, nBase-v)
        │
        ├── 更新 pageAlloc.start/end (chunk 索引范围)
        ├── 扩展 summary 数组 (基数树各层)
        ├── sysMap 需要的 chunk L2 数组
        │   if chunkHugePages:
        │     sysHugePage(l2, size)          // MADV_HUGEPAGE
        └── 更新 scavengeIndex (chunks/grow)
```

### 3.4 initSpan：将 span 注册到 arena 元数据

```go
// mheap.go:1430
func (h *mheap) initSpan(s *mspan, typ spanAllocType, spanclass spanClass,
                          base, npages, scav uintptr) {
    s.init(base, npages)               // 填充 span 基本字段
    // ... 设置 size class, elemsize, nelems, allocBits, gcmarkBits ...
    s.state.set(mSpanInUse)            // 原子发布屏障

    // 将 span 指针写入每个对应 page 的 arena.spans[] 条目
    h.setSpans(s.base(), npages, s)    // ★ 关键步骤

    // 标记 arena.pageInUse 位图
    arena, pageIdx, pageMask := pageIndexOf(s.base())
    atomic.Or8(&arena.pageInUse[pageIdx], pageMask)
}
```

### 3.5 setSpans：span → arena 的反向映射

```go
// mheap.go:1039
func (h *mheap) setSpans(base, npage uintptr, s *mspan) {
    p := base / pageSize                       // 全局页号
    ai := arenaIndex(base)                     // arena 索引
    ha := h.arenas[ai.l1()][ai.l2()]          // 定位到 heapArena
    for n := uintptr(0); n < npage; n++ {
        i := (p + n) % pagesPerArena           // arena 内的页偏移
        if i == 0 {                            // 跨越 arena 边界
            ai = arenaIndex(base + n*pageSize)
            ha = h.arenas[ai.l1()][ai.l2()]
        }
        ha.spans[i] = s                        // ★ 将 span 指针写入每个 page
    }
}
```

这意味着：给定任意堆地址，可以通过 `spanOf(p)` 在 O(1) 时间内找到它属于哪个 span：

```go
// mheap.go:676
func spanOf(p uintptr) *mspan {
    ai := arenaIndex(p)
    ha := mheap_.arenas[ai.l1()][ai.l2()]
    return ha.spans[(p / pageSize) % pagesPerArena]  // 直接数组索引
}
```

### 3.6 物理内存的实际提交

**关键点**：Go 运行时通过 `mmap` 获得的是**虚拟地址空间**，物理内存的提交是**惰性**的。

```
时间线:
  1. sysReserve → mmap(PROT_NONE)         虚拟地址已分配, 物理内存未提交
  2. sysMap     → mmap(PROT_READ|PROT_WRITE, MAP_FIXED)
                 虚拟地址可访问, 物理内存仍未提交 (demand paging)
  3. 首次访问   → page fault → OS 分配物理页 (匿名页, 零填充)
```

所以：**Go 不会主动"申请物理内存"**。物理内存的提交发生在用户代码首次读写对应地址时，由内核通过缺页处理透明完成。

---

## 四、从对象释放到物理内存回收

### 4.1 完整流程

```
GC 标记阶段
  │
  ▼
GC 扫描阶段 (sweep)
  │
  ├── sweepLocked.sweep(span)                 // mgcsweep.go:505
  │     │
  │     ├── 扫描 span 中每个对象的 mark bit
  │     ├── 将未标记的对象回收到 free list
  │     ├── 更新 allocCount
  │     │
  │     └── allocCount == 0 ?  (span 完全空闲)
  │           │
  │           ├── 是 → mheap_.freeSpan(s)     // mgcsweep.go:788
  │           │         │
  │           │         ▼
  │           │       freeSpanLocked            // mheap.go:1721
  │           │         │
  │           │         ├── arena.pageInUse &= ^mask    (清除 in-use 位)
  │           │         ├── h.pages.free(base, npages)  (标记 page 为空闲)
  │           │         │     │
  │           │         │     ├── pallocBits 清零       (位图: 1→0)
  │           │         │     ├── 更新基数树 summary
  │           │         │     └── scavengeIndex.free()  (更新 scavenge 索引)
  │           │         │
  │           │         └── s.state = mSpanDead         (释放 span 结构体)
  │           │
  │           └── 否 → span 仍在使用, 等待下次 GC
  │
  ▼
此时: 页面已经回到页分配器的空闲池中
      但物理内存仍然占用 RSS — 页面未被 scavenge!
```

### 4.2 Scavenger：物理内存真正归还 OS

```
后台 scavenger goroutine (bgscavenge)
  │
  ├── 循环:
  │   ├── scavenger.run()
  │   │     │
  │   │     ├── for workTime < 1ms:
  │   │     │     ├── shouldStop()? → break  (目标已达成)
  │   │     │     └── s.scavenge(64KB)       (每次 64KB)
  │   │     │
  │   │     └── return (released bytes, workTime)
  │   │
  │   ├── released == 0? → park (休眠, 等待唤醒)
  │   └── released > 0?  → sleep(workTime * sleepRatio)  (PI 控制器决定)
  │
  └── 被唤醒条件:
      ├── 新分配触发了 allocSpan
      ├── GC 完成更新了 scavenge 目标
      └── 定时器超时

scavenge(64KB)
  │
  └── p.scavengeOne(ci, pageIdx, max)
        │
        ├── findScavengeCandidate(searchIdx, minPages, maxPages)
        │     │
        │     ├── 从高地址向低地址搜索 (在 pallocData 内部)
        │     ├── 找连续的 "空闲且未 scavenged" 页面
        │     └── 大页保护: 如果候选跨越了大页边界,
        │         将 start 向下取整到大页边界 (全要或全不要)
        │
        ├── p.chunkOf(ci).allocRange(base, npages)   // 临时标记为"已分配"
        │                                               防止并发分配器抢走
        │
        ├── sysUnused(addr, npages*pageSize)         // ★ 归还物理内存
        │     │
        │     └── sysUnusedOS(v, n) in mem_linux.go:
        │           1. 尝试 MADV_FREE → 成功
        │           2. 失败 → MADV_DONTNEED
        │           3. 失败 → mmap 重新映射
        │           4. debug.harddecommit? → mmap(PROT_NONE)
        │
        ├── 更新统计: heapReleased↑, heapFree↓, committed↓, released↑
        │
        └── p.chunkOf(ci).free(base, npages)         // "释放"回来(标记为 scavenged)
              │
              ├── pallocBits 保持为 0 (仍是空闲)
              ├── scavenged 位图标记为 1 (已 scavenged)
              └── 这样重新分配时知道这些页是 scavenged 的,
                  需要 sysUsed 后再用

再次分配 scavenged 页面时:
  allocSpan → 发现 scav > 0
    → sysUsed(addr, nbytes, scav)   // Prepared → Ready
    → gcController.heapReleased -= scav
    → committed += scav, released -= scav
```

### 4.3 scavenge 之后的三种物理内存状态

```
分配前 (空闲, 未 scavenged):
  │  虚拟地址已映射 (PROT_READ|PROT_WRITE)
  │  物理内存已提交 (可能有数据)
  │  RSS 计入
  │  重新分配无需 page fault (快!)

scavenge 后 (MADV_FREE):
  │  虚拟地址仍映射 (PROT_READ|PROT_WRITE)
  │  物理内存标记为"可回收"
  │  RSS 不立即下降 (内核在压力下才回收)
  │  重新分配无需 page fault (数据可能还在)

scavenge 后 (MADV_DONTNEED):
  │  虚拟地址仍映射 (PROT_READ|PROT_WRITE)
  │  物理内存已释放
  │  RSS 立即下降
  │  重新分配触发 page fault → 获得零页
```

---

## 五、大页 (THP) 的完整交互

### 5.1 大页相关的配置路径

```
                    ┌─── sysMapOS ─── debug.disablethp=1 ? ─── MADV_NOHUGEPAGE
                    │                                          (堆内存禁用大页)
                    │
                    ├─── enableMetadataHugePages (堆 ≥ 1GB 时启用)
                    │     ├── chunks L2 数组 → sysHugePage (MADV_HUGEPAGE)
                    │     └── arena L2 数组   → sysHugePage (MADV_HUGEPAGE)
                    │
Go 运行时 ──────────┼─── scavenger 大页保护
                    │     ├── 密度启发式: 密集 chunk 在 2 个 GC 周期内不 scavenge
                    │     └── 候选对齐: scavenge 区域对齐到大页边界
                    │
                    ├─── 分配时 (已禁用, 曾被注释掉)
                    │     └── scavengeIndex.alloc → MADV_COLLAPSE
                    │
                    └─── 用户可控配置
                          GODEBUG=disablethp=1    禁用堆 THP
                          GODEBUG=madvdontneed=1  使用 MADV_DONTNEED
```

### 5.2 大页保护的逐页逻辑

```go
// mgcscavenge.go:962-987
// findScavengeCandidate 中的大页保护

if physHugePageSize > pageSize && physHugePageSize > physPageSize {
    pagesPerHugePage := physHugePageSize / pageSize  // 2MB / 8KB = 256 页
    hugePageAbove := alignUp(start, pagesPerHugePage)

    if hugePageAbove <= end {
        // 候选区域跨越了大页边界!
        hugePageBelow := alignDown(start, pagesPerHugePage)

        if hugePageBelow >= end-run {
            // 向下扩展, 包含整个大页
            // 确保要么释放整个大页, 要么完全不释放
            size = size + (start - hugePageBelow)
            start = hugePageBelow
        }
    }
}
```

**图示**：

```
大页边界 (每 256 pages / 2MB):
│                 │                 │                 │
│   大页 A        │   大页 B        │   大页 C        │
│   (256 pages)   │   (256 pages)   │   (256 pages)   │
│                 │                 │                 │

假设候选区域 = [start, start+size):

  情况 1: 候选完全在大页 B 内部
    大页 A  │████████████████████│  大页 C
            │  [start..start+sz]│
    → 不做扩展, 正常 scavenge

  情况 2: 候选跨越大页边界
    大页 A  │█████████│██████████████   大页 C
            │    [start....start+size)│
                      ↑ 边界
    → hugePageBelow = 取整到 B 的开头
    → start = hugePageBelow (向下扩展)
    → 结果: 整个大页 B 被 scavenge

  情况 3: 候选已对齐到大页边界
    大页 A  │████████████████████│  大页 C
            │[start........+size)│
    → start 已对齐, 不扩展
```

### 5.3 密度启发式详解

```go
// mgcscavenge.go:1295
func (sc scavChunkData) shouldScavenge(currGen uint32, force bool) bool {
    if sc.isEmpty()       { return false }  // 全部分配或全部已 scavenged
    if force              { return true  }  // 强制模式 (内存限制/freeOSMemory)

    if sc.gen == currGen {
        // 当前代: 需要当前和上一代都不是密集的
        return sc.inUse < scavChunkHiOccPages &&     // 当前 < 96.875%
               sc.lastInUse < scavChunkHiOccPages    // 上一代 < 96.875%
    }
    // 落后一代以上: 只看当前 inUse
    return sc.inUse < scavChunkHiOccPages
}
```

**代际更新** (`scavengeIndex.nextGen()`, 每次 GC 周期调用一次):

```
GC 周期 N:
  gen = 1
  alloc(npages): 如果 sc.gen != 1 → sc.lastInUse = sc.inUse, sc.gen = 1
                 然后 sc.inUse += npages

GC 周期 N+1:
  gen = 2
  因为 sc.gen(=1) != currGen(=2), shouldScavenge 只看 sc.inUse < 96.875%
  不需要 lastInUse 的条件

GC 周期 N+1 之后 alloc:
  sc.gen = 2, sc.lastInUse = 快照的 inUse, sc.inUse += npages
  现在 gen == currGen, shouldScavenge 需要 inUse 和 lastInUse 都 < 96.875%
```

**这意味着**：一个 chunk 要成为 scavenge 候选，必须在**至少两个 GC 周期**中保持非密集。这避免了对频繁使用区域进行 scavenge 从而破坏大页。

---

## 六、完整生命周期时序图

```
时间 ──────────────────────────────────────────────────────────────>

程序的虚拟地址空间 (简化示意):

  ┌────────────────────────────────────────────────────────────────┐
  │ heapArena[0]  64MB              │ heapArena[1]  64MB           │
  │ ┌────┬────┬────┬────┬────┬────┬────┬────┬────┬────┬────┐      │
  │ │chk0│chk1│chk2│... │chk7│chk8│... │chk15│   │    │    │      │
  │ │4MB │4MB │4MB │    │4MB │4MB │    │4MB │   │    │    │      │
  │ └────┴────┴────┴────┴────┴────┴────┴────┴────┴────┴────┘      │
  └────────────────────────────────────────────────────────────────┘
  每个 4MB chunk = 512 pages (每页 8KB)
  每个 chunk 有一个 pallocData (512 bits = 分配位图)

程序启动时:
  ├── sysReserve: 预保留大段地址空间 (PROT_NONE)
  └── arena.spans[] = nil (所有页面尚未使用)

第一次分配 (例如 make([]byte, 1MB)):
  ├── mheap.allocSpan(npages=128, 即 1MB/8KB)
  │     ├── h.pages.alloc(128) → 无空闲页!
  │     └── h.grow(128)
  │           ├── sysMap(chunk[0]): mmap(PROT_READ|PROT_WRITE)
  │           │   Reserved → Prepared
  │           │   虚拟地址现在可访问
  │           │   物理内存: 2MB (大页对齐) 由内核惰性提交
  │           │
  │           ├── h.pages.grow(v, 4MB)
  │           │   pallocBits[chunk0] = 全 0 (空闲)
  │           │   summary 更新: 该区域最大连续 512 页
  │           │
  │           └── h.pages.alloc(128) 重试 → 成功!
  │                 pallocBits[0..127] = 1
  │                 返回 base=arena[0]+0, scav=0
  │
  ├── sysUsed: 无操作 (scav=0)
  │
  ├── initSpan:
  │     s = new(mspan) 从 fixalloc
  │     s.init(base, 128)
  │       s.startAddr = arena[0] + 0×0
  │       s.npages = 128
  │       s.limit = s.base() + 1MB
  │     h.setSpans(base, 128, s):
  │       arena[0].spans[0] = s
  │       arena[0].spans[1] = s
  │       ...                     (每个 page 都指向 s)
  │       arena[0].spans[127] = s
  │
  └── 返回给用户: &(span[0])
       用户写入数据 → page fault → 内核分配物理页

GC 后 span 完全空闲:
  ├── sweep → mheap_.freeSpan(s)
  │     ├── arena[0].pageInUse[0] = 0 (清除)
  │     └── h.pages.free(base, 128)
  │           pallocBits[0..127] = 0 (空闲)
  │           pallocData.scavenged[0..127] = 0 (未 scavenged)
  │           物理内存仍占用 RSS!

后台 scavenger 动作:
  ├── find: shouldScavenge(chunk0)?
  │     inUse = 0 (空闲), lastInUse = 128 (上一代是满的!)
  │     → 本代 gen==currGen: 需要 lastInUse < 96.875%, 但 lastInUse=128>124
  │     → 回答: NO, 不 scavenge
  │
  ├── 等待下一个 GC 周期...
  │     nextGen() → gen++
  │     现在 chunk0: gen=1, currGen=2
  │     → gen != currGen, 只需要 inUse < 96.875%
  │     → inUse=0 < 124 → YES!
  │
  └── scavengeOne(chunk0):
        ├── findScavengeCandidate:
        │    找到连续的 pallocBits=0 && scavenged=0: pages[0..511]
        │    大页保护:
        │      2MB / 8KB = 256 pages 为一个 huge page
        │      start=0 不对齐到 256 → 扩展!
        │      start=0 (已在边界)
        │      但 size=512 > 1 个 huge page, 没关系
        │
        ├── allocRange(0, 512): 临时标记为已分配
        │
        ├── sysUnused(base, 512×8KB=4MB):
        │     madvise(base, 4MB, MADV_FREE)
        │     内核将这 4MB 标记为可回收
        │     包含 2 个 2MB 大页!
        │
        ├── 统计: heapReleased += 4MB, committed -= 4MB
        │
        └── free(0, 512): 释放回来
              pallocBits[0..511] = 0 (空闲)
              pallocData.scavenged[0..511] = 1 (已 scavenged)

此时:
  虚拟地址仍然可访问
  物理内存:
    MADV_FREE → RSS 不立即下降, 内核压力下回收
    MADV_DONTNEED → RSS 立即下降, 再次访问需 page fault

再次分配这些页面:
  allocSpan → 找到 scavenged 页面
    → sysUsed(base, nbytes, scav=4MB)
        在 Linux 上默认是空操作 (demand paging)
    → heapReleased -= 4MB
    → committed += 4MB (重新计入)
    → 用户写入 → page fault → 内核分配新物理页
```

---

## 七、不同配置对比总结

| 配置 | 归还 OS 的方式 | RSS 变化 | 大页行为 | 再次分配成本 |
|------|--------------|----------|----------|-------------|
| **默认** | `MADV_FREE` | 不立即下降 | 密度启发式保护 | 可能无 page fault |
| **`madvdontneed=1`** | `MADV_DONTNEED` | 立即下降 | 密度启发式保护 (同默认) | page fault + 零页 |
| **`disablethp=1`** | 同默认 | 同默认 | 堆内存主动禁用 THP (`MADV_NOHUGEPAGE`) | 同默认 |
| **`harddecommit=1`** | `MADV_DONTNEED` + `mmap(PROT_NONE)` | 立即下降 + 访问 SIGSEGV | 同默认 | `sysUsed` (mmap 恢复权限) |
| **内存限制** | scavenge force=true | 立即 (忽略密度启发式) | 忽略密度保护 | page fault |
| **`debug.FreeOSMemory()`** | scavenge force=true | 立即 (全部空闲页) | 忽略所有保护 | page fault |

---

## 八、关键源码索引

| 文件 | 核心内容 |
|------|----------|
| `src/runtime/mem.go` | 跨平台内存状态抽象 (sysAlloc/sysReserve/sysMap/sysUsed/sysUnused/sysFree) |
| `src/runtime/mem_linux.go` | Linux 实现 (mmap/munmap/MADV_FREE/MADV_DONTNEED/MADV_HUGEPAGE/MADV_NOHUGEPAGE) |
| `src/runtime/malloc.go:233-360` | 地址空间常量定义 (heapArenaBytes, pallocChunkBytes, arenaBits 等) |
| `src/runtime/malloc.go:405-724` | mallocinit (大页检测、地址空间预保留) |
| `src/runtime/malloc.go:741-922` | mheap.sysAlloc (向 OS 申请 arena) |
| `src/runtime/malloc.go:976-1017` | enableMetadataHugePages |
| `src/runtime/mheap.go:64-262` | mheap 结构体 (arenas 两级索引、pageAlloc、curArena) |
| `src/runtime/mheap.go:266-338` | heapArena 结构体 (spans 数组、pageInUse/pageMarks 位图) |
| `src/runtime/mheap.go:422-516` | mspan 结构体 (startAddr, npages, spanclass, allocBits 等) |
| `src/runtime/mheap.go:594-754` | arenaIndex/pageIndexOf/setSpans (地址 → arena → span 映射) |
| `src/runtime/mheap.go:1039-1051` | setSpans (将 span 注册到 arena.spans[]) |
| `src/runtime/mheap.go:1215-1414` | allocSpan (页面分配主函数) |
| `src/runtime/mheap.go:1430-1538` | initSpan (span 初始化 + 注册到 arena) |
| `src/runtime/mheap.go:1540-1654` | mheap.grow (堆增长, sysMap + pages.grow) |
| `src/runtime/mheap.go:1721-1777` | freeSpanLocked (span 释放, pages.free) |
| `src/runtime/mheap.go:1786-1807` | scavengeAll / FreeOSMemory |
| `src/runtime/mpagealloc.go:1-310` | pageAlloc 结构体 (基数树 summary, chunks 两级索引, pallocData) |
| `src/runtime/mcache.go` | 每 P 的小对象缓存 |
| `src/runtime/mcentral.go` | size 类中心缓存 |
| `src/runtime/mpachecache.go` | 每 P 的页面缓存 (pageCache: 64 页) |
| `src/runtime/mgcsweep.go` | GC 扫描 (sweep → freeSpan) |
| `src/runtime/mgcscavenge.go:86-90` | 文件头注释 (scavenger 设计概述) |
| `src/runtime/mgcscavenge.go:167-211` | gcPaceScavenger (scavenge 目标计算) |
| `src/runtime/mgcscavenge.go:576-642` | scavenger.run (后台 scavenger 主循环) |
| `src/runtime/mgcscavenge.go:649-664` | bgscavenge (后台 goroutine) |
| `src/runtime/mgcscavenge.go:675-690` | pageAlloc.scavenge |
| `src/runtime/mgcscavenge.go:732-802` | scavengeOne (scavenge 单次操作, sysUnused 调用) |
| `src/runtime/mgcscavenge.go:894-989` | findScavengeCandidate (大页保护核心逻辑) |
| `src/runtime/mgcscavenge.go:991-1050` | scavengeIndex (scavenge 索引结构) |
| `src/runtime/mgcscavenge.go:1295-1346` | shouldScavenge/alloc/free (密度启发式) |
