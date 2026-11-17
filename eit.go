package main

// EPG_MODE=synthetic で、Mirakurun の番組表 (/api/programs) から EIT・PAT・SDT を組み立て、
// チューナーを使わずに use=epg へ返すための符号化。ARIB STD-B10/B24 の必要な部分だけを書いている。

import (
	"encoding/binary"
	"sort"
	"time"
)

// 合成する TS の PID と table_id
const (
	pidPAT = 0x0000
	pidSDT = 0x0011
	pidEIT = 0x0012

	tablePF       = 0x4e
	tableSchedule = 0x50 // 0x50〜0x51 の 2 本で 8 日ぶん (1 本 = 32 セグメント = 4 日)
	tableLast     = tableSchedule + 1
	tableSDT      = 0x42

	// 1 つの table_id が持つセグメント (3 時間) の数。denpa の ScheduleProgress と同じ数え方
	segmentsPerTable = 32
	segmentSpanMS    = int64(3 * time.Hour / time.Millisecond)

	// EIT の 1 セクションに入る番組のバイト数 (section_length 4093 - 残りのヘッダ 11 - CRC 4)
	eitEventCapacity = 4078
	// 番組 1 つの記述子の上限。超えた詳細は切り捨てる
	// shortcut: 3 KB を超える詳細は捨てる、実機で出たら拡張表 (0x58〜) に分ける
	descriptorBudget = 3000
)

// epgProgram は Mirakurun の /api/programs から使う項目だけ
type epgProgram struct {
	EventID     int               `json:"eventId"`
	ServiceID   int               `json:"serviceId"`
	NetworkID   int               `json:"networkId"`
	StartAt     int64             `json:"startAt"`
	Duration    int64             `json:"duration"`
	IsFree      bool              `json:"isFree"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Extended    map[string]string `json:"extended"`
	Genres      []epgGenre        `json:"genres"`
	Video       *epgVideo         `json:"video"`
	Audios      []epgAudio        `json:"audios"`
}

type epgGenre struct {
	Lv1 int `json:"lv1"`
	Lv2 int `json:"lv2"`
	Un1 int `json:"un1"`
	Un2 int `json:"un2"`
}

type epgVideo struct {
	StreamContent int `json:"streamContent"`
	ComponentType int `json:"componentType"`
}

type epgAudio struct {
	ComponentType int      `json:"componentType"`
	ComponentTag  int      `json:"componentTag"`
	IsMain        bool     `json:"isMain"`
	SamplingRate  int      `json:"samplingRate"`
	Langs         []string `json:"langs"`
}

// epgService は合成する TS に載せる局
type epgService struct {
	NetworkID int
	ServiceID int
	Type      int
	Name      string
}

// aribAliases は JIS と Unicode で食い違う字。WHATWG の EUC-JP が返す字から引く
var aribAliases = map[rune]rune{
	'〜': '～', // 〜
	'−': '－', // −
	'¥': '￥', // ¥
	'‾': '￣', // ‾
	'‖': '∥', // ‖
}

// encodeArib は文字列を ARIB STD-B24 の 8 単位符号にする。cuts は文字の切れ目 (継続部分に分けるときに使う)。
// 初期状態は漢字 (denpa の synth.ts と同じ)。英数は MSZ で半角にしてから ESC ( J で入る。
func encodeArib(s string) (out []byte, cuts []int) {
	const (
		modeKanji = iota
		modeAlnum
		modeKana
	)
	mode, half := modeKanji, false
	put := func(bs ...byte) { out = append(out, bs...) }
	set := func(to int) {
		if to == modeAlnum {
			if mode == modeAlnum {
				return
			}
			put(0x1b, '(', 'J')
			if !half {
				put(0x89) // MSZ
				half = true
			}
			mode = modeAlnum
			return
		}
		if mode == to && !half {
			return
		}
		if to == modeKana {
			if half {
				put(0x8a) // NSZ
				half = false
			}
			put(0x1b, '(', 'I')
		} else {
			put(0x1b, '$', 'B')
			if half {
				put(0x8a)
				half = false
			}
		}
		mode = to
	}
	for _, r := range s {
		switch {
		case r < 0x80:
			set(modeAlnum)
			put(byte(r))
		case r >= 0xff61 && r <= 0xff9f: // 半角カナ
			set(modeKana)
			put(byte(0xa1 + r - 0xff61))
		default:
			set(modeKanji)
			code, ok := jis0208[r]
			if !ok {
				if a, found := aribAliases[r]; found {
					code, ok = jis0208[a]
				}
			}
			if !ok {
				code = jis0208['？'] // 表に無い字は全角の疑問符にする
			}
			put(byte(code>>8), byte(code))
		}
		cuts = append(cuts, len(out))
	}
	return out, cuts
}

// truncateArib は符号化した文字列を max バイト以下に切る。文字の途中では切らない
func truncateArib(s string, max int) []byte {
	enc, cuts := encodeArib(s)
	n := 0
	for _, c := range cuts {
		if c > max {
			break
		}
		n = c
	}
	return enc[:n]
}

// splitArib は符号化した文字列を文字の切れ目で max バイト前後に分ける
func splitArib(s string, max int) [][]byte {
	enc, cuts := encodeArib(s)
	if len(enc) == 0 {
		return nil
	}
	var parts [][]byte
	start := 0
	for _, c := range cuts {
		if c-start >= max {
			parts = append(parts, enc[start:c])
			start = c
		}
	}
	if start < len(enc) {
		parts = append(parts, enc[start:])
	}
	return parts
}

// sizedText は長さ 1 バイト付きの文字列 (SI の記述子の形)
func sizedText(b []byte) []byte {
	return append([]byte{byte(len(b))}, b...)
}

// shortEventDescriptor は番組名と短い説明。名前を優先し、入りきらない説明は切る
func shortEventDescriptor(name, description string) []byte {
	nameBytes := truncateArib(name, 250)
	descBytes := truncateArib(description, 250-len(nameBytes))
	body := []byte{'j', 'p', 'n', byte(len(nameBytes))}
	body = append(body, nameBytes...)
	body = append(body, byte(len(descBytes)))
	body = append(body, descBytes...)
	return append([]byte{0x4d, byte(len(body))}, body...)
}

// extendedEventDescriptors は詳細の項目。1 つの記述子に 255 バイトしか入らないので、
// 長い項目は同じ見出しの継続 (見出し長 0) に分けて、descriptor_number を振る
func extendedEventDescriptors(extended map[string]string) [][]byte {
	keys := make([]string, 0, len(extended))
	for k := range extended {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	type item struct{ heading, text []byte }
	var items []item
	for _, k := range keys {
		heading := truncateArib(k, 60)
		parts := splitArib(extended[k], 150)
		if len(parts) == 0 {
			parts = [][]byte{{}}
		}
		for i, part := range parts {
			if i == 0 {
				items = append(items, item{heading, part})
			} else {
				items = append(items, item{nil, part})
			}
		}
	}
	var groups [][]byte
	var cur []byte
	flush := func() {
		if len(cur) > 0 {
			groups = append(groups, cur)
			cur = nil
		}
	}
	for _, it := range items {
		b := append(sizedText(it.heading), sizedText(it.text)...)
		if len(cur)+len(b) > 250 {
			flush()
		}
		cur = append(cur, b...)
	}
	flush()
	// shortcut: descriptor_number は 4 ビットなので 16 本まで、超えた詳細は捨てる
	if len(groups) > 16 {
		groups = groups[:16]
	}
	var out [][]byte
	for i, g := range groups {
		body := []byte{byte(i<<4) | byte(len(groups)-1), 'j', 'p', 'n', byte(len(g))}
		body = append(body, g...)
		body = append(body, 0) // 項目の後ろの自由記述は使わない
		out = append(out, append([]byte{0x4e, byte(len(body))}, body...))
	}
	return out
}

// contentDescriptor はジャンル (大分類・中分類)
func contentDescriptor(genres []epgGenre) []byte {
	var body []byte
	for _, g := range genres {
		if len(body)+2 > 254 {
			break
		}
		body = append(body, byte((g.Lv1&0x0f)<<4|(g.Lv2&0x0f)), byte((g.Un1&0x0f)<<4|(g.Un2&0x0f)))
	}
	if len(body) == 0 {
		return nil
	}
	return append([]byte{0x54, byte(len(body))}, body...)
}

// componentDescriptor は映像。denpa は stream_content から符号を、component_type の上位から解像度を読む
func componentDescriptor(streamContent, componentType int) []byte {
	return []byte{0x50, 0x06, 0xf0 | byte(streamContent&0x0f), byte(componentType), 0x00, 'j', 'p', 'n'}
}

func samplingRateCode(rate int) byte {
	switch rate {
	case 16000:
		return 1
	case 22050:
		return 2
	case 24000:
		return 3
	case 32000:
		return 5
	case 44100:
		return 6
	default:
		return 7 // 48000 (分からなければ denpa の synth と同じ)
	}
}

func lang3(langs []string, i int) string {
	if i < len(langs) && len(langs[i]) == 3 {
		return langs[i]
	}
	return "jpn"
}

// audioDescriptors は音声の一覧
func audioDescriptors(audios []epgAudio) []byte {
	var out []byte
	for i, a := range audios {
		if i >= 8 {
			break
		}
		multi := len(a.Langs) > 1
		flags := samplingRateCode(a.SamplingRate) << 1
		if multi {
			flags |= 0x80
		}
		if a.IsMain {
			flags |= 0x40
		}
		tag := a.ComponentTag
		if tag == 0 {
			tag = 0x10 + i
		}
		body := []byte{0x02, byte(a.ComponentType), byte(tag), 0x0f, 0xff, flags}
		body = append(body, lang3(a.Langs, 0)...)
		if multi {
			body = append(body, lang3(a.Langs, 1)...)
		}
		out = append(out, 0xc4, byte(len(body)))
		out = append(out, body...)
	}
	return out
}

// eitEventBytes は EIT の event 1 つ (見出し + 記述子)
func eitEventBytes(p epgProgram, runningStatus int) []byte {
	var d []byte
	add := func(b []byte) {
		if len(d)+len(b) <= descriptorBudget {
			d = append(d, b...)
		}
	}
	add(shortEventDescriptor(p.Name, p.Description))
	for _, b := range extendedEventDescriptors(p.Extended) {
		add(b)
	}
	add(contentDescriptor(p.Genres))
	if p.Video != nil {
		add(componentDescriptor(p.Video.StreamContent, p.Video.ComponentType))
	}
	add(audioDescriptors(p.Audios))

	free := byte(0)
	if !p.IsFree {
		free = 0x10 // free_CA_mode: 有料
	}
	body := []byte{byte(p.EventID >> 8), byte(p.EventID)}
	body = append(body, mjdTime(p.StartAt)...)
	body = append(body, bcdDuration(p.Duration)...)
	body = append(body, byte(runningStatus<<5)|free|byte(len(d)>>8)&0x0f, byte(len(d)))
	return append(body, d...)
}

func bcd(v int) byte { return byte((v/10)<<4 | v%10) }

// mjdTime は epoch ms を MJD + BCD の 5 バイトにする。放送の時刻は日本時間で書く
func mjdTime(ms int64) []byte {
	jst := ms/1000 + 9*3600
	mjd := jst/86400 + 40587
	rest := jst % 86400
	return []byte{byte(mjd >> 8), byte(mjd), bcd(int(rest / 3600)), bcd(int(rest/60) % 60), bcd(int(rest % 60))}
}

// bcdDuration は尺 (ms) を BCD の 3 バイトにする
func bcdDuration(ms int64) []byte {
	total := ms / 1000
	return []byte{bcd(int(total / 3600)), bcd(int(total/60) % 60), bcd(int(total % 60))}
}

var crcTable = func() [256]uint32 {
	var t [256]uint32
	for i := range t {
		c := uint32(i) << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04c11db7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}()

// crc32MPEG は MPEG-2 の CRC32。CRC を含めて回すと 0 になる
func crc32MPEG(data []byte) uint32 {
	crc := uint32(0xffffffff)
	for _, b := range data {
		crc = crc<<8 ^ crcTable[byte(crc>>24)^b]
	}
	return crc
}

// withCRC は section_length を埋めて CRC を付ける
func withCRC(body []byte) []byte {
	length := len(body) - 3 + 4
	body[1] = 0xb0 | byte(length>>8)&0x0f
	body[2] = byte(length)
	out := make([]byte, len(body)+4)
	copy(out, body)
	binary.BigEndian.PutUint32(out[len(body):], crc32MPEG(out[:len(body)]))
	return out
}

// psiSection は PAT・SDT の共通部分を組む。extension は PAT と SDT では transport_stream_id
func psiSection(tableID byte, extension int, section, lastSection byte, rest []byte) []byte {
	body := []byte{tableID, 0, 0, byte(extension >> 8), byte(extension), 0xc1, section, lastSection}
	body = append(body, rest...)
	return withCRC(body)
}

func patSection(tsid int, services []epgService) []byte {
	var rest []byte
	for i, s := range services {
		pmt := 0x1000 + i // denpa は番組表の取得で PMT を読まないので、ぶつからなければよい
		if pmt > 0x1fff {
			break
		}
		rest = append(rest, byte(s.ServiceID>>8), byte(s.ServiceID), 0xe0|byte(pmt>>8), byte(pmt))
	}
	return psiSection(0x00, tsid, 0, 0, rest)
}

func sdtSection(tsid, onid int, services []epgService) []byte {
	rest := []byte{byte(onid >> 8), byte(onid), 0xff}
	for _, s := range services {
		name := truncateArib(s.Name, 200)
		payload := append([]byte{byte(s.Type), 0x00}, sizedText(name)...)
		desc := append([]byte{0x48, byte(len(payload))}, payload...)
		entry := []byte{byte(s.ServiceID >> 8), byte(s.ServiceID), 0xfc, 0xf0 | byte(len(desc)>>8)&0x0f, byte(len(desc))}
		entry = append(entry, desc...)
		// shortcut: SDT は 1 セクションに収まるぶんだけ載せる (denpa は最初の SDT しか読まない)
		if len(rest)+len(entry) > 4070 {
			break
		}
		rest = append(rest, entry...)
	}
	return psiSection(tableSDT, tsid, 0, 0, rest)
}

// eitSectionBytes は EIT のセクション 1 本
func eitSectionBytes(tableID byte, serviceID, tsid, onid int, section, lastSection, segmentLast, lastTable byte, events [][]byte) []byte {
	body := []byte{tableID, 0, 0,
		byte(serviceID >> 8), byte(serviceID),
		0xc1, section, lastSection,
		byte(tsid >> 8), byte(tsid),
		byte(onid >> 8), byte(onid),
		segmentLast, lastTable}
	for _, e := range events {
		body = append(body, e...)
	}
	return withCRC(body)
}

// packEvents は 1 セグメント (3 時間) の番組をセクションの大きさに分ける。空でも 1 本返す
func packEvents(events []epgProgram, runningStatus int) [][]byte {
	var out [][]byte
	var cur []byte
	for _, p := range events {
		e := eitEventBytes(p, runningStatus)
		if len(cur)+len(e) > eitEventCapacity {
			if len(cur) > 0 {
				out = append(out, cur)
				cur = nil
			}
			if len(e) > eitEventCapacity {
				continue
			}
		}
		cur = append(cur, e...)
	}
	if len(cur) > 0 || len(out) == 0 {
		out = append(out, cur)
	}
	return out
}

// jstMidnight は now を含む日の日本時間 0:00 (epoch ms)
func jstMidnight(ms int64) int64 {
	jst := ms/1000 + 9*3600
	return (jst/86400*86400 - 9*3600) * 1000
}

func presentFollowing(programs []epgProgram, now int64) (*epgProgram, *epgProgram) {
	sorted := make([]epgProgram, 0, len(programs))
	for _, p := range programs {
		if p.StartAt > 0 && p.Duration > 0 {
			sorted = append(sorted, p)
		}
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].StartAt != sorted[j].StartAt {
			return sorted[i].StartAt < sorted[j].StartAt
		}
		return sorted[i].EventID < sorted[j].EventID
	})
	var present, following *epgProgram
	for i := range sorted {
		p := &sorted[i]
		if p.StartAt <= now && now < p.StartAt+p.Duration {
			present = p
			if i+1 < len(sorted) {
				following = &sorted[i+1]
			}
			break
		}
	}
	if present == nil {
		for i := range sorted {
			if sorted[i].StartAt > now {
				following = &sorted[i]
				break
			}
		}
	}
	return present, following
}

// serviceEITSections は 1 局ぶんの EIT を組む。p/f (0x4E) と、いまから 8 日先までの
// schedule (0x50・0x51)。来ないセグメントを denpa が待ち続けるので、番組の無いセグメントも
// 空のセクションを 1 本だけ流す
func serviceEITSections(svc epgService, programs []epgProgram, now int64) [][]byte {
	midnight := jstMidnight(now)
	segment := func(at int64) int { return int((at - midnight) / segmentSpanMS) }
	current := segment(now)

	sorted := make([]epgProgram, 0, len(programs))
	for _, p := range programs {
		if p.StartAt > 0 && p.Duration > 0 {
			sorted = append(sorted, p)
		}
	}
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].StartAt != sorted[j].StartAt {
			return sorted[i].StartAt < sorted[j].StartAt
		}
		return sorted[i].EventID < sorted[j].EventID
	})
	bySegment := map[int][]epgProgram{}
	for _, p := range sorted {
		s := segment(p.StartAt)
		if s < current || s >= 2*segmentsPerTable {
			continue
		}
		bySegment[s] = append(bySegment[s], p)
	}

	var out [][]byte
	for t := 0; t < 2; t++ {
		for i := 0; i < segmentsPerTable; i++ {
			s := t*segmentsPerTable + i
			if s < current {
				continue // 過ぎたセグメントは放送でも流れてこない
			}
			blobs := packEvents(bySegment[s], 0)
			// shortcut: 1 セグメント 8 セクション (section_number の 3 ビット) を超える量は捨てる
			if len(blobs) > 8 {
				blobs = blobs[:8]
			}
			for j, blob := range blobs {
				out = append(out, eitSectionBytes(byte(tableSchedule+t), svc.ServiceID, svc.NetworkID, svc.NetworkID,
					byte(i*8+j), 248, byte(i*8+len(blobs)-1), tableLast, [][]byte{blob}))
			}
		}
	}

	if present, following := presentFollowing(sorted, now); present != nil {
		last := byte(0)
		if following != nil {
			last = 1
		}
		out = append(out, eitSectionBytes(tablePF, svc.ServiceID, svc.NetworkID, svc.NetworkID,
			0, last, last, tableLast, [][]byte{eitEventBytes(*present, 4)}))
		if following != nil {
			out = append(out, eitSectionBytes(tablePF, svc.ServiceID, svc.NetworkID, svc.NetworkID,
				1, last, last, tableLast, [][]byte{eitEventBytes(*following, 1)}))
		}
	}
	return out
}

// tsWriter はセクションを 188 バイトのパケットにする。continuity counter は PID ごとに数える
type tsWriter struct{ cc map[int]byte }

func newTSWriter() *tsWriter { return &tsWriter{cc: map[int]byte{}} }

func (w *tsWriter) section(pid int, section []byte) []byte {
	data := append([]byte{0x00}, section...) // pointer_field = 0
	var out []byte
	for first := true; len(data) > 0; first = false {
		n := min(len(data), 184)
		cc := w.cc[pid]
		w.cc[pid] = (cc + 1) & 0x0f
		p := make([]byte, 188)
		p[0] = 0x47
		p[1] = byte(pid>>8) & 0x1f
		if first {
			p[1] |= 0x40
		}
		p[2] = byte(pid)
		p[3] = 0x10 | cc
		copy(p[4:], data[:n])
		for i := 4 + n; i < len(p); i++ {
			p[i] = 0xff
		}
		out = append(out, p...)
		data = data[n:]
	}
	return out
}
