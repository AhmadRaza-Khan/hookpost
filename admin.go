package hookpost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

// AdminOptions configures AdminHandler.
type AdminOptions struct {
	// Owner returns the owner ID for the request, e.g. from the session.
	// Return "" to reject the request with 401. Required.
	Owner func(r *http.Request) string
}

// AdminHandler returns a JSON API for managing endpoints and inspecting,
// retrying and replaying deliveries. Mount it behind your own auth; every
// route is scoped to the owner returned by opts.Owner:
//
//	mux.Handle("/api/webhooks/", http.StripPrefix("/api/webhooks",
//		wh.AdminHandler(hookpost.AdminOptions{
//			Owner: func(r *http.Request) string { return auth.ShopID(r) },
//		})))
//
// Routes:
//
//	GET    /endpoints                     list endpoints
//	POST   /endpoints                     create; the only response with the secret
//	GET    /endpoints/{id}                get
//	PATCH  /endpoints/{id}                update url, description, event_types
//	DELETE /endpoints/{id}                delete
//	POST   /endpoints/{id}/enable         re-enable (e.g. after a 410)
//	POST   /endpoints/{id}/disable        disable
//	POST   /endpoints/{id}/rotate-secret  {"grace_seconds": 86400} -> {"secret": ...}
//	POST   /endpoints/{id}/recover        re-queue dead deliveries (?since=RFC3339)
//	GET    /messages                      ?event_type=&limit=&before=<message id>
//	GET    /messages/{id}/deliveries      delivery status per endpoint
//	POST   /messages/{id}/resend          {"endpoint_id": "..."} replay to one endpoint
//	GET    /deliveries/{id}               get
//	GET    /deliveries/{id}/attempts      attempt log
//	POST   /deliveries/{id}/retry         re-queue now (works on dead deliveries)
//
// List responses are {"data": [...]}; /messages adds "next_before" when
// there may be more. Errors are {"error": "..."}.
func (c *Client) AdminHandler(opts AdminOptions) http.Handler {
	if opts.Owner == nil {
		panic("hookpost: AdminOptions.Owner is required")
	}
	a := &admin{c: c, owner: opts.Owner}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /endpoints", a.listEndpoints)
	mux.HandleFunc("POST /endpoints", a.createEndpoint)
	mux.HandleFunc("GET /endpoints/{id}", a.getEndpoint)
	mux.HandleFunc("PATCH /endpoints/{id}", a.updateEndpoint)
	mux.HandleFunc("DELETE /endpoints/{id}", a.deleteEndpoint)
	mux.HandleFunc("POST /endpoints/{id}/enable", a.setDisabled(false))
	mux.HandleFunc("POST /endpoints/{id}/disable", a.setDisabled(true))
	mux.HandleFunc("POST /endpoints/{id}/rotate-secret", a.rotateSecret)
	mux.HandleFunc("POST /endpoints/{id}/recover", a.recoverEndpoint)
	mux.HandleFunc("GET /messages", a.listMessages)
	mux.HandleFunc("GET /messages/{id}/deliveries", a.listDeliveries)
	mux.HandleFunc("POST /messages/{id}/resend", a.resendMessage)
	mux.HandleFunc("GET /deliveries/{id}", a.getDelivery)
	mux.HandleFunc("GET /deliveries/{id}/attempts", a.listAttempts)
	mux.HandleFunc("POST /deliveries/{id}/retry", a.retryDelivery)
	return a.authed(mux)
}

type admin struct {
	c     *Client
	owner func(*http.Request) string
}

type ownerKey struct{}

func (a *admin) authed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := a.owner(r)
		if o == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ownerKey{}, o)))
	})
}

func ownerOf(r *http.Request) string { return r.Context().Value(ownerKey{}).(string) }

type list[T any] struct {
	Data       []T     `json:"data"`
	NextBefore *string `json:"next_before,omitempty"`
}

func (a *admin) listEndpoints(w http.ResponseWriter, r *http.Request) {
	eps, err := a.c.ListEndpoints(r.Context(), ownerOf(r))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list[Endpoint]{Data: eps})
}

func (a *admin) createEndpoint(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL         string   `json:"url"`
		Description string   `json:"description"`
		EventTypes  []string `json:"event_types"`
	}
	if !decode(w, r, &in, false) {
		return
	}
	ep, err := a.c.CreateEndpoint(r.Context(), EndpointInput{
		OwnerID: ownerOf(r), URL: in.URL, Description: in.Description, EventTypes: in.EventTypes,
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, ep)
}

func (a *admin) getEndpoint(w http.ResponseWriter, r *http.Request) {
	ep, err := a.c.GetEndpoint(r.Context(), ownerOf(r), r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ep)
}

func (a *admin) updateEndpoint(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL         *string   `json:"url"`
		Description *string   `json:"description"`
		EventTypes  *[]string `json:"event_types"`
	}
	if !decode(w, r, &in, false) {
		return
	}
	ep, err := a.c.UpdateEndpoint(r.Context(), ownerOf(r), r.PathValue("id"), EndpointUpdate{
		URL: in.URL, Description: in.Description, EventTypes: in.EventTypes,
	})
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ep)
}

func (a *admin) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	if err := a.c.DeleteEndpoint(r.Context(), ownerOf(r), r.PathValue("id")); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *admin) setDisabled(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := a.c.SetEndpointDisabled(r.Context(), ownerOf(r), r.PathValue("id"), disabled); err != nil {
			a.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (a *admin) rotateSecret(w http.ResponseWriter, r *http.Request) {
	in := struct {
		GraceSeconds *int64 `json:"grace_seconds"`
	}{}
	if !decode(w, r, &in, true) {
		return
	}
	grace := DefaultGrace
	if in.GraceSeconds != nil {
		if *in.GraceSeconds < 0 || *in.GraceSeconds > 30*24*3600 {
			writeErr(w, http.StatusUnprocessableEntity, "grace_seconds must be between 0 and 2592000")
			return
		}
		grace = time.Duration(*in.GraceSeconds) * time.Second
	}
	secret, err := a.c.RotateSecret(r.Context(), ownerOf(r), r.PathValue("id"), grace)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret})
}

func (a *admin) recoverEndpoint(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	if s := r.URL.Query().Get("since"); s != "" {
		var err error
		if since, err = time.Parse(time.RFC3339, s); err != nil {
			writeErr(w, http.StatusBadRequest, "since must be an RFC 3339 timestamp")
			return
		}
	}
	n, err := a.c.RecoverEndpoint(r.Context(), ownerOf(r), r.PathValue("id"), since)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"requeued": n})
}

func (a *admin) listMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mq := MessageQuery{EventType: q.Get("event_type"), Before: q.Get("before")}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		mq.Limit = n
	}
	msgs, err := a.c.ListMessages(r.Context(), ownerOf(r), mq)
	if err != nil {
		a.fail(w, err)
		return
	}
	out := list[MessageRecord]{Data: msgs}
	if len(msgs) == clampLimit(mq.Limit) {
		out.NextBefore = &msgs[len(msgs)-1].ID
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *admin) listDeliveries(w http.ResponseWriter, r *http.Request) {
	ds, err := a.c.ListDeliveries(r.Context(), ownerOf(r), r.PathValue("id"))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list[Delivery]{Data: ds})
}

func (a *admin) resendMessage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		EndpointID string `json:"endpoint_id"`
	}
	if !decode(w, r, &in, false) {
		return
	}
	if in.EndpointID == "" {
		writeErr(w, http.StatusUnprocessableEntity, "endpoint_id is required")
		return
	}
	d, err := a.c.ResendMessage(r.Context(), ownerOf(r), r.PathValue("id"), in.EndpointID)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, d)
}

func (a *admin) getDelivery(w http.ResponseWriter, r *http.Request) {
	id, ok := deliveryID(w, r)
	if !ok {
		return
	}
	d, err := a.c.GetDelivery(r.Context(), ownerOf(r), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (a *admin) listAttempts(w http.ResponseWriter, r *http.Request) {
	id, ok := deliveryID(w, r)
	if !ok {
		return
	}
	as, err := a.c.ListAttempts(r.Context(), ownerOf(r), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list[Attempt]{Data: as})
}

func (a *admin) retryDelivery(w http.ResponseWriter, r *http.Request) {
	id, ok := deliveryID(w, r)
	if !ok {
		return
	}
	if err := a.c.RetryDelivery(r.Context(), ownerOf(r), id); err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func deliveryID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return 0, false
	}
	return id, true
}

// decode reads a JSON body of at most 64 KiB into v. An empty body is
// accepted only when optional is set.
func decode(w http.ResponseWriter, r *http.Request, v any, optional bool) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v)
	if err == nil || (optional && errors.Is(err, io.EOF)) {
		return true
	}
	writeErr(w, http.StatusBadRequest, "invalid JSON body")
	return false
}

// fail maps library errors to HTTP status codes; anything unexpected is
// logged and reported as a 500 without details.
func (a *admin) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeErr(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrInvalidURL), errors.Is(err, ErrNoOwner):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
	default:
		a.c.log.Error("hookpost: admin", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
