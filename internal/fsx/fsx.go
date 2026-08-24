// Package fsutil 提供原子写文件工具（临时文件 + fsync + rename），
// 避免 server 异常导致配置写到一半损坏。
package fsx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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

// WriteFileAtomic 原子写入：写临时文件 → fsync(临时文件) → close → rename →
// fsync(父目录)。目录 fsync 保证 rename 产生的目录项在掉电场景下同样持久化。
// mode 为 0 时用 0644。
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
	// rename 后 fsync 父目录，持久化新的目录项。
	// 个别文件系统对目录 Sync 返回“不支持”（EINVAL/ENOTSUP/EOPNOTSUPP），
	// 这类明确的不支持错误可以忽略；正常 I/O 错误必须上抛。
	if err := syncDir(dir); err != nil {
		return err
	}
	return nil
}

// RenameAtomic 将已就绪的文件原子改名到目标位置（同文件系统内 rename），
// 成功后 fsync 目标父目录以持久化目录项。真实 I/O 错误会原样返回。
func RenameAtomic(oldpath, newpath string) error {
	if err := os.Rename(oldpath, newpath); err != nil {
		return err
	}
	return syncDir(filepath.Dir(newpath))
}

// syncDir 对目录执行 fsync；仅忽略“文件系统明确不支持”类错误。
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("打开父目录失败: %w", err)
	}
	serr := d.Sync()
	cerr := d.Close()
	if serr != nil && !isUnsupportedSync(serr) {
		return fmt.Errorf("目录 fsync 失败: %w", serr)
	}
	if cerr != nil && !isUnsupportedSync(cerr) {
		return fmt.Errorf("目录句柄关闭失败: %w", cerr)
	}
	return nil
}

// isUnsupportedSync 判断是否为“文件系统不支持该操作”类错误。
func isUnsupportedSync(err error) bool {
	return errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EOPNOTSUPP)
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
