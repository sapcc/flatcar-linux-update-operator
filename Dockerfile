FROM golang:1.18-alpine as builder

RUN apk add -U make git
WORKDIR /usr/src/github.com/flatcar-linux/flatcar-linux-update-operator
COPY . .
RUN make bin/update-agent

FROM gcr.io/distroless/static:nonroot
LABEL source_repository=https://github.com/sapcc/flatcar-linux-update-operator""
WORKDIR /bin
COPY --from=builder /usr/src/github.com/flatcar-linux/flatcar-linux-update-operator/bin/update-agent .
USER nonroot:nonroot

ENTRYPOINT ["/bin/update-agent"]
