package llm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gsh/internal/api"
)

func TestManagerSingleModelBackwardCompatible(t *testing.T) {
	m, err := NewManager(map[string]any{
		"provider": "openai",
		"model":    "m1",
		"api_key":  "k",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Names(); len(got) != 1 || got[0] != "default" {
		t.Fatalf("旧单模型配置应退化成 default: %v", got)
	}
	if m.ActiveName() != "default" || m.AuxName() != "default" {
		t.Fatal("主/辅都应指向 default")
	}
}

func TestManagerMultipleModelsInheritCommonFields(t *testing.T) {
	m, err := NewManager(map[string]any{
		"provider": "openai",
		"base_url": "http://common.invalid",
		"api_key":  "common-key",
		"active":   "fast",
		"aux":      "cheap",
		"models": map[string]any{
			"main":  map[string]any{"model": "big-model"},
			"fast":  map[string]any{"model": "fast-model"},
			"cheap": map[string]any{"model": "cheap-model", "api_key": "own-key"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	names := m.Names()
	if strings.Join(names, ",") != "cheap,fast,main" {
		t.Fatalf("模型名应排序: %v", names)
	}
	if m.ActiveName() != "fast" || m.AuxName() != "cheap" {
		t.Fatalf("active/aux 解析错误: %s / %s", m.ActiveName(), m.AuxName())
	}
	// 公共字段继承与覆盖
	mainSpec := m.specs["main"]
	if mainSpec["api_key"] != "common-key" || mainSpec["base_url"] != "http://common.invalid" {
		t.Fatalf("公共字段未继承: %v", mainSpec)
	}
	if m.specs["cheap"]["api_key"] != "own-key" {
		t.Fatal("模型内字段应覆盖公共字段")
	}
	if err := m.Use("nope"); err == nil {
		t.Fatal("切到不存在的模型应报错")
	}
	if err := m.Use("main"); err != nil || m.ActiveName() != "main" {
		t.Fatalf("切换失败: %v", err)
	}
	if m.AuxName() != "cheap" {
		t.Fatal("切主模型不应改变辅助模型")
	}
}

func TestManagerSwitchRoutesToDifferentEndpoint(t *testing.T) {
	var mainHits, auxHits atomic.Int32
	newSrv := func(counter *atomic.Int32, text string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			counter.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, sseBody(`{"choices":[{"delta":{"content":"`+text+`"}}]}`))
		}))
	}
	srvA := newSrv(&mainHits, "A")
	defer srvA.Close()
	srvB := newSrv(&auxHits, "B")
	defer srvB.Close()

	m, err := NewManager(map[string]any{
		"active": "a", "aux": "b",
		"models": map[string]any{
			"a": map[string]any{"base_url": srvA.URL, "model": "a", "api_key": "x"},
			"b": map[string]any{"base_url": srvB.URL, "model": "b", "api_key": "x"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	main, aux := m.CurrentProxy(), m.AuxProxy()
	msgs := []api.Message{{Role: api.RoleUser, Content: "hi"}}
	got, err := main.Chat(context.Background(), msgs, nil, api.StreamHandlers{})
	if err != nil || got.Content != "A" || mainHits.Load() != 1 {
		t.Fatalf("主模型路由错误: %q err=%v hits=%d", got.Content, err, mainHits.Load())
	}
	got, err = aux.Chat(context.Background(), msgs, nil, api.StreamHandlers{})
	if err != nil || got.Content != "B" || auxHits.Load() != 1 {
		t.Fatalf("辅助模型路由错误: %q err=%v hits=%d", got.Content, err, auxHits.Load())
	}
	if err := m.Use("b"); err != nil {
		t.Fatal(err)
	}
	got, err = main.Chat(context.Background(), msgs, nil, api.StreamHandlers{})
	if err != nil || got.Content != "B" || mainHits.Load() != 1 {
		t.Fatalf("热切换后主模型应打到 B: %q hits(main=%d aux=%d)", got.Content, mainHits.Load(), auxHits.Load())
	}
	if auxHits.Load() != 2 {
		t.Fatalf("热切换后请求应落在 B：aux hits=%d", auxHits.Load())
	}
}

func TestManagerRejectsBadProviderEarly(t *testing.T) {
	_, err := NewManager(map[string]any{
		"models": map[string]any{"broken": map[string]any{"provider": "nope"}},
	})
	if err == nil {
		t.Fatal("配置错误应在启动时暴露，而不是等第一次对话")
	}
}
