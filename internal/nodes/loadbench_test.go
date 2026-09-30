package nodes

import (
	"os"
	"path/filepath"
	"testing"
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
