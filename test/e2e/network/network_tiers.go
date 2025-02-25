/*
Copyright 2017 The Kubernetes Authors.

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

package network

import (
	"context"
	"fmt"
	"net/http"
	"time"

	compute "google.golang.org/api/compute/v1"

	"github.com/GoogleCloudPlatform/k8s-cloud-provider/pkg/cloud"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/providers/gce"
	e2eservice "k8s.io/kubernetes/test/e2e/framework/service"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	"k8s.io/kubernetes/test/e2e/network/common"
	gcecloud "k8s.io/legacy-cloud-providers/gce"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/onsi/ginkgo/v2"
)

var _ = common.SIGDescribe("Services GCE [Slow]", func() {
	f := framework.NewDefaultFramework("services")
	f.NamespacePodSecurityEnforceLevel = admissionapi.LevelPrivileged

	var cs clientset.Interface
	serviceLBNames := []string{}

	ginkgo.BeforeEach(func() {
		// This test suite requires the GCE environment.
		e2eskipper.SkipUnlessProviderIs("gce")
		cs = f.ClientSet
	})

	ginkgo.AfterEach(func() {
		if ginkgo.CurrentSpecReport().Failed() {
			DescribeSvc(f.Namespace.Name)
		}
		for _, lb := range serviceLBNames {
			framework.Logf("cleaning gce resource for %s", lb)
			framework.TestContext.CloudConfig.Provider.CleanupServiceResources(cs, lb, framework.TestContext.CloudConfig.Region, framework.TestContext.CloudConfig.Zone)
		}
		//reset serviceLBNames
		serviceLBNames = []string{}
	})
	ginkgo.It("should be able to create and tear down a standard-tier load balancer [Slow]", func(ctx context.Context) {
		lagTimeout := e2eservice.LoadBalancerLagTimeoutDefault
		createTimeout := e2eservice.GetServiceLoadBalancerCreationTimeout(cs)

		svcName := "net-tiers-svc"
		ns := f.Namespace.Name
		jig := e2eservice.NewTestJig(cs, ns, svcName)

		ginkgo.By("creating a pod to be part of the service " + svcName)
		_, err := jig.Run(nil)
		framework.ExpectNoError(err)

		// Test 1: create a standard tiered LB for the Service.
		ginkgo.By("creating a Service of type LoadBalancer using the standard network tier")
		svc, err := jig.CreateTCPService(func(svc *v1.Service) {
			svc.Spec.Type = v1.ServiceTypeLoadBalancer
			setNetworkTier(svc, string(gcecloud.NetworkTierAnnotationStandard))
		})
		framework.ExpectNoError(err)
		// Verify that service has been updated properly.
		svcTier, err := gcecloud.GetServiceNetworkTier(svc)
		framework.ExpectNoError(err)
		framework.ExpectEqual(svcTier, cloud.NetworkTierStandard)
		// Record the LB name for test cleanup.
		serviceLBNames = append(serviceLBNames, cloudprovider.DefaultLoadBalancerName(svc))

		// Wait and verify the LB.
		ingressIP := waitAndVerifyLBWithTier(jig, "", createTimeout, lagTimeout)

		// Test 2: re-create a LB of a different tier for the updated Service.
		ginkgo.By("updating the Service to use the premium (default) tier")
		svc, err = jig.UpdateService(func(svc *v1.Service) {
			setNetworkTier(svc, string(gcecloud.NetworkTierAnnotationPremium))
		})
		framework.ExpectNoError(err)
		// Verify that service has been updated properly.
		svcTier, err = gcecloud.GetServiceNetworkTier(svc)
		framework.ExpectNoError(err)
		framework.ExpectEqual(svcTier, cloud.NetworkTierDefault)

		// Wait until the ingress IP changes. Each tier has its own pool of
		// IPs, so changing tiers implies changing IPs.
		ingressIP = waitAndVerifyLBWithTier(jig, ingressIP, createTimeout, lagTimeout)

		// Test 3: create a standard-tierd LB with a user-requested IP.
		ginkgo.By("reserving a static IP for the load balancer")
		requestedAddrName := fmt.Sprintf("e2e-ext-lb-net-tier-%s", framework.RunID)
		gceCloud, err := gce.GetGCECloud()
		framework.ExpectNoError(err)
		requestedIP, err := reserveRegionalAddress(gceCloud, requestedAddrName, cloud.NetworkTierStandard)
		framework.ExpectNoError(err, "failed to reserve a STANDARD tiered address")
		defer func() {
			if requestedAddrName != "" {
				// Release GCE static address - this is not kube-managed and will not be automatically released.
				if err := gceCloud.DeleteRegionAddress(requestedAddrName, gceCloud.Region()); err != nil {
					framework.Logf("failed to release static IP address %q: %v", requestedAddrName, err)
				}
			}
		}()
		framework.ExpectNoError(err)
		framework.Logf("Allocated static IP to be used by the load balancer: %q", requestedIP)

		ginkgo.By("updating the Service to use the standard tier with a requested IP")
		svc, err = jig.UpdateService(func(svc *v1.Service) {
			svc.Spec.LoadBalancerIP = requestedIP
			setNetworkTier(svc, string(gcecloud.NetworkTierAnnotationStandard))
		})
		framework.ExpectNoError(err)
		// Verify that service has been updated properly.
		framework.ExpectEqual(svc.Spec.LoadBalancerIP, requestedIP)
		svcTier, err = gcecloud.GetServiceNetworkTier(svc)
		framework.ExpectNoError(err)
		framework.ExpectEqual(svcTier, cloud.NetworkTierStandard)

		// Wait until the ingress IP changes and verifies the LB.
		waitAndVerifyLBWithTier(jig, ingressIP, createTimeout, lagTimeout)
	})
})

func waitAndVerifyLBWithTier(jig *e2eservice.TestJig, existingIP string, waitTimeout, checkTimeout time.Duration) string {
	// If existingIP is "" this will wait for any ingress IP to show up. Otherwise
	// it will wait for the ingress IP to change to something different.
	svc, err := jig.WaitForNewIngressIP(existingIP, waitTimeout)
	framework.ExpectNoError(err)

	svcPort := int(svc.Spec.Ports[0].Port)
	lbIngress := &svc.Status.LoadBalancer.Ingress[0]
	ingressIP := e2eservice.GetIngressPoint(lbIngress)

	ginkgo.By("running sanity and reachability checks")
	if svc.Spec.LoadBalancerIP != "" {
		// Verify that the new ingress IP is the requested IP if it's set.
		framework.ExpectEqual(ingressIP, svc.Spec.LoadBalancerIP)
	}
	// If the IP has been used by previous test, sometimes we get the lingering
	// 404 errors even after the LB is long gone. Tolerate and retry until the
	// new LB is fully established.
	e2eservice.TestReachableHTTPWithRetriableErrorCodes(ingressIP, svcPort, []int{http.StatusNotFound}, checkTimeout)

	// Verify the network tier matches the desired.
	svcNetTier, err := gcecloud.GetServiceNetworkTier(svc)
	framework.ExpectNoError(err)
	netTier, err := getLBNetworkTierByIP(ingressIP)
	framework.ExpectNoError(err, "failed to get the network tier of the load balancer")
	framework.ExpectEqual(netTier, svcNetTier)

	return ingressIP
}

func getLBNetworkTierByIP(ip string) (cloud.NetworkTier, error) {
	var rule *compute.ForwardingRule
	// Retry a few times to tolerate flakes.
	err := wait.PollImmediate(5*time.Second, 15*time.Second, func() (bool, error) {
		obj, err := getGCEForwardingRuleByIP(ip)
		if err != nil {
			return false, err
		}
		rule = obj
		return true, nil
	})
	if err != nil {
		return "", err
	}
	return cloud.NetworkTierGCEValueToType(rule.NetworkTier), nil
}

func getGCEForwardingRuleByIP(ip string) (*compute.ForwardingRule, error) {
	cloud, err := gce.GetGCECloud()
	if err != nil {
		return nil, err
	}
	ruleList, err := cloud.ListRegionForwardingRules(cloud.Region())
	if err != nil {
		return nil, err
	}
	for _, rule := range ruleList {
		if rule.IPAddress == ip {
			return rule, nil
		}
	}
	return nil, fmt.Errorf("forwarding rule with ip %q not found", ip)
}

func setNetworkTier(svc *v1.Service, tier string) {
	key := gcecloud.NetworkTierAnnotationKey
	if svc.ObjectMeta.Annotations == nil {
		svc.ObjectMeta.Annotations = map[string]string{}
	}
	svc.ObjectMeta.Annotations[key] = tier
}

// TODO: add retries if this turns out to be flaky.
func reserveRegionalAddress(cloud *gcecloud.Cloud, name string, netTier cloud.NetworkTier) (string, error) {
	Addr := &compute.Address{
		Name:        name,
		NetworkTier: netTier.ToGCEValue(),
	}

	if err := cloud.ReserveRegionAddress(Addr, cloud.Region()); err != nil {
		return "", err
	}

	addr, err := cloud.GetRegionAddress(name, cloud.Region())
	if err != nil {
		return "", err
	}

	return addr.Address, nil
}
