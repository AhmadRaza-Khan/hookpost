package hookpost_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ahmadraza-khan/hookpost"
)

// adminAPI wraps AdminHandler; the owner comes from the X-Owner header.
type adminAPI struct {
	t *testing.T
	h http.Handler
}

func newAdmin(t *testing.T, c *hookpost.Client) *adminAPI {
	return &adminAPI{t, c.AdminHandler(hookpost.AdminOptions{
		Owner: func(r *http.Request) string { return r.Header.Get("X-Owner") },
	})}
}

// do sends a request as owner and decodes the JSON response into out (if non-nil).
func (a *adminAPI) do(owner, method, path string, body any, out any) int {
	a.t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req := httptest.NewRequest(method, path, rd)
	if owner != "" {
		req.Header.Set("X-Owner", owner)
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	// 405s come from http.ServeMux itself and are plain text.
	if rec.Body.Len() > 0 && rec.Code != http.StatusMethodNotAllowed && !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		a.t.Fatalf("%s %s: content-type %q", method, path, rec.Header().Get("Content-Type"))
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			a.t.Fatalf("%s %s: decode %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec.Code
}

func (a *adminAPI) create(owner, url string) hookpost.Endpoint {
	a.t.Helper()
	var ep hookpost.Endpoint
	if code := a.do(owner, "POST", "/endpoints", map[string]any{"url": url}, &ep); code != http.StatusCreated {
		a.t.Fatalf("create: %d", code)
	}
	return ep
}

func TestAdminRequiresOwner(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	api := newAdmin(t, c)
	var e map[string]string
	if code := api.do("", "GET", "/endpoints", nil, &e); code != http.StatusUnauthorized || e["error"] == "" {
		t.Fatalf("got %d %v, want 401", code, e)
	}
}

func TestAdminPanicsWithoutOwnerFunc(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	defer func() {
		if recover() == nil {
			t.Fatal("want panic")
		}
	}()
	c.AdminHandler(hookpost.AdminOptions{})
}

func TestAdminEndpointCRUD(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	api := newAdmin(t, c)

	ep := api.create(testOwner, "https://example.com/a")
	if ep.Secret == "" || ep.ID == "" {
		t.Fatalf("create %+v", ep)
	}

	var got hookpost.Endpoint
	if code := api.do(testOwner, "GET", "/endpoints/"+ep.ID, nil, &got); code != 200 || got.Secret != "" {
		t.Fatalf("get %d %+v", code, got)
	}
	var raw map[string]any
	api.do(testOwner, "GET", "/endpoints/"+ep.ID, nil, &raw)
	if _, ok := raw["secret"]; ok {
		t.Fatal("GET response contains a secret field")
	}

	var lst struct{ Data []hookpost.Endpoint }
	if code := api.do(testOwner, "GET", "/endpoints", nil, &lst); code != 200 || len(lst.Data) != 1 || lst.Data[0].Secret != "" {
		t.Fatalf("list %d %+v", code, lst)
	}

	if code := api.do(testOwner, "PATCH", "/endpoints/"+ep.ID,
		map[string]any{"description": "orders", "event_types": []string{"order.created"}}, &got); code != 200 ||
		got.Description != "orders" || len(got.EventTypes) != 1 || got.URL != ep.URL {
		t.Fatalf("patch %d %+v", code, got)
	}

	if code := api.do(testOwner, "POST", "/endpoints/"+ep.ID+"/disable", nil, nil); code != http.StatusNoContent {
		t.Fatalf("disable %d", code)
	}
	api.do(testOwner, "GET", "/endpoints/"+ep.ID, nil, &got)
	if !got.Disabled {
		t.Fatal("not disabled")
	}
	if code := api.do(testOwner, "POST", "/endpoints/"+ep.ID+"/enable", nil, nil); code != http.StatusNoContent {
		t.Fatalf("enable %d", code)
	}

	var rot map[string]string
	if code := api.do(testOwner, "POST", "/endpoints/"+ep.ID+"/rotate-secret", nil, &rot); code != 200 ||
		!strings.HasPrefix(rot["secret"], "whsec_") || rot["secret"] == ep.Secret {
		t.Fatalf("rotate %d %v", code, rot)
	}
	if code := api.do(testOwner, "POST", "/endpoints/"+ep.ID+"/rotate-secret", map[string]int{"grace_seconds": 60}, &rot); code != 200 {
		t.Fatalf("rotate with grace %d", code)
	}
	if code := api.do(testOwner, "POST", "/endpoints/"+ep.ID+"/rotate-secret", map[string]int{"grace_seconds": -1}, nil); code != 422 {
		t.Fatalf("negative grace %d, want 422", code)
	}

	if code := api.do(testOwner, "DELETE", "/endpoints/"+ep.ID, nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete %d", code)
	}
	if code := api.do(testOwner, "GET", "/endpoints/"+ep.ID, nil, nil); code != 404 {
		t.Fatalf("get after delete %d", code)
	}
}

func TestAdminBadInput(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	api := newAdmin(t, c)
	cases := []struct {
		method, path string
		body         any
		want         int
	}{
		{"POST", "/endpoints", "{not json", 400},
		{"POST", "/endpoints", "", 400},
		{"POST", "/endpoints", map[string]string{"url": "ftp://x"}, 422},
		{"GET", "/messages?limit=abc", nil, 400},
		{"GET", "/deliveries/abc", nil, 404},
		{"POST", "/endpoints/ep_x/recover?since=yesterday", nil, 400},
		{"POST", "/messages/msg_x/resend", map[string]string{}, 422},
		{"PUT", "/endpoints", nil, 405},
	}
	for _, tc := range cases {
		if code := api.do(testOwner, tc.method, tc.path, tc.body, nil); code != tc.want {
			t.Errorf("%s %s: %d, want %d", tc.method, tc.path, code, tc.want)
		}
	}
}

// Owner B must get 404 for everything that belongs to owner A.
func TestAdminOwnerIsolation(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	api := newAdmin(t, c)
	ep := api.create(testOwner, "https://example.com/a")
	msgID := send(t, c, 1)
	ds, err := c.ListDeliveries(context.Background(), testOwner, msgID)
	if err != nil || len(ds) != 1 {
		t.Fatalf("deliveries %v %v", ds, err)
	}
	did := func(s string) string { return strings.Replace(s, "{d}", itoa(ds[0].ID), 1) }

	const other = "shop_2"
	for _, r := range []struct{ method, path string }{
		{"GET", "/endpoints/" + ep.ID},
		{"PATCH", "/endpoints/" + ep.ID},
		{"DELETE", "/endpoints/" + ep.ID},
		{"POST", "/endpoints/" + ep.ID + "/disable"},
		{"POST", "/endpoints/" + ep.ID + "/enable"},
		{"POST", "/endpoints/" + ep.ID + "/rotate-secret"},
		{"POST", "/endpoints/" + ep.ID + "/recover"},
		{"GET", "/messages/" + msgID + "/deliveries"},
		{"GET", did("/deliveries/{d}")},
		{"GET", did("/deliveries/{d}/attempts")},
		{"POST", did("/deliveries/{d}/retry")},
	} {
		body := any(map[string]string{})
		if code := api.do(other, r.method, r.path, body, nil); code != 404 {
			t.Errorf("%s %s as other owner: %d, want 404", r.method, r.path, code)
		}
	}
	// resend: other owner's message to own endpoint, and own message to other's endpoint
	otherEp := api.create(other, "https://example.com/b")
	if code := api.do(other, "POST", "/messages/"+msgID+"/resend", map[string]string{"endpoint_id": otherEp.ID}, nil); code != 404 {
		t.Errorf("resend foreign message: %d", code)
	}
	if code := api.do(testOwner, "POST", "/messages/"+msgID+"/resend", map[string]string{"endpoint_id": otherEp.ID}, nil); code != 404 {
		t.Errorf("resend to foreign endpoint: %d", code)
	}

	var lst struct{ Data []json.RawMessage }
	api.do(other, "GET", "/messages", nil, &lst)
	if len(lst.Data) != 0 {
		t.Errorf("other owner sees %d messages", len(lst.Data))
	}
	got, err := c.GetEndpoint(context.Background(), testOwner, ep.ID)
	if err != nil || got.Disabled {
		t.Fatalf("endpoint changed: %+v %v", got, err)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestAdminMessagesPagination(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	api := newAdmin(t, c)
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, send(t, c, i))
	}
	if _, err := c.Send(context.Background(), testPool,
		hookpost.Message{OwnerID: testOwner, EventType: "invoice.paid"}); err != nil {
		t.Fatal(err)
	}

	type page struct {
		Data       []hookpost.MessageRecord
		NextBefore *string `json:"next_before"`
	}
	var seen []string
	path := "/messages?event_type=order.created&limit=2"
	for i := 0; i < 5; i++ {
		var p page
		if code := api.do(testOwner, "GET", path, nil, &p); code != 200 {
			t.Fatalf("page %d: %d", i, code)
		}
		for _, m := range p.Data {
			if m.EventType != "order.created" || len(m.Payload) == 0 {
				t.Fatalf("message %+v", m)
			}
			seen = append(seen, m.ID)
		}
		if p.NextBefore == nil {
			break
		}
		path = "/messages?event_type=order.created&limit=2&before=" + *p.NextBefore
	}
	if len(seen) != 5 {
		t.Fatalf("saw %d messages, want 5", len(seen))
	}
	for i := range seen { // newest first
		if seen[i] != ids[len(ids)-1-i] {
			t.Fatalf("order %v, want reverse of %v", seen, ids)
		}
	}
}

func TestAdminRetryDeadDelivery(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{Schedule: []time.Duration{}})
	api := newAdmin(t, c)
	var fail atomic.Bool
	fail.Store(true)
	rc := &receiver{status: func(int) int {
		if fail.Load() {
			return 500
		}
		return 200
	}}
	serve(t, rc)
	run(t, c)

	msgID := send(t, c, 1)
	waitStatus(t, msgID, "dead")

	var ds struct{ Data []hookpost.Delivery }
	if code := api.do(testOwner, "GET", "/messages/"+msgID+"/deliveries", nil, &ds); code != 200 || len(ds.Data) != 1 {
		t.Fatalf("deliveries %d %+v", code, ds)
	}
	d := ds.Data[0]
	if d.Status != "dead" || d.Attempts != 1 || d.LastStatusCode == nil || *d.LastStatusCode != 500 {
		t.Fatalf("delivery %+v", d)
	}

	fail.Store(false)
	if code := api.do(testOwner, "POST", "/deliveries/"+itoa(d.ID)+"/retry", nil, nil); code != http.StatusAccepted {
		t.Fatalf("retry %d", code)
	}
	waitStatus(t, msgID, "succeeded")

	var as struct{ Data []hookpost.Attempt }
	if code := api.do(testOwner, "GET", "/deliveries/"+itoa(d.ID)+"/attempts", nil, &as); code != 200 || len(as.Data) != 2 {
		t.Fatalf("attempts %d %+v", code, as)
	}
	if *as.Data[0].StatusCode != 500 || *as.Data[1].StatusCode != 200 || as.Data[1].Attempt != 2 {
		t.Fatalf("attempt log %+v", as.Data)
	}
}

func TestAdminRecoverEndpoint(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{Schedule: []time.Duration{}})
	api := newAdmin(t, c)
	var fail atomic.Bool
	fail.Store(true)
	rc := &receiver{status: func(int) int {
		if fail.Load() {
			return 503
		}
		return 200
	}}
	ep := serve(t, rc)
	stop := run(t, c)

	a, b := send(t, c, 1), send(t, c, 2)
	waitStatus(t, a, "dead")
	waitStatus(t, b, "dead")
	stop()

	fail.Store(false)
	var out map[string]int
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if code := api.do(testOwner, "POST", "/endpoints/"+ep+"/recover?since="+future, nil, &out); code != 200 || out["requeued"] != 0 {
		t.Fatalf("recover since future: %d %v", code, out)
	}
	if code := api.do(testOwner, "POST", "/endpoints/"+ep+"/recover", nil, &out); code != 200 || out["requeued"] != 2 {
		t.Fatalf("recover: %d %v", code, out)
	}
	if d := delivery(t, a); d.status != "pending" || d.attempts != 0 {
		t.Fatalf("recovered delivery %+v", d)
	}
	run(t, c)
	waitStatus(t, a, "succeeded")
	waitStatus(t, b, "succeeded")
}

func TestAdminResendToNewEndpoint(t *testing.T) {
	c := newDispatchClient(t, hookpost.Config{})
	api := newAdmin(t, c)
	msgID := send(t, c, map[string]int{"id": 9}) // no endpoints yet
	rc := &receiver{}
	ep := serve(t, rc)
	run(t, c)

	var d hookpost.Delivery
	if code := api.do(testOwner, "POST", "/messages/"+msgID+"/resend", map[string]string{"endpoint_id": ep}, &d); code != http.StatusAccepted ||
		d.MessageID != msgID || d.EndpointID != ep {
		t.Fatalf("resend %d %+v", code, d)
	}
	waitStatus(t, msgID, "succeeded")

	// resending again re-queues the same delivery row
	if code := api.do(testOwner, "POST", "/messages/"+msgID+"/resend", map[string]string{"endpoint_id": ep}, &d); code != http.StatusAccepted {
		t.Fatalf("second resend %d", code)
	}
	waitFor(t, "second request", func() bool { return rc.count() == 2 })
	if n := deliveriesFor(t, msgID); n != 1 {
		t.Fatalf("%d delivery rows, want 1", n)
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.headers[0].Get("webhook-id") != msgID || rc.headers[1].Get("webhook-id") != msgID {
		t.Fatal("replay changed webhook-id")
	}
}
