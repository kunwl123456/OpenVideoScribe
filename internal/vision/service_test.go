package vision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"scribe-web/internal/config"
	"scribe-web/internal/media"
	"scribe-web/internal/vlm"
)

// stubProvider lets tests script per-call replies + observe concurrency.
type stubProvider struct {
	mu       sync.Mutex
	calls    int32
	inFlight int32
	peak     int32
	// reply / err per call index; if i is out of range, default ok reply.
	replies []string
	errs    []error
	// optional hook fired on entry, lets tests inject delays.
	enter func()
}

func (s *stubProvider) Chat(_ context.Context, _ vlm.ChatRequest) (*vlm.ChatResponse, error) {
	idx := int(atomic.AddInt32(&s.calls, 1)) - 1
	cur := atomic.AddInt32(&s.inFlight, 1)
	defer atomic.AddInt32(&s.inFlight, -1)
	for {
		peak := atomic.LoadInt32(&s.peak)
		if cur <= peak || atomic.CompareAndSwapInt32(&s.peak, peak, cur) {
			break
		}
	}
	if s.enter != nil {
		s.enter()
	}

	s.mu.Lock()
	var reply string
	var err error
	if idx < len(s.errs) && s.errs[idx] != nil {
		err = s.errs[idx]
	}
	if idx < len(s.replies) && s.replies[idx] != "" {
		reply = s.replies[idx]
	} else if err == nil {
		reply = "画面：第 " + itoa(idx) + " 帧\n文字：无"
	}
	s.mu.Unlock()

	if err != nil {
		return nil, err
	}
	return &vlm.ChatResponse{
		Choices: []vlm.ChatChoice{{Message: vlm.AssistantOutput{Role: "assistant", Content: reply}}},
		Usage:   vlm.ChatUsage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
	}, nil
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = digits[i%10]
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}

func makeFrames(t *testing.T, n int) []media.Frame {
	t.Helper()
	dir := t.TempDir()
	out := make([]media.Frame, n)
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'X', 0xFF, 0xD9}
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, "frame_"+itoa(i)+".jpg")
		if err := os.WriteFile(p, jpeg, 0o644); err != nil {
			t.Fatal(err)
		}
		out[i] = media.Frame{Index: i, TimestampSec: float64(i) * 5, ImagePath: p}
	}
	return out
}

func enabledCfg(conc int) *config.VLMConfig {
	return &config.VLMConfig{
		BaseURL:            "http://stub",
		APIKey:             "key",
		Model:              "test-vlm",
		Concurrency:        conc,
		PriceInputPerMTok:  2,
		PriceOutputPerMTok: 4,
	}
}

func TestDescribe_NilOrEmpty(t *testing.T) {
	// Nil service / nil provider → ErrDisabled.
	_, err := (*Service)(nil).Describe(context.Background(), nil, nil)
	if !errors.Is(err, ErrDisabled) {
		t.Errorf("nil service: err = %v", err)
	}
	svc := New(nil, enabledCfg(1))
	_, err = svc.Describe(context.Background(), nil, nil)
	if !errors.Is(err, ErrDisabled) {
		t.Errorf("nil provider: err = %v", err)
	}
	// Provider OK but empty frames → no error, no insights.
	svc2 := New(&stubProvider{}, enabledCfg(1))
	got, err := svc2.Describe(context.Background(), nil, nil)
	if err != nil {
		t.Errorf("empty frames: err = %v", err)
	}
	if got != nil {
		t.Errorf("empty frames: insights = %v", got)
	}
}

func TestDescribe_ConcurrencyCapped(t *testing.T) {
	const conc = 4
	const total = 20
	// Stagger entries so the in-flight count actually has time to grow.
	gate := make(chan struct{}, conc)
	for i := 0; i < conc; i++ {
		gate <- struct{}{}
	}
	stub := &stubProvider{
		enter: func() {
			<-gate
			gate <- struct{}{}
		},
	}
	svc := New(stub, enabledCfg(conc))
	frames := makeFrames(t, total)
	insights, err := svc.Describe(context.Background(), frames, nil)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(insights) != total {
		t.Errorf("insights len = %d, want %d", len(insights), total)
	}
	if peak := atomic.LoadInt32(&stub.peak); peak > int32(conc) {
		t.Errorf("peak in-flight = %d, want <= %d", peak, conc)
	}
	if peak := atomic.LoadInt32(&stub.peak); peak < 2 {
		// We expected at least *some* parallelism; otherwise the semaphore
		// is effectively serial and the test isn't meaningful.
		t.Errorf("peak in-flight = %d, expected parallelism", peak)
	}
}

func TestDescribe_PerFrameFailureNotFatal(t *testing.T) {
	stub := &stubProvider{
		errs: []error{nil, nil, errors.New("boom"), nil},
	}
	svc := New(stub, enabledCfg(1))
	frames := makeFrames(t, 4)
	insights, err := svc.Describe(context.Background(), frames, nil)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(insights) != 4 {
		t.Fatalf("insights len = %d", len(insights))
	}
	// Failed frame must have Error set + caption fallback.
	var failed Insight
	for _, in := range insights {
		if in.Error != "" {
			failed = in
		}
	}
	if failed.Error == "" {
		t.Fatalf("no failed insight recorded")
	}
	if !strings.Contains(failed.Caption, "画面描述失败") {
		t.Errorf("failed caption = %q", failed.Caption)
	}
}

func TestDescribe_RecordsUsageAndEstimatedCost(t *testing.T) {
	stub := &stubProvider{}
	svc := New(stub, enabledCfg(1))
	frames := makeFrames(t, 1)
	insights, err := svc.Describe(context.Background(), frames, nil)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(insights) != 1 {
		t.Fatalf("insights len = %d", len(insights))
	}
	got := insights[0]
	if got.TokensUsed != 10 || got.PromptTokens != 7 || got.CompletionTokens != 3 {
		t.Fatalf("usage = total %d prompt %d completion %d, want 10/7/3", got.TokensUsed, got.PromptTokens, got.CompletionTokens)
	}
	if got.EstimatedCost <= 0 || got.EstimatedCostText == "" {
		t.Fatalf("estimated cost missing: cost=%v text=%q", got.EstimatedCost, got.EstimatedCostText)
	}
}

func TestEstimateCost_NAWhenUsageOrPricingMissing(t *testing.T) {
	if cost, text := EstimateCost(0, 0, enabledCfg(1)); cost != 0 || text != "" {
		t.Fatalf("empty usage cost = %v %q, want zero/empty", cost, text)
	}
	cfg := enabledCfg(1)
	cfg.PriceInputPerMTok = 0
	cfg.PriceOutputPerMTok = 0
	if cost, text := EstimateCost(7, 3, cfg); cost != 0 || text != "" {
		t.Fatalf("unpriced cost = %v %q, want zero/empty", cost, text)
	}
}

func TestDescribe_TimestampSortedAndIndexed(t *testing.T) {
	stub := &stubProvider{}
	svc := New(stub, enabledCfg(8))
	// Feed frames in reverse order on purpose.
	dir := t.TempDir()
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'X', 0xFF, 0xD9}
	makeFrame := func(i int, ts float64) media.Frame {
		p := filepath.Join(dir, "frame_"+itoa(i)+".jpg")
		if err := os.WriteFile(p, jpeg, 0o644); err != nil {
			t.Fatal(err)
		}
		return media.Frame{Index: i, TimestampSec: ts, ImagePath: p}
	}
	frames := []media.Frame{
		makeFrame(0, 30.0),
		makeFrame(1, 5.0),
		makeFrame(2, 20.0),
		makeFrame(3, 1.0),
	}
	insights, err := svc.Describe(context.Background(), frames, nil)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	prev := -1.0
	for i, in := range insights {
		if in.TimestampSec < prev {
			t.Errorf("not sorted at %d: %v < %v", i, in.TimestampSec, prev)
		}
		if in.Index != i {
			t.Errorf("Index at %d = %d, want %d", i, in.Index, i)
		}
		prev = in.TimestampSec
	}
}

func TestDescribe_ProgressCallback(t *testing.T) {
	stub := &stubProvider{}
	svc := New(stub, enabledCfg(2))
	frames := makeFrames(t, 6)
	var calls int32
	var lastDone, lastTotal int
	_, err := svc.Describe(context.Background(), frames, func(done, total int) {
		atomic.AddInt32(&calls, 1)
		lastDone, lastTotal = done, total
	})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != int32(len(frames)) {
		t.Errorf("progress calls = %d, want %d", got, len(frames))
	}
	if lastDone != len(frames) || lastTotal != len(frames) {
		t.Errorf("final progress = (%d/%d), want (%d/%d)", lastDone, lastTotal, len(frames), len(frames))
	}
}

func TestParseReply(t *testing.T) {
	cases := map[string]struct {
		caption, ocr string
	}{
		"画面：开场幻灯片\n文字：标题 大模型 101": {caption: "开场幻灯片", ocr: "标题 大模型 101"},
		"画面：演讲者特写\n文字：无":          {caption: "演讲者特写", ocr: ""},
		"画面:ASCII colon\n文字:abc":  {caption: "ASCII colon", ocr: "abc"},
		"  \n画面：仅画面\n":            {caption: "仅画面", ocr: ""},
		"model went off-script":   {caption: "model went off-script", ocr: ""},
	}
	for in, want := range cases {
		c, o := parseReply(in)
		if c != want.caption || o != want.ocr {
			t.Errorf("parseReply(%q) = (%q, %q), want (%q, %q)", in, c, o, want.caption, want.ocr)
		}
	}
}

func TestFormatTimestamp(t *testing.T) {
	cases := map[float64]string{
		0:    "00:00",
		59:   "00:59",
		60:   "01:00",
		73.4: "01:13",
		3599: "59:59",
	}
	for in, want := range cases {
		if got := FormatTimestamp(in); got != want {
			t.Errorf("FormatTimestamp(%v) = %q, want %q", in, got, want)
		}
	}
}
