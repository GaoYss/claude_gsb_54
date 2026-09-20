// Package dbx 提供跨仓储共享的数据库事务辅助。
//
// 业务流转(开工 / 完工 / 退回 / 关闭)往往同时写维修记录、故障状态、
// 路灯运行状态与处置轨迹, 需要落在同一个数据库事务里。
// 通过 context 透传事务句柄, 各模块仓储无需相互引用即可加入同一事务。
package dbx

import (
	"context"

	"gorm.io/gorm"
)

type txKey struct{}

// WithTx 在数据库事务中执行 fn, 事务句柄经由 context 传给仓储层。
// fn 返回错误或 panic 时事务回滚, nil 时提交。
// 若 ctx 已携带事务(外层已开启), 则直接加入外层事务, 不重复开启,
// 提交与回滚由最外层负责。
func WithTx(ctx context.Context, db *gorm.DB, fn func(ctx context.Context) error) error {
	if tx, ok := ctx.Value(txKey{}).(*gorm.DB); ok && tx != nil {
		return fn(ctx)
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return fn(context.WithValue(ctx, txKey{}, tx))
	})
}

// Session 返回 ctx 中携带的事务句柄; 未开启事务时退回默认连接。
func Session(ctx context.Context, db *gorm.DB) *gorm.DB {
	if tx, ok := ctx.Value(txKey{}).(*gorm.DB); ok && tx != nil {
		return tx.WithContext(ctx)
	}
	return db.WithContext(ctx)
}
