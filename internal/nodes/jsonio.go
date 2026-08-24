package nodes

import (
	"bytes"
	"encoding/json"

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

// SaveNodesFile 原子写 nodes.json（Python 兼容格式）。
func saveJSONFile(path string, v any, perm uint32) error {
	data, err := marshalIndentCompact(v)
	if err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, append(data, '\n'), 0o644)
}
