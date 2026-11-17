package modsite

import (
	"strings"
	"testing"
	"time"

	"github.com/jay-day/rmgk-lobby/internal/protocol"
	"github.com/jay-day/rmgk-lobby/internal/ratelimit"
)

type rateReply struct {
	Attached         bool
	ExemptLoopback   bool
	Kinds            []rateKindDTO
	BanStrikes       int
	BanWindowSeconds int64
	BanSeconds       int64
	Total            int
	More             bool
	Records          []rateRecordDTO
}

func TestRateLimitsEndpoint(t *testing.T) {
	limits := ratelimit.DefaultActionLimits()
	limits.Rates[ratelimit.ActionRoomCreate] = ratelimit.Rate{Count: 1, Window: time.Hour}
	ts := newTestSite(t, siteOpts{limits: &limits})

	// a player trips the room_create limit
	const ip = "203.0.113.77"
	ace := ts.connect("Ace", ip)
	room := protocol.RoomCreateRequest{Name: "Remix", Rom: protocol.RomInfo{Name: "Smash Remix"}, MaxPlayers: 2}
	ace.send(protocol.TypeRoomCreate, room)
	ace.await(protocol.TypeRoomCreated)
	ace.send(protocol.TypeRoomLeave, nil)
	ace.send(protocol.TypeRoomCreate, room)
	if _, ok := ace.await(protocol.TypeRoomCreateFail); !ok {
		t.Fatal("no refusal")
	}

	if w := ts.serve(req{method: "GET", path: "/api/admin/ratelimits", ip: modIP}); !isBare404(w) {
		t.Fatalf("signed out = %d", w.Code)
	}
	cookie := ts.login(modIP)
	var rep rateReply
	if code := ts.api("GET", "/api/admin/ratelimits", cookie, nil, &rep); code != 200 {
		t.Fatalf("GET = %d", code)
	}
	if !rep.Attached || !rep.ExemptLoopback || len(rep.Kinds) != 6 || rep.BanStrikes != 40 || rep.BanSeconds != 3600 ||
		rep.Total != 1 || len(rep.Records) != 1 {
		t.Fatalf("report = %+v", rep)
	}
	r := rep.Records[0]
	if r.IP != ip || r.Refused["room_create"] != 1 || r.TotalRefused != 1 || r.Allowed["room_create"] != 1 ||
		len(r.Usernames) != 1 || r.Usernames[0] != "Ace" || len(r.Online) != 1 || r.Ban != nil || r.LastTrip == 0 {
		t.Fatalf("record = %+v", r)
	}

	// a ban through the Sanctions endpoint shows on the record
	if code := ts.api("POST", "/api/admin/sanctions/ban", cookie, map[string]string{"ip": ip, "duration": "1h"}, nil); code != 200 {
		t.Fatalf("ban = %d", code)
	}
	ace.awaitClosed()
	ts.api("GET", "/api/admin/ratelimits?ip="+ip, cookie, nil, &rep)
	if len(rep.Records) != 1 || rep.Records[0].Ban == nil || rep.Records[0].Ban.Until == 0 {
		t.Fatalf("after the ban: %+v", rep.Records)
	}
	if code := ts.api("GET", "/api/admin/ratelimits?ip=nope", cookie, nil, nil); code != 400 {
		t.Fatalf("bad ip = %d", code)
	}

	// forget, audited; a second time there's nothing
	if code := ts.api("POST", "/api/admin/ratelimits/forget", cookie, map[string]string{"ip": ip}, nil); code != 200 {
		t.Fatalf("forget = %d", code)
	}
	if code := ts.api("POST", "/api/admin/ratelimits/forget", cookie, map[string]string{"ip": ip}, nil); code != 404 {
		t.Fatalf("forget again = %d", code)
	}
	ts.api("GET", "/api/admin/ratelimits", cookie, nil, &rep)
	if len(rep.Records) != 0 {
		t.Fatalf("records after forget: %+v", rep.Records)
	}
	rows, _, err := ts.log.AuditLog(t.Context(), 0, 5)
	if err != nil || len(rows) == 0 || rows[0].Action != "forget-rate-limit" || !strings.Contains(rows[0].Detail, ip) {
		t.Fatalf("audit = %+v (%v)", rows, err)
	}
}

func TestRateLimitsWithLoggingOff(t *testing.T) {
	limits := ratelimit.DefaultActionLimits()
	limits.Rates[ratelimit.ActionRoomJoin] = ratelimit.Rate{Count: 1, Window: time.Hour}
	ts := newTestSite(t, siteOpts{noLog: true, limits: &limits})
	p := ts.connect("Hop", "198.51.100.61")
	for i := 0; i < 2; i++ { // the second is refused: the room doesn't exist, but the attempt counts
		p.send(protocol.TypeRoomJoin, protocol.RoomJoinRequest{RoomID: 12345})
		p.await(protocol.TypeRoomJoinFail)
	}
	ts.hub.SaveRateLimits() // nothing to save to; must not fail
	var rep rateReply
	if code := ts.api("GET", "/api/admin/ratelimits", ts.login(modIP), nil, &rep); code != 200 ||
		len(rep.Records) != 1 || rep.Records[0].Refused["room_join"] != 1 {
		t.Fatalf("report with logging off = %d %+v", code, rep)
	}
}

func TestRateLimitsWithoutALimiter(t *testing.T) {
	ts := newTestSite(t)
	cookie := ts.login(modIP)
	if code := ts.api("GET", "/api/admin/ratelimits", cookie, nil, nil); code != 503 {
		t.Fatalf("GET without a limiter = %d, want 503", code)
	}
	if code := ts.api("POST", "/api/admin/ratelimits/forget", cookie, map[string]string{"ip": "192.0.2.1"}, nil); code != 503 {
		t.Fatalf("forget without a limiter = %d, want 503", code)
	}
}
