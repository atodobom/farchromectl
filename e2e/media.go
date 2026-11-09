// Geração da mídia de teste e da página HTML que a reproduz. Modo preferido:
// vídeo .webm gerado por ffmpeg (testsrc2, 30s); fallback: <audio> com WAV
// sintetizado em JavaScript (data via Blob), quando o ffmpeg não existe.
package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
)

const mediaDuration = 30 // segundos

// mediaAssets descreve o que foi gerado.
type mediaAssets struct {
	mode string // "video" ou "wav"
	file string // nome do arquivo servido (vazio em modo wav)
	html []byte // página media.html
}

// mediaHTMLHead é o prólogo comum das duas variantes.
const mediaHTMLHead = `<!doctype html>
<html lang="pt-BR">
<head>
<meta charset="utf-8">
<title>farchromectl e2e — media de teste</title>
</head>
<body>
`

// mediaHTMLTail é o fecho comum: botão #play fixo (alvo determinístico do
// clique via CDP) e expõe window.__el / window.__mediaError para inspeção.
const mediaHTMLTail = `
<button id="play" style="position:fixed;left:20px;top:320px;width:320px;height:80px;font-size:28px">play</button>
<script>
  var el = document.getElementById("m");
  window.__el = el;
  window.__mediaError = "";
  el.addEventListener("error", function () { window.__mediaError = "erro ao carregar a midia"; });
  document.getElementById("play").addEventListener("click", function () {
    el.play().catch(function (e) { window.__mediaError = String(e); });
  });
</script>
</body>
</html>
`

// genMedia gera a mídia no diretório dir conforme o modo pedido
// ("auto", "video" ou "wav"). Em "auto", prefere ffmpeg (vídeo) e cai para
// WAV sintetizado se o ffmpeg não existir ou falhar.
func genMedia(dir, mode string) (*mediaAssets, error) {
	switch mode {
	case "video":
		return genVideo(dir)
	case "wav":
		return &mediaAssets{mode: "wav", html: wavHTML()}, nil
	case "auto":
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			return &mediaAssets{mode: "wav", html: wavHTML()}, nil
		}
		if a, err := genVideo(dir); err == nil {
			return a, nil
		}
		return &mediaAssets{mode: "wav", html: wavHTML()}, nil
	default:
		return nil, fmt.Errorf("modo de mídia inválido: %q (use auto, video ou wav)", mode)
	}
}

// genVideo gera media.webm (VP8, sem áudio) com testsrc2 via ffmpeg.
func genVideo(dir string) (*mediaAssets, error) {
	out := filepath.Join(dir, "media.webm")
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=duration=%d:size=320x240:rate=10", mediaDuration),
		"-c:v", "libvpx", "-b:v", "300k", "-deadline", "good", "-cpu-used", "5", "-pix_fmt", "yuv420p",
		out)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %v: %s", err, tail(stderr.String(), 400))
	}
	html := []byte(mediaHTMLHead + `<video id="m" src="/media.webm" controls preload="auto"></video>` + mediaHTMLTail)
	return &mediaAssets{mode: "video", file: "media.webm", html: html}, nil
}

// wavHTML devolve a página que sintetiza um WAV de 30s (senoide 440Hz) em
// JavaScript e o toca em <audio>.
func wavHTML() []byte {
	html := mediaHTMLHead + `<audio id="m" controls preload="auto"></audio>` + mediaHTMLTail
	// Injeta a síntese antes do fecho (o <script> do fecho roda depois e
	// referencia window.__el; a ordem no HTML mantém __el já definido).
	synth := `
<script>
  (function () {
    var sr = 8000, n = sr * ` + fmt.Sprint(mediaDuration) + `;
    var buf = new ArrayBuffer(44 + n * 2), dv = new DataView(buf);
    function ws(o, s) { for (var i = 0; i < s.length; i++) dv.setUint8(o + i, s.charCodeAt(i)); }
    ws(0, "RIFF"); dv.setUint32(4, 36 + n * 2, true); ws(8, "WAVE"); ws(12, "fmt ");
    dv.setUint32(16, 16, true); dv.setUint16(20, 1, true); dv.setUint16(22, 1, true);
    dv.setUint32(24, sr, true); dv.setUint32(28, sr * 2, true);
    dv.setUint16(32, 2, true); dv.setUint16(34, 16, true);
    ws(36, "data"); dv.setUint32(40, n * 2, true);
    for (var i = 0; i < n; i++) {
      dv.setInt16(44 + i * 2, Math.round(Math.sin(2 * Math.PI * 440 * i / sr) * 12000), true);
    }
    document.getElementById("m").src = URL.createObjectURL(new Blob([buf], { type: "audio/wav" }));
  })();
</script>
`
	return []byte(html + synth)
}

// tail devolve os últimos n caracteres de s (para mensagens de erro).
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
