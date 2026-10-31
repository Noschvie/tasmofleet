package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/config"
)

// sendSMTP delivers ev as one e-mail. The whole conversation is bounded by sendTimeout. It
// returns "accepted by <host>" on success. tlsConf is nil in production (system roots, the
// host as server name); tests pass a pool that trusts their server.
func sendSMTP(ctx context.Context, c config.Config, ev Event, tlsConf *tls.Config) (string, error) {
	from, err := mail.ParseAddress(c.NotifySMTPFrom)
	if err != nil {
		return "", errors.New("the From address is not valid")
	}
	to, err := config.SMTPRecipients(c.NotifySMTPTo)
	if err != nil || len(to) == 0 {
		return "", errors.New("the To addresses are not valid")
	}
	host := c.NotifySMTPHost
	addr := net.JoinHostPort(host, strconv.Itoa(c.NotifySMTPPort))
	tc := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	if tlsConf != nil {
		tc = tlsConf.Clone()
		tc.ServerName = host
	}

	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", errors.New(scrubNet(err, c.NotifySMTPPassword))
	}
	defer func() { _ = conn.Close() }() // the outcome is decided by the SMTP replies, not by the close
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl) // a failure here only loses the early timeout; the context still bounds the dial
	}
	if c.NotifySMTPSecurity == config.NotifySecTLS {
		conn = tls.Client(conn, tc)
	}
	cl, err := smtp.NewClient(conn, host)
	if err != nil {
		return "", errors.New("the server did not greet: " + scrubNet(err, c.NotifySMTPPassword))
	}
	defer func() { _ = cl.Close() }() // as above; Quit already ended a good session
	if c.NotifySMTPSecurity == config.NotifySecStartTLS {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return "", errors.New("the server does not offer STARTTLS")
		}
		if err := cl.StartTLS(tc); err != nil {
			return "", errors.New("STARTTLS failed: " + scrubNet(err, c.NotifySMTPPassword))
		}
	}
	if c.NotifySMTPUser != "" {
		if err := smtpAuth(cl, c); err != nil {
			return "", err
		}
	}
	if err := cl.Mail(from.Address); err != nil {
		return "", errors.New("sender refused: " + smtpReason(err))
	}
	for _, r := range to {
		if err := cl.Rcpt(r.Address); err != nil {
			return "", errors.New("recipient " + r.Address + " refused: " + smtpReason(err))
		}
	}
	w, err := cl.Data()
	if err != nil {
		return "", errors.New("message refused: " + smtpReason(err))
	}
	if _, err := w.Write(buildMessage(from, to, ev)); err != nil {
		return "", errors.New("sending failed: " + scrubNet(err, c.NotifySMTPPassword))
	}
	if err := w.Close(); err != nil {
		return "", errors.New("message refused: " + smtpReason(err))
	}
	_ = cl.Quit() // the message is accepted; a server that drops the goodbye changes nothing
	return "accepted by " + host, nil
}

// smtpAuth logs in with PLAIN, or LOGIN for a server that offers only that. It also works on a
// connection without TLS (net/smtp refuses that for PLAIN): the person chose "none".
func smtpAuth(cl *smtp.Client, c config.Config) error {
	ok, mechs := cl.Extension("AUTH")
	if !ok {
		return errors.New("the server does not offer a login, but a user is set")
	}
	var a smtp.Auth = plainAuth{c.NotifySMTPUser, c.NotifySMTPPassword}
	if !strings.Contains(" "+strings.ToUpper(mechs)+" ", " PLAIN ") {
		if !strings.Contains(" "+strings.ToUpper(mechs)+" ", " LOGIN ") {
			return errors.New("the server offers no login method TasmoFleet supports (PLAIN, LOGIN): " + mechs)
		}
		a = loginAuth{c.NotifySMTPUser, c.NotifySMTPPassword}
	}
	if err := cl.Auth(a); err != nil {
		return errors.New("login refused: " + smtpReason(err))
	}
	return nil
}

type plainAuth struct{ user, pass string }

func (a plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}
func (plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge")
	}
	return nil, nil
}

type loginAuth struct{ user, pass string }

func (loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) { return "LOGIN", nil, nil }
func (a loginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(challenge))) {
	case "username:":
		return []byte(a.user), nil
	case "password:":
		return []byte(a.pass), nil
	}
	return nil, errors.New("unexpected server challenge")
}

// smtpReason is the server's own reply text, one line.
func smtpReason(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// buildMessage writes the e-mail. Header values are single-line (the title and the addresses
// cannot start a header of their own), the body is quoted-printable UTF-8.
func buildMessage(from *mail.Address, to []*mail.Address, ev Event) []byte {
	var b bytes.Buffer
	oneLine := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	var rcpt []string
	for _, a := range to {
		rcpt = append(rcpt, a.String())
	}
	fmt.Fprintf(&b, "From: %s\r\n", from.String())
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(rcpt, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", oneLine(ev.Title)))
	fmt.Fprintf(&b, "Date: %s\r\n", ev.Time.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\nAuto-Submitted: auto-generated\r\n\r\n")
	body := ev.Text + "\r\n"
	if ev.URL != "" {
		body += "\r\n" + ev.URL + "\r\n"
	}
	body += "\r\n-- \r\nTasmoFleet\r\n"
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(body)) // a bytes.Buffer does not fail
	_ = qp.Close()
	return b.Bytes()
}
