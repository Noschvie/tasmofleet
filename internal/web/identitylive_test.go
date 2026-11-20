package web

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// Why (#1003): the Identity card showed the old OTA URL after the background identity read had stored the new one,
// until the page was loaded again: the live refresh swapped the header but not the card. The live answer now carries
// the card out of band when what it shows changed. An unchanged card is left alone, so a "Refresh identity" notice in it
// and the focus on its button survive the refreshes that every state change of the device asks for. The card names what
// it shows in a hidden field the live region sends back, so the card a Refresh identity just swapped in counts too.
func TestTheLiveRefreshSwapsTheIdentityCardOnlyWhenItChanged(t *testing.T) {
	e := setup(t)
	_, page := e.do(t, "GET", "/devices/1", "operator", nil)
	sig := regexp.MustCompile(`<input type="hidden" id="identsig" name="ident" value="([0-9a-f]+)"`).FindStringSubmatch(page)
	if sig == nil {
		t.Fatalf("the Identity card does not name what it shows:\n%s", shortDump(page))
	}
	if !strings.Contains(page, `hx-include="#identsig"`) {
		t.Fatalf("the live region does not send the Identity card's signature back:\n%s", shortDump(page))
	}
	live := "/devices/1/live?ident=" + url.QueryEscape(sig[1])
	_, same := e.do(t, "GET", live, "operator", nil)
	if strings.Contains(same, `id="identity"`) {
		t.Error("the unchanged Identity card was sent again: it would wipe a Refresh identity notice at every state change")
	}
	if !strings.Contains(same, `id="devlive"`) || !strings.Contains(same, `id="devhd"`) {
		t.Error("the rest of the live answer must still come")
	}
	e.inv.mu.Lock()
	d := e.inv.devs[1]
	d.OTAURL = "http://ota.example/tasmota-16.0.0.5.bin.gz"
	d.Identity.Version = "16.0.0.5"
	e.inv.devs[1] = d
	e.inv.mu.Unlock()
	_, changed := e.do(t, "GET", live, "operator", nil)
	card := regexp.MustCompile(`(?s)<section id="identity"[^>]*hx-swap-oob="true"[^>]*>.*?</section>`).FindString(changed)
	if card == "" {
		t.Fatalf("a background identity read that changed the card did not reach it:\n%s", shortDump(changed))
	}
	if !strings.Contains(strings.ReplaceAll(card, "<wbr>", ""), "http://ota.example/tasmota-16.0.0.5.bin.gz") {
		t.Errorf("the swapped card does not show the new OTA URL:\n%s", card)
	}
	if strings.Contains(card, `value="`+sig[1]+`"`) {
		t.Error("the swapped card still names the old signature")
	}
}
