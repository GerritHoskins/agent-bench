FROM scratch
ENV PATH=/
COPY --chmod=0555 dist/docker/agentbench-worker /agentbench-worker
USER 1000:1000
WORKDIR /workspace
ENTRYPOINT ["/agentbench-worker"]
