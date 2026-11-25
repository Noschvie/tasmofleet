package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/notify"
	"github.com/hellwand/tasmofleet/internal/web/ui"
)

// The Web Push card of Settings -> Notifications (#611): this browser's state (push.js draws it), and the
// list of every subscribed browser with Send test and Remove. The list is the server's; the state of this
// browser is the browser's own and is told by scripts. Everything the list does goes through the server and
// needs neither HTTPS nor push support in the browser at hand.

// sysPushRow is one subscribed browser as the list shows it. The page never carries the endpoint: the row
// is named by its id (notify.SubscriptionID), and the address of the push service is shown as its host.
type sysPushRow struct {
	ID, Label, Owner, Host string
	Created, LastOK        time.Time
	LastError              string
	Result                 string // the outcome of a test pressed without scripts (the page was posted)
	TestURL, RemoveURL     string
	RemoveText             string
}

// sysPushList is the card's data. On is false without a push channel (no VAPID key): the card is not drawn.
type sysPushList struct {
	On   bool
	Key  string // the VAPID public key, the browser's applicationServerKey (not a secret)
	Base string // the base path: push.js finds the worker and the subscribe route from it
	Rows []sysPushRow
	Err  string // the list could not be read
}

// pushLabel names the browser and system of a subscribing request from its User-Agent, through a short fixed
// table: the label is one of these words or "Browser", never the header itself (it is shown on a page, and
// the header is the client's to choose).
func pushLabel(ua string) string {
	browser := ""
	for _, b := range []struct{ token, name string }{
		{"Edg/", "Edge"}, {"EdgA/", "Edge"}, {"EdgiOS/", "Edge"}, {"OPR/", "Opera"}, {"SamsungBrowser/", "Samsung Internet"},
		{"Firefox/", "Firefox"}, {"FxiOS/", "Firefox"}, {"Chromium/", "Chromium"}, {"CriOS/", "Chrome"}, {"Chrome/", "Chrome"},
	} {
		if strings.Contains(ua, b.token) {
			browser = b.name
			break
		}
	}
	if browser == "" && strings.Contains(ua, "Version/") && strings.Contains(ua, "Safari/") {
		browser = "Safari"
	}
	system := ""
	for _, s := range []struct{ token, name string }{
		{"Android", "Android"}, {"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Windows", "Windows"}, {"Macintosh", "macOS"},
		{"CrOS", "ChromeOS"}, {"Linux", "Linux"}, {"X11", "Linux"},
	} {
		if strings.Contains(ua, s.token) {
			system = s.name
			break
		}
	}
	switch {
	case browser == "" && system == "":
		return "Browser"
	case browser == "":
		return "Browser on " + system
	case system == "":
		return browser
	}
	return browser + " on " + system
}

// sysPushOf lists the subscribed browsers for the page: the signed-in admin's first, then the others, the
// newest first.
func (h *handler) sysPushOf(r *http.Request, base string) sysPushList {
	if h.d.Push == nil {
		return sysPushList{}
	}
	out := sysPushList{On: true, Key: h.d.Push.PublicKey(), Base: base}
	infos, err := h.d.Push.List(r.Context())
	if err != nil {
		h.log.Warn("web push: listing the subscriptions failed", "err", err)
		out.Err = "The subscribed browsers could not be read."
		return out
	}
	me, _ := auth.FromContext(r.Context())
	mine := func(i notify.PushInfo) bool { return i.OwnerMethod == me.Method && i.OwnerSubject == me.Subject }
	slices.SortStableFunc(infos, func(a, b notify.PushInfo) int {
		switch {
		case mine(a) != mine(b):
			if mine(a) {
				return -1
			}
			return 1
		case !a.CreatedAt.Equal(b.CreatedAt):
			return b.CreatedAt.Compare(a.CreatedAt)
		}
		return strings.Compare(a.ID, b.ID)
	})
	for _, i := range infos {
		label := i.Label
		if label == "" {
			label = "Browser"
		}
		owner := h.pr.Text(i.OwnerSubject)
		row := sysPushRow{ID: i.ID, Label: label, Owner: owner, Host: i.Host, Created: i.CreatedAt, LastOK: i.LastOKAt, LastError: i.LastError,
			RemoveURL: base + sysPath + "/push/remove/" + i.ID, RemoveText: pushRemoveText(label, owner)}
		if h.d.Notify != nil { // the test goes through the dispatcher
			row.TestURL = base + sysPath + "/push/test/" + i.ID
		}
		out.Rows = append(out.Rows, row)
	}
	return out
}

// pushRemoveText is the dialog text of Remove on a subscribed browser.
func pushRemoveText(label, owner string) string {
	return label + " of " + owner + " stops getting pushes at once. It can subscribe again from this page; nothing else changes."
}

// pushFind is the row with this id, read now.
func (h *handler) pushFind(r *http.Request, id string) (notify.PushInfo, bool, error) {
	infos, err := h.d.Push.List(r.Context())
	if err != nil {
		return notify.PushInfo{}, false, err
	}
	for _, i := range infos {
		if i.ID == id {
			return i, true, nil
		}
	}
	return notify.PushInfo{}, false, nil
}

// pushRemoveConfirm is the confirmation of Remove as a page (no scripts); the dialog is the same words.
func (h *handler) pushRemoveConfirm(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	i, found, err := h.pushFind(r, id)
	switch {
	case err != nil:
		h.fail(w, r, err)
		return
	case !found:
		h.notFound(w, r)
		return
	}
	label := i.Label
	if label == "" {
		label = "Browser"
	}
	h.renderActionConfirm(w, r, "system", actionConfirm{
		Title: "Remove " + label + "?", Text: pushRemoveText(label, h.pr.Text(i.OwnerSubject)),
		Crumbs: []ui.Crumb{sysTabCrumb("notifications")}, Last: "Remove browser",
		Action: sysPath + "/push/remove/" + id, Verb: "Remove", CancelHref: sysTabPath("notifications"), CancelLabel: "Cancel",
	})
}

// pushRemove deletes one subscription, whoever owns it. Every admin may: that is how the pushes to an admin
// who was removed at the identity provider and never signs in again are stopped (web-push.md, W6).
func (h *handler) pushRemove(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	i, found, err := h.d.Push.Remove(r.Context(), r.PathValue("id"))
	switch {
	case err != nil:
		h.log.Warn("web push: removing a subscription failed", "err", err)
		h.recordPushHost(r, "push.remove", "", "failed")
		h.fail(w, r, err)
	case !found:
		h.notFound(w, r)
	default:
		h.recordPushHost(r, "push.remove", i.Host, "ok")
		redirect(w, r, sysTabPath("notifications")+"&ok=push-removed")
	}
}

const pushTestHelp = "It shows within a few seconds; if not, check the system's notification settings."

// pushTest sends the test push to one subscribed browser and answers with the row (scripts) or the page (no
// scripts). The answer says what the server knows: the push service took the message (2xx), not that the
// system showed it.
func (h *handler) pushTest(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	id := r.PathValue("id")
	before, found, err := h.pushFind(r, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	msg := "failed: no such subscription (it was removed meanwhile)"
	if found {
		msg = h.pushTestMessage(r.Context(), id)
		h.recordPushHost(r, "push.test", before.Host, strings.SplitN(msg, ",", 2)[0])
	}
	h.pushAnswer(w, r, id, msg)
}

// pushTestMessage runs the test and words its outcome like the channel tests: "sent, ..." or "failed: ...".
func (h *handler) pushTestMessage(ctx context.Context, id string) string {
	a, err := h.d.Notify.TestPush(ctx, id)
	switch {
	case errors.Is(err, notify.ErrTooSoon):
		return "failed: wait a few seconds between tests"
	case errors.Is(err, notify.ErrPushNotFound):
		return "failed: no such subscription (it was removed meanwhile)"
	case err != nil:
		h.log.Warn("web push: the test could not run", "err", err)
		return "failed: " + err.Error()
	case a.OK:
		return "sent, " + a.Detail + ". " + pushTestHelp
	}
	return "failed: " + a.Detail
}

// pushAnswer shows the result of a test: the swapped row with scripts, the reloaded page without. A row the
// test removed (the push service no longer knew it, or its admin is no longer one) is gone from the answer.
func (h *handler) pushAnswer(w http.ResponseWriter, r *http.Request, id, msg string) {
	list := h.sysPushOf(r, httpBase(r))
	i := slices.IndexFunc(list.Rows, func(row sysPushRow) bool { return row.ID == id })
	if r.Header.Get("HX-Request") != "" {
		if i < 0 {
			h.component(w, r, http.StatusOK, sysPushGone(msg))
			return
		}
		list.Rows[i].Result = msg
		h.component(w, r, http.StatusOK, sysPushRowView(list.Rows[i]))
		return
	}
	p := h.sysData(r, nil, nil)
	p.Active = "notifications"
	if j := slices.IndexFunc(p.Notify.Push.Rows, func(row sysPushRow) bool { return row.ID == id }); j >= 0 {
		p.Notify.Push.Rows[j].Result = msg
	} else {
		p.Error = msg
	}
	h.sysRender(w, r, http.StatusOK, p)
}
