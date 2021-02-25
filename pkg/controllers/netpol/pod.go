package netpol

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/cloudnativelabs/kube-router/pkg/utils"
	"github.com/coreos/go-iptables/iptables"
	"github.com/golang/glog"
	api "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

func (npc *NetworkPolicyController) newPodEventHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if !npc.readyForUpdates {
				return
			}
			podObj := obj.(*api.Pod)
			glog.V(2).Infof("Received pod:%s/%s add event", podObj.Namespace, podObj.Name)
			npc.RequestFullSync()			
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			if !npc.readyForUpdates {
				return
			}
			newPodObj := newObj.(*api.Pod)
			oldPodObj := oldObj.(*api.Pod)
			glog.V(2).Infof("Received pod:%s/%s update event", newPodObj.Namespace, newPodObj.Name)
			// for the network policies, we are only interested in pod status phase change
			// or IP change or change of pod labels
			if newPodObj.Status.Phase != oldPodObj.Status.Phase ||
				newPodObj.Status.PodIP != oldPodObj.Status.PodIP ||
				!reflect.DeepEqual(newPodObj.Labels, oldPodObj.Labels) {
					npc.RequestFullSync()			

			}
		},
		DeleteFunc: func(obj interface{}) {
			if !npc.readyForUpdates {
				return
			}
			npc.RequestFullSync()			

		},
	}
}

/*
func (npc *NetworkPolicyController) processPodAddUpdateEvents(pod *api.Pod) {

	// skip processing update to pods in host network
	if pod.Spec.HostNetwork {
		return
	}
	// skip pods in trasient state
	if len(pod.Status.PodIP) == 0 || pod.Status.PodIP == "" {
		return
	}

	// if there is outstanding full-sync request the skip processing the event
	if len(npc.fullSyncRequestChan) == cap(npc.fullSyncRequestChan) {
		return
	}

	npc.mu.Lock()
	defer npc.mu.Unlock()

	iptablesCmdHandler, err := iptables.New()
	if err != nil {
		glog.Fatalf("Failed to initialize iptables executor: %s", err.Error())
	}
	podInfo := podInfo{ip: pod.Status.PodIP,
		name:      pod.ObjectMeta.Name,
		namespace: pod.ObjectMeta.Namespace,
		labels:    pod.ObjectMeta.Labels}

	podNamespacedName := pod.ObjectMeta.Namespace + "/" + pod.ObjectMeta.Name

	err = npc.syncAffectedNetworkPolicyChains(&podInfo, syncVersion)
	if err != nil {
		glog.Errorf("failed to refresh network policy chains affected by pod:%s event due to %s", podNamespacedName, err.Error())
	}

	// only for local pods we need to setup pod firewall chains
	if !isLocalPod(pod, npc.nodeIP.String()) {
		return
	}
	networkPoliciesInfo, err := npc.buildNetworkPoliciesInfo()
	if err != nil {
		glog.Errorf("Failed to build network policies info due to %s", err.Error())
	}
	err = npc.syncPodFirewall(&podInfo, networkPoliciesInfo, syncVersion, iptablesCmdHandler)
	if err != nil {
		glog.Errorf("Failed to sync pod:%s firewall chain due to %s", podNamespacedName, err.Error())
	}
}
*/

// OnPodDelete handles delete of a pods event from the Kubernetes api server
func (npc *NetworkPolicyController) processPodDeleteEvent(obj interface{}) {
	pod, ok := obj.(*api.Pod)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			glog.Errorf("unexpected object type: %v", obj)
			return
		}
		if pod, ok = tombstone.Obj.(*api.Pod); !ok {
			glog.Errorf("unexpected object type: %v", obj)
			return
		}
	}
	glog.V(2).Infof("Received pod:%s/%s delete event", pod.Namespace, pod.Name)

	// skip processing update to pods in host network
	if pod.Spec.HostNetwork {
		return
	}

	// if there is outstanding full-sync request the skip processing the event
	if len(npc.fullSyncRequestChan) == cap(npc.fullSyncRequestChan) {
		return
	}

	npc.mu.Lock()
	defer npc.mu.Unlock()

	podInfo := podInfo{ip: pod.Status.PodIP,
		name:      pod.ObjectMeta.Name,
		namespace: pod.ObjectMeta.Namespace,
		labels:    pod.ObjectMeta.Labels}

	err := npc.syncAffectedNetworkPolicyChains(&podInfo, syncVersion)
	if err != nil {
		glog.Errorf("failed to refresh network policy chains affected by pod %s/%s delete event due to %s", pod.Namespace, pod.Name, err.Error())
	}

	// cleanup of firewall chains needed only for local pods
	if !isLocalPod(pod, npc.nodeIP.String()) {
		return
	}

	podFwChainName := podFirewallChainName(pod.Namespace, pod.Name, syncVersion)
	iptablesCmdHandler, err := iptables.New()
	if err != nil {
		glog.Fatalf("Failed to initialize iptables executor: %s", err.Error())
	}
	topLevelChains := []string{kubeInputChainName, kubeForwardChainName, kubeOutputChainName}
	for _, chain := range topLevelChains {
		chainRules, err := iptablesCmdHandler.List("filter", chain)
		if err != nil {
			glog.Fatalf("failed to list rules in filter table, %s top level chain due to %s", chain, err.Error())
		}
		var realRuleNo int
		for i, rule := range chainRules {
			if strings.Contains(rule, podFwChainName) {
				err = iptablesCmdHandler.Delete("filter", chain, strconv.Itoa(i-realRuleNo))
				if err != nil {
					glog.Errorf("failed to delete rule: %s from the %s top level chian of filter table due to %s", rule, chain, err.Error())
				}
				realRuleNo++
			}
		}
	}

	err = iptablesCmdHandler.ClearChain("filter", podFwChainName)
	if err != nil {
		glog.Errorf("Failed to flush the rules in chain %s due to %s", podFwChainName, err.Error())
	}
	err = iptablesCmdHandler.DeleteChain("filter", podFwChainName)
	if err != nil {
		glog.Errorf("Failed to delete the chain %s due to %s", podFwChainName, err.Error())
	}
}

// when a new pod added/deleted/updated this function ensures only matching network
// policies (i.e. pod labels match with one of network policy target pod selector or
// ingress pod selector or egress pod selector) are synced to reflect the desired state
func (npc *NetworkPolicyController) syncAffectedNetworkPolicyChains(pod *podInfo, version string) error {

	for _, policyObj := range npc.npLister.List() {
		policy, ok := policyObj.(*networking.NetworkPolicy)
		targetPodSelector, _ := v1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
		if !ok {
			return fmt.Errorf("Failed to convert network policy pod selector")
		}

		if policy.ObjectMeta.Namespace == pod.namespace && targetPodSelector.Matches(labels.Set(pod.labels)) {
			matchingPods, err := npc.listPodsByNamespaceAndLabels(policy.Namespace, targetPodSelector)
			if err != nil {
				return err
			}
			matchingPodIps := make([]string, 0, len(matchingPods))
			for _, matchingPod := range matchingPods {
				if matchingPod.Status.PodIP == "" {
					continue
				}
				matchingPodIps = append(matchingPodIps, matchingPod.Status.PodIP)
			}
			if len(policy.Spec.Ingress) > 0 {
				// create a ipset for all destination pod ip's matched by the policy spec target PodSelector
				targetDestPodIPSetName := policyDestinationPodIPSetName(policy.Namespace, policy.Name)
				targetDestPodIPSet, err := npc.ipSetHandler.Create(targetDestPodIPSetName, utils.TypeHashIP, utils.OptionTimeout, "0")
				if err != nil {
					return fmt.Errorf("failed to create ipset: %s", err.Error())
				}
				err = targetDestPodIPSet.Refresh(matchingPodIps, utils.OptionTimeout, "0")
				if err != nil {
					glog.Errorf("failed to refresh targetDestPodIPSet,: " + err.Error())
				}
			}
			if len(policy.Spec.Egress) > 0 {
				// create a ipset for all source pod ip's matched by the policy spec target PodSelector
				targetSourcePodIPSetName := policySourcePodIPSetName(policy.Namespace, policy.Name)
				targetSourcePodIPSet, err := npc.ipSetHandler.Create(targetSourcePodIPSetName, utils.TypeHashIP, utils.OptionTimeout, "0")
				if err != nil {
					return fmt.Errorf("failed to create ipset: %s", err.Error())
				}
				err = targetSourcePodIPSet.Refresh(matchingPodIps, utils.OptionTimeout, "0")
				if err != nil {
					glog.Errorf("failed to refresh targetSourcePodIPSet: " + err.Error())
				}
			}
		}

		for i, specIngressRule := range policy.Spec.Ingress {
			if len(specIngressRule.From) == 0 {
				continue
			}
			for _, peer := range specIngressRule.From {
				if peer.PodSelector == nil && peer.NamespaceSelector == nil && peer.IPBlock != nil {
					continue
				}
				var matchesPodSelector, matchesNamespaceSelector bool
				if peer.PodSelector == nil {
					matchesPodSelector = true
				} else {
					fromPodSelector, _ := v1.LabelSelectorAsSelector(peer.PodSelector)
					matchesPodSelector = fromPodSelector.Matches(labels.Set(pod.labels))
				}
				if peer.NamespaceSelector == nil {
					matchesNamespaceSelector = true
				} else {
					namespaceSelector, _ := v1.LabelSelectorAsSelector(peer.NamespaceSelector)
					namespaceLister := listers.NewNamespaceLister(npc.nsLister)
					namespaceObj, _ := namespaceLister.Get(pod.namespace)
					matchesNamespaceSelector = namespaceSelector.Matches(labels.Set(namespaceObj.Labels))
				}
				if !(matchesNamespaceSelector && matchesPodSelector) {
					continue
				}
				peerPods, err := npc.evalPodPeer(policy, peer)
				if err == nil {
					return err
				}
				ingressRuleSrcPodIPs := make([]string, 0, len(peerPods))
				for _, peerPod := range peerPods {
					if peerPod.Status.PodIP == "" {
						continue
					}
					ingressRuleSrcPodIPs = append(ingressRuleSrcPodIPs, peerPod.Status.PodIP)
				}
				srcPodIPSetName := policyIndexedSourcePodIPSetName(policy.Namespace, policy.Name, i)
				srcPodIPSet, err := npc.ipSetHandler.Create(srcPodIPSetName, utils.TypeHashIP, utils.OptionTimeout, "0")
				if err != nil {
					return fmt.Errorf("failed to create ipset: %s", err.Error())
				}
				err = srcPodIPSet.Refresh(ingressRuleSrcPodIPs)
				if err != nil {
					glog.Errorf("failed to refresh srcPodIPSet: " + err.Error())
				}
			}
		}

		for i, specEgressRule := range policy.Spec.Egress {
			if len(specEgressRule.To) == 0 {
				continue
			}
			for _, peer := range specEgressRule.To {
				if peer.PodSelector == nil && peer.NamespaceSelector == nil && peer.IPBlock != nil {
					continue
				}
				var matchesPodSelector, matchesNamespaceSelector bool
				if peer.PodSelector == nil {
					matchesPodSelector = true
				} else {
					toPodSelector, _ := v1.LabelSelectorAsSelector(peer.PodSelector)
					matchesPodSelector = toPodSelector.Matches(labels.Set(pod.labels))
				}
				if peer.NamespaceSelector == nil {
					matchesNamespaceSelector = true
				} else {
					namespaceSelector, _ := v1.LabelSelectorAsSelector(peer.NamespaceSelector)
					namespaceLister := listers.NewNamespaceLister(npc.nsLister)
					namespaceObj, _ := namespaceLister.Get(pod.namespace)
					matchesNamespaceSelector = namespaceSelector.Matches(labels.Set(namespaceObj.Labels))
				}
				if !(matchesNamespaceSelector && matchesPodSelector) {
					continue
				}
				peerPods, err := npc.evalPodPeer(policy, peer)
				if err == nil {
					return err
				}
				egressRuleDstPodIps := make([]string, 0, len(peerPods))
				for _, peerPod := range peerPods {
					if peerPod.Status.PodIP == "" {
						continue
					}
					egressRuleDstPodIps = append(egressRuleDstPodIps, peerPod.Status.PodIP)
				}
				dstPodIPSetName := policyIndexedDestinationPodIPSetName(policy.Namespace, policy.Name, i)
				dstPodIPSet, err := npc.ipSetHandler.Create(dstPodIPSetName, utils.TypeHashIP, utils.OptionTimeout, "0")
				if err != nil {
					return fmt.Errorf("failed to create ipset: %s", err.Error())
				}
				err = dstPodIPSet.Refresh(egressRuleDstPodIps)
				if err != nil {
					glog.Errorf("failed to refresh srcPodIPSet: " + err.Error())
				}
			}
		}
	}
	return nil
}

func (npc *NetworkPolicyController) fullSyncPodFirewallChains(currentFilterTable *bytes.Buffer, networkPoliciesInfo []networkPolicyInfo, version string) (map[string]bool, error) {

	activePodFwChains := make(map[string]bool)

	iptablesCmdHandler, err := iptables.New()
	if err != nil {
		glog.Fatalf("Failed to initialize iptables executor: %s", err.Error())
	}

	allLocalPods, err := npc.getLocalPods(npc.nodeIP.String())
	if err != nil {
		return nil, err
	}
	for _, pod := range *allLocalPods {
		podFwChainName := podFirewallChainName(pod.namespace, pod.name, version)
		currentFilterTable.WriteString(":"+podFwChainName+"\n")

		activePodFwChains[podFwChainName] = true
		err = npc.syncPodFirewall(currentFilterTable, &pod, networkPoliciesInfo, version, iptablesCmdHandler)
		if err != nil {
			return nil, fmt.Errorf("Failed to sync pod firewall: %s", err.Error())
		}
	}

	return activePodFwChains, nil
}

func (npc *NetworkPolicyController) syncPodFirewall(currentFilterTable *bytes.Buffer, pod *podInfo, networkPoliciesInfo []networkPolicyInfo, version string, iptablesCmdHandler *iptables.IPTables) error {
	podFwChainName := podFirewallChainName(pod.namespace, pod.name, version)

	// setup rules to run pod inbound traffic through applicable ingress network policies
	err := npc.setupPodIngressRules(pod, podFwChainName, networkPoliciesInfo, currentFilterTable, version)
	if err != nil {
		return err
	}

	// setup rules to run pod outbound traffic through applicable egress network policies
	err = npc.setupPodEgressRules(pod, podFwChainName, networkPoliciesInfo, currentFilterTable, version)
	if err != nil {
		return err
	}

	// setup rules to drop the traffic from/to the pods that is not expliclty whitelisted
	err = npc.processNonWhitelistedTrafficRules(pod.name, pod.namespace, podFwChainName, currentFilterTable)
	if err != nil {
		return err
	}

	// setup rules to process the traffic from/to the pods that is whitelisted
	err = npc.processWhitelistedTrafficRules(pod.name, pod.namespace, podFwChainName, currentFilterTable)
	if err != nil {
		return err
	}

	// setup rules to intercept inbound traffic to the pods
	err = npc.interceptPodInboundTraffic(pod, podFwChainName, currentFilterTable)
	if err != nil {
		return err
	}

	// setup rules to intercept outbound traffic from the pods
	err = npc.interceptPodOutboundTraffic(pod, podFwChainName, currentFilterTable)
	if err != nil {
		return err
	}

	return nil
}

// setup iptable rules to intercept inbound traffic to pods and run it across the
// firewall chain corresponding to the pod so that ingress network policies are enforced
func (npc *NetworkPolicyController) interceptPodInboundTraffic(pod *podInfo, podFwChainName string, currentFilterTable *bytes.Buffer) error {
	// ensure there is rule in filter table and FORWARD chain to jump to pod specific firewall chain
	// this rule applies to the traffic getting routed (coming for other node pods)
	comment := "\"rule to jump traffic destined to POD name:" + pod.name + " namespace: " + pod.namespace +
		" to chain " + podFwChainName + "\""
	args := []string{"-I", kubeForwardChainName, "1", "-m", "comment", "--comment", comment, "-d", pod.ip, "-j", podFwChainName, "\n"}
	currentFilterTable.WriteString(strings.Join(args, " "))

	// ensure there is rule in filter table and OUTPUT chain to jump to pod specific firewall chain
	// this rule applies to the traffic from a pod getting routed back to another pod on same node by service proxy
	args = []string{"-I", kubeOutputChainName, "1", "-m", "comment", "--comment", comment, "-d", pod.ip, "-j", podFwChainName, "\n"}
	currentFilterTable.WriteString(strings.Join(args, " "))

	// ensure there is rule in filter table and forward chain to jump to pod specific firewall chain
	// this rule applies to the traffic getting switched (coming for same node pods)
	comment = "\"rule to jump traffic destined to POD name:" + pod.name + " namespace: " + pod.namespace +
		" to chain " + podFwChainName + "\""
	args = []string{"-I", kubeForwardChainName, "1", "-m", "physdev", "--physdev-is-bridged",
		"-m", "comment", "--comment", comment,
		"-d", pod.ip,
		"-j", podFwChainName, "\n"}
	currentFilterTable.WriteString(strings.Join(args, " "))

	return nil
}

// setup iptable rules to intercept outbound traffic from pods and run it across the
// firewall chain corresponding to the pod so that egress network policies are enforced
func (npc *NetworkPolicyController) interceptPodOutboundTraffic(pod *podInfo, podFwChainName string, currentFilterTable *bytes.Buffer) error {
	egressFilterChains := []string{kubeInputChainName, kubeForwardChainName, kubeOutputChainName}
	for _, chain := range egressFilterChains {
		// ensure there is rule in filter table and FORWARD chain to jump to pod specific firewall chain
		// this rule applies to the traffic getting forwarded/routed (traffic from the pod destinted
		// to pod on a different node)
		comment := "\"rule to jump traffic from POD name:" + pod.name + " namespace: " + pod.namespace +
			" to chain " + podFwChainName + "\""
		args := []string{"-I", chain, "1", "-m", "comment", "--comment", comment, "-s", pod.ip, "-j", podFwChainName, "\n"}
		currentFilterTable.WriteString(strings.Join(args, " "))
	}

	// ensure there is rule in filter table and forward chain to jump to pod specific firewall chain
	// this rule applies to the traffic getting switched (coming for same node pods)
	comment := "\"rule to jump traffic from POD name:" + pod.name + " namespace: " + pod.namespace +
		" to chain " + podFwChainName + "\""
	args := []string{"-I", kubeForwardChainName, "1", "-m", "physdev", "--physdev-is-bridged",
		"-m", "comment", "--comment", comment,
		"-s", pod.ip,
		"-j", podFwChainName, "\n"}
	currentFilterTable.WriteString(strings.Join(args, " "))

	return nil
}

// setup rules to jump to applicable network policy chaings for the pod inbound traffic
func (npc *NetworkPolicyController) setupPodIngressRules(pod *podInfo, podFwChainName string, networkPoliciesInfo []networkPolicyInfo, currentFilterTable *bytes.Buffer, version string) error {
	var ingressPoliciesPresent bool
	// add entries in pod firewall to run through required network policies
	for _, policy := range networkPoliciesInfo {
		if _, ok := policy.targetPods[pod.ip]; !ok {
			continue
		}
		ingressPoliciesPresent = true
		comment := "\"run through nw policy " + policy.name + "\""
		policyChainName := networkPolicyChainName(policy.namespace, policy.name, version)
		args := []string{"-I", podFwChainName, "1", "-m", "comment", "--comment", comment, "-j", policyChainName, "\n"}
		currentFilterTable.WriteString(strings.Join(args, " "))
	}

	if !ingressPoliciesPresent {
		comment := "\"run through default ingress policy  chain\""
		args := []string{"-I", podFwChainName, "1", "-d", pod.ip, "-m", "comment", "--comment", comment, "-j", kubeIngressNetpolChain, "\n"}
		currentFilterTable.WriteString(strings.Join(args, " "))
	}

	comment := "\"rule to permit the traffic traffic to pods when source is the pod's local node\""
	args := []string{"-I", podFwChainName, "1", "-m", "comment", "--comment", comment, "-m", "addrtype", "--src-type", "LOCAL", "-d", pod.ip, "-j", "ACCEPT", "\n"}
	currentFilterTable.WriteString(strings.Join(args, " "))

	// ensure statefull firewall, that permits return traffic for the traffic originated by the pod
	comment = "\"rule for stateful firewall for pod\""
	args = []string{"-I", podFwChainName, "1",  "-m", "comment", "--comment", comment, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT", "\n"}
	currentFilterTable.WriteString(strings.Join(args, " "))

	return nil
}

// setup rules to jump to applicable network policy chains for the pod outbound traffic
func (npc *NetworkPolicyController) setupPodEgressRules(pod *podInfo, podFwChainName string, networkPoliciesInfo []networkPolicyInfo, currentFilterTable *bytes.Buffer, version string) error {
	var egressPoliciesPresent bool
	// add entries in pod firewall to run through required network policies
	for _, policy := range networkPoliciesInfo {
		if _, ok := policy.targetPods[pod.ip]; !ok {
			continue
		}
		egressPoliciesPresent = true
		comment := "\"run through nw policy " + policy.name + "\""
		policyChainName := networkPolicyChainName(policy.namespace, policy.name, version)
		args := []string{"-I", podFwChainName, "1", "-m", "comment", "--comment", comment, "-j", policyChainName, "\n"}
		currentFilterTable.WriteString(strings.Join(args, " "))

	}

	if !egressPoliciesPresent {
		comment := "\"run through default egress policy  chain\""
		args := []string{"-I", podFwChainName, "1", "-s", pod.ip, "-m", "comment", "--comment", comment, "-j", kubeEgressNetpolChain, "\n"}
		currentFilterTable.WriteString(strings.Join(args, " "))

	}

	// ensure statefull firewall, that permits return traffic for the traffic originated by the pod
	comment := "\"rule for stateful firewall for pod\""
	args := []string{"-I", podFwChainName, "1", "-m", "comment", "--comment", comment, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT", "\n"}
	currentFilterTable.WriteString(strings.Join(args, " "))

	return nil
}

func (npc *NetworkPolicyController) processNonWhitelistedTrafficRules(podName, podNamespace, podFwChainName string, currentFilterTable *bytes.Buffer) error {
	// add rule to log the packets that will be dropped due to network policy enforcement
	comment := "\"rule to log dropped traffic POD name:" + podName + " namespace: " + podNamespace + "\""
	args := []string{"-A", podFwChainName, "-m", "comment", "--comment", comment, "-m", "mark", "!", "--mark", "0x10000/0x10000", "-j", "NFLOG", "--nflog-group", "100", "-m", "limit", "--limit", "10/minute", "--limit-burst", "10", "\n"}
	currentFilterTable.WriteString(strings.Join(args, " "))

	// add rule to DROP if no applicable network policy permits the traffic
	comment = "\"rule to REJECT traffic destined for POD name:" + podName + " namespace: " + podNamespace + "\""
	args = []string{"-A", podFwChainName, "-m", "comment", "--comment", comment, "-m", "mark", "!", "--mark", "0x10000/0x10000", "-j", "REJECT", "\n"}
	currentFilterTable.WriteString(strings.Join(args, " "))

	return nil
}

func (npc *NetworkPolicyController) processWhitelistedTrafficRules(podName, podNamespace, podFwChainName string, currentFilterTable *bytes.Buffer) error {
	// if the traffic is whitelisted, reset mark to let traffic pass through
	// matching pod firewall chains (only case this happens is when source
	// and destination are on the same pod in which policies for both the pods
	// need to be run through)
	args := []string{"-A", podFwChainName, "-j", "MARK", "--set-mark", "0/0x10000", "\n"}
	currentFilterTable.WriteString(strings.Join(args, " "))

	// set mark to indicate traffic passed network policies. Mark will be
	// checked to ACCEPT the traffic
	comment := "\"set mark to ACCEPT traffic that comply to network policies\""
	args = []string{"-A", podFwChainName, "-m", "comment", "--comment", comment, "-j", "MARK", "--set-mark", "0x20000/0x20000", "\n"}
	currentFilterTable.WriteString(strings.Join(args, " "))

	return nil
}

func (npc *NetworkPolicyController) getLocalPods(nodeIP string) (*map[string]podInfo, error) {
	localPods := make(map[string]podInfo)
	for _, obj := range npc.podLister.List() {
		pod := obj.(*api.Pod)
		// skip pods not local to the node
		if !isLocalPod(pod, nodeIP) {
			continue
		}

		// skip pods in host network
		if pod.Spec.HostNetwork {
			continue
		}

		// skip pods in trasient state
		if len(pod.Status.PodIP) == 0 || pod.Status.PodIP == "" {
			continue
		}
		localPods[pod.Status.PodIP] = podInfo{ip: pod.Status.PodIP,
			name:      pod.ObjectMeta.Name,
			namespace: pod.ObjectMeta.Namespace,
			labels:    pod.ObjectMeta.Labels}
	}
	return &localPods, nil
}

func isLocalPod(pod *api.Pod, nodeIP string) bool {
	return strings.Compare(pod.Status.HostIP, nodeIP) == 0
}

func podFirewallChainName(namespace, podName string, version string) string {
	hash := sha256.Sum256([]byte(namespace + podName + version))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return kubePodFirewallChainPrefix + encoded[:16]
}
