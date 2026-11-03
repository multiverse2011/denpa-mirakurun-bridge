package main

import (
	"regexp"
	"strconv"
)

// DenpaChannel は denpa が PUT /denpa/channels で預ける局のうち、対応付けに使う項目。
type DenpaChannel struct {
	Type      string `json:"type"`
	Channel   string `json:"channel"`
	NetworkID int    `json:"networkId"`
	Services  []struct {
		ServiceID int `json:"serviceId"`
	} `json:"services"`
}

type MirakurunChannel struct {
	Type     string `json:"type"`
	Channel  string `json:"channel"`
	Services []struct {
		ServiceID int `json:"serviceId"`
		NetworkID int `json:"networkId"`
	} `json:"services"`
}

var (
	reUHF         = regexp.MustCompile(`^T0*(\d+)$`)
	reDigits      = regexp.MustCompile(`^\d+$`)
	reSatellite   = regexp.MustCompile(`^(?:BS|CS)0*(\d+)(:\d+)?$`)
	reTransponder = regexp.MustCompile(`(?i)^(?:BS|CS)?0*(\d+)(?:\D|$)`)
)

func sameStream(d DenpaChannel, m MirakurunChannel) bool {
	if d.Type != m.Type {
		return false
	}
	for _, ms := range m.Services {
		for _, ds := range d.Services {
			if ms.NetworkID == d.NetworkID && ms.ServiceID == ds.ServiceID {
				return true
			}
		}
	}
	return false
}

func transponder(name string) int {
	m := reTransponder.FindStringSubmatch(name)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func number(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// toMirakurun は denpa のチャンネル名を Mirakurun のチャンネル名に引く。無ければ ""。
// 地上波は物理チャンネル番号で引く (中継局は別のチャンネルで同じサービスを流す)。
// 衛星は Mirakurun が TSID を出さないので、預かった局のサービスで引く。
func toMirakurun(typ, name string, stored []DenpaChannel, mk []MirakurunChannel) string {
	var ofType []MirakurunChannel
	for _, c := range mk {
		if c.Type == typ {
			ofType = append(ofType, c)
		}
	}
	exact := func() string {
		for _, c := range ofType {
			if c.Channel == name {
				return c.Channel
			}
		}
		return ""
	}

	if typ == "GR" {
		if m := reUHF.FindStringSubmatch(name); m != nil {
			for _, c := range ofType {
				if reDigits.MatchString(c.Channel) && number(c.Channel) == number(m[1]) {
					return c.Channel
				}
			}
		}
		return exact()
	}

	for _, d := range stored {
		if d.Type == typ && d.Channel == name {
			for _, c := range ofType {
				if sameStream(d, c) {
					return c.Channel
				}
			}
		}
	}

	m := reSatellite.FindStringSubmatch(name)
	if m == nil {
		return exact()
	}
	// BS は 1 中継器に TS が複数ある。違う TS を流すより断る
	if m[2] != "" && typ == "BS" {
		return ""
	}
	for _, c := range ofType {
		if transponder(c.Channel) == number(m[1]) {
			return c.Channel
		}
	}
	return ""
}

func toDenpa(typ, mkName string, stored []DenpaChannel, mk []MirakurunChannel) string {
	if typ == "GR" {
		if reDigits.MatchString(mkName) {
			return "T" + strconv.Itoa(number(mkName))
		}
		return mkName
	}
	for _, c := range mk {
		if c.Type == typ && c.Channel == mkName {
			for _, d := range stored {
				if sameStream(d, c) {
					return d.Channel
				}
			}
		}
	}
	return mkName
}
