package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"denpa-mirakurun-bridge/internal/pairing"
)

type Config struct {
	MirakurunURL  string
	PriorityMap   map[int]int
	ChannelsFile  string
	AllowedHosts  []string
	Version       string
	AgentName     string // /denpa/about の name。空なら返さない (denpa は URL のホスト名で埋める)
	KeylessTuners int
	EPGMode       string // "broadcast" (既定) か "synthetic" (実験的: use=epg を Mirakurun の番組表から合成)
	DecodeKeyless bool   // 番組表・ロゴ・スキャンでも decode=1 にする (上流が decode の異なる要求を共有しないとき用)
	English       bool
	Pairing       *pairing.Pairing // nil なら平文の HTTP (v2.27.0 以前の denpa 向け)
}

// messages は応答とログの言葉を選ぶ。コードには英語を先に書き、既定 (en=false) では日本語を出す
type messages struct{ en bool }

func (m messages) t(en, ja string, args ...any) string {
	if !m.en {
		en = ja
	}
	return fmt.Sprintf(en, args...)
}

func parsePriorityMap(text string, m messages) (map[int]int, error) {
	out := map[int]int{}
	for _, pair := range strings.Split(text, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		from, to, ok := strings.Cut(pair, ":")
		f, err1 := strconv.Atoi(from)
		t, err2 := strconv.Atoi(to)
		if !ok || err1 != nil || err2 != nil {
			return nil, errors.New(m.t("cannot parse PRIORITY_MAP: %s (e.g. 10:2,9:1)", "PRIORITY_MAP を読めません: %s (例: 10:2,9:1)", pair))
		}
		out[f] = t
	}
	return out, nil
}

// Mirakurun の HTTP API は 0 未満を 400 で断る
func mapPriority(p int, m map[int]int) int {
	if v, ok := m[p]; ok {
		p = v
	}
	return max(p, 0)
}
