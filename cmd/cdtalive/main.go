package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cdtalive/internal/app"
	"cdtalive/internal/config"
	"cdtalive/internal/store"
	webserver "cdtalive/internal/web"
)

func main() {
	address := flag.String("addr", envOr("CDT_WEB_ADDR", "127.0.0.1:5201"), "Web 监听地址")
	flag.Parse()

	database, err := store.Open(store.DBPath())
	if err != nil {
		log.Fatalf("打开 SQLite 失败: %v", err)
	}
	defer database.Close()

	if err := prune(database); err != nil {
		log.Fatalf("清理过期数据失败: %v", err)
	}

	service := app.NewService(database)
	scheduler := app.NewScheduler(service)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go scheduler.Run(ctx)
	go dailyMaintenance(ctx, database)

	server := &http.Server{
		Addr:              *address,
		Handler:           webserver.New(service, scheduler),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		log.Printf("CDT Alive 已启动：http://%s", *address)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Web 服务退出: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Web 服务关闭失败: %v", err)
	}
}

func dailyMaintenance(ctx context.Context, database *store.Store) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := prune(database); err != nil {
				log.Printf("每日数据清理失败: %v", err)
			} else {
				log.Print("每日数据清理完成")
			}
		}
	}
}

func prune(database *store.Store) error {
	now := time.Now().In(config.Location())
	return database.Prune(store.PreviousMonthStart(now), now.AddDate(0, 0, -30).Unix())
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
