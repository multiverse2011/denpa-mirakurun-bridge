package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
		return errors.New(msg.t("KEYLESS_TUNERS must be an integer of 0 or more (got %s)", "KEYLESS_TUNERS は 0 以上の整数で指定してください (指定値: %s)", os.Getenv("KEYLESS_TUNERS")))
	}
	// denpa のエージェントと同じ既定 (6 時間)。0 なら待たない
	shutdownWait, err := strconv.ParseInt(env("SHUTDOWN_WAIT", "21600000"), 10, 64)
	// 上限は time.Duration に収まる長さ (超えると負に回って待たなくなる)
	if err != nil || shutdownWait < 0 || shutdownWait > math.MaxInt64/int64(time.Millisecond) {
		return errors.New(msg.t("SHUTDOWN_WAIT must be milliseconds of 0 or more (got %s)", "SHUTDOWN_WAIT は 0 以上のミリ秒で指定してください (指定値: %s)", os.Getenv("SHUTDOWN_WAIT")))
	}
	var allowed []string
	if v := env("ALLOWED_HOSTS", ""); v != "" {
		allowed = strings.Split(v, ",")
	}
	// 既定は TLS (denpa v2.28.0 以降)。AGENT_TLS=0 で v2.27.0 以前の denpa 向けの平文 HTTP
	var pairing *Pairing
	switch env("AGENT_TLS", "1") {
	case "1":
		dir := filepath.Join(filepath.Dir(env("CHANNELS_FILE", "data/channels.json")), "tls")
		if pairing, err = LoadPairing(dir, os.Getenv("AGENT_PAIR_TOKEN")); err != nil {
			return fmt.Errorf("%s: %w", dir, err)
		}
	case "0":
	default:
		return errors.New(msg.t("AGENT_TLS must be 1 or 0 (got %s)", "AGENT_TLS は 1 か 0 で指定してください (指定値: %s)", os.Getenv("AGENT_TLS")))
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
		Pairing:       pairing,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	port := env("PORT", "25252")
	addr := net.JoinHostPort(env("HOST", "127.0.0.1"), port)
	// WriteTimeout は配信を切るので付けない (書き込みの期限は配信ごとに Bridge が付ける)
	srv := &http.Server{Addr: addr, Handler: b, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	srv.RegisterOnShutdown(b.Stop)
	// 録画を待つ間も tuners の通知を流すので、止まれの合図 (ctx) では止めない
	watch, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	go b.Watch(watch)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		stop() // 録画を待つ間に 2 度目の合図が来たら、ふつうに落ちる
		until := time.Now().Add(time.Duration(shutdownWait) * time.Millisecond)
		b.Drain(time.Until(until))
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		// Drain が 0 を見てからリスナーが閉じるまでに始まった録画も、ここで待つ (Shutdown は流れている応答を切らない)
		b.Drain(time.Until(until))
	}()
	b.logf("listening on %s → Mirakurun %s", "%s で待ち受けます (Mirakurun: %s)", addr, mirakurun)
	serve := srv.ListenAndServe
	if pairing != nil {
		srv.TLSConfig = pairing.TLSConfig()
		serve = func() error { return srv.ListenAndServeTLS("", "") }
		// 合言葉は標準出力にだけ出す (denpa が読む /denpa/logs には載せない)
		host, _ := os.Hostname()
		if url, unpaired := pairing.PairURL(host, port); unpaired {
			fmt.Println(msg.t("[bridge] URL for pairing with denpa (TUNER_AGENT_URL): %s", "[bridge] denpa と組むときの URL (TUNER_AGENT_URL): %s", url))
		}
	}
	if err := serve(); err != http.ErrServerClosed {
		return err
	}
	// ListenAndServe は Shutdown の始まりで戻るので、書きかけの応答を待ってから抜ける
	<-stopped
	return nil
}
