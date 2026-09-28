package taihuclient

import (
	"context"

	"github.com/liucxer/taihu/internal/cluster"
)

// TLSConfig 兼容导出：cluster.TLSConfig 的别名（CA/Cert/Key）。
type TLSConfig = cluster.TLSConfig

// NewTiKVKV 兼容导出：连 PD 拿 TiKV TxnKV 客户端。
// 供 EFS_nefs b7cb1616 及更早对接面（pkg/object/taihu newClient）使用；
// 新对接面请直接用 NewFromTiKV。
func NewTiKVKV(ctx context.Context, pdAddrs []string, tls TLSConfig) (*cluster.TiKVKV, error) {
	return cluster.NewTiKVKV(ctx, pdAddrs, tls)
}
