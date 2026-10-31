package notify

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a minimal SMTP server on a loopback port: just enough of RFC 5321 for net/smtp.
type fakeSMTP struct {
	t        *testing.T
	ln       net.Listener
	tlsConf  *tls.Config
	implicit bool // TLS from the first byte
	offerTLS bool // advertise STARTTLS
	refuseTo string

	mu       sync.Mutex
	auth     string // decoded PLAIN credentials: "\x00user\x00pass"
	from, to []string
	data     string
}

func newFakeSMTP(t *testing.T, implicit, startTLS bool) (*fakeSMTP, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSMTP{t: t, ln: ln, implicit: implicit, offerTLS: startTLS,
		tlsConf: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}}
	t.Cleanup(func() { _ = ln.Close() }) // test server shutdown
	go s.serve()
	return s, &tls.Config{RootCAs: pool}
}

func (s *fakeSMTP) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *fakeSMTP) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.session(c)
	}
}

func (s *fakeSMTP) session(c net.Conn) {
	defer func() { _ = c.Close() }()                    // test server connection
	_ = c.SetDeadline(time.Now().Add(10 * time.Second)) // a stuck test must end
	if s.implicit {
		c = tls.Server(c, s.tlsConf)
	}
	r, w := bufio.NewReader(c), func(l string) { _, _ = c.Write([]byte(l + "\r\n")) }
	w("220 fake ESMTP")
	secure := s.implicit
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		up := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			w("250-fake")
			if s.offerTLS && !secure {
				w("250-STARTTLS")
			}
			w("250 AUTH PLAIN")
		case up == "STARTTLS":
			w("220 go ahead")
			c = tls.Server(c, s.tlsConf)
			r, secure = bufio.NewReader(c), true
			w = func(l string) { _, _ = c.Write([]byte(l + "\r\n")) }
		case strings.HasPrefix(up, "AUTH PLAIN"):
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(cmd[len("AUTH PLAIN"):]))
			s.mu.Lock()
			s.auth = string(raw)
			s.mu.Unlock()
			w("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			s.mu.Lock()
			s.from = append(s.from, cmd)
			s.mu.Unlock()
			w("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			if s.refuseTo != "" && strings.Contains(cmd, s.refuseTo) {
				w("550 5.1.1 no such mailbox")
				continue
			}
			s.mu.Lock()
			s.to = append(s.to, cmd)
			s.mu.Unlock()
			w("250 ok")
		case up == "DATA":
			w("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.data = b.String()
			s.mu.Unlock()
			w("250 queued")
		case up == "QUIT":
			w("221 bye")
			return
		default:
			w("502 not implemented " + strconv.Quote(cmd))
		}
	}
}
