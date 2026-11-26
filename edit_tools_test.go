package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type capturedRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   any
}

func newCaptureServer(t *testing.T) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Method = r.Method
		captured.Path = r.URL.EscapedPath()
		captured.Query = r.URL.Query()
		captured.Body = nil
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			var v any
			if err := json.Unmarshal(b, &v); err != nil {
				t.Errorf("request body is not JSON: %s", b)
			}
			captured.Body = v
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

func testServer() *server.MCPServer {
	s := server.NewMCPServer("test", "0", server.WithToolCapabilities(true))
	s.AddTool(mcp.NewTool("library", libraryToolOptions()...), libraryHandler())
	registerEditTools(s)
	return s
}

// callTool invokes a registered tool with JSON-encoded arguments, the way a real client would.
func callTool(t *testing.T, s *server.MCPServer, baseURL, name, argsJSON string) *mcp.CallToolResult {
	t.Helper()
	tool := s.GetTool(name)
	if tool == nil {
		t.Fatalf("tool %q is not registered", name)
	}
	args := map[string]any{}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		t.Fatalf("bad test args: %v", err)
	}
	args["base_url"] = baseURL
	args["token"] = "test-token"
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := tool.Handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	return res
}

func resultText(r *mcp.CallToolResult) string {
	if len(r.Content) == 0 {
		return ""
	}
	if tc, ok := r.Content[0].(mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

func mustJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestEditToolsRequests(t *testing.T) {
	srv, got := newCaptureServer(t)
	s := testServer()

	tests := []struct {
		name, tool, args   string
		method, path, body string
		query              map[string]string
	}{
		{
			name: "update_item sends only passed fields",
			tool: "update_item",
			args: `{"item_id":"li1","title":"Петля судеб. Том 2","authors":["Андрей Васильев"],"narrators":["Александр Чайцын"],
				"series":[{"name":"Файролл","sequence":"13.2"}],"genres":["ЛитРПГ"],"tags":[],"published_year":2021,"subtitle":""}`,
			method: "PATCH", path: "/api/items/li1/media",
			body: `{"tags":[],"metadata":{"title":"Петля судеб. Том 2","subtitle":"","authors":[{"name":"Андрей Васильев"}],
				"narrators":["Александр Чайцын"],"series":[{"name":"Файролл","sequence":"13.2"}],"genres":["ЛитРПГ"],"publishedYear":"2021"}}`,
		},
		{
			name:   "update_item numeric sequence becomes string, empty series clears",
			tool:   "update_item",
			args:   `{"item_id":"li1","series":[{"name":"A","sequence":3}]}`,
			method: "PATCH", path: "/api/items/li1/media",
			body: `{"metadata":{"series":[{"name":"A","sequence":"3"}]}}`,
		},
		{
			name:   "batch_update_items",
			tool:   "batch_update_items",
			args:   `{"updates":[{"item_id":"a","title":"X"},{"item_id":"b","genres":["Фэнтези"]}]}`,
			method: "POST", path: "/api/items/batch/update",
			body: `[{"id":"a","mediaPayload":{"metadata":{"title":"X"}}},{"id":"b","mediaPayload":{"metadata":{"genres":["Фэнтези"]}}}]`,
		},
		{
			name: "batch_get_items", tool: "batch_get_items", args: `{"item_ids":["a","b"]}`,
			method: "POST", path: "/api/items/batch/get", body: `{"libraryItemIds":["a","b"]}`,
		},
		{
			name: "match_item", tool: "match_item", args: `{"item_id":"a","provider":"custom-fantlab","title":"T","override_details":false}`,
			method: "POST", path: "/api/items/a/match", body: `{"provider":"custom-fantlab","title":"T","overrideDetails":false}`,
		},
		{
			name: "set_item_cover url", tool: "set_item_cover", args: `{"item_id":"a","url":"https://x/c.jpg"}`,
			method: "POST", path: "/api/items/a/cover", body: `{"url":"https://x/c.jpg"}`,
		},
		{
			name: "set_item_cover path", tool: "set_item_cover", args: `{"item_id":"a","path":"/audiobooks/a/cover.jpg"}`,
			method: "PATCH", path: "/api/items/a/cover", body: `{"cover":"/audiobooks/a/cover.jpg"}`,
		},
		{name: "scan_item", tool: "scan_item", args: `{"item_id":"a"}`, method: "POST", path: "/api/items/a/scan"},
		{name: "delete_item keeps files", tool: "delete_item", args: `{"item_id":"a"}`, method: "DELETE", path: "/api/items/a"},
		{name: "delete_item hard", tool: "delete_item", args: `{"item_id":"a","delete_files":true}`, method: "DELETE", path: "/api/items/a", query: map[string]string{"hard": "1"}},
		{name: "scan_library force", tool: "scan_library", args: `{"library_id":"L","force":true}`, method: "POST", path: "/api/libraries/L/scan", query: map[string]string{"force": "1"}},
		{
			name: "update_library settings", tool: "update_library", args: `{"library_id":"L","settings":{"autoScanCronExpression":"*/15 * * * *"}}`,
			method: "PATCH", path: "/api/libraries/L", body: `{"settings":{"autoScanCronExpression":"*/15 * * * *"}}`,
		},
		{
			name: "update_author", tool: "update_author", args: `{"author_id":"au","name":"Алекс Экслер"}`,
			method: "PATCH", path: "/api/authors/au", body: `{"name":"Алекс Экслер"}`,
		},
		{
			name: "update_series", tool: "update_series", args: `{"series_id":"se","name":"Господство клана Неспящих"}`,
			method: "PATCH", path: "/api/series/se", body: `{"name":"Господство клана Неспящих"}`,
		},
		{
			name: "rename_narrator", tool: "rename_narrator", args: `{"library_id":"L","name":"Äèì Äèìû÷","new_name":"Дим Димыч"}`,
			method: "PATCH", path: "/api/libraries/L/narrators/" + url.QueryEscape(b64("Äèì Äèìû÷")), body: `{"name":"Дим Димыч"}`,
		},
		{
			name: "rename_genre", tool: "rename_genre", args: `{"genre":"аудиокнига","new_genre":"Аудиокнига"}`,
			method: "POST", path: "/api/genres/rename", body: `{"genre":"аудиокнига","newGenre":"Аудиокнига"}`,
		},
		{name: "delete_genre", tool: "delete_genre", args: `{"genre":"Bibe.ru"}`, method: "DELETE", path: "/api/genres/" + url.QueryEscape(b64("Bibe.ru"))},
		{
			name: "rename_tag", tool: "rename_tag", args: `{"tag":"a","new_tag":"b"}`,
			method: "POST", path: "/api/tags/rename", body: `{"tag":"a","newTag":"b"}`,
		},
		{name: "delete_tag", tool: "delete_tag", args: `{"tag":"Speech"}`, method: "DELETE", path: "/api/tags/" + url.QueryEscape(b64("Speech"))},
		{
			name: "update_collection order", tool: "update_collection", args: `{"collection_id":"c","book_ids":["b","a"]}`,
			method: "PATCH", path: "/api/collections/c", body: `{"books":["b","a"]}`,
		},
		{
			name: "batch_add_to_collection", tool: "batch_add_to_collection", args: `{"collection_id":"c","book_ids":["a","b"]}`,
			method: "POST", path: "/api/collections/c/batch/add", body: `{"books":["a","b"]}`,
		},
		{name: "remove_from_collection", tool: "remove_from_collection", args: `{"collection_id":"c","book_id":"a"}`, method: "DELETE", path: "/api/collections/c/book/a"},
		{name: "delete_collection", tool: "delete_collection", args: `{"collection_id":"c"}`, method: "DELETE", path: "/api/collections/c"},
		{name: "library series gets default limit", tool: "library", args: `{"library_id":"L","series":true}`, method: "GET", path: "/api/libraries/L/series", query: map[string]string{"limit": "1000", "page": "0"}},
		{name: "library q implies search", tool: "library", args: `{"library_id":"L","q":"Файролл","limit":5}`, method: "GET", path: "/api/libraries/L/search", query: map[string]string{"q": "Файролл", "limit": "5"}},
		{
			name: "library items with filter", tool: "library", args: `{"library_id":"L","items":true,"filter_group":"narrators","filter_value":"Александр Чайцын","minified":true,"sort":"media.metadata.title"}`,
			method: "GET", path: "/api/libraries/L/items",
			query: map[string]string{"filter": "narrators." + b64("Александр Чайцын"), "minified": "1", "sort": "media.metadata.title"},
		},
		{name: "library plain", tool: "library", args: `{"library_id":"L"}`, method: "GET", path: "/api/libraries/L"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := callTool(t, s, srv.URL, tt.tool, tt.args)
			if res.IsError {
				t.Fatalf("unexpected tool error: %s", resultText(res))
			}
			if got.Method != tt.method || got.Path != tt.path {
				t.Fatalf("request = %s %s, want %s %s", got.Method, got.Path, tt.method, tt.path)
			}
			for k, v := range tt.query {
				if got.Query.Get(k) != v {
					t.Errorf("query %s = %q, want %q (all: %v)", k, got.Query.Get(k), v, got.Query)
				}
			}
			if tt.query == nil && len(got.Query) > 0 {
				t.Errorf("unexpected query %v", got.Query)
			}
			if tt.body == "" {
				if got.Body != nil {
					t.Errorf("unexpected body %v", got.Body)
				}
				return
			}
			if want := mustJSON(t, tt.body); !reflect.DeepEqual(got.Body, want) {
				gb, _ := json.Marshal(got.Body)
				t.Errorf("body = %s\nwant  %s", gb, strings.Join(strings.Fields(tt.body), " "))
			}
		})
	}
}

func TestEditToolsValidation(t *testing.T) {
	srv, got := newCaptureServer(t)
	s := testServer()

	cases := []struct{ name, tool, args string }{
		{"update_item without fields", "update_item", `{"item_id":"a"}`},
		{"update_item bad series", "update_item", `{"item_id":"a","series":[{"sequence":"1"}]}`},
		{"batch duplicate ids", "batch_update_items", `{"updates":[{"item_id":"a","title":"x"},{"item_id":"a","title":"y"}]}`},
		{"batch empty", "batch_update_items", `{"updates":[]}`},
		{"cover both", "set_item_cover", `{"item_id":"a","url":"u","path":"p"}`},
		{"cover none", "set_item_cover", `{"item_id":"a"}`},
		{"search without q", "library", `{"library_id":"L","search":true}`},
		{"author nothing", "update_author", `{"author_id":"a"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got.Method = ""
			res := callTool(t, s, srv.URL, c.tool, c.args)
			if !res.IsError {
				t.Fatalf("expected error, got %s", resultText(res))
			}
			if got.Method != "" {
				t.Errorf("no request should be sent, got %s %s", got.Method, got.Path)
			}
		})
	}
}
