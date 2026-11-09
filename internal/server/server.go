// Package server expõe o HTTP do farchromectl: UI web embutida, /api/healthz
// (usado pela descoberta) e /ws (WebSocket autenticado por token).
package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"mime"
	"net/http"
	"time"

	"github.com/antonio/farchromectl/internal/hub"
	"github.com/coder/websocket"
)

// Name é o identificador do serviço retornado pelo healthz.
const Name = "farchromectl"

const (
	authTimeout   = 5 * time.Second
	authReadLimit = 1 << 16
)

// Server concentra os handlers HTTP.
type Server struct {
	hub    *hub.Hub
	token  string
	static http.Handler
}

// New cria o servidor. staticFS é o FS embutido da UI (raiz = index.html etc.).
func New(h *hub.Hub, token string, staticFS fs.FS) *Server {
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	return &Server{
		hub:    h,
		token:  token,
		static: http.FileServerFS(staticFS),
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
	_, _ = w.Write([]byte(`{"ok":true,"name":"` + Name + `"}`))
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

	role, ok := s.authenticate(conn)
	if !ok {
		return
	}

	client := hub.NewClient(conn, role)
	s.hub.Register(client)
	s.hub.Run(client) // bloqueia até a conexão fechar
}

// authenticate lê o primeiro frame, valida {role, token} e responde hello.
// Retorna o papel validado e ok=true quando o cliente deve seguir registrado
// no hub.
func (s *Server) authenticate(conn *websocket.Conn) (hub.Role, bool) {
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
	if auth.Token == "" || auth.Token != s.token {
		return fail("token inválido", "token inválido")
	}

	sendHello(true, "")
	return role, true
}
