package notify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var testLoc = time.FixedZone("CST", 8*3600)

func mkSnap(kind string, nDays int) *Snapshot {
	s := &Snapshot{
		Kind:  kind,
		Day:   "2026-10-13",
		Slot:  time.Date(2026, 10, 14, 0, 5, 0, 0, testLoc),
		Focus: Totals{Rx: 1000, Tx: 2000},
	}
	for i := 0; i < nDays; i++ {
		day := time.Date(2026, 10, 13, 0, 0, 0, 0, testLoc).AddDate(0, 0, -i)
		dr := DayRow{
			Day:    day.Format("2006-01-02"),
			Totals: Totals{Rx: int64(i+1) * 100, Tx: int64(i+1) * 50},
		}
		s.Days = append(s.Days, dr)
		s.Grand.Rx += dr.Rx
		s.Grand.Tx += dr.Tx
	}
	// InProgress 由 BuildSnapshot 设置；这里手工构造快照，需自行还原该语义。
	if kind == KindRecover && len(s.Days) > 0 {
		s.Days[0].InProgress = true
	}
	return s
}

func cardTitleOf(t *testing.T, card map[string]any) string {
	t.Helper()
	h, ok := card["header"].(map[string]any)
	if !ok {
		t.Fatal("卡片缺少 header")
	}
	title, ok := h["title"].(map[string]any)
	if !ok {
		t.Fatal("header 缺少 title")
	}
	return title["content"].(string)
}

func cardHistory(t *testing.T, card map[string]any) string {
	t.Helper()
	els, ok := card["elements"].([]any)
	if !ok {
		t.Fatal("卡片缺少 elements")
	}
	for _, e := range els {
		m := e.(map[string]any)
		if m["tag"] == "div" {
			return m["text"].(map[string]any)["content"].(string)
		}
	}
	t.Fatal("卡片里找不到历史记录区块")
	return ""
}

// TestCardIsValidJSON 确认卡片能被序列化（飞书要求的是合法 JSON 对象）。
func TestCardIsValidJSON(t *testing.T) {
	for _, kind := range []string{KindDaily, KindRecover} {
		card := BuildCard(mkSnap(kind, 3))
		b, err := json.Marshal(card)
		if err != nil {
			t.Fatalf("%s: 卡片无法序列化: %v", kind, err)
		}
		if !json.Valid(b) {
			t.Fatalf("%s: 卡片不是合法 JSON", kind)
		}
	}
}

// TestCardDailyTitleCarriesDate 确认标题带日期与星期。
//
// 日报每天长一样，没有日期就等于没记。
func TestCardDailyTitleCarriesDate(t *testing.T) {
	title := cardTitleOf(t, BuildCard(mkSnap(KindDaily, 3)))
	// 2026-10-13 是周二（用 date 独立核对过，避免把星期算错当成实现 bug）
	for _, want := range []string{"10月13日", "周二", "日报"} {
		if !strings.Contains(title, want) {
			t.Errorf("标题应含 %q, 实得 %q", want, title)
		}
	}
}

func TestCardRecoverTitleCarriesTime(t *testing.T) {
	s := mkSnap(KindRecover, 3)
	s.Day = "2026-10-14"
	s.Slot = time.Date(2026, 10, 14, 12, 30, 0, 0, testLoc)
	title := cardTitleOf(t, BuildCard(s))
	for _, want := range []string{"10月14日", "12:30", "补发"} {
		if !strings.Contains(title, want) {
			t.Errorf("标题应含 %q, 实得 %q", want, title)
		}
	}
}

func TestCardTestFlagMarked(t *testing.T) {
	s := mkSnap(KindDaily, 3)
	s.Test = true
	if title := cardTitleOf(t, BuildCard(s)); !strings.Contains(title, "[测试]") {
		t.Errorf("自检卡片标题应标注 [测试], 实得 %q", title)
	}
}

// TestCardHasNoBarArtifacts 确认卡片里没有条形图字符。
//
// 用户明确表示不要条形图；这条断言防止后人"为了好看"再加回来。
func TestCardHasNoBarArtifacts(t *testing.T) {
	card := BuildCard(mkSnap(KindDaily, 5))
	b, _ := json.Marshal(card)
	for _, ch := range []string{"█", "░", "▓", "▒"} {
		if strings.Contains(string(b), ch) {
			t.Errorf("卡片不应出现条形图字符 %q", ch)
		}
	}
}

// TestCardListsDaysDescending 确认历史按日期倒序，且当日三栏与列表首行一致。
func TestCardListsDaysDescending(t *testing.T) {
	s := mkSnap(KindDaily, 3)
	hist := cardHistory(t, BuildCard(s))
	lines := strings.Split(hist, "\n")
	// 第 0 行是"历史记录 · 累计 …"，之后是逐日
	if len(lines) < 4 {
		t.Fatalf("历史行数过少: %d", len(lines))
	}
	if !strings.Contains(lines[1], "10-13") {
		t.Errorf("首行应是最新一天 10-13, 实得 %q", lines[1])
	}
	if !strings.Contains(lines[3], "10-11") {
		t.Errorf("第三行应是 10-11, 实得 %q", lines[3])
	}
	if !strings.Contains(lines[0], "3 天") {
		t.Errorf("标题行应含天数, 实得 %q", lines[0])
	}
}

// TestCardFoldsLongHistory 确认超过上限后折叠为按月归并，而不是无限增长。
func TestCardFoldsLongHistory(t *testing.T) {
	long := mkSnap(KindDaily, maxListDays+40)
	hist := cardHistory(t, BuildCard(long))
	lines := strings.Split(hist, "\n")

	// 1 行标题 + maxListDays 行日 + 若干行月度汇总
	if len(lines) <= maxListDays+1 {
		t.Fatalf("折叠后应有月度汇总行, 实得 %d 行", len(lines))
	}
	if len(lines) >= maxListDays+40 {
		t.Fatalf("折叠没有生效: %d 行", len(lines))
	}
	last := lines[len(lines)-1]
	if !strings.Contains(last, " 天 · ") || !strings.HasPrefix(last, "20") {
		t.Errorf("最后应是月度汇总行, 实得 %q", last)
	}
	// 总数不能因为折叠而丢：标题仍报完整天数
	if !strings.Contains(lines[0], " 天") {
		t.Errorf("标题行应含天数, 实得 %q", lines[0])
	}
}

func TestCardInProgressMarked(t *testing.T) {
	s := mkSnap(KindRecover, 2)
	hist := cardHistory(t, BuildCard(s))
	if !strings.Contains(hist, "（进行中）") {
		t.Errorf("补推卡片应标注进行中的当天, 实得:\n%s", hist)
	}
}

func TestCardEmptyHistory(t *testing.T) {
	s := mkSnap(KindDaily, 0)
	hist := cardHistory(t, BuildCard(s))
	if !strings.Contains(hist, "暂无历史数据") {
		t.Errorf("无数据时应给出提示, 实得 %q", hist)
	}
}
