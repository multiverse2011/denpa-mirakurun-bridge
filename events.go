package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// listener は SSE の購読者 1 人。種類ごとに 1 つだけ溜め、読み直しを重ねない
type listener struct{ tuners, channels chan struct{} }

func (b *Bridge) emit(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for l := range b.listeners {
		c := l.tuners
		if name == "channels" {
			c = l.channels
		}
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

func (b *Bridge) events(w http.ResponseWriter, r *http.Request) {
	l := &listener{make(chan struct{}, 1), make(chan struct{}, 1)}
	b.mu.Lock()
	b.listeners[l] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.listeners, l)
		b.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	rc := http.NewResponseController(w)
	defer rc.SetWriteDeadline(time.Time{}) //nolint:errcheck
	// 時々コメント行を送る。黙って消えた購読者は書いてみないと分からず、
	// denpa (Bun の fetch) は無通信が続くと TimeoutError で切るため
	ping := time.NewTicker(b.heartbeat)
	defer ping.Stop()
	send := func(text string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(b.writeTimeout))
		_, err := io.WriteString(w, text)
		return err == nil && rc.Flush() == nil
	}
	ok := send("")
	for ok {
		select {
		case <-r.Context().Done():
			return
		case <-b.stopping:
			return
		case <-l.tuners:
			ok = send("event: tuners\ndata: {}\n\n")
		case <-l.channels:
			ok = send("event: channels\ndata: {}\n\n")
		case <-ping.C:
			ok = send(":\n\n")
		}
	}
}

// Mirakurun のチューナーの出来事を tuners として流す。中身は読まない (denpa が /denpa/tuners を読み直す)。
// 1 回の選局で出来事は続けて届くので、eventDelay のあいだはまとめて 1 回にする
func (b *Bridge) Watch(ctx context.Context) {
	var armed atomic.Bool
	notify := func() {
		if armed.CompareAndSwap(false, true) {
			time.AfterFunc(b.eventDelay, func() {
				armed.Store(false)
				b.emit("tuners")
			})
		}
	}
	wait := time.Second
	for ctx.Err() == nil {
		err := func() error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.MirakurunURL+"/api/events/stream?resource=tuner", nil)
			if err != nil {
				return err
			}
			res, err := b.client.Do(req)
			if err != nil {
				return err
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusOK {
				return fmt.Errorf("status %d", res.StatusCode)
			}
			wait = time.Second
			buf := make([]byte, 4096)
			for {
				if _, err := res.Body.Read(buf); err != nil {
					return err
				}
				notify()
			}
		}()
		if ctx.Err() != nil {
			return
		}
		b.logf("cannot follow Mirakurun events (retrying in %v): %v", "Mirakurun の通知に繋げません (%v 後に再試行): %v", wait, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Minute)
	}
}
