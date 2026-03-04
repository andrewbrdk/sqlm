FROM golang:1.26

RUN apt-get update && apt-get install pgformatter

RUN mkdir -p /app/context
RUN mkdir -p /app/logs
COPY main.go go.mod index.html style.css /app/
WORKDIR /app
RUN go get queryagent
RUN go build
RUN rm -r main.go go.mod index.html style.css

EXPOSE 8080

ENTRYPOINT ["/app/queryagent"]
