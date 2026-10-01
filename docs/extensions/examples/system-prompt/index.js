export default {
  onRequest(ctx, request) {
    if (ctx.endpoint === "anthropic.messages") {
      if (Array.isArray(request.system)) {
        request.system.push({ type: "text", text: "Answer concisely and cite sources when available." });
      } else {
        request.system = [request.system, "Answer concisely and cite sources when available."].filter(Boolean).join("\n");
      }
    } else if (Array.isArray(request.messages)) {
      request.messages.unshift({ role: "system", content: "Answer concisely and cite sources when available." });
    } else if (ctx.endpoint === "responses") {
      request.instructions = [request.instructions, "Answer concisely and cite sources when available."].filter(Boolean).join("\n");
    }
    return request;
  }
};
