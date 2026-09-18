package api

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx/internal/fsx"
	"github.com/k6nfmm7dbr-commits/sbx/internal/policy"
	"github.com/k6nfmm7dbr-commits/sbx/internal/traffic"
)

// 低频接口的短 TTL 缓存（审计项「HTTP API 缓存/节流」）。
//
// 动机：/api/summary 每次调用都要读并解析 nodes.json、全表扫 totals、扫 samples
// 窗口、再扫当日 daily；/api/live 每 2 秒被每个打开的页面轮询一次，同样要读
// nodes.json。多标签页/多设备同时看面板时这些工作被重复做同样的内容。
//
// 设计要点：
//  1. key 里带**数据版本**（采集器最后一次成功采样时间 + 策略快照版本），
//     数据一变 key 就变 → 天然不会返回过期数据，不依赖 TTL 的正确性；
//     TTL 只用于回收不再被访问的条目，防止 map 无界增长。
//  2. 单飞（single-flight）：同一 key 的并发请求只执行一次加载，其余等待复用，
//     避免"缓存击穿"把 N 个并发请求放大成 N 次全表扫描。
//  3. 全部经互斥锁保护，支持整体失效（策略保存/配额重置后立即失效）。
type ttlCache struct {
	ttl time.Duration

	mu       sync.Mutex
	items    map[string]cacheEntry
	inflight map[string]*cacheCall
}

type cacheEntry struct {
	val any
	exp time.Time
}

// cacheCall 是一次进行中的加载；等待者共享同一结果。
type cacheCall struct {
	wg  sync.WaitGroup
	val any
	err error
}

func newTTLCache(ttl time.Duration) *ttlCache {
	return &ttlCache{
		ttl:      ttl,
		items:    map[string]cacheEntry{},
		inflight: map[string]*cacheCall{},
	}
}

// load 返回 key 对应的缓存值；未命中时调用 fn 加载并缓存。
// 同一 key 的并发调用只会执行一次 fn。
func (c *ttlCache) load(key string, fn func() (any, error)) (any, error) {
	now := time.Now()

	c.mu.Lock()
	if e, ok := c.items[key]; ok && now.Before(e.exp) {
		c.mu.Unlock()
		return e.val, nil
	}
	// 命中进行中的加载：等它完成（单飞）
	if call, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		call.wg.Wait()
		return call.val, call.err
	}
	call := &cacheCall{}
	call.wg.Add(1)
	c.inflight[key] = call
	c.mu.Unlock()

	val, err := fn()

	c.mu.Lock()
	call.val, call.err = val, err
	delete(c.inflight, key)
	if err == nil {
		c.items[key] = cacheEntry{val: val, exp: time.Now().Add(c.ttl)}
	}
	// 顺手回收过期条目，避免 map 随 key 变化无界增长
	// （key 含数据版本，采样每 2 秒就换一次 key）。
	for k, e := range c.items {
		if time.Now().After(e.exp) {
			delete(c.items, k)
		}
	}
	c.mu.Unlock()

	call.wg.Done()
	return val, err
}

// invalidate 清空全部缓存（策略保存、配额重置等写操作后调用）。
func (c *ttlCache) invalidate() {
	c.mu.Lock()
	c.items = map[string]cacheEntry{}
	c.mu.Unlock()
}

// size 返回当前缓存条目数（测试/诊断用）。
func (c *ttlCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// cacheTTL 是缓存条目的存活时间。
// 取值与默认采集间隔一致（2s）：即使数据版本没变（例如 CLI 直接改了数据库、
// 面板无从得知），最坏也只陈旧一个采样周期。
const cacheTTL = 2 * time.Second

// dataVersion 返回当前数据版本串：采集器最后一次成功采样时间 + 策略快照版本。
// 任一数据源更新，版本即变化，缓存 key 随之改变。
func (s *Server) dataVersion() string {
	var lastOK int64
	if s.src != nil {
		lastOK = s.src.Snapshot().LastOK
	}
	var polVer uint64
	if s.policy != nil {
		polVer = s.policy.Version()
	}
	return strconv.FormatInt(lastOK, 10) + "." + strconv.FormatUint(polVer, 10)
}

// cacheKey 组装带数据版本的缓存键。
func (s *Server) cacheKey(kind string, parts ...string) string {
	key := kind + "|" + s.dataVersion()
	for _, p := range parts {
		key += "|" + p
	}
	return key
}

// serveCachedJSON 用缓存承载「构建 → 序列化」的完整结果。
//
// 缓存里存的是**最终 JSON 字节**而不是结构体指针，有两个好处：
//   - 命中时连 json.Marshal 都省掉；
//   - 彻底杜绝"缓存对象被后续请求就地修改"这类并发隐患。
func (s *Server) serveCachedJSON(w http.ResponseWriter, r *http.Request, code, key string, build func() (any, error)) {
	v, err := s.cacheFor().load(key, func() (any, error) {
		obj, berr := build()
		if berr != nil {
			return nil, berr
		}
		return fsx.MarshalCompact(obj)
	})
	if err != nil {
		s.failInternal(w, r, code, err)
		return
	}
	b, ok := v.([]byte)
	if !ok {
		s.failInternal(w, r, code, fmt.Errorf("缓存值类型异常: %T", v))
		return
	}
	s.send(w, r, http.StatusOK, "application/json; charset=utf-8", b)
}

// invalidateCache 清空接口缓存（策略保存、配额重置等写操作后调用）。
// 注意：策略版本号变化已能让缓存 key 自动失效，这里是显式的双保险。
func (s *Server) invalidateCache() {
	if s.cacheInst != nil {
		s.cacheInst.invalidate()
	}
}

// 编译期断言：policy 版本号接口必须存在（防止重构时被误删导致缓存永不失效）。
var _ = func(p *policy.Service) uint64 { return p.Version() }

// 编译期断言：采集器快照类型仍提供 LastOK。
var _ = func(st traffic.Status) int64 { return st.LastOK }
