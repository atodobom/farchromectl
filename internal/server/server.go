// Package server expõe o HTTP do farchromectl: UI web embutida, /api/healthz
// (usado pela descoberta) e /ws (WebSocket autenticado por token).
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/antonio/farchromectl/internal/hub"
	"github.com/coder/websocket"
)

// Name é o identificador do serviço retornado pelo healthz.
const Name = "farchromectl"

const (
	authTimeout   = 5 * time.Second
	authReadLimit = 1 << 16

	// Rate-limit do handshake do token: após authMaxFailures tentativas
	// consecutivas de token errado a partir do mesmo IP de origem, o IP fica
	// em lockout por authLockoutWindow. Constantes de pacote para ajuste
	// fácil (sem flag de CLI de propósito — valor de hardening, não de uso).
	authMaxFailures   = 5
	authLockoutWindow = 60 * time.Second
	// authFailureTTL é a janela de decay da sequência de falhas e também o
	// TTL de sweep das entradas por IP: IP ocioso além desse tempo esquece a
	// sequência e é removido do mapa (o mapa não cresce sem limite).
	authFailureTTL = 10 * time.Minute
)

// authLockedMsg é a mensagem de erro distinta devolvida no hello quando o IP
// está em lockout. A UI/extensão já tratam hello ok:false com error.
const authLockedMsg = "locked: too many attempts, retry later"

// authEntry é o estado de rate-limit de um IP de origem do handshake.
type authEntry struct {
	failures    int       // tentativas consecutivas de token errado (streak)
	lastFailure time.Time // stamp da última falha (decay da streak)
	lockedUntil time.Time // recusa handshakes deste IP até este instante
	lastSeen    time.Time // última atividade (base do sweep)
}

// authLimiter contabiliza falhas de token por IP de origem e aplica lockout.
//
// Semântica escolhida (lockout estrito): durante a janela de lockout o frame
// de autenticação NÃO é lido nem comparado — o hello ok:false com
// authLockedMsg sai imediatamente. A alternativa "leniente" (comparar o token
// durante a janela para aceitar o token correto no meio dela) não desaceleraria
// nada: um atacante na LAN seguiria testando o PIN de 6 dígitos em velocidade
// máxima de rede e esgotaria o espaço em minutos, tornando o lockout
// meramente cosmético. O custo do estrito para o usuário legítimo é limitado:
// tentativas recusadas NÃO renovam a janela (locked é leitura pura), a
// extensão reconecta sozinha (backoff 15s→30s; retry imediato ao salvar as
// opções) e a janela expira em ≤60s da última falha — a primeira tentativa
// pós-janela com token correto limpa o estado do IP.
//
// Todas as mutações acontecem sob mutex: handshakes são servidos em goroutines
// concorrentes.
type authLimiter struct {
	mu  sync.Mutex
	m   map[string]*authEntry
	now func() time.Time // injetável para testes com relógio falso
}

func newAuthLimiter() *authLimiter {
	return &authLimiter{m: make(map[string]*authEntry), now: time.Now}
}

// locked informa se o IP está em lockout. Leitura pura: não renova a janela,
// para que as reconexões automáticas da extensão não perpetuem o lockout.
func (l *authLimiter) locked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.m[ip]
	return ok && l.now().Before(e.lockedUntil)
}

// recordFailure registra uma tentativa de token errado e devolve true se o IP
// entrou em lockout agora. O lockout consome a streak (failures zera), então
// após a janela o IP recupera o orçamento completo de tentativas — a taxa de
// força bruta fica limitada a ~authMaxFailures por janela. O decay da streak
// acontece pelo sweep: IP ocioso além de authFailureTTL é removido e recomeça
// do zero.
func (l *authLimiter) recordFailure(ip string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)

	e, ok := l.m[ip]
	if !ok {
		e = &authEntry{}
		l.m[ip] = e
	}
	e.failures++
	e.lastFailure = now
	e.lastSeen = now
	if e.failures >= authMaxFailures {
		e.lockedUntil = now.Add(authLockoutWindow)
		e.failures = 0
		return true
	}
	return false
}

// recordSuccess remove o estado do IP: sucesso prova conhecimento do token e
// zera qualquer streak pendente.
func (l *authLimiter) recordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, ip)
}

// sweepLocked remove entradas ociosas (lastSeen além do TTL) para o mapa não
// crescer sem limite. Deve ser chamada sob lock. Entradas só são criadas em
// falha, então o limite é "IPs que falharam recentemente", não conexões.
func (l *authLimiter) sweepLocked(now time.Time) {
	for ip, e := range l.m {
		if now.Sub(e.lastSeen) > authFailureTTL {
			delete(l.m, ip)
		}
	}
}

// Server concentra os handlers HTTP.
type Server struct {
	hub     *hub.Hub
	token   string
	version string
	static  http.Handler
	limiter *authLimiter
}

// New cria o servidor. version é a versão do binário (reportada no healthz);
// staticFS é o FS embutido da UI (raiz = index.html etc.).
func New(h *hub.Hub, token, version string, staticFS fs.FS) *Server {
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	return &Server{
		hub:     h,
		token:   token,
		version: version,
		static:  http.FileServerFS(staticFS),
		limiter: newAuthLimiter(),
	}
}

// Handler devolve o roteador completo.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/healthz", s.handleHealthz)
	mux.HandleFunc("/ws", s.handleWS)
	mux.Handle("/", s.static)
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true,"name":"` + Name + `","version":"` + s.version + `"}`))
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// A UI é servida por este mesmo servidor (mesma origem); a extensão vem de
	// chrome-extension:// — token é a única autenticação real, então aceitamos
	// qualquer origem explicitamente.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()

	role, ok := s.authenticate(conn, remoteIP(r.RemoteAddr))
	if !ok {
		return
	}

	client := hub.NewClient(conn, role)
	s.hub.Register(client)
	s.hub.Run(client) // bloqueia até a conexão fechar
}

// remoteIP extrai o IP de origem (sem porta) de r.RemoteAddr. Se o endereço
// não vier em formato host:port, devolve como está — chave de rate-limit só
// precisa ser consistente, não bonita.
func remoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// authenticate lê o primeiro frame, valida {role, token} e responde hello.
// Retorna o papel validado e ok=true quando o cliente deve seguir registrado
// no hub. remoteIP é a chave do rate-limit (lockout por IP de origem).
//
// Só a falha de token (comparação ConstantTimeCompare != 1) conta para o
// lockout: é o único caminho que revela informação sobre o token e, portanto,
// o único que vale desacelerar. Papel inválido, token vazio ou JSON quebrado
// não contabilizam.
func (s *Server) authenticate(conn *websocket.Conn, remoteIP string) (hub.Role, bool) {
	sendHello := func(ok bool, errMsg string) {
		raw, _ := json.Marshal(struct {
			Type  string `json:"type"`
			OK    bool   `json:"ok"`
			Error string `json:"error,omitempty"`
		}{Type: "hello", OK: ok, Error: errMsg})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = conn.Write(ctx, websocket.MessageText, raw)
	}

	fail := func(errMsg, reason string) (hub.Role, bool) {
		sendHello(false, errMsg)
		_ = conn.Close(websocket.StatusPolicyViolation, reason)
		return "", false
	}

	// Lockout estrito (ver justificativa em authLimiter): durante a janela o
	// frame não é lido nem comparado; o hello ok:false sai imediato e a
	// conexão fecha. Mesma forma de {type:"hello", ok:false, error:"..."} que
	// UI e extensão já tratam, com mensagem distinta para lockout.
	if s.limiter.locked(remoteIP) {
		return fail(authLockedMsg, "lockout de autenticação")
	}

	ctx, cancel := context.WithTimeout(context.Background(), authTimeout)
	defer cancel()
	conn.SetReadLimit(authReadLimit)
	msgType, raw, err := conn.Read(ctx)
	if err != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "autenticação ausente")
		return "", false
	}
	if msgType != websocket.MessageText {
		return fail("frame inicial deve ser texto JSON", "formato inválido")
	}

	var auth struct {
		Role  string `json:"role"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &auth); err != nil {
		return fail("JSON inválido", "formato inválido")
	}

	role := hub.Role(auth.Role)
	if role != hub.RoleUI && role != hub.RoleExt {
		return fail("papel inválido", "papel inválido")
	}
	if auth.Token == "" {
		return fail("token inválido", "token inválido")
	}
	// comparação em tempo constante (higiene; o PIN tem só 6 dígitos, mas
	// não custa evitar oráculos de timing no handshake).
	if subtle.ConstantTimeCompare([]byte(auth.Token), []byte(s.token)) != 1 {
		s.limiter.recordFailure(remoteIP)
		return fail("token inválido", "token inválido")
	}

	// token corre prova conhecimento: limpa o estado do IP (streak zera).
	s.limiter.recordSuccess(remoteIP)
	sendHello(true, "")
	return role, true
}
