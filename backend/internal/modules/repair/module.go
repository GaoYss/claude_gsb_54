package repair

import (
	"fmt"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Module 维修记录模块, 负责维修过程录入与完工闭环。
type Module struct {
	repository *Repository
	service    *Service
	handler    *Handler
}

// New 构造维修记录模块, faults 为故障模块提供的端口实现。
func New(db *gorm.DB, faults FaultPort) *Module {
	repository := NewRepository(db)
	service := NewService(repository, faults)
	return &Module{
		repository: repository,
		service:    service,
		handler:    NewHandler(service),
	}
}

// Service 暴露业务服务, 供故障模块装配在办维修联动端口。
func (m *Module) Service() *Service { return m.service }

// Repository 暴露仓储, 供状态查询模块装配。
func (m *Module) Repository() *Repository { return m.repository }

// Name 实现 module.Module 接口。
func (m *Module) Name() string { return "维修记录" }

// Models 实现 module.Module 接口。
func (m *Module) Models() []any { return []any{&Repair{}} }

// EnsureIndexes 创建 AutoMigrate 无法表达的约束索引, 需在自动迁移完成后调用。
// idx_repair_lamp_ongoing 是"同一盏灯同时最多一条在办维修"的数据库兜底,
// 两人同时开工时只有一人的写入生效, 另一个收到唯一约束冲突。
func EnsureIndexes(db *gorm.DB) error {
	statements := []string{
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_repair_lamp_ongoing ON repair (lamp_id) WHERE status = 'ongoing'",
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("创建维修模块约束索引失败: %w", err)
		}
	}
	return nil
}

// RegisterRoutes 实现 module.Module 接口。
func (m *Module) RegisterRoutes(api *gin.RouterGroup) {
	group := api.Group("/repairs")
	{
		group.GET("", m.handler.List)
		group.POST("", m.handler.Create)
		group.GET("/meta", m.handler.Metadata)
		group.GET("/statistics", m.handler.Statistics)
		group.GET("/fault/:faultId", m.handler.ListByFault)
		group.GET("/:id", m.handler.Get)
		group.PUT("/:id", m.handler.Update)
		group.POST("/:id/finish", m.handler.Finish)
		group.DELETE("/:id", m.handler.Delete)
	}
}
