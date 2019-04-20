all: cryptodog-proxy

cryptodog-proxy: proxy.go
	gofmt -l -s -w .
	go vet
	go build
