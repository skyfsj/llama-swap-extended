export default {
  onRequest(ctx, request) {
    ctx.log.info(`request ${ctx.requestId} model=${ctx.resolvedModel} endpoint=${ctx.endpoint}`);
    return request;
  }
};
