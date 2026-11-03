package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	MirakurunURL  string
	PriorityMap   map[int]int
	ChannelsFile  string
	AllowedHosts  []string
	Version       string
	KeylessTuners int
}

func parsePriorityMap(text string) (map[int]int, error) {
	m := map[int]int{}
	for _, pair := range strings.Split(text, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		from, to, ok := strings.Cut(pair, ":")
		f, err1 := strconv.Atoi(from)
		t, err2 := strconv.Atoi(to)
		if !ok || err1 != nil || err2 != nil {
			return nil, fmt.Errorf("PRIORITY_MAP を読めません: %s (例: 10:2,9:1)", pair)
		}
		m[f] = t
	}
	return m, nil
}

// Mirakurun の HTTP API は 0 未満を 400 で断る
func mapPriority(p int, m map[int]int) int {
	if v, ok := m[p]; ok {
		p = v
	}
	return max(p, 0)
}

var reKeyless = regexp.MustCompile(`^(epg|logo|scan) `)

// 掛かっていない SI しか読まない利用者 (denpa の Sink.WantsKeys と同じ)
func keyless(use string) bool { return reKeyless.MatchString(use) }

// DNS リバインディング避け。denpa のエージェントと同じ決まり
func hostAllowed(host string, extra []string) bool {
	if host == "" {
		return false
	}
	if strings.HasPrefix(host, "[") {
		return true
	}
	name := strings.ToLower(host)
	if h, _, err := net.SplitHostPort(name); err == nil {
		name = h
	}
	if net.ParseIP(name) != nil || name == "localhost" || !strings.Contains(name, ".") {
		return true
	}
	for _, s := range []string{".localhost", ".local", ".lan", ".home.arpa", ".internal", ".svc"} {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	for _, e := range extra {
		x := strings.ToLower(strings.TrimSpace(e))
		if x == "" {
			continue
		}
		if strings.HasPrefix(x, ".") && (name == x[1:] || strings.HasSuffix(name, x)) || name == x {
			return true
		}
	}
	return false
}

type mkUser struct {
	ID            string `json:"id"`
	Priority      int    `json:"priority"`
	Agent         string `json:"agent"`
	StreamSetting *struct {
		Channel *struct {
			Type    string `json:"type"`
			Channel string `json:"channel"`
		} `json:"channel"`
	} `json:"streamSetting"`
}

func (u mkUser) on(typ, ch string) bool {
	return u.StreamSetting != nil && u.StreamSetting.Channel != nil &&
		u.StreamSetting.Channel.Type == typ && u.StreamSetting.Channel.Channel == ch
}

type mkTuner struct {
	Index       int      `json:"index"`
	Name        string   `json:"name"`
	Types       []string `json:"types"`
	Pid         *int     `json:"pid"`
	Users       []mkUser `json:"users"`
	IsAvailable bool     `json:"isAvailable"`
	IsFault     bool     `json:"isFault"`
}

type active struct {
	use       string
	priority  int
	typ       string
	mkChannel string
	mkUserID  string
	handedOff atomic.Bool
	cancel    context.CancelFunc
}

// 引き継ぎ (after)。前の利用者は正常に終わらせる
func (a *active) handOff() {
	a.handedOff.Store(true)
	a.cancel()
}

type pendingReq struct{ typ, mkChannel string }

type logLine struct {
	At   int64  `json:"at"`
	Text string `json:"text"`
}

type Bridge struct {
	cfg       Config
	client    *http.Client
	mu        sync.Mutex
	active    map[*active]struct{}
	pending   map[*pendingReq]struct{}
	listeners map[chan string]struct{}
	stored    []json.RawMessage
	parsed    []DenpaChannel
	logs      []logLine
}

func NewBridge(cfg Config) (*Bridge, error) {
	b := &Bridge{
		cfg:       cfg,
		client:    &http.Client{},
		active:    map[*active]struct{}{},
		pending:   map[*pendingReq]struct{}{},
		listeners: map[chan string]struct{}{},
	}
	data, err := os.ReadFile(cfg.ChannelsFile)
	if err == nil {
		var raw []json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("%s: %w", cfg.ChannelsFile, err)
		}
		b.setStored(raw)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return b, nil
}

func (b *Bridge) setStored(raw []json.RawMessage) {
	parsed := make([]DenpaChannel, len(raw))
	for i, r := range raw {
		_ = json.Unmarshal(r, &parsed[i])
	}
	b.stored, b.parsed = raw, parsed
}

func (b *Bridge) logf(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	log.Print("[bridge] " + text)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.logs = append(b.logs, logLine{time.Now().UnixMilli(), text})
	if len(b.logs) > 2000 {
		b.logs = b.logs[1:]
	}
}

func (b *Bridge) emit(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for l := range b.listeners {
		select {
		case l <- name:
		default:
		}
	}
}

func (b *Bridge) mk(ctx context.Context, path string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.MirakurunURL+path, nil)
	res, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("Mirakurun %s -> %d", path, res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (b *Bridge) ours() map[string]*active {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := map[string]*active{}
	for a := range b.active {
		if a.mkUserID != "" {
			m[a.mkUserID] = a
		}
	}
	return m
}

func (b *Bridge) tuners(ctx context.Context) ([]map[string]any, error) {
	var list []mkTuner
	var channels []MirakurunChannel
	if err := b.mk(ctx, "/api/tuners", &list); err != nil {
		return nil, err
	}
	if err := b.mk(ctx, "/api/channels", &channels); err != nil {
		return nil, err
	}
	mine := b.ours()
	b.mu.Lock()
	stored := b.parsed
	b.mu.Unlock()

	out := []map[string]any{}
	for _, t := range list {
		var channel, rank, errText any
		for _, u := range t.Users {
			if c := u.StreamSetting; c != nil && c.Channel != nil {
				channel = map[string]string{"type": c.Channel.Type, "channel": toDenpa(c.Channel.Type, c.Channel.Channel, stored, channels)}
				break
			}
		}
		// Mirakurun は空きを定義の順に使う
		if t.IsAvailable {
			rank = t.Index
		}
		if t.IsFault {
			errText = "Mirakurun がこのチューナーの異常を報告しています"
		}
		users := []map[string]any{}
		for _, u := range t.Users {
			if a, ok := mine[u.ID]; ok {
				users = append(users, map[string]any{"use": a.use, "priority": a.priority})
			} else {
				name := u.Agent
				if name == "" {
					name = u.ID
				}
				users = append(users, map[string]any{"use": "mirakurun " + name, "priority": u.Priority})
			}
		}
		out = append(out, map[string]any{
			"index": t.Index, "name": t.Name, "types": t.Types, "disabled": !t.IsAvailable, "rank": rank,
			"device": nil, "lnb": nil, "channel": channel, "users": users, "pid": t.Pid, "error": errText,
		})
	}
	return out, nil
}

// 番組表・ロゴ・スキャンに新しくチューナーを取らせてよいか。選ぶのは Mirakurun で、ここは取りに行かせるかだけ決める。
// 優先度 0 以上の利用者だけを塞いでいるとみなす (Mirakurun 自身の番組表取得 -1 は誰にでも取り上げられる)。
func (b *Bridge) keylessMayTune(ctx context.Context, typ, mkChannel string) (bool, error) {
	var list []mkTuner
	if err := b.mk(ctx, "/api/tuners", &list); err != nil {
		return false, err
	}
	mine := b.ours()
	held := 0
	sharing := map[string]bool{}
	for _, t := range list {
		var blocking []mkUser
		for _, u := range t.Users {
			if u.Priority >= 0 {
				blocking = append(blocking, u)
			}
		}
		for _, u := range blocking {
			if u.on(typ, mkChannel) {
				return true, nil
			}
		}
		if !slices.Contains(t.Types, typ) {
			continue
		}
		for _, s := range t.Types {
			sharing[s] = true
		}
		if len(blocking) > 0 && !slices.ContainsFunc(blocking, func(u mkUser) bool {
			a, ok := mine[u.ID]
			return !ok || !keyless(a.use)
		}) {
			held++
		}
	}
	waiting := map[pendingReq]bool{}
	b.mu.Lock()
	for p := range b.pending {
		if sharing[p.typ] {
			waiting[*p] = true
		}
	}
	b.mu.Unlock()
	return held+len(waiting) < b.cfg.KeylessTuners, nil
}

func (b *Bridge) stream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	typ, channel, use := q.Get("type"), q.Get("channel"), q.Get("use")
	if use == "" {
		use = "unknown"
	}
	priority, err := strconv.Atoi(q.Get("priority"))
	if q.Get("priority") == "" {
		priority, err = 0, nil
	}
	if !slices.Contains([]string{"GR", "BS", "CS", "SKY"}, typ) || channel == "" || err != nil {
		writeJSON(w, 400, map[string]string{"error": "type・channel・priority を確かめてください"})
		return
	}

	var mkChannels []MirakurunChannel
	if err := b.mk(r.Context(), "/api/channels", &mkChannels); err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	b.mu.Lock()
	mkChannel := toMirakurun(typ, channel, b.parsed, mkChannels)
	var leaving []*active
	for a := range b.active {
		if slices.Contains(q["after"], a.use) {
			leaving = append(leaving, a)
		}
	}
	b.mu.Unlock()
	if mkChannel == "" {
		writeJSON(w, 404, map[string]string{"error": fmt.Sprintf("Mirakurun に %s %s に当たるチャンネルがありません", typ, channel)})
		return
	}

	// 別のチャンネルへの引き継ぎは先に離して空きを見せる。同じチャンネルなら選局を共有したまま、繋がってから離す
	for _, a := range leaving {
		if a.typ != typ || a.mkChannel != mkChannel {
			a.handOff()
		}
	}
	release := sync.OnceFunc(func() {
		for _, a := range leaving {
			a.handOff()
		}
	})
	defer release()

	if keyless(use) {
		ok, err := b.keylessMayTune(r.Context(), typ, mkChannel)
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		if !ok {
			writeJSON(w, 409, map[string]string{"error": fmt.Sprintf("番組表・ロゴ・スキャンに使えるチューナーは %d 台までです (KEYLESS_TUNERS)", b.cfg.KeylessTuners)})
			return
		}
	}
	asking := &pendingReq{typ, mkChannel}
	if keyless(use) {
		b.mu.Lock()
		b.pending[asking] = struct{}{}
		b.mu.Unlock()
	}
	donePending := func() {
		b.mu.Lock()
		delete(b.pending, asking)
		b.mu.Unlock()
	}
	defer donePending()

	decode := "1"
	if keyless(use) {
		decode = "0"
	}
	mkPriority := mapPriority(priority, b.cfg.PriorityMap)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/channels/%s/%s/stream?decode=%s", b.cfg.MirakurunURL, typ, url.PathEscape(mkChannel), decode), nil)
	req.Header.Set("X-Mirakurun-Priority", strconv.Itoa(mkPriority))
	req.Header.Set("User-Agent", fmt.Sprintf("denpa-mirakurun-bridge/%s (%s)", b.cfg.Version, use))
	res, err := b.client.Do(req)
	release()
	if err != nil {
		if r.Context().Err() == nil {
			writeJSON(w, 502, map[string]string{"error": "Mirakurun に繋がりません: " + err.Error()})
		}
		return
	}
	defer res.Body.Close()
	donePending()

	if res.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(res.Body, 500))
		status := 500
		switch res.StatusCode {
		case 503: // Mirakurun の「空きが無い」。denpa は 409 を空き待ちとして掛け直す
			status = 409
		case 404:
			status = 404
		}
		b.logf("%s %s (%s, priority %d→%d) を開けません: Mirakurun %d", typ, channel, use, priority, mkPriority, res.StatusCode)
		writeJSON(w, status, map[string]string{"error": fmt.Sprintf("Mirakurun %d: %s", res.StatusCode, text)})
		return
	}

	me := &active{use: use, priority: priority, typ: typ, mkChannel: mkChannel,
		mkUserID: res.Header.Get("X-Mirakurun-Tuner-User-ID"), cancel: cancel}
	b.mu.Lock()
	b.active[me] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.active, me)
		b.mu.Unlock()
		b.emit("tuners")
	}()
	b.emit("tuners")

	w.Header().Set("Content-Type", "video/MP2T")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 64*1024)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	// Mirakurun 側から終わった (取り上げ・失敗) なら、正常終了と読まれないよう接続ごと壊す
	if !me.handedOff.Load() && r.Context().Err() == nil {
		panic(http.ErrAbortHandler)
	}
}

func (b *Bridge) putChannels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channels []json.RawMessage `json:"channels"`
		Scanned  []string          `json:"scanned"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "本文を JSON として読めません"})
		return
	}
	if body.Channels == nil || len(body.Scanned) == 0 {
		writeJSON(w, 400, map[string]string{"error": "channels と scanned が必要です"})
		return
	}
	// 空で上書きすると録れていた局まで消える (エージェントと同じ断り方)
	if len(body.Channels) == 0 {
		writeJSON(w, 400, map[string]string{"error": "チャンネルが 1 件もありません"})
		return
	}
	b.mu.Lock()
	var next []json.RawMessage
	for i, raw := range b.stored {
		if !slices.Contains(body.Scanned, b.parsed[i].Type) {
			next = append(next, raw)
		}
	}
	next = append(next, body.Channels...)
	data, _ := json.MarshalIndent(next, "", "  ")
	err := os.MkdirAll(filepath.Dir(b.cfg.ChannelsFile), 0o755)
	if err == nil {
		err = os.WriteFile(b.cfg.ChannelsFile+".tmp", data, 0o644)
	}
	if err == nil {
		err = os.Rename(b.cfg.ChannelsFile+".tmp", b.cfg.ChannelsFile)
	}
	if err == nil {
		b.setStored(next)
	}
	b.mu.Unlock()
	if err != nil {
		b.logf("チャンネルを保存できません: %v", err)
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	b.emit("channels")
	writeJSON(w, 200, next)
}

func (b *Bridge) events(w http.ResponseWriter, r *http.Request) {
	ch := make(chan string, 16)
	b.mu.Lock()
	b.listeners[ch] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.listeners, ch)
		b.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case name := <-ch:
			if _, err := fmt.Fprintf(w, "event: %s\ndata: {}\n\n", name); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !hostAllowed(r.Host, b.cfg.AllowedHosts) {
		b.logf("知らない名前「%s」で呼ばれたので断りました (403)。ALLOWED_HOSTS に足してください", r.Host)
		writeJSON(w, 403, map[string]string{"error": "unknown host"})
		return
	}
	// ブラウザが下調べ無しに送れる形 (text/plain など) の書き込みは受けない
	if (r.Method == "PUT" || r.Method == "POST") && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeJSON(w, 415, map[string]string{"error": "application/json だけ受け付けます"})
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /denpa/stream":
		b.stream(w, r)
	case "GET /denpa/tuners":
		list, err := b.tuners(r.Context())
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"tuners": list, "detected": false})
	case "PUT /denpa/tuners":
		writeJSON(w, 405, map[string]string{"error": "チューナーの定義は Mirakurun 側 (tuners.yml) で変えてください"})
	case "GET /denpa/channels":
		b.mu.Lock()
		list := b.stored
		b.mu.Unlock()
		if list == nil {
			list = []json.RawMessage{}
		}
		writeJSON(w, 200, list)
	case "PUT /denpa/channels":
		b.putChannels(w, r)
	case "GET /denpa/events":
		b.events(w, r)
	case "GET /denpa/card":
		// 復号は Mirakurun の decoder が選局時に行う
		writeJSON(w, 200, map[string]any{"ok": true, "message": "", "source": "local", "readers": []any{}})
	case "POST /denpa/decode":
		writeJSON(w, 501, map[string]any{"ok": false, "error": "録画後の復号には対応していません (復号は選局時に Mirakurun の decoder が行います)"})
	case "GET /denpa/logs":
		b.mu.Lock()
		lines := slices.Clone(b.logs)
		b.mu.Unlock()
		if lines == nil {
			lines = []logLine{}
		}
		writeJSON(w, 200, map[string]any{"lines": lines})
	case "GET /denpa/about":
		osName := runtime.GOOS
		if osName == "darwin" {
			osName = "macos"
		}
		arch := runtime.GOARCH
		if arch == "amd64" {
			arch = "x64"
		}
		writeJSON(w, 200, map[string]any{
			"version": b.cfg.Version, "os": osName, "arch": arch,
			"runtime": fmt.Sprintf("Go %s / Mirakurun %s", runtime.Version(), b.cfg.MirakurunURL), "ccidDriver": nil,
		})
	default:
		writeJSON(w, 404, map[string]string{"error": "not found"})
	}
}

// Mirakurun のチューナーの出来事を tuners として流す。中身は読まない (denpa が /denpa/tuners を読み直す)
func (b *Bridge) Watch(ctx context.Context) {
	wait := time.Second
	for ctx.Err() == nil {
		err := func() error {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.MirakurunURL+"/api/events/stream?resource=tuner", nil)
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
				b.emit("tuners")
			}
		}()
		if ctx.Err() != nil {
			return
		}
		b.logf("Mirakurun の通知に繋げません (%v 後に再試行): %v", wait, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Minute)
	}
}
