Kube-router provides an implementation of Kubernetes network policy specification. Modern Linux kernel provide several ways to filter packets. One can use predominant iptables or nftables or more recent BPF technologies etc. At the moment kube-router uses iptables to realize network policy specification. However as BPF technology and tooling mature kube-router will adopt or provide alternate implementation using BPF.

This design document captures the current implementation of network policy specification using iptables.

# Design tenets and goals:

  - full compliance to Kubernetes v1 network policy specification
  - a CNI agnotstic implementation i.e.) no assumption on CNI used, so that its usable with any CNI
  - provide a stateful firewall for higher performance
  - ensure decoupled control/management plane from datapath so that kube-router pod restarts/crash etc does not affect data-path
  - be authoritative (take precedence over any other policies) in accept/deny traffic concerning the traffic from/to the pods
  - kube-router should process the traffic from/to the pods only, and should skip any other traffic and also any configuration changes perform should not resulting in accept/deny of non-pod traffic
  - provide insight in to dropped traffic due to netpol enforcement
  - ensure network policy enforcements are in-place before pod sends the first packet
  - tolerant to out-of-band changes and reconcile desired state
  - fully stateless design, no bookkeeping data to represnet data any of sort. should scale to cluster of any size

# Implementation

## intercepting traffic

iptables `fitler` table has three built in chains `INPUT`, `OUTPUT`, `FORWARD`. On a Linux host incoming traffic, outgoing traffic and forwarded traffic gets run through these chains respectivley.

kube-router introduces three custom chains `KUBE-ROUTER-INPUT`, `KUBE-ROUTER-OUTPUT`, `KUBE-ROUTER-FORWARD` corresponding to `INPUT`, `OUTPUT`, `FORWARD` respectively and inserts following rules at the top of these default chains so that it gets first chance to analyze the traffic

```
-A INPUT   -m comment --comment "kube-router netpol" -j KUBE-ROUTER-INPUT
-A FORWARD -m comment --comment "kube-router netpol" -j KUBE-ROUTER-FORWARD
-A OUTPUT  -m comment --comment "kube-router netpol" -j KUBE-ROUTER-OUTPUT
```

Further to the custom chains mentioned above kube-router implementation of network policies is structured around two other types of custom chains. Each pod running on the node is represented with a custom `fitler` table chain that starts with prefix `KUBE-POD-FW-` followed by hash derived from pod name and namespace for e.g. KUBE-POD-FW-RQTOW7VJ3F3BCPYH. Hash ensures there is unique chain for any pod. On a node there will be such chains only for the pods running on the node. 

Each network policy is represented by custom chain in `filter` table and has a prefix `KUBE-NWPLCY-` and followed by unique hash derived from network poilicy name and namespace name for e.g. `KUBE-NWPLCY-MZJCGTZM7XPM2K7Z`

`INPUT` -> `KUBE-ROUTER-INPUT` -> `KUBE-POD-FW-RQTOW7VJ3F3BCPYH` -> `KUBE-NWPLCY-MZJCGTZM7XPM2K7Z`
`FORWARD` -> `KUBE-ROUTER-FORWARD` -> `KUBE-POD-FW-RQTOW7VJ3F3BCPYH` -> `KUBE-NWPLCY-MZJCGTZM7XPM2K7Z`
`OUTPUT` -> `KUBE-ROUTER-OUTPUT` -> `KUBE-POD-FW-RQTOW7VJ3F3BCPYH` -> `KUBE-NWPLCY-MZJCGTZM7XPM2K7Z`


** input traffic


Pod 
  add 
    launched on the node?
    - Yes:
        matches target pod of any network policy in the namespace?
        - Yes: set up pod firewall chain and rules to run through the network policy chains
        - No:  set up pod firewall chain with default behaviour
    - in both the cases(launched on same or different node) run through all network policies
      - matches destination/source pod selector?
          update corresponding network policy ipset 
  delete
    deleted on the node
      - remove firewall chain for the pod
    in both the cases (deleted on the node and on different node)
      - matches destination/source pod selector?
          update corresponding network policy ipset 

Namespcae
Networkpolicy
