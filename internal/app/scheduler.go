package app

import (
	"context"
	"log"
	"time"

	"cdtalive/internal/config"
)

type Scheduler struct {
	service *Service
	wake    chan struct{}
}

func NewScheduler(service *Service) *Scheduler {
	return &Scheduler{service: service, wake: make(chan struct{}, 1)}
}

func (s *Scheduler) Notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) Run(ctx context.Context) {
	var nextRegular time.Time
	var nextBoundary time.Time
	for {
		if !config.Exists() {
			log.Print("配置文件不存在，跳过本次定时执行")
			s.service.SetUninitialized()
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				nextRegular = time.Time{}
				nextBoundary = time.Time{}
				log.Print("检测到系统已初始化，立即启动定时检查程序")
				continue
			}
		}

		now := time.Now().In(s.service.location)
		boundaryDue := !nextBoundary.IsZero() && !now.Before(nextBoundary)
		regularDue := nextRegular.IsZero() || !now.Before(nextRegular)
		runKind := ""
		if regularDue && !s.service.IsTransitioning() {
			runKind = "regular"
		} else if boundaryDue {
			runKind = "boundary"
		}

		var err error
		if runKind != "" {
			_, err = s.service.RunOnce()
		} else if s.service.IsTransitioning() {
			_, err = s.service.RefreshECSStatus()
		}
		if err != nil {
			log.Printf("定时执行失败: %v", err)
			s.service.SetFailure(err)
		}

		if runKind == "regular" {
			interval := 300
			if cfg, loadErr := config.Load(); loadErr == nil {
				interval = cfg.RunIntervalSeconds
			}
			nextRegular = time.Now().In(s.service.location).Add(time.Duration(interval) * time.Second)
		}

		if cfg, loadErr := config.Load(); loadErr != nil {
			log.Printf("计算下一次定时停机边界失败: %v", loadErr)
			nextBoundary = time.Time{}
		} else if boundary, exists, boundaryErr := s.service.NextStopWindowBoundary(cfg.DailyStopWindows, time.Now().In(s.service.location)); boundaryErr != nil {
			log.Printf("计算下一次定时停机边界失败: %v", boundaryErr)
			nextBoundary = time.Time{}
		} else if exists {
			nextBoundary = boundary
		} else {
			nextBoundary = time.Time{}
		}

		now = time.Now().In(s.service.location)
		wakeAt := nextBoundary
		if s.service.IsTransitioning() {
			wakeAt = earlier(wakeAt, now.Add(TransitionPollInterval))
		} else if !nextRegular.IsZero() {
			wakeAt = earlier(wakeAt, nextRegular)
		}
		if wakeAt.IsZero() {
			wakeAt = now.Add(5 * time.Minute)
		}
		s.service.SetNextRunAt(wakeAt.Unix())
		timer := time.NewTimer(maxDuration(time.Until(wakeAt), 0))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-s.wake:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

func earlier(left, right time.Time) time.Time {
	if left.IsZero() || right.Before(left) {
		return right
	}
	return left
}

func maxDuration(value, minimum time.Duration) time.Duration {
	if value < minimum {
		return minimum
	}
	return value
}
