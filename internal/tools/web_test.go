package tools

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWebFetchHTMLToMarkdown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>Doc</title><script>bad()</script></head><body>
			<h1>Hello</h1><p>See <a href="https://example.com">Example</a>.</p><ul><li>one</li></ul>
		</body></html>`)
	}))
	defer srv.Close()

	ex := &Executor{Policy: Policy{Yolo: true}}
	res := dispatch(t, ex, "webfetch", fmt.Sprintf(`{"url":%q}`, srv.URL))
	if res.Failed {
		t.Fatalf("fetch failed: %+v", res)
	}
	for _, want := range []string{"# Doc", "# Hello", "[Example](https://example.com)", "- one", "status: 200"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("missing %q in:\n%s", want, res.Content)
		}
	}
	if strings.Contains(res.Content, "bad()") {
		t.Fatalf("script survived:\n%s", res.Content)
	}
}

func TestWebFetchJSONAndTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"a":1,"b":[true,false]}`)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("a", 1000))
	}))
	defer srv.Close()

	ex := &Executor{Policy: Policy{Yolo: true}}
	res := dispatch(t, ex, "webfetch", fmt.Sprintf(`{"url":%q}`, srv.URL+"/json"))
	if res.Failed || !strings.Contains(res.Content, `"a": 1`) {
		t.Fatalf("json fetch: %+v", res)
	}
	res = dispatch(t, ex, "webfetch", fmt.Sprintf(`{"url":%q,"max_bytes":100}`, srv.URL+"/big"))
	if res.Failed {
		t.Fatalf("big fetch failed: %+v", res)
	}
	if !strings.Contains(res.Content, "truncated: true") || !strings.Contains(res.Content, "bytes: 100") {
		t.Fatalf("truncation not reported:\n%s", res.Content)
	}
	if strings.Count(res.Content, "a") > 200 {
		t.Fatalf("body not capped")
	}
}

func TestWebFetchGuards(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	offline := &Executor{Policy: Policy{Yolo: true}, Offline: true}
	res := dispatch(t, offline, "webfetch", `{"url":"https://example.com"}`)
	if !res.Failed || !strings.Contains(res.Content, "offline mode") {
		t.Fatalf("offline guard: %+v", res)
	}
	res = dispatch(t, offline, "webfetch", fmt.Sprintf(`{"url":%q}`, srv.URL))
	if res.Failed {
		t.Fatalf("loopback should work offline: %+v", res)
	}

	unapproved := &Executor{}
	res = dispatch(t, unapproved, "webfetch", fmt.Sprintf(`{"url":%q}`, srv.URL))
	if !res.Failed || !strings.Contains(res.Content, "requires approval") {
		t.Fatalf("approval guard: %+v", res)
	}

	res = dispatch(t, offline, "webfetch", `{"url":"file:///etc/passwd"}`)
	if !res.Failed || !strings.Contains(res.Content, "not allowed") {
		t.Fatalf("scheme guard: %+v", res)
	}
	res = dispatch(t, offline, "webfetch", `{"url":"http://user:pass@example.com/"}`)
	if !res.Failed || !strings.Contains(res.Content, "credentials") {
		t.Fatalf("userinfo guard: %+v", res)
	}
	if hits.Load() == 0 {
		t.Fatal("loopback fetch never reached server")
	}
}

func TestWebFetchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	ex := &Executor{Policy: Policy{Yolo: true}}
	res := dispatch(t, ex, "webfetch", fmt.Sprintf(`{"url":%q}`, srv.URL))
	if !res.Failed || !strings.Contains(res.Content, "status 404") {
		t.Fatalf("http error: %+v", res)
	}
}
