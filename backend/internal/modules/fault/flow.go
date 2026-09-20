package fault

import "time"

// 处置流转动作。
const (
	FlowReported = "reported"        // 故障登记(待处理)
	FlowStarted  = "repair_started"  // 维修开工(待处理/返修 -> 维修中)
	FlowFinished = "repair_finished" // 维修完工(结果与说明写入理由)
	FlowReturned = "returned"        // 退回待处理(维修中 -> 待处理)
	FlowClosed   = "closed"          // 关闭故障
)

// Flows 返回全部流转动作取值。
func Flows() []string {
	return []string{FlowReported, FlowStarted, FlowFinished, FlowReturned, FlowClosed}
}

// FaultFlow 处置轨迹: 一条记录对应故障的一次状态流转, 按时间留存操作人与理由。
// 轨迹只追加、不修改、不删除, 是处置过程的审计事实来源。
type FaultFlow struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	FaultID    uint      `gorm:"index;not null" json:"fault_id"`
	FaultNo    string    `gorm:"size:64;index" json:"fault_no"`
	RepairID   *uint     `gorm:"index" json:"repair_id,omitempty"`
	RepairNo   string    `gorm:"size:64" json:"repair_no,omitempty"`
	LampID     uint      `gorm:"index" json:"lamp_id"`
	Action     string    `gorm:"size:32;index;not null" json:"action"`
	FromStatus string    `gorm:"size:32" json:"from_status"`
	ToStatus   string    `gorm:"size:32;not null" json:"to_status"`
	Operator   string    `gorm:"size:64" json:"operator"`
	Reason     string    `gorm:"size:512" json:"reason"`
	OccurredAt time.Time `gorm:"index;not null" json:"occurred_at"`
	CreatedAt  time.Time `json:"created_at"`
}

// TableName 指定表名。
func (FaultFlow) TableName() string { return "fault_flow" }
