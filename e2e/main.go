// Command e2e é o harness de teste ponta-a-ponta do farchromectl:
// lança um Chrome dedicado (perfil temporário, porta de debug dedicada,
// headless), instala a extensão de PRODUÇÃO via CDP Extensions.loadUnpacked,
// sobe o binário farchromectl em porta de teste com token conhecido, abre uma
// página com mídia local e valida a cadeia completa pela perspectiva de um
// cliente "ui": extensão conecta (extStatus), state reporta mídia tocando e
// comandos pause/play/seekTo/setVolume refletem no elemento via CDP.
//
// Uso: make e2e         (ou: go run ./e2e -bin bin/farchromectl)
//
//	make e2e-tls     (ou: go run ./e2e -bin bin/farchromectl -tls)
//
// Modo -tls: o servidor roda com --tls (HTTPS no endereço principal, cert
// self-signed isolado em XDG_CONFIG_HOME temporário — o ~/.config do usuário
// não é tocado); o lado Go verifica o TLS de verdade contra esse cert (sem
// InsecureSkipVerify); o Chrome recebe --ignore-certificate-errors e valida
// na prática: UI servida por https em contexto seguro, UI do navegador
// autenticando por wss, service worker pronto (navigator.serviceWorker.ready)
// e a extensão conectando pelo listener HTTP loopback (porta principal +1) —
// a cadeia completa de comandos roda igual ao modo plano.
//
// O Chrome diário do usuário não é tocado: instância própria, user-data-dir
// temporário e porta de debug distinta; nada é persistido do lado do Chrome.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

var start = time.Now()

// logf imprime uma linha de transcript com o tempo decorrido.
func logf(format string, args ...any) {
	fmt.Printf("[%6.1fs] "+format+"\n", append([]any{time.Since(start).Seconds()}, args...)...)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nE2E: FALHOU — %v\n", err)
		os.Exit(1)
	}
	fmt.Println("\nE2E: PASSOU")
}

// run executa o harness completo; retornos de erro sempre passam pelo cleanup.
func run() error {
	chromeFlag := flag.String("chrome", "", "binário do Chrome/Chromium (padrão: auto-detecção)")
	binFlag := flag.String("bin", "bin/farchromectl", "binário do servidor farchromectl (já compilado; rode make build)")
	extFlag := flag.String("extension", "", "diretório da extensão (padrão: ../extension relativo a este fonte)")
	tokenFlag := flag.String("token", "424242", "token de teste (6 dígitos)")
	mediaFlag := flag.String("media", "auto", "mídia de teste: auto (ffmpeg→vídeo, senão WAV), video ou wav")
	tlsFlag := flag.Bool("tls", false, "modo TLS: servidor com --tls (HTTPS no endereço principal), Chrome com --ignore-certificate-errors, UI via https/wss e extensão via listener HTTP loopback (porta principal +1)")
	timeoutFlag := flag.Duration("timeout", 120*time.Second, "tempo total máximo do harness")
	headlessFlag := flag.Bool("headless", true, "headless=new (false = headed, para depuração)")
	keepFlag := flag.Bool("keep", false, "não remove os diretórios temporários ao final (depuração)")
	verboseFlag := flag.Bool("v", false, "loga todos os frames state/cmdResult recebidos")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeoutFlag)
	defer cancel()

	// ---------- preparação ----------
	chromeBin, err := findChrome(*chromeFlag)
	if err != nil {
		return err
	}
	logf("chrome: %s", chromeBin)

	extDir := *extFlag
	if extDir == "" {
		extDir = defaultExtensionDir()
	}
	if _, err := os.Stat(filepath.Join(extDir, "manifest.json")); err != nil {
		return fmt.Errorf("extensão não encontrada em %s: %v", extDir, err)
	}
	logf("extensão (arquivos de produção): %s", extDir)

	if _, err := os.Stat(*binFlag); err != nil {
		return fmt.Errorf("binário %s não encontrado (rode make build): %v", *binFlag, err)
	}

	serverPort, cdpPort, mediaPort, err := freePorts()
	if err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp("", "farchromectl-e2e-")
	if err != nil {
		return err
	}
	keep := *keepFlag
	defer func() {
		if keep {
			logf("mantendo diretório temporário: %s", tmpDir)
		} else {
			_ = os.RemoveAll(tmpDir)
		}
	}()
	mediaDir := filepath.Join(tmpDir, "media")
	profileDir := filepath.Join(tmpDir, "chrome-profile")
	serverLog := filepath.Join(tmpDir, "server.log")
	chromeLog := filepath.Join(tmpDir, "chrome.log")
	for _, d := range []string{mediaDir, profileDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			return err
		}
	}

	cleanup := newCleanup()
	defer cleanup.run()

	// ---------- mídia + página de teste ----------
	assets, err := genMedia(mediaDir, *mediaFlag)
	if err != nil {
		return err
	}
	logf("midia de teste: modo=%s", assets.mode)

	mediaMux := http.NewServeMux()
	mediaMux.HandleFunc("/media.html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(assets.html)
	})
	if assets.file != "" {
		mediaMux.Handle("/"+assets.file, http.FileServer(http.Dir(mediaDir)))
	}
	mediaLn, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(mediaPort)))
	if err != nil {
		return err
	}
	mediaSrv := &http.Server{Handler: mediaMux}
	go func() { _ = mediaSrv.Serve(mediaLn) }()
	cleanup.add(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = mediaSrv.Shutdown(shutdownCtx)
	})
	mediaURL := fmt.Sprintf("http://127.0.0.1:%d/media.html", mediaPort)
	logf("pagina de midia: %s", mediaURL)

	// ---------- servidor farchromectl ----------
	// No modo -tls: o servidor recebe --tls; a extensão é configurada com a
	// porta loopback (porta principal +1) e o certificado é gerado sob
	// XDG_CONFIG_HOME temporário — o ~/.config do usuário não é tocado e o
	// harness pode ler o cert para VERIFICAR o HTTPS de verdade (sem
	// InsecureSkipVerify no lado Go).
	scheme := "http"
	extPort := serverPort // porta que a extensão usa nas opções
	certPath := ""
	serverArgs := []string{
		"--addr", net.JoinHostPort("127.0.0.1", strconv.Itoa(serverPort)),
		"--token", *tokenFlag,
	}
	var tlsClient *http.Client
	if *tlsFlag {
		scheme = "https"
		extPort = serverPort + 1
		certPath = filepath.Join(tmpDir, "config", "farchromectl", "cert.pem")
		serverArgs = append(serverArgs, "--tls")
	}
	serverCmd := exec.Command(*binFlag, serverArgs...)
	if *tlsFlag {
		serverCmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+filepath.Join(tmpDir, "config"))
	}
	serverCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	sl, err := os.Create(serverLog)
	if err != nil {
		return err
	}
	serverCmd.Stdout, serverCmd.Stderr = sl, sl
	if err := serverCmd.Start(); err != nil {
		sl.Close()
		return fmt.Errorf("não foi possível iniciar %s: %v", *binFlag, err)
	}
	serverProc := serverCmd.Process
	cleanup.add(func() { killProcess(serverProc, serverCmd, sl) })
	logf("servidor: %s %s (log: %s)", *binFlag, strings.Join(serverArgs, " "), serverLog)

	if *tlsFlag {
		// o certificado é gerado ANTES do listener subir: esperar o arquivo
		// existir e montar o cliente HTTPS verificado contra ele.
		if err := waitCert(ctx, certPath, serverCmd); err != nil {
			return fmt.Errorf("%v\n--- server.log ---\n%s", err, readFileTail(serverLog, 2000))
		}
		certPEM, err := os.ReadFile(certPath)
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(certPEM) {
			return fmt.Errorf("cert de teste %s não parseia", certPath)
		}
		tlsClient = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"},
		}}
		logf("certificado de teste verificado (SAN 127.0.0.1): %s", certPath)
	}

	healthzURL := fmt.Sprintf("%s://127.0.0.1:%d/api/healthz", scheme, serverPort)
	if err := waitHealthz(ctx, healthzURL, tlsClient, serverCmd); err != nil {
		return fmt.Errorf("%v\n--- server.log ---\n%s", err, readFileTail(serverLog, 2000))
	}
	logf("servidor respondeu /api/healthz (%s)", scheme)

	// banner no log (saída não-TTY): sem QR (half-blocks) e com o esquema certo
	if err := checkBannerLog(serverLog, scheme, extPort); err != nil {
		return err
	}
	logf("banner no server.log: URLs %s, sem QR (saída não é TTY) — ok", scheme)

	// ---------- Chrome dedicado ----------
	chromeArgs := []string{
		"--user-data-dir=" + profileDir,
		fmt.Sprintf("--remote-debugging-port=%d", cdpPort),
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-gpu",
		"--mute-audio",
		"--hide-scrollbars",
		"--window-size=900,700",
	}
	if *tlsFlag {
		// certificado self-signed: o Chrome de teste ignora o erro para
		// carregar a página https (o lado Go verifica de verdade, acima)
		chromeArgs = append(chromeArgs, "--ignore-certificate-errors")
	}
	if *headlessFlag {
		chromeArgs = append([]string{"--headless=new"}, chromeArgs...)
	}
	chromeArgs = append(chromeArgs, "about:blank")
	chromeCmd := exec.Command(chromeBin, chromeArgs...)
	chromeCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cl, err := os.Create(chromeLog)
	if err != nil {
		return err
	}
	chromeCmd.Stdout, chromeCmd.Stderr = cl, cl
	if err := chromeCmd.Start(); err != nil {
		cl.Close()
		return fmt.Errorf("não foi possível iniciar o Chrome: %v", err)
	}
	chromeProc := chromeCmd.Process
	cleanup.add(func() { killProcess(chromeProc, chromeCmd, cl) })
	logf("chrome dedicado: pid=%d cdp=127.0.0.1:%d perfil=%s", chromeProc.Pid, cdpPort, profileDir)

	cdpBase := fmt.Sprintf("127.0.0.1:%d", cdpPort)
	browser, err := dialBrowser(ctx, cdpBase)
	if err != nil {
		return fmt.Errorf("%v\n--- chrome.log ---\n%s", err, readFileTail(chromeLog, 2000))
	}
	cleanup.add(browser.close)

	// ---------- extensão (arquivos de produção) ----------
	extID, err := browser.loadUnpacked(ctx, extDir)
	if err != nil {
		return fmt.Errorf("Extensions.loadUnpacked falhou: %w", err)
	}
	logf("extensao carregada via CDP: id=%s", extID)

	// ---------- cliente ui ----------
	// tls: wss:// no listener principal, com TLS VERIFICADO contra o cert
	// de teste; senão ws:// como antes.
	uiURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", serverPort)
	var dialOpts *websocket.DialOptions
	if *tlsFlag {
		uiURL = fmt.Sprintf("wss://127.0.0.1:%d/ws", serverPort)
		dialOpts = &websocket.DialOptions{HTTPClient: tlsClient}
	}
	ui, err := dialUI(ctx, uiURL, *tokenFlag, *verboseFlag, dialOpts)
	if err != nil {
		return err
	}
	cleanup.add(ui.close)
	logf("cliente ui autenticado em %s", uiURL)

	// ---------- configuração da extensão pela página de opções ----------
	optTab, err := browser.newTab(ctx, "chrome-extension://"+extID+"/options.html")
	if err != nil {
		return err
	}
	optConn, err := dialCDP(ctx, optTab.WS)
	if err != nil {
		return err
	}
	cleanup.add(optConn.close)
	if err := optConn.waitForExpr(ctx, `document.readyState === "complete" && !!document.getElementById("opt-save")`, 10*time.Second); err != nil {
		return fmt.Errorf("página de opções não carregou: %w", err)
	}
	// Preenche o formulário e clica em salvar; o status "Salvo" é definido
	// de forma assíncrona (dentro do .then de chrome.storage.local.set),
	// então fazemos polling dele em vez de ler sincronamente.
	configureExpr := fmt.Sprintf(`(function(){
		document.getElementById("opt-port").value = %q;
		document.getElementById("opt-token").value = %q;
		document.getElementById("opt-save").click();
	})()`, strconv.Itoa(extPort), *tokenFlag)
	if _, err := optConn.eval(ctx, configureExpr); err != nil {
		return fmt.Errorf("falha ao configurar opções: %w", err)
	}
	if err := optConn.waitForExpr(ctx,
		`document.getElementById("opt-status").textContent.length > 0`, 5*time.Second); err != nil {
		return fmt.Errorf("página de opções não respondeu ao salvar: %w", err)
	}
	val, err := optConn.eval(ctx, `document.getElementById("opt-status").textContent`)
	if err != nil {
		return fmt.Errorf("falha ao ler status das opções: %w", err)
	}
	status, _ := val.(string)
	if !strings.Contains(status, "Salvo") {
		return fmt.Errorf("página de opções não confirmou o salvamento: status=%q", status)
	}
	logf("opções da extensão configuradas (porta=%d token=%s): %q", extPort, *tokenFlag, status)

	// ---------- asserções ----------
	step := func(name string, fn func() error) error {
		if err := fn(); err != nil {
			logf("FALHA: %s: %v", name, err)
			return fmt.Errorf("%s: %w", name, err)
		}
		logf("ok: %s", name)
		return nil
	}

	// (a) extensão conecta ao servidor → extStatus connected
	if err := step("extensao conecta (extStatus connected=true)", func() error {
		return ui.waitExtStatus(ctx, true, 15*time.Second)
	}); err != nil {
		return err
	}

	// ---------- asserções exclusivas do modo -tls (lado navegador) ----------
	if *tlsFlag {
		uiBase := fmt.Sprintf("%s://127.0.0.1:%d", scheme, serverPort)

		// (tls-a) UI servida por HTTPS e em contexto seguro (pré-condição
		// para service worker/install de PWA)
		uiTab, err := browser.newTab(ctx, uiBase+"/")
		if err != nil {
			return err
		}
		uiPage, err := dialCDP(ctx, uiTab.WS)
		if err != nil {
			return err
		}
		cleanup.add(uiPage.close)
		if err := step("UI servida sobre HTTPS em contexto seguro", func() error {
			// gate por conteúdo (título da UI), não só readyState: a aba
			// nasce em about:blank, que também tem readyState "complete"
			if err := uiPage.waitForExpr(ctx,
				`document.readyState === "complete" && document.title === "farchromectl"`,
				10*time.Second); err != nil {
				return err
			}
			for _, expr := range []string{
				`location.protocol === "https:"`,
				`window.isSecureContext === true`,
			} {
				val, err := uiPage.eval(ctx, expr)
				if err != nil {
					return fmt.Errorf("%s: %w", expr, err)
				}
				if ok, _ := val.(bool); !ok {
					return fmt.Errorf("asserção falhou: %s", expr)
				}
			}
			return nil
		}); err != nil {
			return err
		}

		// (tls-b) a UI do NAVEGADOR conecta de verdade: formulário de conexão
		// preenchido via CDP (perfil limpo não tem credenciais), play/pause
		// dos indicadores dot-server (wss autenticado) e dot-ext (extensão
		// conectada via listener loopback, vista pelo hub TLS).
		if err := step("UI do navegador conecta via wss (dot-server e dot-ext acesos)", func() error {
			if err := uiPage.waitForExpr(ctx,
				`!document.getElementById("connect-form").hidden && !!document.getElementById("f-token")`,
				10*time.Second); err != nil {
				return fmt.Errorf("formulário de conexão não apareceu: %w", err)
			}
			fillExpr := fmt.Sprintf(`(function(){
				document.getElementById("f-url").value = %q;
				document.getElementById("f-token").value = %q;
				document.querySelector("#connect-form button[type=submit]").click();
			})()`, uiBase, *tokenFlag)
			if _, err := uiPage.eval(ctx, fillExpr); err != nil {
				return fmt.Errorf("falha ao preencher formulário da UI: %w", err)
			}
			return uiPage.waitForExpr(ctx,
				`document.getElementById("dot-server").classList.contains("on") && document.getElementById("dot-ext").classList.contains("on")`,
				15*time.Second)
		}); err != nil {
			return err
		}
		logf("navegador: UI autenticada por wss e extensao visível (dots verdes)")

		// (tls-c) service worker registra e fica pronto sobre HTTPS
		if err := step("service worker registra e fica pronto (navigator.serviceWorker.ready)", func() error {
			expr := `(function(){
				if (window.__swReady) return true;
				if (navigator.serviceWorker) {
					navigator.serviceWorker.ready.then(function () { window.__swReady = true; }).catch(function () {});
				}
				return false;
			})()`
			return uiPage.waitForExpr(ctx, expr, 20*time.Second)
		}); err != nil {
			return err
		}
	}

	// ---------- página com mídia ----------
	mediaTab, err := browser.newTab(ctx, mediaURL)
	if err != nil {
		return err
	}
	mediaConn, err := dialCDP(ctx, mediaTab.WS)
	if err != nil {
		return err
	}
	cleanup.add(mediaConn.close)
	if err := mediaConn.waitForExpr(ctx, `document.readyState === "complete" && !!window.__el`, 10*time.Second); err != nil {
		return fmt.Errorf("página de mídia não carregou: %w", err)
	}
	logf("aba de midia aberta: %s", mediaTab.URL)

	// start da reprodução com gesto real de usuário (clique via CDP — trusted)
	if err := step("play via clique real (Input.dispatchMouseEvent)", func() error {
		if err := mediaConn.click(ctx, "#play"); err != nil {
			return err
		}
		return mediaConn.waitForExpr(ctx, `window.__el && !window.__el.paused && window.__el.currentTime > 0.3`, 10*time.Second)
	}); err != nil {
		return err
	}

	// estado bruto do elemento via CDP (ground truth, não passa pelo hub)
	elementState := func() (paused bool, t, vol float64, err error) {
		var st struct {
			Paused bool    `json:"paused"`
			T      float64 `json:"t"`
			Vol    float64 `json:"vol"`
			Err    string  `json:"err"`
		}
		if err = mediaConn.evalJSON(ctx, `JSON.stringify({paused:window.__el.paused,t:window.__el.currentTime,vol:window.__el.volume,err:window.__mediaError||""})`, &st); err != nil {
			return
		}
		if st.Err != "" {
			err = fmt.Errorf("mediaError na página: %s", st.Err)
			return
		}
		return st.Paused, st.T, st.Vol, nil
	}

	// (b) state reporta a mídia tocando (título/duração via hub)
	if err := step("state reporta midia tocando (playing, duration, title)", func() error {
		f, err := ui.waitState(ctx, func(f stateFrame) bool {
			return len(f.Tabs) > 0 && f.Tabs[0].Playing && f.Tabs[0].Title != ""
		}, 15*time.Second, "midia tocando")
		if err != nil {
			return err
		}
		tab := f.Tabs[0]
		if tab.Duration == nil || !approx(*tab.Duration, mediaDuration, 1.0) {
			return fmt.Errorf("duration reportado %v, esperado ~%d", tab.Duration, mediaDuration)
		}
		logf("  state: title=%q position=%.2f duration=%.2f volume=%.2f tabId=%d",
			tab.Title, tab.Position, *tab.Duration, tab.Volume, tab.TabID)
		return nil
	}); err != nil {
		return err
	}

	// (c) posição avança (dois states com delta >=0.5; e currentTime no elemento)
	if err := step("posicao avanca (state e elemento)", func() error {
		var first *stateFrame
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case f := <-ui.states:
				if first == nil {
					first = &f
					continue
				}
				// delta de posição só acontece com timeupdates distintos
				// (frames chegam ~1/s enquanto a mídia toca).
				if f.Tabs[0].Position-first.Tabs[0].Position >= 0.5 {
					logf("  posicao no state: %.2f -> %.2f", first.Tabs[0].Position, f.Tabs[0].Position)
					_, t1, _, err := elementState()
					if err != nil {
						return err
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(1500 * time.Millisecond):
					}
					_, t2, _, err := elementState()
					if err != nil {
						return err
					}
					if t2-t1 < 0.5 {
						return fmt.Errorf("currentTime do elemento não avançou: %.2f -> %.2f", t1, t2)
					}
					logf("  currentTime do elemento: %.2f -> %.2f", t1, t2)
					return nil
				}
			case <-time.After(200 * time.Millisecond):
			}
		}
		return errors.New("posição no state não avançou")
	}); err != nil {
		return err
	}

	// (d) pause
	if err := step("comando pause: elemento pausa e state reflete", func() error {
		if err := ui.sendCmd(ctx, "pause", nil); err != nil {
			return err
		}
		if err := ui.waitCmdResult(ctx, "pause", true, 10*time.Second); err != nil {
			return err
		}
		if _, err := ui.waitState(ctx, func(f stateFrame) bool {
			return len(f.Tabs) > 0 && !f.Tabs[0].Playing
		}, 10*time.Second, "pausado"); err != nil {
			return err
		}
		paused, _, _, err := elementState()
		if err != nil {
			return err
		}
		if !paused {
			return errors.New("elemento não está pausado (CDP)")
		}
		return nil
	}); err != nil {
		return err
	}

	// (e) play (resume — elemento destravado pelo gesto anterior)
	if err := step("comando play: elemento volta a tocar", func() error {
		if err := ui.sendCmd(ctx, "play", nil); err != nil {
			return err
		}
		if err := ui.waitCmdResult(ctx, "play", true, 10*time.Second); err != nil {
			return err
		}
		if _, err := ui.waitState(ctx, func(f stateFrame) bool {
			return len(f.Tabs) > 0 && f.Tabs[0].Playing
		}, 10*time.Second, "tocando"); err != nil {
			return err
		}
		paused, _, _, err := elementState()
		if err != nil {
			return err
		}
		if paused {
			return errors.New("elemento continua pausado (CDP)")
		}
		return nil
	}); err != nil {
		return err
	}

	// (f) seekTo
	if err := step("comando seekTo 10s: currentTime salta", func() error {
		v := 10.0
		if err := ui.sendCmd(ctx, "seekTo", &v); err != nil {
			return err
		}
		if err := ui.waitCmdResult(ctx, "seekTo", true, 10*time.Second); err != nil {
			return err
		}
		if _, err := ui.waitState(ctx, func(f stateFrame) bool {
			return len(f.Tabs) > 0 && approx(f.Tabs[0].Position, 10, 1.5)
		}, 10*time.Second, "position ~10"); err != nil {
			return err
		}
		_, t, _, err := elementState()
		if err != nil {
			return err
		}
		if !approx(t, 10, 1.0) {
			return fmt.Errorf("currentTime=%.2f, esperado ~10", t)
		}
		logf("  currentTime=%.2f apos seekTo 10", t)
		return nil
	}); err != nil {
		return err
	}

	// (g) setVolume
	if err := step("comando setVolume 0.37: volume muda no elemento e no state", func() error {
		v := 0.37
		if err := ui.sendCmd(ctx, "setVolume", &v); err != nil {
			return err
		}
		if err := ui.waitCmdResult(ctx, "setVolume", true, 10*time.Second); err != nil {
			return err
		}
		if _, err := ui.waitState(ctx, func(f stateFrame) bool {
			return len(f.Tabs) > 0 && approx(f.Tabs[0].Volume, 0.37, 0.01)
		}, 10*time.Second, "volume ~0.37"); err != nil {
			return err
		}
		_, _, vol, err := elementState()
		if err != nil {
			return err
		}
		if !approx(vol, 0.37, 0.001) {
			return fmt.Errorf("volume do elemento=%.3f, esperado 0.370", vol)
		}
		logf("  volume=%.3f apos setVolume 0.37", vol)
		return nil
	}); err != nil {
		return err
	}

	logf("todos os passos passaram — encerrando e limpando")
	return nil
}

// ---------- auxiliares ----------

// findChrome localiza o binário do Chrome/Chromium.
func findChrome(flagVal string) (string, error) {
	candidates := []string{flagVal, os.Getenv("FARCHROMECTL_E2E_CHROME"),
		"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome-for-testing"}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if path, err := exec.LookPath(c); err == nil {
			return path, nil
		}
	}
	return "", errors.New("nenhum Chrome/Chromium encontrado (instale google-chrome/chromium ou use -chrome); sem ele, a Fase 1 fica Blocked")
}

// defaultExtensionDir resolve o diretório extension/ do repo relativo a este
// fonte (funciona independente do cwd).
func defaultExtensionDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "extension")
}

// freePorts devolve portas TCP livres em 127.0.0.1 para o harness. A porta
// do servidor vem com a garantia extra de que a sucessora (porta+1) também
// está livre: no modo -tls o servidor usa porta+1 como listener HTTP
// loopback da extensão.
func freePorts() (serverPort, cdpPort, mediaPort int, err error) {
	for i := 0; i < 100; i++ {
		p, err := freePort()
		if err != nil {
			return 0, 0, 0, err
		}
		if !portFree(p + 1) {
			continue
		}
		cdp, err := freePort()
		if err != nil {
			return 0, 0, 0, err
		}
		media, err := freePort()
		if err != nil {
			return 0, 0, 0, err
		}
		used := map[int]bool{p: true, p + 1: true}
		if used[cdp] || used[media] || cdp == media {
			continue
		}
		return p, cdp, media, nil
	}
	return 0, 0, 0, errors.New("não achei conjunto de portas livres (servidor com sucessora, cdp e media)")
}

// freePort devolve uma porta TCP livre em 127.0.0.1.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// portFree informa se a porta está livre em 127.0.0.1.
func portFree(p int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// waitCert espera o certificado de teste existir (o servidor o grava antes
// de subir o listener), vigiando o processo.
func waitCert(ctx context.Context, certPath string, cmd *exec.Cmd) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(cmd.Process) {
			return fmt.Errorf("processo do servidor morreu durante o startup")
		}
		if _, err := os.Stat(certPath); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("certificado %s não apareceu em 10s", certPath)
}

// checkBannerLog valida o banner do servidor no log (saída não-TTY): URLs no
// esquema esperado, linha da extensão na porta certa e NENHUM QR (o QR só
// sai quando stdout é terminal — pipe/arquivo fica limpo).
func checkBannerLog(serverLog, scheme string, extPort int) error {
	raw, err := os.ReadFile(serverLog)
	if err != nil {
		return err
	}
	log := string(raw)
	if !strings.Contains(log, scheme+"://") {
		return fmt.Errorf("banner sem URLs %s:// (log: %.300s)", scheme, log)
	}
	want := fmt.Sprintf("ws://127.0.0.1:%d/ws", extPort)
	if !strings.Contains(log, want) {
		return fmt.Errorf("banner sem a linha da extensão %q", want)
	}
	for _, block := range []string{"█", "▀", "▄"} {
		if strings.Contains(log, block) {
			return fmt.Errorf("banner imprime QR (%q) em saída não-TTY — gating quebrado", block)
		}
	}
	return nil
}

// waitHealthz espera o /api/healthz do servidor responder com corpo válido,
// vigiando o processo. client nil → HTTP puro (default); não-nil → cliente
// HTTPS já configurado para verificar o certificado de teste.
func waitHealthz(ctx context.Context, url string, client *http.Client, cmd *exec.Cmd) error {
	if client == nil {
		client = http.DefaultClient
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(cmd.Process) {
			return fmt.Errorf("processo do servidor morreu durante o startup")
		}
		resp, err := client.Get(url) //nolint:gosec // porta local de teste
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(body), `"name":"farchromectl"`) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("servidor não respondeu %s em 10s", url)
}

// processAlive checa se o processo existe (signal 0).
func processAlive(p *os.Process) bool {
	return p != nil && p.Signal(syscall.Signal(0)) == nil
}

// killProcess encerra um processo filho (SIGTERM, espera, SIGKILL) e fecha o
// log associado. Processos em grupo próprio (Setpgid) recebem sinal no grupo.
func killProcess(p *os.Process, cmd *exec.Cmd, logFile *os.File) {
	if p == nil {
		return
	}
	_ = syscall.Kill(-p.Pid, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
		<-done
	}
	if logFile != nil {
		_ = logFile.Close()
	}
}

// readFileTail devolve o final do arquivo (para diagnóstico de falha).
func readFileTail(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(sem %s: %v)", path, err)
	}
	if len(data) > n {
		data = data[len(data)-n:]
	}
	return string(data)
}

// approx checa aproximação com tolerância absoluta.
func approx(got, want, tol float64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d <= tol
}

// cleanup acumula funções de encerramento executadas na ordem inversa.
type cleanupStack struct {
	fns []func()
}

func newCleanup() *cleanupStack { return &cleanupStack{} }

func (c *cleanupStack) add(fn func()) { c.fns = append(c.fns, fn) }

func (c *cleanupStack) run() {
	for i := len(c.fns) - 1; i >= 0; i-- {
		c.fns[i]()
	}
}
