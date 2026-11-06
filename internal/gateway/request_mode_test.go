package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func requestMode(t *testing.T, mode string) {
	t.Helper()
	s := settings.Load()
	s.RequestMode = mode
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
}

func requestModeProvider(t *testing.T, id string, proto provider.Protocol, serve http.HandlerFunc) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		n.Add(1)
		serve(w, r)
	}))
	t.Cleanup(up.Close)
	p := provider.Provider{ID: id, Name: strings.ToUpper(id), Key: "local-test-key", Models: []string{"m"}}
	switch proto {
	case provider.Responses:
		p.Responses = up.URL + "/v1"
	case provider.Anthropic:
		p.Anthropic = up.URL
	default:
		p.Chat = up.URL + "/v1"
	}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	return &n
}

func requestModeResponse(s *Server, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	return rec
}

// The 502 and its wording are the archived relay's error. Even a request
// larger than the catalog's 272K window is not evidence of an overflow.
func TestRequestModePassthroughDoesNotInferOverflow(t *testing.T) {
	for _, mode := range []string{"", settings.RequestPassthrough} {
		t.Run("mode="+mode, func(t *testing.T) {
			fresh(t)
			requestMode(t, mode)
			const upstream = `{"code":"upstream_error","message":"Upstream service temporarily unavailable"}`
			n := requestModeProvider(t, "relay", provider.Responses, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "17")
				w.WriteHeader(http.StatusBadGateway)
				io.WriteString(w, upstream)
			})
			p, _ := provider.Find("relay")
			p.Contexts = map[string]int{"m": 272000}
			if err := provider.Save(*p); err != nil {
				t.Fatal(err)
			}
			rec := requestModeResponse(New(), `{"model":"relay/m","stream":true,"input":"`+strings.Repeat("x", 1700000)+`"}`)
			if rec.Code != http.StatusBadGateway || rec.Body.String() != upstream || rec.Header().Get("Retry-After") != "17" {
				t.Fatalf("relay error changed: status=%d retry-after=%q body=%s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
			}
			if n.Load() != 1 {
				t.Fatalf("one client request reached the upstream %d times", n.Load())
			}
		})
	}
}

func TestRequestModePassthroughKeepsHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{"invalid request", 400, `{"error":{"message":"Invalid request parameters","type":"invalid_request_error","code":"invalid_argument"}}`},
		{"authentication", 401, `{"error":{"message":"invalid api key","type":"authentication_error"}}`},
		{"rate limit", 429, `{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit_exceeded"}}`},
		{"body size", 413, `{"error":{"message":"body exceeds the byte limit","code":"request_too_large"}}`},
		{"explicit overflow", 400, `{"error":{"message":"Your input exceeds the context window of this model.","code":"context_length_exceeded","type":"invalid_request_error"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			requestMode(t, settings.RequestPassthrough)
			n := requestModeProvider(t, "relay", provider.Responses, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "11")
				w.WriteHeader(tc.code)
				io.WriteString(w, tc.body)
			})
			rec := requestModeResponse(New(), `{"model":"relay/m","stream":true,"input":"hi"}`)
			if rec.Code != tc.code || rec.Body.String() != tc.body || n.Load() != 1 {
				t.Fatalf("status=%d calls=%d body=%s; want %d %s", rec.Code, n.Load(), rec.Body.String(), tc.code, tc.body)
			}
			if rec.Header().Get("Retry-After") != "11" {
				t.Fatalf("lost upstream Retry-After: %v", rec.Header())
			}
		})
	}
}

func TestRequestModePassthroughKeepsNetworkFailure(t *testing.T) {
	fresh(t)
	requestMode(t, settings.RequestPassthrough)
	n := requestModeProvider(t, "relay", provider.Responses, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	})
	p, _ := provider.Find("relay")
	p.Contexts = map[string]int{"m": 32}
	if err := provider.Save(*p); err != nil {
		t.Fatal(err)
	}
	rec := requestModeResponse(New(), `{"model":"relay/m","input":"`+strings.Repeat("x", 4096)+`"}`)
	body := rec.Body.String()
	if rec.Code != 502 || n.Load() != 1 || strings.Contains(body, "context_length_exceeded") || strings.Contains(body, "prompt is too long") {
		t.Fatalf("network failure changed: status=%d calls=%d body=%s", rec.Code, n.Load(), body)
	}
	if !strings.Contains(body, "EOF") && !strings.Contains(strings.ToLower(body), "connection") {
		t.Fatalf("network failure lost its cause: %s", body)
	}
}

func TestRequestModePassthroughTranslatedHTTPError(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		msg    string
	}{
		{400, "invalid_argument", "Invalid request parameters"},
		{413, "request_too_large", "body exceeds the byte limit"},
		{502, "upstream_error", "Upstream service temporarily unavailable"},
		{400, "context_length_exceeded", "Your input exceeds the context window of this model."},
	} {
		t.Run(tc.code, func(t *testing.T) {
			fresh(t)
			requestMode(t, settings.RequestPassthrough)
			n := requestModeProvider(t, "relay", provider.Chat, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "9")
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"error":{"message":%q,"code":%q,"type":"request_error"}}`, tc.msg, tc.code)
			})
			rec := requestModeResponse(New(), `{"model":"relay/m","stream":true,"input":"hi"}`)
			body := rec.Body.String()
			if rec.Code != tc.status || n.Load() != 1 || !strings.Contains(body, tc.msg) || gjson.Get(body, "error.code").String() != tc.code {
				t.Fatalf("translated failure changed: status=%d calls=%d body=%s", rec.Code, n.Load(), body)
			}
			if rec.Header().Get("Retry-After") != "9" {
				t.Fatalf("translated failure lost Retry-After: %v", rec.Header())
			}
		})
	}
}

// The selected key's failure is the client's: another key, fallback model
// or routing-group member must not turn one request into several calls.
func TestRequestModePassthroughDoesNotChangeCandidate(t *testing.T) {
	for _, group := range []bool{false, true} {
		t.Run(fmt.Sprintf("group=%v", group), func(t *testing.T) {
			fresh(t)
			requestMode(t, settings.RequestPassthrough)
			first := requestModeProvider(t, "first", provider.Responses, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(503)
				io.WriteString(w, `{"error":{"message":"first member unavailable","code":"upstream_error"}}`)
			})
			spare := requestModeProvider(t, "spare", provider.Responses, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"spare","object":"response","status":"completed","output":[]}`)
			})
			p, _ := provider.Find("first")
			p.Keys = []provider.KeyAccount{{Key: "another-local-key"}}
			p.Fallback = []string{"spare/m"}
			if err := provider.Save(*p); err != nil {
				t.Fatal(err)
			}
			model := "first/m"
			if group {
				if err := provider.SaveGroup(provider.Group{Name: "G", Members: []string{"first/m", "spare/m"}, Routing: provider.Ordered}); err != nil {
					t.Fatal(err)
				}
				model = "group/g"
			}
			rec := requestModeResponse(New(), `{"model":"`+model+`","input":"hi"}`)
			if rec.Code != 503 || !strings.Contains(rec.Body.String(), "first member unavailable") || first.Load() != 1 || spare.Load() != 0 {
				t.Fatalf("status=%d first=%d spare=%d body=%s", rec.Code, first.Load(), spare.Load(), rec.Body.String())
			}
		})
	}
}

func TestRequestModePassthroughKeepsSSEFailure(t *testing.T) {
	failure := sse(`event: response.created`+"\n"+`data: {"type":"response.created","response":{"id":"resp_failure","status":"in_progress","output":[]}}`,
		`event: response.failed`+"\n"+`data: {"type":"response.failed","response":{"id":"resp_failure","status":"failed","error":{"code":"upstream_error","message":"Upstream service temporarily unavailable"}}}`)
	fresh(t)
	requestMode(t, settings.RequestPassthrough)
	n := requestModeProvider(t, "relay", provider.Responses, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, failure)
	})
	rec := requestModeResponse(New(), `{"model":"relay/m","stream":true,"input":"hi"}`)
	body := rec.Body.String()
	if rec.Code != 200 || n.Load() != 1 || !strings.Contains(body, `"code":"upstream_error"`) || !strings.Contains(body, "Upstream service temporarily unavailable") {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, n.Load(), body)
	}
	if strings.Contains(body, "context_length_exceeded") || strings.Contains(body, "response.completed") {
		t.Fatalf("upstream failure became a different ending: %s", body)
	}
}

// A Responses stream cut after text must stay a failed stream. The next
// upstream answer is not spliced onto it and it never ends completed.
func TestRequestModePassthroughDoesNotResumeCutStream(t *testing.T) {
	fresh(t)
	requestMode(t, settings.RequestPassthrough)
	part := sse(`event: response.created`+"\n"+`data: {"type":"response.created","response":{"id":"resp_cut","status":"in_progress","output":[]}}`,
		`event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","item_id":"msg_cut","output_index":0,"content_index":0,"delta":"partial answer"}`)
	n := requestModeProvider(t, "relay", provider.Responses, func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: %d\r\n\r\n", len(part)+100)
		io.WriteString(buf, part)
		buf.Flush()
	})
	rec := requestModeResponse(New(), `{"model":"relay/m","stream":true,"input":"hi"}`)
	body := rec.Body.String()
	if rec.Code != 200 || n.Load() != 1 || !strings.Contains(body, "partial answer") || !strings.Contains(body, "error") {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, n.Load(), body)
	}
	if strings.Contains(body, "response.completed") || strings.Contains(body, "context_length_exceeded") {
		t.Fatalf("a cut stream became completed or an overflow: %s", body)
	}
}

func TestRequestModePassthroughDoesNotRetryTranslatedEmptyReply(t *testing.T) {
	for _, reasoning := range []string{"", "thinking it over"} {
		t.Run("reasoning="+reasoning, func(t *testing.T) {
			fresh(t)
			requestMode(t, settings.RequestPassthrough)
			n := requestModeProvider(t, "relay", provider.Chat, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, sse(`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"`+reasoning+`"}}]}`,
					`data: {"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
					`data: [DONE]`))
			})
			rec := requestModeResponse(New(), `{"model":"relay/m","stream":true,"input":"hi"}`)
			if n.Load() != 1 {
				t.Fatalf("a translated empty reply was asked for %d times: %d %s", n.Load(), rec.Code, rec.Body.String())
			}
		})
	}
}

// A settings change made while the upstream is answering applies to the
// next request. It must not turn this passthrough request into a retry.
func TestRequestModeIsFixedForTheRequest(t *testing.T) {
	fresh(t)
	requestMode(t, settings.RequestPassthrough)
	var calls atomic.Int32
	requestModeProvider(t, "relay", provider.Chat, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			s := settings.Load()
			s.RequestMode = settings.RequestManaged
			if err := settings.Save(s); err != nil {
				t.Error(err)
			}
		}
		if n < 3 {
			w.WriteHeader(503)
			io.WriteString(w, `{"error":{"message":"temporary upstream failure"}}`)
			return
		}
		io.WriteString(w, chatOK)
	})
	s := New()
	const body = `{"model":"relay/m","messages":[{"role":"user","content":"hi"}]}`
	code, got := postAs(t, s, "", body)
	if code != 503 || calls.Load() != 1 {
		t.Fatalf("in-flight mode changed: status=%d calls=%d body=%s", code, calls.Load(), got)
	}
	code, got = postAs(t, s, "", body)
	if code != 200 || calls.Load() != 3 || !strings.Contains(got, "hello") {
		t.Fatalf("next request did not use managed retries: status=%d calls=%d body=%s", code, calls.Load(), got)
	}
}

func TestRequestModePassthroughResponsesSocketDoesNotRetry(t *testing.T) {
	fresh(t)
	requestMode(t, settings.RequestPassthrough)
	u := &respUp{reply: func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		io.WriteString(w, `{"error":{"message":"fixture upstream unavailable","type":"upstream_error"}}`)
	}}
	sockUp(t, u)
	_, ts := sockServer(t)
	c, _, err := dialSock(t, ts, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := c.turn(create("", userHi))
	e := last(got)
	if e.Get("type").String() != "error" || e.Get("status").Int() != 502 || !strings.Contains(e.Get("error.message").String(), "fixture upstream unavailable") || u.calls() != 1 {
		t.Fatalf("one socket turn: calls=%d events=%v", u.calls(), got)
	}
}

func TestRequestModeTransportDoesNotMakePostReplayable(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(fmt.Sprintf("managed=%v", managed), func(t *testing.T) {
			var calls int
			var marked bool
			transport := replayable{roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				_, has := r.Header["X-Idempotency-Key"]
				marked = marked || has
				if tr := httptrace.ContextClientTrace(r.Context()); tr != nil && tr.GotConn != nil {
					tr.GotConn(httptrace.GotConnInfo{Reused: false})
				}
				return nil, net.ErrClosed
			})}
			ctx := context.WithValue(context.Background(), requestModeKey{}, managed)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://local.invalid/v1/responses", strings.NewReader(`{"input":"hi"}`))
			if err != nil {
				t.Fatal(err)
			}
			_, err = transport.RoundTrip(req)
			want := 1
			if managed {
				want += len(unsentAgain)
			}
			if !errors.Is(err, net.ErrClosed) || calls != want || marked != managed {
				t.Fatalf("calls=%d marked=%v err=%v; want calls=%d marked=%v", calls, marked, err, want, managed)
			}
			if _, changed := req.Header["X-Idempotency-Key"]; changed {
				t.Fatal("transport changed its caller's request headers")
			}
		})
	}
}

func TestRequestModeTransportDoesNotResendAfterLostPing(t *testing.T) {
	for _, managed := range []bool{false, true} {
		t.Run(fmt.Sprintf("managed=%v", managed), func(t *testing.T) {
			var ordinary, uploads int
			var slow atomic.Int64
			ping := errors.New("http2: client connection lost")
			transport := drawsApart{
				any: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					ordinary++
					return nil, ping
				}),
				uploads: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					uploads++
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
				}),
				slow: &slow,
			}
			ctx := context.WithValue(context.Background(), requestModeKey{}, managed)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://local.invalid/v1/responses", strings.NewReader(strings.Repeat("x", int(uploadBytes))))
			if err != nil {
				t.Fatal(err)
			}
			res, err := transport.RoundTrip(req)
			if res != nil {
				res.Body.Close()
			}
			if managed {
				if err != nil || ordinary != 1 || uploads != 1 || slow.Load() == 0 {
					t.Fatalf("managed lost-ping recovery: ordinary=%d uploads=%d slow=%d err=%v", ordinary, uploads, slow.Load(), err)
				}
			} else if !errors.Is(err, ping) || ordinary != 1 || uploads != 0 || slow.Load() != 0 {
				t.Fatalf("passthrough lost-ping error was retried: ordinary=%d uploads=%d slow=%d err=%v", ordinary, uploads, slow.Load(), err)
			}
		})
	}
}

// modeHeaderWriter reports any attempt to start the client's response.
// The recorder is read only after the handler has stopped writing.
type modeHeaderWriter struct {
	*httptest.ResponseRecorder
	written atomic.Bool
	head    chan int
}

func (w *modeHeaderWriter) note(status int) {
	if w.written.CompareAndSwap(false, true) {
		w.head <- status
	}
}

func (w *modeHeaderWriter) WriteHeader(status int) {
	w.note(status)
	w.ResponseRecorder.WriteHeader(status)
}

func (w *modeHeaderWriter) Write(b []byte) (int, error) {
	w.note(http.StatusOK)
	return w.ResponseRecorder.Write(b)
}

func (w *modeHeaderWriter) WriteString(s string) (int, error) {
	w.note(http.StatusOK)
	return w.ResponseRecorder.WriteString(s)
}

func (w *modeHeaderWriter) Flush() {
	w.note(http.StatusOK)
	w.ResponseRecorder.Flush()
}

func TestRequestModePassthroughWaitsForUpstreamHTTPStatus(t *testing.T) {
	fresh(t)
	requestMode(t, settings.RequestPassthrough)
	release := make(chan struct{})
	arrived := make(chan struct{})
	requestModeProvider(t, "relay", provider.Responses, func(w http.ResponseWriter, r *http.Request) {
		close(arrived)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		io.WriteString(w, `{"error":{"code":"upstream_error","message":"delayed upstream failure"}}`)
	})
	w := &modeHeaderWriter{ResponseRecorder: httptest.NewRecorder(), head: make(chan int, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		New().Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"relay/m","stream":true,"input":"hi"}`)))
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		close(release)
		<-done
		t.Fatal("upstream request never arrived")
	}
	// The upstream is deliberately blocked beyond the legacy keepalive
	// threshold and several watch ticks, rather than relying on a fast run.
	var early int
	select {
	case early = <-w.head:
	case <-time.After(keepHeldAfter + 3*watchEvery):
	}
	close(release)
	<-done
	if early != 0 || w.Code != 502 || !strings.Contains(w.Body.String(), "delayed upstream failure") {
		t.Fatalf("client headers started before the vendor answered: early=%d status=%d body=%s", early, w.Code, w.Body.String())
	}
}

// Passing an account over before a generation was sent is selection,
// not a retry: the next eligible account must still be considered.
func TestRequestModePassthroughSelectsAfterOnAccountSkip(t *testing.T) {
	codexSignedIn(t, "spare@example.com")
	requestMode(t, settings.RequestPassthrough)
	if err := provider.SetRouting("codex", provider.Ordered); err != nil {
		t.Fatal(err)
	}
	var tried []string
	chatgptRefusing(t, &tried, func(w http.ResponseWriter) { t.Error("skipped account was sent the generation") })
	onMiddleware(t, `
export function onAccount(account, ctx) {
  if (ctx.state.passed) return;
  ctx.state.passed = account.ref;
  return {skip: true};
}`)
	code, body := codexAskOn(New(), `{"model":"gpt-5.5","stream":true,"input":"ping"}`)
	if code != 200 || !strings.Contains(body, "pong") || strings.Join(tried, ",") != "acct-2" {
		t.Fatalf("initial selection did not reach the eligible account alone: status=%d tried=%v body=%s", code, tried, body)
	}
	if rest, ok := restOf("codex@me@example.com"); ok {
		t.Fatalf("the skipped account was rested: %+v", rest)
	}
}

// The native backend route can bypass serveAgent when Codex brings its
// own sign-in. It must still use this computer's failure policy.
func TestRequestModePassthroughNativeSocketHandshakeRefusal(t *testing.T) {
	fresh(t)
	socketOn(t)
	f := &fakeSocketBackend{refuse: http.StatusForbidden}
	var handshakes atomic.Int32
	chatgpt(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			handshakes.Add(1)
		}
		f.ServeHTTP(w, r)
	})
	requestMode(t, settings.RequestPassthrough)
	for i := range 2 {
		code, body := codexPost(t, `{"model":"gpt-5.5","stream":true,"input":"ping"}`)
		if code != 403 || !strings.Contains(body, "no sockets here") {
			t.Fatalf("turn %d lost the handshake refusal: status=%d body=%s", i, code, body)
		}
	}
	if posts, _ := f.counts(); posts != 0 || handshakes.Load() != 2 {
		t.Fatalf("native handshake refusal caused HTTP fallback or shunning: posts=%d handshakes=%d", posts, handshakes.Load())
	}
}

func TestRequestModePassthroughNativeSocketFailureDoesNotPost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames func(socketTurn) []string
		close  websocket.StatusCode
		status int
		cause  string
	}{
		{"internal close", func(socketTurn) []string { return nil }, websocket.StatusInternalError, 502, "StatusInternalError"},
		{"message too big", func(socketTurn) []string { return nil }, websocket.StatusMessageTooBig, 502, "StatusMessageTooBig"},
		{"connection limit", func(socketTurn) []string {
			return []string{`{"type":"error","status":429,"error":{"code":"websocket_connection_limit_reached","message":"fixture socket connection limit"}}`}
		}, 0, 429, "fixture socket connection limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			socketOn(t)
			f := &fakeSocketBackend{frames: tc.frames, closeWith: tc.close}
			chatgpt(t, f.ServeHTTP)
			requestMode(t, settings.RequestPassthrough)
			for i := range 2 {
				code, body := codexPost(t, `{"model":"gpt-5.5","stream":true,"input":"ping"}`)
				if code != tc.status || !strings.Contains(body, tc.cause) {
					t.Fatalf("turn %d lost the socket failure: status=%d body=%s; want %d containing %q", i, code, body, tc.status, tc.cause)
				}
			}
			if posts, sockets := f.counts(); posts != 0 || sockets != 2 {
				t.Fatalf("native socket failure caused fallback/retries: posts=%d sockets=%d", posts, sockets)
			}
			codexShunMu.Lock()
			shunned, tooBig := len(codexShun), len(codexTooBig)
			codexShunMu.Unlock()
			if shunned != 0 || tooBig != 0 {
				t.Fatalf("passthrough failure changed future transport selection: shunned=%d tooBig=%d", shunned, tooBig)
			}
		})
	}
}
