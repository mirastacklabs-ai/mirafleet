// Package mcpserver is the MCP stdio server. It is the only package in this module that imports
// github.com/modelcontextprotocol/go-sdk (enforced by TestOrchestratorDeps).
//
// # Design
//
// The package is decoupled from internal/orch: the caller hands it tools through [Config].
//
//   - [Serve] runs the server over the given streams (os.Stdin/os.Stdout in production). stdout
//     carries protocol messages only; every log line goes to Config.Logf (stderr). Serve returns
//     nil on stdin EOF or ctx cancellation, and an error only for a broken Config or a transport
//     failure. If ctx is cancelled while a read on in is blocked, that read goroutine is
//     abandoned (it ends with the process).
//   - On stdin EOF, Serve first lets the non-tools/call calls already read (initialize, tools/list,
//     ping) be answered, for at most 2 s; tools/call is cancelled at once (drain.go). A write to a
//     closed stdout (EPIPE, or the Windows broken-pipe errors) ends Serve with nil.
//   - A [Tool] is a mirafleet-owned struct: name, description, JSON input and output schemas
//     (json.RawMessage; the input schema must have type "object"), [Annotations] and a handler
//     func(ctx, Request) (any, error). The SDK does no argument validation for raw tools: the
//     handler decodes Request.Args strictly itself (argv only, regex-validated params).
//   - The handler result is JSON-encoded once and sent as structuredContent plus the same bytes
//     as a single TextContent. Result encoding runs inside the same recover as the handler.
//   - Errors: an error anywhere in the chain (errors.As) that implements [PublicError] (as
//     *[Error] does) is sent as its code and message only; wrapping text is dropped. Error
//     results have isError:true and a single TextContent {"error":{"code","message"}} and NO
//     structuredContent, so a client-side outputSchema check cannot reject them. Any other error
//     (and a recovered panic, including one in MarshalJSON) becomes code "internal" with a fixed
//     generic message: err.Error() is logged via Logf and never sent. A cancelled context
//     becomes "canceled".
//   - Arguments whose first non-space byte is not '{' are refused with a bad_request tool error
//     before the handler runs. [DecodeArgs] decodes Request.Args strictly (unknown fields and
//     trailing data refused) and returns a bad_request *[Error]; handlers should use it.
//   - Config validation: tool names match ^[A-Za-z0-9_.-]{1,128}$ and are unique; input and
//     output schemas (when set) must be JSON objects with type "object".
//   - Capabilities: tools only (no resources, prompts, completions, logging, sampling, roots).
//   - notifications/cancelled cancels the handler's ctx (the SDK's jsonrpc2 layer does this).
//   - With a progressToken in the request _meta, notifications/progress are sent every
//     Config.ProgressInterval (default 15s) until the handler returns; none is sent after the
//     response. Tool output itself stays buffered.
//   - Instructions default to [DefaultInstructions]; Config.Instructions overrides them. An
//     override must keep the sentence saying tool output is data, not instructions; to add text,
//     append to DefaultInstructions rather than replacing it.
//   - [ToolNames] lists the registered tool names (used by the plugin test).
//   - A stub `ping` tool exists only in tests; it is not in any default tool list.
package mcpserver
