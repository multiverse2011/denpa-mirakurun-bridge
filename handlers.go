package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"denpa-mirakurun-bridge/internal/atomicfile"
)

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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (b *Bridge) putChannels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channels []json.RawMessage `json:"channels"`
		Scanned  []string          `json:"scanned"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": b.msg.t("cannot parse the body as JSON", "本文を JSON として読めません")})
		return
	}
	if body.Channels == nil || len(body.Scanned) == 0 {
		writeJSON(w, 400, map[string]string{"error": b.msg.t("channels and scanned are required", "channels と scanned が必要です")})
		return
	}
	// 空で上書きすると録れていた局まで消える (エージェントと同じ断り方)
	if len(body.Channels) == 0 {
		writeJSON(w, 400, map[string]string{"error": b.msg.t("no channels given", "チャンネルが 1 件もありません")})
		return
	}
	// 保存は saveMu で 1 本ずつ通し、b.mu はディスク I/O (fsync) の間握らない
	b.saveMu.Lock()
	defer b.saveMu.Unlock()
	b.mu.Lock()
	var next []json.RawMessage
	for i, raw := range b.stored {
		if !slices.Contains(body.Scanned, b.parsed[i].Type) {
			next = append(next, raw)
		}
	}
	b.mu.Unlock()
	next = append(next, body.Channels...)
	data, _ := json.MarshalIndent(next, "", "  ")
	err := os.MkdirAll(filepath.Dir(b.cfg.ChannelsFile), 0o755)
	if err == nil {
		err = atomicfile.WriteSynced(b.cfg.ChannelsFile+".tmp", data, 0o644)
	}
	if err == nil {
		err = os.Rename(b.cfg.ChannelsFile+".tmp", b.cfg.ChannelsFile)
	}
	if err == nil {
		err = atomicfile.SyncDir(filepath.Dir(b.cfg.ChannelsFile))
	}
	if err == nil {
		b.mu.Lock()
		b.setStored(next)
		b.mu.Unlock()
	}
	if err != nil {
		b.logf("cannot save channels: %v", "チャンネルを保存できません: %v", err)
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	b.emit("channels")
	writeJSON(w, 200, next)
}

func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// TLS ではクライアント証明書が要り、ブラウザは繋げないので、名前と Content-Type は見ない (denpa のエージェントと同じ)
	if b.cfg.Pairing != nil {
		if !b.gate(w, r) {
			return
		}
	} else if !hostAllowed(r.Host, b.cfg.AllowedHosts) {
		b.logf("refused unknown host %q (403); add it to ALLOWED_HOSTS",
			"知らないホスト名「%s」で呼ばれたので断りました (403)。ALLOWED_HOSTS に足してください", r.Host)
		writeJSON(w, 403, map[string]string{"error": "unknown host"})
		return
	}
	// ブラウザが下調べ無しに送れる形 (text/plain など) の書き込みは受けない
	if b.cfg.Pairing == nil && (r.Method == "PUT" || r.Method == "POST") && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeJSON(w, 415, map[string]string{"error": b.msg.t("only application/json is accepted", "application/json だけ受け付けます")})
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
		writeJSON(w, 405, map[string]string{"error": b.msg.t("change tuners in Mirakurun (tuners.yml)",
			"チューナーの定義は Mirakurun 側 (tuners.yml) で変えてください")})
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
		// 復号は Mirakurun の decoder が選局時に行う。カードの番号が無いと denpa は復号の結果を書く先が無く、
		// 有料の局をずっと「確かめていない」とする (自動予約も録らない)。そこで仮のカードを 1 枚名乗る。
		// 契約はカードに聞けない (confirm は 501) ので、denpa は「復号を確かめる」と、復号できた録画をエンコードしたときに局を契約中にする。
		// denpa にはカードを持つエージェントに見えるので、有料の局の振り分けでも候補に入る (契約中と確かめた局が先)。
		// source は local にする (remote だと denpa は readers の ids を見ない)。名前の括弧は denpa が名前の下に小さく出す
		writeJSON(w, 200, map[string]any{"ok": true, "source": "local", "message": "", "readers": []any{map[string]any{
			"name": b.msg.t("Mirakurun decoder (stand-in card)", "Mirakurun の decoder (仮のカード)"),
			"card": true, "active": true, "ids": []string{b.cardID}, "tuners": []string{},
		}}})
	case "POST /denpa/decode":
		writeJSON(w, 501, map[string]any{"ok": false, "error": b.msg.t("decoding after recording is not supported (Mirakurun's decoder decodes while tuning)",
			"録画後の復号には対応していません (復号は選局時に Mirakurun の decoder が行います)")})
	case "DELETE /denpa/pair":
		b.unpair(w, r)
	case "POST /denpa/raw/probe":
		// 元の TS の置き場は見ない (録画後に復号しない)。denpa は 404 で本文が "not found" 以外のときだけ
		// 「置き場が見えない」とし、それ以外の失敗は「確かめられない (null)」として扱う (scramble.ts の probeRaw)。
		// 501 にしておくと、設定の「受信」に警告が出ない
		writeJSON(w, 501, map[string]string{"error": b.msg.t("the bridge does not read the raw TS directory (it does not decode after recording)",
			"ブリッジは元の TS の置き場を読みません (録画後の復号をしないため)")})
	case "PUT /denpa/card/readers":
		// 仮のリーダーは止められない。本文は denpa の設定画面にそのまま出る (scramble.ts の putCardReaders)
		writeJSON(w, 405, map[string]string{"error": b.msg.t("the bridge's card reader is a stand-in; decoding is done by Mirakurun's decoder",
			"ブリッジのカードリーダーは仮のものです。復号は Mirakurun の decoder が行います")})
	case "POST /denpa/card/confirm":
		// 仮のカードなので契約は読めない。denpa は番組表に契約の情報 (0xCB) がある局でだけ呼び、失敗はそのまま見せる
		// (contracts.ts の confirmCardContract)。そうした局も「復号を確かめる」で契約中にできる。
		// 12 時間ごとの定期の確認でも出るので、仮のカードだからだと分かる文にする。ボタン名は denpa の画面のまま
		writeJSON(w, 501, map[string]any{"ok": false, "error": b.msg.t("the bridge's card is a stand-in and cannot answer contract queries; use the decode check (「復号を確かめる」) while a paid program is airing",
			"ブリッジのカードは仮のもので、契約に答えられません。有料の番組を流している間に「復号を確かめる」で確かめてください")})
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
		out := map[string]any{
			"version": b.cfg.Version, "os": osName, "arch": arch,
			"runtime": fmt.Sprintf("denpa-mirakurun-bridge %s / Go %s / %s", version, runtime.Version(),
				b.msg.t("upstream %s", "上流 %s", b.cfg.MirakurunURL)),
			"ccidDriver": nil,
		}
		if name := strings.TrimSpace(b.cfg.AgentName); name != "" {
			out["name"] = name
		}
		writeJSON(w, 200, out)
	default:
		writeJSON(w, 404, map[string]string{"error": "not found"})
	}
}
