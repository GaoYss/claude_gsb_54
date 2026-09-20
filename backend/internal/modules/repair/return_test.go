package repair_test

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"streetlight/internal/apperr"
	"streetlight/internal/database"
	"streetlight/internal/modules/fault"
	"streetlight/internal/modules/lamp"
	"streetlight/internal/modules/repair"
)

// requireBadRequest 断言错误是 400 参数错误。
func requireBadRequest(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	businessErr, ok := apperr.As(err)
	require.True(t, ok, "期望业务错误, 实际: %v", err)
	require.Equal(t, http.StatusBadRequest, businessErr.Status, "错误信息: %s", businessErr.Message)
}

// startRepair 是开工的便捷封装。
func startRepair(t *testing.T, h *harness, faultID uint, repairman string) *repair.Repair {
	t.Helper()
	record, err := h.repairs.Create(context.Background(), repair.CreateRequest{
		FaultID:    faultID,
		Repairman:  repairman,
		RepairTeam: "市政照明一班",
	})
	require.NoError(t, err)
	return record
}

func TestRepairReturnToPending(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-R-001")
	flt := h.createFault(t, device.ID, "误判: 实际为周边施工临时断电")

	record := startRepair(t, h, flt.ID, "维修工甲")

	// 开工后: 故障维修中, 路灯维修中
	processing, err := h.faults.GetByID(ctx, flt.ID)
	require.NoError(t, err)
	require.Equal(t, fault.StatusProcessing, processing.Status)
	lampMaintenance, err := h.lamps.Get(ctx, device.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusMaintenance, lampMaintenance.RunStatus)

	// 退回必须填写原因
	_, err = h.repairs.Return(ctx, record.ID, repair.ReturnRequest{Reason: "   "})
	requireBadRequest(t, err)

	// 退回待处理
	returned, err := h.repairs.Return(ctx, record.ID, repair.ReturnRequest{
		Reason:   "到场复核为外接电源跳闸, 灯具本身无故障, 退回待处理",
		Operator: "班组长钱七",
	})
	require.NoError(t, err)
	require.Equal(t, repair.StatusReturned, returned.Status)
	require.NotNil(t, returned.ReturnedAt)
	require.Equal(t, "班组长钱七", returned.ReturnedBy)
	require.Contains(t, returned.ReturnReason, "外接电源跳闸")

	// 故障回到待处理, 路灯运行状态随故障分布回落为故障
	pending, err := h.faults.GetByID(ctx, flt.ID)
	require.NoError(t, err)
	require.Equal(t, fault.StatusPending, pending.Status)
	require.Equal(t, 1, pending.RepairCount, "退回不减少维修次数, 保留处置过程")
	lampFault, err := h.lamps.Get(ctx, device.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusFault, lampFault.RunStatus, "退回后路灯应由维修中回落为故障")

	// 已退回不允许再次退回, 也不允许完工
	_, err = h.repairs.Return(ctx, record.ID, repair.ReturnRequest{Reason: "再次退回"})
	requireConflict(t, err)
	_, err = h.repairs.Finish(ctx, record.ID, repair.FinishRequest{Result: repair.ResultFixed})
	requireConflict(t, err)

	// 处置轨迹按时间保留每次流转的操作人与理由
	var flows []fault.FaultFlow
	require.NoError(t, h.db.Order("occurred_at ASC, id ASC").Find(&flows).Error)
	require.Len(t, flows, 3)
	require.Equal(t, fault.FlowReported, flows[0].Action)
	require.Equal(t, fault.FlowStarted, flows[1].Action)
	require.Equal(t, "维修工甲", flows[1].Operator)
	require.Equal(t, fault.FlowReturned, flows[2].Action)
	require.Equal(t, "班组长钱七", flows[2].Operator)
	require.Equal(t, fault.StatusProcessing, flows[2].FromStatus)
	require.Equal(t, fault.StatusPending, flows[2].ToStatus)
	require.NotEmpty(t, flows[2].Reason)

	// 退回后可重新开工, 同一盏灯仍只有一条在办维修
	reopened := startRepair(t, h, flt.ID, "维修工乙")
	require.Equal(t, repair.StatusOngoing, reopened.Status)
	require.NotEqual(t, record.ID, reopened.ID)
	processing2, err := h.faults.GetByID(ctx, flt.ID)
	require.NoError(t, err)
	require.Equal(t, fault.StatusProcessing, processing2.Status)
	require.Equal(t, 2, processing2.RepairCount)
	lampMaintenance2, err := h.lamps.Get(ctx, device.ID)
	require.NoError(t, err)
	require.Equal(t, lamp.RunStatusMaintenance, lampMaintenance2.RunStatus)
}

func TestReturnBlockedOnClosedFault(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-R-002")
	flt := h.createFault(t, device.ID, "修复后闭环")

	record := startRepair(t, h, flt.ID, "维修工甲")
	_, err := h.repairs.Finish(ctx, record.ID, repair.FinishRequest{Result: repair.ResultFixed})
	require.NoError(t, err)
	_, err = h.faults.Close(ctx, flt.ID, fault.CloseRequest{Remark: "复核闭环"})
	require.NoError(t, err)

	// 已关闭的故障不再参与退回
	_, err = h.repairs.Return(ctx, record.ID, repair.ReturnRequest{Reason: "尝试退回"})
	requireConflict(t, err)

	// 维修中故障也不允许直接关闭(必须先完工或退回)
	device2 := h.createLamp(t, "LD-R-003")
	flt2 := h.createFault(t, device2.ID, "维修中尝试关闭")
	record2 := startRepair(t, h, flt2.ID, "维修工乙")
	_ = record2
	_, err = h.faults.Close(ctx, flt2.ID, fault.CloseRequest{Remark: "强行关闭"})
	requireConflict(t, err)
}

func TestConcurrentFinishAndReturnOnlyOneWins(t *testing.T) {
	// 使用共享缓存的多连接内存库, 真实模拟两人同时点击"完工"与"退回"。
	db := newConcurrentDB(t)
	h := assembleConcurrentHarness(t, db)
	ctx := context.Background()

	device, err := h.lamps.Create(ctx, lamp.CreateRequest{Code: "LD-R-004", RoadName: "测试路", LampType: lamp.LampTypeLED})
	require.NoError(t, err)
	flt, err := h.faults.Create(ctx, fault.CreateRequest{
		LampID: device.ID, FaultType: "灯不亮", Description: "并发测试", Reporter: "巡检员",
	})
	require.NoError(t, err)
	record := startRepair(t, h, flt.ID, "维修工甲")

	// 第一轮: 完工 vs 退回, 只有一次生效。
	winners := raceFinishAndReturn(t, h, record.ID)
	require.Len(t, winners, 1, "完工与退回并发只能有一次生效")

	// 在办维修必须已离场: 数据库局部唯一索引 + 条件更新双重保证。
	var ongoing int64
	require.NoError(t, db.Model(&repair.Repair{}).
		Where("fault_id = ? AND status = ?", flt.ID, repair.StatusOngoing).Count(&ongoing).Error)
	require.Equal(t, int64(0), ongoing)

	// 无论上一轮胜者是完工(非已修复, 故障仍维修中)还是退回(故障待处理),
	// 都已无在办维修, 可直接重新开工产生第二条在办记录。
	record2 := startRepair(t, h, flt.ID, "维修工乙")

	results := make(chan string, 2)
	race(t, func() {
		if _, e := h.repairs.Return(ctx, record2.ID, repair.ReturnRequest{Reason: "退回人A"}); e == nil {
			results <- "A"
		}
	}, func() {
		if _, e := h.repairs.Return(ctx, record2.ID, repair.ReturnRequest{Reason: "退回人B"}); e == nil {
			results <- "B"
		}
	})
	close(results)
	returnWinners := make([]string, 0, 2)
	for winner := range results {
		returnWinners = append(returnWinners, winner)
	}
	require.Len(t, returnWinners, 1, "两人同时退回只能有一次生效")
}

// newConcurrentDB 构造支持多连接并发写的共享内存 sqlite。
func newConcurrentDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:mem_return_concurrent?mode=memory&cache=shared&_pragma=busy_timeout(8000)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		NamingStrategy: schema.NamingStrategy{SingularTable: true},
	})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	require.NoError(t, db.AutoMigrate(&lamp.Lamp{}, &fault.Fault{}, &repair.Repair{}, &fault.FaultFlow{}))
	require.NoError(t, database.EnsureBusinessIndexes(db))
	return db
}

// assembleConcurrentHarness 在给定连接上装配真实模块。
func assembleConcurrentHarness(t *testing.T, db *gorm.DB) *harness {
	t.Helper()
	lampRepository := lamp.NewRepository(db)
	lampService := lamp.NewService(lampRepository)
	faultRepository := fault.NewRepository(db)
	faultService := fault.NewService(faultRepository, lampService)
	lampService.SetOpenFaultCounter(faultRepository)
	repairRepository := repair.NewRepository(db)
	repairService := repair.NewService(repairRepository, faultService)
	faultService.SetOngoingRepairChecker(repairRepository)
	return &harness{lamps: lampService, faults: faultService, repairs: repairService, db: db}
}

// raceFinishAndReturn 并发触发完工与退回, 返回成功方标识。
func raceFinishAndReturn(t *testing.T, h *harness, repairID uint) []string {
	t.Helper()
	ctx := context.Background()
	results := make(chan string, 2)
	race(t, func() {
		cost := 100.0
		if _, e := h.repairs.Finish(ctx, repairID, repair.FinishRequest{Result: repair.ResultPendingParts, Cost: &cost}); e == nil {
			results <- "finish"
		}
	}, func() {
		if _, e := h.repairs.Return(ctx, repairID, repair.ReturnRequest{Reason: "误判退回"}); e == nil {
			results <- "return"
		}
	})
	close(results)
	winners := make([]string, 0, 2)
	for winner := range results {
		winners = append(winners, winner)
	}
	return winners
}

// race 同时启动两个动作并等待结束(用 start channel 保证最大并发)。
func race(t *testing.T, a, b func()) {
	t.Helper()
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-start; a() }()
	go func() { defer wg.Done(); <-start; b() }()
	close(start)
	wg.Wait()
}

func TestConcurrentStartOnlyOneOngoing(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	device := h.createLamp(t, "LD-R-005")
	flt := h.createFault(t, device.ID, "两人同时开工")

	const workers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	var success int64
	var mu sync.Mutex
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := h.repairs.Create(ctx, repair.CreateRequest{FaultID: flt.ID, Repairman: "抢单维修工"})
			if err == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	// 内存库单连接下开工被串行化, 第二条会因唯一索引/状态条件失败。
	require.Equal(t, int64(1), success, "同一故障只允许一条在办维修")
}

func TestStatisticsExcludesReturnedCost(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)

	device1 := h.createLamp(t, "LD-R-010")
	flt1 := h.createFault(t, device1.ID, "正常完工计入费用")
	r1 := startRepair(t, h, flt1.ID, "维修工甲")
	cost1 := 200.0
	_, err := h.repairs.Finish(ctx, r1.ID, repair.FinishRequest{Result: repair.ResultFixed, Cost: &cost1})
	require.NoError(t, err)

	device2 := h.createLamp(t, "LD-R-011")
	flt2 := h.createFault(t, device2.ID, "误判退回不计费用")
	r2 := startRepair(t, h, flt2.ID, "维修工乙")
	_, err = h.repairs.Return(ctx, r2.ID, repair.ReturnRequest{Reason: "判断有误"})
	require.NoError(t, err)

	stats, err := h.repairs.Statistics(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), stats.FinishedTotal)
	require.Equal(t, int64(1), stats.ReturnedTotal)
	require.Equal(t, 200.0, stats.TotalCost, "退回作废的维修费用不计入合计")
	require.Equal(t, 200.0, stats.AverageCost)
}
