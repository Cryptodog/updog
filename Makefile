all: updog

updog: proxy.go
	gofmt -l -s -w .
	go vet
	go build
