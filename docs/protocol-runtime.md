# Protocol runtime

The runtime separates portable conversation content, provider-specific state,
gateway tool execution and owned file data. Provider IDs and reasoning artifacts
are scoped to provider, adapter, model, credential and conversation version.

## MCP configuration

```yaml
protocolRuntime:
  mcp:
    enabled: true
    servers:
      knowledge:
        transport: http
        url: https://mcp.example.com/mcp
        headers:
          Authorization: KNOWLEDGE_AUTHORIZATION
        models: [assistant]
        tools: [search]
        resources: ["docs://manual"]
        prompts: [summarize_document]
        approval: always
      local:
        transport: stdio
        command: /usr/local/bin/local-mcp
        args: [serve]
        env: [HOME, LOCAL_SERVICE_TOKEN]
        approval: always
```

`headers` maps header names to environment variable names. The variable contains
the complete header value, such as `Bearer …`. `env` names the variables passed
to a stdio process; other variables, apart from `PATH`, are not inherited.
HTTP credentials never cross an origin boundary. `sse` selects the legacy SSE
transport. `http` selects Streamable HTTP.

`/mcp?model=assistant&live=true` enables persistent inventories, list-change
notifications, resource subscriptions, progress and legacy logging. Legacy
roots, sampling and elicitation are forwarded to the originating client.
For 2026 clients, legacy callbacks become explicit input-required rounds for
tools, resources and prompts. Each suspended legacy operation expires after
two minutes and at most ten input rounds. It cannot survive a gateway restart.
Live bridges are isolated by identity, model, configuration and credential
version; each bridge serializes upstream operations while routing callbacks.
Up to 64 live bridges are retained; inactive bridges expire after 30 minutes.

Connections are reused within one authenticated identity, including between
approval and execution. Configuration changes retire old sessions. Idle
sessions expire after 15 minutes; shutdown cancels active operations. The runtime
allows up to 32 configured servers, 32 connections per identity and 128 total.

An empty allowlist allows all entries. `*` also allows all entries. Unknown or
omitted approval policy requires approval; `never` permits direct execution by
an explicitly trusted server. Tool names include a stable namespace and hash.
Matching client MCP declarations can narrow the configured tool set and require
additional approval. They cannot weaken the configured approval policy. Inline
client MCP credentials never replace configured gateway credentials. Tool-choice
and parallel-call restrictions are checked before server execution.

The inference routes import configured MCP tools into the existing Extensions
tool loop. Client tools remain client-owned. Resources and prompts can be added
to a request with:

```json
{
  "gateway_context": {
    "resources": [{"server": "knowledge", "uri": "docs://manual"}],
    "prompts": [{"server": "knowledge", "name": "summarize_document", "arguments": {}}]
  }
}
```

Resources and prompts become model context. Their gateway origin does not make
them provider-native MCP features. Content that the selected client dialect
cannot represent is rejected.

`/mcp?model=assistant` exposes the gateway MCP server with inference
authentication and model authorization. `/api/mcp` remains the documentation
MCP endpoint. A stdio client can use the gateway through:

```bash
go build -o ./build/mcp-proxy ./cmd/mcp-proxy
./build/mcp-proxy -url 'http://localhost:8080/mcp?model=assistant&live=true' \
  -authorization-env LLAMA_SWAP_AUTHORIZATION
```

Approvals return HTTP 409 with `approval_id`, `approval_ids`, `approval_url` and
`resume_url`. Read an approval with `GET /api/mcp/approvals/{id}` and decide with
`POST /api/mcp/approvals/{id}` and `{"approve":true}` or `{"approve":false}`.
Execution is claimed atomically and is not repeated by a duplicate approval.
Resolve every approval in a tool round before posting to its `resume_url`.
For mixed client/server rounds, send the listed client results as
`{"tool_results":{"call_id":"result"}}` when resuming.
Each saved model continuation can be consumed once. Approval records expire
after 15 minutes. A `store:false` request cannot be retained for model approval
continuation.

MCP results retain their content, structured content and error state. Supported
media also travels as native tool-result blocks. A target without the required
media representation receives an explicit error. Independent MCP clients echo
`requestState` and `inputResponses` through `/mcp`. Inference tool loops retain
pending input and their model continuation. Read the returned record to obtain
`inputRequests`, then post `{"input_responses":{"request_id":{"action":"accept","content":{}}}}`
to `/api/mcp/approvals/{id}`. Every pending input must be answered; form input
is checked against its schema. Repeated input-required rounds keep the original
opaque state and session. Once complete, post to `resume_url` to continue model
generation. Previously completed tools are retained and are not run again.

The default `/mcp` endpoint exposes snapshot inventories; use `live=true` for
notifications and legacy callbacks. Gateway approvals remain gateway events,
not provider-native approval events. Human decisions and sampling input are
supplied by the client, rather than synthesized by the gateway.

## Files

`POST /v1/files` accepts multipart `file`; `GET /v1/files` lists owned files.
`GET /v1/files/{id}`, `GET /v1/files/{id}/content` and
`DELETE /v1/files/{id}` retrieve or remove owned data. IDs start with `gfile_`.
Files are limited to 16 MiB each and 256 MiB per identity. Original bytes are
content-addressed and are independent of request-audit retention.

Use a gateway ID in an attachment's `file_id`, `fileUri` or `uri`. The runtime
resolves it before dialect conversion, then uploads owned data for the selected
provider and credential. Native references are cached and checked before reuse.
An expired or deleted provider reference is re-created from owned bytes.
Gemini resumable uploads reject cross-origin upload URLs. Chat uses native PDF
references and inline images; unsupported file modalities require another
surface. External URLs and provider-only IDs are not downloaded into gateway
ownership automatically.

## Conversations and checkpoints

Create with `POST /api/conversations` and `{"model":"assistant"}`. Send the
returned ID as `X-Llama-Swap-Conversation-ID` on generation requests. Successful
turns advance `X-Llama-Swap-Conversation-Version`; streams hold their terminal
event until the turn has been saved. Concurrent writers conflict instead of
overwriting each other's state.

| Endpoint | Operation |
| --- | --- |
| `GET /api/conversations` | List owned conversations |
| `GET /api/conversations/{id}` | Read portable content and lineage |
| `DELETE /api/conversations/{id}` | Delete conversation and checkpoints |
| `POST /api/conversations/{id}/checkpoints` | Save a checkpoint |
| `POST /api/conversations/{id}/fork` | Fork current state or `checkpoint_id` |
| `POST /api/conversations/{id}/compact` | Save checkpoint and apply a summary |
| `POST /api/conversations/{id}/generations` | Start background checkpoint generation |
| `GET /api/conversation-generations/{id}` | Poll background status and final response |
| `POST /api/conversation-generations/{id}/cancel` | Request cancellation |
| `DELETE /api/conversation-generations/{id}` | Remove a terminal generation record |

Compaction accepts `summary` and `retain_messages`. Omitting `summary` requests
a model-generated semantic summary. This is gateway compaction; it does not
produce provider-encrypted compaction artifacts. Native artifacts remain in the
checkpoint and are invalidated on the compacted branch. Provider/model changes
use portable history and never reuse another scope's opaque state. Returning
to an older provider requires a valid matching lineage or a checkpoint fork.

Gateway conversations require a single response candidate and explicit
retention. They cannot be combined with `store:false`, provider conversation
handles, `previous_response_id` or `previous_interaction_id`.
Background checkpoint requests use `{"format":"openai.responses","request":{"model":"assistant","input":"..."}}`
on the `generations` endpoint. All five formats are accepted. The gateway
executes a foreground upstream request asynchronously and saves its final
response and checkpoint. Polling does not commit the same turn again. A
cancelled turn does not advance the checkpoint; cancellation after a successful
commit leaves the completed result intact. The endpoint returns final results,
without a live output stream. Up to 32 jobs run concurrently, with a 30-minute
deadline; each identity retains at most 256 records and 256 MiB of results.

Add `"execution":"native"` to hand the work to the provider instead of running
it in the gateway. The request is submitted with `background:true` to the
provider's own Responses or Interactions API and the returned job id is what
the gateway watches; the other three formats are rejected because they have no
provider-owned background job. Native execution also requires that exact
upstream surface: a model reached through a different dialect fails instead of
being silently converted into a synchronous request.

The saved checkpoint records the provider, adapter, upstream model and
credential that own the job. Every later poll and the cancellation request go
to that exact source rather than through the pool scheduler, because a
provider-owned job is only readable by the account that created it. A source
that left the configuration fails the record permanently, while a credential
that is briefly cooling down or a 5xx keeps the watch going. Cancellation is
forwarded to the provider and confirmed by polling, with a 30-second grace
period before the job is reported as not acknowledged. A watch that ends
without a terminal answer — gateway shutdown or the 30-minute deadline —
leaves `"status":"interrupted"`, because the provider may still finish the job.
A completed native turn keeps its opaque state, so the next turn continues the
same provider-side conversation instead of replaying history. The checkpoint is
written against the conversation version captured at submit: if another turn
advanced the conversation while the provider was working, the late commit fails
with a version conflict instead of overwriting that turn.

Queued or running records from another gateway process become `interrupted`
when read after restart. Their outcome is unknown and they are never rerun
automatically. Inspect the conversation before retrying. A native record that
saved its scope is re-attached instead, and its pending checkpoint is committed
when the provider finishes.

## Compatibility planning

`POST /api/protocol/plan` accepts `source`, `model` and the original `request`.
Plans classify candidate surfaces as native, compatible, degraded or rejected
and include conversion diagnostics and configured/cached model capabilities.
Missing capability evidence remains unknown.

`X-Llama-Swap-Compatibility` selects `portable` (default), `strict` or `native`.
Strict conversion rejects diagnostics, except a required default token limit.
Native mode requires the original API surface. Portable conversion reports
diagnostic codes through `X-Llama-Swap-Conversion-Diagnostics`, including HTTP
trailers on streamed responses. Provider-hosted tools, cache behavior,
background execution and encrypted state require their actual native surface
or an explicit gateway implementation; generic conversion does not make them
equivalent.
