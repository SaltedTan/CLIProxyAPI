package cliproxy

import (
	"net"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
)

// warnIfClientAuthOff warns when no access provider guards the proxy endpoints and the
// server listens on an address other hosts can reach. With no client API keys and no
// plugin access provider, every request is served without a key.
func (s *Service) warnIfClientAuthOff() {
	if s == nil || s.accessManager == nil || s.cfg == nil || len(s.accessManager.Providers()) > 0 {
		return
	}
	host := strings.TrimSpace(s.cfg.Host)
	if isLoopbackHost(host) {
		return
	}
	where := "all interfaces, port " + strconv.Itoa(s.cfg.Port)
	if host != "" {
		where = net.JoinHostPort(host, strconv.Itoa(s.cfg.Port))
	}
	log.Warnf("no client API keys are configured (access.api-keys): the proxy endpoints on %s accept every request without a key. Add a key, or set server.host to 127.0.0.1 to keep the proxy local", where)
}

// isLoopbackHost reports whether host only accepts connections from this machine.
func isLoopbackHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
