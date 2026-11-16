package discovery

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hellwand/tasmofleet/internal/model"
	"github.com/hellwand/tasmofleet/internal/mqttbus"
)

func foundNames(x Explanation, kind string) []string {
	var out []string
	for _, f := range x.Findings {
		if f.Kind == kind {
			out = append(out, f.DeviceName+" by "+f.By)
		}
	}
	return out
}

// Why (#713): the lookup must name the record discovery itself would route a claim to. A device
// announces MAC X at an address that another stored record holds; discovery matches by MAC first,
// so the answer for that address is the record of X, not the one that happens to sit there.
func TestExplainFollowsDiscoverysMatchOrder(t *testing.T) {
	e := newMQTTEnv(t, nil)
	if _, err := e.inv.Create(bg, "t", model.Device{Name: "lamp", Address: "172.31.13.31", MAC: "02:00:5e:d2:2c:14", Topic: "lamp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.inv.Create(bg, "t", model.Device{Name: "shed", Address: "172.31.13.50"}); err != nil {
		t.Fatal(err)
	}
	e.feed(discoveryMsg("02005ED22C14", "172.31.13.50", "lamp-new"))

	for _, q := range []string{"172.31.13.50", "lamp-new", "02:00:5E:D2:2C:14", "02005ed22c14"} {
		x, err := e.mgr.Explain(bg, q)
		if err != nil {
			t.Fatal(err)
		}
		if got := foundNames(x, FindingInDeviceList); len(got) != 1 || got[0] != "lamp by MAC" {
			t.Errorf("%q: device list findings = %v, want [lamp by MAC]", q, got)
		}
	}
}

// Why (#713): after a restart or with discovery off the topic table is empty. The address alone must
// still find the record that holds it, by the last rule discovery uses.
func TestExplainFindsARecordByAddressWithoutTheTopicTable(t *testing.T) {
	e := newMQTTEnv(t, nil)
	if _, err := e.inv.Create(bg, "t", model.Device{Name: "shed", Address: "172.31.13.50"}); err != nil {
		t.Fatal(err)
	}
	x, err := e.mgr.Explain(bg, "172.31.13.50")
	if err != nil {
		t.Fatal(err)
	}
	if got := foundNames(x, FindingInDeviceList); len(got) != 1 || got[0] != "shed by address" {
		t.Fatalf("findings = %v", got)
	}
}

// Why (#713): one IP can stand in the topic table twice (a lease handed on). The answer must not
// depend on map order, or the page shows another record on every reload: newest sighting first.
func TestExplainOrdersTwoTopicsOfOneAddressNewestFirst(t *testing.T) {
	e := newMQTTEnv(t, nil)
	for _, d := range []model.Device{
		{Name: "older", Address: "172.31.13.31", MAC: "02:00:5e:00:00:01", Topic: "older"},
		{Name: "newer", Address: "172.31.13.32", MAC: "02:00:5e:00:00:02", Topic: "newer"},
	} {
		if _, err := e.inv.Create(bg, "t", d); err != nil {
			t.Fatal(err)
		}
	}
	e.feed(discoveryMsg("02005E000001", "172.31.13.60", "older"))
	e.clk.add(time.Minute)
	e.feed(discoveryMsg("02005E000002", "172.31.13.60", "newer"))
	for i := 0; i < 20; i++ { // map order is random: repeat
		x, err := e.mgr.Explain(bg, "172.31.13.60")
		if err != nil {
			t.Fatal(err)
		}
		got := foundNames(x, FindingInDeviceList)
		if len(got) != 2 || got[0] != "newer by MAC" || got[1] != "older by MAC" {
			t.Fatalf("findings = %v, want newest sighting first", got)
		}
	}
}

// Why (#713): a topic that is on the broker without a MAC cannot be added; the lookup must give
// the reason the Add page already keeps for it, not "nothing seen".
func TestExplainGivesTheReasonOfAnUnidentifiedTopic(t *testing.T) {
	e := newMQTTEnv(t, nil)
	e.feed(mqttbus.Message{Kind: mqttbus.KindLWT, Topic: "ghost", Payload: []byte("Offline"), Retained: true})
	x, err := e.mgr.Explain(bg, "ghost")
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Findings) != 1 || x.Findings[0].Kind != FindingUnidentified || !strings.Contains(x.Findings[0].Unidentified.Reason, "offline") {
		t.Fatalf("findings = %+v", x.Findings)
	}
}

// Why (#713): a text that is no MAC, IP or topic must be refused with a sentence the page can show,
// and a long one must not be matched at all.
func TestExplainRefusesUnusableText(t *testing.T) {
	e := newMQTTEnv(t, nil)
	for _, q := range []string{"", "   ", strings.Repeat("a", maxLookupLen+1), "lamp\nlamp"} {
		if _, err := e.mgr.Explain(bg, q); !errors.Is(err, ErrBadLookup) {
			t.Errorf("%q: err = %v, want ErrBadLookup", q, err)
		}
	}
}
