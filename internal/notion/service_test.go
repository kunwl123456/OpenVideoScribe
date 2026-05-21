package notion

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"scribe-web/internal/config"
	"scribe-web/internal/store"
	"scribe-web/internal/ytdlp"
)

func TestExportJob_ChunksBlocksAndCreatesPage(t *testing.T) {
	var createChildren int
	var appendCalls int
	var createBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret_token" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.Header.Get("Notion-Version"); got != "2022-06-28" {
			t.Fatalf("notion version = %q", got)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/pages":
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			if parent, _ := createBody["parent"].(map[string]any); parent["database_id"] != "db_123" {
				t.Fatalf("parent.database_id = %#v", parent["database_id"])
			}
			children, _ := createBody["children"].([]any)
			createChildren = len(children)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"page_123","url":"https://notion.so/page_123"}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/v1/blocks/page_123/children":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode append body: %v", err)
			}
			children, _ := body["children"].([]any)
			if len(children) > maxChildrenPerRequest {
				t.Fatalf("append children too many: %d", len(children))
			}
			appendCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"results":[]}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := &config.NotionConfig{
		BaseURL:       srv.URL + "/v1",
		Token:         "secret_token",
		DatabaseID:    "db_123",
		NotionVersion: "2022-06-28",
		TitleProperty: "Name",
		URLProperty:   "SourceURL",
		Timeout:       3 * time.Second,
	}
	s := New(cfg)
	job := &store.Job{
		ID:        "job-1",
		URL:       "https://example.com/video",
		CreatedAt: time.Now().UTC(),
		Source: &ytdlp.Info{
			Title:    "Notion 导出测试",
			Uploader: "tester",
		},
	}
	for i := 0; i < 140; i++ {
		job.Chapters = append(job.Chapters, store.Chapter{
			Title:    "章节 " + strings.Repeat("A", 3),
			StartSec: float64(i * 10),
			EndSec:   float64(i*10 + 9),
			Bullets:  []string{"要点 1", "要点 2"},
		})
	}

	res, err := s.ExportJob(context.Background(), job)
	if err != nil {
		t.Fatalf("ExportJob: %v", err)
	}
	if res.PageID != "page_123" || res.PageURL == "" {
		t.Fatalf("unexpected result: %+v", res)
	}
	if createChildren == 0 || createChildren > maxChildrenPerRequest {
		t.Fatalf("create children = %d", createChildren)
	}
	if appendCalls == 0 {
		t.Fatalf("expected append calls > 0")
	}
	props, _ := createBody["properties"].(map[string]any)
	if _, ok := props["Name"]; !ok {
		t.Fatalf("missing database title property in request")
	}
}
