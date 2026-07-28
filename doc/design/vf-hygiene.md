---
title: Do Not Hand Out VFs Carrying a Previous Consumer's State
authors:
  - glennswest
reviewers:
  - TBD
creation-date: 28-07-2026
last-updated: 28-07-2026
---

# Do Not Hand Out VFs Carrying a Previous Consumer's State

## Summary

A workload that configures an SR-IOV virtual function from inside its pod is
responsible for reverting those settings before it terminates. When it does not
— because it exits without cleanup, or crashes — the VF is returned to the host
pool still carrying that configuration and is handed to the next pod in that
condition.

This proposes a component in the config-daemon that detects released VFs which
are not in a known state and returns them to one, without unbinding the VF and
without draining the node, together with Kubernetes Events reporting the VF
lifecycle that is currently unobservable from the cluster.

## Motivation

The next workload receives a device configured in a way it did not ask for and
cannot see. The consequences are diffuse and land far from the cause: elevated
interrupt load, routing-protocol instability, packet loss and rising discard
counters, and at the extreme, node-level impact. Receive-path settings are the
most disruptive — a VF left in promiscuous mode causes the adapter's embedded
switch to keep replicating traffic to a vport whose consumer no longer exists —
but any stale attribute can produce behaviour the next workload cannot account
for.

The difficulty is attribution. Nothing in the symptom points at a recycled
device, so the cost is paid in investigation time long before anyone suspects VF
state.

Neither existing in-band path closes the gap. In sriov-cni:

- `CmdDel` returns early when its cached netconf cannot be loaded — deliberate,
  to avoid a kubelet retry loop, but a path on which no restore happens.
- The namespace path is skipped when the pod netns is already gone: the
  node-crash case.
- The namespace path is not entered at all for VFs bound to a DPDK driver
  (`if !netConf.DPDKMode`), so a restore placed in `ReleaseVF` does not cover
  userspace-driver workloads.

The operator itself restores only the administrative attributes it configured,
by design. This proposal does not dispute that scoping as a description of what
the operator configures — it proposes extending what the operator *guarantees on
release*, deliberately and with the new scope stated explicitly.

The justification is the same as for the default (see "Why this is enabled by
default"): the alternative to the
platform guaranteeing a known state is that every consumer of every VF must
defend against arbitrary inherited configuration, and must diagnose it without
any signal pointing at the device. Scoping the guarantee to "attributes we
happened to set" leaves the failure mode fully intact, because the attributes
that cause the damage are precisely the ones the operator did not set.

### Use Cases

- A workload exits normally without reverting the netdev flags it set; the next
  pod to receive that VF inherits them.
- A workload crashes or is OOM-killed, so no teardown path runs at all.
- A node crashes, so CNI DEL never runs for the VFs that were in use.
- A DPDK workload's VF is bound to `vfio-pci` and is never covered by the CNI's
  namespace path.

### Goals

- A released VF is returned to a known state before it is reused.
- Cleanup happens without unbinding the VF and without draining the node.
- A VF in use by a live consumer is never modified.
- The VF lifecycle is observable from the cluster.

### Non-Goals

- Relieving workloads of responsibility for their own cleanup. This is a safety
  net for the cases where that responsibility cannot be discharged, not a
  replacement for it.
- Covering device state that cannot currently be read back (see Constraints).

## Proposal

### Workflow Description

**Detecting release.** The kubelet device-plugin API has an `Allocate` RPC and
no `Free` or `Deallocate` RPC, so nothing in Kubernetes reports that a VF has
been returned. Two signals are used instead:

- *Kernel link events, as the primary trigger.* When a pod's network namespace
  is destroyed — on graceful exit and on crash alike — the VF's netdev is
  returned to the initial namespace, emitting `RTM_NEWLINK` there. A netlink
  subscription turns release into an event-driven operation.
- *A periodic sweep, as the backstop*, for what the event stream cannot cover:
  VFs bound to a userspace driver have no netdev and emit no link event; events
  during a daemon restart are missed, as netlink delivers no history; and the
  socket can overrun under load.

The two are gated independently, since either is useful without the other.

**Deciding a VF is free.** Answered from the allocation state sriov-cni already
maintains, rather than by introducing a second allocation model. That state
records the consumer's network namespace path per VF; resolving it is what makes
the check crash-aware — if the namespace is gone, so is the consumer.

**Serialisation.** Observing a VF as free and then writing to it is a
check-then-act race: the VF can be handed to a new pod in between, and the
cleanup would revert settings the new consumer legitimately applied. Every
evaluation happens under the same per-VF lock sriov-cni takes for the duration
of CNI ADD and DEL, with allocation and device state re-read after the lock is
acquired. The lock is taken non-blocking, so a VF the CNI is mid-operation on is
left for the next pass rather than stalling behind it.

This matters most on the event path, where the release event and the CNI's own
teardown race by construction — the kernel emits the event as the namespace goes
away while the CNI may still be finishing.

### Known values, not a remembered baseline

A consumer configures a device when it opens it and inherits nothing meaningful
from whoever held it before, so there is no association between one consumer and
the next and no value in preserving a particular VF's previous settings.

A released VF is therefore returned to a *known* state, identical for every VF
of that device. Values come from the device where the device defines them — the
permanent hardware address is read back from the NIC rather than remembered —
and from the kernel's documented defaults where it does not: an administratively
unset VF MAC is all zeros, an unset VLAN is 0, unset rate limits are 0, link
state is auto, and the receive-path flags are off.

This also removes a failure mode a remembered baseline cannot avoid: a VF that
was already dirty the first time it was observed is still returned to the known
state, because the target never depended on having seen the device clean.

### Scope

| Plane | Attributes |
|---|---|
| netdev-level | promiscuous mode, all-multicast, attached XDP program, MTU, effective MAC, admin up/down |
| PF-side administrative | admin MAC, VLAN + QoS + protocol, spoofchk, trust, min/max tx rate, link state, RSS query permission |

The PF-side plane is readable and writable through the parent PF regardless of
what the VF is bound to, which is what allows a VF bound to `vfio-pci` — with no
netdev — to be cleaned.

The restored set is selectable at runtime, and a dry-run mode reports what would
change without changing it.

### API Extensions

None. No new CRD, no change to existing CRDs.

Kubernetes Events are emitted on the node's existing `SriovNetworkNodeState`, so
`kubectl describe sriovnetworknodestate <node>` shows the VF lifecycle next to
the rest of that node's SR-IOV configuration:

| Reason | Type | Meaning |
|---|---|---|
| `VFReleaseDetected` | Normal | VF returned to the host, confirmed to have no live consumer |
| `VFStateRestored` | Normal | Released VF returned to the known state |
| `VFRestoreFailed` | Warning | Restore failed; the VF may be reused while still dirty |

Release is reported on the lifecycle transition, not on remediation: a VF
released clean reports the release and reports no restore.

### Implementation Details/Notes/Constraints

**Not part of the nodeState reconcile loop.** Two properties of that path rule
it out:

- It reconciles only on `SriovNetworkNodeState` annotation or generation
  changes, with no resync. A VF is dirtied at pod teardown, which produces no
  nodeState change, so a check placed there would never fire.
- Work routed through it reaches `needToUpdateVFs()` → `needDrainNode()` →
  `handleDrain()`. Draining every SR-IOV pod off a node to clean one released VF
  defeats the purpose.

The component runs as its own Runnable, with its own trigger, and never requests
a drain.

**Per-driver capability.** There is deliberately no table of which NIC supports
what. VF operations are implemented independently by each PF driver and the
supported subset differs by driver and by kernel version, so a vendor table
would encode a snapshot that is wrong for the next driver, the next kernel, or
an out-of-tree driver. The kernel is asked instead: an operation reported
`EOPNOTSUPP` is skipped as not applicable and the remaining attributes are still
restored. A single unsupported attribute must never prevent promiscuous mode
from being cleared. Events report the attributes actually written, which may be
narrower than the set attempted.

**Coverage gaps**, recorded rather than left implicit:

- `IFLA_VF_IB_NODE_GUID` / `IFLA_VF_IB_PORT_GUID` are settable through netlink
  but not readable back through the VF table, so a deviation cannot be detected.
- `IFLA_VF_RSS_QUERY_EN` is readable but has no setter available, so it is
  detected and reported, then skipped as unsupported.
- Device-level settings reachable through ethtool — offload features, ring
  sizes, interrupt coalescing, channel counts, RSS hash key and indirection
  table — and the netdev's unicast/multicast filter lists and VLAN filter table
  are not yet covered. A consumer can change any of these and they survive its
  exit. This is the largest piece of remaining work.

### Why this is enabled by default

Other optional config-daemon behaviours are opt-in, so this warrants an explicit
argument rather than an assumption.

A VF recycled with a previous consumer's configuration behaves like a fault
injected at random: it lands on whichever workload next receives that VF, it
produces different symptoms depending on which attribute was left set and what
the new workload does with the device, and none of those symptoms names the
cause. The operational cost is therefore not one incident but an ongoing source
of unreproducible behaviour, investigated repeatedly and attributed elsewhere.

That is what makes an opt-in switch the wrong shape. The operators who need this
are, by construction, the ones who have not yet determined that stale VF state is
what they are chasing — so a flag they must first know to set delivers the fix to
everyone except the people currently paying for its absence.

Returning a device to a known state before reuse is a property the platform
should provide as a matter of course, in the same way it does not hand out a
partially configured VF at allocation time. `--vf-hygiene-disable` turns the
component off entirely for anyone who needs to observe the unclean behaviour
without interference, and `--vf-hygiene-dry-run` reports what would change
without changing it.

### Upgrade & Downgrade considerations

On upgrade, existing VFs are evaluated on the first sweep. Downgrade removes the
component; no persistent state is written, so nothing is left behind.

### Test Plan

**Status: not yet executed on hardware.** Automated tests exist and pass on a
development host, but no part of this has been validated against an SR-IOV NIC.
The rest of this section is the intended plan, not a report of completed work.

Automated coverage that exists today:

- Unit coverage for the sweep decision logic: allocated VFs are never touched;
  VFs with unknown allocation state are skipped; scope is respected; dry-run
  changes nothing; a driver-unsupported attribute does not block the rest.
- Race coverage: a VF re-allocated between the unlocked pre-check and the lock
  is left alone; a device held by another component is skipped rather than
  waited on.
- Kernel-level tests against real netlink on dummy interfaces: flags are read
  and cleared, MTU and MAC are returned to the known values, and a device
  standing in for an in-use VF is untouched.

Planned, on SR-IOV hardware, before this is proposed upstream:

- The PF-side restore path against a real PF VF table, across more than one PF
  driver, so that capability detection is exercised on a driver that does not
  implement the full attribute set.
- The netns-return trigger. This cannot be reproduced with virtual interfaces,
  since a dummy or veth is destroyed with its namespace rather than relocated to
  the initial namespace as a VF is, so the event path has so far been exercised
  only by driving the handler directly.
- Graceful pod deletion and abrupt termination, including a VF bound to a
  userspace driver, for which no link event is emitted and the periodic sweep is
  the only path.
- Confirmation that a restored VF is usable by a subsequent workload, which is
  the property the whole component exists to provide.
- **That returning the VF to the known state actually stops delivery to it.**
  Clearing promiscuous mode removes only the rule that replicates everything to
  a vport; its unicast MAC filter, subscribed multicast addresses, VLAN filter
  entries and queues all survive, so traffic addressed to that VF can continue
  to arrive into queues nobody is draining. Field evidence is consistent with
  this — with the VF released and promiscuous mode cleared, discard and pause
  counters kept climbing, and only a full unbind stopped them. The test measures
  per-vport receive and discard counters after each step in turn: promiscuity
  cleared, then admin-down, then link state disabled. The step at which delivery
  stops identifies what is sufficient. If none of them do, flushing the filter
  lists is required, and the gap recorded above becomes a blocker rather than a
  limitation.
