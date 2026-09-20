package repair_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"streetlight/internal/apperr"
	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/lamp"
	"streetlight/internal/modules/repair"
)

// harness 使用内存数据库装配真实模块, 用于验证跨模块业务流程。
type harness struct {
	lamps   *lamp.Service
	faults  *fault.Service
	repairs *repair.Service
	db      *gorm.DB
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	db, err := gorm.Open(sqlite.Open("file::memory:"), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		NamingStrategy: schema.NamingStrategy{SingularTable: true},
	})
	require.NoError(t, err)

	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	require.NoError(t, db.AutoMigrate(&lamp.Lamp{}, &fault.Fault{}, &fault.FaultTransition{}, &repair.Repair{}))
	require.NoError(t, repair.EnsureIndexes(db))

	lampRepository := lamp.NewRepository(db)
	lampService := lamp.NewService(lampRepository)

	faultRepository := fault.NewRepository(db)
	faultService := fault.NewService(faultRepository, lampService)
	lampService.SetOpenFaultCounter(faultRepository)

	repairRepository := repair.NewRepository(db)
	repairService := repair.NewService(repairRepository, faultService)
	faultService.SetRepairPort(repairService)

	return &harness{lamps: lampService, faults: faultService, repairs: repairService, db: db}
}

func (h *harness) createLamp(t *testing.T, code string) *lamp.Lamp {
	t.Helper()
	entity, err := h.lamps.Create(context.Background(), lamp.CreateRequest{
		Code:     code,
		Name:     "测试灯杆",
		RoadName: "测试路",
		LampType: lamp.LampTypeLED,
	})
	require.NoError(t, err)
	return entity
}

func (h *harness) createFault(t *testing.T, lampID uint, description string) *fault.Fault {
	t.Helper()
	entity, err := h.faults.Create(context.Background(), fault.CreateRequest{
		LampID:      lampID,
		FaultType:   "灯不亮",
		FaultLevel:  fault.LevelHigh,
		Source:      fault.SourceInspection,
		Description: description,
		Reporter:    "巡检员",
	})
	require.NoError(t, err)
	return entity
}

// requireConflict 断言错误是 409 业务冲突。
func requireConflict(t *testing.T, err error) {
	t.Helper()
	requireStatus(t, err, http.StatusConflict)
}

// requireStatus 断言错误是指定 HTTP 状态的业务错误。
func requireStatus(t *testing.T, err error, status int) {
	t.Helper()
	require.Error(t, err)
	businessErr, ok := apperr.As(err)
	require.True(t, ok, "期望业务错误, 实际: %v", err)
	require.Equal(t, status, businessErr.Status, "错误信息: %s", businessErr.Message)
}

func TestFaultRepairLifecycle(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-001")

	entity := h.createFault(t, device.ID, "整灯不亮, 疑似驱动电源故障")
	require.Equal(t, fault.StatusPending, entity.Status)
	require.Regexp(t, `^GD\d{8}\d{4}$`, entity.FaultNo)

	afterReport, err := h.lamps.Get(ctx, device.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusFault, afterReport.RunStatus, "登记故障后路灯应变为故障状态")

	// 同一盏路灯不允许存在多条未闭环故障
	_, err = h.faults.Create(ctx, fault.CreateRequest{
		LampID: device.ID, FaultType: "灯不亮", Description: "重复登记",
	})
	requireConflict(t, err)

	// 维修开工
	record, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工甲", RepairTeam: "市政照明一班",
	})
	require.NoError(t, err)
	require.Equal(t, repair.StatusOngoing, record.Status)
	require.Regexp(t, `^WX\d{8}\d{4}$`, record.RepairNo)

	faultAfterStart, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, fault.StatusProcessing, faultAfterStart.Status)
	require.Equal(t, 1, faultAfterStart.RepairCount)
	require.NotNil(t, faultAfterStart.LatestRepairID)
	require.Equal(t, record.ID, *faultAfterStart.LatestRepairID)

	lampAfterStart, err := h.lamps.Get(ctx, device.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusMaintenance, lampAfterStart.RunStatus)

	// 同一故障不允许并行开工
	_, err = h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工乙"})
	requireConflict(t, err)

	// 完工且结果为已修复
	cost := 210.0
	finished, err := h.repairs.Finish(ctx, record.ID, repair.FinishRequest{
		Result: repair.ResultFixed, Content: "更换驱动电源", Cost: &cost,
	})
	require.NoError(t, err)
	require.Equal(t, repair.StatusFinished, finished.Status)
	require.NotNil(t, finished.FinishedAt)

	faultAfterFinish, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, fault.StatusRepaired, faultAfterFinish.Status)

	lampAfterFinish, err := h.lamps.Get(ctx, device.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusNormal, lampAfterFinish.RunStatus, "修复后路灯应恢复为正常")

	// 关闭故障形成闭环
	closed, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "现场复核通过"})
	require.NoError(t, err)
	require.Equal(t, fault.StatusClosed, closed.Status)
	require.NotNil(t, closed.ClosedAt)

	// 已产生的维修记录使故障不可删除
	requireConflict(t, h.faults.Delete(ctx, entity.ID))
	// 已关闭故障不允许再次关闭
	_, err = h.faults.Close(ctx, entity.ID, fault.CloseRequest{})
	requireConflict(t, err)
}

func TestRepairPendingPartsKeepsFaultProcessing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-002")
	entity := h.createFault(t, device.ID, "线路老化需要更换电缆")

	record, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工丙",
	})
	require.NoError(t, err)

	_, err = h.repairs.Finish(ctx, record.ID, repair.FinishRequest{Result: repair.ResultPendingParts})
	require.NoError(t, err)

	faultAfterFinish, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, fault.StatusProcessing, faultAfterFinish.Status, "非已修复结果不应结束故障")

	lampAfterFinish, err := h.lamps.Get(ctx, device.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusMaintenance, lampAfterFinish.RunStatus)

	// 可继续登记第二次维修(返修)
	second, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工丙", Content: "物料到场后更换电缆",
	})
	require.NoError(t, err)
	require.Equal(t, repair.StatusOngoing, second.Status)

	faultAfterSecond, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, 2, faultAfterSecond.RepairCount)
}

func TestRepairRejectedOnClosedFault(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-003")
	entity := h.createFault(t, device.ID, "误报故障需要作废")

	_, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "误报作废"})
	require.NoError(t, err)

	_, err = h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工丁"})
	requireConflict(t, err)
}

func TestFaultValidation(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-004")

	// 非法故障类型
	_, err := h.faults.Create(ctx, fault.CreateRequest{
		LampID: device.ID, FaultType: "不存在的类型", Description: "测试",
	})
	businessErr, ok := apperr.As(err)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, businessErr.Status)

	// 路灯不存在
	_, err = h.faults.Create(ctx, fault.CreateRequest{
		LampID: 99999, FaultType: "灯不亮", Description: "测试",
	})
	businessErr, ok = apperr.As(err)
	require.True(t, ok)
	require.Equal(t, http.StatusNotFound, businessErr.Status)

	// 重复路灯编号
	_, err = h.lamps.Create(ctx, lamp.CreateRequest{
		Code: device.Code, RoadName: "测试路", LampType: lamp.LampTypeLED,
	})
	requireConflict(t, err)

	// 存在未闭环故障时不允许删除路灯
	h.createFault(t, device.ID, "删除校验")
	requireConflict(t, h.lamps.Delete(ctx, device.ID))
}

// TestReturnToPendingLifecycle 验证退回待处理的完整链路:
// 故障回落待处理、在办维修中止、路灯状态回落、轨迹记录操作人与理由、退回后可重新开工。
func TestReturnToPendingLifecycle(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-101")
	entity := h.createFault(t, device.ID, "上报为灯具破损")

	record, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工甲",
	})
	require.NoError(t, err)

	lampInRepair, err := h.lamps.Get(ctx, device.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusMaintenance, lampInRepair.RunStatus)

	// 维修过程中发现判断错误, 退回待处理
	returned, err := h.faults.Return(ctx, entity.ID, fault.ReturnRequest{
		Operator: "调度员小李", Reason: "现场核查为线路故障, 原判断灯具破损有误",
	})
	require.NoError(t, err)
	require.Equal(t, fault.StatusPending, returned.Status)

	// 在办维修记录被中止为已退回, 并保留退回原因
	aborted, err := h.repairs.Get(ctx, record.ID)
	require.NoError(t, err)
	require.Equal(t, repair.StatusReturned, aborted.Status)
	require.Equal(t, "现场核查为线路故障, 原判断灯具破损有误", aborted.ReturnReason)

	// 路灯运行状态从维修中回落到故障
	lampAfterReturn, err := h.lamps.Get(ctx, device.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusFault, lampAfterReturn.RunStatus)

	// 处置轨迹按时间保留每次流转的操作人和理由
	transitions, err := h.faults.ListTransitions(ctx, entity.ID)
	require.NoError(t, err)
	require.Len(t, transitions, 3)
	require.Equal(t, fault.ActionReported, transitions[0].Action)
	require.Equal(t, "巡检员", transitions[0].Operator)
	require.Equal(t, fault.ActionRepairStarted, transitions[1].Action)
	require.Equal(t, "维修工甲", transitions[1].Operator)
	require.Equal(t, fault.ActionReturned, transitions[2].Action)
	require.Equal(t, "调度员小李", transitions[2].Operator)
	require.Equal(t, "现场核查为线路故障, 原判断灯具破损有误", transitions[2].Reason)
	require.Equal(t, fault.StatusProcessing, transitions[2].FromStatus)
	require.Equal(t, fault.StatusPending, transitions[2].ToStatus)

	// 退回后可以重新开工, 同一盏灯仍然只有一条在办维修
	second, err := h.repairs.Create(ctx, repair.CreateRequest{
		FaultID: entity.ID, Repairman: "维修工乙",
	})
	require.NoError(t, err)
	require.Equal(t, repair.StatusOngoing, second.Status)

	faultAfterRestart, err := h.faults.GetByID(ctx, entity.ID)
	require.NoError(t, err)
	require.Equal(t, fault.StatusProcessing, faultAfterRestart.Status)
	require.Equal(t, 2, faultAfterRestart.RepairCount)

	lampAfterRestart, err := h.lamps.Get(ctx, device.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusMaintenance, lampAfterRestart.RunStatus)
}

// TestReturnRejectedCases 验证退回的边界: 待处理无需退回、已关闭不再参与、操作人与原因必填。
func TestReturnRejectedCases(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-102")

	// 待处理状态无需退回
	entity := h.createFault(t, device.ID, "待处理故障")
	_, err := h.faults.Return(ctx, entity.ID, fault.ReturnRequest{Operator: "调度员", Reason: "误判"})
	requireConflict(t, err)

	// 已关闭的故障不再参与退回
	_, err = h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "误报作废"})
	require.NoError(t, err)
	_, err = h.faults.Return(ctx, entity.ID, fault.ReturnRequest{Operator: "调度员", Reason: "误判"})
	requireConflict(t, err)

	// 退回必须填写操作人和原因(故障需处于可退回状态)
	another := h.createFault(t, device.ID, "另一个故障")
	record, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: another.ID, Repairman: "维修工甲"})
	require.NoError(t, err)
	_, err = h.faults.Return(ctx, another.ID, fault.ReturnRequest{Operator: "", Reason: "误判"})
	requireStatus(t, err, http.StatusBadRequest)
	_, err = h.faults.Return(ctx, another.ID, fault.ReturnRequest{Operator: "调度员", Reason: "  "})
	requireStatus(t, err, http.StatusBadRequest)

	// 已修复的故障也允许退回待处理
	_, err = h.repairs.Finish(ctx, record.ID, repair.FinishRequest{Result: repair.ResultFixed})
	require.NoError(t, err)
	returned, err := h.faults.Return(ctx, another.ID, fault.ReturnRequest{Operator: "调度员", Reason: "复核未通过"})
	require.NoError(t, err)
	require.Equal(t, fault.StatusPending, returned.Status)
}

// TestDuplicateReturnConflict 验证重复退回只有一人生效(并发退回的串行等价)。
func TestDuplicateReturnConflict(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-103")
	entity := h.createFault(t, device.ID, "故障")

	_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
	require.NoError(t, err)

	_, err = h.faults.Return(ctx, entity.ID, fault.ReturnRequest{Operator: "调度员甲", Reason: "判断有误"})
	require.NoError(t, err)

	// 第二次退回不再生效
	_, err = h.faults.Return(ctx, entity.ID, fault.ReturnRequest{Operator: "调度员乙", Reason: "重复操作"})
	requireConflict(t, err)

	// 轨迹中只有一条退回记录
	transitions, err := h.faults.ListTransitions(ctx, entity.ID)
	require.NoError(t, err)
	returned := 0
	for _, item := range transitions {
		if item.Action == fault.ActionReturned {
			returned++
			require.Equal(t, "调度员甲", item.Operator)
		}
	}
	require.Equal(t, 1, returned)
}

// TestOneLampOneOngoingRepairConstraint 验证数据库层"一灯一条在办维修"的硬约束:
// 即使并发写入绕过服务层校验, 唯一索引也只允许一条生效。
func TestOneLampOneOngoingRepairConstraint(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-104")
	entity := h.createFault(t, device.ID, "故障")

	_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
	require.NoError(t, err)

	// 服务层校验: 同一盏灯已有在办维修时直接 409
	_, err = h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工乙"})
	requireConflict(t, err)

	// 模拟并发下绕过服务层校验的写入: 唯一索引兜底, 并映射为 409 冲突
	repo := repair.NewRepository(h.db)
	duplicate := &repair.Repair{
		FaultID: entity.ID, FaultNo: entity.FaultNo,
		LampID: device.ID, LampCode: device.Code,
		Repairman: "维修工乙", StartedAt: time.Now(), Status: repair.StatusOngoing,
	}
	err = repo.CreateWithUniqueNo(ctx, duplicate, "WX"+time.Now().Format("20060102"))
	requireConflict(t, err)
}

// TestCloseRejectedWithOngoingRepair 验证存在在办维修时不允许直接关闭, 退回中止后才可关闭。
func TestCloseRejectedWithOngoingRepair(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-105")
	entity := h.createFault(t, device.ID, "故障")

	_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
	require.NoError(t, err)

	_, err = h.faults.Close(ctx, entity.ID, fault.CloseRequest{Remark: "尝试关闭"})
	requireConflict(t, err)

	// 退回中止在办维修后可以关闭
	_, err = h.faults.Return(ctx, entity.ID, fault.ReturnRequest{Operator: "调度员", Reason: "判断有误"})
	require.NoError(t, err)
	closed, err := h.faults.Close(ctx, entity.ID, fault.CloseRequest{Operator: "调度员", Remark: "作废"})
	require.NoError(t, err)
	require.Equal(t, fault.StatusClosed, closed.Status)

	// 关闭轨迹保留了操作人与说明
	transitions, err := h.faults.ListTransitions(ctx, entity.ID)
	require.NoError(t, err)
	last := transitions[len(transitions)-1]
	require.Equal(t, fault.ActionClosed, last.Action)
	require.Equal(t, "调度员", last.Operator)
	require.Equal(t, "作废", last.Reason)
}

// TestFinishRejectedAfterReturn 验证退回后被中止的维修记录不允许再完工。
func TestFinishRejectedAfterReturn(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-T-106")
	entity := h.createFault(t, device.ID, "故障")

	record, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: entity.ID, Repairman: "维修工甲"})
	require.NoError(t, err)

	_, err = h.faults.Return(ctx, entity.ID, fault.ReturnRequest{Operator: "调度员", Reason: "重新研判"})
	require.NoError(t, err)

	_, err = h.repairs.Finish(ctx, record.ID, repair.FinishRequest{Result: repair.ResultFixed})
	requireConflict(t, err)
}
