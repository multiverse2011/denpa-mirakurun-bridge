package main

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTunerEventsBecomeSSE(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	res, err := http.Get(e.base + "/denpa/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	time.Sleep(100 * time.Millisecond) // 購読が Mirakurun まで繋がるのを待つ
	open(t, e.base, q("GR", "T27", "9", "live a"))
	buf := make([]byte, 64)
	n, err := res.Body.Read(buf)
	if err != nil || !strings.HasPrefix(string(buf[:n]), "event: tuners\ndata: {}\n\n") {
		t.Fatalf("%q %v", buf[:n], err)
	}
}

// SSE を行ごとに読む
func sseLines(t *testing.T, base string) <-chan string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/denpa/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 100)
	go func() {
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	return lines
}

func TestTunerEventsAreCoalesced(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9)
	lines := sseLines(t, e.base)
	// 繋がったときの "[" から出る 1 回を流してから数える
	time.Sleep(300 * time.Millisecond)
	for len(lines) > 0 {
		<-lines
	}
	e.mk.mu.Lock()
	for range 5 {
		e.mk.changed()
	}
	e.mk.mu.Unlock()
	count := 0
	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case l := <-lines:
			if l == "event: tuners" {
				count++
			}
			continue
		case <-deadline:
		}
		break
	}
	if count != 1 {
		t.Fatal(count)
	}
}

func TestSSEHeartbeat(t *testing.T) {
	e := setup(t, oneGR, grChannels, "", 9, func(b *Bridge) { b.heartbeat = 50 * time.Millisecond })
	lines := sseLines(t, e.base)
	select {
	case l := <-lines:
		if l != ":" {
			t.Fatalf("%q", l)
		}
	case <-time.After(time.Second):
		t.Fatal("死活確認が来ません")
	}
}
