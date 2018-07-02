/*
Copyright 2018 The Knative Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package istio

import (
	"fmt"
	"sort"

	"github.com/knative/serving/pkg"
	"github.com/knative/serving/pkg/apis/istio/v1alpha3"
	"github.com/knative/serving/pkg/apis/serving/v1alpha1"
	"github.com/knative/serving/pkg/controller"
	"github.com/knative/serving/pkg/controller/revision"
	"github.com/knative/serving/pkg/controller/route/traffic"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	PortNumber            = 80
	PortName              = "http"
	EnvoyTimeoutHeader    = "x-envoy-upstream-rq-timeout-ms"
	DefaultEnvoyTimeoutMs = "60000"
)

// MakeVirtualService creates an Istio VirtualService to set up routing rules.  Such VirtualService specifies
// which Gateways and Hosts that it applies to, as well as the routing rules.
func MakeVirtualService(u *v1alpha1.Route, tc *traffic.TrafficConfig) *v1alpha3.VirtualService {
	return &v1alpha3.VirtualService{
		ObjectMeta: metav1.ObjectMeta{
			Name:            controller.GetVirtualServiceName(u),
			Namespace:       u.Namespace,
			Labels:          map[string]string{"route": u.Name},
			OwnerReferences: []metav1.OwnerReference{*controller.NewRouteControllerRef(u)},
		},
		Spec: makeVirtualServiceSpec(u, tc.Targets),
	}
}

func makeVirtualServiceSpec(u *v1alpha1.Route, targets map[string][]traffic.RevisionTarget) v1alpha3.VirtualServiceSpec {
	domain := u.Status.Domain
	spec := v1alpha3.VirtualServiceSpec{
		// We want to connect to two Gateways: the Knative shared
		// Gateway, and the 'mesh' Gateway.  The former provides
		// access from outside of the cluster, and the latter provides
		// access for services from inside the cluster.
		Gateways: []string{
			controller.GetServingK8SGatewayFullname(),
			"mesh",
		},
		Hosts: []string{
			// Traffic originates from outside of the cluster would be of the form "*.domain", or "domain"
			fmt.Sprintf("*.%s", domain),
			domain,
			// Traffic from inside the cluster will use the FQDN of the Route's headless Service.
			controller.GetServingK8SServiceFullnameForRoute(u),
		},
	}
	names := []string{}
	for name := range targets {
		names = append(names, name)
	}
	// Sort the names to give things a deterministic ordering.
	sort.Strings(names)
	// The routes are matching rule based on domain name to traffic split targets.
	for _, name := range names {
		spec.Http = append(spec.Http, *makeVirtualServiceRoute(getRouteDomains(name, u, domain), u.Namespace, targets[name]))
	}
	return spec
}

func getRouteDomains(targetName string, u *v1alpha1.Route, domain string) []string {
	if targetName == "" {
		// Nameless traffic targets correspond to two domains: the Route.Status.Domain, and also the FQDN
		// of the Route's headless Service.
		return []string{domain, controller.GetServingK8SServiceFullnameForRoute(u)}
	}
	// Named traffic targets correspond to a subdomain of the Route.Status.Domain.
	return []string{fmt.Sprintf("%s.%s", targetName, domain)}
}

func makeVirtualServiceRoute(domains []string, ns string, targets []traffic.RevisionTarget) *v1alpha3.HTTPRoute {
	matches := []v1alpha3.HTTPMatchRequest{}
	// Istio list of matches are OR'ed together.  The following build a match set that matches any of the given domains.
	for _, domain := range domains {
		matches = append(matches, v1alpha3.HTTPMatchRequest{
			Authority: &v1alpha3.StringMatch{
				Exact: domain,
			},
		})
	}
	active, inactive := groupInactiveTargets(targets)
	weights := []v1alpha3.DestinationWeight{}
	for _, t := range active {
		if t.Percent == 0 {
			continue
		}
		weights = append(weights, v1alpha3.DestinationWeight{
			Destination: v1alpha3.Destination{
				Host: controller.GetK8SServiceFullname(
					controller.GetServingK8SServiceNameForObj(t.TrafficTarget.RevisionName), ns),
				Port: v1alpha3.PortSelector{
					Number: uint32(revision.ServicePort),
				},
			},
			Weight: t.Percent,
		})
	}
	route := v1alpha3.HTTPRoute{
		Match: matches,
		Route: weights,
	}
	// Add traffic rules for activator.
	return addActivatorRoutes(&route, ns, inactive)
}

/////////////////////////////////////////////////
// Activator routing logic.
/////////////////////////////////////////////////

// TODO: The ideal solution is to append different revision name as headers for each inactive revision.
// See https://github.com/istio/issues/issues/332
//
// We will direct traffic for all inactive revisions to activator service; and the activator will send
// the request to the inactive revision with the largest traffic weight.
// The consequence of using appendHeaders at Spec is: if there are more than one inactive revisions, the
// traffic split percentage would be distorted in a short period of time.
func addActivatorRoutes(r *v1alpha3.HTTPRoute, ns string, inactive []traffic.RevisionTarget) *v1alpha3.HTTPRoute {
	if len(inactive) == 0 {
		// No need to change
		return r
	}
	totalInactivePercent := 0
	maxInactiveTarget := traffic.RevisionTarget{}

	for _, t := range inactive {
		totalInactivePercent += t.Percent
		if t.Percent >= maxInactiveTarget.Percent {
			maxInactiveTarget = t
		}
	}
	r.Route = append(r.Route, v1alpha3.DestinationWeight{
		Destination: v1alpha3.Destination{
			Host: fmt.Sprintf("%s.%s.svc.cluster.local", controller.GetServingK8SActivatorServiceName(), pkg.GetServingSystemNamespace()),
			Port: v1alpha3.PortSelector{
				Number: uint32(revision.ServicePort),
			},
		},
		Weight: totalInactivePercent,
	})
	r.AppendHeaders = map[string]string{
		controller.GetRevisionHeaderName():      maxInactiveTarget.RevisionName,
		controller.GetRevisionHeaderNamespace(): ns,
		EnvoyTimeoutHeader:                      DefaultEnvoyTimeoutMs,
	}
	return r
}

func groupInactiveTargets(targets []traffic.RevisionTarget) (active []traffic.RevisionTarget, inactive []traffic.RevisionTarget) {
	for _, t := range targets {
		if t.Active {
			active = append(active, t)
		} else {
			inactive = append(inactive, t)
		}
	}
	return active, inactive
}