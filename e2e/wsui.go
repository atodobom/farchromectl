// Cliente WebSocket com papel "ui": autentica no /ws do farchromectl e
// expõe os frames do hub (extStatus, state, cmdResult) em canais.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coder/websocket"
)

// tabState é o estado de uma aba reportado pela extensão.
type tabState struct {
	TabID        int      `json:"tabId"`
	Title        string   `json:"title"`
	Playing      bool     `json:"playing"`
	Position     float64  `json:"position"`
	Duration     *float64 `json:"duration"`
	Volume       float64  `json:"volume"`
	Muted        bool     `json:"muted"`
	PlaybackRate float64  `json:"playbackRate"`
}

// stateFrame é um frame {"type":"state"} do hub.
type stateFrame struct {
	Tabs        []tabState `json:"tabs"`
	ActiveTabID int        `json:"activeTabId"`
}

// cmdResultFrame é um frame {"type":"cmdResult"} do hub.
type cmdResultFrame struct {
	OK    bool   `json:"ok"`
	Cmd   string `json:"cmd"`
	Error string `json:"error"`
	TabID int    `json:"tabId"`
}

// extStatusFrame é um frame {"type":"extStatus"} do hub.
type extStatusFrame struct {
	Connected bool `json:"connected"`
}

// uiClient é a conexão "ui" autenticada.
type uiClient struct {
	conn       *websocket.Conn
	extStatus  chan extStatusFrame
	states     chan stateFrame
	cmdResults chan cmdResultFrame
	verbose    bool
}

// dialUI conecta ao /ws, autentica {role:"ui",token} e valida o hello.
// opts permite injetar o cliente HTTPS verificado no modo -tls (wss).
func dialUI(ctx context.Context, url, token string, verbose bool, opts *websocket.DialOptions) (*uiClient, error) {
	conn, _, err := websocket.Dial(ctx, url, opts)
	if err != nil {
		return nil, err
	}
	auth, _ := json.Marshal(map[string]string{"role": "ui", "token": token})
	if err := conn.Write(ctx, websocket.MessageText, auth); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	// hello
	_, data, err := conn.Read(ctx)
	if err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	var hello struct {
		Type  string `json:"type"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &hello); err != nil || hello.Type != "hello" {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("hello inesperado: %s", string(data))
	}
	if !hello.OK {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("autenticação ui rejeitada: %s", hello.Error)
	}
	ui := &uiClient{
		conn:       conn,
		extStatus:  make(chan extStatusFrame, 16),
		states:     make(chan stateFrame, 128),
		cmdResults: make(chan cmdResultFrame, 32),
		verbose:    verbose,
	}
	go ui.readLoop(ctx)
	return ui, nil
}

// close encerra a conexão.
func (ui *uiClient) close() { _ = ui.conn.CloseNow() }

// readLoop lê frames até a conexão fechar e os distribui nos canais.
func (ui *uiClient) readLoop(ctx context.Context) {
	for {
		typ, data, err := ui.conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var env struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		switch env.Type {
		case "extStatus":
			var f extStatusFrame
			if json.Unmarshal(data, &f) == nil {
				select {
				case ui.extStatus <- f:
				default: // descarta: só o último status importa
				}
			}
		case "state":
			var f stateFrame
			if json.Unmarshal(data, &f) == nil {
				if ui.verbose {
					logf("frame state: %s", compactJSON(data))
				}
				select {
				case ui.states <- f:
				default:
					<-ui.states // descarta o mais antigo
					ui.states <- f
				}
			}
		case "cmdResult":
			var f cmdResultFrame
			if json.Unmarshal(data, &f) == nil {
				if ui.verbose {
					logf("frame cmdResult: %s", compactJSON(data))
				}
				select {
				case ui.cmdResults <- f:
				default:
					<-ui.cmdResults
					ui.cmdResults <- f
				}
			}
		}
	}
}

// sendCmd envia um comando {type:"cmd",cmd,value?} à extensão.
func (ui *uiClient) sendCmd(ctx context.Context, cmd string, value *float64) error {
	frame := map[string]any{"type": "cmd", "cmd": cmd}
	if value != nil {
		frame["value"] = *value
	}
	raw, _ := json.Marshal(frame)
	return ui.conn.Write(ctx, websocket.MessageText, raw)
}

// waitExtStatus espera um extStatus com o valor pedido.
func (ui *uiClient) waitExtStatus(ctx context.Context, connected bool, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout esperando extStatus connected=%v", connected)
		case f := <-ui.extStatus:
			if f.Connected == connected {
				return nil
			}
		}
	}
}

// waitState espera um state que satisfaça pred.
func (ui *uiClient) waitState(ctx context.Context, pred func(stateFrame) bool, timeout time.Duration, what string) (stateFrame, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return stateFrame{}, fmt.Errorf("timeout esperando state: %s", what)
		case f := <-ui.states:
			if pred(f) {
				return f, nil
			}
		}
	}
}

// waitCmdResult espera o cmdResult do comando dado com ok esperado.
func (ui *uiClient) waitCmdResult(ctx context.Context, cmd string, wantOK bool, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout esperando cmdResult do comando %q", cmd)
		case f := <-ui.cmdResults:
			if f.Cmd == cmd {
				if f.OK != wantOK {
					return fmt.Errorf("cmdResult de %q: ok=%v (esperado %v) erro=%q", cmd, f.OK, wantOK, f.Error)
				}
				return nil
			}
		}
	}
}

// compactJSON re-serializa JSON de forma compacta (para log).
func compactJSON(data []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		return string(data)
	}
	return buf.String()
}
