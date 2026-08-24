// Package fsutil 提供原子写文件工具（临时文件 + fsync + rename），
// 避免 server 异常导致配置写到一半损坏。
package fsx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// MarshalCompact 序列化为紧凑 JSON，不转义 HTML 字符、不转义非 ASCII，
// 与旧 Python 实现 json.dumps(obj, ensure_ascii=False, separators=(",", ":")) 对齐。
func MarshalCompact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// json.Encoder 结尾会附加一个换行，去掉以对齐 Python 输出
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// MarshalIndent 序列化为缩进 JSON（两空格），对齐
// json.dumps(obj, indent=2, ensure_ascii=False)。键序按 Go 规则排序，
// 与 Python 的插入序不同，但所有消费方均为 JSON 解析器，语义等价。
func MarshalIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// WriteFileAtomic 原子写入：写临时文件 → fsync → rename。mode 为 0 时用 0644。
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	if mode == 0 {
		mode = 0644
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // 成功 rename 后为无害的 no-op

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync 失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("原子替换失败: %w", err)
	}
	return nil
}

// WriteJSONAtomic 以 Python 兼容格式（indent 可选 + 结尾换行）原子写 JSON。
func WriteJSONAtomic(path string, v any, indent bool) error {
	var (
		data []byte
		err  error
	)
	if indent {
		data, err = MarshalIndent(v)
	} else {
		data, err = MarshalCompact(v)
	}
	if err != nil {
		return err
	}
	data = append(data, '\n')
	mode := os.FileMode(0644)
	if st, serr := os.Stat(path); serr == nil {
		mode = st.Mode().Perm() // 保持原权限位
	}
	return WriteFileAtomic(path, data, mode)
}
