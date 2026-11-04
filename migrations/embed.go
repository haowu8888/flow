// Package migrations 将 SQL 迁移脚本嵌入二进制，由 store.Migrate 在启动时执行。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
