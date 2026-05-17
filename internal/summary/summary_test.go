package summary

import (
	"context"
	"errors"
	"strings"
	"testing"

	"scribe-web/internal/asr"
	"scribe-web/internal/config"
	"scribe-web/internal/llm"
	"scribe-web/internal/vision"
)

// stubProvider captures the last ChatRequest sent and returns whatever
// the test stuffed into Reply / Err.
type stubProvider struct {
	got   llm.ChatRequest
	reply string
	err   error
}

func (s *stubProvider) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	s.got = req
	if s.err != nil {
		return nil, s.err
	}
	return &llm.ChatResponse{
		Model: "stub-model",
		Choices: []llm.ChatChoice{{
			Message: llm.ChatMessage{Role: "assistant", Content: s.reply},
		}},
		Usage: llm.ChatUsage{PromptTokens: 30, CompletionTokens: 12, TotalTokens: 42},
	}, nil
}

func sampleTranscript() *asr.Result {
	return &asr.Result{
		Language: "zh-Hans",
		Duration: 180,
		FullText: "今天我们来聊一聊大模型。\n\n首先，大模型并不是万能的……",
	}
}

func TestParseKind(t *testing.T) {
	for _, k := range AllKinds {
		got, err := ParseKind(string(k))
		if err != nil || got != k {
			t.Errorf("ParseKind(%q) = (%v, %v)", k, got, err)
		}
	}
	if _, err := ParseKind("nope"); err == nil {
		t.Errorf("expected error for unknown kind")
	}
	// Whitespace + case tolerance.
	if got, _ := ParseKind("  BRIEF  "); got != KindBrief {
		t.Errorf("ParseKind whitespace = %q", got)
	}
}

func TestGenerate_PromptWiring(t *testing.T) {
	stub := &stubProvider{reply: "一句话总结：大模型并非万能。"}
	svc := New(stub, nil)
	res, err := svc.Generate(context.Background(), sampleTranscript(), KindBrief, Metadata{
		Title:    "测试视频",
		Uploader: "tester",
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.Kind != KindBrief {
		t.Errorf("kind = %q", res.Kind)
	}
	if res.Markdown != "一句话总结：大模型并非万能。" {
		t.Errorf("markdown = %q", res.Markdown)
	}
	if res.TokensUsed != 42 {
		t.Errorf("tokens = %d", res.TokensUsed)
	}
	if res.Model != "stub-model" {
		t.Errorf("model = %q", res.Model)
	}

	// Sanity check the prompt payload.
	if got := stub.got.Messages; len(got) != 2 || got[0].Role != "system" || got[1].Role != "user" {
		t.Fatalf("messages shape: %#v", got)
	}
	user := stub.got.Messages[1].Content
	for _, want := range []string{"测试视频", "tester", "今天我们来聊一聊大模型"} {
		if !strings.Contains(user, want) {
			t.Errorf("user prompt missing %q\n---prompt---\n%s", want, user)
		}
	}
}

func TestGenerate_KindsAllRender(t *testing.T) {
	for _, k := range AllKinds {
		k := k
		t.Run(string(k), func(t *testing.T) {
			stub := &stubProvider{reply: "ok body"}
			svc := New(stub, nil)
			_, err := svc.Generate(context.Background(), sampleTranscript(), k, Metadata{Title: "T"})
			if err != nil {
				t.Fatalf("kind %s: %v", k, err)
			}
			if !strings.Contains(stub.got.Messages[1].Content, "T") {
				t.Errorf("kind %s: title not interpolated", k)
			}
		})
	}
}

func TestGenerate_EmptyTranscript(t *testing.T) {
	svc := New(&stubProvider{reply: "x"}, nil)
	_, err := svc.Generate(context.Background(),
		&asr.Result{FullText: "   "}, KindBrief, Metadata{})
	if err == nil || !strings.Contains(err.Error(), "transcript is empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestGenerate_NoProvider(t *testing.T) {
	svc := New(nil, nil)
	_, err := svc.Generate(context.Background(), sampleTranscript(), KindBrief, Metadata{})
	if !errors.Is(err, llm.ErrNoAPIKey) {
		t.Fatalf("err = %v, want ErrNoAPIKey", err)
	}
}

func TestGenerate_ProviderError(t *testing.T) {
	stub := &stubProvider{err: llm.ErrRateLimited}
	svc := New(stub, nil)
	_, err := svc.Generate(context.Background(), sampleTranscript(), KindBrief, Metadata{})
	if !errors.Is(err, llm.ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

func TestPrepareText_Truncates(t *testing.T) {
	body := strings.Repeat("一二三四五", 5000) // 25 000 chars
	out := prepareText(body, MaxTranscriptChars)
	if !strings.Contains(out, "中段省略") {
		t.Fatalf("expected elision marker, got %q", out[:60])
	}
	runes := []rune(out)
	// Must come in under the cap by a reasonable margin (cap + marker).
	if len(runes) > MaxTranscriptChars+20 {
		t.Errorf("len = %d, want <= %d", len(runes), MaxTranscriptChars+20)
	}
	// Head + tail of the original must be present.
	if !strings.HasPrefix(out, "一二三四五") {
		t.Errorf("head lost")
	}
	if !strings.HasSuffix(out, "一二三四五") {
		t.Errorf("tail lost")
	}
}

func TestPrepareText_ShortPassthrough(t *testing.T) {
	in := "你好\n\n世界"
	out := prepareText(in, MaxTranscriptChars)
	if out != "你好 世界" {
		t.Errorf("got %q", out)
	}
}

func TestGenerate_TokenSplitAndCost(t *testing.T) {
	stub := &stubProvider{reply: "ok"}
	cfg := &config.LLMConfig{
		PriceInputPerMTok:  0.30, // ¥ / 1M tok
		PriceOutputPerMTok: 0.60,
	}
	svc := New(stub, cfg)
	res, err := svc.Generate(context.Background(), sampleTranscript(), KindBrief, Metadata{Title: "T"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if res.PromptTokens != 30 || res.CompletionTokens != 12 {
		t.Fatalf("tokens split: prompt=%d completion=%d", res.PromptTokens, res.CompletionTokens)
	}
	// 30 * 0.30/1e6 + 12 * 0.60/1e6 = 0.0000090 + 0.0000072 = 0.0000162
	if res.EstimatedCost < 1.5e-5 || res.EstimatedCost > 1.7e-5 {
		t.Errorf("cost = %v", res.EstimatedCost)
	}
	if res.EstimatedCostText == "" {
		t.Errorf("cost text empty")
	}
}

func TestEstimateCost_NoPricing(t *testing.T) {
	c, txt := EstimateCost(1000, 500, nil)
	if c != 0 || txt != "" {
		t.Errorf("nil cfg: c=%v txt=%q", c, txt)
	}
	c, txt = EstimateCost(1000, 500, &config.LLMConfig{}) // zero prices
	if c != 0 || txt != "" {
		t.Errorf("zero cfg: c=%v txt=%q", c, txt)
	}
}

func TestEstimateCost_Formatting(t *testing.T) {
	cfg := &config.LLMConfig{PriceInputPerMTok: 10, PriceOutputPerMTok: 20}
	cases := []struct {
		p, c int
		want string
	}{
		{1, 1, "¥0.0000"},              // sub-cent
		{1000, 1000, "¥0.030"},         // < ¥1
		{100_000, 100_000, "¥3.00"},    // >= ¥1
	}
	for _, tc := range cases {
		_, txt := EstimateCost(tc.p, tc.c, cfg)
		if txt != tc.want {
			t.Errorf("EstimateCost(%d,%d) = %q, want %q", tc.p, tc.c, txt, tc.want)
		}
	}
}

func TestGenerate_VisualInsightsInjected(t *testing.T) {
	stub := &stubProvider{reply: "一句话总结"}
	svc := New(stub, nil)
	meta := Metadata{
		Title: "T",
		Frames: []vision.Insight{
			{TimestampSec: 5, Caption: "开场幻灯片", OCRText: "大模型 101"},
			{TimestampSec: 73.4, Caption: "讲师特写"},
			{TimestampSec: 90, Caption: "(画面描述失败)", Error: "boom"},
		},
	}
	if _, err := svc.Generate(context.Background(), sampleTranscript(), KindBrief, meta); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	user := stub.got.Messages[1].Content
	if !strings.Contains(user, "画面信息（按时间戳") {
		t.Fatalf("missing visual block:\n%s", user)
	}
	if !strings.Contains(user, "[00:05] 画面：开场幻灯片  文字：大模型 101") {
		t.Errorf("first frame line missing:\n%s", user)
	}
	if !strings.Contains(user, "[01:13] 画面：讲师特写") {
		t.Errorf("second frame line missing:\n%s", user)
	}
	if strings.Contains(user, "画面描述失败") {
		t.Errorf("failed frame should be dropped:\n%s", user)
	}
}

func TestGenerate_NoFramesNoVisualBlock(t *testing.T) {
	stub := &stubProvider{reply: "一句话"}
	svc := New(stub, nil)
	if _, err := svc.Generate(context.Background(), sampleTranscript(), KindOutline, Metadata{Title: "T"}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	user := stub.got.Messages[1].Content
	if strings.Contains(user, "画面信息") {
		t.Errorf("visual block should be omitted when no frames:\n%s", user)
	}
}

func TestRenderVisualInsights_TrimAndDrop(t *testing.T) {
	out := renderVisualInsights([]vision.Insight{
		{TimestampSec: 0, Caption: "  spaced  ", OCRText: "  "},
		{TimestampSec: 10, Caption: "", OCRText: ""},
		{TimestampSec: 20, Caption: "(画面描述失败)", Error: "x"},
		{TimestampSec: 30, Caption: "良品", OCRText: "OCR"},
	}, MaxVisualInsightsChars)
	if !strings.Contains(out, "[00:00] 画面：spaced") {
		t.Errorf("space-trimmed caption missing:\n%s", out)
	}
	if strings.Contains(out, "[00:10]") {
		t.Errorf("empty insight should be dropped:\n%s", out)
	}
	if strings.Contains(out, "[00:20]") {
		t.Errorf("failed insight should be dropped:\n%s", out)
	}
	if !strings.Contains(out, "[00:30] 画面：良品  文字：OCR") {
		t.Errorf("good insight missing:\n%s", out)
	}
}

func TestRenderVisualInsights_TruncatesAtCap(t *testing.T) {
	var many []vision.Insight
	for i := 0; i < 200; i++ {
		many = append(many, vision.Insight{
			TimestampSec: float64(i),
			Caption:      strings.Repeat("文", 50),
		})
	}
	out := renderVisualInsights(many, 1000)
	runes := []rune(out)
	if len(runes) > 1020 {
		t.Errorf("len = %d, want <= ~1020", len(runes))
	}
	if !strings.Contains(out, "中段省略") {
		t.Errorf("missing elision marker")
	}
}

func TestStripCodeFence(t *testing.T) {
	cases := map[string]string{
		"```markdown\n# 标题\n- a\n```":          "# 标题\n- a",
		"```\n# 标题\n```":                       "# 标题",
		"# 标题\n- a":                            "# 标题\n- a",
		"# 标题\n```js\ncode\n```":               "# 标题\n```js\ncode\n```",
		"```\n# 标题\n":                          "# 标题",
	}
	for in, want := range cases {
		if got := stripCodeFence(in); got != want {
			t.Errorf("stripCodeFence(%q) = %q, want %q", in, got, want)
		}
	}
}
