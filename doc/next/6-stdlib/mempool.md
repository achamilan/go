## New mempool package

The new [mempool](/pkg/mempool) package provides C-style manual memory
management: [Malloc](/pkg/mempool#Malloc)/[Free](/pkg/mempool#Free) for
small blocks with deterministic reuse, and
[AllocSpan](/pkg/mempool#AllocSpan)/[AllocSpanTyped](/pkg/mempool#AllocSpanTyped)
for large objects whose memory is returned to the operating system
immediately on free.
