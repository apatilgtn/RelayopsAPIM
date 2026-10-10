FROM gcr.io/distroless/static-debian12:nonroot

COPY bin/linux/relayops /usr/local/bin/relayops
COPY bin/linux/relayopsctl /usr/local/bin/relayopsctl
COPY bin/linux/mockupstream /usr/local/bin/mockupstream
COPY bin/linux/relayops-secrets /usr/local/bin/relayops-secrets


# The last-known-good config cache is written to ./data; mount a volume there
# when the root filesystem is read-only.
WORKDIR /home/nonroot
# 8080 proxy, 9090 control plane, 9091 status listener of gateway-only nodes
EXPOSE 8080 9090 9091
ENTRYPOINT ["/usr/local/bin/relayops"]

