package web

import _ "embed"

// AdminIndexHTML 是项目目录 web/admin 下的 Provider 管理页，构建时嵌入二进制。
//
//go:embed admin/index.html
var AdminIndexHTML []byte
