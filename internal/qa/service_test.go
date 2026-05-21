package qa

import (
	"context"
	"strings"
	"testing"

	"scribe-web/internal/asr"
	"scribe-web/internal/llm"
)

type stubProvider struct {
	reply string
	err   error
	got   llm.ChatRequest
}

func (s *stubProvider) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	s.got = req
	if s.err != nil {
		return nil, s.err
	}
	return &llm.ChatResponse{
		Model: "stub-model",
		Choices: []llm.ChatChoice{
			{Message: llm.ChatMessage{Role: "assistant", Content: s.reply}},
		},
	}, nil
}

func TestRetrieveTopSegments(t *testing.T) {
	segments := []asr.Segment{
		{Start: 0, End: 10, Text: "今天先介绍项目背景。"},
		{Start: 10, End: 20, Text: "这里重点讲 RAG 检索、分段评分和问答流程。"},
		{Start: 20, End: 30, Text: "最后总结部署方式。"},
	}
	hits := RetrieveTopSegments("RAG 如何做分段检索？", segments, 2)
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if !strings.Contains(hits[0].Segment.Text, "RAG") {
		t.Fatalf("top1 = %q, want RAG segment", hits[0].Segment.Text)
	}
	if hits[0].Score < hits[1].Score {
		t.Fatalf("scores not sorted desc: %+v", hits)
	}
}

func TestParseChapters(t *testing.T) {
	raw := "```json\n{\n  \"chapters\": [\n    {\n      \"title\": \"开场\",\n      \"start_sec\": 0,\n      \"end_sec\": 40,\n      \"bullets\": [\"背景\", \"目标\"],\n      \"key_quotes\": [{\"text\": \"先看背景\", \"start_sec\": 1, \"end_sec\": 3}]\n    },\n    {\n      \"title\": \"实现\",\n      \"start_sec\": 40,\n      \"end_sec\": 90,\n      \"bullets\": [\"后端\", \"前端\", \"验证\", \"回归\", \"文档\", \"额外\"]\n    }\n  ]\n}\n```"
	chs, err := ParseChapters(raw, 100, []asr.Segment{
		{Start: 45, End: 49, Text: "这里是实现阶段的关键句。"},
	})
	if err != nil {
		t.Fatalf("ParseChapters: %v", err)
	}
	if len(chs) != 2 {
		t.Fatalf("len = %d, want 2", len(chs))
	}
	if got := len(chs[0].Bullets); got != 3 {
		t.Fatalf("chapter0 bullets = %d, want 3 (auto padded)", got)
	}
	if got := len(chs[1].Bullets); got != 5 {
		t.Fatalf("chapter1 bullets = %d, want 5 (trimmed)", got)
	}
	if chs[1].EndSec > 100 {
		t.Fatalf("chapter1 end = %v, want <= 100", chs[1].EndSec)
	}
	if len(chs[0].KeyQuotes) == 0 || !strings.Contains(chs[0].KeyQuotes[0].Text, "背景") {
		t.Fatalf("chapter0 key quotes not parsed: %+v", chs[0].KeyQuotes)
	}
	if len(chs[1].KeyQuotes) == 0 {
		t.Fatalf("chapter1 key quotes should fallback from segments")
	}
}

func TestAnswerUsesRetrievedCitations(t *testing.T) {
	stub := &stubProvider{reply: "答案是检索分段后再让模型综合。"}
	svc := New(stub, nil)
	res, err := svc.AnswerWithContext(context.Background(), &asr.Result{
		Segments: []asr.Segment{
			{Start: 0, End: 8, Text: "这是简介。"},
			{Start: 8, End: 20, Text: "问答用到关键词检索和片段评分。"},
		},
	}, "问答怎么实现？", []ChatMessage{
		{Role: "user", Content: "上一轮说了什么？"},
		{Role: "assistant", Content: "上一轮讨论了背景。"},
	}, 2)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if strings.TrimSpace(res.Answer) == "" {
		t.Fatal("answer is empty")
	}
	if len(res.Citations) == 0 {
		t.Fatal("citations empty")
	}
	if !strings.Contains(stub.got.Messages[1].Content, "历史对话") {
		t.Fatalf("history not injected into prompt: %s", stub.got.Messages[1].Content)
	}
}

func TestAnswerAcrossJobs(t *testing.T) {
	stub := &stubProvider{reply: "跨视频结论：两个视频都强调检索质量。"}
	svc := New(stub, nil)
	res, err := svc.AnswerAcrossJobs(context.Background(), "检索质量", []GlobalDocument{
		{
			JobID:    "j1",
			JobTitle: "视频A",
			Segments: []asr.Segment{{Start: 1, End: 8, Text: "视频A强调检索召回率。"}},
		},
		{
			JobID:    "j2",
			JobTitle: "视频B",
			Segments: []asr.Segment{{Start: 3, End: 12, Text: "视频B强调检索精度。"}},
		},
	}, 4)
	if err != nil {
		t.Fatalf("AnswerAcrossJobs: %v", err)
	}
	if len(res.Citations) == 0 {
		t.Fatal("citations empty")
	}
	if res.Citations[0].JobID == "" {
		t.Fatalf("citation missing job id: %+v", res.Citations[0])
	}
	if !strings.Contains(stub.got.Messages[1].Content, "跨视频") {
		t.Fatalf("global QA prompt missing cross-video marker")
	}
}
