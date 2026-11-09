// Package hub implementa o relay WebSocket entre clientes "ui" (navegadores
// do celular/desktop) e o cliente "ext" (extensão do Chrome).
//
// Regras:
//   - qualquer número de clientes "ui";
//   - no máximo um cliente "ext": uma nova conexão ext substitui a anterior;
//   - frames {"type":"cmd"} vindos de ui são reenviados ao ext (raw);
//   - frames {"type":"state"} e {"type":"cmdResult"} vindos do ext são
//     broadcast para todos os ui;
//   - ao conectar/desconectar o ext, todos os ui recebem
//     {"type":"extStatus","connected":bool};
//   - cmd sem ext conectado devolve ao ui um cmdResult de erro.
//
// O hub não conhece o token: autenticação é responsabilidade do pacote server.
package hub

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Role de um cliente autenticado.
type Role string

const (
	RoleUI  Role = "ui"
	RoleExt Role = "ext"
)

// Tipos de frame roteados pelo hub.
const (
	TypeCmd       = "cmd"
	TypeState     = "state"
	TypeCmdResult = "cmdResult"
	TypeExtStatus = "extStatus"
)

const (
	sendBuffer    = 64
	pingInterval  = 25 * time.Second
	pingTimeout   = 5 * time.Second
	writeTimeout  = 10 * time.Second
	readLimit     = 1 << 20
	slowClientMsg = "hub: descartando mensagem para cliente lento (buffer cheio)"
)

// Client é uma conexão WebSocket autenticada registrada no hub.
// Crie com NewClient; não use o zero value.
type Client struct {
	conn *websocket.Conn
	role Role

	send      chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

// NewClient envolve uma conexão autenticada com o papel dado.
func NewClient(conn *websocket.Conn, role Role) *Client {
	return &Client{
		conn: conn,
		role: role,
		send: make(chan []byte, sendBuffer),
		done: make(chan struct{}),
	}
}

// Role devolve o papel do cliente.
func (c *Client) Role() Role { return c.role }

// Close encerra a conexão de forma idempotente.
func (c *Client) Close() { c.close() }

func (c *Client) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		// CloseNow: encerra o TCP imediatamente; o par detecta e cai.
		_ = c.conn.CloseNow()
	})
}

// enqueue tenta entregar msg ao write pump. Não bloqueia: se o buffer estiver
// cheio ou o cliente já tiver sido fechado, a mensagem é descartada.
func (c *Client) enqueue(msg []byte) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case <-c.done:
		return false
	case c.send <- msg:
		return true
	default:
		log.Println(slowClientMsg)
		return false
	}
}

// Hub concentra o estado do relay.
type Hub struct {
	mu  sync.Mutex
	uis map[*Client]struct{}
	ext *Client
}

// New cria um hub vazio.
func New() *Hub {
	return &Hub{uis: make(map[*Client]struct{})}
}

// Register registra o cliente, inicia o write pump e notifica os demais
// conforme o papel. Deve ser chamada uma única vez por cliente.
func (h *Hub) Register(c *Client) {
	if c.role == RoleExt {
		h.mu.Lock()
		old := h.ext
		h.ext = c
		h.mu.Unlock()
		if old != nil && old != c {
			old.close()
		}
		go h.writePump(c)
		h.broadcastToUIs(extStatusFrame(true))
		return
	}

	h.mu.Lock()
	h.uis[c] = struct{}{}
	extConnected := h.ext != nil
	h.mu.Unlock()
	go h.writePump(c)
	// Late joiner: informa o status atual da extensão.
	c.enqueue(extStatusFrame(extConnected))
}

// Unregister remove o cliente e notifica conforme o papel. Idempotente.
func (h *Hub) Unregister(c *Client) {
	if c.role == RoleExt {
		h.mu.Lock()
		if h.ext == c {
			h.ext = nil
			h.mu.Unlock()
			c.close()
			h.broadcastToUIs(extStatusFrame(false))
			return
		}
		h.mu.Unlock()
		c.close()
		return
	}

	h.mu.Lock()
	delete(h.uis, c)
	h.mu.Unlock()
	c.close()
}

// Run executa o read loop do cliente até a conexão fechar. Bloqueante —
// chame na goroutine do handler. Ao retornar, o cliente já foi desregistrado.
func (h *Hub) Run(c *Client) {
	defer h.Unregister(c)
	c.conn.SetReadLimit(readLimit)
	for {
		typ, data, err := c.conn.Read(context.Background())
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		h.route(c, data)
	}
}

// route decide o destino de um frame.
func (h *Hub) route(c *Client, raw []byte) {
	var env struct {
		Type string `json:"type"`
		Cmd  string `json:"cmd"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return // frame não-JSON: ignora
	}

	switch c.role {
	case RoleUI:
		if env.Type != TypeCmd {
			return
		}
		h.mu.Lock()
		ext := h.ext
		h.mu.Unlock()
		if ext == nil {
			c.enqueue(cmdResultFrame(false, "extensão desconectada", env.Cmd))
			return
		}
		// repassa o frame cru, preservando campos extras (value, tabId...).
		ext.enqueue(raw)
	case RoleExt:
		switch env.Type {
		case TypeState, TypeCmdResult:
			h.broadcastToUIs(raw)
		}
	}
}

// broadcastToUIs entrega msg a todos os clientes ui atuais.
func (h *Hub) broadcastToUIs(msg []byte) {
	h.mu.Lock()
	targets := make([]*Client, 0, len(h.uis))
	for c := range h.uis {
		targets = append(targets, c)
	}
	h.mu.Unlock()
	for _, c := range targets {
		c.enqueue(msg)
	}
}

// writePump serializa as escritas e mantém a conexão viva com pings.
func (h *Hub) writePump(c *Client) {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-c.done:
			return
		case msg := <-c.send:
			ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			err := c.conn.Write(ctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				h.Unregister(c)
				return
			}
		case <-ping.C:
			ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
			err := c.conn.Ping(ctx)
			cancel()
			if err != nil {
				h.Unregister(c)
				return
			}
		}
	}
}

func extStatusFrame(connected bool) []byte {
	b, _ := json.Marshal(struct {
		Type      string `json:"type"`
		Connected bool   `json:"connected"`
	}{Type: TypeExtStatus, Connected: connected})
	return b
}

func cmdResultFrame(ok bool, errMsg, cmd string) []byte {
	b, _ := json.Marshal(struct {
		Type  string `json:"type"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
		Cmd   string `json:"cmd,omitempty"`
	}{Type: TypeCmdResult, OK: ok, Error: errMsg, Cmd: cmd})
	return b
}
