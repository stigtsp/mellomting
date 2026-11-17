# Session affinity for chat completions across replicas (open problem)

**Status:** noted 2026-11-16 during the Brev pilot. Nothing is decided or
implemented, and nothing here amends PLAN.md.

## The problem

When several backends serve the same public model (for example 8
data-parallel replicas of one model, each attached by its own agent), the
proxy may route each request of a multi-turn conversation to a different
replica.

Inference engines keep their **prefix cache per replica**. An agentic
client (a code-review harness) re-sends the whole growing conversation on
every turn, so performance depends heavily on the next turn reaching the
replica that already holds the earlier turns:

- **Measured on one replica** (vLLM, Ornith 1.5 122B, 2026-11-16): **74%
  of prompt tokens were prefix-cache hits**. Effective prompt throughput was
  21.4K tok/s against about 10K uncached, roughly halving prompt cost.
- **With N replicas and no affinity,** about (N−1)/N of follow-up turns
  miss the cache (7 in 8 for N = 8). The benefit is mostly lost.

## What exists today

`internal/proxy/affinity.go` (PLAN §21.1, §21.4) pins only **Responses
API** stored responses: (key ID, response ID) maps to a backend, for
`previous_response_id` follow-ups and retrieval. **Chat completions carry
no conversation ID**, so they get no affinity. An attached agent's
responses are pinned to one attachment, and every reattach is a new one,
so they are "not found" after the agent reconnects or changes models
(PLAN §104.2).

## Options

1. **Client-side pinning, no code:** expose each replica under its own
   public model name (`ns/model-r0` … `ns/model-r7`) and let the client
   assign each conversation to one replica, for example by a hash of a work
   item ID. This works today; the client carries the burden.
2. **Opt-in affinity header:** the client sends a session identifier (for
   example `X-Mellomting-Session` or the OpenAI `user` field). The proxy
   hashes (key ID, session) to a backend among the healthy ones for that
   model, and falls back when the backend leaves. Design questions:
   - bounded table or stateless consistent hashing;
   - behaviour when a backend detaches mid-session;
   - the session value must never be logged in clear (hash it);
   - it must never be usable to reach a backend the key may not use.
3. **Prefix-hash routing:** hash the first N tokens or bytes (system
   prompt plus first user message) and route on that. Transparent to
   clients, but it needs body inspection before routing, interacts with
   body limits and streaming, and can skew load when many conversations
   share a prefix (they all go to one replica). Probably not worth it.
4. **Engine-side routing** (for example a vLLM router with KV-aware
   routing) in front of the replicas, with mellomting seeing a single
   backend. It moves the problem out of mellomting, at the cost of another
   component.

Suggested order: use 1 for the pilot's CPAN run if data-parallel replicas
are chosen. Consider 2 if replicated backends become a normal deployment.
A single tensor-parallel backend (one replica) does not have the problem.

## How to measure

vLLM exposes `vllm:prefix_cache_queries_total` and
`vllm:prefix_cache_hits_total` per replica. Compare the hit rate with one
replica against N replicas under the same client load.
