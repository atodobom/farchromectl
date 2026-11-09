package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// newTestHub sobe um hub e um servidor HTTP de teste cujo /ws?role=<ui|ext>
// faz upgrade e registra o cliente no hub com o papel pedido. A porta real do
// IPv4 local é usada pelo httptest, então são dials WebSocket reais.
func newTestHub(t *testing.T) (*Hub, *httptest.Server) {
	t.Helper()
	h := New()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		role := Role(r.URL.Query().Get("role"))
		if role != RoleExt {
			role = RoleUI
		}
		cl := NewClient(c, role)
		h.Register(cl)
		h.Run(cl) // bloqueia até a conexão fechar
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, srv
}

func dialWS(t *testing.T, srv *httptest.Server, role string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?role=" + role
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
		t.Fatalf("envio %s: %v", raw, err)
	}
}

// recvJSON lê um frame com timeout e faz unmarshal em v.
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

// drainUntil lê frames até que pred devolva true para algum deles e devolve
// esse frame.
func drainUntil(t *testing.T, conn *websocket.Conn, pred func(map[string]any) bool, what string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var m map[string]any
		recvJSON(t, conn, &m)
		if pred(m) {
			return m
		}
	}
	t.Fatalf("não recebeu %s no prazo", what)
	return nil
}

func isExtStatus(m map[string]any, connected bool) bool {
	return m["type"] == TypeExtStatus && m["connected"] == connected
}

func TestUICmdForwardedToExt(t *testing.T) {
	_, srv := newTestHub(t)
	ui := dialWS(t, srv, "ui")
	ext := dialWS(t, srv, "ext")

	// ui deve tomar conhecimento de que a extensão conectou.
	drainUntil(t, ui, func(m map[string]any) bool { return isExtStatus(m, true) }, "extStatus true")

	sendJSON(t, ui, map[string]any{"type": "cmd", "cmd": "play", "tabId": nil})
	var cmd map[string]any
	recvJSON(t, ext, &cmd)
	if cmd["type"] != TypeCmd || cmd["cmd"] != "play" {
		t.Fatalf("ext recebeu frame inesperado: %v", cmd)
	}
}

func TestExtStateBroadcastToAllUIs(t *testing.T) {
	_, srv := newTestHub(t)
	ui1 := dialWS(t, srv, "ui")
	ui2 := dialWS(t, srv, "ui")
	ext := dialWS(t, srv, "ext")

	drainUntil(t, ui1, func(m map[string]any) bool { return isExtStatus(m, true) }, "extStatus true (ui1)")
	drainUntil(t, ui2, func(m map[string]any) bool { return isExtStatus(m, true) }, "extStatus true (ui2)")

	state := map[string]any{
		"type": "state",
		"tabs": []any{map[string]any{"tabId": 7, "title": "x", "playing": true}},
	}
	sendJSON(t, ext, state)

	for name, conn := range map[string]*websocket.Conn{"ui1": ui1, "ui2": ui2} {
		var got map[string]any
		recvJSON(t, conn, &got)
		if got["type"] != TypeState {
			t.Fatalf("%s recebeu frame inesperado: %v", name, got)
		}
	}
}

func TestExtDisconnectNotifiesUIs(t *testing.T) {
	_, srv := newTestHub(t)
	ui := dialWS(t, srv, "ui")
	ext := dialWS(t, srv, "ext")

	drainUntil(t, ui, func(m map[string]any) bool { return isExtStatus(m, true) }, "extStatus true")

	_ = ext.CloseNow() // desconecta a extensão

	drainUntil(t, ui, func(m map[string]any) bool { return isExtStatus(m, false) }, "extStatus false")
}

func TestExtReconnect(t *testing.T) {
	_, srv := newTestHub(t)
	ui := dialWS(t, srv, "ui")

	ext1 := dialWS(t, srv, "ext")
	drainUntil(t, ui, func(m map[string]any) bool { return isExtStatus(m, true) }, "extStatus true (1ª)")
	_ = ext1.CloseNow()
	drainUntil(t, ui, func(m map[string]any) bool { return isExtStatus(m, false) }, "extStatus false")

	ext2 := dialWS(t, srv, "ext")
	drainUntil(t, ui, func(m map[string]any) bool { return isExtStatus(m, true) }, "extStatus true (2ª)")

	// forwarding volta a funcionar após reconexão
	sendJSON(t, ui, map[string]any{"type": "cmd", "cmd": "pause"})
	var cmd map[string]any
	recvJSON(t, ext2, &cmd)
	if cmd["cmd"] != "pause" {
		t.Fatalf("ext2 recebeu frame inesperado: %v", cmd)
	}
}

func TestExtReplacementKeepsUIsPosted(t *testing.T) {
	_, srv := newTestHub(t)
	ui := dialWS(t, srv, "ui")

	dialWS(t, srv, "ext") // primeiro ext
	drainUntil(t, ui, func(m map[string]any) bool { return isExtStatus(m, true) }, "extStatus true (1º)")

	// um segundo ext substitui o primeiro sem deixar os ui em estado "desconectado"
	dialWS(t, srv, "ext")
	sendJSON(t, ui, map[string]any{"type": "cmd", "cmd": "toggle"}) // exercita o forwarding com o novo ext
}

func TestCmdWithoutExtReturnsErrorToUI(t *testing.T) {
	_, srv := newTestHub(t)
	ui := dialWS(t, srv, "ui") // nenhum ext conectado

	sendJSON(t, ui, map[string]any{"type": "cmd", "cmd": "play"})
	// (o ui também recebe extStatus(false) no registro — drenamos até o cmdResult)
	res := drainUntil(t, ui, func(m map[string]any) bool { return m["type"] == TypeCmdResult },
		"cmdResult de erro")
	if res["ok"] != false {
		t.Fatalf("esperava cmdResult ok:false, veio %v", res)
	}
	if msg, _ := res["error"].(string); msg == "" {
		t.Fatalf("cmdResult sem mensagem de erro: %v", res)
	}
}

func TestUIFramesNonCmdNotForwarded(t *testing.T) {
	_, srv := newTestHub(t)
	ui := dialWS(t, srv, "ui")
	ext := dialWS(t, srv, "ext")

	drainUntil(t, ui, func(m map[string]any) bool { return isExtStatus(m, true) }, "extStatus true")

	// um ui tentando se passar por ext (enviar state) não deve contaminar os demais
	sendJSON(t, ui, map[string]any{"type": "state", "tabs": []any{}})

	// em seguida um cmd legítimo deve chegar ao ext — prova que o state do ui foi descartado
	sendJSON(t, ui, map[string]any{"type": "cmd", "cmd": "mute"})
	var cmd map[string]any
	recvJSON(t, ext, &cmd)
	if cmd["cmd"] != "mute" {
		t.Fatalf("ext recebeu frame inesperado: %v", cmd)
	}
}
