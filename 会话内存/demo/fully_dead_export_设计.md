Fully Dead 站点导出与编译器集成设计
====================================

1. 概述
-------
本文档描述基于 gcdeadtrace 输出的"fully dead"（Freed Only）分配站点，
自动生成编译器配置文件，将特定分配点重定向到 session arena 的完整方案。

2. 数据来源
-----------
excel_report 已从 gcdeadtrace 输出中解析出以下关键区域：

  === Alloc File:Line Only in Freed (fully dead) ===
  | Alloc File:Line         | Freed Objs | Freed Bytes   | #Stacks | Call Stacks |
  | main.go:362             | 20         | 5120          | 3       | main.(*Work... |
  | main.go:484             | 15         | 3840          | 2       | main.(*Work... |

2026-07-15: 只导出 Fully Dead 区域。Both Freed & Alive 暂不处理。

3. 配置格式
-----------
导出的 alloc_sites.cfg 格式采用纯文本 YAML-like 风格，
每条记录 = 源文件:行号 + 类型信息。

```yaml
# alloc_sites.cfg — 自动由 gcdeadtrace → excel_report 生成
# 只包含 "Only in Freed (fully dead)" 的站点

[[session_alloc_sites]]

  # ---- Freed Only (fully dead) ----
  # 这些站点的所有 session 分配对象都已随会话死亡，无残留。
  # 最安全的 arena 候选——整块释放不影响任何存活对象。

  - file: "D:/code/go/go/会话内存/demo/main.go"
    line: 362
    func: "main.(*WorkerActor).patternSession"
    type: "[]uint8"
    size: 256

  - file: "D:/code/go/go/会话内存/demo/main.go"
    line: 484
    func: "main.(*WorkerActor).patternConcurrent.func1"
    type: "[]uint8"
    size: 256

  - file: "D:/code/go/go/会话内存/demo/main.go"
    line: 485
    func: "main.(*WorkerActor).patternConcurrent.func1"
    type: "[]uint8"
    size: 1024

  - file: "D:/code/go/go/会话内存/demo/main.go"
    line: 495
    func: "main.(*WorkerActor).patternConcurrent.func2"
    type: "[]uint8"
    size: 256
```

约束：
- 只导出 type 为 `[]byte` / `[]uint8` / `[N]byte` / `[N]uint8` 等
  noscan 类型的站点（含指针类型需要额外验证，当前不做）
- 每个站点指明 go 源文件和行号（精确到行）
- size 为固定大小时填具体值，非定长填 `variable`

4. 导出流程
-----------
现有工具链扩展：

```
原始数据:
  output/gcdeadtrace_demo_*.txt

现有分析:
  excel_report.go  →  gcdeadtrace_report.xlsx  (人看)
                     ↑ 已有

新增导出:
  excel_report.go  →  alloc_sites.cfg  (编译器吃)
                     ↑ 新增功能
```

excel_report 在遍历 allData 时，对每个 mode 的 cross-reference 区域
"Alloc File:Line Only in Freed (fully dead)" 里的站点条目，
逐条写出到 `output/alloc_sites.cfg`。

5. 编译器侧设计（未来实现，本文档暂不涉及代码）
-----------------------------------------------
编译器改造的入口是读取 `alloc_sites.cfg`，在 walk 阶段
将匹配的 `make([]T, n)` / `new(T)` 替换为 `sessionalloc` 调用。

详细设计见 `TODO` 章节，当前只完成配置导出部分。

6. 验证方案
-----------
验证导出正确性的步骤：

6.1 格式验证
  - 运行 demo 生成 gcdeadtrace 输出
  - 运行 excel_report 生成 alloc_sites.cfg
  - 检查 cfg 中每条记录的 file/line 在源码中确实存在

6.2 完整性验证
  - excel_report xlsx 中 Freed Only 区域的条目数 == cfg 行数
  - cfg 中每条记录的类型列与源码中 match（如 `[]uint8` 对应 `make([]byte, ...)`）

6.3 边界验证
  - 无 fully dead 站点时：输出空 cfg（非错误）
  - 多文件多模式：所有 mode 的 fully dead 合并去重后写入同一 cfg

7. 后续方向（不在此次实现）
---------------------------
- Both Freed & Alive 站点的导出（需要标记 alive 引用，风险更高）
- 类型过滤改进：支持含指针但明确安全的类型
- 跨版本兼容：编译器接口变化时配置格式无需变更
