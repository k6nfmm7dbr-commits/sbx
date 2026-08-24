// Package webui 内嵌面板前端。前端资源随二进制一起发布/升级：
// 升级 sbx-core 即原子更新 UI，磁盘上不再有可被旧文件遮蔽的副本。
// （panel.json 里的 web_root 键保留解析以兼容旧配置，但不再读取。）
package webui

import (
	"embed"
	"io/fs"
)

//go:embed static/index.html static/login.html static/app.js static/style.css
var embedded embed.FS

// FS 返回以 "web/" 为根的前端文件系统。
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		return embedded
	}
	return sub
}
