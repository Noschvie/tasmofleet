package app

import (
	"context"
	"net"
	"net/netip"
	"slices"

	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// setupService backs the first-run setup page.
type setupService struct {
	sys   *sysconfig.Service
	addrs func() ([]net.Addr, error)
}

func newSetupService(sys *sysconfig.Service) *setupService {
	return &setupService{sys: sys, addrs: net.InterfaceAddrs}
}

// Done: the setup was finished or skipped, or the installation already has device
// networks (an upgrade from environment-based configuration must not be pushed into it).
func (s *setupService) Done() bool { return s.sys.SetupDone() || s.NetworksConfigured() }

func (s *setupService) Finish(ctx context.Context, actor string) error {
	return s.sys.MarkSetupDone(ctx, actor)
}

func (s *setupService) NetworksConfigured() bool { return len(s.sys.Snapshot().DeviceNetworks) > 0 }

// Suggestions lists the private IPv4 subnets of the machine's own interfaces. The
// setup shows them as a proposal; nothing is configured until the admin saves.
func (s *setupService) Suggestions() []netip.Prefix {
	addrs, err := s.addrs()
	if err != nil {
		return nil
	}
	return privateSubnets(addrs)
}

// privateSubnets turns interface addresses into the private IPv4 networks they
// sit in (never loopback, link-local, public or IPv6 ones), deduplicated and
// no larger than a /16 so a mistyped mask cannot suggest half the internet.
func privateSubnets(addrs []net.Addr) []netip.Prefix {
	var out []netip.Prefix
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		ones, _ := ipn.Mask.Size()
		if !ip.Is4() || !ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		if ones < 16 {
			ones = 16
		}
		p := netip.PrefixFrom(ip, ones).Masked()
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}
