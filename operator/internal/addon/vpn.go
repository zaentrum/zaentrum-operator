package addon

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"

	zaentrumv1alpha1 "github.com/zaentrum/zaentrum-operator/operator/api/v1alpha1"
)

// The platform VPN (spec.vpn): WireGuard gateways behind a SOCKS5 proxy
// Service, a platform service of its own. A chart assigns a pod to it with
// LabelVPN: VPNAssigned on the pod template of a Deployment or a Job — the
// gateways admit only such pods — and has its program use zaentrum.vpn.proxy.
// The operator, never the chart, renders the kill switch of those pods: a
// NetworkPolicy that lets them connect directly only to the cluster's own
// networks, so that with the VPN down they have no internet rather than a way
// around it (VPNPolicy).
const (
	// LabelVPN on a Deployment's or Job's pod template assigns its pods to the
	// platform VPN. VPNAssigned is its one value; any other is refused.
	LabelVPN    = "zaentrum.io/vpn"
	VPNAssigned = "true"
)

// VPN is the platform's VPN as its addons see it: spec.vpn, checked.
type VPN struct {
	// Proxy is zaentrum.vpn.proxy, the VPN's SOCKS5 proxy; "" when the
	// platform has no VPN.
	Proxy string
	// ClusterCIDRs are the networks a pod assigned to the VPN may still reach
	// directly: each by its network address, as an ipBlock takes it, without
	// repeats, in the order the Zaentrum lists them.
	ClusterCIDRs []string
}

// PlatformVPN checks spec.vpn, as its CRD schema does and more strictly, and
// returns the VPN as the addons see it. The proxy is a socks5:// or socks5h://
// URL with a host — a DNS name, an IPv4 address or an IPv6 one in brackets —
// and a port, and nothing else: no credentials, as every addon is handed the
// URL, and no path, query or fragment. Each cluster CIDR must parse, and none
// may be a /0, which would let a pod assigned to the VPN reach everything
// around it; with a proxy there must be at least one.
func PlatformVPN(spec zaentrumv1alpha1.VPNSpec) (VPN, error) {
	var out VPN
	seen := map[string]bool{}
	for i, c := range spec.ClusterCIDRs {
		p, err := netip.ParsePrefix(c)
		switch {
		case err != nil:
			return VPN{}, fmt.Errorf("spec.vpn.clusterCIDRs[%d]: %q is not a CIDR", i, c)
		case p.Addr().Is4In6():
			return VPN{}, fmt.Errorf("spec.vpn.clusterCIDRs[%d]: %q is an IPv4-mapped IPv6 network: name it as IPv4", i, c)
		case p.Bits() == 0:
			return VPN{}, fmt.Errorf("spec.vpn.clusterCIDRs[%d]: %q is every address, not the cluster's networks: a pod assigned to the VPN could reach the internet around it", i, c)
		}
		if network := p.Masked().String(); !seen[network] {
			seen[network] = true
			out.ClusterCIDRs = append(out.ClusterCIDRs, network)
		}
	}
	if spec.Proxy == "" {
		return out, nil
	}
	if err := checkProxy(spec.Proxy); err != nil {
		shown := spec.Proxy
		if u, perr := url.Parse(shown); perr == nil && u.User != nil {
			shown = u.Redacted()
		}
		return VPN{}, fmt.Errorf("spec.vpn.proxy %q %s", shown, err.Error())
	}
	if len(out.ClusterCIDRs) == 0 {
		return VPN{}, errors.New("spec.vpn.clusterCIDRs is required when spec.vpn.proxy is set: the cluster's pod and service networks, which a pod assigned to the VPN may still reach directly")
	}
	out.Proxy = spec.Proxy
	return out, nil
}

// checkProxy says what is wrong with a proxy URL, or nil.
func checkProxy(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return errors.New("is not a URL")
	}
	if u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return errors.New("is not a socks5:// or socks5h:// URL")
	}
	if u.User != nil {
		return errors.New("must not carry credentials: every addon is handed it")
	}
	host, port := u.Hostname(), u.Port()
	if host == "" {
		return errors.New("needs a host")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || strings.HasPrefix(port, "0") {
		return errors.New("needs a port, 1-65535")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		switch {
		case ip.Zone() != "":
			return errors.New("names an address with a zone")
		case ip.Is6() && !strings.HasPrefix(u.Host, "["):
			return errors.New("names an IPv6 address outside brackets")
		}
	} else if strings.HasPrefix(u.Host, "[") || len(validation.IsDNS1123Subdomain(strings.ToLower(strings.TrimSuffix(host, ".")))) > 0 {
		return errors.New("names a host that is neither a DNS name nor an IP address")
	}
	if raw != u.Scheme+"://"+u.Host {
		return errors.New("must be " + u.Scheme + "://<host>:<port> and nothing else")
	}
	return nil
}

// VPNPolicyName is the NetworkPolicy the operator renders for an addon's pods
// assigned to the VPN. It is under ReservedNamePrefix, which no chart may
// render anything under, like the addon's generated values Secret.
func VPNPolicyName(addon string) string {
	return ReservedNamePrefix + addon + "-vpn"
}

// VPNPodSelector selects an addon's pods assigned to the VPN, and no other:
// LabelAddon, which Decorate sets to the addon's name on every pod template it
// applies, and LabelVPN.
func VPNPodSelector(addon string) map[string]string {
	return map[string]string{LabelAddon: addon, LabelVPN: VPNAssigned}
}

// VPNPolicy renders the kill switch of an addon's pods assigned to the VPN, or
// returns nil when no Deployment or Job of objs assigns its pods to it. The one
// NetworkPolicy, VPNPolicyName, selects VPNPodSelector and is of policy type
// Egress only: those pods may connect directly to clusterCIDRs alone, on every
// port and protocol, and to nothing else. Without a CIDR it has no egress rule
// at all — every connection refused — never a rule without peers, which would
// allow every one.
func VPNPolicy(objs []*unstructured.Unstructured, name, namespace string, clusterCIDRs []string) *unstructured.Unstructured {
	assigned := false
	for _, o := range objs {
		assigned = assigned || assignsVPN(o)
	}
	if !assigned {
		return nil
	}
	selector := map[string]interface{}{}
	for k, v := range VPNPodSelector(name) {
		selector[k] = v
	}
	spec := map[string]interface{}{
		"podSelector": map[string]interface{}{"matchLabels": selector},
		"policyTypes": []interface{}{"Egress"},
	}
	if len(clusterCIDRs) > 0 {
		to := make([]interface{}, 0, len(clusterCIDRs))
		for _, c := range clusterCIDRs {
			to = append(to, map[string]interface{}{"ipBlock": map[string]interface{}{"cidr": c}})
		}
		spec["egress"] = []interface{}{map[string]interface{}{"to": to}}
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata":   map[string]interface{}{"name": VPNPolicyName(name), "namespace": namespace},
		"spec":       spec,
	}}
}

// IsVPNPolicy reports whether an applied object is the addon's VPN kill switch.
func IsVPNPolicy(o *unstructured.Unstructured, addon string) bool {
	return o.GetKind() == "NetworkPolicy" && o.GetName() == VPNPolicyName(addon)
}

// vpnLabel returns the LabelVPN value of a workload's pod template as the chart
// rendered it, which need not be a string, and whether it carries the label.
func vpnLabel(o *unstructured.Unstructured) (interface{}, bool) {
	labels, _, _ := unstructured.NestedFieldNoCopy(o.Object, "spec", "template", "metadata", "labels")
	m, _ := labels.(map[string]interface{})
	v, ok := m[LabelVPN]
	return v, ok
}

// assignsVPN reports whether a rendered Deployment or Job assigns its pods to
// the VPN: its pod template carries LabelVPN: VPNAssigned.
func assignsVPN(o *unstructured.Unstructured) bool {
	if o.GetKind() != "Deployment" && o.GetKind() != "Job" {
		return false
	}
	v, _ := vpnLabel(o)
	s, _ := v.(string)
	return s == VPNAssigned
}

// vpnViolations refuses a pod template's LabelVPN of any value but
// VPNAssigned, and VPNAssigned on a platform without a VPN: its pods would
// have no internet.
func vpnViolations(o *unstructured.Unstructured, id, proxy string) []string {
	v, ok := vpnLabel(o)
	if !ok {
		return nil
	}
	s, isString := v.(string)
	switch {
	case !isString:
		return []string{fmt.Sprintf(`%s: label %s: %v not allowed (only the string %q, which assigns the pods to the platform VPN)`, id, LabelVPN, v, VPNAssigned)}
	case s != VPNAssigned:
		return []string{fmt.Sprintf(`%s: label %s: %q not allowed (only %q, which assigns the pods to the platform VPN)`, id, LabelVPN, s, VPNAssigned)}
	case proxy == "":
		return []string{fmt.Sprintf(`%s: label %s: %q assigns the pods to the platform VPN, but the platform has none (spec.vpn.proxy): they would have no internet`, id, LabelVPN, s)}
	}
	return nil
}
