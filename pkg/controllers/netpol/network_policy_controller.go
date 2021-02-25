package netpol

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudnativelabs/kube-router/pkg/healthcheck"
	"github.com/cloudnativelabs/kube-router/pkg/metrics"
	"github.com/cloudnativelabs/kube-router/pkg/options"
	"github.com/cloudnativelabs/kube-router/pkg/utils"
	"github.com/coreos/go-iptables/iptables"
	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	kubePodFirewallChainPrefix   = "KUBE-POD-FW-"
	kubeNetworkPolicyChainPrefix = "KUBE-NWPLCY-"
	kubeSourceIPSetPrefix        = "KUBE-SRC-"
	kubeDestinationIPSetPrefix   = "KUBE-DST-"
	kubeInputChainName           = "KUBE-ROUTER-INPUT"
	kubeForwardChainName         = "KUBE-ROUTER-FORWARD"
	kubeOutputChainName          = "KUBE-ROUTER-OUTPUT"
	KubeDefaultPodFWChain        = "KUBE-POD-FW-DEFAULT"
	kubeIngressNetpolChain       = "KUBE-NWPLCY-DEFAULT-INGRESS"
	kubeEgressNetpolChain        = "KUBE-NWPLCY-DEFAULT-EGRESS"
)

// Network policy controller provides both ingress and egress filtering for the pods as per the defined network
// policies. Please refer to https://github.com/cloudnativelabs/kube-router/blob/maste/docs/design/npc-design.md
// for the design details

// NetworkPolicyController struct to hold information required by NetworkPolicyController
type NetworkPolicyController struct {
	nodeIP                  net.IP
	nodeHostName            string
	nodePodIPCIDR           string
	serviceClusterIPRange   net.IPNet
	serviceExternalIPRanges []net.IPNet
	serviceNodePortRange    string
	mu                      sync.Mutex
	syncPeriod              time.Duration
	MetricsEnabled          bool
	healthChan              chan<- *healthcheck.ControllerHeartbeat
	fullSyncRequestChan     chan struct{}
	readyForUpdates         bool
	netpolAllowPreCheck     bool

	ipSetHandler *utils.IPSet

	podLister cache.Indexer
	npLister  cache.Indexer
	nsLister  cache.Indexer

	PodEventHandler           cache.ResourceEventHandler
	NamespaceEventHandler     cache.ResourceEventHandler
	NetworkPolicyEventHandler cache.ResourceEventHandler
}

// internal structure to represent a network policy
type networkPolicyInfo struct {
	name        string
	namespace   string
	podSelector labels.Selector

	// set of pods matching network policy spec podselector label selector
	targetPods map[string]podInfo

	// whitelist ingress rules from the network policy spec
	ingressRules []ingressRule

	// whitelist egress rules from the network policy spec
	egressRules []egressRule

	// policy type "ingress" or "egress" or "both" as defined by PolicyType in the spec
	policyType string
}

// internal structure to represent Pod
type podInfo struct {
	ip        string
	name      string
	namespace string
	labels    map[string]string
}

// internal structure to represent NetworkPolicyIngressRule in the spec
type ingressRule struct {
	matchAllPorts  bool
	ports          []protocolAndPort
	namedPorts     []endPoints
	matchAllSource bool
	srcPods        map[string]podInfo
	srcIPBlocks    [][]string
}

// internal structure to represent NetworkPolicyEgressRule in the spec
type egressRule struct {
	matchAllPorts        bool
	ports                []protocolAndPort
	namedPorts           []endPoints
	matchAllDestinations bool
	dstPods              map[string]podInfo
	dstIPBlocks          [][]string
}

type protocolAndPort struct {
	protocol string
	port     string
}

type endPoints struct {
	ips []string
	protocolAndPort
}

type numericPort2eps map[string]*endPoints
type protocol2eps map[string]numericPort2eps
type namedPort2eps map[string]protocol2eps

// Run runs forever till we receive notification on stopCh to shutdown
func (npc *NetworkPolicyController) Run(healthChan chan<- *healthcheck.ControllerHeartbeat, stopCh <-chan struct{}, wg *sync.WaitGroup) {
	t := time.NewTicker(npc.syncPeriod)
	defer t.Stop()
	defer wg.Done()

	glog.Info("Starting network policy controller")
	npc.healthChan = healthChan

	// Full syncs of the network policy controller take a lot of time and can only be processed one at a time,
	// therefore, we start it in it's own goroutine and request a sync through a single item channel
	glog.Info("Starting network policy controller full sync goroutine")
	wg.Add(1)
	go func(fullSyncRequest <-chan struct{}, stopCh <-chan struct{}, wg *sync.WaitGroup) {
		defer wg.Done()
		for {
			// Add an additional non-blocking select to ensure that if the stopCh channel is closed it is handled first
			select {
			case <-stopCh:
				glog.Info("Shutting down network policies full sync goroutine")
				return
			default:
			}
			select {
			case <-stopCh:
				glog.Info("Shutting down network policies full sync goroutine")
				return
			case <-fullSyncRequest:
				glog.V(3).Info("Received request for a full sync, processing")
				npc.fullPolicySync()       // fullPolicySync() is a blocking request here
				npc.readyForUpdates = true // used to ensure atleast one full sync to happen before processing pod/netpol/namespace events
			}
		}
	}(npc.fullSyncRequestChan, stopCh, wg)

	// loop forever till notified to stop on stopCh
	for {
		glog.V(1).Info("Requesting periodic sync of iptables to reflect network policies")
		npc.RequestFullSync()
		select {
		case <-stopCh:
			glog.Infof("Shutting down network policies controller")
			return
		case <-t.C:
		}
	}
}

// RequestFullSync allows the request of a full network policy sync without blocking the callee
func (npc *NetworkPolicyController) RequestFullSync() {
	select {
	case npc.fullSyncRequestChan <- struct{}{}:
		glog.V(3).Info("Full sync request queue was empty so a full sync request was successfully sent")
	default: // Don't block if the buffered channel is full, return quickly so that we don't block callee execution
		glog.V(1).Info("Full sync request queue was full, skipping...")
	}
}

var syncVersion string

// Sync synchronizes iptables to desired state of network policies
func (npc *NetworkPolicyController) fullPolicySync() {

	var err error
	var networkPoliciesInfo []networkPolicyInfo
	npc.mu.Lock()
	defer npc.mu.Unlock()

	healthcheck.SendHeartBeat(npc.healthChan, "NPC")
	start := time.Now()
	syncVersion = strconv.FormatInt(start.UnixNano(), 10)
	defer func() {
		endTime := time.Since(start)
		if npc.MetricsEnabled {
			metrics.ControllerIptablesSyncTime.Observe(endTime.Seconds())
		}
		glog.V(1).Infof("sync iptables took %v", endTime)
	}()

	glog.V(1).Infof("Starting sync of iptables with version: %s", syncVersion)

	// setup default pod firewall chain
	npc.ensureDefaultPodFWChains()

	// ensure kube-router specific top level chains and corresponding rules exist
	npc.ensureTopLevelChains()

	// ensure default network policies chains
	npc.ensureDefaultNetworkPolicyChains()

	networkPoliciesInfo, err = npc.buildNetworkPoliciesInfo()
	if err != nil {
		glog.Errorf("Aborting sync. Failed to build network policies: %v", err.Error())
		return
	}

	var filterTableRules bytes.Buffer
	if err := utils.SaveInto("filter", &filterTableRules); err != nil {
		glog.Errorf("Aborting sync. Failed to run iptables-save: %v" + err.Error())
		return
	}

	activePolicyChains, activePolicyIPSets, err := npc.fullSyncNetworkPolicyChains(&filterTableRules, networkPoliciesInfo, syncVersion)
	if err != nil {
		glog.Errorf("Aborting sync. Failed to sync network policy chains: %v" + err.Error())
		return
	}

	activePodFwChains, err := npc.fullSyncPodFirewallChains(&filterTableRules, networkPoliciesInfo, syncVersion)
	if err != nil {
		glog.Errorf("Aborting sync. Failed to sync pod firewalls: %v", err.Error())
		return
	}

	err = cleanupStaleRules(&filterTableRules, activePolicyChains, activePodFwChains, activePolicyIPSets)
	if err != nil {
		glog.Errorf("Aborting sync. Failed to cleanup stale iptables rules: %v", err.Error())
		return
	}
}

// Creates custom chains KUBE-ROUTER-INPUT, KUBE-ROUTER-FORWARD, KUBE-ROUTER-OUTPUT
// and rules in the filter table to jump from builtin chain to custom chain
func (npc *NetworkPolicyController) ensureTopLevelChains() {

	iptablesCmdHandler, err := iptables.New()
	if err != nil {
		glog.Fatalf("Failed to initialize iptables executor due to %s", err.Error())
	}

	addUUIDForRuleSpec := func(chain string, ruleSpec *[]string) (string, error) {
		hash := sha256.Sum256([]byte(chain + strings.Join(*ruleSpec, "")))
		encoded := base32.StdEncoding.EncodeToString(hash[:])[:16]
		for idx, part := range *ruleSpec {
			if "--comment" == part {
				(*ruleSpec)[idx+1] = (*ruleSpec)[idx+1] + " - " + encoded
				return encoded, nil
			}
		}
		return "", fmt.Errorf("could not find a comment in the ruleSpec string given: %s", strings.Join(*ruleSpec, " "))
	}

	ensureRuleAtPosition := func(chain string, ruleSpec []string, uuid string, position int) {
		exists, err := iptablesCmdHandler.Exists("filter", chain, ruleSpec...)
		if err != nil {
			glog.Fatalf("Failed to verify rule exists in %s chain due to %s", chain, err.Error())
		}
		if !exists {
			err := iptablesCmdHandler.Insert("filter", chain, position, ruleSpec...)
			if err != nil {
				glog.Fatalf("Failed to run iptables command to insert in %s chain %s", chain, err.Error())
			}
			return
		}
		rules, err := iptablesCmdHandler.List("filter", chain)
		if err != nil {
			glog.Fatalf("failed to list rules in filter table %s chain due to %s", chain, err.Error())
		}

		var ruleNo, ruleIndexOffset int
		for i, rule := range rules {
			rule = strings.Replace(rule, "\"", "", 2) //removes quote from comment string
			if strings.HasPrefix(rule, "-P") || strings.HasPrefix(rule, "-N") {
				// if this chain has a default policy, then it will show as rule #1 from iptablesCmdHandler.List so we
				// need to account for this offset
				ruleIndexOffset++
				continue
			}
			if strings.Contains(rule, uuid) {
				// range uses a 0 index, but iptables uses a 1 index so we need to increase ruleNo by 1
				ruleNo = i + 1 - ruleIndexOffset
				break
			}
		}
		if ruleNo != position {
			err = iptablesCmdHandler.Insert("filter", chain, position, ruleSpec...)
			if err != nil {
				glog.Fatalf("Failed to run iptables command to insert in %s chain %s", chain, err.Error())
			}
			err = iptablesCmdHandler.Delete("filter", chain, strconv.Itoa(ruleNo+1))
			if err != nil {
				glog.Fatalf("Failed to delete incorrect rule in %s chain due to %s", chain, err.Error())
			}
		}
	}

	chains := map[string]string{"INPUT": kubeInputChainName, "FORWARD": kubeForwardChainName, "OUTPUT": kubeOutputChainName}

	if npc.nodePodIPCIDR != "" {
		// optimize for the case when we know pod CIDR for the node
		//-A INPUT -s 10.1.2.0/24 -m comment --comment "kube-router netpol - PQPITJNHBPGOWBG3" -j KUBE-ROUTER-INPUT
		//-A FORWARD -s 10.1.2.0/24 -m comment --comment "kube-router netpol - B54YCUOMUZH6LGXL" -j KUBE-ROUTER-FORWARD
		//-A FORWARD -d 10.1.2.0/24 -m comment --comment "kube-router netpol - BEVEPCOUQNUZIPVK" -j KUBE-ROUTER-FORWARD
		//-A OUTPUT -d 10.1.2.0/24 -m comment --comment "kube-router netpol - AFSPBOUT2BJFJDZ3" -j KUBE-ROUTER-OUTPUT
		for _, customChain := range chains {
			err = iptablesCmdHandler.NewChain("filter", customChain)
			if err != nil && err.(*iptables.Error).ExitStatus() != 1 {
				glog.Fatalf("Failed to run iptables command to create %s chain due to %s", customChain, err.Error())
			}
		}
		args := []string{"-m", "comment", "--comment", "kube-router netpol", "-s", npc.nodePodIPCIDR, "-j", kubeInputChainName}
		uuid, err := addUUIDForRuleSpec("INPUT", &args)
		if err != nil {
			glog.Fatalf("Failed to get uuid for rule: %s", err.Error())
		}
		ensureRuleAtPosition("INPUT", args, uuid, 1)

		args = []string{"-m", "comment", "--comment", "kube-router netpol", "-d", npc.nodePodIPCIDR, "-j", kubeOutputChainName}
		uuid, err = addUUIDForRuleSpec("OUTPUT", &args)
		if err != nil {
			glog.Fatalf("Failed to get uuid for rule: %s", err.Error())
		}
		ensureRuleAtPosition("OUTPUT", args, uuid, 1)

		args = []string{"-m", "comment", "--comment", "kube-router netpol", "-s", npc.nodePodIPCIDR, "-j", kubeForwardChainName}
		uuid, err = addUUIDForRuleSpec("FORWARD", &args)
		if err != nil {
			glog.Fatalf("Failed to get uuid for rule: %s", err.Error())
		}
		ensureRuleAtPosition("FORWARD", args, uuid, 1)

		args = []string{"-m", "comment", "--comment", "kube-router netpol", "-d", npc.nodePodIPCIDR, "-j", kubeForwardChainName}
		uuid, err = addUUIDForRuleSpec("FORWARD", &args)
		if err != nil {
			glog.Fatalf("Failed to get uuid for rule: %s", err.Error())
		}
		ensureRuleAtPosition("FORWARD", args, uuid, 2)
	} else {
		// -A INPUT   -m comment --comment "kube-router netpol" -j KUBE-ROUTER-INPUT
		// -A FORWARD -m comment --comment "kube-router netpol" -j KUBE-ROUTER-FORWARD
		// -A OUTPUT  -m comment --comment "kube-router netpol" -j KUBE-ROUTER-OUTPUT
		for builtinChain, customChain := range chains {
			err = iptablesCmdHandler.NewChain("filter", customChain)
			if err != nil && err.(*iptables.Error).ExitStatus() != 1 {
				glog.Fatalf("Failed to run iptables command to create %s chain due to %s", customChain, err.Error())
			}
			args := []string{"-m", "comment", "--comment", "kube-router netpol", "-j", customChain}
			uuid, err := addUUIDForRuleSpec(builtinChain, &args)
			if err != nil {
				glog.Fatalf("Failed to get uuid for rule: %s", err.Error())
			}
			ensureRuleAtPosition(builtinChain, args, uuid, 1)
		}
	}

	whitelistServiceVips := []string{"-m", "comment", "--comment", "allow traffic to cluster IP", "-d", npc.serviceClusterIPRange.String(), "-j", "RETURN"}
	uuid, err := addUUIDForRuleSpec(kubeInputChainName, &whitelistServiceVips)
	if err != nil {
		glog.Fatalf("Failed to get uuid for rule: %s", err.Error())
	}
	ensureRuleAtPosition(kubeInputChainName, whitelistServiceVips, uuid, 1)

	whitelistTCPNodeports := []string{"-p", "tcp", "-m", "comment", "--comment", "allow LOCAL TCP traffic to node ports", "-m", "addrtype", "--dst-type", "LOCAL",
		"-m", "multiport", "--dports", npc.serviceNodePortRange, "-j", "RETURN"}
	uuid, err = addUUIDForRuleSpec(kubeInputChainName, &whitelistTCPNodeports)
	if err != nil {
		glog.Fatalf("Failed to get uuid for rule: %s", err.Error())
	}
	ensureRuleAtPosition(kubeInputChainName, whitelistTCPNodeports, uuid, 2)

	whitelistUDPNodeports := []string{"-p", "udp", "-m", "comment", "--comment", "allow LOCAL UDP traffic to node ports", "-m", "addrtype", "--dst-type", "LOCAL",
		"-m", "multiport", "--dports", npc.serviceNodePortRange, "-j", "RETURN"}
	uuid, err = addUUIDForRuleSpec(kubeInputChainName, &whitelistUDPNodeports)
	if err != nil {
		glog.Fatalf("Failed to get uuid for rule: %s", err.Error())
	}
	ensureRuleAtPosition(kubeInputChainName, whitelistUDPNodeports, uuid, 3)

	for externalIPIndex, externalIPRange := range npc.serviceExternalIPRanges {
		whitelistServiceVips := []string{"-m", "comment", "--comment", "allow traffic to external IP range: " + externalIPRange.String(), "-d", externalIPRange.String(), "-j", "RETURN"}
		uuid, err = addUUIDForRuleSpec(kubeInputChainName, &whitelistServiceVips)
		if err != nil {
			glog.Fatalf("Failed to get uuid for rule: %s", err.Error())
		}
		ensureRuleAtPosition(kubeInputChainName, whitelistServiceVips, uuid, externalIPIndex+4)
	}

	for _, chain := range chains {
		// for the traffic to/from the local pods let network policy controller be
		// authoritative entity to ACCEPT the traffic if it complies to network policies
		comment := "rule to explicitly ACCEPT traffic that comply to network policies"
		args := []string{"-m", "comment", "--comment", comment, "-m", "mark", "--mark", "0x20000/0x20000", "-j", "ACCEPT"}
		err = iptablesCmdHandler.AppendUnique("filter", chain, args...)
		if err != nil {
			glog.Fatalf("Failed to run iptables command: %s", err.Error())
		}

		// if the traffic comes to this rule, it means that traffic from/to local pod
		// for which no network policy is setup yet, so run through the default pod firewall
		comment = "rule to apply default pod firewall"
		args = []string{"-m", "comment", "--comment", comment, "-j", KubeDefaultPodFWChain}
		err = iptablesCmdHandler.AppendUnique("filter", chain, args...)
		if err != nil {
			glog.Fatalf("Failed to run iptables command: %s", err.Error())
		}
	}
}

// Creates custom chains KUBE-NWPLCY-DEFAULT-INGRESS, KUBE-NWPLCY-DEFAULT-EGRESS
func (npc *NetworkPolicyController) ensureDefaultNetworkPolicyChains() {

	iptablesCmdHandler, err := iptables.New()
	if err != nil {
		glog.Fatalf("Failed to initialize iptables executor due to %s", err.Error())
	}

	// if there is no matching or applicable network policy to a pod, then these chains set mark
	// so that both ingress and egress traffic gets ACCEPT
	markArgs := make([]string, 0)
	markComment := "rule to mark traffic matching a network policy"
	markArgs = append(markArgs, "-j", "MARK", "-m", "comment", "--comment", markComment, "--set-xmark", "0x10000/0x10000")

	err = iptablesCmdHandler.NewChain("filter", kubeIngressNetpolChain)
	if err != nil && err.(*iptables.Error).ExitStatus() != 1 {
		glog.Fatalf("Failed to run iptables command to create %s chain due to %s", kubeIngressNetpolChain, err.Error())
	}
	err = iptablesCmdHandler.AppendUnique("filter", kubeIngressNetpolChain, markArgs...)
	if err != nil {
		glog.Fatalf("Failed to run iptables command: %s", err.Error())
	}
	err = iptablesCmdHandler.NewChain("filter", kubeEgressNetpolChain)
	if err != nil && err.(*iptables.Error).ExitStatus() != 1 {
		glog.Fatalf("Failed to run iptables command to create %s chain due to %s", kubeEgressNetpolChain, err.Error())
	}
	err = iptablesCmdHandler.AppendUnique("filter", kubeEgressNetpolChain, markArgs...)
	if err != nil {
		glog.Fatalf("Failed to run iptables command: %s", err.Error())
	}
}

// KUBE-POD-FW-DEFAULT chain will be used to enforce configured action during the
// window of time when pod gets launched and starts sending the traffic or receiving
// the traffic to the time when network policy enforcements are in place for the pod
func (npc *NetworkPolicyController) ensureDefaultPodFWChains() {
	iptablesCmdHandler, err := iptables.New()
	if err != nil {
		glog.Fatalf("Failed to initialize iptables executor due to %s", err.Error())
	}
	err = iptablesCmdHandler.NewChain("filter", KubeDefaultPodFWChain)
	if err != nil && err.(*iptables.Error).ExitStatus() != 1 {
		glog.Fatalf("Failed to run iptables command to create %s chain due to %s", KubeDefaultPodFWChain, err.Error())
	}
	if npc.nodePodIPCIDR == "" {
		return
	}

	defaultAction := "REJECT"
	if npc.netpolAllowPreCheck {
		defaultAction = "ACCEPT"
	}
	// default action for pod ingress traffic
	comment := "default action for pod ingress traffic"
	args := []string{"-m", "comment", "--comment", comment, "-d", npc.nodePodIPCIDR, "-j", defaultAction}
	err = iptablesCmdHandler.AppendUnique("filter", KubeDefaultPodFWChain, args...)
	if err != nil {
		glog.Fatalf("Failed to run iptables command: %s", err.Error())
	}
	// default action for pod egress traffic
	comment = "default action for pod egress traffic"
	args = []string{"-m", "comment", "--comment", comment, "-s", npc.nodePodIPCIDR, "-j", defaultAction}
	err = iptablesCmdHandler.AppendUnique("filter", KubeDefaultPodFWChain, args...)
	if err != nil {
		glog.Fatalf("Failed to run iptables command: %s", err.Error())
	}
}

func cleanupStaleRules(currentFilterTable *bytes.Buffer, activePolicyChains, activePodFwChains, activePolicyIPSets map[string]bool) error {

	cleanupPodFwChains := make([]string, 0)
	cleanupPolicyChains := make([]string, 0)
	cleanupPolicyIPSets := make([]*utils.Set, 0)

	// add default network policy chain as active
	activePolicyChains[kubeIngressNetpolChain] = true
	activePolicyChains[kubeEgressNetpolChain] = true

	// add default pod FW chain as active
	activePodFwChains[KubeDefaultPodFWChain] = true

	// initialize tool sets for working with iptables and ipset
	iptablesCmdHandler, err := iptables.New()
	if err != nil {
		glog.Fatalf("failed to initialize iptables command executor due to %s", err.Error())
	}
	ipsets, err := utils.NewIPSet(false)
	if err != nil {
		glog.Fatalf("failed to create ipsets command executor due to %s", err.Error())
	}
	err = ipsets.Save()
	if err != nil {
		glog.Fatalf("failed to initialize ipsets command executor due to %s", err.Error())
	}

	// find iptables chains and ipsets that are no longer used by comparing current to the active maps we were passed
	chains, err := iptablesCmdHandler.ListChains("filter")
	if err != nil {
		return fmt.Errorf("Unable to list chains: %s", err)
	}
	for _, chain := range chains {
		if strings.HasPrefix(chain, kubeNetworkPolicyChainPrefix) {
			if _, ok := activePolicyChains[chain]; !ok {
				cleanupPolicyChains = append(cleanupPolicyChains, chain)
			}
		}
		if strings.HasPrefix(chain, kubePodFirewallChainPrefix) {
			if _, ok := activePodFwChains[chain]; !ok {
				cleanupPodFwChains = append(cleanupPodFwChains, chain)
			}
		}
	}
	for _, set := range ipsets.Sets {
		if strings.HasPrefix(set.Name, kubeSourceIPSetPrefix) ||
			strings.HasPrefix(set.Name, kubeDestinationIPSetPrefix) {
			if _, ok := activePolicyIPSets[set.Name]; !ok {
				cleanupPolicyIPSets = append(cleanupPolicyIPSets, set)
			}
		}
	}

	fmt.Println("HERE1")
	// remove stale iptables podFwChain references from the filter table chains
	for _, podFwChain := range cleanupPodFwChains {

		primaryChains := []string{kubeInputChainName, kubeForwardChainName, kubeOutputChainName}
		for _, egressChain := range primaryChains {
			forwardChainRules, err := iptablesCmdHandler.List("filter", egressChain)
			if err != nil {
				return fmt.Errorf("failed to list rules in filter table, %s podFwChain due to %s", egressChain, err.Error())
			}

			// TODO delete rule by spec, than rule number to avoid extra loop
			var realRuleNo int
			for i, rule := range forwardChainRules {
				if strings.Contains(rule, podFwChain) {
					err = iptablesCmdHandler.Delete("filter", egressChain, strconv.Itoa(i-realRuleNo))
					if err != nil {
						return fmt.Errorf("failed to delete rule: %s from the %s podFwChain of filter table due to %s", rule, egressChain, err.Error())
					}
					realRuleNo++
				}
			}
		}
	}

	fmt.Println("HERE2")

	var newChains, newRules, desiredFilterTable bytes.Buffer
	rules := strings.Split(currentFilterTable.String(), "\n")
	if len(rules) > 0 && rules[len(rules)-1] == "" {
		rules = rules[:len(rules)-1]
	}
	for _, rule := range rules {
		skipRule := false
		for _, podFWChainName := range cleanupPodFwChains {
			if strings.Contains(rule, podFWChainName) {
				skipRule = true
				break
			}
		}
		for _, policyChainName := range cleanupPolicyChains {
			if strings.Contains(rule, policyChainName) {
				skipRule = true
				break
			}
		}
		if strings.Contains(rule, "COMMIT") || strings.HasPrefix(rule, "# ") {
			skipRule = true
		}
		if skipRule {
			continue
		}
		if strings.HasPrefix(rule, ":") {
			newChains.WriteString(rule + " - [0:0]\n")
		}
		if strings.HasPrefix(rule, "-") {
			newRules.WriteString(rule + "\n")
		}
	}
	desiredFilterTable.WriteString("*filter" + "\n")
	desiredFilterTable.Write(newChains.Bytes())
	desiredFilterTable.Write(newRules.Bytes())
	desiredFilterTable.WriteString("COMMIT" + "\n")
	fmt.Println("HERE3")
	fmt.Println(desiredFilterTable.String())
	if err := utils.Restore("filter", desiredFilterTable.Bytes()); err != nil {
		return err
	}

	// cleanup network policy ipsets
	for _, set := range cleanupPolicyIPSets {
		err = set.Destroy()
		if err != nil {
			return fmt.Errorf("Failed to delete ipset %s due to %s", set.Name, err)
		}
	}
	return nil
}

// Cleanup cleanup configurations done
func (npc *NetworkPolicyController) Cleanup() {

	glog.Info("Cleaning up iptables configuration permanently done by kube-router")

	iptablesCmdHandler, err := iptables.New()
	if err != nil {
		glog.Errorf("Failed to initialize iptables executor: %s", err.Error())
	}

	// delete jump rules in FORWARD chain to pod specific firewall chain
	forwardChainRules, err := iptablesCmdHandler.List("filter", kubeForwardChainName)
	if err != nil {
		glog.Errorf("Failed to delete iptables rules as part of cleanup")
		return
	}

	// TODO: need a better way to delte rule with out using number
	var realRuleNo int
	for i, rule := range forwardChainRules {
		if strings.Contains(rule, kubePodFirewallChainPrefix) {
			err = iptablesCmdHandler.Delete("filter", kubeForwardChainName, strconv.Itoa(i-realRuleNo))
			if err != nil {
				glog.Errorf("Failed to delete iptables rule as part of cleanup: %s", err)
			}
			realRuleNo++
		}
	}

	// delete jump rules in OUTPUT chain to pod specific firewall chain
	forwardChainRules, err = iptablesCmdHandler.List("filter", kubeOutputChainName)
	if err != nil {
		glog.Errorf("Failed to delete iptables rules as part of cleanup")
		return
	}

	// TODO: need a better way to delte rule with out using number
	realRuleNo = 0
	for i, rule := range forwardChainRules {
		if strings.Contains(rule, kubePodFirewallChainPrefix) {
			err = iptablesCmdHandler.Delete("filter", kubeOutputChainName, strconv.Itoa(i-realRuleNo))
			if err != nil {
				glog.Errorf("Failed to delete iptables rule as part of cleanup: %s", err)
			}
			realRuleNo++
		}
	}

	// flush and delete pod specific firewall chain
	chains, err := iptablesCmdHandler.ListChains("filter")
	if err != nil {
		glog.Errorf("Unable to list chains: %s", err)
		return
	}
	for _, chain := range chains {
		if strings.HasPrefix(chain, kubePodFirewallChainPrefix) {
			err = iptablesCmdHandler.ClearChain("filter", chain)
			if err != nil {
				glog.Errorf("Failed to cleanup iptables rules: " + err.Error())
				return
			}
			err = iptablesCmdHandler.DeleteChain("filter", chain)
			if err != nil {
				glog.Errorf("Failed to cleanup iptables rules: " + err.Error())
				return
			}
		}
	}

	// flush and delete per network policy specific chain
	chains, err = iptablesCmdHandler.ListChains("filter")
	if err != nil {
		glog.Errorf("Unable to list chains: %s", err)
		return
	}
	for _, chain := range chains {
		if strings.HasPrefix(chain, kubeNetworkPolicyChainPrefix) {
			err = iptablesCmdHandler.ClearChain("filter", chain)
			if err != nil {
				glog.Errorf("Failed to cleanup iptables rules: " + err.Error())
				return
			}
			err = iptablesCmdHandler.DeleteChain("filter", chain)
			if err != nil {
				glog.Errorf("Failed to cleanup iptables rules: " + err.Error())
				return
			}
		}
	}

	// delete all ipsets
	ipset, err := utils.NewIPSet(false)
	if err != nil {
		glog.Errorf("Failed to clean up ipsets: " + err.Error())
	}
	err = ipset.Save()
	if err != nil {
		glog.Errorf("Failed to clean up ipsets: " + err.Error())
	}
	err = ipset.DestroyAllWithin()
	if err != nil {
		glog.Errorf("Failed to clean up ipsets: " + err.Error())
	}
	glog.Infof("Successfully cleaned the iptables configuration done by kube-router")
}

// NewNetworkPolicyController returns new NetworkPolicyController object
func NewNetworkPolicyController(clientset kubernetes.Interface,
	config *options.KubeRouterConfig, podInformer cache.SharedIndexInformer,
	npInformer cache.SharedIndexInformer, nsInformer cache.SharedIndexInformer) (*NetworkPolicyController, error) {
	npc := NetworkPolicyController{}

	// Creating a single-item buffered channel to ensure that we only keep a single full sync request at a time,
	// additional requests would be pointless to queue since after the first one was processed the system would already
	// be up to date with all of the policy changes from any enqueued request after that
	npc.fullSyncRequestChan = make(chan struct{}, 1)

	// Validate and parse ClusterIP service range
	_, ipnet, err := net.ParseCIDR(config.ClusterIPCIDR)
	if err != nil {
		return nil, fmt.Errorf("failed to get parse --service-cluster-ip-range parameter: %s", err.Error())
	}
	npc.serviceClusterIPRange = *ipnet

	if config.RunRouter {
		cidr, err := utils.GetPodCidrFromNodeSpec(clientset, config.HostnameOverride)
		if err != nil {
			return nil, fmt.Errorf("Failed to get pod CIDR details from Node.spec: %s", err.Error())
		}
		npc.nodePodIPCIDR = cidr
	}

	npc.netpolAllowPreCheck = config.NetpolAllowPreCheck
	// Validate and parse NodePort range
	nodePortValidator := regexp.MustCompile(`^([0-9]+)[:-]{1}([0-9]+)$`)
	if matched := nodePortValidator.MatchString(config.NodePortRange); !matched {
		return nil, fmt.Errorf("failed to parse node port range given: '%s' please see specification in help text", config.NodePortRange)
	}
	matches := nodePortValidator.FindStringSubmatch(config.NodePortRange)
	if len(matches) != 3 {
		return nil, fmt.Errorf("could not parse port number from range given: '%s'", config.NodePortRange)
	}
	port1, err := strconv.ParseInt(matches[1], 10, 16)
	if err != nil {
		return nil, fmt.Errorf("could not parse first port number from range given: '%s'", config.NodePortRange)
	}
	port2, err := strconv.ParseInt(matches[2], 10, 16)
	if err != nil {
		return nil, fmt.Errorf("could not parse second port number from range given: '%s'", config.NodePortRange)
	}
	if port1 >= port2 {
		return nil, fmt.Errorf("port 1 is greater than or equal to port 2 in range given: '%s'", config.NodePortRange)
	}
	npc.serviceNodePortRange = fmt.Sprintf("%d:%d", port1, port2)

	// Validate and parse ExternalIP service range
	for _, externalIPRange := range config.ExternalIPCIDRs {
		_, ipnet, err := net.ParseCIDR(externalIPRange)
		if err != nil {
			return nil, fmt.Errorf("failed to get parse --service-external-ip-range parameter: '%s'. Error: %s", externalIPRange, err.Error())
		}
		npc.serviceExternalIPRanges = append(npc.serviceExternalIPRanges, *ipnet)
	}

	if config.MetricsEnabled {
		//Register the metrics for this controller
		prometheus.MustRegister(metrics.ControllerIptablesSyncTime)
		prometheus.MustRegister(metrics.ControllerPolicyChainsSyncTime)
		npc.MetricsEnabled = true
	}

	npc.syncPeriod = config.IPTablesSyncPeriod

	node, err := utils.GetNodeObject(clientset, config.HostnameOverride)
	if err != nil {
		return nil, err
	}

	npc.nodeHostName = node.Name

	nodeIP, err := utils.GetNodeIP(node)
	if err != nil {
		return nil, err
	}
	npc.nodeIP = nodeIP

	ipset, err := utils.NewIPSet(false)
	if err != nil {
		return nil, err
	}
	err = ipset.Save()
	if err != nil {
		return nil, err
	}
	npc.ipSetHandler = ipset

	npc.podLister = podInformer.GetIndexer()
	npc.PodEventHandler = npc.newPodEventHandler()

	npc.nsLister = nsInformer.GetIndexer()
	npc.NamespaceEventHandler = npc.newNamespaceEventHandler()

	npc.npLister = npInformer.GetIndexer()
	npc.NetworkPolicyEventHandler = npc.newNetworkPolicyEventHandler()

	return &npc, nil
}
