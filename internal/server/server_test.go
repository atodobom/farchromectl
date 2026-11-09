package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/antonio/farchromectl/internal/hub"
	"github.com/coder/websocket"
)

const (
	testToken   = "123456"
	testVersion = "9.9.9-test"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	h := hub.New()
	static := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<!doctype html><title>ui</title>")},
	}
	srv := New(h, testToken, testVersion, static)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func dial(t *testing.T, baseURL string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(baseURL, "http") + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func sendJSON(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("envio: %v", err)
	}
}

func recvJSON(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("recepção: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
}

// expectRejected faz o handshake, envia o frame de autenticação dado e espera
// hello ok:false seguido de fechamento da conexão.
func expectRejected(t *testing.T, baseURL string, authFrame any) {
	t.Helper()
	conn := dial(t, baseURL)
	sendJSON(t, conn, authFrame)

	var hello struct {
		Type  string `json:"type"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	recvJSON(t, conn, &hello)
	if hello.Type != "hello" || hello.OK {
		t.Fatalf("esperava hello ok:false, veio %+v", hello)
	}
	if hello.Error == "" {
		t.Fatalf("hello de rejeição sem mensagem de erro: %+v", hello)
	}

	// após a rejeição o servidor deve fechar a conexão.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("conexão deveria ter sido fechada após rejeição")
	}
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t)

	resp, err := ts.Client().Get(ts.URL + "/api/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, queria 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("CORS = %q, queria \"*\"", got)
	}
	body, _ := io.ReadAll(resp.Body)
	var got struct {
		OK      bool   `json:"ok"`
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body %s: %v", body, err)
	}
	if !got.OK || got.Name != Name {
		t.Fatalf("healthz = %+v", got)
	}
	if got.Version != testVersion {
		t.Fatalf("healthz version = %q, queria %q", got.Version, testVersion)
	}
}

func TestStaticUIServed(t *testing.T) {
	ts := newTestServer(t)
	resp, err := ts.Client().Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<title>ui</title>") {
		t.Fatalf("UI não foi servida: %s", body)
	}
}

func TestWSRejectsWrongToken(t *testing.T) {
	ts := newTestServer(t)
	expectRejected(t, ts.URL, map[string]any{"role": "ui", "token": "999999"})
}

func TestWSRejectsMissingToken(t *testing.T) {
	ts := newTestServer(t)
	expectRejected(t, ts.URL, map[string]any{"role": "ui"})
}

func TestWSRejectsInvalidRole(t *testing.T) {
	ts := newTestServer(t)
	expectRejected(t, ts.URL, map[string]any{"role": "admin", "token": testToken})
}

func TestWSRejectsNoAuthFrame(t *testing.T) {
	ts := newTestServer(t)
	conn := dial(t, ts.URL)
	// não envia nada: o servidor deve fechar por timeout de autenticação
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("conexão deveria ser fechada sem frame de autenticação")
	}
}

func TestWSEndToEnd(t *testing.T) {
	ts := newTestServer(t)

	// extensão conecta primeiro
	ext := dial(t, ts.URL)
	sendJSON(t, ext, map[string]any{"role": "ext", "token": testToken})
	var extHello struct {
		Type string `json:"type"`
		OK   bool   `json:"ok"`
	}
	recvJSON(t, ext, &extHello)
	if !extHello.OK {
		t.Fatalf("ext não autenticou: %+v", extHello)
	}

	// ui conecta e deve receber hello ok + extStatus true
	ui := dial(t, ts.URL)
	sendJSON(t, ui, map[string]any{"role": "ui", "token": testToken})
	var uiHello struct {
		Type string `json:"type"`
		OK   bool   `json:"ok"`
	}
	recvJSON(t, ui, &uiHello)
	if !uiHello.OK {
		t.Fatalf("ui não autenticou: %+v", uiHello)
	}
	var extStatus struct {
		Type      string `json:"type"`
		Connected bool   `json:"connected"`
	}
	recvJSON(t, ui, &extStatus)
	if extStatus.Type != "extStatus" || !extStatus.Connected {
		t.Fatalf("ui esperava extStatus true, veio %+v", extStatus)
	}

	// ui -> ext: comando repassado intacto
	cmd := map[string]any{"type": "cmd", "cmd": "seekTo", "value": 42.5, "tabId": 7}
	sendJSON(t, ui, cmd)
	var gotCmd map[string]any
	recvJSON(t, ext, &gotCmd)
	if gotCmd["cmd"] != "seekTo" || gotCmd["value"] != 42.5 || gotCmd["tabId"] != float64(7) {
		t.Fatalf("cmd não chegou intacto ao ext: %v", gotCmd)
	}

	// ext -> ui: estado broadcast
	state := map[string]any{
		"type":        "state",
		"tabs":        []any{map[string]any{"tabId": 7, "title": "música", "playing": true, "position": 10, "duration": 200}},
		"activeTabId": 7,
	}
	sendJSON(t, ext, state)
	var gotState map[string]any
	recvJSON(t, ui, &gotState)
	if gotState["type"] != "state" {
		t.Fatalf("state não chegou ao ui: %v", gotState)
	}

	// ext desconecta: ui é notificado
	_ = ext.CloseNow()
	var disconnected struct {
		Type      string `json:"type"`
		Connected bool   `json:"connected"`
	}
	recvJSON(t, ui, &disconnected)
	if disconnected.Type != "extStatus" || disconnected.Connected {
		t.Fatalf("ui esperava extStatus false, veio %+v", disconnected)
	}
}

// TestWSRealPortDial garante que o dial funciona contra um listener TCP real
// (não apenas httptest.Server via client), exercitando o caminho de rede usado
// pela extensão e pelo celular.
func TestWSRealPortDial(t *testing.T) {
	h := hub.New()
	static := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("x")}}
	srv := &http.Server{Handler: New(h, testToken, testVersion, static).Handler()}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	baseURL := "http://" + ln.Addr().String()
	ui := dial(t, baseURL)
	sendJSON(t, ui, map[string]any{"role": "ui", "token": testToken})
	var hello struct {
		OK bool `json:"ok"`
	}
	recvJSON(t, ui, &hello)
	if !hello.OK {
		t.Fatal("autenticação falhou contra listener real")
	}
}
