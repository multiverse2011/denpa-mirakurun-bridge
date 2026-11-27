// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// denpa の agent/Denpa.Agent/AgentTls.cs (MPL-2.0, https://github.com/danything/denpa) を Go に移したもの。

package main

import (
	"encoding/json"
	"io"
	"net/http"

	"denpa-mirakurun-bridge/internal/pairing"
)

func peerFingerprint(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	return pairing.Fingerprint(r.TLS.PeerCertificates[0].Raw)
}

// gate は組んだ相手だけを通す。POST /denpa/pair は組んでいなくても (組んだ相手の組み直しも) 受ける。通さなければ false
func (b *Bridge) gate(w http.ResponseWriter, r *http.Request) bool {
	p := b.cfg.Pairing
	if r.Method == "POST" && r.URL.Path == "/denpa/pair" {
		b.pair(w, r)
		return false
	}
	peer := peerFingerprint(r)
	ok := peer != "" && p.Paired(peer)
	if !ok {
		// denpa は paired: false を見て組み直す (文言は見ない)
		writeJSON(w, 403, map[string]any{"paired": false, "error": b.msg.t(
			"not paired with this denpa yet; add #pair=<token> to TUNER_AGENT_URL",
			"この denpa とはまだ組んでいません。TUNER_AGENT_URL に #pair=… を付けてください")})
	}
	return ok
}

func (b *Bridge) pair(w http.ResponseWriter, r *http.Request) {
	p := b.cfg.Pairing
	var body struct {
		Client *string `json:"client"`
		Proof  *string `json:"proof"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil || body.Client == nil || body.Proof == nil {
		writeJSON(w, 400, map[string]string{"error": b.msg.t("client and proof are required", "client と proof が必要です")})
		return
	}
	client := *body.Client
	if peer := peerFingerprint(r); peer == "" || client != peer {
		writeJSON(w, 403, map[string]string{"error": b.msg.t("the certificate does not match the client", "証明書が client の値と一致しません")})
		return
	}
	if !p.CheckProof(client, *body.Proof) {
		writeJSON(w, 403, map[string]string{"error": b.msg.t("wrong pairing token", "合言葉が違います")})
		return
	}
	added, err := p.Add(client)
	if err != nil {
		b.logf("cannot save paired denpa: %v", "組んだ denpa を保存できません: %v", err)
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if added {
		b.logf("paired with denpa (certificate %s…)", "denpa (証明書 %s…) と組みました", client[:min(16, len(client))])
	}
	writeJSON(w, 200, map[string]string{"proof": p.Proof("agent", client)})
}

// unpair は呼んできた denpa の指紋を clients から消す。denpa の設定でこのエージェントを削除したときに呼ばれる。
// 組んでいない相手は gate が断る。平文 (AGENT_TLS=0) では相手を見分けられないので断る
func (b *Bridge) unpair(w http.ResponseWriter, r *http.Request) {
	p := b.cfg.Pairing
	if p == nil {
		writeJSON(w, 403, map[string]string{"error": b.msg.t("pairing is available only with TLS (AGENT_TLS=1)",
			"ペアリングは TLS (AGENT_TLS=1) のときだけです")})
		return
	}
	peer := peerFingerprint(r)
	paired, err := p.Remove(peer)
	if !paired {
		writeJSON(w, 403, map[string]any{"paired": false, "error": b.msg.t(
			"not paired with this denpa", "この denpa とは組んでいません")})
		return
	}
	if err != nil {
		b.logf("cannot forget the paired denpa: %v", "組んだ denpa を忘れられません: %v", err)
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	b.logf("unpaired denpa (certificate %s…)", "denpa (証明書 %s…) との組を外しました", peer[:min(16, len(peer))])
	w.WriteHeader(204)
}
