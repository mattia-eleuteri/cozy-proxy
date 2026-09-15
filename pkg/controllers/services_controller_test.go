package controllers

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	nat "github.com/cozystack/cozy-proxy/pkg/proxy"
)

// epOnNode builds an Endpoints with a single address, optionally pinned to a
// node. Pass an empty node to omit NodeName, as an endpoint with no scheduling
// information would have.
func epOnNode(podIP, node string) *v1.Endpoints {
	addr := v1.EndpointAddress{IP: podIP}
	if node != "" {
		addr.NodeName = &node
	}
	return &v1.Endpoints{
		Subsets: []v1.EndpointSubset{{Addresses: []v1.EndpointAddress{addr}}},
	}
}

// lbService builds a LoadBalancer service already assigned the given IP.
func lbService(svcIP string, annot map[string]string) *v1.Service {
	return &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Annotations: annot},
		Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{
				Ingress: []v1.LoadBalancerIngress{{IP: svcIP}},
			},
		},
	}
}

// recordingProxy captures which datapath calls a controller makes.
type recordingProxy struct {
	nat.DummyProxyProcessor
	calls       []string
	keepEgress  map[string]string
	keepIngress map[string]string
}

func (r *recordingProxy) EnsureEgressSNAT(svcIP, podIP string) error {
	r.calls = append(r.calls, "EnsureEgressSNAT")
	return nil
}

func (r *recordingProxy) DeleteEgressSNAT(svcIP, podIP string) error {
	r.calls = append(r.calls, "DeleteEgressSNAT")
	return nil
}

func (r *recordingProxy) EnsureIngressDNAT(svcIP, podIP string) error {
	r.calls = append(r.calls, "EnsureIngressDNAT")
	return nil
}

func (r *recordingProxy) DeleteIngressDNAT(svcIP, podIP string) error {
	r.calls = append(r.calls, "DeleteIngressDNAT")
	return nil
}

func (r *recordingProxy) CleanupRules(keepEgress, keepIngress map[string]string) error {
	r.calls = append(r.calls, "CleanupRules")
	r.keepEgress = keepEgress
	r.keepIngress = keepIngress
	return nil
}

func (r *recordingProxy) EnsurePortFilter(svcIP, podIP string, ports []v1.ServicePort) error {
	r.calls = append(r.calls, "EnsurePortFilter")
	return nil
}

func (r *recordingProxy) DeletePortFilter(svcIP, podIP string) error {
	r.calls = append(r.calls, "DeletePortFilter")
	return nil
}

func (r *recordingProxy) has(call string) bool {
	for _, c := range r.calls {
		if c == call {
			return true
		}
	}
	return false
}

func TestServesEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		nodeName string
		ep       *v1.Endpoints
		expect   bool
	}{
		{"backend on this node", "node-a", epOnNode("10.0.0.1", "node-a"), true},
		{"backend on another node", "node-a", epOnNode("10.0.0.1", "node-b"), false},
		{"endpoint without NodeName is programmed", "node-a", epOnNode("10.0.0.1", ""), true},
		{"NODE_NAME unset programs everything", "", epOnNode("10.0.0.1", "node-b"), true},
		{"invalid endpoint is programmed", "node-a", &v1.Endpoints{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctrl := &ServicesController{NodeName: c.nodeName}
			if got := ctrl.servesEndpoint(c.ep); got != c.expect {
				t.Errorf("servesEndpoint = %v, want %v", got, c.expect)
			}
		})
	}
}

// The owning node programs both halves of the datapath.
func TestApplyRulesProgramsBothHalvesOnOwningNode(t *testing.T) {
	svc := lbService("192.0.2.10", map[string]string{"networking.cozystack.io/wholeIP": "false"})

	local := &recordingProxy{}
	localCtrl := &ServicesController{Proxy: local, NodeName: "node-a"}
	localCtrl.applyRules(svc, epOnNode("10.0.0.1", "node-a"), "test")

	for _, want := range []string{"EnsureEgressSNAT", "EnsureIngressDNAT", "EnsurePortFilter"} {
		if !local.has(want) {
			t.Errorf("owning node must call %s, got %v", want, local.calls)
		}
	}
}

// A non-owning node must not translate the destination — doing so
// desynchronises conntrack on the owning node and gets the reply dropped by
// port_filter — but it must still program the source rewrite.
//
// That rewrite is the only thing that repairs a reply reaching this node
// straight over the overlay, which is what happens whenever the client is
// inside the cluster and kube-ovn SNATs it to this node's own address.
func TestApplyRulesKeepsEgressSNATOnRemoteBackend(t *testing.T) {
	svc := lbService("192.0.2.10", map[string]string{"networking.cozystack.io/wholeIP": "false"})

	remote := &recordingProxy{}
	remoteCtrl := &ServicesController{Proxy: remote, NodeName: "node-a"}
	remoteCtrl.applyRules(svc, epOnNode("10.0.0.1", "node-b"), "test")

	if !remote.has("EnsureEgressSNAT") {
		t.Errorf("non-owning node must still program the source rewrite, got %v", remote.calls)
	}
	if remote.has("DeleteEgressSNAT") {
		t.Errorf("non-owning node must not withdraw the source rewrite, got %v", remote.calls)
	}
	if remote.has("EnsureIngressDNAT") || remote.has("EnsurePortFilter") {
		t.Errorf("non-owning node must not program the ingress half, got %v", remote.calls)
	}
	if !remote.has("DeleteIngressDNAT") {
		t.Errorf("non-owning node must withdraw an inherited destination rewrite, got %v", remote.calls)
	}
}

// A migrated VM leaves its old mapping behind on the node it came from.
func TestWithdrawStaleEndpoint(t *testing.T) {
	svc := lbService("192.0.2.10", map[string]string{"networking.cozystack.io/wholeIP": "false"})

	moved := &recordingProxy{}
	movedCtrl := &ServicesController{Proxy: moved, NodeName: "node-a"}
	movedCtrl.withdrawStaleEndpoint(svc, epOnNode("10.0.0.1", "node-a"), "10.0.0.2", "test")
	// A pod IP that is gone must leave nothing behind, in either half: every
	// node carries its source rewrite, so every node has to drop it.
	for _, want := range []string{"DeleteIngressDNAT", "DeleteEgressSNAT"} {
		if !moved.has(want) {
			t.Errorf("changed pod IP must call %s, got %v", want, moved.calls)
		}
	}

	same := &recordingProxy{}
	sameCtrl := &ServicesController{Proxy: same, NodeName: "node-a"}
	sameCtrl.withdrawStaleEndpoint(svc, epOnNode("10.0.0.1", "node-a"), "10.0.0.1", "test")
	if len(same.calls) != 0 {
		t.Errorf("unchanged pod IP must not touch the datapath, got %v", same.calls)
	}
}

func svcWith(annot map[string]string) *v1.Service {
	return &v1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: annot}}
}

func svcWithLabels(labels map[string]string) *v1.Service {
	return &v1.Service{ObjectMeta: metav1.ObjectMeta{Labels: labels}}
}

func TestIsCozyProxyService(t *testing.T) {
	cases := []struct {
		name   string
		svc    *v1.Service
		expect bool
	}{
		{"label with correct value", svcWithLabels(map[string]string{"service.kubernetes.io/service-proxy-name": "cozy-proxy"}), true},
		{"label with other value", svcWithLabels(map[string]string{"service.kubernetes.io/service-proxy-name": "kube-router"}), false},
		{"label absent", svcWithLabels(map[string]string{}), false},
		{"nil service", nil, false},
		{"only wholeIP annotation no label", svcWith(map[string]string{"networking.cozystack.io/wholeIP": "true"}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isCozyProxyService(c.svc); got != c.expect {
				t.Errorf("isCozyProxyService = %v, want %v", got, c.expect)
			}
		})
	}
}

func TestWholeIPPassthrough(t *testing.T) {
	cases := []struct {
		name   string
		svc    *v1.Service
		expect bool
	}{
		{"explicit true", svcWith(map[string]string{"networking.cozystack.io/wholeIP": "true"}), true},
		{"explicit false", svcWith(map[string]string{"networking.cozystack.io/wholeIP": "false"}), false},
		{"empty value", svcWith(map[string]string{"networking.cozystack.io/wholeIP": ""}), false},
		{"absent annotation defaults to port-filter", svcWith(map[string]string{}), false},
		{"nil annotations defaults to port-filter", &v1.Service{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := wholeIPPassthrough(c.svc); got != c.expect {
				t.Errorf("wholeIPPassthrough = %v, want %v", got, c.expect)
			}
		})
	}
}

func TestAllowICMP(t *testing.T) {
	cases := []struct {
		name   string
		svc    *v1.Service
		expect bool
	}{
		{"explicit true", svcWith(map[string]string{"networking.cozystack.io/allowICMP": "true"}), true},
		{"explicit false", svcWith(map[string]string{"networking.cozystack.io/allowICMP": "false"}), false},
		{"empty value", svcWith(map[string]string{"networking.cozystack.io/allowICMP": ""}), false},
		{"absent annotation defaults to false", svcWith(map[string]string{}), false},
		{"nil annotations defaults to false", &v1.Service{}, false},
		{"unrelated annotation", svcWith(map[string]string{"networking.cozystack.io/wholeIP": "false"}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := allowICMP(c.svc); got != c.expect {
				t.Errorf("allowICMP = %v, want %v", got, c.expect)
			}
		})
	}
}

// The startup snapshot must keep every managed pair in the egress map and only
// the locally hosted ones in the ingress map. Handing the same set to both is
// exactly what leaves an intra-cluster client without a usable reply.
func TestCleanupRemovedServicesScopesKeepMaps(t *testing.T) {
	annot := map[string]string{"networking.cozystack.io/wholeIP": "false"}

	ctrl := &ServicesController{Proxy: &recordingProxy{}, NodeName: "node-a"}
	ctrl.Services = NewServiceMap()
	ctrl.Services.Set("ns", "local", &ServiceEndpoints{
		Service:  lbService("192.0.2.10", annot),
		Endpoint: epOnNode("10.0.0.1", "node-a"),
	})
	ctrl.Services.Set("ns", "remote", &ServiceEndpoints{
		Service:  lbService("192.0.2.11", annot),
		Endpoint: epOnNode("10.0.0.2", "node-b"),
	})
	// A service still waiting for its endpoint contributes to neither map.
	ctrl.Services.Set("ns", "pending", &ServiceEndpoints{
		Service:  lbService("192.0.2.12", annot),
		Endpoint: nil,
	})

	if err := ctrl.cleanupRemovedServices(); err != nil {
		t.Fatalf("cleanupRemovedServices: %v", err)
	}
	rec := ctrl.Proxy.(*recordingProxy)

	wantEgress := map[string]string{"192.0.2.10": "10.0.0.1", "192.0.2.11": "10.0.0.2"}
	if len(rec.keepEgress) != len(wantEgress) {
		t.Fatalf("keepEgress = %v, want %v", rec.keepEgress, wantEgress)
	}
	for svc, pod := range wantEgress {
		if rec.keepEgress[svc] != pod {
			t.Errorf("keepEgress[%s] = %q, want %q", svc, rec.keepEgress[svc], pod)
		}
	}

	wantIngress := map[string]string{"192.0.2.10": "10.0.0.1"}
	if len(rec.keepIngress) != len(wantIngress) {
		t.Fatalf("keepIngress = %v, want %v", rec.keepIngress, wantIngress)
	}
	if rec.keepIngress["192.0.2.10"] != "10.0.0.1" {
		t.Errorf("keepIngress = %v, want %v", rec.keepIngress, wantIngress)
	}
}
