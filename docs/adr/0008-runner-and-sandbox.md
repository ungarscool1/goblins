# ADR 0008: Credentialed runners, sandboxes, and an egress allow-list

**Status:** accepted

**Context.** ADR 0004 splits workloads into those holding credentials, which never run
repository code, and those running repository code, which hold nothing. Until now that split was
a convention of the pipeline, and two gaps stayed open (#16): the agent's login sits in the same
container as the agent, behind the CLI's restricted mode only, and agents have unfiltered egress,
so a prompt-injected agent could send what it reads anywhere.

We considered a **credential gateway**: the agent container would hold no login, and a trusted
process would add it to requests on the way out. With today's subscription CLIs this does not
work. Both refuse to start without a login of their own, refresh it themselves, and talk to
endpoints and with headers that are theirs to change. A gateway would have to forge a login file
and rewrite traffic it does not control, which is fragile, and could break the subscription's
terms. Pointing the CLI at another base URL is also what ADR 0004's billing guard forbids, since
it is how API billing is switched on. A gateway remains the right design for an agent that
accepts an external token broker, and the trust classes below are what it would plug into.

**Decision.**

1. **Trust is part of the runtime contract.** `WorkloadSpec.Trust` is either `credentialed`
   (an agent, or a Git step with a short-lived token) or, by default, `sandbox`. Every runtime
   refuses a sandbox that has secrets or mounts a volume marked as a login, before starting it.
   Agents also declare their login volumes (`core.CredentialHolder`), and every runtime is wrapped
   so that a sandbox naming one of them is refused even if its mount does not say so. The zero
   value being the most confined means a new workload nobody classified holds nothing.
2. **Egress goes through an allow-list.** `goblins egress-proxy` opens HTTPS tunnels to listed
   host names only. It resolves each name itself, refuses private, loopback, link-local, shared
   and NAT64 addresses, and connects to the address it checked, so DNS cannot steer a tunnel into
   the network. Plain HTTP is refused. With `egress_proxy` set:
   - on Docker, egress workloads join an internal network, with no route out, that only the proxy
     also joins. An existing network that is not internal is refused rather than used;
   - on Kubernetes, the egress network policy allows DNS and the proxy's pods, and nothing else.

   Workloads get `HTTPS_PROXY`, but the network is what enforces it: a process that ignores the
   variable reaches nothing.

**Consequences.**

- What a prompt-injected agent can reach is now the listed hosts. The workspace can still leave
  through them, as an agent's own provider is on the list; per-path filtering is not possible
  through a TLS tunnel without breaking TLS, which we do not do.
- The login still sits next to the agent, behind restricted mode. That is unchanged, and stays a
  known gap until an agent supports a token broker.
- Egress workloads behind the proxy share one internal network, so they can reach each other's
  ports. Agents and Git steps open none.
- Allow-lists are the operator's to maintain. Denials are logged with the host, so a missing
  entry is found from the proxy's log.
