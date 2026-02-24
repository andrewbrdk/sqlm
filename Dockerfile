FROM golang:1.26

COPY main.go go.mod index.html style.css /app/

WORKDIR /app
RUN go get gosqlm
RUN go build
RUN rm -r main.go go.mod index.html style.css

EXPOSE 8080

ENTRYPOINT ["/app/gosqlm"]