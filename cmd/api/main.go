package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/ranajahanzaib/customer-data-export/internal/app"
)

func main() {
	a, err := app.New()
	if err != nil {
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}
	defer a.Close()
	addr := env("HTTP_ADDR", ":8080")
	slog.Info("api listening", "address", addr)
	srv := &http.Server{Addr: addr, Handler: a.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
