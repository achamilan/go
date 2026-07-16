# 面向 Session 的手动内存管理技术路线 — PPT 大纲（单页版）

---

## 第 1 页（唯一页）

**标题**：面向 Session 的手动内存管理技术路线（探索方案）

---

### 总览流程

```
① 诊断 → ② 配置 → ③ 替换 → ④ 运行时
```

---

### ① Session 诊断

**思路**：GODEBUG=gcdeadtrace=1 开启运行时追踪，session 分配的对象挂 special，GC 后输出 per-session freed / alive 统计。从中找到 "fully dead"（每次会话结束都释放完）的分配站点作为候选。

**风险**：业务需人工标 session 边界；不同执行路径表现不同，单次采集可能不全。

### ② 配置导出

**思路**：excel_report 解析 gcdeadtrace 输出中的 "Only in Freed" 站点，提取 file:line、type、size、noscan，去重后生成 alloc_sites.cfg。

**风险**：cfg 与行号绑定，改代码后可能失效，需重采。

### ③ 分配替换

**思路**：编译器 Walk 阶段查 cfg 匹配 file:line，将 make/new 替换为 sessionalloc。备选：linkname 拦截 runtime.newobject 运行时切换。

**风险**：方式待定（改编译器 vs linkname）；含指针类型需传递 type 给运行时处理 GC 扫描。

### ④ 运行时 Arena

**思路**：预分配内存池，session 开始从中分配，结束整块释放。关键设计点待定：bump / slab 分配方式、含指针对象的 GC 注册策略、内存大小策略。

**风险**：含指针进 arena 后 GC 扫不到是核心问题；Reset 后外部引用难检测。

### 安全兜底（待定）

- 含指针对象 GC 扫描保障
- cfg 配置错误的编译/运行时检测
- 生产问题定位手段（如内存填 pattern）
- 灰度安全回退机制

---

### 实施阶段

| 阶段 | 内容 | 状态 |
|:---:|------|:---:|
| 1 | 方案设计 | 当前 |
| 2 | 工具链（cfg 导出） | 待实现 |
| 3 | 运行时（arena + 替换） | 待实现 |
| 4 | 验证 + 灰度 | 待实现 |
