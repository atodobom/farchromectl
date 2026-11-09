package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// dialFromIP conecta ao /ws vinculando a origem a localIP. Aliases de
// loopback (127.0.0.x no Linux) permitem simular IPs de origem distintos nos
// testes de isolamento per-IP.
func dialFromIP(t *testing.T, baseURL, localIP string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(baseURL, "http") + "/ws"
	dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(localIP)}}
	client := &http.Client{Transport: &http.Transport{DialContext: dialer.DialContext}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatalf("dial %s a partir de %s: %v", url, localIP, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// expectAuthOK faz o handshake completo com token correto (de localIP, ou da
// origem padrão se vazia) e exige hello ok:true.
func expectAuthOK(t *testing.T, baseURL, localIP string) {
	t.Helper()
	var conn *websocket.Conn
	if localIP == "" {
		conn = dial(t, baseURL)
	} else {
		conn = dialFromIP(t, baseURL, localIP)
	}
	sendJSON(t, conn, map[string]any{"role": "ui", "token": testToken})
	var hello struct {
		Type  string `json:"type"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	recvJSON(t, conn, &hello)
	if hello.Type != "hello" || !hello.OK {
		t.Fatalf("esperava hello ok:true, veio %+v", hello)
	}
}

var wrongAuth = map[string]any{"role": "ui", "token": "999999"}

// failNTimesFrom dispara n handshakes com token errado a partir de localIP
// (origem padrão se vazia), cada um rejeitado normalmente ("token inválido").
func failNTimesFrom(t *testing.T, baseURL, localIP string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if localIP == "" {
			expectRejected(t, baseURL, wrongAuth)
		} else {
			conn := dialFromIP(t, baseURL, localIP)
			sendJSON(t, conn, wrongAuth)
			var hello struct {
				Type  string `json:"type"`
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			}
			recvJSON(t, conn, &hello)
			if hello.Type != "hello" || hello.OK {
				t.Fatalf("esperava rejeição normal (tentativa %d), veio %+v", i+1, hello)
			}
		}
	}
}

// TestWSLockoutAfterRepeatedFailures: authMaxFailures tokens errados
// consecutivos do mesmo IP colocam o IP em lockout — a tentativa seguinte é
// recusada com a mensagem distinta de lockout, sem nem ler o frame de
// autenticação, e a conexão é fechada.
func TestWSLockoutAfterRepeatedFailures(t *testing.T) {
	ts := newTestServer(t)

	// ainda abaixo do limiar: rejeições normais
	failNTimesFrom(t, ts.URL, "", authMaxFailures-1)
	// a authMaxFailures-ésima falha dispara o lockout (ainda responde
	// "token inválido" — é a falha que arma a janela)
	expectRejected(t, ts.URL, wrongAuth)

	// tentativa seguinte: recusada imediatamente, sem frame de autenticação
	conn := dial(t, ts.URL)
	var hello struct {
		Type  string `json:"type"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	recvJSON(t, conn, &hello)
	if hello.Type != "hello" || hello.OK {
		t.Fatalf("esperava hello ok:false em lockout, veio %+v", hello)
	}
	if !strings.Contains(hello.Error, "locked") {
		t.Fatalf("mensagem de lockout deveria ser distinta, veio %q", hello.Error)
	}
	// e o servidor fecha a conexão em seguida
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("conexão deveria ter sido fechada após rejeição por lockout")
	}
}

// TestWSLockoutIsPerIP: lockout de um IP não afeta handshakes legítimos de
// outro IP — o rate-limit é contabilizado por origem.
func TestWSLockoutIsPerIP(t *testing.T) {
	ts := newTestServer(t)

	// trava o IP padrão (127.0.0.1)
	failNTimesFrom(t, ts.URL, "", authMaxFailures)

	// durante a janela de lockout de 127.0.0.1, outro IP autentica normalmente
	expectAuthOK(t, ts.URL, "127.0.0.2")
}

// TestWSLockoutCounterResetsOnSuccess: um handshake bem-sucedido zera a
// contagem do IP. Após 4 falhas + 1 sucesso + 4 falhas (8 falhas totais, mas
// nenhuma sequência de authMaxFailures), um token correto ainda é aceito —
// sem o reset, a 5ª falha acumulada teria travado o IP.
func TestWSLockoutCounterResetsOnSuccess(t *testing.T) {
	ts := newTestServer(t)

	failNTimesFrom(t, ts.URL, "", authMaxFailures-1)
	expectAuthOK(t, ts.URL, "") // sucesso limpa o estado do IP
	failNTimesFrom(t, ts.URL, "", authMaxFailures-1)
	expectAuthOK(t, ts.URL, "") // prova que não houve lockout
}

// ---------- unit tests do authLimiter (relógio falso) ----------

// newFrozenLimiter devolve um limiter cujo relógio é a variável cur (mutável
// pelo teste para avançar o tempo).
func newFrozenLimiter(start time.Time) (*authLimiter, *time.Time) {
	cur := start
	l := newAuthLimiter()
	l.now = func() time.Time { return cur }
	return l, &cur
}

// TestAuthLimiterSweepRemovesIdleEntries: entradas ociosas além do TTL são
// varridas na falha seguinte — o mapa não cresce sem limite.
func TestAuthLimiterSweepRemovesIdleEntries(t *testing.T) {
	start := time.Now()
	l, cur := newFrozenLimiter(start)

	for i := 1; i <= 3; i++ {
		l.recordFailure(fmt.Sprintf("10.0.0.%d", i))
	}
	if len(l.m) != 3 {
		t.Fatalf("mapa = %d entradas, queria 3", len(l.m))
	}

	// avança além do TTL; uma nova falha (de outro IP) dispara o sweep
	*cur = start.Add(authFailureTTL + time.Minute)
	l.recordFailure("__VG_IPV4_f1d91407a68e__")
	for _, ip := range []string{"__VG_IPV4_11b2e3a0f955__", "__VG_IPV4_1282f0dc3895__", "10.0.0.1"} {
		if _, ok := l.m[ip]; ok {
			t.Fatalf("entrada ociosa %s deveria ter sido varrida", ip)
		}
	}
	if len(l.m) != 1 {
		t.Fatalf("mapa = %d entradas após sweep, queria só a ativa", len(l.m))
	}
}

// TestAuthLimiterStreakDecaysAfterTTL: um IP ocioso além do TTL esquece a
// streak (a entrada é varrida e recomeça do zero) — falhas antigas não
// acumulam para travar o IP depois.
func TestAuthLimiterStreakDecaysAfterTTL(t *testing.T) {
	start := time.Now()
	l, cur := newFrozenLimiter(start)
	const ip = "10.2.0.1"

	for i := 0; i < authMaxFailures-1; i++ {
		l.recordFailure(ip)
	}
	// silêncio além do TTL, então 1 falha: a entrada antiga foi varrida e a
	// streak recomeçou em 1 — não trava.
	*cur = start.Add(authFailureTTL + time.Minute)
	if l.recordFailure(ip) {
		t.Fatal("streak antiga deveria ter decaído (sweep); não era para travar")
	}
	// agora é preciso o orçamento completo de falhas consecutivas para travar
	for i := 0; i < authMaxFailures-2; i++ {
		if l.recordFailure(ip) {
			t.Fatalf("travou antes do limiar (falha consecutiva %d)", i+2)
		}
	}
	if !l.recordFailure(ip) {
		t.Fatal("alcançado o limiar de falhas consecutivas, deveria travar")
	}
	if !l.locked(ip) {
		t.Fatal("IP deveria estar em lockout")
	}
}

// TestAuthLimiterWindowExpiresAndIsNotExtended: a janela de lockout expira
// sozinha e consultas (tentativas recusadas) durante a janela não a renovam —
// as reconexões automáticas da extensão não podem perpetuar o lockout.
func TestAuthLimiterWindowExpiresAndIsNotExtended(t *testing.T) {
	start := time.Now()
	l, cur := newFrozenLimiter(start)
	const ip = "10.3.0.1"

	for i := 0; i < authMaxFailures; i++ {
		l.recordFailure(ip)
	}
	if !l.locked(ip) {
		t.Fatal("IP deveria estar em lockout")
	}

	// rajada de tentativas recusadas no meio da janela
	for i := 0; i < 20; i++ {
		l.locked(ip)
	}
	if got := l.m[ip].lockedUntil; !got.Equal(start.Add(authLockoutWindow)) {
		t.Fatalf("janela foi renovada por consultas: lockedUntil = %v, queria %v", got, start.Add(authLockoutWindow))
	}

	*cur = start.Add(authLockoutWindow + time.Second)
	if l.locked(ip) {
		t.Fatal("janela deveria ter expirado")
	}
}

// TestAuthLimiterSuccessClearsEntry: recordSuccess remove o estado do IP.
func TestAuthLimiterSuccessClearsEntry(t *testing.T) {
	l, _ := newFrozenLimiter(time.Now())
	const ip = "10.4.0.1"

	l.recordFailure(ip)
	if len(l.m) != 1 {
		t.Fatal("entrada deveria existir após falha")
	}
	l.recordSuccess(ip)
	if len(l.m) != 0 {
		t.Fatal("sucesso deveria remover a entrada do IP")
	}
}

// TestAuthLimiterConcurrentAccess: exercita o limiter de várias goroutines
// (IPs distintos e um IP compartilhado) para o race detector validar o lock.
func TestAuthLimiterConcurrentAccess(t *testing.T) {
	l := newAuthLimiter() // relógio real

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ip := fmt.Sprintf("10.5.%d.1", g+1)
			for i := 0; i < 50; i++ {
				l.recordFailure(ip)
				l.locked(ip)
				if i%10 == 0 {
					l.recordSuccess(ip)
				}
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			l.recordFailure("__VG_IPV4_731af1cf040e__")
			l.locked("__VG_IPV4_731af1cf040e__")
		}
	}()
	wg.Wait()

	if got := len(l.m); got > 9 {
		t.Fatalf("mapa = %d entradas, queria ≤ 9 (um por IP usado)", got)
	}
}

// TestRemoteIP: extração de IP a partir de RemoteAddr (IPv4, IPv6 e formato
// sem porta).
func TestRemoteIP(t *testing.T) {
	cases := []struct{ in, want string }{
		{"__VG_IPV4_0dd7f56680fd__:54321", "__VG_IPV4_0dd7f56680fd__"},
		{"[::1]:54321", "::1"},
		{"sem-porta", "sem-porta"},
	}
	for _, c := range cases {
		if got := remoteIP(c.in); got != c.want {
			t.Errorf("remoteIP(%q) = %q, queria %q", c.in, got, c.want)
		}
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
