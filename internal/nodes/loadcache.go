package nodes

import (
	"os"
	"sync"
)

// osStat 抽成变量便于测试注入（模拟 stat 失败/替换竞态）。
var osStat = os.Stat

// panelNodesCacheEntry 缓存一次成功解析的结果。mtime+size 检测普通写入，
// os.SameFile 检测原子 rename 替换（即使新旧文件恰好同大小且 mtime 相同）。
type panelNodesCacheEntry struct {
	info    os.FileInfo
	mtimeNS int64
	size    int64
	list    []Node
}

// panelNodesCacheT 是按路径分键的进程级解析结果缓存。
//
// 为什么可以缓存：nodes.json 只在人工 add/edit/remove 时经原子 rename 改写，
// 而策略 reconcile(1Hz)、采集器(0.5Hz) 与每个 HTTP 面板请求都要读它。解析
// 50 节点约 340µs 且分配 87KB/1386 次——这是稳态下最大的可省固定开销之一。
//
// 为什么不会读到陈旧数据：
//   - 只在成功解析后写入；任一次损坏/读失败都返回 error（严格版）或 nil
//     （宽松版）且不缓存；
//   - 命中判据为同一文件身份（os.SameFile）+ (mtime,size) 双字段全等；原子 rename
//     必然更换文件身份，即使新旧文件同大小、刻意保留 mtime 也会失效重读；
//     原子写保证不会读到半写文件；
//   - 缓存值是只读共享的 []Node，调用方不得改写。
//
// 严格版与宽松版的解析结果语义不同（严格版拒绝损坏文件，宽松版跳过非法元素），
// 因此各持一份独立缓存，绝不互相复用。
type panelNodesCacheT struct {
	mu sync.RWMutex
	m  map[string]panelNodesCacheEntry
}

func newPanelNodesCache() *panelNodesCacheT {
	return &panelNodesCacheT{m: map[string]panelNodesCacheEntry{}}
}

// lookup 返回路径缓存的解析结果；调用方已 stat 得到当前 FileInfo，必须文件身份、
// mtime、size 三者都匹配才算命中。stat 只执行一次。
func (c *panelNodesCacheT) lookup(path string, fi os.FileInfo) ([]Node, bool) {
	c.mu.RLock()
	e, ok := c.m[path]
	c.mu.RUnlock()
	if !ok || !os.SameFile(e.info, fi) || e.mtimeNS != fi.ModTime().UnixNano() || e.size != fi.Size() {
		return nil, false
	}
	return e.list, true
}

func (c *panelNodesCacheT) put(path string, fi os.FileInfo, list []Node) {
	c.mu.Lock()
	c.m[path] = panelNodesCacheEntry{info: fi, mtimeNS: fi.ModTime().UnixNano(), size: fi.Size(), list: list}
	c.mu.Unlock()
}

// panelStrictCache 服务 LoadPanelNodesStrict（fail-closed 路径）。
var panelStrictCache = newPanelNodesCache()

// panelTolerantCache 服务 LoadPanelNodes（展示路径：/api/summary、/api/live、
// CLI show 等）。它与严格缓存分开存放，因为两者的解析结果不同。
var panelTolerantCache = newPanelNodesCache()
