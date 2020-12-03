# Builder image
FROM golang:1.13 AS builder



WORKDIR /root/build
COPY . .

# RUN apt-get update && apt-get install -y libflint-dev libmpfr-dev 

# ENV GOOS=linux 
# ENV GO111MODULE=on 
WORKDIR /root/build/server/cmd/dcrdex
RUN go install 

WORKDIR /root/build/server/cmd/genkey
RUN go install 


# Deployment image
FROM debian:10 

WORKDIR /root

# RUN apt-get update && apt-get -y install libflint-2.5.2
COPY --from=builder /go/bin/* /bin/

COPY server/entrypoint.sh server/cmd/dcrdex/sample-markets.json ./

# EXPOSE 5760 443

CMD [ "./entrypoint.sh" ]
