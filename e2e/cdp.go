// Cliente CDP (Chrome DevTools Protocol) mínimo para o harness E2E: conexão
// ao WebSocket do browser (Extensions.loadUnpacked, Target.createTarget) e
// às páginas (Runtime.evaluate, Input.dispatchMouseEvent, Page.navigate).
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// cdpTarget é um alvo listado por /json/list.
type cdpTarget struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title"`
	WS    string `json:"webSocketDebuggerUrl"`
}

// cdpHTTP faz uma requisição HTTP ao endpoint de debug do Chrome.
func cdpHTTP(ctx context.Context, base, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// waitForCDPVersion espera o endpoint de debug do Chrome responder.
func waitForCDPVersion(ctx context.Context, base string) (string, error) {
	for {
		body, err := cdpHTTP(ctx, base, http.MethodGet, "/json/version")
		if err == nil {
			var v struct {
				Browser              string `json:"Browser"`
				WebsocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}
			if err := json.Unmarshal(body, &v); err != nil {
				return "", err
			}
			return v.WebsocketDebuggerURL, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("Chrome não respondeu em http://%s/json/version: %w", base, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// listTargets devolve os alvos correntes do Chrome.
func listTargets(ctx context.Context, base string) ([]cdpTarget, error) {
	body, err := cdpHTTP(ctx, base, http.MethodGet, "/json/list")
	if err != nil {
		return nil, err
	}
	var ts []cdpTarget
	if err := json.Unmarshal(body, &ts); err != nil {
		return nil, err
	}
	return ts, nil
}

// cdpConn é uma conexão CDP (browser ou página). Chamas serializadas por mu.
type cdpConn struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	nextID int
}

// dialCDP conecta a um webSocketDebuggerUrl.
func dialCDP(ctx context.Context, wsURL string) (*cdpConn, error) {
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, err
	}
	// respostas CDP podem ser grandes (ex.: Page.captureScreenshot em base64);
	// o limite default de leitura do coder/websocket (32KiB) as descartaria.
	conn.SetReadLimit(64 << 20)
	return &cdpConn{conn: conn}, nil
}

// close encerra a conexão.
func (c *cdpConn) close() {
	_ = c.conn.CloseNow()
}

// call envia um comando CDP e espera a resposta com o id correspondente,
// descartando eventos intermediários.
func (c *cdpConn) call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := c.nextID

	raw, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if err := c.conn.Write(ctx, websocket.MessageText, raw); err != nil {
		return nil, err
	}
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &msg); err != nil || msg.ID == nil {
			continue // evento assíncrono: ignora
		}
		if *msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("CDP %s: %s (%d)", method, msg.Error.Message, msg.Error.Code)
		}
		return msg.Result, nil
	}
}

// eval avalia uma expressão JavaScript e devolve o valor (returnByValue).
// Retorna erro quando a expressão lança exceção.
func (c *cdpConn) eval(ctx context.Context, expr string) (any, error) {
	res, err := c.call(ctx, "Runtime.evaluate", map[string]any{
		"expression":    expr,
		"returnByValue": true,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Result struct {
			Type  string `json:"type"`
			Value any    `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}
	if out.ExceptionDetails != nil {
		return nil, errors.New("exceção JS: " + out.ExceptionDetails.Text)
	}
	if out.Result.Type == "undefined" {
		return nil, nil
	}
	return out.Result.Value, nil
}

// evalJSON avalia uma expressão que devolve JSON.stringify(...) e decodifica
// em v.
func (c *cdpConn) evalJSON(ctx context.Context, expr string, v any) error {
	val, err := c.eval(ctx, expr)
	if err != nil {
		return err
	}
	s, ok := val.(string)
	if !ok {
		return fmt.Errorf("esperava string JSON, veio %T: %v", val, val)
	}
	return json.Unmarshal([]byte(s), v)
}

// navigate navega a página (conexão de página) e espera o carregamento básico.
func (c *cdpConn) navigate(ctx context.Context, url string) error {
	if _, err := c.call(ctx, "Page.enable", nil); err != nil {
		return err
	}
	if _, err := c.call(ctx, "Page.navigate", map[string]any{"url": url}); err != nil {
		return err
	}
	return nil
}

// waitForExpr faz polling de uma expressão até devolver true (booleano JS).
func (c *cdpConn) waitForExpr(ctx context.Context, expr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		val, err := c.eval(ctx, expr)
		if err == nil {
			if b, ok := val.(bool); ok && b {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout esperando expressão: %s", expr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// click simula um clique de mouse real (Input.dispatchMouseEvent) no centro do
// elemento apontado por seletorCSS. Eventos via CDP são "trusted", o que
// desbloqueia políticas de autoplay.
func (c *cdpConn) click(ctx context.Context, seletorCSS string) error {
	var pt struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	expr := fmt.Sprintf(`JSON.stringify((function(){var el=document.querySelector(%q);if(!el)return null;var r=el.getBoundingClientRect();return {x:r.left+r.width/2,y:r.top+r.height/2};})())`, seletorCSS)
	if err := c.evalJSON(ctx, expr, &pt); err != nil {
		return err
	}
	for _, typ := range []string{"mousePressed", "mouseReleased"} {
		_, err := c.call(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type":       typ,
			"x":          pt.X,
			"y":          pt.Y,
			"button":     "left",
			"clickCount": 1,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// captureScreenshot salva um screenshot PNG (Page.captureScreenshot) da aba
// conectada por page, emulando um viewport de celular (390x844, dpr 2, touch).
// Antes da captura, espera o now-playing da UI refletir a mídia de verdade —
// o título chega à aba via hub (state), então a imagem mostra o estado
// conectado completo: indicadores servidor+extensão acesos e mídia tocando.
func captureScreenshot(ctx context.Context, page *cdpConn, path string) error {
	if err := page.waitForExpr(ctx,
		`(function(){
			var t = document.getElementById("np-title").textContent;
			return t !== "Nada tocando" && t !== "Escolha uma aba" && t !== "Sem título";
		})()`,
		15*time.Second); err != nil {
		return fmt.Errorf("UI não mostrou a mídia tocando: %w", err)
	}
	if _, err := page.call(ctx, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             390,
		"height":            844,
		"deviceScaleFactor": 2,
		"mobile":            true,
	}); err != nil {
		return fmt.Errorf("setDeviceMetricsOverride: %w", err)
	}
	// deixa o relayout assentar antes da captura
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(700 * time.Millisecond):
	}
	res, err := page.call(ctx, "Page.captureScreenshot", map[string]any{"format": "png"})
	if err != nil {
		return fmt.Errorf("captureScreenshot: %w", err)
	}
	var out struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(res, &out); err != nil || out.Data == "" {
		return fmt.Errorf("captureScreenshot: resposta inesperada: %s", string(res))
	}
	raw, err := base64.StdEncoding.DecodeString(out.Data)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

// browser é a conexão CDP de nível de browser.
type browser struct {
	base string // host:porta do endpoint HTTP de debug
	conn *cdpConn
}

// dialBrowser conecta ao WebSocket de nível de browser.
func dialBrowser(ctx context.Context, base string) (*browser, error) {
	wsURL, err := waitForCDPVersion(ctx, base)
	if err != nil {
		return nil, err
	}
	conn, err := dialCDP(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	return &browser{base: base, conn: conn}, nil
}

// close encerra a conexão.
func (b *browser) close() { b.conn.close() }

// loadUnpacked instala a extensão não empacotada (arquivos de produção) via
// Extensions.loadUnpacked e devolve o id da extensão.
func (b *browser) loadUnpacked(ctx context.Context, dir string) (string, error) {
	res, err := b.conn.call(ctx, "Extensions.loadUnpacked", map[string]any{"path": dir})
	if err != nil {
		return "", err
	}
	var r struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res, &r); err != nil || r.ID == "" {
		return "", fmt.Errorf("loadUnpacked: resposta inesperada: %s", string(res))
	}
	return r.ID, nil
}

// newTab abre uma nova aba com a URL dada e devolve o alvo (com WebSocket da
// página). Implementado com Target.createTarget + /json/list.
func (b *browser) newTab(ctx context.Context, url string) (*cdpTarget, error) {
	res, err := b.conn.call(ctx, "Target.createTarget", map[string]any{"url": url})
	if err != nil {
		return nil, err
	}
	var created struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(res, &created); err != nil || created.TargetID == "" {
		return nil, fmt.Errorf("createTarget: resposta inesperada: %s", string(res))
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		targets, err := listTargets(ctx, b.base)
		if err == nil {
			for i := range targets {
				t := &targets[i]
				if t.ID == created.TargetID && t.WS != "" {
					return t, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("aba %s não apareceu em /json/list", created.TargetID)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
