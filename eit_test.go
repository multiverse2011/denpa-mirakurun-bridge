package main

import (
	"bytes"
	"encoding/binary"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCRC32MPEG(t *testing.T) {
	// MPEG-2 の CRC32 の知られている値
	if got := crc32MPEG([]byte("123456789")); got != 0x0376e6e7 {
		t.Fatalf("%08x", got)
	}
	section := withCRC([]byte{0x4e, 0, 0, 0x04, 0x00, 0xc1, 0, 0, 0, 0, 0, 0, 0, 0x51})
	if crc32MPEG(section) != 0 {
		t.Fatal("CRC を付けたセクションを回すと 0 になりません")
	}
	if int(section[1]&0x0f)<<8|int(section[2]) != len(section)-3 {
		t.Fatalf("section_length: %d", int(section[1]&0x0f)<<8|int(section[2]))
	}
	if section[1]&0xd0 != 0x90 { // section_syntax_indicator と reserved
		t.Fatalf("section[1]: %02x", section[1])
	}
}

func TestAribText(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []byte
	}{
		{"A", []byte{0x1b, '(', 'J', 0x89, 'A'}},
		{"あ", []byte{0x24, 0x22}},
		{"Aあ", []byte{0x1b, '(', 'J', 0x89, 'A', 0x1b, '$', 'B', 0x8a, 0x24, 0x22}},
		{"ｱ", []byte{0x1b, '(', 'I', 0x31}}, // ESC ( I の後は 0x21〜0x5F (0xA1 ではない)
	} {
		got, _ := encodeArib(c.in)
		if !bytes.Equal(got, c.want) {
			t.Errorf("%q: % x != % x", c.in, got, c.want)
		}
	}
	// JIS と Unicode で食い違う字も、読む側 (denpa) と同じ字に寄せる
	a, _ := encodeArib("〜")
	b, _ := encodeArib("～")
	if !bytes.Equal(a, b) {
		t.Fatalf("〜: % x != % x", a, b)
	}
	// 文字の途中では切らない
	if got := truncateArib("あああ", 3); !bytes.Equal(got, []byte{0x24, 0x22}) {
		t.Fatalf("% x", got)
	}
}

// 尺が 100 時間以上でも BCD の時 (2 桁) を壊さず、99:59:59 で頭打ちにする
func TestBCDDurationCapsAt99Hours(t *testing.T) {
	cap := []byte{0x99, 0x59, 0x59}
	for _, c := range []struct {
		ms   int64
		want []byte
	}{
		{99*time.Hour.Milliseconds() + 59*time.Minute.Milliseconds() + 59*time.Second.Milliseconds(), cap},
		{100 * time.Hour.Milliseconds(), cap},
		{168 * time.Hour.Milliseconds(), cap},
	} {
		if got := bcdDuration(c.ms); !bytes.Equal(got, c.want) {
			t.Fatalf("%v: % x != % x", c.ms, got, c.want)
		}
	}
}

// ARIB の追加記号・JIS X 0213 の字 (85 区以降) を「？」に落とさない
func TestAribExtraCharacters(t *testing.T) {
	for _, ch := range []string{"🈑", "🈖", "🈀", "🈞", "🈔", "髙", "﨑", "德", "麴", "𠮷"} {
		r := []rune(ch)[0]
		cell, ok := aribExtra[r]
		if !ok {
			t.Fatalf("%s が aribExtra にありません", ch)
		}
		var want []byte
		if cell.designator != 0x42 {
			want = append(want, 0x1b, '$', cell.designator)
		}
		want = append(want, cell.ku, cell.ten)
		got, _ := encodeArib(ch)
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: % x != % x", ch, got, want)
		}
	}
}

// 2 バイト集合の指示 (0x42・0x39・0x3A・0x3B) を切り替えても、指示と区点を正しく出す。
// 0x42 以外は jis0208 に無い字を固定で選び、その designator が実際に使われることを確かめる
func TestAribDesignatorSwitching(t *testing.T) {
	reps := []struct {
		ch         string
		designator byte
		inJIS      bool
	}{
		{"あ", 0x42, true},
		{"㬢", 0x39, false}, // JIS X 0213 の 1 面 85 区
		{"㐆", 0x3a, false}, // JIS X 0213 の 2 面
		{"🈑", 0x3b, false}, // ARIB 追加記号
	}
	for _, rp := range reps {
		r := []rune(rp.ch)[0]
		if _, inJIS := jis0208[r]; inJIS != rp.inJIS {
			t.Fatalf("%s: jis0208 にあるかが違います (%v)", rp.ch, inJIS)
		}
		if !rp.inJIS {
			if cell, ok := aribExtra[r]; !ok || cell.designator != rp.designator {
				t.Fatalf("%s: 指示 %02x のセルがありません (%+v)", rp.ch, rp.designator, cell)
			}
		}
	}
	seq := []rune{'あ', '㬢', '㐆', '🈑', 'い', '🈑', '㐆', '㬢', 'あ'}
	var want []byte
	prev := byte(0x42)
	for _, r := range seq {
		var designator, ku, ten byte
		if code, ok := jis0208[r]; ok {
			designator, ku, ten = 0x42, byte(code>>8), byte(code)
		} else {
			cell := aribExtra[r]
			designator, ku, ten = cell.designator, cell.ku, cell.ten
		}
		if designator != prev {
			want = append(want, 0x1b, '$', designator)
			prev = designator
		}
		want = append(want, ku, ten)
	}
	got, _ := encodeArib(string(seq))
	if !bytes.Equal(got, want) {
		t.Fatalf("% x != % x", got, want)
	}
}

// 英数・半角カナ・漢字・追加記号の間の切り替え
func TestAribModeSwitching(t *testing.T) {
	cell := aribExtra['🈑']
	want := []byte{
		0x1b, '(', 'J', 0x89, 'A', // 英数
		0x8a, 0x1b, '(', 'I', 0x31, // 半角カナ (NSZ して ESC ( I)
		0x1b, '$', 0x42, 0x24, 0x22, // 漢字 (ESC $ B)
		0x1b, '$', cell.designator, cell.ku, cell.ten, // 追加記号
		0x1b, '(', 'J', 0x89, 'A', // 英数に戻る
	}
	got, _ := encodeArib("Aｱあ🈑A")
	if !bytes.Equal(got, want) {
		t.Fatalf("% x != % x", got, want)
	}
}

// splitAribString は CRLF・サロゲートペア・制御文字の途中で切らない
func TestSplitAribStringBoundaries(t *testing.T) {
	cases := []string{
		"a\r\nb",
		"あ\r\nい",
		"漢\r\n字",
		"𠮷\r\nあ",
		"🈑\r\n𠮷",
		"A\tB\r\nC",
		strings.Repeat("あ", 200),
		"A\x00B\r\nC\x1bD",
	}
	for _, s := range cases {
		enc, _ := encodeArib(s)
		for limit := 0; limit <= len(enc)+2; limit++ {
			head, tail := splitAribString(s, limit)
			if head+tail != s {
				t.Fatalf("%q (%d): 前半と後半が元に戻りません", s, limit)
			}
			headEnc, _ := encodeArib(head)
			if len(headEnc) > limit {
				t.Fatalf("%q (%d): 前半が %d バイトあります", s, limit, len(headEnc))
			}
			if !utf8.ValidString(head) || !utf8.ValidString(tail) {
				t.Fatalf("%q (%d): UTF-8 が壊れています", s, limit)
			}
			if strings.HasSuffix(head, "\r") && strings.HasPrefix(tail, "\n") {
				t.Fatalf("%q (%d): CRLF を分けています", s, limit)
			}
		}
	}
}

// 長い説明は short_event に収まる分だけを残し、あふれた後半を extended の「番組内容」に移す
func TestLongDescriptionMovesTailToExtended(t *testing.T) {
	desc := strings.Repeat("あ", 200) // 400 バイト
	p := epgProgram{
		EventID: 1, StartAt: 1, Duration: 1, Name: "テスト", Description: desc,
		Extended: map[string]string{"番組内容": "前書き"},
	}
	ev := eitEventBytes(p, 0)

	nameBytes := truncateArib(p.Name, 250)
	head, tail := splitAribString(desc, 250-len(nameBytes))
	if tail == "" {
		t.Fatal("説明が切られていません")
	}
	if head+tail != desc {
		t.Fatalf("前半と後半が元の説明と一致しません")
	}
	headBytes, _ := encodeArib(head)

	var short []byte
	var ext [][]byte
	for _, d := range descriptorsOf(ev[12:]) {
		switch d[0] {
		case 0x4d:
			short = d
		case 0x4e:
			ext = append(ext, d)
		}
	}
	if short == nil {
		t.Fatal("short_event_descriptor がありません")
	}
	nameLen := int(short[5])
	descLen := int(short[6+nameLen])
	if got := short[7+nameLen : 7+nameLen+descLen]; !bytes.Equal(got, headBytes) {
		t.Fatalf("説明の前半が違います: %d バイト", len(got))
	}
	items := extendedItems(t, ext)
	heading, _ := encodeArib("番組内容")
	want, _ := encodeArib("前書き\n" + tail)
	if !bytes.Equal(items[string(heading)], want) {
		t.Fatalf("extended の番組内容に後半がありません")
	}
}

// descriptorsOf は記述子の並びを 1 本ずつに分ける
func descriptorsOf(data []byte) [][]byte {
	var out [][]byte
	for at := 0; at+2 <= len(data); {
		n := 2 + int(data[at+1])
		if at+n > len(data) {
			break
		}
		out = append(out, data[at:at+n])
		at += n
	}
	return out
}

// 制御文字は ARIB のエスケープと紛れるので '?' に、改行は APR、タブは空白にする
func TestAribControlCharacters(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"A\x1bB", "A?B"},
		{"あ\x1bい", "あ?い"},
		{"\x00\x7f", "??"},
		{"A\tB", "A B"},
		{"A\nB", "A\nB"},
		{"A\r\nB", "A\nB"},
		{"あ\r\nい", "あ\nい"},
	} {
		got, _ := encodeArib(c.in)
		want, _ := encodeArib(c.want)
		if !bytes.Equal(got, want) {
			t.Fatalf("%q: % x != % x", c.in, got, want)
		}
	}
	// CRLF は APR 1 つにまとめる
	if got, _ := encodeArib("A\r\nB"); bytes.Count(got, []byte{0x0d}) != 1 {
		t.Fatalf("CRLF: % x", got)
	}
}

func TestExtendedEventSplitting(t *testing.T) {
	// 日本語 200 文字は 400 バイトなので、1 記述子 (255 バイト) に収まらない
	desc := extendedEventDescriptors(map[string]string{"番組内容": strings.Repeat("あ", 200)})
	if len(desc) < 2 {
		t.Fatalf("1 本に詰まっています: %d", len(desc))
	}
	for i, d := range desc {
		if d[0] != 0x4e || int(d[1]) != len(d)-2 {
			t.Fatalf("%d: tag %02x len %d", i, d[0], d[1])
		}
		if int(d[2]>>4) != i || int(d[2]&0x0f) != len(desc)-1 {
			t.Fatalf("%d: descriptor_number %02x", i, d[2])
		}
		if int(d[6]) != len(d)-8 {
			t.Fatalf("%d: items の長さ %d", i, d[6])
		}
	}
	// 2 本目からは見出しが空 (直前の項目の続き)
	if d := desc[1]; d[7] != 0 {
		t.Fatalf("継続の見出し: %d", d[7])
	}
}

// extendedItems は extended_event_descriptor を読み、見出しごとに継続を繋ぎ直す
// (denpa の ExtendedItems と同じ)
func extendedItems(t *testing.T, descriptors [][]byte) map[string][]byte {
	t.Helper()
	items := map[string][]byte{}
	last := ""
	for i, d := range descriptors {
		if d[0] != 0x4e {
			t.Fatalf("%d: tag %02x", i, d[0])
		}
		if d[1] == 0 || int(d[1]) != len(d)-2 {
			t.Fatalf("%d: descriptor_length %d (len %d)", i, d[1], len(d))
		}
		at, end := 7, 7+int(d[6])
		for at < end {
			n := int(d[at])
			heading := d[at+1 : at+1+n]
			at += 1 + n
			m := int(d[at])
			text := d[at+1 : at+1+m]
			at += 1 + m
			if n == 0 {
				items[last] = append(items[last], text...)
				continue
			}
			last = string(heading)
			items[last] = append(items[last], text...)
		}
	}
	return items
}

// 項目が 250 バイトちょうどでも descriptor_length が 0 に化けない
func TestExtendedEventDescriptorBoundary(t *testing.T) {
	// 見出し 1 文字 (2 バイト) + 本文 23 文字 (46 バイト) = 1 項目 50 バイト、5 個で 250 バイト
	headings := []string{"あ", "い", "う", "え", "お"}
	text := strings.Repeat("か", 23)
	ext := map[string]string{}
	for _, h := range headings {
		ext[h] = text
	}
	desc := extendedEventDescriptors(ext)
	got := extendedItems(t, desc)
	want, _ := encodeArib(text)
	for _, h := range headings {
		heading, _ := encodeArib(h)
		if !bytes.Equal(got[string(heading)], want) {
			t.Fatalf("%s: % x != % x", h, got[string(heading)], want)
		}
	}
}

func TestEventDescriptors(t *testing.T) {
	start := time.Date(2026, 11, 17, 12, 34, 56, 0, time.FixedZone("JST", 9*3600)).UnixMilli()
	p := epgProgram{
		EventID: 0x1234, ServiceID: 1024, NetworkID: 32736,
		StartAt: start, Duration: 30 * time.Minute.Milliseconds(), IsFree: false,
		Name: "番組A", Description: "説明",
		Extended: map[string]string{"出演者": "太郎"},
		Genres:   []epgGenre{{Lv1: 7, Lv2: 1, Un1: 0, Un2: 0}},
		Video:    &epgVideo{StreamContent: 1, ComponentType: 0xb3},
		Audios:   []epgAudio{{ComponentType: 3, ComponentTag: 0x11, IsMain: true, SamplingRate: 48000, Langs: []string{"jpn"}}},
	}
	ev := eitEventBytes(p, 4)
	if binary.BigEndian.Uint16(ev) != 0x1234 {
		t.Fatalf("event_id: %x", ev[:2])
	}
	// 2026-11-17 12:34:56 JST
	if !bytes.Equal(ev[2:7], []byte{0xef, 0xb1, 0x12, 0x34, 0x56}) {
		t.Fatalf("start_time: % x", ev[2:7])
	}
	if !bytes.Equal(ev[7:10], []byte{0x00, 0x30, 0x00}) {
		t.Fatalf("duration: % x", ev[7:10])
	}
	if ev[10]>>5 != 4 || ev[10]&0x10 == 0 {
		t.Fatalf("running_status / free_CA_mode: %02x", ev[10])
	}
	tags := descriptorTags(ev[12:])
	for _, tag := range []byte{0x4d, 0x4e, 0x54, 0x50, 0xc4} {
		if !slices.Contains(tags, tag) {
			t.Fatalf("記述子 %02x がありません: % x", tag, tags)
		}
	}
}

// descriptorTags は記述子の並びからタグだけを取る
func descriptorTags(data []byte) []byte {
	var tags []byte
	for at := 0; at+2 <= len(data); {
		n := int(data[at+1])
		tags = append(tags, data[at])
		at += 2 + n
	}
	return tags
}

func TestEITScheduleTableIDs(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.FixedZone("JST", 9*3600)).UnixMilli()
	svc := epgService{NetworkID: 4, ServiceID: 101, Type: 1}
	programs := []epgProgram{
		{EventID: 0, StartAt: now - 10*time.Minute.Milliseconds(), Duration: 30 * time.Minute.Milliseconds(), Name: "いま"},
		{EventID: 1, StartAt: now + time.Hour.Milliseconds(), Duration: time.Hour.Milliseconds(), Name: "次"},
		{EventID: 2, StartAt: now + 4*24*time.Hour.Milliseconds(), Duration: time.Hour.Milliseconds(), Name: "4 日後"},
	}
	secs := serviceEITSections(svc, programs, now)

	present := map[int]map[byte][]byte{}
	for _, sec := range secs {
		if crc32MPEG(sec) != 0 {
			t.Fatalf("CRC: % x", sec)
		}
		if sec[13] != tableLast {
			t.Fatalf("last_table_id: %02x", sec[13])
		}
		m := present[int(sec[0])]
		if m == nil {
			m = map[byte][]byte{}
			present[int(sec[0])] = m
		}
		m[sec[6]] = sec
	}
	// p/f: 0 がいま、1 が次
	if sec := present[tablePF][0]; sec == nil || !slices.Equal(eventIDs(sec), []int{0}) {
		t.Fatalf("p/f 0: %v", eventIDs(sec))
	}
	if sec := present[tablePF][1]; sec == nil || !slices.Equal(eventIDs(sec), []int{1}) {
		t.Fatalf("p/f 1: %v", eventIDs(sec))
	}
	// 12:00 はその日の 4 番目のセグメント。0x50 は 32、0x51 は 0 から
	if !slices.Contains(eventIDs(present[0x50][32]), 1) {
		t.Fatalf("0x50 の 32: %v", eventIDs(present[0x50][32]))
	}
	if !slices.Contains(eventIDs(present[0x51][32]), 2) {
		t.Fatalf("0x51 の 32: %v", eventIDs(present[0x51][32]))
	}
	// 番組の無いセグメントも空のセクションがある (denpa が待ち続けないように)
	for _, section := range []byte{32, 40, 96, 248} {
		if present[0x50][section] == nil {
			t.Fatalf("0x50 の %d がありません", section)
		}
	}
	for _, section := range []byte{0, 8, 248} {
		if present[0x51][section] == nil {
			t.Fatalf("0x51 の %d がありません", section)
		}
	}
	if got := present[0x50][32][7]; got != 248 {
		t.Fatalf("last_section_number: %d", got)
	}
	if got := present[0x50][32][12]; got != 32 {
		t.Fatalf("segment_last_section_number: %d", got)
	}
	// 12:00 より前は過ぎたセグメントなので流さない
	if present[0x50][0] != nil {
		t.Fatal("過ぎたセグメントを流しています")
	}
}

// 最終セグメントに複数のセクションがあるとき、last_section_number は実際の最後に合わせる
func TestEITLastSectionNumber(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.FixedZone("JST", 9*3600)).UnixMilli()
	start := jstMidnight(now) + 31*segmentSpanMS // 最終セグメント (3 日後の 21:00)
	var programs []epgProgram
	for i := 0; i < 40; i++ {
		programs = append(programs, epgProgram{
			EventID: i, StartAt: start + int64(i)*4*time.Minute.Milliseconds(),
			Duration: time.Minute.Milliseconds(), Name: strings.Repeat("あ", 100),
		})
	}
	var numbers []byte
	var last []byte
	for _, sec := range serviceEITSections(epgService{NetworkID: 4, ServiceID: 101}, programs, now) {
		if sec[0] == tableSchedule {
			numbers = append(numbers, sec[6])
			last = append(last, sec[7])
		}
	}
	maxNumber := slices.Max(numbers)
	if maxNumber <= 248 {
		t.Fatalf("最終セグメントのセクションが %d 本しかありません", len(numbers))
	}
	for i, l := range last {
		if l != maxNumber {
			t.Fatalf("last_section_number %d (section %d、最後は %d)", l, numbers[i], maxNumber)
		}
	}
}

// hasSynthesizablePrograms の判定が、実際に serviceEITSections がイベントを出すかと一致する
func TestHasSynthesizablePrograms(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.FixedZone("JST", 9*3600)).UnixMilli()
	midnight := jstMidnight(now)
	hour := time.Hour.Milliseconds()
	day := 24 * hour
	one := func(eventID int, start, duration int64) []epgProgram {
		return []epgProgram{{EventID: eventID, StartAt: start, Duration: duration}}
	}
	cases := []struct {
		name     string
		programs []epgProgram
		want     bool
	}{
		{"空", nil, false},
		{"過去のみ", one(1, now-2*hour, hour), false},
		{"不正値のみ", []epgProgram{{EventID: 1}, {EventID: 2, StartAt: now, Duration: 0}}, false},
		{"8 日超のみ", one(1, midnight+9*day, hour), false},
		{"前のセグメントに始まって放送中", one(1, now-2*hour, 4*hour), true},
		{"いま開始", one(1, now, hour), true},
		{"夜をまたいで放送中", one(1, now-hour, 20*hour), true},
		{"次の番組のみ", one(1, now+hour, hour), true},
		{"8 日ちょうど", one(1, midnight+8*day, hour), false},
		{"7 日後 21:00", one(1, midnight+7*day+21*hour, hour), true},
		{"過去と未来", append(one(1, now-2*hour, hour), one(2, now+hour, hour)...), true},
	}
	for _, c := range cases {
		got := hasSynthesizablePrograms(c.programs, now)
		events := 0
		for _, sec := range serviceEITSections(epgService{NetworkID: 4, ServiceID: 101}, c.programs, now) {
			events += len(eventIDs(sec))
		}
		if got != c.want || got != (events > 0) {
			t.Fatalf("%s: has=%v events=%d want=%v", c.name, got, events, c.want)
		}
	}
}

// eventIDs は EIT セクションの番組番号
func eventIDs(section []byte) []int {
	if section == nil {
		return nil
	}
	var ids []int
	for at := 14; at+12 <= len(section)-4; {
		ids = append(ids, int(section[at])<<8|int(section[at+1]))
		desc := int(section[at+10]&0x0f)<<8 | int(section[at+11])
		at += 12 + desc
	}
	return ids
}

func TestEITSectionSplit(t *testing.T) {
	// 1 セグメントに入りきらない件数でも、セクションを分けて全部載せる
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.FixedZone("JST", 9*3600)).UnixMilli()
	var programs []epgProgram
	for i := 0; i < 20; i++ {
		programs = append(programs, epgProgram{
			EventID: i, StartAt: now + int64(i)*time.Minute.Milliseconds(),
			Duration: time.Minute.Milliseconds(), Name: strings.Repeat("あ", 100),
		})
	}
	secs := serviceEITSections(epgService{NetworkID: 4, ServiceID: 101}, programs, now)
	var got []int
	for _, sec := range secs {
		if sec[0] != tableSchedule {
			continue
		}
		if crc32MPEG(sec) != 0 || len(sec) > 4096 {
			t.Fatalf("section: %d", len(sec))
		}
		got = append(got, eventIDs(sec)...)
	}
	if len(got) != len(programs) {
		t.Fatalf("番組が %d / %d しか載っていません", len(got), len(programs))
	}
}

func TestTSWriter(t *testing.T) {
	w := newTSWriter()
	section := make([]byte, 400)
	for i := range section {
		section[i] = byte(i)
	}
	packets := w.section(pidEIT, section)
	if len(packets) != 3*188 {
		t.Fatalf("packets: %d", len(packets))
	}
	for i := 0; i < 3; i++ {
		p := packets[i*188 : (i+1)*188]
		if p[0] != 0x47 || int(p[1]&0x1f)<<8|int(p[2]) != pidEIT {
			t.Fatalf("%d: header % x", i, p[:4])
		}
		if (i == 0) != (p[1]&0x40 != 0) {
			t.Fatalf("%d: PUSI %02x", i, p[1])
		}
		if p[3]&0x0f != byte(i) {
			t.Fatalf("%d: continuity %02x", i, p[3])
		}
	}
	// 先頭には pointer_field、端数は 0xFF
	if packets[4] != 0x00 || packets[5] != section[0] {
		t.Fatalf("pointer: % x", packets[4:6])
	}
	last := packets[3*188-1]
	if last != 0xff {
		t.Fatalf("padding: %02x", last)
	}
	// 続きのパケットは同じ PID の続きの番号から
	next := w.section(pidEIT, section[:10])
	if next[3]&0x0f != 3 {
		t.Fatalf("continuity: %02x", next[3])
	}
}
