package repair

import (
	"context"
	"strings"
	"time"

	"streetlight/internal/apperr"
	"streetlight/internal/modules/fault"
	"streetlight/pkg/dbx"
	"streetlight/pkg/pagination"
)

// repairSortSpec 定义维修记录列表允许的排序字段白名单。
var repairSortSpec = pagination.SortSpec{
	Allowed: map[string]string{
		"repair_no":   "repair_no",
		"started_at":  "started_at",
		"finished_at": "finished_at",
		"status":      "status",
		"result":      "result",
		"cost":        "cost",
		"created_at":  "created_at",
	},
	Default: "started_at",
}

// FaultPort 由故障登记模块实现, 维修模块通过它联动故障状态、路灯状态与处置轨迹。
type FaultPort interface {
	GetByID(ctx context.Context, id uint) (*fault.Fault, error)
	OnRepairStarted(ctx context.Context, flow fault.RepairFlow) error
	OnRepairFinished(ctx context.Context, flow fault.RepairFlow, fixed bool) error
	OnRepairReturned(ctx context.Context, flow fault.RepairFlow) error
	OnRepairDeleted(ctx context.Context, flow fault.RepairFlow, repairCount int, latestRepairID *uint) error
}

// Service 承载维修记录录入的业务规则。
type Service struct {
	repo   *Repository
	faults FaultPort
}

// NewService 构造维修记录服务。
func NewService(repo *Repository, faults FaultPort) *Service {
	return &Service{repo: repo, faults: faults}
}

// Get 查询维修记录详情。
func (s *Service) Get(ctx context.Context, id uint) (*Repair, error) {
	return s.repo.GetByID(ctx, id)
}

// List 分页查询维修记录。
func (s *Service) List(ctx context.Context, query ListQuery) ([]Repair, int64, pagination.Query, error) {
	page := pagination.Parse(query.Params, repairSortSpec)
	filter, err := buildFilter(query)
	if err != nil {
		return nil, 0, page, err
	}
	items, total, err := s.repo.List(ctx, filter, page)
	if err != nil {
		return nil, 0, page, err
	}
	return items, total, page, nil
}

// ListByFault 查询某条故障的维修过程记录。
func (s *Service) ListByFault(ctx context.Context, faultID uint) ([]Repair, error) {
	if _, err := s.faults.GetByID(ctx, faultID); err != nil {
		return nil, err
	}
	return s.repo.ListByFault(ctx, faultID)
}

// Create 录入维修记录(维修开工), 并联动故障与路灯状态。
// 维修记录落库与故障状态流转在同一事务内完成; 数据库层的在办唯一索引
// 保证同一故障、同一盏路灯同时只有一条在办维修, 两人并发开工只有一笔生效。
func (s *Service) Create(ctx context.Context, req CreateRequest) (*Repair, error) {
	target, err := s.faults.GetByID(ctx, req.FaultID)
	if err != nil {
		return nil, err
	}
	if target.Status == fault.StatusClosed {
		return nil, apperr.Conflict("故障 %s 已关闭, 不允许再登记维修记录", target.FaultNo)
	}
	if target.Status == fault.StatusRepaired {
		return nil, apperr.Conflict("故障 %s 已修复, 如需返修请先登记新的维修记录并重新开工", target.FaultNo)
	}

	ongoing, err := s.repo.GetOngoingByFault(ctx, target.ID)
	if err != nil {
		return nil, err
	}
	if ongoing != nil {
		return nil, apperr.Conflict("故障 %s 已有进行中的维修记录 %s, 请先完成后再录入", target.FaultNo, ongoing.RepairNo)
	}
	ongoingLamp, err := s.repo.CountOngoingByLamp(ctx, target.LampID)
	if err != nil {
		return nil, err
	}
	if ongoingLamp > 0 {
		return nil, apperr.Conflict("路灯 %s 已有进行中的维修记录, 同一盏灯不允许同时挂两条在办维修", target.LampCode)
	}

	repairman := strings.TrimSpace(req.Repairman)
	if repairman == "" {
		return nil, apperr.BadRequest("维修人员不能为空")
	}

	startedAt, err := parseTime(req.StartedAt, time.Now())
	if err != nil {
		return nil, err
	}
	if startedAt.Before(target.ReportedAt) {
		return nil, apperr.BadRequest("开工时间不能早于故障上报时间 %s", target.ReportedAt.Format("2006-01-02 15:04:05"))
	}

	entity := &Repair{
		FaultID:      target.ID,
		FaultNo:      target.FaultNo,
		LampID:       target.LampID,
		LampCode:     target.LampCode,
		Repairman:    repairman,
		RepairTeam:   strings.TrimSpace(req.RepairTeam),
		ContactPhone: strings.TrimSpace(req.ContactPhone),
		StartedAt:    startedAt,
		Status:       StatusOngoing,
		Content:      strings.TrimSpace(req.Content),
		Materials:    strings.TrimSpace(req.Materials),
		Cost:         valueOrZero(req.Cost),
		Remark:       strings.TrimSpace(req.Remark),
	}

	err = dbx.WithTx(ctx, s.repo.DB(), func(ctx context.Context) error {
		// 先落在办维修记录: 在办唯一索引是并发开工的最终防线,
		// 冲突意味着同一故障/同一盏灯已存在另一条在办维修。
		if err := s.repo.CreateWithUniqueNo(ctx, entity, "WX"+startedAt.Format("20060102")); err != nil {
			if isUniqueViolation(err) {
				return apperr.Conflict("故障 %s 已被其他操作人先行开工, 同一盏灯同一时间只允许一条在办维修", target.FaultNo)
			}
			return err
		}

		return s.faults.OnRepairStarted(ctx, fault.RepairFlow{
			FaultID:    target.ID,
			RepairID:   entity.ID,
			RepairNo:   entity.RepairNo,
			Repairman:  repairman,
			Reason:     entity.Content,
			OccurredAt: startedAt,
		})
	})
	if err != nil {
		return nil, err
	}

	entity.FillDuration()
	return entity, nil
}

// Update 修改维修记录, 仅在办(维修中)的记录允许修改; 已完成与已退回均锁定。
func (s *Service) Update(ctx context.Context, id uint, req UpdateRequest) (*Repair, error) {
	entity, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if entity.Status != StatusOngoing {
		return nil, apperr.Conflict("维修记录 %s 当前为%s状态, 不允许修改", entity.RepairNo, StatusLabel(entity.Status))
	}

	if req.Repairman != nil {
		repairman := strings.TrimSpace(*req.Repairman)
		if repairman == "" {
			return nil, apperr.BadRequest("维修人员不能为空")
		}
		entity.Repairman = repairman
	}
	if req.RepairTeam != nil {
		entity.RepairTeam = strings.TrimSpace(*req.RepairTeam)
	}
	if req.ContactPhone != nil {
		entity.ContactPhone = strings.TrimSpace(*req.ContactPhone)
	}
	if req.StartedAt != nil {
		startedAt, err := parseTime(*req.StartedAt, entity.StartedAt)
		if err != nil {
			return nil, err
		}
		entity.StartedAt = startedAt
	}
	if req.Content != nil {
		entity.Content = strings.TrimSpace(*req.Content)
	}
	if req.Materials != nil {
		entity.Materials = strings.TrimSpace(*req.Materials)
	}
	if req.Cost != nil {
		entity.Cost = *req.Cost
	}
	if req.Remark != nil {
		entity.Remark = strings.TrimSpace(*req.Remark)
	}

	if err := s.repo.Update(ctx, entity); err != nil {
		return nil, err
	}
	entity.FillDuration()
	return entity, nil
}

// Finish 完成维修: 条件更新(仅在办可完工)记录结果与完工时间, 结果为已修复时联动故障转为已修复。
// 两人同时点击完工/退回时, 条件更新保证只有一次操作生效。
func (s *Service) Finish(ctx context.Context, id uint, req FinishRequest) (*Repair, error) {
	entity, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if entity.Status == StatusFinished {
		return nil, apperr.Conflict("维修记录 %s 已完成, 不允许重复提交", entity.RepairNo)
	}
	if entity.Status == StatusReturned {
		return nil, apperr.Conflict("维修记录 %s 已退回待处理, 不允许完工", entity.RepairNo)
	}

	result := strings.TrimSpace(req.Result)
	if !IsValidResult(result) {
		return nil, apperr.BadRequest("非法的维修结果: %s", result)
	}

	finishedAt, err := parseTime(req.FinishedAt, time.Now())
	if err != nil {
		return nil, err
	}
	if finishedAt.Before(entity.StartedAt) {
		return nil, apperr.BadRequest("完工时间不能早于开工时间")
	}

	content := strings.TrimSpace(req.Content)
	materials := strings.TrimSpace(req.Materials)
	remark := strings.TrimSpace(req.Remark)

	columns := map[string]any{
		"status":      StatusFinished,
		"result":      result,
		"finished_at": finishedAt,
	}
	if content != "" {
		columns["content"] = content
	}
	if materials != "" {
		columns["materials"] = materials
	}
	if req.Cost != nil {
		columns["cost"] = *req.Cost
	}
	if remark != "" {
		columns["remark"] = remark
	}

	flowReason := buildFinishReason(result, content, materials, remark)

	err = dbx.WithTx(ctx, s.repo.DB(), func(ctx context.Context) error {
		ok, err := s.repo.UpdateStatusIf(ctx, id, []string{StatusOngoing}, columns)
		if err != nil {
			return err
		}
		if !ok {
			return apperr.Conflict("维修记录 %s 状态已变更, 完工未生效, 请刷新后重试", entity.RepairNo)
		}
		return s.faults.OnRepairFinished(ctx, fault.RepairFlow{
			FaultID:    entity.FaultID,
			RepairID:   entity.ID,
			RepairNo:   entity.RepairNo,
			Repairman:  entity.Repairman,
			Result:     result,
			Reason:     flowReason,
			OccurredAt: finishedAt,
		}, result == ResultFixed)
	})
	if err != nil {
		return nil, err
	}

	// 同步内存对象用于响应。
	entity.Status = StatusFinished
	entity.Result = result
	entity.FinishedAt = &finishedAt
	if content != "" {
		entity.Content = content
	}
	if materials != "" {
		entity.Materials = materials
	}
	if req.Cost != nil {
		entity.Cost = *req.Cost
	}
	if remark != "" {
		entity.Remark = remark
	}
	entity.FillDuration()
	return entity, nil
}

// Return 退回待处理: 现场判断有误时把在办维修退回, 必填退回原因。
// 仅在办维修可退回; 已完成需先返修、已关闭故障不参与退回。
// 退回后故障回到待处理、路灯运行状态随之回落, 之后可重新开工。
// 与完工共用在办状态条件, 两人并发操作只有一次生效。
func (s *Service) Return(ctx context.Context, id uint, req ReturnRequest) (*Repair, error) {
	entity, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if entity.Status == StatusReturned {
		return nil, apperr.Conflict("维修记录 %s 已退回, 无需重复操作", entity.RepairNo)
	}
	if entity.Status == StatusFinished {
		return nil, apperr.Conflict("维修记录 %s 已完成, 不允许退回, 如需返修请重新开工", entity.RepairNo)
	}

	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		return nil, apperr.BadRequest("退回原因不能为空")
	}
	operator := strings.TrimSpace(req.Operator)
	if operator == "" {
		operator = entity.Repairman
	}

	returnedAt, err := parseTime(req.ReturnedAt, time.Now())
	if err != nil {
		return nil, err
	}
	if returnedAt.Before(entity.StartedAt) {
		return nil, apperr.BadRequest("退回时间不能早于开工时间")
	}

	columns := map[string]any{
		"status":        StatusReturned,
		"returned_at":   returnedAt,
		"returned_by":   operator,
		"return_reason": reason,
	}

	err = dbx.WithTx(ctx, s.repo.DB(), func(ctx context.Context) error {
		ok, err := s.repo.UpdateStatusIf(ctx, id, []string{StatusOngoing}, columns)
		if err != nil {
			return err
		}
		if !ok {
			return apperr.Conflict("维修记录 %s 状态已变更, 退回未生效, 请刷新后重试", entity.RepairNo)
		}
		return s.faults.OnRepairReturned(ctx, fault.RepairFlow{
			FaultID:    entity.FaultID,
			RepairID:   entity.ID,
			RepairNo:   entity.RepairNo,
			Repairman:  operator,
			Reason:     reason,
			OccurredAt: returnedAt,
		})
	})
	if err != nil {
		return nil, err
	}

	entity.Status = StatusReturned
	entity.ReturnedAt = &returnedAt
	entity.ReturnedBy = operator
	entity.ReturnReason = reason
	return entity, nil
}

// buildFinishReason 组装完工轨迹的理由文本: 结果 + 内容/耗材/备注。
func buildFinishReason(result, content, materials, remark string) string {
	parts := []string{"结果: " + ResultLabel(result)}
	if content != "" {
		parts = append(parts, "内容: "+content)
	}
	if materials != "" {
		parts = append(parts, "耗材: "+materials)
	}
	if remark != "" {
		parts = append(parts, "备注: "+remark)
	}
	return strings.Join(parts, " ；")
}

// Delete 删除维修记录, 已关闭故障的维修记录不允许删除。
// 删除后同步故障维修统计并清理对应处置轨迹; 若故障因此再无维修, 回退为待处理并回落路灯状态。
func (s *Service) Delete(ctx context.Context, id uint) error {
	entity, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return err
	}
	target, err := s.faults.GetByID(ctx, entity.FaultID)
	if err != nil {
		return err
	}
	if target.Status == fault.StatusClosed {
		return apperr.Conflict("故障 %s 已关闭, 不允许删除其维修记录", target.FaultNo)
	}

	err = dbx.WithTx(ctx, s.repo.DB(), func(ctx context.Context) error {
		if err := s.repo.Delete(ctx, id); err != nil {
			return err
		}

		count, err := s.repo.CountByFault(ctx, entity.FaultID)
		if err != nil {
			return err
		}
		latest, err := s.repo.LatestByFault(ctx, entity.FaultID)
		if err != nil {
			return err
		}
		var latestID *uint
		if latest != nil {
			latestID = &latest.ID
		}

		return s.faults.OnRepairDeleted(ctx, fault.RepairFlow{
			FaultID:   entity.FaultID,
			RepairID:  entity.ID,
			RepairNo:  entity.RepairNo,
			Repairman: entity.Repairman,
		}, int(count), latestID)
	})
	return err
}

// Metadata 返回维修模块字典。
func (s *Service) Metadata(ctx context.Context) (*Meta, error) {
	repairmen, err := s.repo.DistinctValues(ctx, "repairman")
	if err != nil {
		return nil, err
	}
	teams, err := s.repo.DistinctValues(ctx, "repair_team")
	if err != nil {
		return nil, err
	}
	return &Meta{
		Statuses:  Statuses(),
		Results:   Results(),
		Repairmen: repairmen,
		Teams:     teams,
	}, nil
}

// Statistics 汇总维修统计信息。
func (s *Service) Statistics(ctx context.Context) (*Statistics, error) {
	total, err := s.repo.Count(ctx)
	if err != nil {
		return nil, err
	}
	byStatus, err := s.repo.CountByColumn(ctx, "status")
	if err != nil {
		return nil, err
	}
	totalCost, err := s.repo.SumCost(ctx)
	if err != nil {
		return nil, err
	}
	averageDuration, err := s.repo.AverageDurationHours(ctx)
	if err != nil {
		return nil, err
	}

	result := &Statistics{
		Total:             total,
		OngoingTotal:      byStatus[StatusOngoing],
		FinishedTotal:     byStatus[StatusFinished],
		ReturnedTotal:     byStatus[StatusReturned],
		TotalCost:         totalCost,
		AverageDurationHr: averageDuration,
	}
	if result.FinishedTotal > 0 {
		result.AverageCost = totalCost / float64(result.FinishedTotal)
	}
	return result, nil
}

// buildFilter 将查询参数转换为仓储条件并解析日期区间。
func buildFilter(query ListQuery) (Filter, error) {
	filter := Filter{
		Keyword:    strings.TrimSpace(query.Keyword),
		FaultID:    query.FaultID,
		LampID:     query.LampID,
		Repairman:  strings.TrimSpace(query.Repairman),
		RepairTeam: strings.TrimSpace(query.RepairTeam),
		Status:     strings.TrimSpace(query.Status),
		Result:     strings.TrimSpace(query.Result),
	}
	if filter.Status != "" &&
		filter.Status != StatusOngoing && filter.Status != StatusFinished && filter.Status != StatusReturned {
		return filter, apperr.BadRequest("非法的维修状态: %s", filter.Status)
	}
	if filter.Result != "" && !IsValidResult(filter.Result) {
		return filter, apperr.BadRequest("非法的维修结果: %s", filter.Result)
	}

	if value := strings.TrimSpace(query.StartDate); value != "" {
		from, err := parseDay(value)
		if err != nil {
			return filter, err
		}
		filter.StartedFrom = &from
	}
	if value := strings.TrimSpace(query.EndDate); value != "" {
		to, err := parseDay(value)
		if err != nil {
			return filter, err
		}
		to = to.AddDate(0, 0, 1)
		filter.StartedTo = &to
	}
	if filter.StartedFrom != nil && filter.StartedTo != nil && filter.StartedTo.Before(*filter.StartedFrom) {
		return filter, apperr.BadRequest("结束日期不能早于开始日期")
	}
	return filter, nil
}

// parseTime 解析时间字符串, 为空时返回 fallback。
func parseTime(value string, fallback time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, apperr.BadRequest("时间格式不正确, 建议使用 YYYY-MM-DD HH:mm:ss: %s", value)
}

// parseDay 解析 YYYY-MM-DD 日期, 返回当天零点。
func parseDay(value string) (time.Time, error) {
	date, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(value), time.Local)
	if err != nil {
		return time.Time{}, apperr.BadRequest("日期格式应为 YYYY-MM-DD, 当前值: %s", value)
	}
	return date, nil
}

func valueOrZero(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}
