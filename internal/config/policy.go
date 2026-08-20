package config

import "net/netip"

// BackendNetworkPolicy builds the destination check applied to every
// backend dial (PLAN §16). The returned function reports whether an IP
// address may be dialed. It never performs DNS: hosts are resolved by the
// caller and the selected address is checked per connection.
func (s *Security) BackendNetworkPolicy() func(netip.Addr) bool {
	switch s.BackendNetwork.Mode {
	case "any":
		return func(netip.Addr) bool { return true }
	case "allowed-cidrs":
		prefixes := make([]netip.Prefix, 0, len(s.BackendNetwork.CIDRs))
		for _, c := range s.BackendNetwork.CIDRs {
			if p, err := netip.ParsePrefix(c); err == nil {
				prefixes = append(prefixes, p)
			}
		}
		return func(addr netip.Addr) bool {
			for _, p := range prefixes {
				if p.Contains(addr) {
					return true
				}
			}
			return false
		}
	default: // loopback-only (validated default)
		return func(addr netip.Addr) bool { return addr.IsLoopback() }
	}
}
