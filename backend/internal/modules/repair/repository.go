package repair

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"streetlight/internal/apperr"
	"streetlight/pkg/dbx"
	"streetlight/pkg/pagination"
)

// Filter 是仓储层使用的维修记录查询条件。
type Filter struct {
	Keyword     string
	FaultID     uint
	LampID      uint
	Repairman   string
	RepairTeam  string
	Status      string
	Result      string
	StartedFrom *time.Time
	StartedTo   *time.Time
}

// Repository 负责维修记录的数据访问。
type Repository struct {
	db *gorm.DB
}

// NewRepository 构造维修记录仓储。
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{db: db}
}

// DB 暴露底层连接, 供业务层开启跨仓储事务。
func (r *Repository) DB() *gorm.DB { return r.db }

func (r *Repository) session(ctx context.Context) *gorm.DB {
	return dbx.Session(ctx, r.db)
}

// Create 新增维修记录。
func (r *Repository) Create(ctx context.Context, entity *Repair) error {
	if err := r.session(ctx).Create(entity).Error; err != nil {
		return fmt.Errorf("录入维修记录失败: %w", err)
	}
	return nil
}

// CreateWithUniqueNo 生成唯一维修单号并落库, 单号冲突时自动重试。
// 若触发的是在办维修局部唯一索引(fault_id / lamp_id), 不属于单号冲突,
// 立即原样上抛, 由业务层转换为"已有在办维修"的冲突提示。
func (r *Repository) CreateWithUniqueNo(ctx context.Context, entity *Repair, prefix string) error {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		sequence, err := r.NextSequence(ctx, prefix)
		if err != nil {
			return err
		}
		entity.RepairNo = fmt.Sprintf("%s%04d", prefix, sequence+attempt)
		err = r.Create(ctx, entity)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isUniqueViolation(err) {
			return err
		}
		if !isRepairNoViolation(err) {
			return err
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return apperr.Conflict("维修单号生成冲突, 请稍后重试")
}

// isRepairNoViolation 判断唯一冲突是否来自维修单号列(而非在办局部唯一索引)。
// sqlite: "UNIQUE constraint failed: repair.repair_no";
// postgres 单号索引为 gorm 自动命名, 冲突信息含 repair_no 列名。
func isRepairNoViolation(err error) bool {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "fault_id") || strings.Contains(message, "lamp_id") ||
		strings.Contains(message, "idx_repair_ongoing") {
		return false
	}
	return strings.Contains(message, "repair_no")
}

// NextSequence 返回指定前缀下可用的下一个流水号。
func (r *Repository) NextSequence(ctx context.Context, prefix string) (int, error) {
	var latest string
	err := r.session(ctx).Model(&Repair{}).
		Where("repair_no LIKE ?", prefix+"%").
		Order("repair_no DESC").
		Limit(1).
		Pluck("repair_no", &latest).Error
	if err != nil {
		return 0, fmt.Errorf("生成维修单号失败: %w", err)
	}
	if latest == "" {
		return 1, nil
	}
	value, convErr := strconv.Atoi(strings.TrimPrefix(latest, prefix))
	if convErr != nil {
		return 1, nil
	}
	return value + 1, nil
}

// Update 保存维修记录全部字段。
func (r *Repository) Update(ctx context.Context, entity *Repair) error {
	if err := r.session(ctx).Save(entity).Error; err != nil {
		return fmt.Errorf("更新维修记录失败: %w", err)
	}
	return nil
}

// UpdateStatusIf 当前状态命中 expectStatuses 时才执行局部更新(必须包含 status), 返回是否生效。
// 用于完工 / 退回的并发互斥: 先到的请求改走状态, 后到的请求 RowsAffected=0 得到冲突提示。
func (r *Repository) UpdateStatusIf(ctx context.Context, id uint, expectStatuses []string, columns map[string]any) (bool, error) {
	if len(columns) == 0 {
		return false, nil
	}
	result := r.session(ctx).Model(&Repair{}).
		Where("id = ? AND status IN ?", id, expectStatuses).
		Updates(columns)
	if result.Error != nil {
		return false, fmt.Errorf("更新维修记录失败: %w", result.Error)
	}
	return result.RowsAffected > 0, nil
}

// Delete 按主键删除维修记录。
func (r *Repository) Delete(ctx context.Context, id uint) error {
	if err := r.session(ctx).Delete(&Repair{}, id).Error; err != nil {
		return fmt.Errorf("删除维修记录失败: %w", err)
	}
	return nil
}

// GetByID 按主键查询维修记录。
func (r *Repository) GetByID(ctx context.Context, id uint) (*Repair, error) {
	var entity Repair
	err := r.session(ctx).First(&entity, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, apperr.NotFound("维修记录不存在: id=%d", id)
	}
	if err != nil {
		return nil, fmt.Errorf("查询维修记录失败: %w", err)
	}
	entity.FillDuration()
	return &entity, nil
}

// List 分页查询维修记录。
func (r *Repository) List(ctx context.Context, filter Filter, page pagination.Query) ([]Repair, int64, error) {
	base := func() *gorm.DB {
		return applyFilter(r.session(ctx).Model(&Repair{}), filter)
	}

	var total int64
	if err := base().Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("统计维修记录失败: %w", err)
	}

	entities := make([]Repair, 0)
	if err := base().Order(page.OrderClause()).Offset(page.Offset()).Limit(page.Limit()).Find(&entities).Error; err != nil {
		return nil, 0, fmt.Errorf("查询维修记录失败: %w", err)
	}
	for index := range entities {
		entities[index].FillDuration()
	}
	return entities, total, nil
}

// ListByFault 查询某条故障的全部维修记录, 按开工时间正序。
func (r *Repository) ListByFault(ctx context.Context, faultID uint) ([]Repair, error) {
	entities := make([]Repair, 0)
	err := r.session(ctx).Where("fault_id = ?", faultID).Order("started_at ASC, id ASC").Find(&entities).Error
	if err != nil {
		return nil, fmt.Errorf("查询故障维修记录失败: %w", err)
	}
	for index := range entities {
		entities[index].FillDuration()
	}
	return entities, nil
}

// GetOngoingByFault 查询某条故障当前进行中的维修记录, 不存在时返回 nil。
func (r *Repository) GetOngoingByFault(ctx context.Context, faultID uint) (*Repair, error) {
	var entity Repair
	err := r.session(ctx).
		Where("fault_id = ? AND status = ?", faultID, StatusOngoing).
		Order("started_at DESC, id DESC").
		First(&entity).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询进行中的维修记录失败: %w", err)
	}
	return &entity, nil
}

// CountOngoingByLamp 统计某盏路灯当前在办的维修记录数量。
// 同一盏灯不允许同时挂着两条在办维修, 开工前用它做跨故障校验。
func (r *Repository) CountOngoingByLamp(ctx context.Context, lampID uint) (int64, error) {
	var count int64
	err := r.session(ctx).Model(&Repair{}).
		Where("lamp_id = ? AND status = ?", lampID, StatusOngoing).
		Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("统计路灯在办维修记录失败: %w", err)
	}
	return count, nil
}

// CountOngoingByFault 统计某条故障当前在办的维修记录数量, 供故障模块关闭前校验。
func (r *Repository) CountOngoingByFault(ctx context.Context, faultID uint) (int64, error) {
	var count int64
	err := r.session(ctx).Model(&Repair{}).
		Where("fault_id = ? AND status = ?", faultID, StatusOngoing).
		Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("统计故障在办维修记录失败: %w", err)
	}
	return count, nil
}

// LatestByFault 查询某条故障最近一次维修记录, 不存在时返回 nil。
func (r *Repository) LatestByFault(ctx context.Context, faultID uint) (*Repair, error) {
	var entity Repair
	err := r.session(ctx).
		Where("fault_id = ?", faultID).
		Order("started_at DESC, id DESC").
		First(&entity).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("查询最新维修记录失败: %w", err)
	}
	return &entity, nil
}

// CountByFault 统计某条故障的维修记录数量。
func (r *Repository) CountByFault(ctx context.Context, faultID uint) (int64, error) {
	var count int64
	err := r.session(ctx).Model(&Repair{}).Where("fault_id = ?", faultID).Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("统计故障维修记录失败: %w", err)
	}
	return count, nil
}

// CountByColumn 按列分组统计。
func (r *Repository) CountByColumn(ctx context.Context, column string) (map[string]int64, error) {
	type row struct {
		Label string
		Total int64
	}
	rows := make([]row, 0)
	err := r.session(ctx).Model(&Repair{}).
		Select(column + " AS label, COUNT(*) AS total").
		Group(column).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("分组统计 %s 失败: %w", column, err)
	}
	result := make(map[string]int64, len(rows))
	for _, item := range rows {
		result[item.Label] = item.Total
	}
	return result, nil
}

// Count 统计维修记录总数。
func (r *Repository) Count(ctx context.Context) (int64, error) {
	var total int64
	if err := r.session(ctx).Model(&Repair{}).Count(&total).Error; err != nil {
		return 0, fmt.Errorf("统计维修记录总数失败: %w", err)
	}
	return total, nil
}

// CountFinishedBetween 统计完工时间落在区间内的维修记录数量。
func (r *Repository) CountFinishedBetween(ctx context.Context, from, to time.Time) (int64, error) {
	var total int64
	err := r.session(ctx).Model(&Repair{}).
		Where("status = ? AND finished_at >= ? AND finished_at < ?", StatusFinished, from, to).
		Count(&total).Error
	if err != nil {
		return 0, fmt.Errorf("统计区间完工数量失败: %w", err)
	}
	return total, nil
}

// SumCost 汇总已完成维修的费用(退回作废的记录不计入)。
func (r *Repository) SumCost(ctx context.Context) (float64, error) {
	var total float64
	err := r.session(ctx).Model(&Repair{}).
		Where("status = ?", StatusFinished).
		Select("COALESCE(SUM(cost), 0)").Scan(&total).Error
	if err != nil {
		return 0, fmt.Errorf("汇总维修费用失败: %w", err)
	}
	return total, nil
}

// AverageDurationHours 统计已完成维修的平均耗时(小时)。
// 不同数据库的时间差函数差异较大, 因此取回时间戳后在应用层计算, 保证 sqlite 与 postgres 行为一致。
func (r *Repository) AverageDurationHours(ctx context.Context) (float64, error) {
	type row struct {
		StartedAt  time.Time
		FinishedAt time.Time
	}
	rows := make([]row, 0)
	err := r.session(ctx).Model(&Repair{}).
		Select("started_at, finished_at").
		Where("finished_at IS NOT NULL").
		Scan(&rows).Error
	if err != nil {
		return 0, fmt.Errorf("统计平均维修耗时失败: %w", err)
	}

	var total time.Duration
	count := 0
	for _, item := range rows {
		if item.FinishedAt.Before(item.StartedAt) {
			continue
		}
		total += item.FinishedAt.Sub(item.StartedAt)
		count++
	}
	if count == 0 {
		return 0, nil
	}
	return total.Hours() / float64(count), nil
}

// DistinctValues 返回某列的去重取值, 用于下拉选项。
func (r *Repository) DistinctValues(ctx context.Context, column string) ([]string, error) {
	values := make([]string, 0)
	err := r.session(ctx).Model(&Repair{}).
		Where(column+" <> ''").
		Distinct().
		Order(column).
		Pluck(column, &values).Error
	if err != nil {
		return nil, fmt.Errorf("查询 %s 选项失败: %w", column, err)
	}
	return values, nil
}

// applyFilter 统一拼装维修记录查询条件。
func applyFilter(statement *gorm.DB, filter Filter) *gorm.DB {
	if keyword := strings.TrimSpace(filter.Keyword); keyword != "" {
		like := "%" + keyword + "%"
		statement = statement.Where(
			"repair_no LIKE ? OR fault_no LIKE ? OR lamp_code LIKE ? OR repairman LIKE ?",
			like, like, like, like,
		)
	}
	if filter.FaultID > 0 {
		statement = statement.Where("fault_id = ?", filter.FaultID)
	}
	if filter.LampID > 0 {
		statement = statement.Where("lamp_id = ?", filter.LampID)
	}
	if value := strings.TrimSpace(filter.Repairman); value != "" {
		statement = statement.Where("repairman = ?", value)
	}
	if value := strings.TrimSpace(filter.RepairTeam); value != "" {
		statement = statement.Where("repair_team = ?", value)
	}
	if filter.Status != "" {
		statement = statement.Where("status = ?", filter.Status)
	}
	if filter.Result != "" {
		statement = statement.Where("result = ?", filter.Result)
	}
	if filter.StartedFrom != nil {
		statement = statement.Where("started_at >= ?", *filter.StartedFrom)
	}
	if filter.StartedTo != nil {
		statement = statement.Where("started_at < ?", *filter.StartedTo)
	}
	return statement
}

// isUniqueViolation 兼容 sqlite 与 postgres 的唯一约束冲突判断。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint failed") ||
		strings.Contains(message, "duplicate key") ||
		strings.Contains(message, "unique violation")
}
