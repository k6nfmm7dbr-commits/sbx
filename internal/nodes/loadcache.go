package nodes

import (
	"os"
	"sync"
)

// osStat 抽成变量便于测试注入（模拟 stat 失败/替换竞态）。
var osStat = os.Stat

// panelStrictCacheEntry 缓存一次成功解析的结果，用 (mtime, size) 作为失效判据。
type panelStrictCacheEntry struct {
	mtimeNS int64
	size    int64
	list    []Node
}

// panelStrictCacheT 是 LoadPanelNodesStrict 的进程级缓存（按路径分键）。
//
// 为什么可以缓存：nodes.json 只在人工 add/edit/remove 时经原子 rename 改写，
// 而 reconcile(1Hz)/collector(0.5Hz) 每轮都要读它。解析 50 节点约 340µs 且
// 分配 87KB/1386 次——这是稳态下最大的可省固定开销之一。
//
// 为什么不会读到陈旧数据：
//   - 只在成功解析后写入；任一次损坏/读失败都返回 error 且不缓存；
//   - 命中判据是 (mtimeNS, size) 双字段完全一致，写入必然改变 mtime（纳秒）
//     或 size；原子 rename 保证不会读到半写文件；
//   - 缓存值是只读共享的 []Node，调用方不得改写。
type panelStrictCacheT struct {
	mu sync.RWMutex
	m  map[string]panelStrictCacheEntry
}

var panelStrictCache = &panelStrictCacheT{m: map[string]panelStrictCacheEntry{}}

// lookup 返回路径缓存的 (mtime,size,list)；调用方已 stat 得到当前 (mtime,size)，
// 二者完全一致才算命中。把 stat 交给调用方做，全流程只需 1 次 stat。
func (c *panelStrictCacheT) lookup(path string, mtimeNS, size int64) ([]Node, bool) {
	c.mu.RLock()
	e, ok := c.m[path]
	c.mu.RUnlock()
	if !ok || e.mtimeNS != mtimeNS || e.size != size {
		return nil, false
	}
	return e.list, true
}

func (c *panelStrictCacheT) put(path string, mtimeNS, size int64, list []Node) {
	c.mu.Lock()
	c.m[path] = panelStrictCacheEntry{mtimeNS: mtimeNS, size: size, list: list}
	c.mu.Unlock()
}

// statMtimeSize 返回文件的修改时间（纳秒）与大小。文件不存在或 stat 失败时
// 返回 err，调用方据此走完整读取路径（并处理 NotExist）。
func statMtimeSize(path string) (mtimeNS, size int64, err error) {
	fi, err := osStat(path)
	if err != nil {
		return 0, 0, err
	}
	return fi.ModTime().UnixNano(), fi.Size(), nil
}
