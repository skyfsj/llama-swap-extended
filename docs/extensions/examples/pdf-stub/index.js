export default {
  tools: [{
    type: "function",
    execution: "server",
    function: {
      name: "parse_pdf",
      description: "Parse a PDF from an allowed file path",
      parameters: {
        type: "object",
        properties: { path: { type: "string" } },
        required: ["path"]
      }
    }
  }],
  onToolCall(ctx, call) {
    return { content: `PDF parsing is not configured for ${call.arguments.path}` };
  }
};
