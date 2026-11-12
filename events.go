package main

// 事件驱动 agent 基础设施（原版 F-015「对任何事都能反应」的本地等价）：
// 监听日历变更、目标里程碑、资料新增等事件，触发顾问行动。
// 事件源由客户端推送（POST /v1/events），agent 决定是否生成消息或卡片。
import (
	"encoding/json"
	"fmt"
		"time"
)

// Event 是客户端推送的领域事件。
type Event struct {
	ID   string `json:"id"`
	Type string `json:"type"` // calendar_changed, goal_milestone, source_added, goal_stale
	Data json.RawMessage `json:"data"`
	At   time.Time `json:"at"`
}

// CalendarEventData 日历变更事件的数据。
type CalendarEventData struct {
	Title  string `json:"title"`
	Kind   string `json:"kind"` // added, moved, cancelled
	When   time.Time `json:"when"`
}

// GoalEventData 目标里程碑事件的数据。
type GoalEventData struct {
	GoalID string  `json:"goalId"`
	Title  string  `json:"title"`
	Amount float64 `json:"amount"`
	Unit   string  `json:"unit"`
	Total  float64 `json:"total"`
}

// HandleEvent 处理一个事件，返回顾问应该生成的消息（如果有）。
// 这是规则层；有模型时走模型生成个性化消息。
func (s *Store) HandleEvent(user string, ev Event) (map[string]interface{}, error) {
	switch ev.Type {
	case "calendar_changed":
		var data CalendarEventData
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			return nil, ErrInvalid
		}
		return s.handleCalendarChanged(user, data)
	case "goal_milestone":
		var data GoalEventData
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			return nil, ErrInvalid
		}
		return s.handleGoalMilestone(user, data)
	case "goal_stale":
		var data GoalEventData
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			return nil, ErrInvalid
		}
		return s.handleGoalStale(user, data)
	default:
		return nil, nil // 未知事件类型不处理
	}
}

func (s *Store) handleCalendarChanged(user string, data CalendarEventData) (map[string]interface{}, error) {
	var message string
	switch data.Kind {
	case "added":
		message = fmt.Sprintf("「%s」已添加到你的日历。", data.Title)
	case "moved":
		message = fmt.Sprintf("「%s」改期了。", data.Title)
	case "cancelled":
		message = fmt.Sprintf("「%s」取消了。", data.Title)
	default:
		message = fmt.Sprintf("日历有变动：「%s」。", data.Title)
	}
	return map[string]interface{}{
		"type":    "conversation",
		"role":    "assistant",
		"text":    message,
		"kind":    "event-notice",
	}, nil
}

func (s *Store) handleGoalMilestone(user string, data GoalEventData) (map[string]interface{}, error) {
	message := fmt.Sprintf("「%s」累计 %v %s 了，继续！", data.Title, data.Total, data.Unit)
	return map[string]interface{}{
		"type":    "conversation",
		"role":    "assistant",
		"text":    message,
		"kind":    "goal-milestone",
	}, nil
}

func (s *Store) handleGoalStale(user string, data GoalEventData) (map[string]interface{}, error) {
	message := fmt.Sprintf("「%s」有一段时间没动静了。要不要做一点？", data.Title)
	return map[string]interface{}{
		"type":    "conversation",
		"role":    "assistant",
		"text":    message,
		"kind":    "goal-nudge",
	}, nil
}
