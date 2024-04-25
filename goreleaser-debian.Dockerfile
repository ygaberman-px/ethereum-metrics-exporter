FROM debian:12.5
RUN apt-get update && apt-get -y upgrade && apt-get install -y --no-install-recommends \
  libssl-dev \
  ca-certificates \
  && apt-get clean \
  && rm -rf /var/lib/apt/lists/*
COPY ethereum-metrics-exporter* /ethereum-metrics-exporter
ENTRYPOINT ["/ethereum-metrics-exporter"]
