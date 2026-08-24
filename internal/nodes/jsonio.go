package nodes

import (
	"bytes"
	"encoding/json"
	"os"

	"github.com/k6nfmm7dbr-commits/sbx/internal/fsx"
)

// marshalIndentCompact 与 Python json.dumps(obj, indent=2, ensure_ascii=False)
// 对齐（两空格缩进、不转义非 ASCII / HTML 字符），结尾不带换行。
func marshalIndentCompact(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// saveJSONFile 以 Python 兼容格式（indent=2 + 结尾换行）原子写 JSON。
// perm 为最终文件权限，原子替换会强制 chmod 成该值。
// nodes.json/state.json 及候选文件含节点凭据（private_key/password），
// 统一收紧为 0600（读取方均为 root 服务进程，行为无感知）。
func saveJSONFile(path string, v any, perm os.FileMode) error {
	data, err := marshalIndentCompact(v)
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, append(data, '\n'), perm)
}
