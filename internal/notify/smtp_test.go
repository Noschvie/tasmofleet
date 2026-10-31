package notify

import (
	"bytes"
	"io"
	"mime/quotedprintable"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/config"
)

func smtpConfig(port int, sec string) config.Config {
	c := baseConfig()
	c.NotifySMTP, c.NotifySMTPHost, c.NotifySMTPPort, c.NotifySMTPSecurity = true, "localhost", port, sec
	c.NotifySMTPFrom, c.NotifySMTPTo = "TasmoFleet <tasmofleet@example.com>", "admin@example.com, second@example.com"
	return c
}

// Why: the three security modes are three different conversations (plain, TLS from the first
// byte, upgrade after EHLO). One that silently sends in the clear when TLS was asked for would be
// worse than a failure, so each mode runs against a server that insists on it.
func TestSMTPDeliversInEveryMode(t *testing.T) {
	for _, tc := range []struct {
		sec              string
		implicit, offers bool
	}{
		{config.NotifySecNone, false, false},
		{config.NotifySecTLS, true, false},
		{config.NotifySecStartTLS, false, true},
	} {
		srv, tlsConf := newFakeSMTP(t, tc.implicit, tc.offers)
		c := smtpConfig(srv.port(), tc.sec)
		c.NotifySMTPUser, c.NotifySMTPPassword = "mailer", "p4ss-word"
		var log bytes.Buffer
		d := newDispatcher(&c, &log)
		d.o.TLS = tlsConf
		a, err := d.Test(t.Context(), Email, c)
		if err != nil || !a.OK {
			t.Fatalf("%s: err=%v attempt=%+v", tc.sec, err, a)
		}
		if a.Detail != "accepted by localhost" {
			t.Errorf("%s: detail = %q", tc.sec, a.Detail)
		}
		srv.mu.Lock()
		auth, data, nTo := srv.auth, srv.data, len(srv.to)
		srv.mu.Unlock()
		if auth != "\x00mailer\x00p4ss-word" {
			t.Errorf("%s: login = %q", tc.sec, auth)
		}
		if nTo != 2 {
			t.Errorf("%s: %d recipients, want 2", tc.sec, nTo)
		}
		head, body, _ := strings.Cut(data, "\r\n\r\n")
		decoded, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
		all := head + "\n" + string(decoded)
		for _, want := range []string{"Subject: TasmoFleet test notification", "From: \"TasmoFleet\" <tasmofleet@example.com>", "second@example.com", "this channel works", "?tab=notifications"} {
			if !strings.Contains(all, want) {
				t.Errorf("%s: message lacks %q:\n%s", tc.sec, want, data)
			}
		}
		if strings.Contains(a.Detail+log.String(), "p4ss-word") {
			t.Errorf("%s: the password reached the status or the log", tc.sec)
		}
	}
}

// Why: with STARTTLS chosen, a server that does not offer it must fail the delivery. Falling
// back to plain text would send the login in the clear.
func TestSMTPStartTLSIsNeverSkipped(t *testing.T) {
	srv, tlsConf := newFakeSMTP(t, false, false)
	c := smtpConfig(srv.port(), config.NotifySecStartTLS)
	c.NotifySMTPUser, c.NotifySMTPPassword = "mailer", "p4ss-word"
	d := newDispatcher(&c, nil)
	d.o.TLS = tlsConf
	a, _ := d.Test(t.Context(), Email, c)
	srv.mu.Lock()
	auth := srv.auth
	srv.mu.Unlock()
	if a.OK || auth != "" || !strings.Contains(a.Detail, "STARTTLS") {
		t.Fatalf("attempt = %+v, login sent = %q; want a failure naming STARTTLS and no login", a, auth)
	}
}

// Why: a rejected recipient is the typical misconfiguration; the status must say who and why so
// the person can fix it, and the failure must be recorded as the last delivery.
func TestSMTPRefusalIsRecordedWithTheServersReason(t *testing.T) {
	srv, _ := newFakeSMTP(t, false, false)
	srv.refuseTo = "second@example.com"
	c := smtpConfig(srv.port(), config.NotifySecNone)
	d := newDispatcher(&c, nil)
	d.Emit(SnapshotFailed(c, "disk full", t0))
	d.Wait()
	st := d.Status(Email).Delivery
	if st.OK || !strings.Contains(st.Detail, "second@example.com") || !strings.Contains(st.Detail, "no such mailbox") || st.Context != "database snapshot" {
		t.Fatalf("delivery = %+v", st)
	}
}

// Why: a title comes from device names and error texts; a line break in it must not be able to
// start another header (header injection) in the mail.
func TestSMTPSubjectCannotInjectHeaders(t *testing.T) {
	srv, _ := newFakeSMTP(t, false, false)
	c := smtpConfig(srv.port(), config.NotifySecNone)
	d := newDispatcher(&c, nil)
	ev := TestEvent(c, t0)
	ev.Title = "hello\r\nBcc: evil@example.com"
	a := d.deliver(t.Context(), Email, c, ev, true)
	srv.mu.Lock()
	data := srv.data
	srv.mu.Unlock()
	head, _, _ := strings.Cut(data, "\r\n\r\n")
	if !a.OK || strings.Contains(head, "\r\nBcc:") {
		t.Fatalf("ok=%v, header block:\n%s", a.OK, head)
	}
}
