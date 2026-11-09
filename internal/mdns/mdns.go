// Package mdns anuncia o servidor farchromectl na rede local via mDNS
// (serviço _farchromectl._tcp). O anúncio é OPCIONAL: se a rede não suportar
// multicast (sem avahi, rede sem rota multicast, sandbox etc.), Advertise
// devolve erro e o chamador apenas registra um aviso — o servidor continua
// funcionando por IP.
//
// O token NUNCA é anunciado: os registros TXT carregam apenas a versão.
package mdns

import (
	"fmt"
	"net"

	"github.com/grandcat/zeroconf"
)

const (
	// Instance é o nome da instância anunciada.
	Instance = "farchromectl"
	// Service é o tipo de serviço DNS-SD.
	Service = "_farchromectl._tcp"
	// Domain do mDNS.
	Domain = "local."
	// Hostname alvo anunciado (farchromectl.local).
	Hostname = "farchromectl"
)

// registerProxy é uma variável para permitir injeção de falha em testes.
var registerProxy = zeroconf.RegisterProxy

// Advertise registra o serviço e devolve uma função de shutdown.
// Nunca panica: qualquer falha retorna como erro.
func Advertise(port int, version string) (stop func(), err error) {
	ips := NonLoopbackIPStrings()
	if len(ips) == 0 {
		return func() {}, fmt.Errorf("mdns: nenhum IP não-loopback para anunciar")
	}
	srv, err := registerProxy(Instance, Service, Domain, port, Hostname, ips,
		[]string{"version=" + version}, nil)
	if err != nil {
		return func() {}, fmt.Errorf("mdns: registro do serviço falhou: %w", err)
	}
	return func() { srv.Shutdown() }, nil
}

// NonLoopbackIPs devolve os IPs unicast das interfaces de rede ativas,
// excluindo loopback. IPv4 primeiro (mais útil para URLs no celular).
func NonLoopbackIPs() []net.IP {
	var out []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	var v6 []net.IP
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
				continue
			}
			if ip.To4() != nil {
				out = append(out, ip)
			} else {
				v6 = append(v6, ip)
			}
		}
	}
	return append(out, v6...)
}

// NonLoopbackIPStrings é NonLoopbackIPs em formato string.
func NonLoopbackIPStrings() []string {
	ips := NonLoopbackIPs()
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}
