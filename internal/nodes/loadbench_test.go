package nodes

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func benchNodesFile(tb testing.TB, n int) string {
	tb.Helper()
	dir := tb.TempDir()
	p := filepath.Join(dir, "nodes.json")
	var b []byte
	b = append(b, '[')
	for i := 1; i <= n; i++ {
		if i > 1 {
			b = append(b, ',')
		}
		b = append(b, []byte(`{"id":`)...)
		b = append(b, []byte(itoaB(i))...)
		b = append(b, []byte(`,"type":"vless","port":`)...)
		b = append(b, []byte(itoaB(10000+i))...)
		b = append(b, []byte(`,"uuid":"00000000-0000-4000-8000-000000000000","sni":"www.microsoft.com","public_key":"pbkpbkpbkpbkpbkpbkpbkpbkpbkpbkpbkpbkpbkpbkpbk","short_id":"aabbccdd","name":"节点-`)...)
		b = append(b, []byte(itoaB(i))...)
		b = append(b, []byte(`"}`)...)
	}
	b = append(b, ']')
	if err := os.WriteFile(p, b, 0o600); err != nil {
		tb.Fatal(err)
	}
	return p
}

func itoaB(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// BenchmarkLoadPanelNodesStrict 是 reconcile(1s) + collector(2s) 每轮都调用的函数。
func BenchmarkLoadPanelNodesStrict(b *testing.B) {
	for _, n := range []int{5, 50, 200} {
		p := benchNodesFile(b, n)
		b.Run("nodes="+itoaB(n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				list, err := LoadPanelNodesStrict(p)
				if err != nil || len(list) != n {
					b.Fatalf("err=%v n=%d", err, len(list))
				}
			}
		})
	}
}

// BenchmarkLoadPanelNodesTolerant 是 /api/summary 与 /api/live 每次构建都调用的
// 函数（面板开着就每 2~8 秒一次）。它走与严格版同样的 (文件身份, mtime, size)
// 缓存，命中时只剩一次 stat。
func BenchmarkLoadPanelNodesTolerant(b *testing.B) {
	for _, n := range []int{5, 50, 200} {
		p := benchNodesFile(b, n)
		b.Run("nodes="+itoaB(n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				list := LoadPanelNodes(p)
				if len(list) != n {
					b.Fatalf("n=%d", len(list))
				}
			}
		})
	}
}

// BenchmarkLoadPanelNodesTolerantCold 是同一函数的**未命中缓存**成本：每轮用
// os.Chtimes 改 mtime 强制失效（内容不变），等价于优化前每次调用都重新
// read+JSON 解析的行为，用于 A/B 量化缓存收益。
func BenchmarkLoadPanelNodesTolerantCold(b *testing.B) {
	p := benchNodesFile(b, 50)
	base := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := os.Chtimes(p, base.Add(time.Duration(i)*time.Millisecond), base.Add(time.Duration(i)*time.Millisecond)); err != nil {
			b.Fatal(err)
		}
		if list := LoadPanelNodes(p); len(list) != 50 {
			b.Fatalf("n=%d", len(list))
		}
	}
}

// BenchmarkStatOnly 对照：只 stat 文件的成本（mtime 缓存方案的下限）。
func BenchmarkStatOnly(b *testing.B) {
	p := benchNodesFile(b, 50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := os.Stat(p); err != nil {
			b.Fatal(err)
		}
	}
}
