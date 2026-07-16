package web

import (
	"net/http"

	webassets "cdtalive/app"
	"cdtalive/internal/app"
	"cdtalive/internal/config"

	"github.com/gin-gonic/gin"
)

type Server struct {
	service   *app.Service
	scheduler *app.Scheduler
}

func New(service *app.Service, scheduler *app.Scheduler) *gin.Engine {
	server := &Server{service: service, scheduler: scheduler}
	router := gin.New()
	router.Use(gin.Logger(), gin.Recovery())
	router.GET("/", server.index)
	router.GET("/api/status", server.status)
	router.GET("/api/dashboard", server.dashboard)
	router.GET("/api/settings", server.settings)
	router.PUT("/api/settings", server.saveSettings)
	router.POST("/api/init", server.initialize)
	router.POST("/api/run", server.runNow)
	return router
}

func (s *Server) index(c *gin.Context) {
	name := "dashboard.html"
	if !config.Exists() {
		name = "init.html"
	}
	data, err := webassets.Pages.ReadFile(name)
	if err != nil {
		c.String(http.StatusInternalServerError, err.Error())
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
}

func (s *Server) status(c *gin.Context) {
	c.JSON(http.StatusOK, s.service.Status())
}

func (s *Server) dashboard(c *gin.Context) {
	if !config.Exists() {
		c.JSON(http.StatusOK, gin.H{
			"last_run":             gin.H{"status": "未初始化", "error": "配置文件不存在"},
			"balance":              nil,
			"ecs":                  nil,
			"run_interval_seconds": 300,
			"next_run_at":          nil,
		})
		return
	}
	result, err := s.service.Dashboard()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (s *Server) settings(c *gin.Context) {
	if !config.Exists() {
		c.JSON(http.StatusOK, gin.H{"daily_stop_windows": []string{}, "power_mode": "auto"})
		return
	}
	result, err := s.service.Settings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

func (s *Server) saveSettings(c *gin.Context) {
	if !config.Exists() {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "配置文件不存在，请先初始化配置。"})
		return
	}
	var payload struct {
		DailyStopWindows []string `json:"daily_stop_windows"`
		PowerMode        string   `json:"power_mode"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": "daily_stop_windows 必须是字符串列表"})
		return
	}
	if payload.DailyStopWindows == nil {
		payload.DailyStopWindows = []string{}
	}
	if payload.PowerMode == "" {
		payload.PowerMode = "auto"
	}
	result, err := s.service.UpdateSettings(payload.DailyStopWindows, payload.PowerMode)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	s.scheduler.Notify()
	c.JSON(http.StatusOK, result)
}

func (s *Server) initialize(c *gin.Context) {
	if config.Exists() {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "配置文件已存在，无法重复初始化。"})
		return
	}
	var payload config.Config
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	if err := config.Init(payload); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"detail": err.Error()})
		return
	}
	s.scheduler.Notify()
	c.JSON(http.StatusOK, gin.H{"status": "success"})
}

func (s *Server) runNow(c *gin.Context) {
	if !config.Exists() {
		c.JSON(http.StatusBadRequest, gin.H{"detail": "配置文件不存在，请先初始化配置。"})
		return
	}
	result, err := s.service.RunOnce()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"detail": err.Error()})
		return
	}
	if s.service.IsTransitioning() {
		s.scheduler.Notify()
	}
	c.JSON(http.StatusOK, result)
}
