package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"
)

// 偽の Mirakurun。選び方は Tuner._pickTunerDevice() をなぞる
// (同じチャンネルに相乗り → 空き → 利用者のいない選局 → 優先度の低い相手から取り上げ)。
// 本物は 503 の前に 250ms × 50 回選び直すが、試験ではすぐ返す。
type mockUser struct {
	id       string
	priority int
	agent    string
	kick     chan struct{}
}

type mockDevice struct {
	name    string
	types   []string
	typ, ch string
	users   []*mockUser
}

type mockRequest struct {
	Channel  string
	Priority int
	Decode   string
	Agent    string
}

type mockMirakurun struct {
	*httptest.Server
	mu       sync.Mutex
	devices  []*mockDevice
	channels []MirakurunChannel
	requests []mockRequest
	events   []chan struct{}
	seq      int
	stall    bool // 最初の塊のあと何も送らない (チューナーが固まった)
	burst    bool // 大きな塊を休まず送る (受け手が読まないとすぐ詰まる)
}

func newMock(tuners [][]string, channels []MirakurunChannel) *mockMirakurun {
	m := &mockMirakurun{channels: channels}
	for i, types := range tuners {
		m.devices = append(m.devices, &mockDevice{name: fmt.Sprintf("T%d", i), types: types})
	}
	m.Server = httptest.NewServer(http.HandlerFunc(m.serve))
	return m
}

func (m *mockMirakurun) users(i int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.devices[i].users)
}

func (m *mockMirakurun) lastRequest() mockRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests[len(m.requests)-1]
}

func maxPriority(d *mockDevice) int {
	p := -2
	for _, u := range d.users {
		p = max(p, u.priority)
	}
	return p
}

func (m *mockMirakurun) pick(typ, ch string, priority int) *mockDevice {
	var usable []*mockDevice
	for _, d := range m.devices {
		if slices.Contains(d.types, typ) {
			usable = append(usable, d)
		}
	}
	for _, d := range usable {
		if d.typ == typ && d.ch == ch {
			return d
		}
	}
	for _, d := range usable {
		if d.ch == "" {
			return d
		}
	}
	for _, d := range usable {
		if len(d.users) == 0 {
			return d
		}
	}
	slices.SortStableFunc(usable, func(a, b *mockDevice) int { return maxPriority(a) - maxPriority(b) })
	for _, d := range usable {
		if len(d.users) > 0 && maxPriority(d) < priority {
			return d
		}
	}
	return nil
}

func (m *mockMirakurun) changed() {
	for _, e := range m.events {
		select {
		case e <- struct{}{}:
		default:
		}
	}
}

var reMockStream = regexp.MustCompile(`^/api/channels/(\w+)/([^/]+)/stream$`)

func (m *mockMirakurun) serve(w http.ResponseWriter, r *http.Request) {
	send := func(status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	switch r.URL.Path {
	case "/api/channels":
		send(200, m.channels)
		return
	case "/api/tuners":
		m.mu.Lock()
		var out []map[string]any
		for i, d := range m.devices {
			var users []map[string]any
			for _, u := range d.users {
				users = append(users, map[string]any{"id": u.id, "priority": u.priority, "agent": u.agent,
					"streamSetting": map[string]any{"channel": map[string]string{"type": d.typ, "channel": d.ch}}})
			}
			out = append(out, map[string]any{"index": i, "name": d.name, "types": d.types, "users": users,
				"isAvailable": true, "isFault": false})
		}
		m.mu.Unlock()
		send(200, out)
		return
	case "/api/events/stream":
		e := make(chan struct{}, 16)
		m.mu.Lock()
		m.events = append(m.events, e)
		m.mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte("[\n"))
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-e:
				_, _ = w.Write([]byte(`{"resource":"tuner","type":"update"}` + "\n,\n"))
				w.(http.Flusher).Flush()
			}
		}
	}
	match := reMockStream.FindStringSubmatch(r.URL.EscapedPath())
	if match == nil {
		send(404, map[string]string{"error": "not found"})
		return
	}
	typ := match[1]
	ch, _ := url.PathUnescape(match[2])
	priority, _ := strconv.Atoi(r.Header.Get("X-Mirakurun-Priority"))

	m.mu.Lock()
	m.requests = append(m.requests, mockRequest{ch, priority, r.URL.Query().Get("decode"), r.Header.Get("User-Agent")})
	if !slices.ContainsFunc(m.channels, func(c MirakurunChannel) bool { return c.Type == typ && c.Channel == ch }) {
		m.mu.Unlock()
		send(404, map[string]string{"error": "Not Found"})
		return
	}
	d := m.pick(typ, ch, priority)
	if d == nil {
		m.mu.Unlock()
		send(503, map[string]any{"code": 503, "reason": "Tuner Resource Unavailable"})
		return
	}
	if d.typ != typ || d.ch != ch {
		// 選局し直す。いた利用者は Mirakurun と同じく、ストリームを (きれいに) 終わらせる
		for _, u := range d.users {
			close(u.kick)
		}
		d.users, d.typ, d.ch = nil, typ, ch
	}
	m.seq++
	u := &mockUser{id: fmt.Sprintf("127.0.0.1:%d", m.seq), priority: priority, agent: r.Header.Get("User-Agent"), kick: make(chan struct{})}
	d.users = append(d.users, u)
	m.changed()
	stall, burst := m.stall, m.burst
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		d.users = slices.DeleteFunc(d.users, func(x *mockUser) bool { return x == u })
		m.changed()
		m.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "video/MP2T")
	w.Header().Set("X-Mirakurun-Tuner-User-ID", u.id)
	w.WriteHeader(200)
	size, every := 188*10, 20*time.Millisecond
	if burst {
		size, every = 188*1000, time.Millisecond
	}
	packet := make([]byte, size)
	for i := range packet {
		packet[i] = 0x47
	}
	if stall {
		_, _ = w.Write(packet)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-u.kick:
		}
		return
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if _, err := w.Write(packet); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			return
		case <-u.kick:
			return
		case <-tick.C:
		}
	}
}
