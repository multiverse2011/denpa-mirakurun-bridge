package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

func env(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	priorityMap, err := parsePriorityMap(env("PRIORITY_MAP", ""))
	if err != nil {
		return err
	}
	keylessTuners, err := strconv.Atoi(env("KEYLESS_TUNERS", "1"))
	if err != nil || keylessTuners < 0 {
		return fmt.Errorf("KEYLESS_TUNERS は 0 以上の整数です: %s", os.Getenv("KEYLESS_TUNERS"))
	}
	var allowed []string
	if v := env("ALLOWED_HOSTS", ""); v != "" {
		allowed = strings.Split(v, ",")
	}
	mirakurun := strings.TrimRight(env("MIRAKURUN_URL", "http://127.0.0.1:40772"), "/")
	b, err := NewBridge(Config{
		MirakurunURL:  mirakurun,
		PriorityMap:   priorityMap,
		ChannelsFile:  env("CHANNELS_FILE", "data/channels.json"),
		AllowedHosts:  allowed,
		Version:       env("DENPA_VERSION", "bridge-"+version),
		KeylessTuners: keylessTuners,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	addr := net.JoinHostPort(env("HOST", "127.0.0.1"), env("PORT", "25252"))
	srv := &http.Server{Addr: addr, Handler: b, ReadHeaderTimeout: 10 * time.Second}
	go b.Watch(ctx)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	b.logf("listening on %s → Mirakurun %s", addr, mirakurun)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
