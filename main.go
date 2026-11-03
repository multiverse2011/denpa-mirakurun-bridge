package main

import (
	"context"
	"errors"
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
	lang := env("MESSAGE_LANG", "ja")
	if lang != "ja" && lang != "en" {
		return fmt.Errorf("MESSAGE_LANG は ja か en です / MESSAGE_LANG must be ja or en: %s", lang)
	}
	msg := messages{lang == "en"}
	priorityMap, err := parsePriorityMap(env("PRIORITY_MAP", ""), msg)
	if err != nil {
		return err
	}
	keylessTuners, err := strconv.Atoi(env("KEYLESS_TUNERS", "1"))
	if err != nil || keylessTuners < 0 {
		return errors.New(msg.t("KEYLESS_TUNERS must be an integer of 0 or more: %s", "KEYLESS_TUNERS は 0 以上の整数です: %s", os.Getenv("KEYLESS_TUNERS")))
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
		English:       msg.en,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	addr := net.JoinHostPort(env("HOST", "127.0.0.1"), env("PORT", "25252"))
	// WriteTimeout は配信を切るので付けない (書き込みの期限は配信ごとに Bridge が付ける)
	srv := &http.Server{Addr: addr, Handler: b, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go b.Watch(ctx)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	b.logf("listening on %s → Mirakurun %s", "listening on %s → Mirakurun %s", addr, mirakurun)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	// ListenAndServe は Shutdown の始まりで戻るので、書きかけの応答を待ってから抜ける
	<-stopped
	return nil
}
