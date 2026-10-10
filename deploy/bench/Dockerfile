# Tools image for the comparison: RelayOps, the mock upstream and the load
# generator, built by run.sh into .build/ (static Linux binaries).
FROM alpine:3.20
COPY .build/relayops .build/mockupstream .build/bench /usr/local/bin/
WORKDIR /tmp
