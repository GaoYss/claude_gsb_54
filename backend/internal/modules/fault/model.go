package fault

import "time"

// 故障处理状态。
const (
	StatusPending    = "pending"    // 待处理
	StatusProcessing = "processing" // 维修中
	StatusRepaired   = "repaired"   // 已修复
	StatusClosed     = "closed"     // 已关闭
)

// 故障等级(紧急程度)。
const (
	LevelLow    = "low"    // 一般
	LevelNormal = "normal" // 普通
	LevelHigh   = "high"   // 紧急
	LevelUrgent = "urgent" // 特急
)

// 故障来源。
const (
	SourceInspection = "inspection" // 巡检发现
	SourceCitizen    = "citizen"    // 市民上报
	SourceMonitoring = "monitoring" // 系统告警
	SourceOther      = "other"      // 其它
)

// Statuses 返回全部故障状态取值。
func Statuses() []string {
	return []string{StatusPending, StatusProcessing, StatusRepaired, StatusClosed}
}

// Levels 返回全部故障等级取值。
func Levels() []string {
	return []string{LevelLow, LevelNormal, LevelHigh, LevelUrgent}
}

// Sources 返回全部故障来源取值。
func Sources() []string {
	return []string{SourceInspection, SourceCitizen, SourceMonitoring, SourceOther}
}

// FaultTypes 返回全部故障类型取值。
func FaultTypes() []string {
	return []string{
		"灯不亮", "灯光闪烁", "灯具常亮", "灯杆倾斜",
		"线路故障", "控制箱故障", "灯具破损", "其他",
	}
}

// IsValidStatus 校验故障状态取值。
func IsValidStatus(status string) bool {
	for _, item := range Statuses() {
		if item == status {
			return true
		}
	}
	return false
}

// 故障处置动作, 用于处置轨迹记录, 取值与前端时间线字典一致。
const (
	ActionReported       = "reported"        // 故障登记
	ActionRepairStarted  = "repair_started"  // 维修开工
	ActionRepairFinished = "repair_finished" // 维修完成
	ActionReturned       = "returned"        // 退回待处理
	ActionClosed         = "closed"          // 故障关闭
)

// IsOpen 判断故障是否仍处于未闭环状态。
func IsOpen(status string) bool {
	return status == StatusPending || status == StatusProcessing
}

// canTransitTo 校验状态流转是否合法。
// 待处理 -> 维修中 / 已关闭, 维修中 -> 已修复 / 已关闭 / 退回待处理,
// 已修复 -> 已关闭 / 返修(维修中) / 退回待处理, 已关闭不再参与任何流转。
func canTransitTo(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case StatusPending:
		return to == StatusProcessing || to == StatusClosed
	case StatusProcessing:
		return to == StatusRepaired || to == StatusClosed || to == StatusPending
	case StatusRepaired:
		return to == StatusClosed || to == StatusProcessing || to == StatusPending
	default:
		return false
	}
}

// Fault 故障登记记录, 串联路灯台账与维修记录。
type Fault struct {
	ID             uint       `gorm:"primaryKey" json:"id"`
	FaultNo        string     `gorm:"size:64;uniqueIndex;not null" json:"fault_no"`
	LampID         uint       `gorm:"index;not null" json:"lamp_id"`
	LampCode       string     `gorm:"size:64;index" json:"lamp_code"`
	RoadName       string     `gorm:"size:128;index" json:"road_name"`
	FaultType      string     `gorm:"size:32;index;not null" json:"fault_type"`
	FaultLevel     string     `gorm:"size:32;index;not null;default:normal" json:"fault_level"`
	Source         string     `gorm:"size:32;index" json:"source"`
	Description    string     `gorm:"size:512" json:"description"`
	Reporter       string     `gorm:"size:64" json:"reporter"`
	ReporterPhone  string     `gorm:"size:32" json:"reporter_phone"`
	ReportedAt     time.Time  `gorm:"index;not null" json:"reported_at"`
	Status         string     `gorm:"size:32;index;not null;default:pending" json:"status"`
	RepairCount    int        `gorm:"not null;default:0" json:"repair_count"`
	LatestRepairID *uint      `json:"latest_repair_id"`
	ClosedAt       *time.Time `json:"closed_at"`
	CloseRemark    string     `gorm:"size:255" json:"close_remark"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// TableName 指定表名。
func (Fault) TableName() string { return "fault" }

// FaultTransition 故障处置轨迹, 按时间保留每次状态流转的操作人与理由。
type FaultTransition struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	FaultID    uint      `gorm:"index;not null" json:"fault_id"`
	FaultNo    string    `gorm:"size:64;index" json:"fault_no"`
	LampID     uint      `gorm:"index" json:"lamp_id"`
	Action     string    `gorm:"size:32;index;not null" json:"action"`
	FromStatus string    `gorm:"size:32" json:"from_status"`
	ToStatus   string    `gorm:"size:32" json:"to_status"`
	Operator   string    `gorm:"size:64" json:"operator"`
	Reason     string    `gorm:"size:255" json:"reason"`
	CreatedAt  time.Time `gorm:"index;not null" json:"created_at"`
}

// TableName 指定表名。
func (FaultTransition) TableName() string { return "fault_transition" }
