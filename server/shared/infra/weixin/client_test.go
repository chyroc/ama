package weixin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct {
	method, path, query string
	header              http.Header
	body                map[string]any
}

func newServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *[]recorded) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec := recorded{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone()}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &rec.body)
		}
		mu.Lock()
		reqs = append(reqs, rec)
		mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func TestPostHeadersAndBaseInfo(t *testing.T) {
	srv, reqs := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ret":0}`))
	})
	c := New(Config{RouteTag: "rt"})
	if _, err := c.SendText(context.Background(), srv.URL, "tok", "u1", "ctx1", "hello"); err != nil {
		t.Fatal(err)
	}
	r := (*reqs)[0]
	if r.path != "/ilink/bot/sendmessage" || r.method != http.MethodPost {
		t.Fatalf("request %s %s", r.method, r.path)
	}
	if r.header.Get("Authorization") != "Bearer tok" || r.header.Get("AuthorizationType") != "ilink_bot_token" {
		t.Fatalf("auth headers %v", r.header)
	}
	if r.header.Get("iLink-App-Id") != "bot" || r.header.Get("SKRouteTag") != "rt" {
		t.Fatalf("app headers %v", r.header)
	}
	// 2.4.10 → 0x0002040A
	if r.header.Get("iLink-App-ClientVersion") != strconv.Itoa(0x0002040A) {
		t.Fatalf("client version %q", r.header.Get("iLink-App-ClientVersion"))
	}
	uin, err := base64.StdEncoding.DecodeString(r.header.Get("X-WECHAT-UIN"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strconv.ParseUint(string(uin), 10, 32); err != nil {
		t.Fatalf("X-WECHAT-UIN must be base64 of a decimal uint32, got %q", uin)
	}
	bi, _ := r.body["base_info"].(map[string]any)
	if bi["channel_version"] != DefaultChannelVersion || bi["bot_agent"] != DefaultBotAgent {
		t.Fatalf("base_info %v", bi)
	}
	msg, _ := r.body["msg"].(map[string]any)
	if msg["to_user_id"] != "u1" || msg["context_token"] != "ctx1" || msg["message_type"].(float64) != MessageTypeBot ||
		msg["message_state"].(float64) != MessageStateFinish {
		t.Fatalf("msg %v", msg)
	}
	if _, ok := msg["from_user_id"]; !ok {
		t.Fatal("from_user_id must be sent (empty), like the reference client")
	}
	items, _ := msg["item_list"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["text_item"].(map[string]any)["text"] != "hello" {
		t.Fatalf("items %v", items)
	}
}

func TestQRCodeLoginRequests(t *testing.T) {
	srv, reqs := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			_, _ = w.Write([]byte(`{"qrcode":"qr1","qrcode_img_content":"https://example.com/q/1"}`))
		case "/ilink/bot/get_qrcode_status":
			_, _ = w.Write([]byte(`{"status":"confirmed","bot_token":"t","ilink_bot_id":"b@im.bot","baseurl":"https://api.example.com","ilink_user_id":"u@im.wechat"}`))
		}
	})
	c := New(Config{LoginBaseURL: srv.URL})
	qr, err := c.GetBotQRCode(context.Background(), "", nil)
	if err != nil || qr.Code != "qr1" || qr.Content != "https://example.com/q/1" {
		t.Fatalf("qr %+v %v", qr, err)
	}
	st, err := c.GetQRCodeStatus(context.Background(), "", "qr1", "123", time.Second)
	if err != nil || st.Status != QRConfirmed || st.BotID != "b@im.bot" || st.UserID != "u@im.wechat" {
		t.Fatalf("status %+v %v", st, err)
	}
	if (*reqs)[0].query != "bot_type=3" || (*reqs)[0].header.Get("Authorization") != "" {
		t.Fatalf("qrcode request %+v", (*reqs)[0])
	}
	if list, ok := (*reqs)[0].body["local_token_list"].([]any); !ok || len(list) != 0 {
		t.Fatalf("local_token_list must be an empty array, got %v", (*reqs)[0].body)
	}
	status := (*reqs)[1]
	if status.method != http.MethodGet || !strings.Contains(status.query, "qrcode=qr1") || !strings.Contains(status.query, "verify_code=123") {
		t.Fatalf("status request %+v", status)
	}
	if status.header.Get("AuthorizationType") != "" || status.header.Get("iLink-App-Id") == "" {
		t.Fatal("status polling sends only the common app headers")
	}
}

func TestQRStatusTimeoutIsWait(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	c := New(Config{LoginBaseURL: srv.URL})
	st, err := c.GetQRCodeStatus(context.Background(), "", "qr", "", 50*time.Millisecond)
	if err != nil || st.Status != QRWait {
		t.Fatalf("our own deadline must read as wait, got %+v %v", st, err)
	}
	// The caller's cancellation is not a timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.GetQRCodeStatus(ctx, "", "qr", "", time.Second); err == nil {
		t.Fatal("a cancelled caller must get an error")
	}
}

func TestGetUpdatesParsesLargeIDsAndKeepsCursorOnTimeout(t *testing.T) {
	calls := 0
	srv, reqs := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			_, _ = w.Write([]byte(`{"ret":0,"get_updates_buf":"c2","longpolling_timeout_ms":30000,"msgs":[
				{"message_id":18446744073709551615,"from_user_id":"u","message_type":1,"context_token":"ct",
				 "item_list":[{"type":1,"text_item":{"text":"hi"}},{"type":3,"voice_item":{"text":"spoken"}}]}]}`))
			return
		}
		<-r.Context().Done()
	})
	c := New(Config{})
	resp, err := c.GetUpdates(context.Background(), srv.URL, "tok", "c1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Cursor != "c2" || resp.LongPollingTimeoutMs != 30000 || len(resp.Msgs) != 1 {
		t.Fatalf("resp %+v", resp)
	}
	m := resp.Msgs[0]
	if m.MessageID != "18446744073709551615" {
		t.Fatalf("uint64 id lost precision: %q", m.MessageID)
	}
	if m.Items[0].Text.Text != "hi" || m.Items[1].Voice.Text != "spoken" || m.ContextToken != "ct" {
		t.Fatalf("message %+v", m)
	}
	if (*reqs)[0].body["get_updates_buf"] != "c1" {
		t.Fatal("the cursor must be sent")
	}
	resp, err = c.GetUpdates(context.Background(), srv.URL, "tok", "c2", 50*time.Millisecond)
	if err != nil || resp.Cursor != "c2" || len(resp.Msgs) != 0 {
		t.Fatalf("a long-poll timeout is an empty response with the old cursor: %+v %v", resp, err)
	}
}

func TestStaleTokenAndHTTPErrors(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ilink/bot/getupdates" {
			_, _ = w.Write([]byte(`{"ret":-14,"errmsg":"session timeout"}`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("bad gateway"))
	})
	c := New(Config{})
	_, err := c.GetUpdates(context.Background(), srv.URL, "tok", "", time.Second)
	if !IsStaleToken(err) {
		t.Fatalf("ret=-14 must be a stale token, got %v", err)
	}
	_, err = c.SendText(context.Background(), srv.URL, "tok", "u", "", "x")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusBadGateway || IsStaleToken(err) {
		t.Fatalf("HTTP error: %v", err)
	}
}

func TestIDRoundTrip(t *testing.T) {
	var v struct {
		A ID `json:"a"`
		B ID `json:"b"`
		C ID `json:"c,omitempty"`
	}
	if err := json.Unmarshal([]byte(`{"a":9007199254740993,"b":"42"}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.A != "9007199254740993" || v.B != "42" {
		t.Fatalf("%+v", v)
	}
	out, _ := json.Marshal(v)
	if string(out) != `{"a":9007199254740993,"b":42}` {
		t.Fatalf("marshal %s", out)
	}
}
