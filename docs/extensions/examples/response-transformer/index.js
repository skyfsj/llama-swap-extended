export default {
  onResponse(ctx, response) {
    for (const choice of response.choices ?? []) {
      if (typeof choice.message?.content === "string") {
        choice.message.content = choice.message.content.trim();
      }
    }
    return response;
  }
};
