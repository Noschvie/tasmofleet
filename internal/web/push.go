package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/hellwand/tasmofleet/internal/auth"
	"github.com/hellwand/tasmofleet/internal/httpx"
	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/notify"
	"github.com/hellwand/tasmofleet/internal/store"
)

// PushService is the Web Push channel (*notify.Push, #610). Optional: without it the routes below do
// not exist.
type PushService interface {
	// PublicKey is the VAPID public key a browser subscribes with (applicationServerKey).
	PublicKey() string
	// Subscribe checks and stores a subscription (notify.ErrBadSubscription when it is not valid).
	Subscribe(ctx context.Context, s store.PushSubscription) error
	Unsubscribe(ctx context.Context, endpoint string) (bool, error)
	// List is every subscription of every admin, without the endpoint's path (the Settings list, #611).
	List(ctx context.Context) ([]notify.PushInfo, error)
	// Remove deletes the subscription with the given id, whoever owns it; false when there is none.
	Remove(ctx context.Context, id string) (notify.PushInfo, bool, error)
}

// maxPushRequest bounds the JSON of a subscription (an endpoint of at most 2 KB and two keys).
const maxPushRequest = 8 << 10

// registerPush adds the server side of Web Push for admins (notifications are theirs, #540): the public
// key, subscribe and unsubscribe (push.js, #611), and what the Settings list acts with: removing any
// subscription (a confirmation page without scripts) and a test push to one. The unsafe methods pass the
// CSRF check like every other request (origin and token). The test needs the dispatcher as well.
func (h *handler) registerPush(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	if h.d.Push == nil {
		return
	}
	route("GET "+sysPath+"/push/key", adm, h.pushKey)
	route("POST "+sysPath+"/push/subscription", adm, h.pushSubscribe)
	route("DELETE "+sysPath+"/push/subscription", adm, h.pushUnsubscribe)
	route("GET "+sysPath+"/push/remove/{id}", adm, h.pushRemoveConfirm)
	route("POST "+sysPath+"/push/remove/{id}", adm, h.pushRemove)
	if h.d.Notify != nil {
		route("POST "+sysPath+"/push/test/{id}", adm, h.pushTest)
	}
}

func writePushJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client went away; nothing to do
}

func (h *handler) pushKey(w http.ResponseWriter, _ *http.Request) {
	writePushJSON(w, http.StatusOK, map[string]string{"key": h.d.Push.PublicKey()})
}

// pushRequest is PushSubscription.toJSON() of the browser (expirationTime is ignored).
type pushRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func readPushRequest(w http.ResponseWriter, r *http.Request) (pushRequest, bool) {
	var req pushRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPushRequest)).Decode(&req); err != nil {
		writePushJSON(w, http.StatusBadRequest, map[string]string{"error": "the body is not a push subscription (JSON)"})
		return req, false
	}
	return req, true
}

func (h *handler) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	req, ok := readPushRequest(w, r)
	if !ok {
		return
	}
	p, _ := auth.FromContext(r.Context())
	err := h.d.Push.Subscribe(r.Context(), store.PushSubscription{Endpoint: req.Endpoint, P256dh: req.Keys.P256dh, Auth: req.Keys.Auth,
		OwnerMethod: p.Method, OwnerSubject: p.Subject, Label: pushLabel(r.UserAgent())})
	switch {
	case errors.Is(err, notify.ErrBadSubscription):
		h.recordPush(r, "push.subscribe", req.Endpoint, "refused: "+err.Error())
		writePushJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case err != nil:
		h.log.Warn("web push: storing a subscription failed", "err", err)
		writePushJSON(w, http.StatusInternalServerError, map[string]string{"error": "the subscription could not be stored"})
	default:
		h.recordPush(r, "push.subscribe", req.Endpoint, "ok")
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *handler) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	req, ok := readPushRequest(w, r)
	if !ok {
		return
	}
	found, err := h.d.Push.Unsubscribe(r.Context(), req.Endpoint)
	switch {
	case err != nil:
		h.log.Warn("web push: removing a subscription failed", "err", err)
		writePushJSON(w, http.StatusInternalServerError, map[string]string{"error": "the subscription could not be removed"})
	case !found:
		writePushJSON(w, http.StatusNotFound, map[string]string{"error": "no such subscription"})
	default:
		h.recordPush(r, "push.unsubscribe", req.Endpoint, "ok")
		w.WriteHeader(http.StatusNoContent)
	}
}

// recordPush audits a subscription change by the push service's host only: the endpoint's path is the
// browser's capability and never reaches the audit log.
func (h *handler) recordPush(r *http.Request, action, endpoint, result string) {
	host := ""
	if u, err := url.Parse(endpoint); err == nil {
		host = u.Host
	}
	h.recordPushHost(r, action, host, result)
}

// recordPushHost is recordPush for a subscription known by its push service's host.
func (h *handler) recordPushHost(r *http.Request, action, host, result string) {
	if h.d.Recorder == nil {
		return
	}
	h.d.Recorder.Record(r.Context(), model.AuditEvent{At: h.d.Clock.Now(), Actor: actor(r), Action: action, Target: "push:" + host,
		Result: result, RemoteAddr: httpx.ClientIP(r.Context()).String()})
}
