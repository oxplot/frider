FROM golang:1.16-buster

RUN mkdir -p /app
WORKDIR /app
COPY . /app/
RUN go build -o frider

FROM debian
RUN apt update && apt install -y ca-certificates
COPY --from=0 /app/frider /
CMD /frider -config /frider.yaml
