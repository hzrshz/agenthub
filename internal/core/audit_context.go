package core

import (
	"context"
	"strings"
)

// 审计来源 IP 的 context 传递。
//
// 放在 core 而非各业务域：账号域（auth）与节点域（nodes）都要写审计，
// 都需要同一套 key。放 core 可避免 nodes → auth 这类反向依赖。

// auditIPKey 是审计来源 IP 的 context 键。用未导出的空结构体类型，
// 外部包无法构造，因此不可能与其他 context 值串扰。
type auditIPKey struct{}

// WithAuditIP 把请求来源 IP 放入上下文，供后续审计记录使用。
//
// 空 IP 直接返回原 context：本地 CLI（agenthub admin init/reset）没有
// 网络来源，不该往 context 里塞空值。
func WithAuditIP(ctx context.Context, ip string) context.Context {
	trimmed := strings.TrimSpace(ip)
	if trimmed == "" {
		return ctx
	}
	return context.WithValue(ctx, auditIPKey{}, trimmed)
}

// AuditSourceIP 从上下文取来源 IP；没有则返回空串。
//
// 注意这是审计用的来源标识，不是用于任何安全判定——限流键另走
// config.ClientIP（它要考虑可信反代网段），两者的可信度要求不同。
func AuditSourceIP(ctx context.Context) string {
	value, _ := ctx.Value(auditIPKey{}).(string)
	return value
}
