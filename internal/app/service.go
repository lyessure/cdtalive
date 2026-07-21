package app

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"cdtalive/internal/aliyun"
	"cdtalive/internal/config"
	"cdtalive/internal/store"
)

const TransitionPollInterval = 15 * time.Second

type cloud interface {
	TrafficGB() (float64, error)
	Balance() (float64, error)
	Instances() ([]aliyun.Instance, error)
	Instance(string) (*aliyun.Instance, error)
	Start(string) error
	Stop(string) error
}

type cloudFactory func(config.Config) (cloud, error)

type Service struct {
	store     *store.Store
	location  *time.Location
	newCloud  cloudFactory
	runMu     sync.Mutex
	stateMu   sync.RWMutex
	last      map[string]any
	nextRunAt int64
}

func NewService(database *store.Store) *Service {
	return &Service{
		store:    database,
		location: config.Location(),
		newCloud: func(cfg config.Config) (cloud, error) { return aliyun.New(cfg) },
		last:     map[string]any{"status": "未执行"},
	}
}

func (s *Service) Status() map[string]any {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return cloneMap(s.last)
}

func (s *Service) SetFailure(err error) {
	s.setLast(map[string]any{"status": "失败", "error": err.Error()})
}

func (s *Service) SetUninitialized() {
	s.setLast(map[string]any{"status": "未初始化", "error": "配置文件不存在"})
}

func (s *Service) SetNextRunAt(timestamp int64) {
	s.stateMu.Lock()
	s.nextRunAt = timestamp
	s.stateMu.Unlock()
}

func (s *Service) IsTransitioning() bool {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	status, _ := s.last["ecs_status"].(string)
	return status == "Starting" || status == "Stopping"
}

func (s *Service) Settings() (map[string]any, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"daily_stop_windows": cfg.DailyStopWindows,
		"power_mode":         cfg.PowerMode,
	}, nil
}

func (s *Service) UpdateSettings(windows []string, powerMode string) (map[string]any, error) {
	windows, powerMode, err := config.SaveSettings(windows, powerMode)
	if err != nil {
		return nil, err
	}
	result, err := s.RunOnce()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"daily_stop_windows": windows,
		"power_mode":         powerMode,
		"run_result":         result,
	}, nil
}

func (s *Service) RunOnce() (map[string]any, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	client, err := s.newCloud(cfg)
	if err != nil {
		return nil, err
	}
	now := time.Now().In(s.location)
	result := map[string]any{"timestamp": now.Unix(), "action": nil}

	balance, err := client.Balance()
	if err != nil {
		return nil, err
	}
	result["balance_cny"] = balance
	if err := s.store.AddBalance(now.Unix(), balance); err != nil {
		return nil, err
	}

	traffic, err := client.TrafficGB()
	if err != nil {
		return nil, err
	}
	trafficRounded := round(traffic, 4)
	daysInMonth := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, s.location).Day()
	daysLeft := max(daysInMonth-now.Day()+1, 1)
	dailyRemaining := round(math.Max(cfg.TrafficThresholdGB-traffic, 0)/float64(daysLeft), 4)
	result["traffic_gb"] = trafficRounded
	result["daily_remaining_gb"] = dailyRemaining
	result["traffic_threshold_gb"] = cfg.TrafficThresholdGB
	if err := s.store.SaveMetrics(store.Metrics{
		TrafficGB: trafficRounded, TrafficThreshold: cfg.TrafficThresholdGB, DailyRemainingGB: dailyRemaining,
	}); err != nil {
		return nil, err
	}

	switch {
	case traffic >= cfg.TrafficThresholdGB:
		instances, err := client.Instances()
		if err != nil {
			return nil, err
		}
		actions := make(map[string]any, len(instances))
		for _, instance := range instances {
			action, err := s.stop(client, instance.ID)
			if err != nil {
				return nil, err
			}
			actions[instance.ID] = action
		}
		result["action"] = actions
		result["reason"] = "traffic_threshold_reached"
		result["ecs_status"] = "Stopping"
	case cfg.PowerMode == "on":
		action, err := s.start(client, cfg.ECSInstanceID)
		if err != nil {
			return nil, err
		}
		result["action"] = action
		result["reason"] = "forced_on"
		if action == "already_running" {
			result["ecs_status"] = "Running"
		} else {
			result["ecs_status"] = "Starting"
		}
	case cfg.PowerMode == "off":
		action, err := s.stop(client, cfg.ECSInstanceID)
		if err != nil {
			return nil, err
		}
		result["action"] = action
		result["reason"] = "forced_off"
		if action == "already_stopped" {
			result["ecs_status"] = "Stopped"
		} else {
			result["ecs_status"] = "Stopping"
		}
	case withinStopWindow(cfg.DailyStopWindows, now):
		action, err := s.stop(client, cfg.ECSInstanceID)
		if err != nil {
			return nil, err
		}
		result["action"] = action
		result["reason"] = "scheduled_stop_window"
		if action == "already_stopped" {
			result["ecs_status"] = "Stopped"
		} else {
			result["ecs_status"] = "Stopping"
		}
	case balance < cfg.BalanceThreshold:
		instances, err := client.Instances()
		if err != nil {
			return nil, err
		}
		actions := make(map[string]any, len(instances))
		for _, instance := range instances {
			action, err := s.stop(client, instance.ID)
			if err != nil {
				return nil, err
			}
			actions[instance.ID] = action
		}
		result["action"] = actions
		result["reason"] = "low_balance"
	default:
		action, err := s.start(client, cfg.ECSInstanceID)
		if err != nil {
			return nil, err
		}
		result["action"] = action
		result["reason"] = "under_traffic_threshold"
		if action == "already_running" {
			result["ecs_status"] = "Running"
		} else {
			result["ecs_status"] = "Starting"
		}
	}

	fallback, _ := result["ecs_status"].(string)
	reason, _ := result["reason"].(string)
	status, publicIP, err := s.recordInstanceState(client, cfg.ECSInstanceID, fallback, reason)
	if err != nil {
		return nil, err
	}
	result["ecs_status"] = status
	result["ecs_public_ip"] = publicIP
	completed := map[string]any{"status": "完成"}
	for key, value := range result {
		completed[key] = value
	}
	if err := s.store.SaveLastResult(completed); err != nil {
		return nil, err
	}
	s.setLast(completed)
	log.Printf("执行完成：%v", completed)
	return cloneMap(completed), nil
}

func (s *Service) RefreshECSStatus() (map[string]any, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	if err := config.Validate(cfg); err != nil {
		return nil, err
	}
	client, err := s.newCloud(cfg)
	if err != nil {
		return nil, err
	}
	result := s.Status()
	result["ecs_checked_at"] = time.Now().Unix()
	fallback, _ := result["ecs_status"].(string)
	reason, _ := result["reason"].(string)
	status, publicIP, err := s.recordInstanceState(client, cfg.ECSInstanceID, fallback, reason)
	if err != nil {
		return nil, err
	}
	result["ecs_status"] = status
	result["ecs_public_ip"] = publicIP
	if err := s.store.SaveLastResult(result); err != nil {
		return nil, err
	}
	s.setLast(result)
	log.Printf("ECS 状态快速检查完成：%s", status)
	return cloneMap(result), nil
}

func (s *Service) Dashboard() (map[string]any, error) {
	result, err := s.store.LastResult(s.Status())
	if err != nil {
		return nil, err
	}
	metrics, exists, err := s.store.Metrics()
	if err != nil {
		return nil, err
	}
	if exists {
		setIfMissing(result, "traffic_gb", metrics.TrafficGB)
		setIfMissing(result, "traffic_threshold_gb", metrics.TrafficThreshold)
		setIfMissing(result, "daily_remaining_gb", metrics.DailyRemainingGB)
	}

	samples, err := s.store.BalanceSamples()
	if err != nil {
		return nil, err
	}
	var balance any
	if len(samples) > 0 {
		latest := samples[len(samples)-1]
		baseline := samples[0]
		target := latest.Timestamp - 86400
		for _, sample := range samples[1:] {
			if abs64(sample.Timestamp-target) < abs64(baseline.Timestamp-target) {
				baseline = sample
			}
		}
		spent24 := math.Max(0, baseline.AvailableAmount-latest.AvailableAmount)
		now := time.Now().In(s.location)
		monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, s.location).Unix()
		days := time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, s.location).Day()
		balance = map[string]any{
			"available_cny":           latest.AvailableAmount,
			"spent_24h_cny":           round(spent24, 2),
			"spent_current_month_cny": round(spentSince(samples, monthStart), 2),
			"estimated_monthly_cny":   round(spent24*float64(days), 2),
			"updated_at":              latest.Timestamp,
		}
	}

	state, err := s.store.ECSState()
	if err != nil {
		return nil, err
	}
	var ecsStatus any
	if state.LastStatus != "" || state.LastStartupTimestamp != 0 {
		now := time.Now().Unix()
		scheduled, unexpected, err := s.store.StopCounts(now - 30*86400)
		if err != nil {
			return nil, err
		}
		currentState := state.LastStatus
		if value, ok := result["ecs_status"].(string); ok && value != "" {
			currentState = value
		}
		var runningDays any
		var runningSeconds any
		if (currentState == "Running" || currentState == "Starting") && state.LastStartupTimestamp > 0 && state.LastStartupTimestamp <= now {
			elapsedSeconds := now - state.LastStartupTimestamp
			runningSeconds = elapsedSeconds
			runningDays = round(float64(elapsedSeconds)/86400, 2)
		}
		scheduledActive := state.ScheduledStopActive
		if reason, ok := result["reason"].(string); ok && reason != "" {
			scheduledActive = reason == "scheduled_stop_window"
		}
		ecsStatus = map[string]any{
			"state":                 currentState,
			"public_ip":             result["ecs_public_ip"],
			"scheduled_stops_30d":   scheduled,
			"unexpected_stops_30d":  unexpected,
			"running_days":          runningDays,
			"running_seconds":       runningSeconds,
			"scheduled_stop_active": scheduledActive,
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	s.stateMu.RLock()
	nextRunAt := s.nextRunAt
	s.stateMu.RUnlock()
	return map[string]any{
		"last_run":             result,
		"balance":              balance,
		"ecs":                  ecsStatus,
		"run_interval_seconds": cfg.RunIntervalSeconds,
		"next_run_at":          nullableTimestamp(nextRunAt),
	}, nil
}

func (s *Service) NextStopWindowBoundary(windows []string, now time.Time) (time.Time, bool, error) {
	var next time.Time
	for _, window := range windows {
		parts, err := windowMinutes(window)
		if err != nil {
			return time.Time{}, false, err
		}
		for _, minute := range parts {
			candidate := time.Date(now.Year(), now.Month(), now.Day(), minute/60, minute%60, 0, 0, s.location)
			if !candidate.After(now) {
				candidate = candidate.AddDate(0, 0, 1)
			}
			if next.IsZero() || candidate.Before(next) {
				next = candidate
			}
		}
	}
	return next, !next.IsZero(), nil
}

func (s *Service) recordInstanceState(client cloud, instanceID, fallbackStatus, reason string) (string, string, error) {
	now := time.Now().Unix()
	instance, err := client.Instance(instanceID)
	if err != nil {
		return "", "", err
	}
	status := fallbackStatus
	if instance != nil && instance.Status != "" {
		status = instance.Status
	}
	if fallbackStatus == "Starting" && (status == "" || status == "Stopped" || status == "Stopping") {
		status = fallbackStatus
	}
	if fallbackStatus == "Stopping" && (status == "" || status == "Running" || status == "Starting") {
		status = fallbackStatus
	}
	state, err := s.store.ECSState()
	if err != nil {
		return "", "", err
	}
	previousStatus := state.LastStatus
	wasScheduled := state.ScheduledStopActive
	isScheduled := reason == "scheduled_stop_window"
	serviceInitiated := reason == "scheduled_stop_window" || reason == "forced_off" ||
		reason == "traffic_threshold_reached" || reason == "low_balance"
	scheduledEvent := isScheduled && !wasScheduled
	unexpectedEvent := (status == "Stopped" || status == "Stopping") &&
		(previousStatus == "Running" || previousStatus == "Starting") && !wasScheduled && !serviceInitiated
	state.LastStatus = status
	state.ScheduledStopActive = isScheduled
	if scheduledEvent {
		state.LastScheduledStopTimestamp = now
	}
	if unexpectedEvent {
		state.LastUnexpectedTimestamp = now
	}
	if status == "Running" || status == "Starting" {
		state.LastStartupTimestamp = cloudStartTimestamp(instance, now)
	}
	if err := s.store.RecordECSState(state, scheduledEvent, unexpectedEvent, now); err != nil {
		return "", "", err
	}
	publicIP := ""
	if instance != nil {
		publicIP = instance.PublicIP
	}
	return status, publicIP, nil
}

func (s *Service) start(client cloud, instanceID string) (string, error) {
	instance, err := client.Instance(instanceID)
	if err != nil {
		return "", err
	}
	if instance != nil && instance.Status == "Running" {
		return "already_running", nil
	}
	if err := client.Start(instanceID); err != nil {
		return "", err
	}
	return "start_requested", nil
}

func (s *Service) stop(client cloud, instanceID string) (string, error) {
	instance, err := client.Instance(instanceID)
	if err != nil {
		return "", err
	}
	if instance != nil && instance.Status == "Stopped" {
		return "already_stopped", nil
	}
	if err := client.Stop(instanceID); err != nil {
		return "", err
	}
	return "stop_requested", nil
}

func withinStopWindow(windows []string, now time.Time) bool {
	current := now.Hour()*60 + now.Minute()
	for _, window := range windows {
		minutes, err := windowMinutes(window)
		if err != nil {
			continue
		}
		start, end := minutes[0], minutes[1]
		if (start < end && start <= current && current < end) ||
			(start > end && (current >= start || current < end)) {
			return true
		}
	}
	return false
}

func windowMinutes(window string) ([2]int, error) {
	validated, err := config.ValidateStopWindows([]string{window})
	if err != nil {
		return [2]int{}, err
	}
	var startHour, startMinute, endHour, endMinute int
	if _, err := fmt.Sscanf(validated[0], "%d:%d-%d:%d", &startHour, &startMinute, &endHour, &endMinute); err != nil {
		return [2]int{}, err
	}
	return [2]int{startHour*60 + startMinute, endHour*60 + endMinute}, nil
}

func cloudStartTimestamp(instance *aliyun.Instance, fallback int64) int64 {
	if instance == nil || instance.StartTime == "" {
		return fallback
	}
	// 阿里云 DescribeInstances 的 StartTime 可能省略秒，同时兼容带秒格式，
	// 避免将有效的云端启动时间误认为本次状态检查时间。
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00"} {
		parsed, err := time.Parse(layout, instance.StartTime)
		if err == nil {
			return parsed.Unix()
		}
	}
	return fallback
}

func spentSince(samples []store.BalanceSample, since int64) float64 {
	var spent float64
	var previous *float64
	for _, sample := range samples {
		if previous != nil && sample.Timestamp >= since {
			spent += math.Max(*previous-sample.AvailableAmount, 0)
		}
		amount := sample.AvailableAmount
		previous = &amount
	}
	return spent
}

func (s *Service) setLast(result map[string]any) {
	s.stateMu.Lock()
	s.last = cloneMap(result)
	s.stateMu.Unlock()
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func setIfMissing(target map[string]any, key string, value any) {
	if _, exists := target[key]; !exists {
		target[key] = value
	}
}

func round(value float64, decimals int) float64 {
	factor := math.Pow10(decimals)
	return math.Round(value*factor) / factor
}

func abs64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func nullableTimestamp(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}
